package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tagger/taggertest"
)

func enableOnWorkers(t *testing.T, srv *Server, defs ...tagger.WorkerDef) []*taggertest.Worker {
	t.Helper()
	srv.cfgMu.Lock()
	srv.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "wd14", Enabled: true, ConfidenceThreshold: 0.1}}
	srv.cfgMu.Unlock()
	return taggertest.UsePool(t, map[string][]string{"wd14": {"landscape"}}, defs...)
}

func workerBox(t *testing.T, body, name string) string {
	t.Helper()
	_, box, ok := strings.Cut(body, `name="workers" value="`+name+`"`)
	if !ok {
		t.Fatalf("no box for %s in the dialog", name)
	}
	box, _, _ = strings.Cut(box, ">")
	return box
}

func TestTheAutotagDialogOffersWorkersOnceThereAreTwo(t *testing.T) {
	srv := newTestServer(t)
	enableOnWorkers(t, srv, tagger.WorkerDef{Name: tagger.LocalWorker, Local: true})
	_, body := getBody(t, srv, "/")
	wantNotIn(t, body, `id="batch-autotag-workers"`)

	enableOnWorkers(t, srv, tagger.WorkerDef{Name: tagger.LocalWorker, Local: true}, tagger.WorkerDef{Name: "gpu-box"})
	_, body = getBody(t, srv, "/")
	wantIn(t, body, `id="batch-autotag-workers"`)
	wantIn(t, body, `<span class="field-hint">2 at once, this machine</span>`)
	wantIn(t, body, "The ticked workers share the images.")
	wantIn(t, workerBox(t, body, tagger.LocalWorker), `data-holds="wd14" data-local`)
	wantNotIn(t, workerBox(t, body, "gpu-box"), "data-local")
}

func TestAJobOnTheTickedWorkersLeavesTheOthersAlone(t *testing.T) {
	srv := newTestServer(t)
	ws := enableOnWorkers(t, srv, tagger.WorkerDef{Name: tagger.LocalWorker, Local: true}, tagger.WorkerDef{Name: "gpu-box"}, tagger.WorkerDef{Name: "laptop"})
	var ids []string
	for i := range 4 {
		ids = append(ids, strconv.FormatInt(seedImage(t, srv, "img"+strconv.Itoa(i)+".png", 32+i, 32), 10))
	}
	w := postHTMXForm(t, srv, "/internal/autotag", url.Values{"scope": {"selection"}, "ids": ids, "workers": {tagger.LocalWorker, "laptop"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if ws[1].Calls() != 0 {
		t.Errorf("gpu-box, not ticked, took %d calls", ws[1].Calls())
	}
	if got := ws[0].Calls() + ws[2].Calls(); got != 4 {
		t.Errorf("local and laptop took %d calls, want one per image", got)
	}
	tagged := map[string]int{}
	for name, w := range map[string]*taggertest.Worker{tagger.LocalWorker: ws[0], "laptop": ws[2]} {
		if n := w.Calls(); n > 0 {
			tagged[name] = n
		}
	}
	if st, want := srv.jobs.Get(), "auto-tagged 4 images"+(tagger.Result{Tagged: tagged}).Shares(); st == nil || st.Summary != want {
		t.Errorf("job = %+v, want %q", st, want)
	}
}

func TestAJobItsTickedWorkersCannotRunIsRefused(t *testing.T) {
	srv := newTestServer(t)
	ws := enableOnWorkers(t, srv, tagger.WorkerDef{Name: "gpu-box"}, tagger.WorkerDef{Name: "laptop"})
	f := ws[1].Fake()
	f.Mu.Lock()
	f.Models = slices.DeleteFunc(f.Models, func(m worker.Model) bool { return m.Name == "wd14" })
	f.Mu.Unlock()
	ws[1].Refresh()
	id := strconv.FormatInt(seedImage(t, srv, "one.png", 32, 32), 10)

	for _, c := range []struct {
		workers []string
		want    string
	}{
		{[]string{"attic"}, "Worker attic cannot take a job now."},
		{[]string{"laptop"}, "No ticked worker holds wd14."},
	} {
		w := postHTMXForm(t, srv, "/internal/autotag", url.Values{"scope": {"selection"}, "ids": {id}, "workers": c.workers})
		mustCode(t, w, http.StatusBadRequest)
		wantIn(t, w.Body.String(), c.want)
	}
	if ws[0].Calls()+ws[1].Calls() != 0 {
		t.Error("a refused job made calls")
	}
}

func TestAddAWorkerByAddressOnceItAnswersWithItsToken(t *testing.T) {
	srv := newTestServer(t)
	enableOnWorkers(t, srv, tagger.WorkerDef{Name: "gpu-box"})
	addr := tagger.CurrentPool().Workers()[0].URL
	add := func(name, url_, token string) string {
		t.Helper()
		w := postForm(t, srv, "/settings/workers/add", url.Values{"name": {name}, "url": {url_}, "token": {token}, "gateway_token": {""}})
		mustCode(t, w, http.StatusOK)
		if w.Header().Get("HX-Refresh") == "true" {
			return ""
		}
		return w.Body.String()
	}
	for _, tc := range []struct{ name, url, token, want string }{
		{"two words", addr, "tok", "letters, digits"},
		{tagger.LocalWorker, addr, "tok", "local is the worker monbooru runs itself"},
		{"rented", "ftp://example.invalid", "tok", "must start with http:// or https://"},
		{"rented", addr, "", "A token is required"},
		{"rented", addr, "wrong", "refused the token"},
		{"rented", "http://127.0.0.1:1", "tok", "No worker answered at http://127.0.0.1:1"},
	} {
		wantIn(t, add(tc.name, tc.url, tc.token), tc.want)
	}
	if len(srv.cfg.Workers) != 0 {
		t.Fatalf("a refused form wrote %+v", srv.cfg.Workers)
	}
	if body := add("rented", addr+"/", "tok"); body != "" {
		t.Fatalf("a worker that answers was refused: %s", body)
	}
	if got := srv.cfg.Workers; len(got) != 1 || got[0].Name != "rented" || got[0].URL != addr || got[0].Token != "tok" || !got[0].Manual {
		t.Fatalf("workers = %+v", got)
	}
	var told bool
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { told = true }))
	t.Cleanup(gone.Close)
	srv.cfgMu.Lock()
	srv.cfg.Workers[0].URL = gone.URL
	srv.cfgMu.Unlock()
	wantIn(t, add("rented", addr, "tok"), "A worker named rented is already listed")
	mustCode(t, postForm(t, srv, "/settings/workers/rented/remove", url.Values{}), http.StatusOK)
	if told || len(srv.cfgSnapshot().Workers) != 0 {
		t.Errorf("removing a worker added by address: told %v, left %+v; it holds no pairing to end", told, srv.cfgSnapshot().Workers)
	}
}
