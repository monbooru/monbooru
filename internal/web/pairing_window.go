package web

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
)

const pairWindowLength = 10 * time.Minute

// A plugin is never approved without a click: its open pages run with the
// operator's session, and that consent stays a click.
func companionApp(app string) bool { return app == monloader.App || app == contract.AppMonworker }

type autoPairing struct {
	App, Version, Source string
	Scopes               []string
	At                   time.Time
	seen                 bool
}

// The window and the pairings it made live in memory: a restart closes it
// and forgets the trace.
type pairWindow struct {
	mu     sync.Mutex
	closes time.Time
	made   []autoPairing
}

func (pw *pairWindow) open(d time.Duration) {
	pw.mu.Lock()
	pw.closes = time.Now().Add(d)
	pw.mu.Unlock()
}

func (pw *pairWindow) close() {
	pw.mu.Lock()
	pw.closes = time.Time{}
	pw.mu.Unlock()
}

// closesAt is zero while the window is closed.
func (pw *pairWindow) closesAt() time.Time {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if !pw.closes.After(time.Now()) {
		return time.Time{}
	}
	return pw.closes
}

func (pw *pairWindow) record(req peer.Req) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.made = slices.DeleteFunc(pw.made, func(a autoPairing) bool { return a.App == req.App })
	pw.made = append(pw.made, autoPairing{App: req.App, Version: req.Version, Source: req.Source, Scopes: req.Scopes, At: time.Now()})
}

func (pw *pairWindow) forget(app string) {
	pw.mu.Lock()
	pw.made = slices.DeleteFunc(pw.made, func(a autoPairing) bool { return a.App == app })
	pw.mu.Unlock()
}

func (pw *pairWindow) list() []autoPairing {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return slices.Clone(pw.made)
}

func (pw *pairWindow) unseen() []autoPairing {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	var out []autoPairing
	for _, a := range pw.made {
		if !a.seen {
			out = append(out, a)
		}
	}
	return out
}

func (pw *pairWindow) markSeen() {
	pw.mu.Lock()
	for i := range pw.made {
		pw.made[i].seen = true
	}
	pw.mu.Unlock()
}

// A new install is a database made at this start with nothing paired and
// no password: a config that arrives with one gets no window of its own.
func (s *Server) newInstall() bool {
	for _, cx := range s.set.All() {
		if !cx.DB.Created {
			return false
		}
	}
	return s.pairedPeerCount() == 0 && strings.TrimSpace(s.passwordHash()) == ""
}

func forwardedHop(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// Behind a proxy every source reads as the proxy's, so a forwarded offer
// waits on a card. A re-pair does too: the window adds a monloader, it
// does not replace the one already paired.
func (s *Server) windowApproves(r *http.Request, app string, repair bool) bool {
	if !companionApp(app) || repair || s.win.closesAt().IsZero() || forwardedHop(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

// As the approval does, the peer must answer at the address monbooru will
// call, or it would pair dead.
func (s *Server) autoApproves(r *http.Request, req peer.Req) bool {
	if !s.windowApproves(r, req.App, req.Repair) {
		return false
	}
	base := cmp.Or(s.peerOverrideURL(req.App), peer.CallbackURL(req.URL, req.Source))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if base == "" || !peer.Reachable(ctx, base) {
		logx.Infof("pairing: %s is unreachable at %q; its request waits for a click", req.App, base)
		return false
	}
	return true
}

func (s *Server) pairWindowPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if r.FormValue("open") == "1" {
		s.win.open(pairWindowLength)
		logx.Infof("pairing: window opened for %s from %s", pairWindowLength, session.ClientIP(r))
	} else {
		s.win.close()
		logx.Infof("pairing: window closed from %s", session.ClientIP(r))
	}
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData())
}

// The first minutes of a new install, so the peers started beside it
// pair without a click.
func (s *Server) openWindowOnNewInstall() {
	if s.newInstall() {
		s.win.open(pairWindowLength)
		logx.Infof("pairing: new install, accepting monloader and workers for %s", pairWindowLength)
	}
}

func countdown(until time.Time) string {
	left := max(0, int(time.Until(until).Round(time.Second).Seconds()))
	return fmt.Sprintf("%d:%02d", left/60, left%60)
}

type autoPairNotice struct {
	Href  string
	Items []autoPairing
	OOB   bool
}

func (s *Server) autoPairNotice() autoPairNotice {
	return autoPairNotice{Href: "/settings#plugins", Items: s.win.unseen()}
}

func (n autoPairNotice) Text() string {
	switch len(n.Items) {
	case 0:
		return ""
	case 1:
		return "1 app paired without a click"
	}
	return fmt.Sprintf("%d apps paired without a click", len(n.Items))
}

func (n autoPairNotice) Title() string {
	parts := make([]string, 0, len(n.Items))
	for _, a := range n.Items {
		parts = append(parts, a.App+" from "+a.Source+" at "+a.At.Format("15:04"))
	}
	return strings.Join(parts, "; ")
}

func (n autoPairNotice) Key() string {
	if len(n.Items) == 0 {
		return ""
	}
	return fmt.Sprintf("|%d@%d", len(n.Items), n.Items[len(n.Items)-1].At.UnixNano())
}
