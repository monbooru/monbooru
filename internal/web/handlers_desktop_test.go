package web

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/desktop"
)

func isolateDesktopHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("FLATPAK_ID", "")
}

func isolateDesktopApp(t *testing.T, srv *Server) {
	t.Helper()
	srv.desktopApp = "monbooru-test"
	t.Cleanup(func() { _ = srv.desktopHook().SetAutostart(false) })
}

func newDesktopServer(t *testing.T) *Server {
	t.Helper()
	isolateDesktopHome(t)
	root := t.TempDir()
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Server.BindAddress = "127.0.0.1:8455"

		c.Paths.ModelPath = filepath.Join(root, "models")
	})
	srv.desktop = true
	isolateDesktopApp(t, srv)
	srv.logDir = filepath.Join(root, "logs")
	srv.folderOpener = func(string) error { return nil }
	return srv
}

func localRequest(method, target string, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = "127.0.0.1:8455"
	return r
}

func callLocal(h func(http.ResponseWriter, *http.Request), method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, localRequest(method, target, body))
	return w
}

type desktopProbe struct {
	path    string
	handler func(http.ResponseWriter, *http.Request)
	method  string
	body    string
}

func desktopProbes(srv *Server) []desktopProbe {
	return []desktopProbe{
		{"/internal/browse?into=x", srv.localOnly(srv.browseDirs), "GET", ""},
		{"/internal/open-folder", srv.localOnly(srv.openFolder), "POST", "kind=config"},
		{"/settings/quit", srv.localOnly(srv.settingsQuit), "POST", ""},
		{"/settings/restart", srv.localOnly(srv.settingsRestart), "POST", ""},
		{"/internal/desktop-power", srv.localOnly(srv.desktopPowerFragment), "GET", ""},
	}
}

func awaitQuit(t *testing.T, srv *Server) {
	t.Helper()
	select {
	case <-srv.QuitRequested():
	case <-time.After(5 * time.Second):
		t.Fatal("QuitRequested never fired")
	}
}

func TestBrowseListsSubdirectoriesOnly(t *testing.T) {
	srv := newDesktopServer(t)
	root := t.TempDir()
	for _, d := range []string{"alpha", "beta", ".hidden"} {
		mustMkdirAll(t, filepath.Join(root, d), 0o755)
	}
	mustWriteFile(t, filepath.Join(root, "a-file.txt"), []byte("x"), 0o644)

	w := callLocal(srv.browseDirs, "GET", "/internal/browse?into=gallery-add-path&path="+url.QueryEscape(root), "")
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{"alpha/", "beta/"} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, "a-file.txt")
	wantNotIn(t, body, ".hidden")
}

func TestBrowseTruncatesTheAlphabeticalHead(t *testing.T) {
	srv := newDesktopServer(t)
	root := t.TempDir()
	for i := range maxBrowseEntries {
		mustMkdirAll(t, filepath.Join(root, fmt.Sprintf("a%04d", i)), 0o755)
	}
	mustMkdirAll(t, filepath.Join(root, "Zebra"), 0o755)

	w := callLocal(srv.browseDirs, "GET", "/internal/browse?into=gallery-add-path&path="+url.QueryEscape(root), "")
	body := w.Body.String()
	wantIn(t, body, "a0000/")
	wantNotIn(t, body, "Zebra/")
	wantIn(t, body, "Only the first")
}

func TestBrowseListsASymlinkedFolder(t *testing.T) {
	srv := newDesktopServer(t)
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(target, "nothing"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "plain.txt"), []byte("x"), 0o644)

	w := callLocal(srv.browseDirs, "GET", "/internal/browse?into=gallery-add-path&path="+url.QueryEscape(root), "")
	body := w.Body.String()
	if !strings.Contains(body, "linked/") {
		t.Error("a symlinked folder is missing from the listing")
	}
	if strings.Contains(body, "dangling/") {
		t.Error("a symlink pointing at nothing was listed as a folder")
	}
	if strings.Contains(body, "plain.txt") {
		t.Error("a file was listed")
	}
}

func TestBrowseSurvivesAnUnreadableDirectory(t *testing.T) {
	srv := newDesktopServer(t)
	missing := filepath.Join(t.TempDir(), "not-here")
	w := callLocal(srv.browseDirs, "GET", "/internal/browse?into=x&path="+url.QueryEscape(missing), "")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Cannot read this folder.")
}

