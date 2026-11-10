package gallery

import (
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/tags"
)

func TestResolveTagNames_ReadsTheSpellingAsTheAddBoxDoes(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids, warnings, _, err := ResolveTagNames(database, tags.New(database, nil), []string{"artist:", "Artist:Foo", "re:zero"}, "api")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "empty tag name after category prefix") {
		t.Errorf("warnings = %q, want the empty-name refusal for artist:", warnings)
	}
	var got []string
	for _, id := range ids {
		var name, category string
		if err := database.Read.QueryRow(
			`SELECT t.name, c.name FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE t.id = ?`, id,
		).Scan(&name, &category); err != nil {
			t.Fatal(err)
		}
		got = append(got, category+":"+name)
	}
	if want := []string{"artist:foo", "general:re:zero"}; !slices.Equal(got, want) {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

func TestResolveTagNames_ListsARejectedNameAndSkipsIt(t *testing.T) {
	database := newCollectionsTestDB(t)
	svc := tags.New(database, nil)
	general, _, _ := tags.CategoryIDByName(database, "general")
	if _, err := svc.RejectTagName("watermark", general); err != nil {
		t.Fatal(err)
	}
	ids, warnings, rejected, err := ResolveTagNames(database, svc, []string{"watermark", "kept"}, "danbooru")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err = %v, warnings = %v", err, warnings)
	}
	if len(ids) != 1 || !slices.Equal(rejected, []string{"watermark"}) {
		t.Errorf("ids = %v, rejected = %v, want kept alone and watermark listed", ids, rejected)
	}
}
