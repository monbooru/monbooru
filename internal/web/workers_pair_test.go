package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/pairing"
	"github.com/monbooru/monbooru/internal/shared/worker"
)

func workerOffer(t *testing.T, srv *Server, p *httptest.Server, name string) string {
	t.Helper()
	u, _ := url.Parse(p.URL)
	id, st := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monworker","label":"`+name+`","url":"http://localhost:`+u.Port()+`","requested_scopes":["read","write"],"peer_token":"tok-`+name+`","version":"v1.0.0"}`)
	if st != pairing.Pending {
		t.Fatalf("%s: state = %q, want pending", name, st)
	}
	return id
}

func TestTwoWorkersPairUnderTheirNames(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	ids := map[string]string{}
	for _, name := range []string{"gpu-box", "desktop"} {
		ids[name] = workerOffer(t, srv, p, name)
	}
	if n := len(srv.pairs.ListPending()); n != 2 {
		t.Fatalf("pending = %d, want both workers waiting", n)
	}
	for name, id := range ids {
		mustCode(t, pairApprove(srv, id), http.StatusOK)
		if _, out := pairStatus(t, srv, id); len(out["token"]) != 32 {
			t.Fatalf("%s: claim = %v", name, out)
		}
	}
	cfg := srv.cfgSnapshot()
	if len(cfg.Workers) != 2 {
		t.Fatalf("workers = %+v, want one block each", cfg.Workers)
	}
	for _, w := range cfg.Workers {
		if w.Token != "tok-"+w.Name || !strings.HasPrefix(w.URL, "http://127.0.0.1:") {
			t.Errorf("worker = %+v, want its token and the address it was seen at", w)
		}
		tok := cfg.Auth.Tokens.Paired(peer.WorkerKey(w.Name))
		if tok == nil || len(tok.Scopes) != 0 || tok.Name != w.Name+" (worker)" {
			t.Errorf("%s's token = %+v, want one named for it with no scope", w.Name, tok)
		}
	}
	if names := workerNames(srv); len(names) != 2 {
		t.Errorf("pool = %v, want both workers", names)
	}
}

func TestAWorkersTokenOpensNoRouteButItsOwnRemoval(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	_, out := pairStatus(t, srv, id)
	tok := out["token"]

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("the worker's token read the API: %d", rr.Code)
	}

	req = httptest.NewRequest("POST", "/api/v1/pair/remove", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	mustCode(t, rr, http.StatusOK)
	if cfg := srv.cfgSnapshot(); len(cfg.Workers) != 0 || cfg.Auth.Tokens.Paired(peer.WorkerKey("gpu-box")) != nil {
		t.Errorf("after its removal: workers %+v", cfg.Workers)
	}
}

func TestASecondWorkerUnderAPairedNameWaitsOnlyWhileTheFirstIsGone(t *testing.T) {
	f := &worker.Fake{Token: "tok-gpu-box", Info: worker.Info{Slots: 1}}
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			f.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(first.Close)
	srv := newTestServer(t)
	id := workerOffer(t, srv, first, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	pairStatus(t, srv, id)

	second := `{"app":"monworker","label":"gpu-box","url":"http://localhost:1","requested_scopes":[],"peer_token":"tok-2","version":"v1.0.0"}`
	req := httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(second))
	req.RemoteAddr = "127.0.0.1:50001"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "already paired") {
		t.Fatalf("second gpu-box while the first answers: %d %s", rr.Code, rr.Body)
	}

	first.Close()
	if _, st := offerFrom(t, srv, "127.0.0.1:50001", second); st != pairing.Pending {
		t.Fatalf("second gpu-box once the first is gone: %q, want a pending re-pair", st)
	}
	if reqs := srv.pairs.ListPending(); len(reqs) != 1 || !reqs[0].Repair {
		t.Errorf("pending = %+v, want one re-pair", reqs)
	}
}

