package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func pairStubMonloader(t *testing.T, srv *Server, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	stub := httptest.NewServer(handler)
	t.Cleanup(stub.Close)
	tok, _ := apitoken.New("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfgMu.Lock()
	srv.cfg.Auth.Tokens = []apitoken.Token{tok}
	srv.cfg.Monloader.APIToken = "ml-token"
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfgMu.Unlock()
	return stub
}

func seedLookupImage(t *testing.T, srv *Server, id int64) {
	t.Helper()
	mustExec(t, srv.db().Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, '/g/a.png', 'png', 1)`, id, strings.Repeat("a", 63)+string(rune('0'+id)))
}

func TestManualLookupRecordsHistory(t *testing.T) {
	srv := newTestServer(t)
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/lookup" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job_id":91}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	cx := srv.active()
	seedLookupImage(t, srv, 1)

	form := strings.NewReader("id=1&backend=ptr")
	req := httptest.NewRequest("POST", "/images/1/lookup", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "1")
	rr := httptest.NewRecorder()
	srv.lookupImage(rr, req)
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusOK {
		t.Fatalf("lookup returned %d: %s", rr.Code, rr.Body.String())
	}

	rows, err := lookup.ForImage(cx.DB, 1)
	if err != nil {
		t.Fatal(err)
	}
	row := rows[contract.LookupPTR]
	if row.QueuedAt.IsZero() {
		t.Errorf("row = %+v, want the attempt recorded in flight", row)
	}
	if _, ok := rows[contract.LookupBooru]; ok {
		t.Errorf("rows = %v, want only the targeted backend recorded", rows)
	}
}

func TestBackendStatusReadsWhatHappensNext(t *testing.T) {
	day := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	soon := time.Now().Add(48 * time.Hour)
	on := lookupBackendView{Backend: contract.LookupBooru, Enabled: true, ScheduleOn: true}
	with := func(f func(*lookupBackendView)) lookupBackendView {
		v := on
		f(&v)
		return v
	}
	for _, tc := range []struct {
		name string
		view lookupBackendView
		want lookupStatus
	}{
		{"phase off in settings", lookupBackendView{Enabled: true}, lookupStatus{Text: "schedule off"}},
		{"gallery left out", with(func(v *lookupBackendView) { v.GalleryOff = true }), lookupStatus{Text: "off for this gallery"}},
		{"opted out", lookupBackendView{ScheduleOn: true}, lookupStatus{Text: "not scheduled"}},
		{"never tried", on, lookupStatus{Text: "due now"}},
		{"in flight", with(func(v *lookupBackendView) { v.QueuedAt = day }), lookupStatus{Text: "queued", Tone: "queued"}},
		{"ladder spent", with(func(v *lookupBackendView) { v.Exhausted = true }), lookupStatus{Text: "nothing found", Tone: "verdict"}},
		{"matched", with(func(v *lookupBackendView) { v.LastAt, v.LastResult = day, lookup.ResultHit }), lookupStatus{Text: "matched " + localDay(day), Tone: "hit"}},
		{"waiting out a backoff", with(func(v *lookupBackendView) { v.NextDueAt = soon }), lookupStatus{Text: "due " + localDay(soon)}},
		{"backoff elapsed", with(func(v *lookupBackendView) { v.NextDueAt = time.Now().Add(-time.Hour) }), lookupStatus{Text: "due now"}},
	} {
		if got := tc.view.Status(); got != tc.want {
			t.Errorf("%s: Status() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestBackendRetriesOnlyWhatTheScheduleRuns(t *testing.T) {
	due := lookupBackendView{Enabled: true, ScheduleOn: true, NextDueAt: time.Now()}
	with := func(f func(*lookupBackendView)) lookupBackendView {
		v := due
		f(&v)
		return v
	}
	for _, tc := range []struct {
		name string
		view lookupBackendView
		want bool
	}{
		{"scheduled", due, true},
		{"phase off in settings", with(func(v *lookupBackendView) { v.ScheduleOn = false }), false},
		{"gallery left out", with(func(v *lookupBackendView) { v.GalleryOff = true }), false},
		{"opted out", with(func(v *lookupBackendView) { v.Enabled = false }), false},
		{"nothing due", with(func(v *lookupBackendView) { v.NextDueAt = time.Time{} }), false},
	} {
		if got := tc.view.Retrying(); got != tc.want {
			t.Errorf("%s: Retrying() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
