package web

import (
	"cmp"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/pairing"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

type pluginRowView struct {
	Name      string
	Version   string
	Address   string
	Conn      string
	Buttons   []config.PluginButton
	TokenName string
	Scopes    []string
	Paused    bool
	Paired    bool
	// Awaiting is an approval the peer has not collected its token for yet.
	Awaiting     bool
	Companion    bool
	APIURL       string
	WebURL       string
	WebURLHint   string
	APIURLPinned string
	WebURLPinned string
	Command      string
	RunState     string
	Refused      string
	// Installed is a plugin found in the plugins folder: its Enable
	// button is the operator's consent to a launch line they never typed.
	Installed   bool
	Enabled     bool
	SelfManaged bool
}

func (s *Server) pluginRows() []pluginRowView {
	var managed, paired []pluginRowView
	for _, p := range s.effectivePlugins() {
		// Residue, typically of a folder removed after being enabled: the
		// block stays but earns no row.
		if p.PeerToken == "" && !p.Installed && p.APIURL == "" && p.Refused == "" {
			continue
		}
		row := pluginRowView{
			Name:      p.Name,
			Version:   s.peers.Version(p.PluginConfig),
			Address:   s.pluginAddress(p.PluginConfig),
			Buttons:   p.Buttons,
			Paused:    p.Paused,
			Installed: p.Installed,
			Enabled:   p.Enabled,
			Refused:   p.Refused,
		}
		if p.PeerToken != "" {
			row.Conn = peerConn(p.Paused, s.peers.ProbeSeed(p.Name).Conn)
			row.TokenName, row.Scopes = s.pairedTokenInfo(p.Name)
			row.Paired = true
		}
		if p.Installed {
			row.Command = p.CommandLine()
			row.RunState = s.peers.State(p.Name)
			managed = append(managed, row)
			continue
		}
		if p.Refused != "" {
			managed = append(managed, row)
			continue
		}
		row.SelfManaged = true
		paired = append(paired, row)
	}
	byName := func(a, b pluginRowView) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(managed, byName)
	slices.SortFunc(paired, byName)

	// Rendered even unpaired: its api url is an override an operator may
	// set before pairing.
	rows := make([]pluginRowView, 0, len(managed)+len(paired)+1)
	rows = append(rows, s.monloaderRow())
	rows = append(rows, managed...)
	rows = append(rows, paired...)
	for _, req := range s.pairs.ListApproved() {
		for i := range rows {
			if rows[i].Name == req.App {
				rows[i].Awaiting = true
			}
		}
	}
	return rows
}

func (s *Server) monloaderRow() pluginRowView {
	ml := s.ml.Status()
	s.cfgMu.RLock()
	apiURL, webURL := s.cfg.Monloader.APIURL, s.cfg.Server.MonloaderURL
	apiPinned, webPinned := s.cfg.EnvPinned("monloader.api_url"), s.cfg.EnvPinned("server.monloader_url")
	paused := s.cfg.Monloader.Paused
	s.cfgMu.RUnlock()
	name, scopes := s.pairedTokenInfo(monloader.App)
	return pluginRowView{
		Name:         monloader.App,
		Version:      ml.Version,
		Address:      s.ml.Base(),
		Conn:         peerConn(paused, ml.Conn),
		TokenName:    name,
		Scopes:       scopes,
		Paused:       paused,
		Paired:       s.pairedWith(monloader.App),
		Companion:    true,
		APIURL:       apiURL,
		WebURL:       webURL,
		WebURLHint:   "http://localhost:" + contract.PortMonloader,
		APIURLPinned: apiPinned,
		WebURLPinned: webPinned,
	}
}

func peerConn(paused bool, probe string) string {
	if paused {
		return peer.ConnPaused
	}
	return cmp.Or(probe, "checking")
}

func (s *Server) pairedTokenInfo(app string) (string, []string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if t := s.cfg.Auth.Tokens.Paired(app); t != nil {
		return t.Name, t.Scopes
	}
	return "", nil
}

func (s *Server) pairedPeerCount() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	n := 0
	for _, t := range s.cfg.Auth.Tokens {
		if t.Paired != "" {
			n++
		}
	}
	return n
}

// The answers carry the nav notice and the settings count too, so a
// decision clears them at once rather than on the next job-status poll.
func (s *Server) pairViewData() map[string]any {
	n := s.pairNotice()
	n.OOB = true
	return map[string]any{
		"Pending": s.pendingOffers(),
		"Paired":  s.pairedPeerCount(),
		"Notice":  n,
		"PairKey": n.Key(),
	}
}

func (s *Server) pairNotice() webui.PairNotice {
	n := webui.PairNotice{Href: "/settings#plugins"}
	for _, req := range s.pairs.ListPending() {
		n.Items = append(n.Items, webui.PairItem{App: req.App, Source: req.Source, Version: req.Version, Repair: req.Repair, ExpiresAt: req.ExpiresAt()})
	}
	return n
}

