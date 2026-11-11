package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/tags"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWith(t, false, nil)
}

func newTestServerWith(t *testing.T, degraded bool, mutate func(*config.Config)) *Server {
	t.Helper()
	return newTestServerAt(t, degraded, mutate, nil)
}

func newTestServerAt(t *testing.T, degraded bool, mutate func(*config.Config), configPath func(dir string) string) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { gallery.WaitPageThumbs(10 * time.Second) })
	galleryDir := filepath.Join(dir, "gallery")
	if degraded {
		galleryDir = filepath.Join(dir, "nonexistent_gallery")
	} else {
		mustMkdirAll(t, galleryDir, 0o755)
	}

	cfg := config.Default()
	cfg.Paths.DataPath = filepath.Join(dir, "data")
	cfg.Galleries[0].GalleryPath = galleryDir
	cfg.Galleries[0].DBPath = filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db")
	cfg.Galleries[0].ThumbnailsPath = filepath.Join(cfg.Paths.DataPath, "default", "thumbnails")
	cfg.Gallery.WatchEnabled = false

	cfg.Server.BaseURL = "http://example.com"
	if mutate != nil {
		mutate(cfg)
	}

	for _, g := range cfg.Galleries {
		if _, err := os.Stat(g.DBPath); errors.Is(err, fs.ErrNotExist) {
			mustMkdirAll(t, filepath.Dir(g.DBPath), 0o755)
			mustWriteFile(t, g.DBPath, mustBootstrappedDB(t), 0o644)
		}
	}

	path := filepath.Join(dir, "monbooru.toml")
	if configPath != nil {
		path = configPath(dir)
	}
	srv, err := NewServer(cfg, path, jobs.NewManager(), Desktop{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

var bootstrappedDB = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "monbooru-web-test")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "monbooru.db")
	database, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	if err := db.Bootstrap(database); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := database.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

func mustBootstrappedDB(t *testing.T) []byte {
	t.Helper()
	data, err := bootstrappedDB()
	if err != nil {
		t.Fatalf("bootstrap template db: %v", err)
	}
	return data
}

func do(srv *Server, method, target string, extra ...[2]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func setTokens(srv *Server, tokens []apitoken.Token) {
	srv.cfgMu.Lock()
	srv.cfg.Auth.Tokens = tokens
	srv.cfgMu.Unlock()
}

func pairPeer(srv *Server, peer string) apitoken.Token {
	tok, _ := apitoken.New(peer+" (paired)", config.AllScopes)
	tok.Paired = peer
	setTokens(srv, []apitoken.Token{tok})
	return tok
}

func postForm(t *testing.T, srv *Server, path string, form url.Values, extra ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	return sendForm(t, srv, "POST", path, form, extra...)
}

func sendForm(t *testing.T, srv *Server, method, path string, form url.Values, extra ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	csrf := srv.sessions.CSRFToken("anon")
	form.Set("_csrf", csrf)
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func postHTMXForm(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, srv, path, form, [2]string{"HX-Request", "true"})
}

func mustTag(t *testing.T, svc *tags.Service, name string, categoryID int64) *models.Tag {
	t.Helper()
	tag, err := svc.GetOrCreateTag(name, categoryID)
	if err != nil {
		t.Fatalf("GetOrCreateTag(%q): %v", name, err)
	}
	return tag
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("expected %d, got %d: %s", want, w.Code, w.Body.String())
	}
}

func mustCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("expected %d, got %d: %s", want, w.Code, w.Body.String())
	}
}

func wantIn(t *testing.T, haystack, want string) {
	t.Helper()
	if !strings.Contains(haystack, want) {
		t.Errorf("missing %q in %q", want, haystack)
	}
}

func mustIn(t *testing.T, haystack, want string) {
	t.Helper()
	if !strings.Contains(haystack, want) {
		t.Fatalf("missing %q in %q", want, haystack)
	}
}

func wantNotIn(t *testing.T, haystack, unwanted string) {
	t.Helper()
	if strings.Contains(haystack, unwanted) {
		t.Errorf("unexpected %q in %q", unwanted, haystack)
	}
}

func TestServerStartsAndServesStatic(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/static/main.css")

	wantCode(t, w, http.StatusOK)
	for path, want := range map[string]string{
		"/static/main.css":    "text/css",
		"/static/main.js":     "text/javascript",
		"/static/favicon.png": "image/png",
	} {
		w := do(srv, "GET", path)
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Errorf("GET %s served Content-Type %q, want %s", path, ct, want)
		}
	}
}

func TestPagesLinkTheStaticFilesThePageCacheKeeps(t *testing.T) {
	srv := newTestServer(t)

	body := do(srv, "GET", "/").Body.String()
	for _, path := range []string{"/static/shared/base.css", "/static/main.css", "/static/shared/htmx.min.js", "/static/shared/base.js", "/static/main.js"} {
		wantIn(t, body, `"`+path+"?v="+assetVersion+`"`)
	}
	for target, want := range map[string]string{
		"/static/main.css?v=" + assetVersion:        "max-age=31536000, immutable",
		"/static/shared/base.css?v=" + assetVersion: "max-age=31536000, immutable",
		"/static/light/logo.png?v=" + assetVersion:  "max-age=31536000, immutable",
		"/static/shared/jetbrains-mono.woff2":       "no-cache",
	} {
		w := do(srv, "GET", target)
		wantCode(t, w, http.StatusOK)
		if got := w.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control %q, want %q", target, got, want)
		}
	}
	w := do(srv, "GET", "/static/shared/jetbrains-mono.woff2", [2]string{"If-None-Match", `"` + assetVersion + `"`})
	wantCode(t, w, http.StatusNotModified)
}

func TestTrailingSlashRedirect(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/categories/")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/categories" {
		t.Errorf("GET /categories/ = %d -> %q, want 301 to /categories", w.Code, w.Header().Get("Location"))
	}

	w = do(srv, "GET", "/definitely-not-here")
	wantCode(t, w, http.StatusNotFound)
}

func TestTrailingSlashRedirect_StaysOnThisServer(t *testing.T) {
	srv := newTestServer(t)
	for _, target := range []string{"/%2Fevil.example/", "/%5Cevil.example/", "/%09/evil.example/"} {
		w := do(srv, "GET", target)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d -> %q, want the 404 page", target, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestSetFlashHeader_KeepsTheHeaderASCII(t *testing.T) {
	const text = "added: 猫耳 é 🐈"
	w := httptest.NewRecorder()
	setFlashHeader(w, text, "ok", nil)
	header := w.Header().Get("HX-Trigger")
	for i := 0; i < len(header); i++ {
		if header[i] >= 0x80 {
			t.Fatalf("byte %#x at %d in %q", header[i], i, header)
		}
	}
	var got map[string]map[string]string
	mustUnmarshal(t, []byte(header), &got)
	if got["action-flash"]["text"] != text {
		t.Errorf("text = %q, want %q", got["action-flash"]["text"], text)
	}
}

func TestSettingsStatsSection(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/settings")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	checks := []string{
		`href="#stats"`,
		`id="stats"`,
		`Process memory`,
		`Database size per gallery`,
		`Goroutines`,
		srv.cfg.Galleries[0].Name,
	}
	for _, c := range checks {
		wantIn(t, body, c)
	}
}

