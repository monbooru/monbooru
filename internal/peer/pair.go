// Package peer is what monloader and the plugins share: the pending pairing
// requests, the grant and revoke written to the config, the address rules,
// the /health probe and the teardown notice.
package peer

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/pairing"
)

type Req struct {
	pairing.Request
	URL       string
	PeerToken string
	Version   string
	Buttons   []config.PluginButton
	Repair    bool
}

// An open button's page runs on monbooru's origin with the operator's
// session, so without write it grants more than the scopes say.
func (r Req) OpenOutranksScopes() bool {
	return !slices.Contains(r.Scopes, config.ScopeWrite) &&
		slices.ContainsFunc(r.Buttons, func(b config.PluginButton) bool { return b.Mode == config.ModeOpen })
}

type Store = pairing.Store[Req, *Req]

func NewStore() *Store { return pairing.NewStore[Req]() }

func ValidateOffer(app, version string, buttons []config.PluginButton) error {
	if err := config.ValidatePluginName(app); err != nil {
		return err
	}
	if utf8.RuneCountInString(version) > config.MaxPluginVersion {
		return fmt.Errorf("version must be at most %d characters", config.MaxPluginVersion)
	}
	return config.ValidatePluginButtons(buttons)
}

// A peer advertises its own base_url, whose host (usually localhost) means
// nothing from here; the request's source is where it can be reached.
func CallbackURL(advertised, source string) string {
	source = strings.TrimSpace(source)
	u, err := url.Parse(strings.TrimSpace(advertised))
	if err != nil || u.Host == "" || source == "" {
		return advertised
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(source, port)
	} else if strings.Contains(source, ":") {
		u.Host = "[" + source + "]"
	} else {
		u.Host = source
	}
	return u.String()
}

func Repoint(c *config.Config, app, url string) {
	if t := c.Auth.Tokens.Paired(app); t != nil {
		t.PeerURL = url
	}
}

// The request stores the grant, so the approval card shows what the
// token will carry.
func GrantedScopes(requested []string) []string {
	if scopes := apitoken.Filter(requested, config.AllScopes); len(scopes) > 0 {
		return scopes
	}
	return []string{config.ScopeRead, config.ScopeWrite}
}

// Grant stores tok, minted for req, and the peer's own credential.
func Grant(c *config.Config, req Req, tok apitoken.Token) {
	// A re-pair replaces the credentials: the copy that held the old
	// token is gone.
	c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t apitoken.Token) bool { return t.Paired == req.App })
	c.Auth.Tokens = append(c.Auth.Tokens, tok)
	if req.App == contract.AppMonloader {
		if c.EnvPinned("monloader.api_token") == "" {
			c.Monloader.APIToken = req.PeerToken
		}
		c.Monloader.Paused = false
		return
	}
	p := c.FindPlugin(req.App)
	if p == nil {
		c.Plugins = append(c.Plugins, config.PluginConfig{Name: req.App})
		p = &c.Plugins[len(c.Plugins)-1]
	}
	p.Version, p.PeerToken, p.Buttons, p.Paused = req.Version, req.PeerToken, req.Buttons, false
}

func SetPaused(c *config.Config, app string, paused bool) {
	if app == contract.AppMonloader {
		c.Monloader.Paused = paused
		return
	}
	if p := c.FindPlugin(app); p != nil {
		p.Paused = paused
	}
}

// Keeps the configured api_url so an operator's URL survives an unpair
// and re-pair.
func Revoke(c *config.Config, app string) {
	c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t apitoken.Token) bool { return t.Paired == app })
	if app == contract.AppMonloader {
		if c.EnvPinned("monloader.api_token") == "" {
			c.Monloader.APIToken = ""
		}
		return
	}
	if p := c.FindPlugin(app); p != nil {
		p.PeerToken, p.Version, p.Buttons = "", "", nil
	}
	// A block with the operator's own lines stays, since config.Save
	// re-encodes the whole file from the struct. Enabled counts: dropping
	// it would leave a running plugin whose row offers to enable it.
	c.Plugins = slices.DeleteFunc(c.Plugins, func(p config.PluginConfig) bool {
		return p.Name == app && p.APIURL == "" && !p.Enabled
	})
}

func OverrideURL(c *config.Config, app string) string {
	if app == contract.AppMonloader {
		return strings.TrimSpace(c.Monloader.APIURL)
	}
	if p := c.FindPlugin(app); p != nil {
		return strings.TrimSpace(p.APIURL)
	}
	return ""
}
