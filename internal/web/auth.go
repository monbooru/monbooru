package web

import (
	"net/http"
	"net/url"

	"github.com/monbooru/monbooru/internal/shared/desktop"
)

const sessionCookie = "monbooru_session"

func isOpenPath(path string) bool { return isPublicPath(path) || isStaticPath(path) }

func (s *Server) hostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cfgMu.RLock()
		bind, base := s.cfg.Server.BindAddress, s.cfg.Server.BaseURL
		s.cfgMu.RUnlock()
		if desktop.ForeignHost(r, bind, base) {
			http.Error(w, "monbooru does not answer to this host name: set server.base_url to it (MONBOORU_SERVER_BASE_URL)", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The login page and the wizard load the theme files, browsers fetch the
// manifest without credentials, and the single-instance check and the
// container healthcheck read /health's body.
func isPublicPath(path string) bool {
	switch path {
	case "/login", "/health", "/manifest.json", "/theme.css", "/theme.logo", "/theme.favicon":
		return true
	}
	return false
}

func isStaticPath(path string) bool { return len(path) >= 8 && path[:8] == "/static/" }

func isHTMXRequest(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// sameOriginReferer keeps a form's 303 from following a forged Referer
// off-site or into a javascript: URL.
func sameOriginReferer(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "/"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/"
	}
	return ref
}
