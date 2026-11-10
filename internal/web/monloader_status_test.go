package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/monloader"
)

func healthStub(t *testing.T, version string) *httptest.Server {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"version":"` + version + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(stub.Close)
	return stub
}

func TestMonloaderStatusLight(t *testing.T) {
	stub := healthStub(t, "v1.2.3")

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Server.MonloaderURL = stub.URL
	rr := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if body := rr.Body.String(); !strings.Contains(body, "connected to ") || !strings.Contains(body, ">monloader</a>") || !strings.Contains(body, "v1.2.3") {
		t.Errorf("light should show connected + linked monloader + version, got %q", body)
	}

	setTokens(srv, nil)
	rr2 := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr2, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	body := rr2.Body.String()
	wantIn(t, body, `hx-trigger="every 15s"`)
	if !regexp.MustCompile(`<span id="peer-light"[^>]*></span>`).MatchString(body) {
		t.Errorf("unpaired light should be empty, got %q", body)
	}
}

func TestMonloaderLightNamesAnAPIMismatch(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v9","api":9,"min_peer_api":9}`))
	}))
	t.Cleanup(stub.Close)
	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	rr := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	body := rr.Body.String()
	if !strings.Contains(body, ">api mismatch</span>") || !strings.Contains(body, "it speaks api 9 and drives 9 or newer") {
		t.Errorf("light should name the api mismatch, got %q", body)
	}
}

func TestMonloaderLightNamesAnAPIMismatchWhenTheTokenIsRejected(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"version":"v9","api":9,"min_peer_api":9}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(stub.Close)
	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "revoked"
	rr := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "auth rejected") || !strings.Contains(body, ">api mismatch</span>") || !strings.Contains(body, "it speaks api 9 and drives 9 or newer") {
		t.Errorf("rejected light should name the api mismatch, got %q", body)
	}
}

func TestMonloaderLightSeedsCachedStatus(t *testing.T) {
	stub := healthStub(t, "v1.2.3")

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL

	page := func() string {
		w := do(srv, "GET", "/")
		return w.Body.String()
	}

	mustIn(t, page(), "footer-dot-checking")

	srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if body := page(); !strings.Contains(body, "footer-dot-ok") {
		t.Errorf("warm cache should seed the connected light, got %q", body)
	}
}

func TestMonloaderFooterLink(t *testing.T) {
	srv := newTestServer(t)
	pairPeer(srv, "monloader")

	page := func() string {
		w := do(srv, "GET", "/")
		return w.Body.String()
	}
	poll := func() string {
		w := httptest.NewRecorder()
		srv.monloaderStatusHandler(w, httptest.NewRequest("GET", "/internal/monloader-status", nil))
		return w.Body.String()
	}

	srv.cfg.Server.MonloaderURL = "http://localhost:8081/"
	link := `<a href="http://localhost:8081/queue" target="_blank" rel="noopener">monloader</a>`
	wantIn(t, page(), link)
	wantIn(t, poll(), link)

	srv.cfg.Server.MonloaderURL = ""
	srv.cfg.Monloader.APIURL = "http://monloader:8081"
	if out := poll(); strings.Contains(out, "<a ") {
		t.Errorf("footer word should be plain when only the api url is set, got %q", out)
	}

	srv.cfg.Monloader.APIURL = ""
	if out := poll(); strings.Contains(out, "<a ") {
		t.Errorf("footer word should be plain when no monloader url is set, got %q", out)
	}
}

func TestMonloaderProbeReadsPTRCapability(t *testing.T) {
	status := `{"enabled":false,"state":"disabled"}`
	stub := ptrStatusStub(t, &status)

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	poll := func() {
		srv.ml.Cache.Expire()
		srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
	}

	poll()
	if srv.ml.Cache.Seed().PTR {
		t.Error("disabled PTR must seed false")
	}

	status = `{"enabled":true,"state":"syncing"}`
	poll()
	if ml := srv.ml.Cache.Seed(); ml.PTR || !ml.PTRSyncing {
		t.Errorf("a syncing PTR must seed unusable but syncing, got ptr=%v syncing=%v", ml.PTR, ml.PTRSyncing)
	}

	status = `{"enabled":true,"state":"paused","queryable":false}`
	poll()
	if ml := srv.ml.Cache.Seed(); ml.PTR || !ml.PTRSyncing {
		t.Errorf("a PTR paused mid-build must seed unusable but syncing, got ptr=%v syncing=%v", ml.PTR, ml.PTRSyncing)
	}

	status = `{"enabled":true,"state":"paused","queryable":true}`
	poll()
	if ml := srv.ml.Cache.Seed(); !ml.PTR || ml.PTRSyncing {
		t.Errorf("a paused complete PTR must seed usable, got ptr=%v syncing=%v", ml.PTR, ml.PTRSyncing)
	}

	status = `{"enabled":true,"state":"ready","contrib":{"account":true,"banned":false,"failed":2}}`
	poll()
	if !srv.ml.Cache.Seed().PTR {
		t.Error("a caught-up PTR must seed true after the poll re-probes")
	}
	if got := srv.ml.Cache.Seed().ContribFailed; got != 2 {
		t.Errorf("contrib failed seed = %d, want 2", got)
	}
}

