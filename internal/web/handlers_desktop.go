package web

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The bind address is no condition: Restart must survive the wizard's network
// choice. Callers 404 to hide it.
func (s *Server) desktopLocal(r *http.Request) bool {
	return s.desktop && desktop.LocalRequest(r)
}

const maxBrowseEntries = 500

// 404 rather than 403, so the controls do not advertise themselves.
func (s *Server) localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.desktopLocal(r) {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	l, ok := desktop.Browse(r.URL.Query().Get("into"), r.URL.Query().Get("path"), maxBrowseEntries)
	if !ok {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	s.renderTemplate(w, "webui/dir_picker", struct {
		desktop.Listing
		Route string
	}{l, "/internal/browse"})
}

// A kind, never a path, so no operator string reaches the launcher.
var openFolderKinds = []string{"config", "data", "gallery", "logs", "models"}

func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	kind := r.FormValue("kind")
	if !slices.Contains(openFolderKinds, kind) {
		externalErr(w, r, "Unknown folder.", http.StatusBadRequest)
		return
	}
	dir, ours := s.desktopFolder(kind)
	// A tagger name passes the allowlist, so it cannot leave the models folder.
	if kind == "models" && dir != "" {
		name := r.FormValue("name")
		if name != "" {
			if err := tagger.ValidateTaggerName(name); err != nil {
				externalErr(w, r, err.Error(), http.StatusBadRequest)
				return
			}
			dir = filepath.Join(dir, name)
		}
	}
	if dir == "" {
		externalErr(w, r, "That folder is not configured.", http.StatusBadRequest)
		return
	}
	if ours {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			externalErr(w, r, "Could not create "+dir+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := s.folderOpener(dir); err != nil {
		logx.Warnf("open-folder %s: %v", kind, err)
		externalErr(w, r, "Could not open "+dir+".", http.StatusInternalServerError)
		return
	}
	writeInlineFlash(w, "ok", "Opened "+dir+".")
}

// The gallery is not ours to create: an empty one would be a second library.
func (s *Server) desktopFolder(kind string) (string, bool) {
	s.cfgMu.RLock()
	dataPath := s.cfg.Paths.DataPath
	modelPath := s.cfg.Paths.ModelPath
	s.cfgMu.RUnlock()
	switch kind {
	case "config":
		return filepath.Dir(s.configPath), true
	case "data":
		return dataPath, true
	case "logs":
		return s.logDir, true
	case "models":
		return modelPath, true
	case "gallery":
		if cx := s.active(); cx != nil {
			return cx.GalleryPath, false
		}
	}
	return "", false
}

func (s *Server) availableFolders() []string {
	out := make([]string, 0, len(openFolderKinds))
	for _, kind := range openFolderKinds {
		if dir, _ := s.desktopFolder(kind); dir != "" {
			out = append(out, kind)
		}
	}
	return out
}

func (s *Server) DesktopHook() desktop.Hook { return s.desktopHook() }

func (s *Server) desktopHook() desktop.Hook {
	icon, _ := fs.ReadFile(s.staticFS, "appicon.png")
	return desktop.Hook{
		App:        s.desktopApp,
		Name:       s.booruName(),
		Comment:    "Lightweight and fast private booru",
		Categories: "Graphics;2DGraphics;RasterGraphics;Viewer;",
		Icon:       icon,
	}
}

func (s *Server) desktopIntegration() desktop.Integration {
	return s.desktopHook().Integration(s.TrayEnabled())
}

func (s *Server) TrayEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Desktop.Tray
}

func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	failures := s.desktopHook().Apply(r.FormValue("menu_entry") == "on", r.FormValue("start_at_login") == "on")
	if desktop.TrayAvailable() {
		tray := r.FormValue("tray") == "on"
		if err := s.withConfig(func(c *config.Config) error {
			c.Desktop.Tray = tray
			return nil
		}); err != nil {
			failures = append(failures, "tray icon: "+err.Error())
		}
	}
	if len(failures) > 0 {
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
		return
	}
	writeInlineFlash(w, "ok", "Saved.")
}

func (s *Server) desktopJobWarning() string {
	st := s.jobs.Get()
	if st == nil || !st.Running {
		return ""
	}
	return runningJobName(st.JobType) + " is running and will not resume."
}

func (s *Server) desktopPowerFragment(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/desktop_power.html", map[string]any{
		"Warning":   s.desktopJobWarning(),
		"CSRFToken": s.sessions.CSRFToken(session.FromContext(r.Context())),
	})
}

// quitDelay lets the flushed page reach the browser before the listener closes.
const quitDelay = 300 * time.Millisecond

func (s *Server) settingsQuit(w http.ResponseWriter, r *http.Request) {
	s.stopAfterRender(w, r, map[string]any{
		"Heading": s.booruName() + " has stopped",
		"Hint":    "You can close this tab. Start it again from the applications menu.",
	}, "quit", s.RequestQuit)
}

func (s *Server) settingsRestart(w http.ResponseWriter, r *http.Request) {
	s.stopAfterRender(w, r, map[string]any{
		"Heading": s.booruName() + " is restarting",
		"Hint":    "This page will reload automatically.",
		"Poll":    true,
	}, "restart", s.requestRestart)
}

func (s *Server) stopAfterRender(w http.ResponseWriter, r *http.Request, page map[string]any, verb string, then func()) {
	heading, _ := page["Heading"].(string)
	page["App"] = "monbooru"
	s.renderTemplate(w, "webui/message_page", s.standalonePageData(heading, page))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	logx.Infof("%s requested from the UI", verb)
	go func() {
		time.Sleep(quitDelay)
		then()
	}()
}

// Idempotent: the shutdown posts the tray's window close, which quits again.
func (s *Server) RequestQuit() {
	s.quitOnce.Do(func() { close(s.quit) })
}

func (s *Server) requestRestart() {
	s.restart.Store(true)
	s.RequestQuit()
}
