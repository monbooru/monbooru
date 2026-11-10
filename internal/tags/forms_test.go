package tags

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func categoryIDOf(t *testing.T, svc *Service, name string) int64 {
	t.Helper()
	id, ok, err := CategoryIDByName(svc.db, name)
	if err != nil || !ok {
		t.Fatalf("category %q: %v (found %t)", name, err, ok)
	}
	return id
}

func TestParseForm_ReadsAPrefixNamingNoCategoryAsGeneral(t *testing.T) {
	_, svc := setupTestDB(t)
	general, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "character")
	for form, want := range map[string]struct {
		cat  int64
		bare string
	}{
		"character:miku": {character, "miku"},
		"Character:Miku": {character, "Miku"},
		"re:zero":        {general, "re:zero"},
		"solo":           {general, "solo"},
	} {
		cat, bare, ok := svc.ParseForm(form)
		if !ok || cat != want.cat || bare != want.bare {
			t.Errorf("ParseForm(%q) = (%d, %q, %t), want (%d, %q)", form, cat, bare, ok, want.cat, want.bare)
		}
	}
	for _, form := range []string{"  ", "artist:"} {
		if _, _, ok := svc.ParseForm(form); ok {
			t.Errorf("ParseForm(%q) parsed", form)
		}
	}
}