func TestGatherStats_MountLabelsAreCompact(t *testing.T) {
	srv := newMultiGalleryServer(t)
	stats := srv.gatherStats()
	if len(stats.Mounts) == 0 {
		t.Skip("filesystem stats unavailable on this platform")
	}
	for _, m := range stats.Mounts {
		seen := map[string]int{}
		for _, l := range m.Labels {
			seen[l]++
			if seen[l] > 1 {
				t.Errorf("mount labels duplicate gallery name: %v", m.Labels)
			}
		}
		for _, l := range m.Labels {
			if strings.ContainsAny(l, " ") {
				t.Errorf("mount label %q includes a kind suffix; should be just the gallery name", l)
			}
		}
	}
}

func TestPageLoadIndicator_RenderedOnFullLayoutPages(t *testing.T) {
	srv := newTestServer(t)

	pages := []string{"/", "/tags", "/categories", "/settings"}
	for _, page := range pages {
		w := do(srv, "GET", page)

		if w.Code != http.StatusOK {
			t.Errorf("GET %s expected 200, got %d", page, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "page loaded in ") {
			t.Errorf("%s: footer page-load indicator missing", page)
		}
	}
}

func TestMonloaderLight_RendersOnFullLayoutPages(t *testing.T) {
	srv := newTestServer(t)
	pairPeer(srv, "monloader")

	for _, page := range []string{"/", "/tags", "/settings"} {
		w := do(srv, "GET", page)

		if w.Code != http.StatusOK {
			t.Errorf("GET %s expected 200, got %d", page, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "checking monloader") {
			t.Errorf("%s: monloader light shell missing", page)
		}
	}
}

func TestThemeAssets_ReachableWithPasswordAuthAndNoSession(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{
		"aurora/theme.css":   ":root{}",
		"aurora/logo.png":    "PNGBYTES",
		"aurora/favicon.png": "PNGBYTES",
	})
	srv.cfg.Server.Theme = "aurora"
	setTestPassword(t, srv, "s3cret")

	for _, path := range []string{"/theme.css", "/theme.logo", "/theme.favicon"} {
		w := do(srv, "GET", path)

		if w.Code != http.StatusOK {
			t.Errorf("GET %s with auth on and no cookie expected 200, got %d (Location=%q)",
				path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestFavicon_DefaultsToTheBundledIconOnEveryPage(t *testing.T) {
	srv := newTestServer(t)

	pages := []string{"/", "/tags", "/categories", "/settings", "/login"}
	for _, page := range pages {
		w := do(srv, "GET", page)

		if !strings.Contains(w.Body.String(), `href="/static/favicon.png"`) {
			t.Errorf("%s: default favicon link missing", page)
		}
	}
}

func TestFaviconIco_DefaultsToBundledFavicon(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/favicon.ico")

	if loc := w.Header().Get("Location"); loc != "/static/favicon.png" {
		t.Errorf("GET /favicon.ico redirected to %q, want the bundled icon", loc)
	}
}

func decodeManifest(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	w := do(srv, "GET", "/manifest.json")

	mustCode(t, w, http.StatusOK)
	var m map[string]any
	mustUnmarshal(t, w.Body.Bytes(), &m)
	return m
}

func TestManifest_CarriesConfiguredNameAndPalette(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Server.BooruName = "Privbooru"

	m := decodeManifest(t, srv)
	for _, key := range []string{"name", "short_name"} {
		if m[key] != "Privbooru" {
			t.Errorf("manifest %s should track server.name, got %v", key, m[key])
		}
	}
	for _, key := range []string{"background_color", "theme_color"} {
		if m[key] != "#0e0e0e" {
			t.Errorf("manifest %s should match the --bg palette value, got %v", key, m[key])
		}
	}

	srv.cfg.Server.ThemeColor = "#f4f1ee"
	m = decodeManifest(t, srv)
	for _, key := range []string{"background_color", "theme_color"} {
		if m[key] != "#f4f1ee" {
			t.Errorf("manifest %s should track server.theme_color, got %v", key, m[key])
		}
	}
}

func TestThemeColor_InvalidValueSuppressedAtBoot(t *testing.T) {
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		cfg.Server.ThemeColor = "not-a-colour"
	})

	if got := srv.cfg.Server.ThemeColor; got != "" {
		t.Errorf("invalid theme_color should be suppressed at boot, got %q", got)
	}
	m := decodeManifest(t, srv)
	if m["theme_color"] != "#0e0e0e" {
		t.Errorf("manifest theme_color = %v, want the bundled palette", m["theme_color"])
	}
}

func TestManifest_ReachableWithPasswordAuthAndNoSession(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")

	w := do(srv, "GET", "/manifest.json")

	if w.Code != http.StatusOK {
		t.Fatalf("GET /manifest.json with auth on and no cookie expected 200, got %d (Location=%q)",
			w.Code, w.Header().Get("Location"))
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q", ct)
	}
}

func TestManifest_IconIsTheBundledAppIcon(t *testing.T) {
	if bundled := wantBundledManifestIcon(t, newTestServer(t)); bundled != nil && bundled["sizes"] != "192x192" {
		t.Errorf("bundled icon must declare its real size, got %v", bundled["sizes"])
	}
}

func wantBundledManifestIcon(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	icons, _ := decodeManifest(t, srv)["icons"].([]any)
	if len(icons) != 1 {
		t.Errorf("manifest icons = %v, want one entry", icons)
		return nil
	}
	icon, _ := icons[0].(map[string]any)
	if icon["src"] != "/static/icon-192.png" {
		t.Errorf("manifest icon src = %v, want the bundled icon", icon["src"])
	}
	return icon
}

func TestTopbarUsesTheBundledLogoWithNoTheme(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/")

	body := w.Body.String()
	wantIn(t, body, `src="/static/logo.png?v=`+assetVersion+`"`)
	wantNotIn(t, body, `src="/static/favicon.png"`)
}

func TestBooruName_DefaultsToMonbooru(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		page  string
		title string
	}{
		{"/", "Images - Monbooru"},
		{"/tags", "Tags - Monbooru"},
		{"/categories", "Categories - Monbooru"},
		{"/settings", "Settings - Monbooru"},
		{"/login", "Login - Monbooru"},
	}
	for _, tc := range cases {
		w := do(srv, "GET", tc.page)
		if !strings.Contains(w.Body.String(), "<title>"+tc.title+"</title>") {
			t.Errorf("%s: title should default to %q", tc.page, tc.title)
		}
	}
	for _, page := range []string{"/", "/tags"} {
		w := do(srv, "GET", page)
		if !strings.Contains(w.Body.String(), `<span>Mon<span class="wordmark-accent">booru</span></span>`) {
			t.Errorf("%s: topbar wordmark should default to Monbooru", page)
		}
	}
	w := do(srv, "GET", "/login")
	wantIn(t, w.Body.String(), `<h1>Mon<span class="wordmark-accent">booru</span></h1>`)
}

func TestBooruName_OverridesTitleAndWordmark(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Server.BooruName = "Privbooru"

	cases := []struct {
		page  string
		title string
	}{
		{"/", "Images - Privbooru"},
		{"/tags", "Tags - Privbooru"},
		{"/categories", "Categories - Privbooru"},
		{"/settings", "Settings - Privbooru"},
		{"/login", "Login - Privbooru"},
	}
	for _, tc := range cases {
		w := do(srv, "GET", tc.page)
		if !strings.Contains(w.Body.String(), "<title>"+tc.title+"</title>") {
			t.Errorf("%s: title should resolve to %q, body=\n%s", tc.page, tc.title, w.Body.String())
		}
	}
	w := do(srv, "GET", "/")
	wantIn(t, w.Body.String(), `<span>Privbooru</span>`)
	w = do(srv, "GET", "/login")
	wantIn(t, w.Body.String(), `<h1>Privbooru</h1>`)
}

func TestLoginPageRendersWithoutAuth(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/login")

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Password authentication is disabled")
	wantNotIn(t, w.Body.String(), `type="password"`)
}

func TestGalleryReturns200(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/")

	wantCode(t, w, http.StatusOK)
}

func TestGalleryContainsExpectedElements(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/")

	body := w.Body.String()
	checks := []string{
		`id="search-input"`,
		`id="gallery-grid"`,
		`id="batch-bar"`,
		`<span>Mon<span class="wordmark-accent">booru</span></span>`,
		`/static/main.css`,
		`/static/shared/htmx.min.js`,
	}
	for _, s := range checks {
		wantIn(t, body, s)
	}
}

func TestGalleryHTMXPartialReturnsGrid(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/?q=test&sort=newest", [2]string{"HX-Request", "true"}, [2]string{"HX-Target", "gallery-grid"})

	wantCode(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("HTMX partial Content-Type = %q, want text/html…", ct)
	}
	body := w.Body.String()
	wantNotIn(t, body, "<html")
	wantIn(t, body, "thumb-grid")
	wantIn(t, body, "sidebar-inner")
	wantIn(t, body, `id="footer-version" hx-swap-oob="true" title="page loaded in `)
}

func TestGalleryPageAndFragmentBothVaryOnHXTarget(t *testing.T) {
	srv := newTestServer(t)
	for _, target := range []string{"", "gallery-grid"} {
		req := httptest.NewRequest("GET", "/?q=test&sort=newest", nil)
		if target != "" {
			req.Header.Set("HX-Request", "true")
			req.Header.Set("HX-Target", target)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		wantCode(t, w, http.StatusOK)
		if got := w.Header().Values("Vary"); !slices.Contains(got, "HX-Target") {
			t.Errorf("HX-Target %q: Vary = %q, want HX-Target", target, got)
		}
	}
}

func TestGalleryFragmentMidSyncCountsWhatTheGridShows(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	for i := range 120 {
		mustWriteFile(t, filepath.Join(cx.GalleryPath, fmt.Sprintf("s%03d.png", i)), makePNGBytes(t, 4+i%11, 4+i/11, uint8(i), 0, 0), 0o644)
	}
	resultCount := regexp.MustCompile(`id="result-count" class="result-count">(\d+)<`)
	footerCount := regexp.MustCompile(`id="footer-count-images" hx-swap-oob="true">(\d+) img<`)
	ticks := 0
	_, err := cx.Sync(t.Context(), 100, gallery.Naming{}, func(processed, _ int, _ string) {
		if processed == 0 {
			return
		}
		ticks++
		live := countRows(t, cx.DB, "images WHERE is_missing = 0")
		w := do(srv, "GET", "/", [2]string{"HX-Request", "true"}, [2]string{"HX-Target", "gallery-grid"})
		body := w.Body.String()
		for _, re := range []*regexp.Regexp{resultCount, footerCount} {
			if m := re.FindStringSubmatch(body); m == nil || m[1] != fmt.Sprint(live) {
				t.Errorf("at %d processed, %s = %v, want the live %d", processed, re, m, live)
			}
		}
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if ticks < 2 {
		t.Fatalf("saw %d mid-sync ticks, want the 50-file ones", ticks)
	}
}

func TestGallerySearchParamsPreserved(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/?q=mycategory&sort=newest&order=asc&page=1")

	wantCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "mycategory")
	if !strings.Contains(body, `value="newest"`) || !strings.Contains(body, "selected") {
		t.Error("sort option should be selected")
	}
}

func TestSessionMiddlewareRedirectsWhenAuthEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Auth.EnablePassword = true

	w := do(srv, "GET", "/")

	wantCode(t, w, http.StatusSeeOther)
}

func TestRouteStatuses(t *testing.T) {
	srv := newTestServer(t)

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"gallery", "GET", "/", http.StatusOK},
		{"tags", "GET", "/tags", http.StatusOK},
		{"categories", "GET", "/categories", http.StatusOK},
		{"settings", "GET", "/settings", http.StatusOK},
		{"empty search", "GET", "/?q=", http.StatusOK},
		{"job status partial", "GET", "/internal/job/status", http.StatusOK},
		{"GET bypasses CSRF", "GET", "/", http.StatusOK},
		{"missing image", "GET", "/images/99999", http.StatusNotFound},
		{"post without a token", "POST", "/internal/sync", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(srv, tc.method, tc.path)
			if w.Code != tc.want {
				t.Errorf("%s %s = %d, want %d\nbody: %s", tc.method, tc.path, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestJobStatusHandler_RunningMarkup(t *testing.T) {
	srv := newTestServer(t)
	mustStartJob(t, srv.jobs, "re-extract")
	defer srv.jobs.Complete("done")
	srv.jobs.Update(3, 10, "reading metadata")

	w := do(srv, "GET", "/internal/job/status")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		`class="job-running`,
		`data-job-type="re-extract"`,
		`<button class="job-dismiss"`,
		`reading metadata`,
	} {
		wantIn(t, body, want)
	}
}

func TestFooterCounts_SwapsTheThreeCellsOutOfBand(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "footer.png", 8, 8)
	mustTag(t, srv.tagSvc(), "footer_probe", srv.active().GeneralCategoryID)
	tagCount, _ := srv.active().TagCount()

	w := do(srv, "GET", "/internal/footer-counts", [2]string{"HX-Request", "true"})

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, `<span id="footer-count-images" hx-swap-oob="true">1 img</span>`)
	wantIn(t, body, fmt.Sprintf(`<span id="footer-count-tags" hx-swap-oob="true">%d tags</span>`, tagCount))
	wantIn(t, body, `<span id="footer-count-collections" hx-swap-oob="true">0 collections</span>`)
}

func TestJobStatusHandler_RunningJobShowsItsLabel(t *testing.T) {
	for _, tc := range []struct{ jobType, want string }{
		{"rebuild-thumbs", `<span class="job-type">thumbnails</span>`},
		{"sync", `<span class="job-type">sync</span>`},
	} {
		srv := newTestServer(t)
		mustStartJob(t, srv.jobs, tc.jobType)
		wantIn(t, do(srv, "GET", "/internal/job/status").Body.String(), tc.want)
		srv.jobs.Complete("done")
	}
}

func TestJobStatusHandler_NoCancelForWatcher(t *testing.T) {
	srv := newTestServer(t)
	srv.jobs.SetWatcherMessage("added foo.png")

	w := do(srv, "GET", "/internal/job/status")

	body := w.Body.String()
	wantNotIn(t, body, `class="job-running`)
	wantIn(t, body, "added foo.png")
}

func TestJobStatusHandler_CancelTracksCancelTitles(t *testing.T) {
	for _, tc := range []struct {
		jobType string
		title   string
	}{
		{"meta-tags", "Stop the meta tag pass"},
		{"index-workflows", "Stop workflow indexing"},
		{"transfer", "Stop the transfer"},
		{"fold", ""},
		{"vacuum", ""},
	} {
		t.Run(tc.jobType, func(t *testing.T) {
			srv := newTestServer(t)
			mustStartJob(t, srv.jobs, tc.jobType)
			defer srv.jobs.Complete("done")

			body := do(srv, "GET", "/internal/job/status").Body.String()

			if tc.title == "" {
				wantNotIn(t, body, `<button class="job-dismiss"`)
				return
			}
			wantIn(t, body, `<button class="job-dismiss"`)
			wantIn(t, body, `title="`+tc.title+`"`)
		})
	}
}

func insertTestImage(t *testing.T, srv *Server) int64 {
	t.Helper()
	return insertBareImage(t, srv, "/tmp/test.jpg", "jpg", "abc123")
}

func TestDetailPage_BackPageReflectsCachedPositionWhenWarm(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	id := insertTestImage(t, srv)

	q := search.Query{Cached: true, Text: "general:tag", Sort: "newest", Order: "desc"}
	ids := make([]int64, 30)
	for i := range ids {
		ids[i] = int64(900_000 + i)
	}
	ids[12] = id
	srv.active().Source().Cache.Drop()
	srv.active().Source().Cache.Seed(q, ids)

	url := fmt.Sprintf(
		"/images/%d?back_q=%s&back_sort=newest&back_order=desc&back_page=1",
		id, "general%3Atag",
	)
	w := do(srv, "GET", url)

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "&page=3")
	wantNotIn(t, body, "&page=1#img-")
}

func raiseRankPageBudget(t *testing.T) {
	t.Helper()
	old := rankPageBudget
	rankPageBudget = time.Minute
	t.Cleanup(func() { rankPageBudget = old })
}

func raiseHiddenIndicatorBudget(t *testing.T) {
	t.Helper()
	old := galleryHiddenIndicatorBudget
	galleryHiddenIndicatorBudget = time.Minute
	t.Cleanup(func() { galleryHiddenIndicatorBudget = old })
}

func insertRankRows(t *testing.T, srv *Server, prefix string, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := range n {
		ids = append(ids, insertBareImage(t, srv, fmt.Sprintf("/tmp/%s_%d.jpg", prefix, i), "jpg", fmt.Sprintf("%s_sha_%d", prefix, i)))
	}
	return ids
}

func TestDetailPage_BackPageRankQueryOnCacheMiss(t *testing.T) {
	raiseRankPageBudget(t)
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5

	ids := insertRankRows(t, srv, "rank", 12)
	target := ids[3]
	srv.active().Source().Cache.Drop()

	url := fmt.Sprintf(
		"/images/%d?back_q=&back_sort=newest&back_order=desc&back_page=1",
		target,
	)
	w := do(srv, "GET", url)

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "&page=2")
	wantNotIn(t, body, "&page=1#img-")
}

func TestDetailPage_BackPageFollowsTheCarriedPosition(t *testing.T) {
	raiseRankPageBudget(t)
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5

	ids := insertRankRows(t, srv, "carried", 12)
	srv.active().Source().Cache.Drop()

	target := ids[len(ids)-1-9]
	w := do(srv, "GET", fmt.Sprintf(
		"/images/%d?back_q=&back_sort=newest&back_order=desc&back_page=2&back_idx=4", target))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, fmt.Sprintf("&page=2&at=%d", target))
	wantIn(t, body, "back_page=3&back_idx=0")
	wantIn(t, body, "back_page=2&back_idx=3")
}

