package tags

import (
	"slices"
	"testing"
)

func TestGlobalTagsInTheListingAndTheCleanup(t *testing.T) {
	database, svc := setupTestDB(t)
	shared, err := svc.GetOrCreateTag("animation", svc.generalCatID)
	if err != nil {
		t.Fatal(err)
	}
	local, err := svc.GetOrCreateTag("watercolor", svc.generalCatID)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := svc.CreateAlias("anime", svc.generalCatID, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE tags SET is_global = 1 WHERE id = ?`, shared.ID); err != nil {
		t.Fatal(err)
	}

	ids := func(scope string) []int64 {
		t.Helper()
		got, err := svc.ListTagIDs(TagFilter{Scope: scope, Type: "all", ShowZero: true, CategoryID: &svc.generalCatID})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		return got
	}
	if got, want := ids("global"), []int64{shared.ID, alias.ID}; !slices.Equal(got, want) {
		t.Errorf("global scope = %v, want the tag and its alias %v", got, want)
	}
	if got, want := ids("local"), []int64{local.ID}; !slices.Equal(got, want) {
		t.Errorf("local scope = %v, want %v", got, want)
	}

	for id, want := range map[int64]bool{shared.ID: true, alias.ID: true, local.ID: false} {
		got, err := svc.GetTag(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Global != want {
			t.Errorf("tag %s Global = %v, want %v", got.Name, got.Global, want)
		}
	}

	if _, err := database.Write.Exec(`DELETE FROM tags WHERE id = ?`, alias.ID); err != nil {
		t.Fatal(err)
	}
	plain, kept, err := svc.UnusedTags()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(kept, shared.ID) || slices.Contains(plain, shared.ID) {
		t.Errorf("an unused global tag: plain %v, kept %v; want it kept", plain, kept)
	}
	if !slices.Contains(plain, local.ID) {
		t.Errorf("an unused local tag is not in plain %v", plain)
	}
}