func TestSplitForm(t *testing.T) {
	database, svc := setupTestDB(t)
	general, artist, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "artist"), categoryIDOf(t, svc, "character")
	for form, want := range map[string]Form{
		"character:miku":  {CategoryID: character, Category: "character", Name: "miku"},
		" Artist:Foo ":    {CategoryID: artist, Category: "artist", Name: "Foo"},
		"artist:":         {CategoryID: artist, Category: "artist"},
		"general:re:zero": {CategoryID: general, Category: "general", Name: "re:zero"},
		"re:zero":         {Name: "re:zero", Namespace: "re"},
		"a:b:c":           {Name: "a:b:c", Namespace: "a"},
		":3":              {Name: ":3"},
		"solo":            {Name: "solo"},
	} {
		if got, err := SplitForm(database, form); err != nil || got != want {
			t.Errorf("SplitForm(%q) = %+v (%v), want %+v", form, got, err, want)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := SplitForm(database, "character:miku"); err == nil {
		t.Errorf("a failed category read gave %+v, want the error", got)
	}
	if got, err := SplitForm(database, "solo"); err != nil || got.Name != "solo" {
		t.Errorf("a bare name read the catalog: %+v (%v)", got, err)
	}
}

func TestNormalizeForm_ElidesTheGeneralCategory(t *testing.T) {
	_, svc := setupTestDB(t)
	for form, want := range map[string]string{
		"general:Long Hair": "long_hair",
		"Long Hair":         "long_hair",
		"character:Miku":    "character:miku",
	} {
		if got, ok := svc.NormalizeForm(form); !ok || got != want {
			t.Errorf("NormalizeForm(%q) = %q (%t), want %q", form, got, ok, want)
		}
	}
	if _, ok := svc.NormalizeForm(`bad"name`); ok {
		t.Error("a name with a quote normalized")
	}
	if got := FormName("general", "solo"); got != "solo" {
		t.Errorf("FormName(general, solo) = %q", got)
	}
}

func TestAliasForms_KeysTheFoldedSpellingToo(t *testing.T) {
	_, svc := setupTestDB(t)
	character := categoryIDOf(t, svc, "character")
	miku := mustTag(t, svc, "miku", character)
	mustCreateAlias(t, svc, "hatsune_miku(v3)", character, miku.ID)
	byTag, byAlias, err := svc.AliasForms([]int64{miku.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(byTag["character:miku"], []string{"character:hatsune_miku(v3)"}) {
		t.Errorf("byTag = %v", byTag)
	}
	for _, form := range []string{"character:hatsune_miku(v3)", LegacyFold("character:hatsune_miku(v3)")} {
		if byAlias[form] != "character:miku" {
			t.Errorf("byAlias[%q] = %q, want character:miku", form, byAlias[form])
		}
	}
}

func TestWouldAddImplication(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	cat := mustTag(t, svc, "cat_ears", general)
	animal := mustTag(t, svc, "animal_ears", general)
	mustCreateAlias(t, svc, "kitty_ears", general, cat.ID)
	mustAddImply(t, svc, cat.ID, animal.ID)
	for form, want := range map[string]bool{
		"animal_ears": false,
		"kitty_ears":  false,
		"ears":        true,
		`bad"name`:    false,
	} {
		if got := svc.WouldAddImplication(cat.ID, form, true); got != want {
			t.Errorf("WouldAddImplication(cat_ears -> %q) = %t, want %t", form, got, want)
		}
	}
	if !svc.WouldAddImplication(cat.ID, "animal_ears", false) {
		t.Error("the reverse edge is not declared, so it would add one")
	}
	if svc.FormFree("kitty_ears") || !svc.FormFree("fox_ears") || !svc.FormExists("kitty_ears") {
		t.Error("an alias's spelling reads as free, or a new one as taken")
	}
}

func TestPlainForms_LeavesOutAliasesAndRatings(t *testing.T) {
	database, svc := setupTestDB(t)
	character := categoryIDOf(t, svc, "character")
	miku := mustTag(t, svc, "miku", character)
	alias := mustCreateAlias(t, svc, "hatsune", character, miku.ID)
	forms, err := svc.PlainForms([]int64{miku.ID, alias.ID, ratingTagIDByName(t, database, "general")})
	if err != nil {
		t.Fatal(err)
	}
	if len(forms) != 1 || forms[0] != (TagForm{ID: miku.ID, Form: "character:miku"}) {
		t.Errorf("forms = %+v, want only character:miku", forms)
	}
}

func TestStorageForms_KeepsTheContextApart(t *testing.T) {
	rows := []models.ImageTag{
		{TagID: 1, TagName: "solo", Category: "general"},
		{TagID: 2, TagName: "animal_ears", Category: "general", IsImplied: true},
		{TagID: 3, TagName: "explicit", Category: "rating"},
		{TagID: 4, TagName: "highres", Category: "meta", TaggerName: models.TagSourceMonbooru},
		{TagID: 5, TagName: "absurdres", Category: "meta", TaggerName: models.TagSourceMonbooru},
	}
	ledger := map[int64][]TagSource{5: {{Source: models.TagSourceMonbooru}, {Source: "danbooru"}}}
	storage, context := StorageForms(rows, ledger)
	if !slices.Equal(storage, []string{"solo", "meta:absurdres"}) {
		t.Errorf("storage = %v", storage)
	}
	if !slices.Equal(context, []string{"animal_ears", "rating:explicit", "meta:highres"}) {
		t.Errorf("context = %v", context)
	}
}

func TestTagNameVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "shared", "contract", "testdata", "tagnames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Names, Forms []struct{ In, Out string } }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Names {
		if got := NormalizeName(c.In); got != c.Out {
			t.Errorf("NormalizeName(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	database, _ := setupTestDB(t)
	for _, c := range v.Forms {
		f, err := SplitForm(database, c.In)
		if err != nil {
			t.Fatal(err)
		}
		got := NormalizeName(f.Name)
		switch {
		case f.CategoryID == 0:
			got = NormalizeName(c.In)
		case got == "":
		case f.Category != "general":
			got = f.Category + ":" + got
		}
		if got != c.Out {
			t.Errorf("form %q = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestTheSchemaSeedsTheBuiltinCategories(t *testing.T) {
	schema, err := os.ReadFile(filepath.Join("..", "db", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var seeded []string
	for _, m := range regexp.MustCompile(`\('(\w+)',\s+'#[0-9a-f]{6}', 1\)`).FindAllStringSubmatch(string(schema), -1) {
		seeded = append(seeded, m[1])
	}
	if !slices.Equal(seeded, contract.BuiltinCategories) {
		t.Errorf("schema.sql seeds %v, contract.BuiltinCategories is %v", seeded, contract.BuiltinCategories)
	}
}