func TestDetailPage_BackPageSkipsTheRankWhenTheCachedListExcludesTheImage(t *testing.T) {
	raiseRankPageBudget(t)
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5

	ids := insertRankRows(t, srv, "excluded", 12)

	target := ids[3]

	q := search.Query{Cached: true, Sort: "newest", Order: "desc"}
	srv.active().Source().Cache.Drop()
	srv.active().Source().Cache.Seed(q, []int64{900_001, 900_002, 900_003})

	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc&back_page=1", target)
	w := do(srv, "GET", url)

	mustCode(t, w, http.StatusOK)
	wantNotIn(t, w.Body.String(), "&page=2")
}

func TestDetailPage_BackPageFromSimilarityFanOnCacheMiss(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5

	insert := func(name string) int64 { return insertBareImage(t, srv, "/tmp/"+name+".jpg", "jpg", "sim_sha_"+name) }
	shared := mustTag(t, srv.tagSvc(), "plaid_scarf", srv.active().GeneralCategoryID)
	attach := func(id int64) {
		t.Helper()
		if _, err := srv.tagSvc().AddTagsToOneImage(id, []int64{shared.ID}, ""); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 5; i++ {
		insert(fmt.Sprintf("filler_%d", i))
	}
	seedID := insert("seed")
	attach(seedID)
	var sharers []int64
	for i := 0; i < 12; i++ {
		id := insert(fmt.Sprintf("sharer_%02d", i))
		attach(id)
		sharers = append(sharers, id)
	}

	target := sharers[len(sharers)-1-5]
	srv.active().Source().Cache.Drop()

	url := fmt.Sprintf(
		"/images/%d?back_q=similar%%3A%d&back_sort=similarity&back_order=desc&back_page=1",
		target, seedID,
	)
	w := do(srv, "GET", url)

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "&page=2")
	q := search.Query{Cached: true, Text: fmt.Sprintf("similar:%d", seedID), Sort: "similarity", Order: "desc"}
	if last, ok := search.PageOf(srv.source(), q, sharers[0], 1); !ok || last != len(sharers) {
		t.Fatalf("ranked fan not left in the cache: the last sharer sits at %d (ok=%v), want %d", last, ok, len(sharers))
	}
}

