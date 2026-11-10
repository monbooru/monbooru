package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/registry"
	"github.com/monbooru/monbooru/internal/shared/webui"
	webFS "github.com/monbooru/monbooru/web"
)

func seedHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func installThemes(t *testing.T, srv *Server, files map[string]string) string {
	t.Helper()
	dir := srv.themesDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		mustMkdirAll(t, filepath.Dir(path), 0o755)
		mustWriteFile(t, path, []byte(body), 0o644)
	}
	return dir
}

func TestListThemesTakesBothShapes(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{
		"aurora/theme.css":  ":root{--bg:#fff}",
		"aurora/logo.png":   "png",
		"midnight.css":      ":root{--bg:#000}",
		"notatheme/read.me": "no stylesheet here",
	})

	got := srv.listThemes()
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "dark,light,legacy-dark,legacy-light,aurora,midnight" {
		t.Fatalf("themes = %v", names)
	}
	if got[4].Logo == "" {
		t.Errorf("folder form = %+v, want aurora carrying its logo", got[4])
	}
	if got[5].Logo != "" || got[5].Builtin {
		t.Errorf("bare css form = %+v", got[5])
	}
}

func TestListThemesLogsARepeatedNoteOnce(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{"twin/theme.css": ":root{}", "twin.css": ":root{}"})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for range 3 {
		srv.listThemes()
	}
	if n := strings.Count(buf.String(), "WARN"); n != 1 {
		t.Errorf("three listings logged %d warning(s), want one:\n%s", n, buf.String())
	}
}

func TestInstalledThemeOverridesABuiltin(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{"light/theme.css": ":root{--bg:#eee}"})

	got := srv.listThemes()
	if len(got) != 4 || got[1].Name != "light" || got[1].Builtin {
		t.Fatalf("themes = %+v, want the installed light", got)
	}
	srv.cfg.Server.Theme = "light"
	e := srv.activeTheme()
	if e.Builtin {
		t.Errorf("active = %+v, want the installed copy", e)
	}
	if got := themeLogoURL(e); got != "/static/light/logo.png" {
		t.Errorf("logo url = %q, want the light art", got)
	}
	if got := themeMascots(e)["lost"]; got != "/static/light/mascot-lost.png" {
		t.Errorf("lost drawing = %q, want the light art", got)
	}
}

func TestBuiltinThemesDrawTheMascotAndTheLegacyOnesDoNot(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct{ theme, logo, lost string }{
		{"dark", "/static/logo.png", "/static/mascot-lost.png"},
		{"light", "/static/light/logo.png", "/static/light/mascot-lost.png"},
		{"legacy-dark", "/theme.logo", ""},
		{"legacy-light", "/theme.logo", ""},
	} {
		srv.cfg.Server.Theme = tc.theme
		e := srv.activeTheme()
		if e.Name != tc.theme {
			t.Fatalf("%s resolved to %+v", tc.theme, e)
		}
		if got := themeLogoURL(e); got != tc.logo {
			t.Errorf("%s logo = %q, want %q", tc.theme, got, tc.logo)
		}
		if got := themeMascots(e)["lost"]; got != tc.lost {
			t.Errorf("%s lost drawing = %q, want %q", tc.theme, got, tc.lost)
		}
	}
	for _, path := range []string{"/static/light/logo.png", "/static/light/mascot-sleep.png", "/static/mascot-panic.png"} {
		if rr := do(srv, "GET", path); rr.Code != http.StatusOK || !strings.HasPrefix(rr.Body.String(), "\x89PNG") {
			t.Errorf("%s = %d", path, rr.Code)
		}
	}
	for _, name := range []string{"logo.png", "favicon.png", "mascot-lost.png"} {
		want, _ := fs.ReadFile(registry.FS, registry.LightThemeDir+"/"+name)
		if rr := do(srv, "GET", "/static/light/"+name); len(want) == 0 || !bytes.Equal(rr.Body.Bytes(), want) {
			t.Errorf("/static/light/%s is not the registry's %s", name, name)
		}
	}
	srv.cfg.Server.Theme = "legacy-light"
	legacy, _ := fs.ReadFile(registry.FS, registry.LegacyLightThemeDir+"/logo.png")
	if rr := do(srv, "GET", "/theme.logo"); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), legacy) {
		t.Errorf("/theme.logo under legacy-light = %d, %d bytes", rr.Code, rr.Body.Len())
	}
}

