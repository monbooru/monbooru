package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestFetchStatusHandler_PendingThenReloadOnOk(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.Record(g, 7, "pending", "")
	rec := do(srv, "GET", "/internal/images/7/fetch-status?n=0")
	mustCode(t, rec, http.StatusOK)
	if got := rec.Header().Get("HX-Refresh"); got != "" {
		t.Errorf("pending set HX-Refresh=%q, want empty", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Fetching data") || !strings.Contains(body, "hx-get") {
		t.Errorf("pending body = %q, want the polling pill", body)
	}

	srv.ml.Fetches.Record(g, 7, "ok", "")
	rec = do(srv, "GET", "/internal/images/7/fetch-status?n=1")
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Errorf("ok HX-Refresh = %q, want true", rec.Header().Get("HX-Refresh"))
	}
	wantIn(t, rec.Header().Get("HX-Trigger"), "action-flash")
	if _, ok := srv.ml.Fetches.Load(g, 7); ok {
		t.Error("ok outcome should be cleared once the poll consumes it")
	}
}

func TestFetchStatusHandler_SurfacesReportedFailure(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.Record(g, 11, "unsupported_url", "no suitable extractor found")
	rec := do(srv, "GET", "/internal/images/11/fetch-status?n=3")
	body := rec.Body.String()
	if !strings.Contains(body, "flash-err") || !strings.Contains(body, "fetch this source URL") {
		t.Errorf("reported failure body = %q, want a mapped inline error", body)
	}
	wantNotIn(t, body, "hx-get")
	if _, ok := srv.ml.Fetches.Load(g, 11); ok {
		t.Error("reported failure should be cleared after the poll consumes it")
	}
}

func TestFetchStatusHandler_HashNotFoundListsTrailAndHashes(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.RecordLookup(g, 12, "md5 0123abcd")
	srv.ml.Fetches.Record(g, 12, "hash_not_found", "Public Tag Repository: no match; danbooru: no match")
	rec := do(srv, "GET", "/internal/images/12/fetch-status?n=1")
	body := rec.Body.String()
	for _, want := range []string{"flash-warn", "No online source found",
		"<div>Public Tag Repository: no match</div>", "<li>danbooru: no match</li>", "Searched md5 0123abcd",
		"compressed or re-encoded", "similarity lookup service",
		`id="fetch-status"`, "hx-swap-oob"} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, "<li>Public Tag Repository")
	wantNotIn(t, body, "hx-get")

	srv.ml.Fetches.Record(g, 12, "hash_not_found", "")
	rec = do(srv, "GET", "/internal/images/12/fetch-status?n=1")
	if body := rec.Body.String(); !strings.Contains(body, "No source found; no tags found.") {
		t.Errorf("empty-trail body = %q, want the fallback sentence", body)
	}
}

func TestFetchStatusHandler_MissHintOnlyWithoutSimilarity(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.Record(g, 13, "hash_not_found", "danbooru: no match; iqdb: no match")
	rec := do(srv, "GET", "/internal/images/13/fetch-status?n=1")
	if body := rec.Body.String(); strings.Contains(body, "similarity lookup service") {
		t.Errorf("miss body = %q, want no hint when iqdb answered", body)
	}

	srv.ml.Fetches.Record(g, 13, "hash_not_found", "danbooru: no match; saucenao: skipped, needs api key")
	rec = do(srv, "GET", "/internal/images/13/fetch-status?n=1")
	if body := rec.Body.String(); !strings.Contains(body, "similarity lookup service") {
		t.Errorf("miss body = %q, want the hint when the only similarity entry is a skip", body)
	}
}

func TestFetchStatusHandler_PTROnlyMissOffersNoSimilarityService(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.Record(g, 15, "hash_not_found", "Public Tag Repository: no match")
	body := do(srv, "GET", "/internal/images/15/fetch-status?n=1").Body.String()
	wantIn(t, body, "compressed or re-encoded")
	wantNotIn(t, body, "similarity lookup service")
}

func TestFetchStatusHandler_PTRMatchAndCandidateLinks(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.RecordLookup(g, 14, "md5 0123abcd, sha256 4567ef")
	srv.ml.Fetches.Record(g, 14, "hash_not_found",
		"Public Tag Repository: match, tags applied; danbooru: no match; "+
			"iqdb: no match, closest candidates: https://danbooru.donmai.us/posts/1 (62%), https://yande.re/post/show/2 (58%)")
	rec := do(srv, "GET", "/internal/images/14/fetch-status?n=1")
	body := rec.Body.String()
	for _, want := range []string{
		"<div>Public Tag Repository: match, tags applied (reload the page to see them)</div>",
		"No online source found",
		"<li>danbooru: no match</li>",
		`<a href="https://danbooru.donmai.us/posts/1" target="_blank" rel="noopener">danbooru.donmai.us</a> (62%)`,
		`<a href="https://yande.re/post/show/2" target="_blank" rel="noopener">yande.re</a> (58%)`,
	} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, "similarity lookup service")
	if got := rec.Header().Get("HX-Refresh"); got != "" {
		t.Errorf("HX-Refresh = %q, want empty (the reload would wipe the trail)", got)
	}
}

func TestFetchStatusHandler_SurfacesMismatch(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeGallery()

	srv.ml.Fetches.Record(g, 9, "mismatch", "the source returned a different file (hash mismatch); no tags applied")
	rec := do(srv, "GET", "/internal/images/9/fetch-status?n=2")
	if got := rec.Header().Get("HX-Refresh"); got != "" {
		t.Errorf("mismatch set HX-Refresh=%q, want empty (no reload)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "flash-err") || !strings.Contains(body, "hash mismatch") {
		t.Errorf("mismatch body = %q, want an inline error flash", body)
	}
	wantNotIn(t, body, "hx-get")
	if _, ok := srv.ml.Fetches.Load(g, 9); ok {
		t.Error("mismatch outcome should be cleared after the poll consumes it")
	}
}

func TestFetchStatusHandler_NotInFlightStopsPolling(t *testing.T) {
	srv := newTestServer(t)
	rr := do(srv, "GET", "/internal/images/12/fetch-status?n=1")
	mustCode(t, rr, http.StatusOK)
	if body := rr.Body.String(); strings.Contains(body, "hx-get") || strings.TrimSpace(body) != "" {
		t.Errorf("not-in-flight poll should return an empty non-polling body, got %q", body)
	}
}

func TestFetchStatusHandler_PollCapStopsPolling(t *testing.T) {
	srv := newTestServer(t)
	srv.ml.Fetches.Record(srv.activeGallery(), 12, "pending", "")
	rr := do(srv, "GET", "/internal/images/12/fetch-status?n=30")
	body := rr.Body.String()
	wantNotIn(t, body, "hx-get")
	wantIn(t, body, "Still fetching")
}