func TestDetailPageReturns200(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)

	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))

	wantCode(t, w, http.StatusOK)
}

func TestDetailPageContainsMetadata(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)

	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))

	body := w.Body.String()
	checks := []string{"detail-topbar", "detail-media", "meta-table", "danger-zone",
		`/internal/sidebar-browse`,
		`id="gallery-header"`, `id="search-form"`, `id="search-input"`,
	}
	for _, sel := range checks {
		wantIn(t, body, sel)
	}
}

func TestDetailPageShowsASeedPastInt64(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	mustExec(t, srv.db().Write, `INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler) VALUES (?, 'a cat', '', '', -1, 'euler')`, id)
	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	wantIn(t, w.Body.String(), `<td class="node-param-value">18446744073709551615</td>`)
}

func TestDetailPageShowsGenerationDataWithNullTextColumns(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	mustExec(t, srv.db().Write, `INSERT INTO sd_metadata (image_id, prompt, steps) VALUES (?, 'a cat', 20)`, id)
	mustExec(t, srv.db().Write, `INSERT INTO comfyui_metadata (image_id, seed, generation_hash) VALUES (?, 7, 'abc123')`, id)
	body := do(srv, "GET", fmt.Sprintf("/images/%d", id)).Body.String()
	wantIn(t, body, "<summary>Generation data (SD / Forge)</summary>")
	wantIn(t, body, `<pre class="gen-text">a cat</pre>`)
	wantIn(t, body, "<summary>Generation data (ComfyUI)</summary>")
	wantIn(t, body, "generated:abc123")
	wantNotIn(t, body, "►")
}

