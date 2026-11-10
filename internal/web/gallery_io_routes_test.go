package web

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

func imageCount(t *testing.T, srv *Server, gallery string) int {
	t.Helper()
	return countRows(t, srv.get(gallery).DB, "images")
}

func TestBatchTransfer_CopiesTheScopeAndKeepsTheSource(t *testing.T) {
	srv := newMultiGalleryServer(t)
	a := seedImage(t, srv, "one.png", 10, 10)
	b := seedImage(t, srv, "two.png", 11, 11)

	w := postForm(t, srv, "/internal/batch-transfer", url.Values{
		"scope":  {"selection"},
		"ids":    {strconv.FormatInt(a, 10), strconv.FormatInt(b, 10)},
		"target": {"stock"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if got := imageCount(t, srv, "stock"); got != 2 {
		t.Errorf("target holds %d images, want 2", got)
	}
	if got := imageCount(t, srv, "default"); got != 2 {
		t.Errorf("source holds %d images, want both left in place without remove_after", got)
	}
}

func TestBatchTransfer_RefusesATargetThatIsNotAnotherLiveGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	id := seedImage(t, srv, "one.png", 10, 10)

	for _, target := range []string{"", "default", "nosuchgallery"} {
		w := postForm(t, srv, "/internal/batch-transfer", url.Values{
			"scope":  {"selection"},
			"ids":    {strconv.FormatInt(id, 10)},
			"target": {target},
		})
		wantCode(t, w, http.StatusBadRequest)
	}
	if got := imageCount(t, srv, "stock"); got != 0 {
		t.Errorf("a refused transfer put %d images in the target", got)
	}
}
