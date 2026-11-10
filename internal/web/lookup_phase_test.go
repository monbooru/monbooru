package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

type ptrStubHandler struct {
	index     uint64
	hits      map[string][]string
	batches   [][]string
	ids       []int64
	scheduled []bool
}

func (p *ptrStubHandler) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/ptr/status":
		_, _ = w.Write([]byte(`{"enabled":true,"state":"ready","progress":{"update_index":` +
			strconv.FormatUint(p.index, 10) + `}}`))
	case "/api/v1/ptr/lookup":
		var req contract.PTRLookupRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		batch := make([]string, len(req.Images))
		results := map[string][]string{}
		for i, img := range req.Images {
			batch[i] = img.SHA256
			p.ids = append(p.ids, img.ImageID)
			if tags, ok := p.hits[img.SHA256]; ok {
				results[img.SHA256] = tags
			}
		}
		p.batches = append(p.batches, batch)
		p.scheduled = append(p.scheduled, req.Scheduled)
		out, _ := json.Marshal(contract.PTRLookupResponse{Index: p.index, Results: results})
		_, _ = w.Write(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func seedPhaseImage(t *testing.T, srv *Server, id int64, sha string) {
	t.Helper()
	mustExec(t, srv.db().Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 1)`, id, sha, "/g/img"+sha+".png")
}

func TestScheduledLookupsReachOnlyTheirGalleries(t *testing.T) {
	stub := &ptrStubHandler{index: 4812}
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "photos", filepath.Join(t.TempDir(), "photos"))
	pairStubMonloader(t, srv, stub.handle)
	srv.ml.Cache.Store(monloader.Status{Conn: "ok", PTR: true})
	seedPhaseImage(t, srv, 1, strings.Repeat("a", 64))
	mustExec(t, srv.get("photos").DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (1, ?, '/p/img.png', 'png', 1)`, strings.Repeat("b", 64))
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time: "01:00", LookupPTR: true, LookupBooru: true,
		Galleries: map[string][]string{config.ActionLookupPTR: {"default"}, config.ActionLookupBooru: {}},
	}
	srv.cfgMu.Unlock()

	srv.sched.RunNow()

	if len(stub.batches) != 1 || !slices.Equal(stub.batches[0], []string{strings.Repeat("a", 64)}) {
		t.Errorf("batches = %v, want the default gallery's image alone", stub.batches)
	}
	if n := countRows(t, srv.get("photos").DB, "image_lookups"); n != 0 {
		t.Errorf("photos has %d lookup row(s), want none", n)
	}
	lines := srv.sched.Status().LookupInfo
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "[default] PTR lookup") {
		t.Errorf("status lines = %q, want the default gallery's PTR line alone", lines)
	}
}

func TestScheduledOnlineLookupQueuesOnTheBudgetAndReportsItsLine(t *testing.T) {
	srv := newTestServer(t)
	var mu sync.Mutex
	var posted []map[string]any
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		posted = append(posted, req)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":7}`))
	})
	srv.ml.Cache.Store(monloader.Status{Conn: "ok"})
	mustExec(t, srv.db().Write, `INSERT INTO images (id, sha256, md5, canonical_path, file_type, file_size) VALUES (1, ?, ?, '/g/a.png', 'png', 1)`,
		strings.Repeat("a", 64), strings.Repeat("a", 32))
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", LookupBooru: true}
	srv.cfgMu.Unlock()

	srv.sched.RunNow()

	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0]["image_id"] != float64(1) || posted[0]["background"] != true || posted[0]["budgeted"] != true {
		t.Errorf("posted = %v, want image 1 on the background lane and the budget", posted)
	}
	lines := srv.sched.Status().LookupInfo
	if want := []string{"[default] Online lookup: 1 queued on monloader."}; !slices.Equal(lines, want) {
		t.Errorf("status lines = %q, want %q", lines, want)
	}
}

func TestScheduledLookupSkipLinesNameOnlyTheirGalleries(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "photos", filepath.Join(t.TempDir(), "photos"))
	pairStubMonloader(t, srv, (&ptrStubHandler{}).handle)
	srv.ml.Cache.Store(monloader.Status{Conn: "down"})
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time: "01:00", LookupPTR: true,
		Galleries: map[string][]string{config.ActionLookupPTR: {"photos"}},
	}
	srv.cfgMu.Unlock()

	srv.sched.RunNow()

	if lines := srv.sched.Status().LookupInfo; len(lines) != 1 || !strings.HasPrefix(lines[0], "[photos] Lookup skipped") {
		t.Errorf("status lines = %q, want one skip line for photos", lines)
	}
}

