package web

import (
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/registry"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/webui"
	webFS "github.com/monbooru/monbooru/web"
)

const builtinLightDir = registry.LightThemeDir

// themeVars are the custom properties a theme sets: the stable surface
// the themes registry builds on, beside one --cat-<hex> per built-in
// category colour. Rules and every other variable may change in any
// release. The nav hues have no default in main.css.
var themeVars = append(slices.Clone(webui.ThemeVars),
	"--monloader", "--tagger", "--plugin", "--fav", "--remove", "--alternate", "--media-bg",
	"--nav-images", "--nav-inbox", "--nav-categories", "--nav-tags",
	"--nav-collections", "--nav-relations", "--nav-settings",
)

var builtinThemes = []webui.Theme{
	{Name: "dark", Builtin: true},
	{Name: "light", Builtin: true, Path: builtinLightDir + "/theme.css"},
	{
		Name: "legacy-dark", Builtin: true,
		Path:    registry.LegacyDarkThemeDir + "/theme.css",
		Logo:    registry.LegacyDarkThemeDir + "/logo.png",
		Favicon: registry.LegacyDarkThemeDir + "/favicon.png",
	},
	{
		Name: "legacy-light", Builtin: true,
		Path:    registry.LegacyLightThemeDir + "/theme.css",
		Logo:    registry.LegacyLightThemeDir + "/logo.png",
		Favicon: registry.LegacyLightThemeDir + "/favicon.png",
	},
}

var builtinArt = map[string]string{"dark": "/static/", "light": "/static/light/"}

var ownScheme = func() string {
	sheet, _ := fs.ReadFile(webFS.FS, "static/main.css")
	return webui.SheetScheme(sheet, "")
}()

func themeScheme(e webui.Theme) string { return webui.ThemeScheme(e, registry.FS, ownScheme) }

var mascotFaces = []string{"hello", "lost", "sleep", "panic", "sparkle-head"}

func themeMascots(e webui.Theme) map[string]string {
	out := map[string]string{}
	for _, face := range mascotFaces {
		switch {
		case e.Mascots[face] != "":
			out[face] = "/theme.mascot/" + face
		case builtinArt[e.Name] != "":
			out[face] = builtinArt[e.Name] + "mascot-" + face + ".png"
		}
	}
	return out
}

func (s *Server) themesDir() string { return s.configSubdir("themes") }

// A deleted copy is not rebuilt.
func (s *Server) seedThemesDir() {
	logNotes(webui.SeedTheme(registry.FS, builtinLightDir, []string{"theme.css"}, s.themesDir(), "light"))
}

func logNotes(notes []webui.Note) {
	for _, n := range notes {
		if n.Warn {
			logx.Warnf("%s", n.Text)
		} else {
			logx.Infof("%s", n.Text)
		}
	}
}

func (s *Server) listThemes() []webui.Theme {
	themes, notes := webui.ListThemes(builtinThemes, s.themesDir())
	s.themeWarn.notesOnce(notes)
	return themes
}

// server.theme is only matched against the listing, never used as a path.
func (s *Server) activeTheme() webui.Theme {
	if s.themeName() == "" {
		return builtinThemes[0]
	}
	return s.activeThemeIn(s.listThemes())
}

func (s *Server) activeThemeIn(entries []webui.Theme) webui.Theme {
	name := s.themeName()
	e, found := webui.FindTheme(entries, name)
	if !found {
		s.themeWarn.warnOnce(name)
	}
	return e
}

func (s *Server) themeName() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return strings.TrimSpace(s.cfg.Server.Theme)
}

type themeWarnings struct {
	mu        sync.Mutex
	last      string
	lastNotes string
}

// Every page render lists the themes, so a listing's notes are logged
// only when they differ from the last listing's.
func (t *themeWarnings) notesOnce(notes []webui.Note) {
	var key strings.Builder
	for _, n := range notes {
		key.WriteString(n.Text + "\n")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if key.String() == t.lastNotes {
		return
	}
	t.lastNotes = key.String()
	logNotes(notes)
}

func (t *themeWarnings) warnOnce(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == name {
		return
	}
	t.last = name
	logx.Warnf("server.theme %q is not an installed or built-in theme; the shipped look is used", name)
}

func (s *Server) serveThemeCSS(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Path, "theme")
}

func (s *Server) serveThemeLogo(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Logo, "themelogo")
}

func (s *Server) serveThemeFavicon(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Favicon, "themefavicon")
}

func (s *Server) serveThemeMascot(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	face := r.PathValue("face")
	s.serveThemeFile(w, r, e, e.Mascots[face], "thememascot-"+face)
}

func (s *Server) serveThemeFile(w http.ResponseWriter, r *http.Request, e webui.Theme, path, kind string) {
	if path != "" && !e.Builtin {
		// An archive gives every theme it unpacks one mtime.
		setGalleryScopedCache(w, "custom", kind+"-"+url.PathEscape(e.Name), path)
	}
	webui.ServeThemeFile(w, r, e, path, registry.FS)
}

type themeCluster struct {
	Names  []string
	Active string
}

func (s *Server) themeCluster() themeCluster {
	entries := s.listThemes()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return themeCluster{Names: names, Active: s.activeThemeIn(entries).Name}
}

func (s *Server) settingsThemePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("theme"))
	if name != "" && !slices.ContainsFunc(s.listThemes(), func(e webui.Theme) bool { return e.Name == name }) {
		themeRefused(w, "No theme named "+name+".")
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Server.Theme = name
		return nil
	}); err != nil {
		themeRefused(w, "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: theme set to %q", name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// The cluster's buttons swap nothing, and the page's first action-flash slot
// is another section's.
func themeRefused(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#flash-plugins")
	w.Header().Set("HX-Reswap", "innerHTML")
	writeInlineFlash(w, "err", msg)
}