func TestDetailPageEscapesCommentaryAndNote(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url, commentary) VALUES (?, 'danbooru', '1', 'https://d/1', ?)`, id, "<script>alert('x')</script>")
	mustExec(t, srv.db().Write, `UPDATE images SET note = ? WHERE id = ?`, "<b>my note</b>", id)
	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	body := w.Body.String()
	wantNotIn(t, body, "<script>alert('x')</script>")
	wantIn(t, body, "&lt;script&gt;")
	wantIn(t, body, "detail-commentary")
	wantNotIn(t, body, "<b>my note</b>")
}

func TestDetailPageRendersAnnotationOverlay(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	mustExec(t, srv.db().Write, `UPDATE images SET width = 200, height = 100 WHERE id = ?`, id)
	mustExec(t, srv.db().Write, `INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body) VALUES (?, 'danbooru', '', 50, 25, 40, 20, ?)`, id, "<script>alert('n')</script>")
	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	body := w.Body.String()
	if !strings.Contains(body, "annotation-overlay") || !strings.Contains(body, "annotation-box") {
		t.Error("annotation overlay/box not rendered")
	}

	wantIn(t, body, "left:25.0000%")
	wantNotIn(t, body, "<script>alert('n')</script>")
}

func TestSidebarBrowseReturnsBrowseSections(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/internal/sidebar-browse")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "folder-tree-section")
	wantNotIn(t, body, `id="tag-groups"`)
}

func TestDegradedModeBannerShown(t *testing.T) {
	srv := newTestServerWith(t, true, nil)

	w := do(srv, "GET", "/")

	body := w.Body.String()
	wantIn(t, body, "degraded-banner")
	wantNotIn(t, body, `action="/internal/sync"`)
}

func TestDegradedModeSyncBlocked(t *testing.T) {
	srv := newTestServerWith(t, true, nil)

	w := postForm(t, srv, "/internal/sync", url.Values{})

	wantCode(t, w, http.StatusServiceUnavailable)
}

func TestMissingImageBannerShown(t *testing.T) {
	srv := newTestServer(t)
	id, _ := mustExec(t, srv.db().Write, `
		INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		VALUES ('/nonexistent/file.jpg', 'jpg', 1024, 'deadbeef', 1, datetime('now'))`).LastInsertId()

	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))

	wantCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "missing-banner")
	wantIn(t, body, "no longer present on disk")
}

func TestMissingImageBannerTellsALeftOutFileFromAGoneOne(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	ingestAt := func(rel string, shade uint8) int64 {
		path := filepath.Join(cx.GalleryPath, rel)
		mustMkdirAll(t, filepath.Dir(path), 0o755)
		mustWriteFile(t, path, makePNGBytes(t, 8, 8, shade, 0, 0), 0o644)
		img, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, path, "")
		mustExec(t, cx.DB.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, img.ID)
		return img.ID
	}
	ignored := ingestAt("screens/a.png", 10)
	back := ingestAt("kept/b.png", 20)
	gone := ingestAt("kept/c.png", 30)
	if err := os.Remove(filepath.Join(cx.GalleryPath, "kept", "c.png")); err != nil {
		t.Fatal(err)
	}
	postForm(t, srv, "/settings/general/ignore", url.Values{"ignore": {"screens"}})

	for _, tc := range []struct {
		id          int64
		want        []string
		removeOffer bool
	}{
		{ignored, []string{"the gallery leaves it out: &#34;screens&#34; is on the ignore list", "Left out: &#34;screens&#34; is on the ignore list"}, false},
		{back, []string{"back on disk; the next sync restores it", "Back on disk, restored by the next sync"}, false},
		{gone, []string{"no longer present on disk", "File missing from disk"}, true},
	} {
		body := mustGetBody(t, srv, fmt.Sprintf("/images/%d", tc.id))
		for _, want := range tc.want {
			wantIn(t, body, want)
		}
		if got := strings.Contains(body, "Remove from library"); got != tc.removeOffer {
			t.Errorf("image %d: Remove from library offered = %v, want %v", tc.id, got, tc.removeOffer)
		}
	}
}

func TestToggleFavoriteReturnsButton(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)

	postW := postForm(t, srv, fmt.Sprintf("/images/%d/favorite", id), url.Values{})

	wantCode(t, postW, http.StatusOK)
	body := postW.Body.String()
	wantIn(t, body, "fav-btn")
}

func TestToggleInboxReturnsButtonAndInvalidatesCount(t *testing.T) {
	srv := newTestServer(t)

	id := insertTestImage(t, srv)
	mustExec(t, srv.db().Write, ` INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at) VALUES ('/tmp/test2.jpg', 'jpg', 1024, 'def456', datetime('now'))`)
	cx := srv.active()

	pre, ok := cx.InboxCount()
	if !ok {
		t.Fatal("InboxCount pre failed")
	}
	if pre != 2 {
		t.Errorf("expected 2 inbox images before toggle, got %d", pre)
	}

	postW := postForm(t, srv, fmt.Sprintf("/images/%d/inbox", id), url.Values{})

	wantCode(t, postW, http.StatusOK)
	body := postW.Body.String()
	wantIn(t, body, "inbox-btn")
	wantIn(t, body, ">Back to inbox</button>")
	wantIn(t, body, `title="Send to inbox (i)"`)
	if !strings.Contains(body, `id="inbox-nav-count"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("toggle inbox response missing inbox-nav-count OOB swap, got: %s", body)
	}
	wantIn(t, body, `<span id="inbox-nav-count" hx-swap-oob="true"> (1)</span>`)

	post, ok := cx.InboxCount()
	if !ok {
		t.Fatal("InboxCount post failed")
	}
	if post != 1 {
		t.Errorf("expected 1 inbox image after toggle, got %d (cache may not have invalidated)", post)
	}
}

func TestDeleteImage(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)

	w := deleteWithCSRF(t, srv, fmt.Sprintf("/images/%d", id))

	wantCode(t, w, http.StatusOK)
	count := countRows(t, srv.db(), "images WHERE id = ?", id)
	if count != 0 {
		t.Error("image should be deleted from DB")
	}

	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("no back-context redirect = %q, want /", got)
	}
}