func TestLookupPhasesSleepWhileMonloaderIsUnpaired(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", LookupPTR: true, LookupBooru: true}
	srv.cfgMu.Unlock()

	srv.sched.RunNow()
	if lines := srv.sched.Status().LookupInfo; len(lines) != 0 {
		t.Errorf("status lines = %q, want none while unpaired", lines)
	}
	wantIn(t, srv.sched.Status().NextRunNote, "every action is off")

	pairStubMonloader(t, srv, (&ptrStubHandler{}).handle)
	if _, ok := srv.sched.NextFire(time.Now()); !ok {
		t.Error("the lookups did not come back with the pairing")
	}
}

func TestLookupDueFollowsTheGalleryLists(t *testing.T) {
	srv := newTestServer(t)
	pairStubMonloader(t, srv, (&ptrStubHandler{index: 1}).handle)
	srv.ml.Cache.Store(monloader.Status{Conn: "ok", PTR: true})
	schedule := func(ptr []string) {
		srv.cfgMu.Lock()
		srv.cfg.Schedule = config.ScheduleConfig{
			Time: "01:00", LookupPTR: true, LookupBooru: true,
			Galleries: map[string][]string{config.ActionLookupPTR: ptr, config.ActionLookupBooru: {}},
		}
		srv.cfgMu.Unlock()
	}

	schedule([]string{"photos"})
	w := postForm(t, srv, "/settings/maintenance/lookup-due", url.Values{})
	wantIn(t, w.Body.String(), "lookups are off for gallery &#34;default&#34; in Settings &gt; Schedule")
	if srv.jobs.IsRunning() {
		t.Error("a refused lookup started a job")
	}

	schedule([]string{"default"})
	wantIn(t, postForm(t, srv, "/settings/maintenance/lookup-due", url.Values{}).Body.String(), "Lookup started")
	awaitJobsDrain(t, srv)
	summary := srv.jobs.Get().Summary
	wantIn(t, summary, "PTR lookup")
	if strings.Contains(summary, "Online lookup") {
		t.Errorf("summary = %q, want no online phase for a gallery its list leaves out", summary)
	}
}

func runLookupDueAgainst(t *testing.T, ptrStatus int) string {
	t.Helper()
	srv := newTestServer(t)
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ptr/status":
			_, _ = w.Write([]byte(`{"enabled":true,"state":"ready","progress":{"update_index":1}}`))
		case "/api/v1/ptr/lookup":
			w.WriteHeader(ptrStatus)
		case "/api/v1/lookup":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job_id":7}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv.ml.Cache.Store(monloader.Status{Conn: "ok", PTR: true})
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", LookupPTR: true, LookupBooru: true}
	srv.cfgMu.Unlock()
	seedPhaseImage(t, srv, 1, strings.Repeat("e", 64))

	wantIn(t, postForm(t, srv, "/settings/maintenance/lookup-due", url.Values{}).Body.String(), "Lookup started")
	awaitJobsDrain(t, srv)
	st := srv.jobs.Get()
	return st.Summary + st.Error
}

func TestLookupDueButton_OldMonloaderSaysSkipped(t *testing.T) {
	outcome := runLookupDueAgainst(t, http.StatusNotFound)
	wantIn(t, outcome, "PTR lookup skipped: monloader is too old for batch PTR lookup.")
	wantIn(t, outcome, "Online lookup:")
}

func TestLookupDueButton_PTRFailureStillRunsOnline(t *testing.T) {
	outcome := runLookupDueAgainst(t, http.StatusInternalServerError)
	wantIn(t, outcome, "PTR lookup failed")
	wantIn(t, outcome, "Online lookup:")
}

func runScheduledPassCancelledMidLookup(t *testing.T, shutdown bool) *Server {
	t.Helper()
	srv := newTestServer(t)
	stub := &ptrStubHandler{index: 4812}
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ptr/lookup" {
			if shutdown {
				close(srv.done)
			}
			srv.jobs.Cancel()
		}
		stub.handle(w, r)
	})
	srv.ml.Cache.Store(monloader.Status{Conn: "ok", PTR: true})
	seedPhaseImage(t, srv, 1, strings.Repeat("a", 64))
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", LookupPTR: true}
	srv.cfgMu.Unlock()

	srv.sched.RunNow()

	if info := srv.sched.Status().LastInfo; info != "cancelled" {
		t.Fatalf("LastInfo = %q, want cancelled", info)
	}
	return srv
}

func TestAScheduledPassCutShortByShutdownStaysOwed(t *testing.T) {
	srv := runScheduledPassCancelledMidLookup(t, true)
	if last := srv.sched.LastRun(); !last.IsZero() {
		t.Errorf("state file records %v, want the pass still owed", last)
	}
}

func TestAScheduledPassTheUserCancelledIsRecorded(t *testing.T) {
	srv := runScheduledPassCancelledMidLookup(t, false)
	if srv.sched.LastRun().IsZero() {
		t.Error("state file records no run, want the cancelled pass")
	}
}
