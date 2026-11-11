package web

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
)

type setupData struct {
	BooruName string
	CSRFToken string
	// The rest of what partials/head.html reads.
	Title        string
	BooruFavicon string
	Theme        bool
	ColorScheme  string
	Mascot       string
	Err          string
	GalleryPath  string
	LAN          bool
	Menu         bool
	Autostart    bool
	Port         string
	BindPinned   string
	Integration  desktop.Integration
}

func (s *Server) setupPending() bool {
	if !s.desktop {
		return false
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return !s.cfg.SetupDone
}

func (s *Server) setupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupPending() || setupExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if isHTMXRequest(r) {
			w.Header().Set("HX-Redirect", "/setup")
			w.WriteHeader(http.StatusOK)
			return
		}
		// An API client would take the wizard's HTML 200 for an answer,
		// so it gets a JSON refusal.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			contract.WriteError(w, http.StatusConflict, contract.CodeSetupRequired, "monbooru has not been set up yet")
			return
		}
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}

func setupExempt(path string) bool {
	switch path {
	case "/health", "/setup", "/internal/browse":
		return true
	}
	if strings.HasPrefix(path, "/theme.mascot/") {
		return true
	}
	// With auth on, the session gate sends /setup to /login, so gating
	// the login here would loop.
	return isOpenPath(path)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.setupPending() {
		s.notFoundHandler(w, r)
		return
	}
	s.cfgMu.RLock()
	galleryPath := ""
	if g := s.cfg.FindGallery(s.cfg.DefaultGallery); g != nil {
		galleryPath = g.GalleryPath
	}
	bind := s.cfg.Server.BindAddress
	s.cfgMu.RUnlock()
	s.renderSetup(w, r, setupData{
		GalleryPath: galleryPath,
		LAN:         !desktop.IsLoopbackAddr(bind),
		Menu:        true,
		Autostart:   true,
	})
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, d setupData) {
	s.cfgMu.RLock()
	bind := s.cfg.Server.BindAddress
	s.cfgMu.RUnlock()
	_, port, err := net.SplitHostPort(bind)
	if err != nil {
		port = contract.PortMonbooru
	}
	d.BooruName = s.booruName()
	d.Title = d.BooruName + " setup"
	d.CSRFToken = s.sessions.CSRFToken(session.FromContext(r.Context()))
	theme := s.activeTheme()
	d.BooruFavicon = themeFaviconURL(theme)
	d.Theme = theme.Path != ""
	d.ColorScheme = themeScheme(theme)
	d.Mascot = themeMascots(theme)["hello"]
	d.Port = port
	d.BindPinned = s.envPinned("server.bind_address")
	d.Integration = s.desktopIntegration()
	s.renderTemplate(w, "setup.html", d)
}

// The gallery goes first, the one input that can fail; SetupDone is saved
// with the address, so a submit that fails midway leaves the wizard up.
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if !s.setupPending() {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	form := setupData{
		GalleryPath: strings.TrimSpace(r.FormValue("gallery_path")),
		LAN:         r.FormValue("reach") == "lan",
		Menu:        r.FormValue("menu_entry") == "on",
		Autostart:   r.FormValue("start_at_login") == "on",
	}
	if err := s.repointGallery(s.defaultGallery(), form.GalleryPath); err != nil {
		form.Err = err.Error()
		s.renderSetup(w, r, form)
		return
	}
	var bind string
	err := s.withConfig(func(c *config.Config) error {
		if c.EnvPinned("server.bind_address") == "" {
			_, port, splitErr := net.SplitHostPort(c.Server.BindAddress)
			if splitErr != nil {
				return fmt.Errorf("server.bind_address %q is not a host:port", c.Server.BindAddress)
			}
			host := "127.0.0.1"
			if form.LAN {
				host = "0.0.0.0"
			}
			c.Server.BindAddress = net.JoinHostPort(host, port)
		}
		bind = c.Server.BindAddress
		c.SetupDone = true
		return nil
	})
	if err != nil {
		form.Err = "Could not save: " + err.Error()
		s.renderSetup(w, r, form)
		return
	}
	for _, f := range s.desktopHook().Apply(form.Menu, form.Autostart) {
		logx.Warnf("setup: could not write %s", f)
	}
	logx.Infof("setup: done, reachable from %s", map[bool]string{true: "the network", false: "this computer"}[!desktop.IsLoopbackAddr(bind)])
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
