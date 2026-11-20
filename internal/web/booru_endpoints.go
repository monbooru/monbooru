package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
)

type booruTokenOption struct {
	ID, Name string
	Chosen   bool
}

type booruAddress struct {
	Gallery string
	URL     string
	On      bool
}

type booruEndpointsView struct {
	Options   []booruTokenOption
	Addresses []booruAddress
	// Why a phone could not reach the addresses, if it could not.
	Unreachable string
}

// The tokens the layer can use, and one address per gallery for the one
// picked, on the host this browser reached, as an app on the same network
// reaches it. Each gallery shows whether it answers there.
func (s *Server) booruEndpoints(r *http.Request) booruEndpointsView {
	s.cfgMu.RLock()
	auth, bind := s.cfg.Auth, s.cfg.Server.BindAddress
	auth.Tokens = slices.Clone(auth.Tokens)
	s.cfgMu.RUnlock()
	chosen := api.ChosenBooruToken(auth)
	var v booruEndpointsView
	for _, t := range auth.Tokens {
		if api.BooruToken(t) {
			v.Options = append(v.Options, booruTokenOption{ID: t.ID, Name: t.Name, Chosen: chosen != nil && chosen.ID == t.ID})
		}
	}
	if chosen == nil {
		return v
	}
	origin, key := s.browserOrigin(r), api.BooruKey(chosen.TokenHash)
	for _, g := range s.galleries() {
		v.Addresses = append(v.Addresses, booruAddress{Gallery: g.Name, URL: origin + api.BooruPath(g.Name, key), On: g.Booru})
	}
	switch {
	case desktop.IsLoopbackAddr(bind):
		v.Unreachable = "monbooru only answers this computer, so a phone cannot reach these addresses. " +
			"Set server.bind_address to 0.0.0.0 with its port (MONBOORU_SERVER_BIND_ADDRESS) and restart."
	case desktop.IsLoopbackAddr(r.Host):
		v.Unreachable = "These addresses use localhost, which a phone cannot reach. " +
			"Open Settings from this computer's network address to get ones that work."
	}
	return v
}

// A token's create, revoke or privileges change can change what the block
// offers.
func (s *Server) renderBooruEndpointsOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/booru_endpoints.html", map[string]any{
		"View": s.booruEndpoints(r),
		"OOB":  true,
	})
}

// A pick its token no longer backs is dropped, so giving read back does
// not reopen the layer unasked.
func dropStaleBooruPick(auth *config.AuthConfig) {
	if api.ChosenBooruToken(*auth) == nil {
		auth.BooruTokenID = ""
	}
}

var errBooruTokenUnfit = errors.New("that token needs the read privilege and must not belong to a pairing")

// An empty token_id closes the layer. Like the token forms, the answer is
// a flash with the block swapped in beside it.
func (s *Server) settingsBooruTokenPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := strings.TrimSpace(r.FormValue("token_id"))
	err := s.withConfig(func(c *config.Config) error {
		if id != "" {
			if t := c.Auth.Tokens.ByID(id); t == nil || !api.BooruToken(*t) {
				return errBooruTokenUnfit
			}
		}
		c.Auth.BooruTokenID = id
		return nil
	})
	if err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: booru token set to %q from %s", id, session.ClientIP(r))
	if id == "" {
		writeInlineFlash(w, "ok", "Danbooru compatible endpoint turned off.")
	} else {
		writeInlineFlash(w, "ok", "Danbooru compatible endpoint token saved.")
	}
	s.renderBooruEndpointsOOB(w, r)
}

// An unticked box sends no on, which turns the gallery off.
func (s *Server) settingsBooruGalleryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, on := r.FormValue("gallery"), r.FormValue("on") != ""
	err := s.withConfig(func(c *config.Config) error {
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
