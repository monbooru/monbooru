package web

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

func TestBatchLookupCount_SplitsTheScopeBySource(t *testing.T) {
	srv := newTestServer(t)
	sourced := []int64{
		seedImage(t, srv, "sourced_a.png", 10, 10),
		seedImage(t, srv, "sourced_b.png", 11, 11),
	}
	unsourced := []int64{
		seedImage(t, srv, "bare_a.png", 12, 12),
		seedImage(t, srv, "bare_b.png", 13, 13),
		seedImage(t, srv, "bare_c.png", 14, 14),
	}
	for i, id := range sourced {
		mustExec(t, srv.db().Write,
			`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'danbooru', ?, ?)`,
			id, strconv.Itoa(i), "https://danbooru.donmai.us/posts/"+strconv.Itoa(i))
	}

	mustExec(t, srv.db().Write,
		`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'manual', '', '')`, unsourced[0])

	all := append(append([]int64{}, sourced...), unsourced...)
	w := postForm(t, srv, "/internal/batch-lookup/count", url.Values{
		"scope": {"selection"}, "ids": {idCSV(all...)},
	})
	mustCode(t, w, http.StatusOK)

	var got map[string]int
	mustUnmarshal(t, w.Body.Bytes(), &got)
	want := map[string]int{"total": 5, "sourced": 2, "unsourced": 3}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (got %v)", k, got[k], v, got)
		}
	}
}
