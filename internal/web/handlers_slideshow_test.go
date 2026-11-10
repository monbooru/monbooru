package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
)

type slideshowResult struct {
	IDs    []int64 `json:"ids"`
	Videos []int64 `json:"videos"`
	Start  int     `json:"start"`
}

func decodeSlideshow(t *testing.T, w *httptest.ResponseRecorder) slideshowResult {
	t.Helper()
	mustCode(t, w, http.StatusOK)
	var got slideshowResult
	mustUnmarshal(t, w.Body.Bytes(), &got)
	return got
}

func TestSlideshow_SearchPlaysInViewOrder(t *testing.T) {
	srv := newTestServer(t)
	small := seedImage(t, srv, "small.png", 10, 10)
	big := seedImage(t, srv, "big.png", 11, 11)
	mid := seedImage(t, srv, "mid.png", 12, 12)
	mustExec(t, srv.db().Write, `UPDATE images SET file_size = 100 WHERE id = ?`, small)
	mustExec(t, srv.db().Write, `UPDATE images SET file_size = 300 WHERE id = ?`, big)
	mustExec(t, srv.db().Write, `UPDATE images SET file_size = 200 WHERE id = ?`, mid)

	got := decodeSlideshow(t, postForm(t, srv, "/internal/slideshow", url.Values{
		"scope": {"search"}, "q": {""}, "sort": {"filesize"}, "order": {"desc"},
	}))
	if want := []int64{big, mid, small}; !slices.Equal(got.IDs, want) {
		t.Errorf("ids = %v, want %v", got.IDs, want)
	}
}

func TestSlideshow_SearchHonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, _ := seedRatedPair(t, srv)

	got := decodeSlideshow(t, postCeilingRequest(t, srv, "/internal/slideshow", url.Values{"scope": {"search"}, "q": {""}}))
	if !slices.Equal(got.IDs, []int64{safe}) {
		t.Errorf("ids = %v, want only the image under the ceiling", got.IDs)
	}
}

func TestSlideshow_SelectionKeepsPickOrderAndDropsWhatCannotPlay(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	clip := seedImage(t, srv, "clip.png", 11, 11)
	archive := seedImage(t, srv, "archive.png", 12, 12)
	gone := seedImage(t, srv, "gone.png", 13, 13)
	b := seedImage(t, srv, "b.png", 14, 14)
	mustExec(t, srv.db().Write, `UPDATE images SET file_type = 'webm' WHERE id = ?`, clip)
	mustExec(t, srv.db().Write, `UPDATE images SET file_type = 'cbz' WHERE id = ?`, archive)
	mustExec(t, srv.db().Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)

	got := decodeSlideshow(t, postForm(t, srv, "/internal/slideshow", url.Values{
		"scope": {"selection"}, "ids": {idCSV(b, archive, clip, gone, a)},
	}))
	if want := []int64{b, clip, a}; !slices.Equal(got.IDs, want) {
		t.Errorf("ids = %v, want %v", got.IDs, want)
	}
	if !slices.Equal(got.Videos, []int64{clip}) {
		t.Errorf("videos = %v, want [%d]", got.Videos, clip)
	}
}

func TestSlideshow_StartsAtThePlaceOfTheStartImage(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	archive := seedImage(t, srv, "archive.png", 11, 11)
	b := seedImage(t, srv, "b.png", 12, 12)
	last := seedImage(t, srv, "last.png", 13, 13)
	mustExec(t, srv.db().Write, `UPDATE images SET file_type = 'cbz' WHERE id IN (?, ?)`, archive, last)
	const outside = 999999

	for _, c := range []struct {
		name        string
		start, next int64
		want        int
	}{
		{"a playable start", b, 0, 1},
		{"an archive", archive, 0, 1},
		{"an archive past the last playable item", last, 0, 1},
		{"a start outside the scope falls back to next", outside, b, 1},
		{"neither in the scope", outside, outside, 0},
		{"no start", 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			form := url.Values{"scope": {"selection"}, "ids": {idCSV(a, archive, b, last)}}
			if c.start != 0 {
				form.Set("start", strconv.FormatInt(c.start, 10))
			}
			if c.next != 0 {
				form.Set("next", strconv.FormatInt(c.next, 10))
			}
			got := decodeSlideshow(t, postForm(t, srv, "/internal/slideshow", form))
			if !slices.Equal(got.IDs, []int64{a, b}) || got.Start != c.want {
				t.Errorf("ids = %v start = %d, want [%d %d] start %d", got.IDs, got.Start, a, b, c.want)
			}
		})
	}
}

func TestSlideshow_EmptyScopeAnswersEmptyLists(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/internal/slideshow", url.Values{"scope": {"search"}, "q": {""}})
	mustCode(t, w, http.StatusOK)
	if body := w.Body.String(); body != "{\"ids\":[],\"start\":0,\"videos\":[]}\n" {
		t.Errorf("body = %q, want empty lists, not null", body)
	}
}
