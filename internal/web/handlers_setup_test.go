package web

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func newSetupServer(t *testing.T) *Server {
	t.Helper()
	isolateDesktopHome(t)
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Server.BindAddress = "127.0.0.1:8455"
		c.SetupDone = false
	})
	srv.desktop = true
	isolateDesktopApp(t, srv)
	return srv
}

func setupForm(t *testing.T, extra string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pics")
	mustMkdirAll(t, dir, 0o755)
	return "gallery_path=" + url.QueryEscape(dir) + "&" + extra
}

func TestSetupGateRedirectsEveryPage(t *testing.T) {
	srv := newSetupServer(t)
	h := srv.Handler()
	for _, path := range []string{"/", "/tags", "/settings", "/collections"} {
		w := httptest.NewRecorder()
		r := localRequest("GET", path, "")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
			t.Errorf("%s: status = %d location = %q, want 303 -> /setup", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestSetupGateExemptsWhatTheWizardNeeds(t *testing.T) {
	srv := newSetupServer(t)
	h := srv.Handler()
	for _, path := range []string{"/health", "/setup", "/static/main.css"} {
		w := callLocal(h.ServeHTTP, "GET", path, "")
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestSetupGateLeavesTheLoginReachable(t *testing.T) {
	srv := newSetupServer(t)
	if err := srv.withConfig(func(c *config.Config) error {
		c.Auth.EnablePassword = true
		c.Auth.PasswordHash = "$2a$10$hash"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	for _, path := range []string{"/login", "/manifest.json"} {
		w := callLocal(h.ServeHTTP, "GET", path, "")
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestSetupGateAnswersTheAPIInItsOwnEnvelope(t *testing.T) {
	srv := newSetupServer(t)
	h := srv.Handler()
	w := callLocal(h.ServeHTTP, "POST", "/api/v1/pair/request", "")
	mustCode(t, w, http.StatusConflict)
	var body struct{ Error, Code string }
	mustUnmarshal(t, w.Body.Bytes(), &body)
	if body.Code != "setup_required" {
		t.Errorf("code = %q, want setup_required", body.Code)
	}
}

func TestSetupGateAnswersHTMXWithARedirectHeader(t *testing.T) {
	srv := newSetupServer(t)
	h := srv.Handler()
	w := httptest.NewRecorder()
	r := localRequest("GET", "/internal/sidebar", "")
	r.Header.Set("HX-Request", "true")
	h.ServeHTTP(w, r)
	if got := w.Header().Get("HX-Redirect"); got != "/setup" {
		t.Errorf("HX-Redirect = %q, want /setup", got)
	}
}

func TestSetupGateIsOffWithoutTheDesktopProfile(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) { c.SetupDone = false })
	h := srv.Handler()
	w := callLocal(h.ServeHTTP, "GET", "/", "")
	wantCode(t, w, http.StatusOK)
	w = callLocal(h.ServeHTTP, "GET", "/setup", "")
	wantCode(t, w, http.StatusNotFound)
}

func TestSetupRepointsTheGalleryAndClearsTheGate(t *testing.T) {
	srv := newSetupServer(t)
	dir := filepath.Join(t.TempDir(), "pics")
	mustMkdirAll(t, dir, 0o755)
	w := callLocal(srv.setupPost, "POST", "/setup", "gallery_path="+url.QueryEscape(dir)+"&reach=local")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("status = %d location = %q: %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if got := srv.active().GalleryPath; got != dir {
		t.Errorf("gallery path = %q, want %q", got, dir)
	}
	if srv.setupPending() {
		t.Error("the gate should be down once the wizard is through")
	}
}

func TestSetupRefusesAnUnreadableFolder(t *testing.T) {
	srv := newSetupServer(t)
	missing := filepath.Join(t.TempDir(), "not-here")
	w := callLocal(srv.setupPost, "POST", "/setup", "gallery_path="+url.QueryEscape(missing)+"&reach=lan")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "not readable")
	if !srv.setupPending() {
		t.Error("a failed submit must not clear the gate")
	}
	if got := srv.cfgSnapshot().Server.BindAddress; got != "127.0.0.1:8455" {
		t.Errorf("bind = %q, want the address left alone", got)
	}
}

func TestSetupRefusalKeepsWhatWasEntered(t *testing.T) {
	srv := newSetupServer(t)
	missing := filepath.Join(t.TempDir(), "not-here")
	w := callLocal(srv.setupPost, "POST", "/setup", "gallery_path="+url.QueryEscape(missing)+"&reach=lan&start_at_login=on")
	body := w.Body.String()
	wantIn(t, body, missing)
	wantIn(t, body, `value="lan" checked`)
}

func TestSetupOpensTheBindToTheNetwork(t *testing.T) {
	srv := newSetupServer(t)
	w := callLocal(srv.setupPost, "POST", "/setup", setupForm(t, "reach=lan"))
	mustCode(t, w, http.StatusSeeOther)
	if got := srv.cfgSnapshot().Server.BindAddress; got != "0.0.0.0:8455" {
		t.Errorf("bind = %q, want 0.0.0.0:8455", got)
	}
}

func TestSetupKeepsLoopbackForThisComputerOnly(t *testing.T) {
	srv := newSetupServer(t)
	w := callLocal(srv.setupPost, "POST", "/setup", setupForm(t, "reach=local"))
	mustCode(t, w, http.StatusSeeOther)
	if got := srv.cfgSnapshot().Server.BindAddress; got != "127.0.0.1:8455" {
		t.Errorf("bind = %q", got)
	}
}

func TestSetupLogsTheReachOfAnEnvironmentPinnedBind(t *testing.T) {
	isolateDesktopHome(t)
	srv, _ := newEnvPinnedServer(t, "MONBOORU_SERVER_BIND_ADDRESS", "0.0.0.0:8455", func(file *config.Config) {
		file.Server.BindAddress = "127.0.0.1:8455"
		file.SetupDone = false
	})
	srv.desktop = true
	isolateDesktopApp(t, srv)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	prev := logx.Get()
	logx.Set("info")
	t.Cleanup(func() { logx.Set(prev) })

	w := callLocal(srv.setupPost, "POST", "/setup", setupForm(t, "menu_entry=on"))
	mustCode(t, w, http.StatusSeeOther)
	wantIn(t, buf.String(), "setup: done, reachable from the network")
}

func TestSetupLeavesTheLoginAlone(t *testing.T) {
	for _, reach := range []string{"local", "lan"} {
		srv := newSetupServer(t)
		if err := srv.withConfig(func(c *config.Config) error {
			c.Auth.EnablePassword = true
			c.Auth.PasswordHash = "$2a$10$hash"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		callLocal(srv.setupPost, "POST", "/setup", setupForm(t, "reach="+reach))
		cfg := srv.cfgSnapshot()
		if !cfg.Auth.EnablePassword || cfg.Auth.PasswordHash != "$2a$10$hash" {
			t.Errorf("reach=%s dropped the password: %+v", reach, cfg.Auth)
		}
	}
}

func TestSetupDropsTheSectionThePlatformCannotFill(t *testing.T) {
	srv := newSetupServer(t)
	w := callLocal(srv.setupPage, "GET", "/setup", "")
	body := w.Body.String()
	mustIn(t, body, "Where are your images?")
	if got, want := strings.Contains(body, "start_at_login"), srv.desktopIntegration().Supported(); got != want {
		t.Errorf("start_at_login rendered = %v, want %v", got, want)
	}
}

func TestSetupIsGoneOnceDone(t *testing.T) {
	srv := newSetupServer(t)
	if err := srv.withConfig(func(c *config.Config) error { c.SetupDone = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w := callLocal(srv.Handler().ServeHTTP, "GET", "/setup", "")
	wantCode(t, w, http.StatusNotFound)
	w = callLocal(srv.setupPost, "POST", "/setup", setupForm(t, "reach=lan"))
	wantCode(t, w, http.StatusNotFound)
	if got := srv.cfgSnapshot().Server.BindAddress; got != "127.0.0.1:8455" {
		t.Errorf("bind = %q, want the address left alone", got)
	}
}

func TestSetupRefusesARemotePeer(t *testing.T) {
	srv := newSetupServer(t)
	r := localRequest("POST", "/setup", setupForm(t, "reach=lan"))
	r.RemoteAddr = "192.168.1.20:54321"
	w := httptest.NewRecorder()
	srv.localOnly(srv.setupPost)(w, r)
	wantCode(t, w, http.StatusNotFound)
	if !srv.setupPending() {
		t.Error("a remote submit must not clear the gate")
	}
	if got := srv.cfgSnapshot().Server.BindAddress; got != "127.0.0.1:8455" {
		t.Errorf("bind = %q, want the address left alone", got)
	}
}

func TestSetupFormNamesTheDefaultGallery(t *testing.T) {
	isolateDesktopHome(t)
	second := filepath.Join(t.TempDir(), "second")
	mustMkdirAll(t, second, 0o755)
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Server.BindAddress = "127.0.0.1:8455"
		c.SetupDone = false
		db, th := c.DerivePaths("second")
		c.Galleries = append(c.Galleries, config.Gallery{Name: "second", GalleryPath: second, DBPath: db, ThumbnailsPath: th})
		c.DefaultGallery = "second"
	})
	srv.desktop = true
	w := callLocal(srv.setupPage, "GET", "/setup", "")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `value="`+second+`"`)
}

func TestSetupPageShowsTheThemesHelloDrawingOrNone(t *testing.T) {
	srv := newSetupServer(t)
	for theme, want := range map[string]string{"dark": "/static/mascot-hello.png", "light": "/static/light/mascot-hello.png"} {
		srv.cfg.Server.Theme = theme
		body := do(srv, "GET", "/setup").Body.String()
		wantIn(t, body, `<div id="setup-page" class="setup-drawn">`)
		wantIn(t, body, `src="`+want+`"`)
		wantIn(t, body, `<h1>Mon<span class="wordmark-accent">booru</span></h1>`)
	}
	srv.cfg.Server.Theme = "legacy-dark"
	body := do(srv, "GET", "/setup").Body.String()
	if strings.Contains(body, `class="mascot"`) {
		t.Error("legacy-dark's setup page shows a drawing")
	}
	wantIn(t, body, `<div id="setup-page">`)
}

func TestSetupGateLetsTheWizardLoadAThemesDrawing(t *testing.T) {
	srv := newSetupServer(t)
	installThemes(t, srv, map[string]string{"aurora/theme.css": ":root{}", "aurora/mascot-hello.png": "hello-bytes"})
	srv.cfg.Server.Theme = "aurora"
	wantIn(t, do(srv, "GET", "/setup").Body.String(), `src="/theme.mascot/hello"`)
	if rr := do(srv, "GET", "/theme.mascot/hello"); rr.Code != http.StatusOK || rr.Body.String() != "hello-bytes" {
		t.Errorf("/theme.mascot/hello during setup = %d %q", rr.Code, rr.Body.String())
	}
}
