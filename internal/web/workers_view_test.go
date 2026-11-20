package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tagger/taggertest"
)

func fakeWorker(t *testing.T, provider, digest string) *httptest.Server {
	t.Helper()
	f := &worker.Fake{Token: "tok", Info: worker.Info{Version: "v1.0.0", Provider: provider, Slots: 1, ModelsDir: "/models"},
		Models: []worker.Model{{Name: "wd-swinv2", Kind: worker.KindTagger, Digest: digest, Ready: true}}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func usePool(t *testing.T, defs ...tagger.WorkerDef) {
	t.Helper()
	p := tagger.NewPool("")
	p.Set(defs)
	p.Refresh(context.Background())
	tagger.SetPool(p)
	t.Cleanup(func() { tagger.SetPool(tagger.NewPool("")) })
}

func workerRowHTML(t *testing.T, body, name string) string {
	t.Helper()
	_, row, ok := strings.Cut(body, `data-worker="`+name+`"`)
	if !ok {
		t.Fatalf("no row for %s in:\n%s", name, body)
	}
	row, _, _ = strings.Cut(row, "</tr>")
	return row
}

func TestTheWorkersBlockShowsEachWorkersState(t *testing.T) {
	srv := newTestServer(t)
	gone := fakeWorker(t, "cpu", "a")
	gone.Close()
	usePool(t,
		tagger.WorkerDef{Name: "gpu-box", URL: fakeWorker(t, "cuda", "a").URL, Token: "tok"},
		tagger.WorkerDef{Name: "desktop", URL: fakeWorker(t, "directml", "b").URL, Token: "tok"},
		tagger.WorkerDef{Name: "attic", URL: gone.URL, Token: "tok"},
		tagger.WorkerDef{Name: "spare", URL: gone.URL, Token: "tok", Paused: true},
	)
	_, body := getBody(t, srv, "/internal/workers")

	gpu := workerRowHTML(t, body, "gpu-box")
	for _, want := range []string{"footer-dot-ok", "v1.0.0", "CUDA", ">idle<", "wd-swinv2", "/pause", "/remove"} {
		wantIn(t, gpu, want)
	}
	wantNotIn(t, gpu, "differs")
	wantIn(t, workerRowHTML(t, body, "desktop"), `title="Not the files gpu-box holds under this name">differs`)
	attic := workerRowHTML(t, body, "attic")
	wantIn(t, attic, "footer-dot-down")
	wantIn(t, attic, ">offline since ")
	spare := workerRowHTML(t, body, "spare")
	wantIn(t, spare, ">paused<")
	wantIn(t, spare, "/settings/workers/spare/resume")
	wantIn(t, body, "To add a worker on another machine")
}

func TestTheWorkersBlockFollowsAWorkerThatStartsOrStopsTagging(t *testing.T) {
	srv := newTestServer(t)
	busy := fakeWorker(t, "cuda", "a")
	usePool(t, tagger.WorkerDef{Name: "gpu-box", URL: busy.URL, Token: "tok"})
	f := busy.Config.Handler.(*worker.Fake)
	for _, state := range []string{"tagging", "idle"} {
		f.Mu.Lock()
		f.Info.Busy = state == "tagging"
		f.Mu.Unlock()
		_, body := getBody(t, srv, "/internal/workers")
		wantIn(t, workerRowHTML(t, body, "gpu-box"), ">"+state+"<")
	}
}

func TestWithNoWorkerOnlyTheWorkersBlockShows(t *testing.T) {
	srv := newTestServer(t)
	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, "No worker. To add one, start a monworker instance")
	wantIn(t, body, `title="No worker to tag with">no worker`)
	wantNotIn(t, body, "<h4>Taggers list</h4>")
	wantNotIn(t, body, "<h4>Local worker</h4>")
}

func TestALocalWorkerTurnedOffOffersToEnable(t *testing.T) {
	path, _ := standIn(t)
	srv := newTestServer(t)
	srv.local = &localWorker{path: path, addr: "127.0.0.1:47002"}
	_, body := getBody(t, srv, "/internal/workers")
	row := workerRowHTML(t, body, "local")
	for _, want := range []string{"footer-dot-paused", "this machine", ">disabled<", "/settings/workers/local/enable"} {
		wantIn(t, row, want)
	}

	w := postForm(t, srv, "/settings/workers/local/enable", url.Values{})
	if w.Header().Get("HX-Refresh") != "true" || !srv.cfgSnapshot().Tagger.LocalWorker {
		t.Errorf("enable: refresh %q, local_worker %v", w.Header().Get("HX-Refresh"), srv.cfgSnapshot().Tagger.LocalWorker)
	}
	_ = postForm(t, srv, "/settings/workers/local/disable", url.Values{})
	if srv.cfgSnapshot().Tagger.LocalWorker {
		t.Error("disable left local_worker on")
	}

	srv, _ = newEnvPinnedServer(t, "MONBOORU_TAGGER_LOCAL_WORKER", "false", func(*config.Config) {})
	srv.local = &localWorker{path: path, addr: "127.0.0.1:47003"}
	_, body = getBody(t, srv, "/internal/workers")
	wantIn(t, workerRowHTML(t, body, "local"), "Set by MONBOORU_TAGGER_LOCAL_WORKER")
}

func TestAWorkersRequestWaitsInTheWorkersBlock(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")

	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, `href="/settings#tagger"`)
	wantIn(t, body, "gpu-box wants to pair")
	_, workers := getBody(t, srv, "/internal/workers")
	wantIn(t, workers, "A worker wants to pair.")
	wantIn(t, workers, "/settings/plugins/pair/"+id+"/approve")
	_, panel := getBody(t, srv, "/internal/plugins/pairing")
	wantNotIn(t, panel, id)

	w := postForm(t, srv, "/settings/plugins/pair/"+id+"/deny", url.Values{})
	wantIn(t, w.Body.String(), `id="tagger-workers"`)
	wantNotIn(t, w.Body.String(), "A worker wants to pair.")
}