func TestThemeMascotServesOnlyTheFoldersOwnDrawings(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{
		"aurora/theme.css":       ":root{--bg:#fff}",
		"aurora/mascot-lost.png": "lost-bytes",
	})
	srv.cfg.Server.Theme = "aurora"
	got := themeMascots(srv.activeTheme())
	if got["lost"] != "/theme.mascot/lost" || got["sleep"] != "" {
		t.Errorf("aurora's drawings = %v, want its lost face and nothing else", got)
	}
	if rr := do(srv, "GET", "/theme.mascot/lost"); rr.Code != http.StatusOK || rr.Body.String() != "lost-bytes" {
		t.Errorf("/theme.mascot/lost = %d %q", rr.Code, rr.Body.String())
	}
	if rr := do(srv, "GET", "/theme.mascot/sleep"); rr.Code != http.StatusNotFound {
		t.Errorf("/theme.mascot/sleep = %d, want 404", rr.Code)
	}
}

func TestSeedThemesDirWritesTheExampleOnce(t *testing.T) {
	srv := newTestServer(t)
	srv.seedThemesDir()
	sheet := filepath.Join(srv.themesDir(), "light", "theme.css")
	body, err := os.ReadFile(sheet)
	if err != nil {
		t.Fatalf("no seeded theme: %v", err)
	}
	wantIn(t, string(body), "color-scheme: light")
	if _, err := os.Stat(filepath.Join(srv.themesDir(), "light", "logo.png")); err == nil {
		t.Error("the seeded copy carries a logo, which each app brings itself")
	}

	if err := os.RemoveAll(filepath.Join(srv.themesDir(), "light")); err != nil {
		t.Fatal(err)
	}
	srv.seedThemesDir()
	if _, err := os.Stat(sheet); err == nil {
		t.Error("seeding rebuilt a copy the operator deleted")
	}
}

func TestSeedThemesDirFillsInWhatTheCopyLacks(t *testing.T) {
	srv := newTestServer(t)
	light := filepath.Join(srv.themesDir(), "light")
	if err := os.Remove(filepath.Join(light, "theme.css")); err != nil {
		t.Fatal(err)
	}
	srv.seedThemesDir()
	if body, err := os.ReadFile(filepath.Join(light, "theme.css")); err != nil || !strings.Contains(string(body), "color-scheme: light") {
		t.Errorf("theme.css was not filled in: %q %v", body, err)
	}
}

func TestSeedThemesDirMovesAnUntouchedCopyToTheAppsLightArt(t *testing.T) {
	srv := newTestServer(t)
	light := filepath.Join(srv.themesDir(), "light")
	sheet, _ := fs.ReadFile(registry.FS, builtinLightDir+"/theme.css")
	oldLogo, oldIcon, edited := "previous logo", "previous icon", "the operator's logo"
	record := func(logo string) {
		mustWriteFile(t, filepath.Join(light, "logo.png"), []byte(logo), 0o644)
		mustWriteFile(t, filepath.Join(light, "favicon.png"), []byte(oldIcon), 0o644)
		mustWriteFile(t, filepath.Join(light, webui.SeedFile), []byte(seedHash(sheet)+"  theme.css\n"+seedHash([]byte(oldIcon))+"  favicon.png\n"+seedHash([]byte(oldLogo))+"  logo.png\n"), 0o644)
	}
	srv.cfg.Server.Theme = "light"

	record(oldLogo)
	srv.seedThemesDir()
	for _, file := range []string{"logo.png", "favicon.png"} {
		if _, err := os.Stat(filepath.Join(light, file)); err == nil {
			t.Errorf("the untouched copy kept the previous %s", file)
		}
	}
	if got := themeFaviconURL(srv.activeTheme()); got != "/static/light/favicon.png" {
		t.Errorf("favicon url = %q, want the light art", got)
	}

	record(edited)
	srv.seedThemesDir()
	if body, _ := os.ReadFile(filepath.Join(light, "logo.png")); string(body) != edited {
		t.Errorf("an edited logo was dropped: %q", body)
	}
	if got := themeLogoURL(srv.activeTheme()); got != "/theme.logo" {
		t.Errorf("logo url = %q, want the operator's own", got)
	}
}

