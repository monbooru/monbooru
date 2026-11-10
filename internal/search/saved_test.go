package search

import (
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func TestSavedSearches_RoundTripAndRefuseATakenName(t *testing.T) {
	database, _ := setupSearchDB(t)
	for _, name := range []string{"zeta", "alpha"} {
		if err := SaveSearch(database, models.SavedSearch{Name: name, Query: "tag", Sort: "newest", Order: "desc", Seed: "7"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveSearch(database, models.SavedSearch{Name: "alpha", Query: "other"}); err == nil {
		t.Error("a taken name overwrote its saved search")
	}
	got, err := SavedSearches(database)
	if err != nil || len(got) != 2 || got[0].Name != "alpha" || got[0].Seed != "7" || got[1].Name != "zeta" {
		t.Fatalf("saved searches = %+v (%v), want alpha then zeta", got, err)
	}
	if err := DeleteSavedSearch(database, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := SavedSearches(database); len(left) != 1 || left[0].Name != "zeta" {
		t.Errorf("after the delete = %+v, want only zeta", left)
	}
}