func TestBackgroundProbeRefreshesMonloader(t *testing.T) {
	stub := healthStub(t, "v1")

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL

	srv.ml.Cache.Store(monloader.Status{Conn: "down"})
	srv.ml.Cache.Expire()
	srv.refreshPluginProbes(t.Context())
	if got := srv.ml.Cache.Seed().Conn; got != "ok" {
		t.Errorf("background probe left the companion at %q, want ok", got)
	}
	if !srv.ml.Usable() {
		t.Error("a companion answering its probe must read usable")
	}

	srv.cfg.Monloader.Paused = true
	srv.ml.Cache.Store(monloader.Status{Conn: "down"})
	srv.ml.Cache.Expire()
	srv.refreshPluginProbes(t.Context())
	if got := srv.ml.Cache.Seed().Conn; got != "down" {
		t.Errorf("a paused link was probed anyway: %q", got)
	}
}

func TestMonloaderLightTriggersOnAChangeTheBackgroundProbeSaw(t *testing.T) {
	status := `{"enabled":true,"state":"syncing"}`
	stub := ptrStatusStub(t, &status)

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	seenRe := regexp.MustCompile(`"seen":"([^"]*)"`)
	poll := func(seen string) (trigger, nextSeen string) {
		w := httptest.NewRecorder()
		srv.monloaderStatusHandler(w, httptest.NewRequest("GET", "/internal/monloader-status?seen="+url.QueryEscape(seen), nil))
		if m := seenRe.FindStringSubmatch(html.UnescapeString(w.Body.String())); m != nil {
			nextSeen = m[1]
		}
		return w.Header().Get("HX-Trigger"), nextSeen
	}

	_, seen := poll("")
	if trigger, _ := poll(seen); trigger != "" {
		t.Fatalf("an unchanged status triggered %q", trigger)
	}

	status = `{"enabled":true,"state":"ready"}`
	srv.ml.Cache.Expire()
	srv.refreshPluginProbes(t.Context())
	if !srv.ml.Cache.Seed().PTR {
		t.Fatal("the background probe did not store the ready PTR")
	}
	trigger, seen := poll(seen)
	if trigger != "monloader-status-changed" {
		t.Errorf("a change the background probe saw first: trigger %q, want monloader-status-changed", trigger)
	}
	if trigger, _ := poll(seen); trigger != "" {
		t.Errorf("the page was already told: trigger %q", trigger)
	}
}

func TestMonloaderProbeReadsContribGate(t *testing.T) {
	statusBody := `{"enabled":true,"state":"ready"}`
	stub := ptrStatusStub(t, &statusBody)

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	reprobe := func() bool {
		srv.ml.Cache.Expire()
		srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
		contrib := srv.ml.Cache.Seed().Contrib
		return contrib
	}

	if reprobe() {
		t.Error("absent contrib field must gate off")
	}
	statusBody = `{"enabled":true,"state":"ready","contrib":{"account":true,"banned":false}}`
	if !reprobe() {
		t.Error("usable account must gate on")
	}
	statusBody = `{"enabled":true,"state":"ready","contrib":{"account":true,"banned":true}}`
	if reprobe() {
		t.Error("banned account must gate off")
	}
	statusBody = `{"enabled":true,"state":"ready","contrib":{"account":false,"banned":false}}`
	if reprobe() {
		t.Error("no account must gate off")
	}

	statusBody = `{"enabled":true,"state":"syncing","contrib":{"account":true,"banned":false}}`
	if reprobe() {
		t.Error("syncing index must gate contribution off")
	}
}

func ptrStatusStub(t *testing.T, status *string) *httptest.Server {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"version":"v1"}`))
		case "/api/v1/ptr/status":
			_, _ = w.Write([]byte(*status))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}