func TestBrowseRejectsABadTargetField(t *testing.T) {
	srv := newDesktopServer(t)
	w := callLocal(srv.browseDirs, "GET", "/internal/browse?into="+url.QueryEscape(`" onload="x`), "")
	wantCode(t, w, http.StatusBadRequest)
}

func TestDesktopControlsAreGated(t *testing.T) {
	cases := []struct {
		name    string
		profile bool
		bind    string
		remote  string
	}{
		{"no desktop profile", false, "127.0.0.1:8455", "127.0.0.1:5000"},
		{"remote is not loopback", true, "127.0.0.1:8455", "192.168.1.20:5000"},
		{"remote is not loopback on a network bind", true, "0.0.0.0:8455", "192.168.1.20:5000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWith(t, false, func(c *config.Config) { c.Server.BindAddress = tc.bind })
			srv.desktop = tc.profile

			for _, probe := range desktopProbes(srv) {
				w := httptest.NewRecorder()
				r := localRequest(probe.method, probe.path, probe.body)
				r.RemoteAddr = tc.remote
				probe.handler(w, r)
				if w.Code != http.StatusNotFound {
					t.Errorf("%s %s: status = %d, want 404", probe.method, probe.path, w.Code)
				}
			}
		})
	}
}

func TestDesktopControlsRefuseAForwardedHop(t *testing.T) {
	srv := newDesktopServer(t)
	for _, header := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "Via"} {
		r := localRequest("GET", "/internal/browse?into=x", "")
		r.Header.Set(header, "192.168.1.20")
		if srv.desktopLocal(r) {
			t.Errorf("%s: a proxied request passed as the local machine", header)
		}
		w := httptest.NewRecorder()
		srv.localOnly(srv.browseDirs)(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: browse status = %d, want 404", header, w.Code)
		}
	}
}

func TestDesktopControlsRefuseARenamedHost(t *testing.T) {
	srv := newDesktopServer(t)
	if err := srv.withConfig(func(c *config.Config) error { c.SetupDone = false; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, probe := range append(desktopProbes(srv),
		desktopProbe{"/settings/desktop", srv.localOnly(srv.settingsDesktopPost), "POST", "tray=on"},
		desktopProbe{"/setup", srv.localOnly(srv.setupPost), "POST", setupForm(t, "reach=lan")},
	) {
		w := httptest.NewRecorder()
		r := localRequest(probe.method, probe.path, probe.body)
		r.Host = "evil.example:8455"
		probe.handler(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", probe.method, probe.path, w.Code)
		}
	}
	if got := srv.cfgSnapshot().Server.BindAddress; got != "127.0.0.1:8455" {
		t.Errorf("bind = %q, want the address left alone", got)
	}
}

func TestEveryLocalControlIsRoutedThroughTheGate(t *testing.T) {
	srv := newDesktopServer(t)
	if err := srv.withConfig(func(c *config.Config) error { c.SetupDone = true; return nil }); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	forwarded := func(probe desktopProbe) {
		t.Helper()
		w := httptest.NewRecorder()
		r := localRequest(probe.method, probe.path, probe.body)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-For", "192.168.1.20")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", probe.method, probe.path, w.Code)
		}
	}
	for _, probe := range append(desktopProbes(srv), desktopProbe{"/settings/desktop", nil, "POST", "tray=on"}) {
		forwarded(probe)
	}
	if err := srv.withConfig(func(c *config.Config) error { c.SetupDone = false; return nil }); err != nil {
		t.Fatal(err)
	}
	forwarded(desktopProbe{"/setup", nil, "POST", setupForm(t, "reach=lan")})
	if !srv.setupPending() {
		t.Error("a forwarded submit cleared the setup gate")
	}
}

func TestDesktopControlsSurviveANetworkBind(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Server.BindAddress = "0.0.0.0:8455" })
	srv.desktop = true

	w := callLocal(srv.browseDirs, "GET", "/internal/browse?into=x", "")
	wantCode(t, w, http.StatusOK)
	if !srv.desktopLocal(localRequest("POST", "/settings/restart", "")) {
		t.Error("the restart control is gone on the address the wizard offers")
	}
}

func TestAvailableFoldersDropsWhatIsNotConfigured(t *testing.T) {
	srv := newDesktopServer(t)
	if !slices.Contains(srv.availableFolders(), "models") {
		t.Fatalf("models missing from %v with a model path set", srv.availableFolders())
	}
	srv.cfgMu.Lock()
	srv.cfg.Paths.ModelPath = ""
	srv.cfgMu.Unlock()
	if slices.Contains(srv.availableFolders(), "models") {
		t.Errorf("models still offered with no model path: %v", srv.availableFolders())
	}
}