type pendingOffer struct {
	peer.Req
	SessionNote bool
}

// A managed plugin is a process on this box, which outranks a session
// already. The offer's name is unauthenticated, so it counts as one only
// from loopback.
func (s *Server) pendingOffers() []pendingOffer {
	pending := s.pairs.ListPending()
	if len(pending) == 0 {
		return nil
	}
	managed := map[string]bool{}
	for _, p := range s.effectivePlugins() {
		managed[p.Name] = p.Installed
	}
	out := make([]pendingOffer, 0, len(pending))
	for _, req := range pending {
		ip := net.ParseIP(req.Source)
		local := managed[req.App] && ip != nil && ip.IsLoopback()
		out = append(out, pendingOffer{Req: req, SessionNote: req.OpenOutranksScopes() && !local})
	}
	return out
}

func (s *Server) pluginPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.pairViewData()
	count, _ := data["Paired"].(int)
	if r.URL.Query().Get("pair") == data["PairKey"] {
		delete(data, "Notice")
	}
	s.renderTemplate(w, "partials/plugin_pairing.html", data)
	if was := r.URL.Query().Get("paired"); was != "" && was != strconv.Itoa(count) {
		s.renderPluginRows(w, r, true)
		s.renderAuthTokensOOB(w, r)
	}
}

func (s *Server) pluginPairDeny(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.pairs.SetState(r.PathValue("id"), pairing.Denied)
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData())
}

func (s *Server) pluginPairRemove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	notifyErr := s.removePeerPairing(r, name)
	s.renderPluginRows(w, r, false)
	s.renderAuthTokensOOB(w, r)
	if notifyErr != nil {
		writeFlashOOB(w, "flash-plugins", "warn", "Removed here, but could not reach "+name+" - remove the pairing there too.")
	}
}

// The error is a peer that could not be told, for the operator to clean up
// there by hand.
func (s *Server) removePeerPairing(r *http.Request, name string) error {
	if name == monloader.App {
		return s.teardownMonloaderPairing(r)
	}
	notifyErr := s.teardownPluginPairing(name)
	logx.Infof("pairing: removed %s pairing from %s", name, session.ClientIP(r))
	if p, ok := s.effective(name); ok && p.Installed {
		if p.Enabled {
			s.setPluginEnabled(name, false)
			s.peers.Retire(s.pairs, name)
		}
		// A folder has no far end to clean up by hand, and the token is
		// revoked either way, so a copy the plugin kept authenticates to
		// nothing.
		return nil
	}
	return notifyErr
}

func (s *Server) pluginPause(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	paused := r.FormValue("paused") == "1"
	if err := s.setPluginPaused(name, paused); err != nil {
		logx.Errorf("plugins: pause %s: %v", name, err)
	}
	logx.Infof("plugins: %s %s from %s", name, map[bool]string{true: "paused", false: "resumed"}[paused], session.ClientIP(r))
	s.renderPluginRows(w, r, false)
}

func (s *Server) pluginStart(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if p, ok := s.effective(r.PathValue("name")); ok && p.Installed {
		s.setPluginEnabled(p.Name, true)
		s.peers.Run(p)
		logx.Infof("plugins: started %s from %s", p.Name, session.ClientIP(r))
	}
	s.renderPluginRows(w, r, false)
}

func (s *Server) pluginStop(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	if p, ok := s.effective(name); ok && p.Installed {
		s.setPluginEnabled(name, false)
	}
	s.peers.Halt(name)
	logx.Infof("plugins: stopped %s from %s", name, session.ClientIP(r))
	s.renderPluginRows(w, r, false)
}

func (s *Server) setPluginEnabled(name string, enabled bool) {
	err := s.withConfig(func(c *config.Config) error {
		plugins.SetEnabled(c, name, enabled)
		return nil
	})
	if err != nil {
		logx.Errorf("plugins: persist enabled=%v for %s: %v", enabled, name, err)
	}
}

func (s *Server) renderPluginRows(w http.ResponseWriter, r *http.Request, oob bool) {
	s.renderTemplate(w, "partials/plugin_rows.html", map[string]any{
		"Rows": s.pluginRows(),
		"OOB":  oob,
	})
}

func (s *Server) setPluginPaused(name string, paused bool) error {
	return s.withConfig(func(c *config.Config) error {
		peer.SetPaused(c, name, paused)
		return nil
	})
}

func (s *Server) teardownPluginPairing(name string) error {
	p, ok := s.plugin(name)
	if !ok {
		if err := s.removePairing(name); err != nil {
			logx.Errorf("pairing: remove %s failed: %v", name, err)
		}
		return nil
	}
	// Read the address before removePairing drops the token it may live on.
	base, token := s.pluginAddress(p), p.PeerToken
	if err := s.removePairing(name); err != nil {
		logx.Errorf("pairing: remove %s failed: %v", name, err)
	}
	return s.peers.Teardown(name, base, token)
}
