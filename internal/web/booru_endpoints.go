package web

import (
	"net/http"
	"slices"

	"github.com/monbooru/monbooru/internal/api"
)

type booruEndpoint struct {
	Token     string
	Addresses []booruAddress
}

type booruAddress struct {
	Gallery string
	URL     string
}

// One address per gallery for each token that has one, on the host this
// browser reached, as an app on the same network reaches it.
func (s *Server) booruEndpoints(r *http.Request) []booruEndpoint {
	s.cfgMu.RLock()
	tokens := slices.Clone(s.cfg.Auth.Tokens)
	s.cfgMu.RUnlock()
	origin := s.browserOrigin(r)
	galleries := s.galleries()
	var out []booruEndpoint
	for _, t := range tokens {
		if !api.BooruToken(t) {
			continue
		}
		e := booruEndpoint{Token: t.Name}
		key := api.BooruKey(t.TokenHash)
		for _, g := range galleries {
			e.Addresses = append(e.Addresses, booruAddress{Gallery: g.Name, URL: origin + api.BooruPath(g.Name, key)})
		}
		out = append(out, e)
	}
	return out
}

// A token's create, revoke or privileges change moves its addresses too.
func (s *Server) renderBooruEndpointsOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/booru_endpoints.html", map[string]any{
		"Endpoints": s.booruEndpoints(r),
		"OOB":       true,
	})
}
