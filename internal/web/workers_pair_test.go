package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

	mustCode(t, postHTMXForm(t, srv, "/settings/workers/gpu-box/remove", url.Values{}), http.StatusOK)
	if rr := checkPairing(srv, out["token"], `{}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("check after a removal here: %d %s, want 401", rr.Code, rr.Body)
	}
}