func TestOpenFolderRefusesAnUnknownKind(t *testing.T) {
	srv := newDesktopServer(t)
	w := callLocal(srv.openFolder, "POST", "/internal/open-folder", "kind=/etc")
	wantCode(t, w, http.StatusBadRequest)
}

func TestDesktopFolderResolvesEveryKind(t *testing.T) {
	srv := newDesktopServer(t)
	for _, kind := range openFolderKinds {
		dir, ours := srv.desktopFolder(kind)
		if dir == "" {
			t.Errorf("kind %q resolved to an empty path", kind)
		}
		if kind == "gallery" && ours {
			t.Error("the gallery is the operator's; monbooru must not create it")
		}
	}
	if _, ok := srv.desktopFolder("passwd"); ok {
		t.Error("an unknown kind must not resolve")
	}
}

func TestQuitSignalsTheCommand(t *testing.T) {
	srv := newDesktopServer(t)
	w := callLocal(srv.settingsQuit, "POST", "/settings/quit", "")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "has stopped")
	awaitQuit(t, srv)
}

func TestOpenFolderCreatesATaggerSubfolder(t *testing.T) {
	srv := newDesktopServer(t)
	var opened string
	srv.folderOpener = func(path string) error { opened = path; return nil }
	w := callLocal(srv.openFolder, "POST", "/internal/open-folder", "kind=models&name=wd-swinv2")
	mustCode(t, w, http.StatusOK)
	dir := filepath.Join(srv.cfgSnapshot().Paths.ModelPath, "wd-swinv2")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("subfolder not created: %v", err)
	}
	if opened != dir {
		t.Errorf("opened %q, want %q", opened, dir)
	}
}

func TestOpenFolderReportsAnOpenerThatFails(t *testing.T) {
	srv := newDesktopServer(t)
	srv.folderOpener = func(string) error { return errors.New("no opener") }
	w := callLocal(srv.openFolder, "POST", "/internal/open-folder", "kind=config")
	mustCode(t, w, http.StatusInternalServerError)
	wantIn(t, w.Body.String(), "Could not open")

	r := localRequest("POST", "/internal/open-folder", "kind=config")
	r.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	srv.openFolder(w, r)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Could not open")
}

func TestOpenFolderRefusesATaggerNameThatIsAPath(t *testing.T) {
	for _, name := range []string{"../etc", "a/b", ".."} {
		srv := newDesktopServer(t)
		w := callLocal(srv.openFolder, "POST", "/internal/open-folder", "kind=models&name="+url.QueryEscape(name))
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want 400", name, w.Code)
		}
	}
}

func TestDesktopTestServersLeaveTheRealEntriesAlone(t *testing.T) {
	for _, srv := range []*Server{newDesktopServer(t), newSetupServer(t)} {
		if got := srv.desktopHook().App; got == "monbooru" {
			t.Errorf("a test server drives the real %q entries", got)
		}
	}
}

func TestTraySwitchFollowsTheBuild(t *testing.T) {
	srv := newDesktopServer(t)
	w := callLocal(srv.settingsHandler, "GET", "/settings", "")
	shown := strings.Contains(w.Body.String(), `name="tray"`)
	if want := desktop.TrayAvailable(); shown != want {
		t.Errorf("tray switch rendered = %v, want %v", shown, want)
	}
	if !srv.TrayEnabled() {
		t.Error("the tray should start on")
	}

	w = callLocal(srv.settingsDesktopPost, "POST", "/settings/desktop", "")
	mustCode(t, w, http.StatusOK)
	if got, want := srv.TrayEnabled(), !desktop.TrayAvailable(); got != want {
		t.Errorf("tray after an unticked save = %v, want %v", got, want)
	}
}

func TestADesktopSaveThatFailsAnswersAFlash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	srv := newDesktopServer(t)
	data := os.Getenv("XDG_DATA_HOME")
	mustMkdirAll(t, data, 0o755)
	if err := os.Chmod(data, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(data, 0o755) })

	w := callLocal(srv.settingsDesktopPost, "POST", "/settings/desktop", "menu_entry=on")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `<div class="flash flash-err">menu entry: `)
}

func TestRestartAsksForAFreshProcess(t *testing.T) {
	srv := newDesktopServer(t)
	if srv.RestartRequested() {
		t.Fatal("nothing has asked for a restart yet")
	}
	w := callLocal(srv.settingsRestart, "POST", "/settings/restart", "")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "is restarting")
	awaitQuit(t, srv)
	if !srv.RestartRequested() {
		t.Error("the command would stop instead of starting again")
	}
}

