package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
)

func TestListCollections_CountsSortsFiltersAndPages(t *testing.T) {
	env := newTestEnv(t)
	size := 10
	file := func(name, collection string) int64 {
		t.Helper()
		size++
		id := env.createTestImage(t, name, size, size)
		body := fmt.Appendf(nil, `{"collection":%q}`, collection)
		mustCode(t, doJSON(env.mux, "PATCH", fmt.Sprintf("/api/v1/images/%d", id), body), http.StatusOK)
		return id
	}
	file("a1.png", "Beach Trip")
	file("a2.png", "Beach Trip")
	file("a3.png", "Beach Trip")
	file("b1.png", "alps")
	gone := file("c1.png", "Lost Album")
	mustExec(t, env.database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)

	type page struct {
		Total   int                  `json:"total"`
		Results []collectionResponse `json:"results"`
	}
	list := func(query string) page {
		t.Helper()
		w := env.do("GET", "/api/v1/collections?"+query)
		mustCode(t, w, http.StatusOK)
		var p page
		mustDecode(t, w, &p)
		return p
	}

	if got := list(""); got.Total != 2 || !slices.Equal(got.Results, []collectionResponse{{"Beach Trip", 3}, {"alps", 1}}) {
		t.Errorf("default = %+v, want both live collections, biggest first", got)
	}
	if got := list("sort=name"); !slices.Equal(got.Results, []collectionResponse{{"alps", 1}, {"Beach Trip", 3}}) {
		t.Errorf("sort=name = %+v, want case-insensitive name order", got)
	}
	if got := list("q=BEACH"); got.Total != 1 || len(got.Results) != 1 || got.Results[0].Name != "Beach Trip" {
		t.Errorf("q=BEACH = %+v, want the one match", got)
	}
	if got := list("limit=1&page=2"); got.Total != 2 || len(got.Results) != 1 || got.Results[0].Name != "alps" {
		t.Errorf("page 2 = %+v, want the second collection", got)
	}
	if got := list("q=nothing_like_it"); got.Total != 0 || got.Results == nil || len(got.Results) != 0 {
		t.Errorf("no match = %+v, want an empty page", got)
	}
}