func TestActiveThemeRefusesAnythingButAListedName(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{"light/theme.css": ":root{--bg:#fff}"})

	for _, name := range []string{"../../etc/passwd", "light/theme.css", "gone", "/etc/passwd"} {
		srv.cfg.Server.Theme = name
		if e := srv.activeTheme(); e.Name != "dark" {
			t.Errorf("theme %q resolved to %+v, want the dark fallback", name, e)
		}
	}
	srv.cfg.Server.Theme = "light"
	if e := srv.activeTheme(); e.Path == "" {
		t.Error("a listed name should resolve to a stylesheet")
	}
}

func TestThemeRoutesFollowTheActiveTheme(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{
		"light/theme.css": ":root{--bg:#fff}",
		"light/logo.png":  "png-bytes",
		"plain.css":       ":root{--bg:#111}",
	})

	get := func(path string) (int, string) {
		rr := do(srv, "GET", path)
		return rr.Code, rr.Body.String()
	}

	if code, _ := get("/theme.css"); code != http.StatusNotFound {
		t.Errorf("/theme.css with no theme = %d", code)
	}
	rr := do(srv, "GET", "/settings")
	wantNotIn(t, rr.Body.String(), `href="/theme.css"`)

	srv.cfg.Server.Theme = "light"
	if code, body := get("/theme.css"); code != http.StatusOK || !strings.Contains(body, "--bg:#fff") {
		t.Errorf("/theme.css = %d %q", code, body)
	}
	if code, body := get("/theme.logo"); code != http.StatusOK || body != "png-bytes" {
		t.Errorf("/theme.logo = %d %q", code, body)
	}
	if got := themeLogoURL(srv.activeTheme()); got != "/theme.logo" {
		t.Errorf("logo url = %q, want the theme's", got)
	}

	srv.cfg.Server.Theme = "plain"
	if code, _ := get("/theme.logo"); code != http.StatusNotFound {
		t.Errorf("/theme.logo for a logo-less theme = %d", code)
	}
	if got := themeLogoURL(srv.activeTheme()); got != "/static/logo.png" {
		t.Errorf("logo url = %q, want the bundled one", got)
	}
}

func TestThemeFileRevalidatesAgainstItsMtime(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{"aurora/theme.css": ":root{--bg:#fff}"})
	srv.cfg.Server.Theme = "aurora"

	rr := do(srv, "GET", "/theme.css")
	if cc := rr.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, no-cache")
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag; an edited sheet would go heuristically stale")
	}

	rr = do(srv, "GET", "/theme.css", [2]string{"If-None-Match", etag})
	if rr.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rr.Code)
	}
}