func checkPairing(srv *Server, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/pair/check", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50002"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAWorkerLearnsAtStartWhetherItsPairingStillHolds(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	_, out := pairStatus(t, srv, id)
	mustCode(t, checkPairing(srv, out["token"], `{}`), http.StatusOK)
	mustCode(t, checkPairing(srv, "", `{}`), http.StatusUnauthorized)
	mustCode(t, checkPairing(srv, "not-a-token", `{}`), http.StatusUnauthorized)

	rm := postHTMXForm(t, srv, "/settings/workers/gpu-box/remove", url.Values{})
	mustCode(t, rm, http.StatusOK)
	wantIn(t, rm.Body.String(), "could not reach gpu-box: it offers itself again at its next start.")
	if rr := checkPairing(srv, out["token"], `{}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("check after a removal here: %d %s, want 401", rr.Code, rr.Body)
	}
}

func TestAWorkerThatMovedIsCalledWhereItSaysAtItsCheck(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	_, out := pairStatus(t, srv, id)
	mustCode(t, checkPairing(srv, out["token"], `{"app":"monworker","label":"gpu-box","url":"http://localhost:61234"}`), http.StatusOK)
	cfg := srv.cfgSnapshot()
	if len(cfg.Workers) != 1 || cfg.Workers[0].URL != "http://127.0.0.1:61234" {
		t.Errorf("workers = %+v, want gpu-box at its new port", cfg.Workers)
	}
	if tok := cfg.Auth.Tokens.Paired(peer.WorkerKey("gpu-box")); tok == nil || tok.PeerURL != "http://127.0.0.1:61234" {
		t.Errorf("token = %+v, want it pointing at the new port", tok)
	}
}

func TestAnOfferUnderTheNameOfAWorkerAddedByAddressIsRefused(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	rented := config.WorkerConfig{Name: "rented", URL: "https://pod.example", Token: "operator-secret", GatewayToken: "gw-key", Manual: true, OnDemand: true}
	if err := srv.withConfig(func(c *config.Config) error {
		c.Workers = append(c.Workers, rented)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(p.URL)
	req := httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(`{"app":"monworker","label":"rented","url":"http://localhost:`+u.Port()+`","requested_scopes":[],"peer_token":"tok-rented","version":"v1.0.0"}`))
	req.RemoteAddr = "127.0.0.1:50000"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	mustCode(t, rr, http.StatusConflict)
	wantIn(t, rr.Body.String(), `"code":"name_taken"`)
	wantIn(t, rr.Body.String(), "a worker named rented is already listed, added by address; set MONWORKER_NAME to another name")
	if n := len(srv.pairs.ListPending()); n != 0 {
		t.Errorf("pending = %d, want the offer refused", n)
	}
	if ws := srv.cfgSnapshot().Workers; len(ws) != 1 || ws[0] != rented {
		t.Errorf("workers = %+v, want the one added by address kept", ws)
	}
}

func TestARePairKeepsOnDemandAndTheGatewayToken(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	pairStatus(t, srv, id)
	if err := srv.withConfig(func(c *config.Config) error {
		w := peer.FindWorker(c, "gpu-box")
		w.OnDemand, w.GatewayToken, w.Paused = true, "gw-key", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	id = workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	if _, out := pairStatus(t, srv, id); len(out["token"]) != 32 {
		t.Fatalf("re-pair claim = %v", out)
	}
	ws := srv.cfgSnapshot().Workers
	if len(ws) != 1 || !ws[0].OnDemand || ws[0].GatewayToken != "gw-key" || ws[0].Paused {
		t.Errorf("workers = %+v, want gpu-box on demand behind its gateway, no longer paused", ws)
	}
}

func TestRemovingAWorkerTellsItOnlyOnceTheSaveWorked(t *testing.T) {
	var told atomic.Bool
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/api/v1/pair/remove":
			told.Store(true)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.Close)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	pairStatus(t, srv, id)

	configPath := srv.configPath
	notADir := filepath.Join(t.TempDir(), "file")
	mustWriteFile(t, notADir, nil, 0o644)
	srv.configPath = filepath.Join(notADir, "monbooru.toml")
	rm := postHTMXForm(t, srv, "/settings/workers/gpu-box/remove", url.Values{})
	wantIn(t, rm.Body.String(), "Could not save: ")
	if told.Load() {
		t.Error("the worker was told to unpair although the save failed")
	}
	if len(srv.cfgSnapshot().Workers) != 1 {
		t.Errorf("workers = %+v, want gpu-box kept", srv.cfgSnapshot().Workers)
	}

	srv.configPath = configPath
	postHTMXForm(t, srv, "/settings/workers/gpu-box/remove", url.Values{})
	if !told.Load() || len(srv.cfgSnapshot().Workers) != 0 {
		t.Errorf("once saved: told = %v, workers = %+v", told.Load(), srv.cfgSnapshot().Workers)
	}
}

func TestRemovingANameNotListedTouchesNoFile(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	data := srv.cfgSnapshot().Paths.DataPath
	victim := filepath.Join(data, "victim.json")
	mustWriteFile(t, victim, []byte("{}"), 0o644)
	mustCode(t, postHTMXForm(t, srv, "/settings/workers/..%2fvictim/remove", url.Values{}), http.StatusOK)
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim.json: %v, want it kept", err)
	}

	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	pairStatus(t, srv, id)
	saved := filepath.Join(data, "workers", "gpu-box.json")
	mustMkdirAll(t, filepath.Dir(saved), 0o755)
	mustWriteFile(t, saved, []byte("{}"), 0o644)
	mustCode(t, postHTMXForm(t, srv, "/settings/workers/gpu-box/remove", url.Values{}), http.StatusOK)
	cfg := srv.cfgSnapshot()
	if len(cfg.Workers) != 0 || cfg.Auth.Tokens.Paired(peer.WorkerKey("gpu-box")) != nil {
		t.Errorf("workers = %+v, want gpu-box and its token gone", cfg.Workers)
	}
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Errorf("workers/gpu-box.json: %v, want it gone", err)
	}
}

func TestACheckWithAnAddressWithNoHostKeepsTheWorkersAddress(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	id := workerOffer(t, srv, p, "gpu-box")
	mustCode(t, pairApprove(srv, id), http.StatusOK)
	_, out := pairStatus(t, srv, id)
	was := srv.cfgSnapshot().Workers[0].URL
	mustCode(t, checkPairing(srv, out["token"], `{"url":"8457"}`), http.StatusOK)
	if got := srv.cfgSnapshot().Workers[0].URL; got != was {
		t.Errorf("url = %q, want %q kept", got, was)
	}
}
