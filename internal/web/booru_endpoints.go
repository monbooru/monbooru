package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
)

// The name the endpoint's token shows in API Tokens.
const booruTokenName = "danbooru endpoint"

// What the token table and its dialogs say about the endpoint's token.
const (
	booruManagedBy  = "the Danbooru compatible endpoint"
	booruRevokeNote = "This removes the Danbooru compatible endpoint: its addresses are disabled."
)

// tokenRevokeNotes is what revoking a token of each class also does. The
// shared token table reads it as RevokeNotes; an app without classed
// tokens passes none.
func tokenRevokeNotes() map[string]string { return map[string]string{api.BooruClass: booruRevokeNote} }

// The feature a token's class says manages it, or "" for one the
// operator manages.
func (s *Server) tokenManagedBy(id string) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if t := s.cfg.Auth.Tokens.ByID(id); t != nil && t.Class == api.BooruClass {
		return booruManagedBy
	}
	return ""
}

type booruAddress struct {
	Gallery string
	URL     string
	On      bool
}

type booruEndpointsView struct {
	Exists    bool
	Addresses []booruAddress
	// Why a phone could not reach the addresses, if it could not.
	Unreachable string
}

// One address per gallery for the endpoint's token, on the host this
// browser reached, as an app on the same network reaches it. Each gallery
// shows whether it answers there.
func (s *Server) booruEndpoints(r *http.Request) booruEndpointsView {
	c := s.cfgSnapshot()
	tok := api.BooruToken(c.Auth)
	if tok == nil {
		return booruEndpointsView{}
	}
	v := booruEndpointsView{Exists: true}
	origin, key := s.browserOrigin(r), api.BooruKey(tok.TokenHash)
	for _, g := range c.Galleries {
		v.Addresses = append(v.Addresses, booruAddress{Gallery: g.Name, URL: origin + api.BooruPath(g.Name, key), On: g.Booru})
	}
	switch {
	case desktop.IsLoopbackAddr(c.Server.BindAddress):
		v.Unreachable = "monbooru only answers this computer, so a phone cannot reach these addresses. " +
			"Set server.bind_address to 0.0.0.0 with its port (MONBOORU_SERVER_BIND_ADDRESS) and restart."
	case desktop.IsLoopbackAddr(r.Host):
		v.Unreachable = "These addresses use localhost, which a phone cannot reach. " +
			"Open Settings from this computer's network address to get ones that work."
	}
	return v
}

// A token's create, revoke or privileges change can change what the block
// shows.
func (s *Server) renderBooruEndpointsOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/booru_endpoints.html", map[string]any{
		"View": s.booruEndpoints(r),
		"OOB":  true,
	})
}

// closeBooruEndpointIfGone turns every gallery's address off once the
// endpoint's token is gone, so a new endpoint starts with none shared.
func closeBooruEndpointIfGone(c *config.Config) {
	if api.BooruToken(c.Auth) != nil {
		return
	}
	for i := range c.Galleries {
		c.Galleries[i].Booru = false
	}
}

var errBooruEndpointExists = errors.New("the Danbooru compatible endpoint already exists")

// The endpoint makes a token of its own, locked to read. Its secret is
// never shown: only the key derived from its hash is used.
func (s *Server) settingsBooruEndpointCreate(w http.ResponseWriter, r *http.Request) {
	tok, _ := apitoken.New(booruTokenName, []string{config.ScopeRead})
	tok.Class = api.BooruClass
	err := s.withConfig(func(c *config.Config) error {
		if c.Auth.Tokens.ByClass(api.BooruClass) != nil {
			return errBooruEndpointExists
		}
		return c.Auth.Tokens.Add(tok)
	})
	if err != nil {
		writeInlineFlash(w, "err", "Could not create the endpoint: "+err.Error())
		return
	}
	logx.Infof("settings: Danbooru compatible endpoint created from %s", session.ClientIP(r))
	writeInlineFlash(w, "ok", "Danbooru compatible endpoint created. Turn on the galleries to share.")
	s.renderAuthTokensOOB(w, r)
}

// Removing the endpoint is revoking its token.
func (s *Server) settingsBooruEndpointRemove(w http.ResponseWriter, r *http.Request) {
	var removed bool
	err := s.withConfig(func(c *config.Config) error {
		if tok := c.Auth.Tokens.ByClass(api.BooruClass); tok != nil {
			removed = c.Auth.Tokens.Remove(tok.ID)
		}
		closeBooruEndpointIfGone(c)
		return nil
	})
	if err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !removed {
		writeInlineFlash(w, "err", "There is no Danbooru compatible endpoint.")
		return
	}
	logx.Infof("settings: Danbooru compatible endpoint removed from %s", session.ClientIP(r))
	writeInlineFlash(w, "ok", "Danbooru compatible endpoint removed, its addresses are disabled.")
	s.renderAuthTokensOOB(w, r)
}

var errNoBooruEndpoint = errors.New("create the Danbooru compatible endpoint first")

// An unticked box sends no on, which turns the gallery off.
func (s *Server) settingsBooruGalleryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, on := r.FormValue("gallery"), r.FormValue("on") != ""
	err := s.withConfig(func(c *config.Config) error {
		if api.BooruToken(c.Auth) == nil {
			return errNoBooruEndpoint
		}
		g := c.FindGallery(name)
		if g == nil {
			return fmt.Errorf("unknown gallery %q", name)
		}
		g.Booru = on
		return nil
	})
	if err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: booru address of gallery %s turned %s from %s", name, map[bool]string{true: "on", false: "off"}[on], session.ClientIP(r))
	if on {
		writeInlineFlash(w, "ok", name+" answers at its Danbooru compatible address.")
	} else {
		writeInlineFlash(w, "ok", name+" no longer answers at its Danbooru compatible address.")
	}
	s.renderBooruEndpointsOOB(w, r)
}