func installThemesAtOneMtime(t *testing.T, srv *Server, files map[string]string) {
	t.Helper()
	dir := installThemes(t, srv, files)
	when := time.Unix(1700000000, 0)
	for name := range files {
		if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThemeSwitchRevalidatesBetweenThemesWithOneMtime(t *testing.T) {
	srv := newTestServer(t)
	installThemesAtOneMtime(t, srv, map[string]string{"a/theme.css": ":root{--bg:#aaa}", "b/theme.css": ":root{--bg:#bbb}"})
	srv.cfg.Server.Theme = "a"
	rr := do(srv, "GET", "/theme.css")
	etag := rr.Header().Get("ETag")

	srv.cfg.Server.Theme = "b"
	rr = do(srv, "GET", "/theme.css", [2]string{"If-None-Match", etag})
	if rr.Code != http.StatusOK || rr.Body.String() != ":root{--bg:#bbb}" {
		t.Errorf("b revalidated with a's tag %s: %d %q, want 200 with b's sheet", etag, rr.Code, rr.Body.String())
	}
}

func TestFaviconVersionNamesTheTheme(t *testing.T) {
	srv := newTestServer(t)
	installThemesAtOneMtime(t, srv, map[string]string{
		"a/theme.css": ":root{--bg:#aaa}", "a/favicon.png": "a-tab",
		"b/theme.css": ":root{--bg:#bbb}", "b/favicon.png": "b-tab",
	})
	srv.cfg.Server.Theme = "a"
	first := themeFaviconURL(srv.activeTheme())
	srv.cfg.Server.Theme = "b"
	if other := themeFaviconURL(srv.activeTheme()); other == first {
		t.Errorf("a and b share the icon url %q", other)
	}
}

func TestBuiltinLightServesItsSheetAndTheAppsLightArt(t *testing.T) {
	srv := newTestServer(t)
	if err := os.RemoveAll(srv.themesDir()); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Server.Theme = "light"
	if e := srv.activeTheme(); !e.Builtin {
		t.Fatalf("active = %+v, want the built-in", e)
	}
	get := func(path string) (int, string) {
		rr := do(srv, "GET", path)
		return rr.Code, rr.Body.String()
	}

	if code, body := get("/theme.css"); code != http.StatusOK || !strings.Contains(body, "color-scheme: light") {
		t.Errorf("/theme.css = %d %q", code, body)
	}
	if got := themeLogoURL(srv.activeTheme()); got != "/static/light/logo.png" {
		t.Errorf("logo url = %q, want the light art", got)
	}
	if got := themeFaviconURL(srv.activeTheme()); got != "/static/light/favicon.png" {
		t.Errorf("favicon url = %q, want the light art", got)
	}

	srv.cfg.Server.Theme = "dark"
	if code, _ := get("/theme.css"); code != http.StatusNotFound {
		t.Errorf("/theme.css under dark = %d", code)
	}
	if code, _ := get("/theme.logo"); code != http.StatusNotFound {
		t.Errorf("/theme.logo under dark = %d", code)
	}
	if code, _ := get("/theme.favicon"); code != http.StatusNotFound {
		t.Errorf("/theme.favicon under dark = %d", code)
	}
	if got := themeLogoURL(srv.activeTheme()); got != "/static/logo.png" {
		t.Errorf("logo url = %q, want the bundled one", got)
	}
}

func TestThemeLogoLeavesTheFaviconAlone(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{
		"aurora/theme.css": ":root{--bg:#fff}",
		"aurora/logo.png":  "png-bytes",
	})
	srv.cfg.Server.Theme = "aurora"

	if got := themeLogoURL(srv.activeTheme()); got != "/theme.logo" {
		t.Fatalf("logo url = %q, want the theme's", got)
	}
	if got := themeFaviconURL(srv.activeTheme()); got != "/static/favicon.png" {
		t.Errorf("favicon url = %q, want the bundled icon", got)
	}

	rr := do(srv, "GET", "/")
	body := rr.Body.String()
	wantIn(t, body, `href="/static/favicon.png"`)
	wantIn(t, body, `src="/theme.logo"`)

	rr = do(srv, "GET", "/favicon.ico")
	if loc := rr.Header().Get("Location"); loc != "/static/favicon.png" {
		t.Errorf("/favicon.ico redirected to %q, want the bundled icon", loc)
	}

	wantBundledManifestIcon(t, srv)
}

func TestThemeFaviconTakesTheTab(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{
		"light/theme.css":   ":root{--bg:#fff}",
		"light/logo.png":    "topbar-bytes",
		"light/favicon.png": "tab-bytes",
	})
	srv.cfg.Server.Theme = "light"

	first := themeFaviconURL(srv.activeTheme())
	if !strings.HasPrefix(first, "/theme.favicon?v=") {
		t.Fatalf("favicon url = %q, want the theme's with a version", first)
	}

	rr := do(srv, "GET", "/")
	body := rr.Body.String()
	wantIn(t, body, `href="`+first+`"`)
	wantIn(t, body, `src="/theme.logo"`)

	rr = do(srv, "GET", "/theme.favicon")
	if rr.Code != http.StatusOK || rr.Body.String() != "tab-bytes" {
		t.Errorf("GET /theme.favicon = %d %q, want the theme's own file", rr.Code, rr.Body.String())
	}

	rr = do(srv, "GET", "/favicon.ico")
	if loc := rr.Header().Get("Location"); loc != first {
		t.Errorf("/favicon.ico redirected to %q, want %q", loc, first)
	}

	wantBundledManifestIcon(t, srv)

	installThemes(t, srv, map[string]string{
		"aurora/theme.css":   ":root{--bg:#101820}",
		"aurora/favicon.png": "other-tab-bytes",
	})
	srv.cfg.Server.Theme = "aurora"
	if other := themeFaviconURL(srv.activeTheme()); other == first {
		t.Errorf("aurora and light share the icon url %q", other)
	}
}