func insertTwoDistinctImages(t *testing.T, database *db.DB) (older, newer int64) {
	t.Helper()
	older, _ = mustExec(t, database.Write, `
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/older.jpg', 'jpg', 1024, 'sha_older', '2024-01-01T00:00:00Z')`).LastInsertId()
	newer, _ = mustExec(t, database.Write, `
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/newer.jpg', 'jpg', 1024, 'sha_newer', '2025-01-01T00:00:00Z')`).LastInsertId()
	return
}

func TestDeleteImage_RedirectsToNextInSearch(t *testing.T) {
	srv := newTestServer(t)
	older, newer := insertTwoDistinctImages(t, srv.db())

	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc", newer)
	w := deleteWithCSRF(t, srv, url)

	mustCode(t, w, http.StatusOK)
	got := w.Header().Get("HX-Redirect")
	wantPrefix := fmt.Sprintf("/images/%d", older)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("redirect = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "back_sort=newest") || !strings.Contains(got, "back_order=desc") {
		t.Errorf("redirect %q should carry back_sort/back_order", got)
	}
}

func TestDeleteImage_FallsBackToGalleryOnLastImage(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)

	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc", id)
	w := deleteWithCSRF(t, srv, url)

	mustCode(t, w, http.StatusOK)
	got := w.Header().Get("HX-Redirect")
	if strings.HasPrefix(got, "/images/") {
		t.Errorf("last-image redirect = %q, want gallery URL", got)
	}
}

func TestDeleteImage_NotFound(t *testing.T) {
	srv := newTestServer(t)

	w := deleteWithCSRF(t, srv, "/images/999999")

	mustCode(t, w, http.StatusNotFound)
}

func TestSettingsGeneralPost(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/settings/general", url.Values{"watch_enabled": {"on"}, "max_file_size_mb": {"200"}, "page_size": {"60"}})

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Saved")
	if !srv.cfg.Gallery.WatchEnabled {
		t.Error("WatchEnabled should be true after save")
	}
	if srv.cfg.Gallery.MaxFileSizeMB != 200 {
		t.Errorf("MaxFileSizeMB = %d, want 200", srv.cfg.Gallery.MaxFileSizeMB)
	}
	if srv.cfg.UI.PageSize != 60 {
		t.Errorf("PageSize = %d, want 60", srv.cfg.UI.PageSize)
	}
}

func TestSettingsGeneralPost_ReachesARunningWatcher(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Gallery.WatchEnabled = true })
	srv.StartWatchers()
	cx := srv.active()

	w := postForm(t, srv, "/settings/general", url.Values{
		"watch_enabled": {"on"}, "max_file_size_mb": {"100"}, "page_size": {"60"},
		"rename_on_ingest": {"on"}, "default_upload_name": {"{gallery}-{id}"},
	})
	wantIn(t, w.Body.String(), "Saved")
	mustWriteFile(t, filepath.Join(cx.GalleryPath, "dropped.png"), makePNGBytes(t, 8, 8, 10, 20, 30), 0o644)

	want := filepath.Join(cx.GalleryPath, "default-1.png")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got string
		_ = cx.DB.Read.QueryRow(`SELECT canonical_path FROM images`).Scan(&got)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("canonical_path = %q, want the saved template applied: %q", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSettingsPage_SlidersHoldAStoredValueOutsideTheirRange(t *testing.T) {
	for _, tc := range []struct {
		name, field, min, max string
		mutate                func(*config.Config)
	}{
		{"no size cap", "max_file_size_mb", "0", "5120", func(c *config.Config) { c.Gallery.MaxFileSizeMB = 0 }},
		{"a cap past the slider", "max_file_size_mb", "1", "10240", func(c *config.Config) { c.Gallery.MaxFileSizeMB = 10240 }},
		{"a page size past the slider", "page_size", "1", "250", func(c *config.Config) { c.UI.PageSize = 250 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWith(t, false, tc.mutate)
			body := do(srv, "GET", "/settings").Body.String()
			input := regexp.MustCompile(`<input type="range" name="` + tc.field + `"[^>]*>`).FindString(body)
			wantIn(t, input, `min="`+tc.min+`" max="`+tc.max+`"`)
		})
	}
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Gallery.MaxFileSizeMB = 0 })
	wantIn(t, do(srv, "GET", "/settings").Body.String(), "0 means no cap")
}

func TestSettingsIgnorePost(t *testing.T) {
	srv := newTestServer(t)
	root := srv.get("default").GalleryPath

	w := postForm(t, srv, "/settings/general/ignore", url.Values{"ignore": {"@eaDir\r\ndrafts/\r\n\r\n"}})
	body := w.Body.String()
	wantIn(t, body, "A sync applies the ignore list")
	wantIn(t, body, `<div id="ignore-list" class="ignore-list" hx-swap-oob="true">`)
	wantIn(t, body, "<span title=\"@eaDir\ndrafts\">2 patterns</span>")
	if want := []string{"@eaDir", "drafts"}; !slices.Equal(srv.cfg.Gallery.Ignore, want) {
		t.Errorf("Ignore = %q, want %q", srv.cfg.Gallery.Ignore, want)
	}
	if !srv.get("default").Boundary().Excludes(filepath.Join(root, "a", "drafts", "x.png")) {
		t.Error("the saved list did not reach the gallery's boundary")
	}

	postForm(t, srv, "/settings/general", url.Values{"default_upload_folder": {"inbox"}, "page_size": {"40"}})
	if want := []string{"@eaDir", "drafts"}; !slices.Equal(srv.cfg.Gallery.Ignore, want) {
		t.Errorf("a General save changed the list to %q", srv.cfg.Gallery.Ignore)
	}
	for _, c := range []struct {
		ignore, want string
	}{
		{"ok\n*", `&#34;*&#34; would leave out every file.`},
		{"[abc", `&#34;[abc&#34; is not a valid pattern.`},
		{"inbox", `Received files go to a folder this list leaves out: &#34;inbox&#34; is on the ignore list.`},
	} {
		w := postForm(t, srv, "/settings/general/ignore", url.Values{"ignore": {c.ignore}})
		wantIn(t, w.Body.String(), c.want)
	}
	if want := []string{"@eaDir", "drafts"}; !slices.Equal(srv.cfg.Gallery.Ignore, want) {
		t.Errorf("a refused save changed the list to %q", srv.cfg.Gallery.Ignore)
	}

	w = postForm(t, srv, "/settings/general", url.Values{"default_upload_folder": {"a/drafts"}})
	wantIn(t, w.Body.String(), `&#34;a/drafts&#34; is on the ignore list`)

	for i, rel := range []string{"late/x.png", "late/sub/y.png", "kept.png"} {
		path := filepath.Join(root, rel)
		mustMkdirAll(t, filepath.Dir(path), 0o755)
		mustWriteFile(t, path, makePNGBytes(t, 8, 8, uint8(40+i), 0, 0), 0o644)
		mustIngest(t, srv.db(), root, srv.active().ThumbnailsPath, path, "")
	}
	w = postForm(t, srv, "/settings/general/ignore", url.Values{"ignore": {"@eaDir\ndrafts\nlate"}})
	wantIn(t, w.Body.String(), "The next sync sets aside 2 indexed images this list leaves out.")
}