func TestALocalNamedWorkerIsRefused(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monworker","label":"local","url":"http://localhost:8457","peer_token":"sek"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a worker named local: %d %s", rr.Code, rr.Body)
	}
}

func TestRemovingAWorkerTellsItAndForgetsIt(t *testing.T) {
	var told string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/remove" {
			told = r.Header.Get("Authorization")
		}
	}))
	t.Cleanup(remote.Close)
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Workers = []config.WorkerConfig{{Name: "gpu-box", URL: remote.URL, Token: "its-token"}}
	})

	mustCode(t, postForm(t, srv, "/settings/workers/gpu-box/pause", url.Values{}), http.StatusOK)
	if w := peer.FindWorker(srv.cfgSnapshot(), "gpu-box"); w == nil || !w.Paused {
		t.Fatalf("after pause: %+v", w)
	}
	mustCode(t, postForm(t, srv, "/settings/workers/gpu-box/resume", url.Values{}), http.StatusOK)
	if w := peer.FindWorker(srv.cfgSnapshot(), "gpu-box"); w == nil || w.Paused {
		t.Fatalf("after resume: %+v", w)
	}

	w := postForm(t, srv, "/settings/workers/gpu-box/remove", url.Values{})
	if told != "Bearer its-token" {
		t.Errorf("the worker was told with %q", told)
	}
	if len(srv.cfgSnapshot().Workers) != 0 {
		t.Errorf("workers after remove = %+v", srv.cfgSnapshot().Workers)
	}
	wantNotIn(t, w.Body.String(), "could not reach")
}

func TestTheInstallDialogNamesEachWorkersFolder(t *testing.T) {
	srv := newTestServer(t)
	usePool(t,
		tagger.WorkerDef{Name: "gpu-box", URL: fakeWorker(t, "cuda", "a").URL, Token: "tok"},
		tagger.WorkerDef{Name: "desktop", URL: fakeWorker(t, "cpu", "a").URL, Token: "tok"},
	)
	targets := srv.installTargets(srv.cfgSnapshot(), srv.workersView())
	if len(targets) != 2 || targets[0] != (installTarget{Name: "gpu-box", Where: "gpu-box", Dir: "/models"}) {
		t.Errorf("targets = %+v", targets)
	}
	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, `name="tagger-cmd-worker" data-where="desktop" data-dir="/models"`)
	wantIn(t, body, `Run the command below on <span id="tagger-cmd-where">gpu-box</span>`)
}

func TestOnlyAFolderOnThisMachineOffersDelete(t *testing.T) {
	srv, modelPath := newModelServer(t, config.TaggerInstance{Name: "wd-swinv2", ConfidenceThreshold: 0.35})
	taggertest.Use(t, map[string][]string{"wd-swinv2": {"1girl"}})
	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, "/settings/tagger/wd-swinv2/enable")
	wantNotIn(t, body, "/settings/tagger/wd-swinv2/delete")

	mustMkdirAll(t, filepath.Join(modelPath, "wd-swinv2"), 0o755)
	_, body = getBody(t, srv, "/settings")
	wantIn(t, body, "/settings/tagger/wd-swinv2/delete")
}

func TestTheStatsBlockOfTheTaggerPollsOnItsOwn(t *testing.T) {
	srv := newTestServer(t)
	_, page := getBody(t, srv, "/settings")
	_, block := getBody(t, srv, "/internal/tagger-stats")
	for _, body := range []string{page, block} {
		wantIn(t, body, `id="tagger-stats"`)
		wantIn(t, body, `hx-get="/internal/tagger-stats" hx-trigger="every 5s"`)
		wantIn(t, body, "<h4>Auto-tagger</h4>")
	}
	wantNotIn(t, block, "<h4>Database size per gallery</h4>")
}

func TestTheStatsBlockListsEveryWorker(t *testing.T) {
	srv := newTestServer(t)
	_, body := getBody(t, srv, "/internal/tagger-stats")
	wantIn(t, body, "<th>Status</th><td>no worker</td>")

	busy := fakeWorker(t, "cuda", "a")
	gone := fakeWorker(t, "cpu", "a")
	gone.Close()
	usePool(t,
		tagger.WorkerDef{Name: "gpu-box", URL: busy.URL, Token: "tok"},
		tagger.WorkerDef{Name: "laptop", URL: fakeWorker(t, "cpu", "a").URL, Token: "tok"},
		tagger.WorkerDef{Name: "attic", URL: gone.URL, Token: "tok"},
	)
	f := busy.Config.Handler.(*worker.Fake)
	f.Mu.Lock()
	f.Info.Loaded, f.Info.Busy = []string{"wd-swinv2"}, true
	f.Mu.Unlock()
	_, body = getBody(t, srv, "/internal/tagger-stats")
	compact := strings.Join(strings.Fields(body), "")
	for _, want := range []string{
		`<th>gpu-box</th><td>tagging</td></tr><tr><thclass="stats-d1">Mode</th>`,
		`<th>laptop</th><td>idle</td>`,
		`<thclass="stats-d1">Mode</th><td>CUDA</td></tr><tr><thclass="stats-d1">Models</th><td>wd-swinv2</td>`,
		`<thclass="stats-d1">Models</th><td>notloaded</td>`,
		`<th>attic</th><td>offlinesince`,
	} {
		wantIn(t, compact, want)
	}
	_, attic, _ := strings.Cut(compact, "<th>attic</th>")
	wantNotIn(t, attic, "Mode")
	wantNotIn(t, compact, "WorkerPID")
}