func TestThemeSwitchPersistsAndRefuses(t *testing.T) {
	srv := newTestServer(t)
	installThemes(t, srv, map[string]string{"light/theme.css": ":root{--bg:#fff}"})

	post := func(v string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.settingsThemePost(rr, pluginFormRequest("/settings/plugins/theme", "theme="+v))
		return rr
	}
	if rr := post("light"); rr.Header().Get("HX-Refresh") != "true" {
		t.Errorf("switch should repaint the page: %d %q", rr.Code, rr.Body.String())
	}
	if srv.cfg.Server.Theme != "light" {
		t.Errorf("theme = %q", srv.cfg.Server.Theme)
	}
	if rr := post("nope"); rr.Code != http.StatusOK || rr.Header().Get("HX-Retarget") != "#flash-plugins" || rr.Header().Get("HX-Reswap") != "innerHTML" {
		t.Errorf("unknown theme = %d %v, want a 200 flash in the plugins section", rr.Code, rr.Header())
	} else {
		wantIn(t, rr.Body.String(), `<div class="flash flash-err">No theme named nope.</div>`)
	}

	if rr := post("dark"); rr.Code != http.StatusNoContent || srv.cfg.Server.Theme != "dark" {
		t.Errorf("dark should be selectable: %d %q", rr.Code, srv.cfg.Server.Theme)
	}
	if e := srv.activeTheme(); e.Name != "dark" || e.Path != "" {
		t.Errorf("dark resolved to %+v, want the shipped look", e)
	}
}