func TestSettingsMonloaderPost(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/settings/monloader", url.Values{"monloader_url": {"http://localhost:8081"}})

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Saved")
	if srv.cfg.Server.MonloaderURL != "http://localhost:8081" {
		t.Errorf("MonloaderURL = %q, want http://localhost:8081", srv.cfg.Server.MonloaderURL)
	}
}

func TestSettingsMonloaderPostRefusesANonURL(t *testing.T) {
	srv := newTestServer(t)
	for field, value := range map[string]string{"monloader_url": "monloader.lan:8456", "api_url": "ftp://monloader:8456"} {
		w := postForm(t, srv, "/settings/monloader", url.Values{field: {value}})
		wantIn(t, w.Body.String(), "must be http(s) URLs")
	}
	if srv.cfg.Server.MonloaderURL != "" || srv.cfg.Monloader.APIURL != "" {
		t.Errorf("a refused save stored %q / %q", srv.cfg.Server.MonloaderURL, srv.cfg.Monloader.APIURL)
	}
}

func TestSettingsTagger_RejectsBadName(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	csrf := srv.sessions.CSRFToken("anon")
	form := "_csrf=" + csrf

	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/settings/tagger/foo$bar/enable"},
		{"POST", "/settings/tagger/foo$bar/disable"},
		{"GET", "/settings/tagger/foo$bar/config"},
		{"POST", "/settings/tagger/foo$bar/config"},
		{"POST", "/settings/tagger/foo$bar/reset"},
		{"POST", "/settings/tagger/foo$bar/delete"},
	}
	for _, tc := range cases {
		var body string
		if tc.method == "POST" {
			body = form
		}
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
		if tc.method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: expected 400 for bad tagger name, got %d", tc.method, tc.path, w.Code)
		}
	}

	for _, ti := range srv.cfg.Tagger.Taggers {
		if ti.Name == "foo$bar" {
			t.Errorf("rejected tagger name persisted to config: %+v", ti)
		}
	}
}

func TestSettingsTaggerLabels_NonDefaultLabelFile(t *testing.T) {
	srv := newTestServer(t)
	modelRoot := filepath.Join(t.TempDir(), "models")
	dir := installTagger(t, modelRoot, "joytag", "")
	mustWriteFile(t, filepath.Join(dir, "tags.txt"), []byte("1girl\nmonochrome\n"), 0o644)
	srv.cfg.Paths.ModelPath = modelRoot
	srv.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "joytag", Enabled: true, ConfidenceThreshold: 0.4}}

	body := mustGetBody(t, srv, "/settings/tagger/joytag/labels")
	wantNotIn(t, body, "Cannot read the label file")
	wantIn(t, body, `data-source="monochrome"`)
}

func TestPruneMissingImages(t *testing.T) {
	srv := newTestServer(t)

	seedImage(t, srv, "present.png", 10, 10)

	mustExec(t, srv.db().Write, ` INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at) VALUES ('/nonexistent/file.jpg', 'jpg', 1024, 'prune_test_hash', 1, datetime('now'))`)

	w := postForm(t, srv, "/settings/maintenance/prune-missing", url.Values{})

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "started")

	awaitJobsDrain(t, srv)

	count := countRows(t, srv.db(), "images WHERE sha256 = 'prune_test_hash'")
	if count != 0 {
		t.Error("missing image should have been pruned")
	}
}

func TestPruneMissingImages_KeepsARowWhoseFileIsStillOnDisk(t *testing.T) {
	srv := newTestServer(t)
	kept := seedImage(t, srv, "still_here.png", 10, 10)
	mustExec(t, srv.db().Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, kept)
	mustExec(t, srv.db().Write, ` INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at) VALUES ('/nonexistent/gone.jpg', 'jpg', 1024, 'gone_hash', 1, datetime('now'))`)

	w := postForm(t, srv, "/settings/maintenance/prune-missing", url.Values{})
	wantIn(t, w.Body.String(), "started")
	awaitJobsDrain(t, srv)

	if n := countRows(t, srv.db(), "images WHERE id = ?", kept); n != 1 {
		t.Error("a row whose file is still on disk was pruned")
	}
	if n := countRows(t, srv.db(), "images WHERE sha256 = 'gone_hash'"); n != 0 {
		t.Error("the row whose file is gone survived")
	}
	if got := srv.jobs.Get().Summary; got != "Removed 1 missing image; kept 1 whose file is still on disk." {
		t.Errorf("summary = %q", got)
	}
}

func TestPruneMissingImages_RejectsConcurrentRun(t *testing.T) {
	srv := newTestServer(t)

	mustStartJob(t, srv.jobs, models.JobTypeVacuum)
	defer srv.jobs.Complete("test cleanup")

	mustExec(t, srv.db().Write, ` INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at) VALUES ('/nonexistent/conflict.jpg', 'jpg', 1024, 'conflict_hash', 1, datetime('now'))`)

	w := postForm(t, srv, "/settings/maintenance/prune-missing", url.Values{})

	wantCode(t, w, http.StatusConflict)
}

func TestPruneOrphanedThumbnails_AsJob(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	orphan := filepath.Join(cx.ThumbnailsPath, "777777.jpg")
	mustWriteFile(t, orphan, []byte("orphan"), 0o644)

	w := postForm(t, srv, "/settings/maintenance/prune-orphaned-thumbnails", url.Values{})

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "started")

	awaitJobsDrain(t, srv)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan thumbnail should have been removed")
	}
}

