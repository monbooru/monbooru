package web

import (
	"cmp"
	"fmt"
	"math"
	"net/http"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		s.renderTemplate(w, "webui/login_page", s.loginPageData(map[string]any{
			"Error":        "Password authentication is disabled. Enable it from Settings -> Authentication.",
			"AuthDisabled": true,
		}))
		return
	}
	s.renderTemplate(w, "webui/login_page", s.loginPageData(map[string]any{"Next": loginNext(r.URL.Query().Get("next"))}))
}

func loginNext(p string) string {
	if session.LocalPath(p) {
		return p
	}
	return ""
}

func (s *Server) loginPageData(extra map[string]any) map[string]any {
	data := s.standalonePageData("Login - "+s.booruName(), extra)
	data["App"], data["Manifest"] = "monbooru", "/manifest.json"
	return data
}

// Carries every field partials/head.html reads: these pages skip s.base().
func (s *Server) standalonePageData(title string, extra map[string]any) map[string]any {
	theme := s.activeTheme()
	data := map[string]any{
		"Title":        title,
		"CSRFToken":    s.sessions.CSRFToken(session.Anon),
		"BooruName":    s.booruName(),
		"BooruFavicon": themeFaviconURL(theme),
		"Theme":        theme.Path != "",
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	next := loginNext(r.FormValue("next"))
	ip := session.ClientIP(r)
	if wait := s.loginRL.Wait(ip); wait > 0 {
		logx.Warnf("login rate-limited from %s", ip)
		secs := int(math.Ceil(wait.Seconds()))
		s.renderTemplate(w, "webui/login_page", s.loginPageData(map[string]any{
			"Error": fmt.Sprintf("Wait %d %s before trying again.", secs, models.Plural(secs, "second", "seconds")),
			"Next":  next,
		}))
		return
	}

	password := r.FormValue("password")
	if err := bcrypt.CompareHashAndPassword(
		[]byte(s.passwordHash()), []byte(password),
	); err != nil {
		s.loginRL.Fail(ip)
		logx.Warnf("login failed from %s", ip)
		s.renderTemplate(w, "webui/login_page", s.loginPageData(map[string]any{
			"Error": "Invalid password",
			"Next":  next,
		}))
		return
	}
	s.loginRL.Succeed(ip)
	logx.Infof("login success from %s", ip)

	s.setSessionCookie(w, r, s.sessions.Create(s.sessionLifetimeDays()))
	http.Redirect(w, r, cmp.Or(next, "/"), http.StatusSeeOther)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sessID string) {
	session.SetCookie(w, r, sessionCookie, sessID, s.sessionLifetimeDays()*86400)
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	sessID := session.FromContext(r.Context())
	s.sessions.Delete(sessID)
	session.SetCookie(w, r, sessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "webui/auth_password_section", map[string]any{
		"AuthEnabled": s.authEnabled(),
		"HasPassword": s.passwordHash() != "",
		"OOB":         true,
	})
}