func TestThemeVarsAreTheSheetsContract(t *testing.T) {
	read := func(path string) string {
		b, err := fs.ReadFile(webFS.FS, path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	decls := func(css string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`(--[a-z0-9-]+)\s*:`).FindAllStringSubmatch(css, -1) {
			out = append(out, m[1])
		}
		return out
	}
	mainCSS := read("static/main.css")
	root := mainCSS[strings.Index(mainCSS, ":root {"):]
	root = root[:strings.Index(root, "}")]
	var base, nav []string
	for _, v := range themeVars {
		if strings.HasPrefix(v, "--nav-") {
			nav = append(nav, v)
		} else {
			base = append(base, v)
		}
	}
	if got := decls(root); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(base))) {
		t.Errorf("main.css :root declares %v, themeVars' base tokens are %v", got, base)
	}
	for _, v := range webui.ThemeVars {
		if !slices.Contains(decls(root), v) {
			t.Errorf("main.css :root leaves the shared token %s undefined", v)
		}
	}
	defined := map[string]bool{}
	for _, v := range append(decls(mainCSS), themeVars...) {
		defined[v] = true
	}
	setters := read("static/main.js")
	_ = fs.WalkDir(webFS.FS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			setters += read(path)
		}
		return nil
	})
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+)['"]?\s*[:,]`).FindAllStringSubmatch(setters, -1) {
		defined[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)`).FindAllStringSubmatch(mainCSS, -1) {
		if !defined[m[1]] && !strings.HasPrefix(m[1], "--cat-") {
			t.Errorf("main.css reads %s, which neither it nor the theme contract defines", m[1])
		}
	}
	for _, v := range nav {
		if !strings.Contains(mainCSS, "var("+v+")") {
			t.Errorf("themeVars lists %s, which main.css never reads", v)
		}
	}

	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	builtin := map[string]bool{}
	for _, m := range regexp.MustCompile(`\('\w+',\s+'#([0-9a-f]{6})', 1\)`).FindAllStringSubmatch(string(schema), -1) {
		builtin["--cat-"+m[1]] = true
	}
	light, err := fs.ReadFile(registry.FS, builtinLightDir+"/theme.css")
	if err != nil {
		t.Fatal(err)
	}
	monbooruOnly := regexp.MustCompile(`:root\[data-app="monloader"\]\s*\{[^}]*\}`).ReplaceAllString(string(light), "")
	for _, v := range decls(monbooruOnly) {
		if !slices.Contains(themeVars, v) && !builtin[v] {
			t.Errorf("the light theme sets %s, which is neither a ThemeVar nor a built-in category colour", v)
		}
	}
}

func TestPagesShowTheActiveThemesDrawingOrNone(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "one.png", 8, 8)
	body := func(path string) string { return do(srv, "GET", path).Body.String() }
	for _, tc := range []struct{ path, face string }{
		{"/no/such/page", "lost"},
		{"/?q=no_such_tag", "lost"},
		{"/?q=inbox:false+no_such_tag", "lost"},
	} {
		if b := body(tc.path); !strings.Contains(b, `src="/static/mascot-`+tc.face+`.png"`) {
			t.Errorf("%s under dark: want the %s drawing", tc.path, tc.face)
		}
	}
	srv.cfg.Server.Theme = "legacy-dark"
	for _, path := range []string{"/no/such/page", "/?q=no_such_tag"} {
		if b := body(path); strings.Contains(b, `class="mascot"`) {
			t.Errorf("%s under legacy-dark shows a drawing", path)
		}
	}
	srv.cfg.Server.Theme = "light"
	if b := body("/no/such/page"); !strings.Contains(b, `src="/static/light/mascot-lost.png"`) {
		t.Error("404 under light: want the dark-haired lost drawing")
	}
}

func TestEmptyGalleryStatesPickTheirFace(t *testing.T) {
	srv := newTestServer(t)
	body := func(path string) string { return do(srv, "GET", path).Body.String() }
	b := body("/")
	wantIn(t, b, `src="/static/mascot-sleep.png"`)
	wantIn(t, b, "empty-gallery-hint")
	b = body("/?q=inbox:true")
	wantIn(t, b, `src="/static/mascot-sleep.png"`)
	wantIn(t, b, "Inbox zero.")
	srv.active().Degraded = true
	b = body("/")
	wantIn(t, b, `src="/static/mascot-panic.png"`)
	if strings.Contains(b, "empty-gallery-hint") {
		t.Error("a degraded gallery offers the sync hint")
	}
}

func TestRunningJobShowsTheSparkleHead(t *testing.T) {
	srv := newTestServer(t)
	mustStartJob(t, srv.jobs, "sync")
	b := do(srv, "GET", "/internal/job/status").Body.String()
	wantIn(t, b, `src="/static/mascot-sparkle-head.png" alt="" class="job-mascot"`)
	wantIn(t, b, `class="job-spin"`)
	srv.jobs.Complete("3 added")
	b = do(srv, "GET", "/internal/job/status").Body.String()
	wantIn(t, b, "✓ 3 added</span>")
	if strings.Contains(b, "job-mascot") {
		t.Error("a finished job still shows the sparkle head")
	}
	srv.cfg.Server.Theme = "legacy-light"
	mustStartJob(t, srv.jobs, "sync")
	defer srv.jobs.Complete("")
	if b := do(srv, "GET", "/internal/job/status").Body.String(); strings.Contains(b, "job-mascot") {
		t.Error("legacy-light shows the sparkle head")
	}
}

func TestFavoriteToggleCarriesTheBurstOnlyWhenTurningOn(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "fav.png", 8, 8)
	on := postForm(t, srv, fmt.Sprintf("/images/%d/favorite", id), url.Values{}).Body.String()
	wantIn(t, on, `class="btn-fav active fav-burst"`)
	wantIn(t, on, `title="Unfavorite (f)"`)
	off := postForm(t, srv, fmt.Sprintf("/images/%d/favorite", id), url.Values{}).Body.String()
	if strings.Contains(off, "fav-burst") {
		t.Errorf("turning a favorite off plays the burst: %q", off)
	}
	wantIn(t, off, `title="Favorite (f)"`)
}