func TestStalePageWriteLeavesTheActiveGalleryAlone(t *testing.T) {
	srv := newMultiGalleryServer(t)
	for _, name := range []string{"default", "stock"} {
		cx := srv.get(name)
		mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size, ingested_at) VALUES (1, ?, ?, 'png', 5, datetime('now'))`, name+"-sha", filepath.Join(cx.GalleryPath, name+".png"))
	}
	if err := srv.switchGallery("stock"); err != nil {
		t.Fatal(err)
	}
	post := func(path, page string, form url.Values) *httptest.ResponseRecorder {
		return postForm(t, srv, path, form, [2]string{"X-Monbooru-Gallery", page})
	}
	stock := srv.get("stock").DB
	var favorited, rows int

	if w := post("/images/1/favorite", "default", url.Values{}); w.Code != http.StatusConflict {
		t.Errorf("favorite from a page of gallery default: %d %q", w.Code, w.Body.String())
	}
	if w := post("/internal/batch-delete", "default", url.Values{"ids": {"1"}}); w.Code != http.StatusConflict {
		t.Errorf("batch delete from a page of gallery default: %d %q", w.Code, w.Body.String())
	}
	_ = stock.Read.QueryRow(`SELECT COUNT(*), COALESCE(MAX(is_favorited), 0) FROM images WHERE id = 1`).Scan(&rows, &favorited)
	if rows != 1 || favorited != 0 {
		t.Fatalf("stock's image 1 after writes from another gallery's page: rows=%d favorited=%d", rows, favorited)
	}

	if w := post("/images/1/favorite", "stock", url.Values{}); w.Code >= 400 {
		t.Errorf("favorite from a page of gallery stock: %d %q", w.Code, w.Body.String())
	}
	_ = stock.Read.QueryRow(`SELECT is_favorited FROM images WHERE id = 1`).Scan(&favorited)
	if favorited != 1 {
		t.Error("a write from a page of the active gallery did not land")
	}
}

func TestStalePageSwapAsksForAReload(t *testing.T) {
	srv := newMultiGalleryServer(t)
	if err := srv.switchGallery("stock"); err != nil {
		t.Fatal(err)
	}
	get := func(page string, htmx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/?page=2", nil)
		req.Header.Set("X-Monbooru-Gallery", page)
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	w := get("default", true)
	mustCode(t, w, http.StatusConflict)
	if got := w.Header().Get("X-Monbooru-Gallery"); got != "stock" {
		t.Errorf("X-Monbooru-Gallery = %q, want stock", got)
	}
	wantIn(t, w.Body.String(), "This page shows gallery default, but stock is now active.")
	mustCode(t, get("stock", true), http.StatusOK)
	mustCode(t, get("default", false), http.StatusOK)
}

func newMultiGalleryServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWith(t, false, func(c *config.Config) {
		g2 := filepath.Join(filepath.Dir(c.Paths.DataPath), "g2")
		mustMkdirAll(t, g2, 0o755)
		c.Galleries = append(c.Galleries, config.Gallery{
			Name: "stock", GalleryPath: g2,
			DBPath:         filepath.Join(c.Paths.DataPath, "stock", "monbooru.db"),
			ThumbnailsPath: filepath.Join(c.Paths.DataPath, "stock", "thumbnails"),
		})
	})
}

func TestGallerySwitcherButtonShownWithMultipleGalleries(t *testing.T) {
	srv := newMultiGalleryServer(t)

	for _, path := range []string{"/", "/categories", "/tags"} {
		w := do(srv, "GET", path)

		body := w.Body.String()
		if !strings.Contains(body, `id="gallery-switch-btn"`) {
			t.Errorf("%s: layout should render the gallery switcher button with 2+ galleries", path)
		}
		if !strings.Contains(body, `id="gallery-switch-dialog"`) {
			t.Errorf("%s: layout should render the gallery switch dialog with 2+ galleries", path)
		}
	}
}

func TestGallerySwitchChangesActive(t *testing.T) {
	srv := newMultiGalleryServer(t)

	w := postHTMXForm(t, srv, "/internal/gallery/switch", url.Values{"name": {"stock"}})

	mustCode(t, w, http.StatusOK)
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want / (the old page belongs to the old gallery's namespace)", got)
	}
	if srv.activeGallery() != "stock" {
		t.Errorf("active gallery = %q, want stock", srv.activeGallery())
	}
}

func TestGallerySwitch_RedirectsHomeFromSearch(t *testing.T) {
	srv := newMultiGalleryServer(t)

	w := postForm(t, srv, "/internal/gallery/switch", url.Values{"name": {"stock"}}, [2]string{"HX-Request", "true"}, [2]string{"HX-Current-URL", "http://localhost/?q=cat"})

	mustCode(t, w, http.StatusOK)
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want /", got)
	}
	if w.Header().Get("HX-Refresh") != "" {
		t.Error("search-bearing URL should redirect, not refresh in place")
	}
}

func TestGallerySwitcherHiddenWithSingleGallery(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/")
	wantNotIn(t, w.Body.String(), `id="gallery-switch-btn"`)
}

func TestGalleryAccessorsDoNotReenterContextLock(t *testing.T) {
	srv := newTestServer(t)

	srv.set.RLock()
	defer srv.set.RUnlock()

	waiting := make(chan struct{})
	go func() {
		close(waiting)
		srv.set.Lock()
		//lint:ignore SA2001 only the queued acquisition matters
		srv.set.Unlock() //nolint:staticcheck
	}()
	<-waiting
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.active()
		srv.get("default")
		srv.db()
		srv.tagSvc()
		srv.activeGallery()
		srv.allContexts()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gallery accessors blocked behind a queued writer on the set")
	}
}

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustSetHome(t *testing.T, database *db.DB, imageID int64, name string, order *int) {
	t.Helper()
	if err := gallery.SetHomeCollection(database, imageID, name, order); err != nil {
		t.Fatalf("SetHomeCollection(%d, %q): %v", imageID, name, err)
	}
}

func mustAddCollection(t *testing.T, database *db.DB, imageID int64, name string, order *int) {
	t.Helper()
	if err := gallery.AddCollectionMembership(database, imageID, name, order); err != nil {
		t.Fatalf("AddCollectionMembership(%d, %q): %v", imageID, name, err)
	}
}

func mustIngest(t *testing.T, database *db.DB, galleryPath, thumbnailsPath, path, origin string) (*models.Image, bool) {
	t.Helper()
	img, dup, err := gallery.Ingest(gallery.Handle{GalleryPath: galleryPath, ThumbnailsPath: thumbnailsPath, DB: database, TagSvc: tags.New(database, nil)}, path, origin)
	if err != nil {
		t.Fatalf("Ingest %s: %v", path, err)
	}
	return img, dup
}

func mustImport(t *testing.T, srv *Server, name, format string, upload io.Reader) {
	t.Helper()
	if _, err := srv.importGallery(name, format, upload); err != nil {
		t.Fatalf("importGallery(%q, %q): %v", name, format, err)
	}
}

func mustAddTag(t *testing.T, svc *tags.Service, imageID, tagID int64, isAuto bool, confidence *float64) {
	t.Helper()
	if err := svc.AddTagToImage(imageID, tagID, isAuto, confidence); err != nil {
		t.Fatalf("AddTagToImage(%d, %d): %v", imageID, tagID, err)
	}
}

func mustAddImply(t *testing.T, svc *tags.Service, parentID, impliedID int64) bool {
	t.Helper()
	created, err := svc.AddImplication(parentID, impliedID)
	if err != nil {
		t.Fatalf("AddImplication(%d, %d): %v", parentID, impliedID, err)
	}
	return created
}

func mustCreateAlias(t *testing.T, svc *tags.Service, name string, categoryID, canonicalID int64) *models.Tag {
	t.Helper()
	tag, err := svc.CreateAlias(name, categoryID, canonicalID)
	if err != nil {
		t.Fatalf("CreateAlias(%q): %v", name, err)
	}
	return tag
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func mustWriteField(t *testing.T, mw *multipart.Writer, field, value string) {
	t.Helper()
	if err := mw.WriteField(field, value); err != nil {
		t.Fatalf("write field %s: %v", field, err)
	}
}

func mustStartJob(t *testing.T, m *jobs.Manager, jobType string) {
	t.Helper()
	if err := m.Start(jobType); err != nil {
		t.Fatalf("jobs.Start(%q): %v", jobType, err)
	}
}

func mustLoadNextPair(t *testing.T, cx *library.Gallery, order, detector string, ceiling *Ceiling, pinA, pinB int64) (*sessionPairView, relations.QueueCounts) {
	t.Helper()
	pair, counts, err := loadNextPair(cx, order, detector, ceiling, pinA, pinB)
	if err != nil {
		t.Fatalf("loadNextPair: %v", err)
	}
	return pair, counts
}