func TestJobWarningNamesTheRunningJob(t *testing.T) {
	srv := newDesktopServer(t)
	if got := srv.desktopJobWarning(); got != "" {
		t.Errorf("warning = %q, want none on an idle server", got)
	}
	mustStartJob(t, srv.jobs, models.JobTypeRebuildThumbs)
	if got, want := srv.desktopJobWarning(), "A thumbnail rebuild is running and will not resume."; got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
	srv.jobs.Complete("")

	mustStartJob(t, srv.jobs, "some-new-job")
	if got, want := srv.desktopJobWarning(), "A job is running and will not resume."; got != want {
		t.Errorf("unmapped warning = %q, want %q", got, want)
	}
	srv.jobs.Complete("")
}

func TestClose_WaitsForTheRunningJob(t *testing.T) {
	srv := newTestServer(t)
	mustStartJob(t, srv.jobs, "test")
	ended := make(chan struct{})
	go func() {
		<-srv.jobs.Context().Done()
		time.Sleep(100 * time.Millisecond)
		srv.jobs.Complete("cancelled")
		close(ended)
	}()
	srv.Close()
	select {
	case <-ended:
	default:
		t.Fatal("Close returned while the job's worker was still running")
	}
}

func TestRunningJobNames_CoverEveryCancellableJob(t *testing.T) {
	for jobType := range cancelTitles {
		if _, ok := runningJobNames[jobType]; !ok {
			t.Errorf("%q has no running-job name", jobType)
		}
	}
}

func TestQuitDoesNotAskForARestart(t *testing.T) {
	srv := newDesktopServer(t)
	callLocal(srv.settingsQuit, "POST", "/settings/quit", "")
	awaitQuit(t, srv)
	if srv.RestartRequested() {
		t.Error("a stop asked for a restart")
	}
}

func TestQuitIsIdempotentAndKeepsAPendingRestart(t *testing.T) {
	srv := newDesktopServer(t)
	srv.requestRestart()
	srv.RequestQuit()
	srv.RequestQuit()
	awaitQuit(t, srv)
	if !srv.RestartRequested() {
		t.Error("a second quit dropped the restart")
	}
}

func TestMonloaderWebURLHintNamesTheCompanionPort(t *testing.T) {
	srv := newTestServerWith(t, false, nil)
	if got := srv.monloaderRow().WebURLHint; got != "http://localhost:8456" {
		t.Errorf("hint = %q", got)
	}
	srv.desktop = true
	if got := srv.monloaderRow().WebURLHint; got != "http://localhost:8456" {
		t.Errorf("desktop hint = %q", got)
	}
}

var hicolorSizes = []int{16, 22, 24, 32, 36, 48, 64, 72, 96, 128, 192, 256, 512}

func TestDesktopHookIconFitsTheIconTheme(t *testing.T) {
	srv := newDesktopServer(t)
	cfg, err := png.DecodeConfig(bytes.NewReader(srv.desktopHook().Icon))
	if err != nil {
		t.Fatalf("decoding the hook icon: %v", err)
	}
	if cfg.Width != cfg.Height {
		t.Fatalf("icon is %dx%d, want a square", cfg.Width, cfg.Height)
	}
	if !slices.Contains(hicolorSizes, cfg.Width) {
		t.Errorf("icon is %dpx, want one of %v", cfg.Width, hicolorSizes)
	}
}

func TestDesktopHookFilesMonbooruAsAnImageViewer(t *testing.T) {
	if got := newDesktopServer(t).desktopHook().Categories; got != "Graphics;2DGraphics;RasterGraphics;Viewer;" {
		t.Errorf("menu categories = %q", got)
	}
}

func TestThePowerButtonsWarnOfAJobStartedAfterThePageRendered(t *testing.T) {
	srv := newDesktopServer(t)
	get := func() string {
		w := httptest.NewRecorder()
		srv.desktopPowerFragment(w, localRequest("GET", "/internal/desktop-power", ""))
		return w.Body.String()
	}
	if body := get(); strings.Contains(body, "data-confirm-danger=") || !strings.Contains(body, `hx-trigger="every 5s [!confirmOpen()]"`) {
		t.Errorf("idle fragment = %q, want a polled one with no warning", body)
	}
	mustStartJob(t, srv.jobs, "rebuild-thumbs")
	defer srv.jobs.Complete("")
	if body := get(); !strings.Contains(body, "will not resume.") {
		t.Errorf("fragment during a job = %q, want its warning", body)
	}
}
