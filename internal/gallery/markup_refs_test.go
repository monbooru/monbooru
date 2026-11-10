package gallery

import (
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func insertTag(t *testing.T, database *db.DB, name, category string, usage int) int64 {
	t.Helper()
	var catID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, category).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	res, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?)`, name, catID, usage)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestResolveTagRefs(t *testing.T) {
	database := newCollectionsTestDB(t)
	insertTag(t, database, "hulk", "character", 5)
	insertTag(t, database, "shared", "character", 2)
	insertTag(t, database, "shared", "copyright", 9)
	canonical := insertTag(t, database, "lapis_lazuli", "character", 3)
	alias := insertTag(t, database, "lapis", "general", 0)
	mustExec(t, database.Write, `UPDATE tags SET is_alias = 1, canonical_tag_id = ? WHERE id = ?`, canonical, alias)

	got := ResolveTagRefs(database, []string{"hulk", "shared", "character:shared", "lapis", "nowhere"})

	if !got["hulk"].Found || got["hulk"].Color != "#00aa00" {
		t.Errorf("hulk = %+v", got["hulk"])
	}
	if got["shared"].Color != "#aa00aa" {
		t.Errorf("unqualified shared = %+v, want the copyright colour", got["shared"])
	}
	if got["character:shared"].Color != "#00aa00" {
		t.Errorf("qualified shared = %+v, want the character colour", got["character:shared"])
	}
	if got["lapis"].Color != "#00aa00" {
		t.Errorf("alias = %+v, want its canonical row's colour", got["lapis"])
	}
	if got["nowhere"].Found {
		t.Errorf("nowhere = %+v, want not found", got["nowhere"])
	}
}

func TestResolveTagRefs_ACategoryPrefixWinsOverTheLiteralName(t *testing.T) {
	database := newCollectionsTestDB(t)
	insertTag(t, database, "artist:foo", "general", 50)
	insertTag(t, database, "foo", "artist", 1)

	got := ResolveTagRefs(database, []string{"artist:foo", "Artist:foo", "artist:nowhere"})

	for _, ref := range []string{"artist:foo", "Artist:foo"} {
		if got[ref].Color != "#cc0000" {
			t.Errorf("%s = %+v, want the artist colour", ref, got[ref])
		}
	}
	if got["artist:nowhere"].Found {
		t.Errorf("artist:nowhere = %+v, want not found", got["artist:nowhere"])
	}
}

func TestExistingImageIDs(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	got := ExistingImageIDs(database, []int64{id, id + 999})
	if !got[id] || got[id+999] {
		t.Fatalf("got %v", got)
	}
	if len(ExistingImageIDs(database, nil)) != 0 {
		t.Fatal("empty input should query nothing")
	}
}

func TestImageIDsBySourceURL(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	mustAddSource(t, database, id, "danbooru", "7", "https://danbooru.example/posts/7")
	got := ImageIDsBySourceURL(database, []string{
		"https://danbooru.example/posts/7", "https://danbooru.example/posts/8",
	})
	if got["https://danbooru.example/posts/7"] != id {
		t.Fatalf("got %v, want the local image for the held URL", got)
	}
	if _, ok := got["https://danbooru.example/posts/8"]; ok {
		t.Fatal("a URL the gallery does not hold should not resolve")
	}
}
