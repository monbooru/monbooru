package web

import (
	"net/http"
	"slices"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
)

type pluginButtonView struct {
	Label string
	Mode  string
	Href  string
	Index int
	Off   bool
	Why   string
}

type pluginGroup struct {
	Peer    string
	Buttons []pluginButtonView
}

// ImageID is 0 on the gallery, where a relay acts on the live selection.
type pluginSlotView struct {
	ImageID int64
	Groups  []pluginGroup
}

func (v pluginSlotView) Any() bool { return len(v.Groups) > 0 }

func (v pluginSlotView) AnyOpen() bool {
	for _, g := range v.Groups {
		if slices.ContainsFunc(g.Buttons, func(b pluginButtonView) bool { return b.Mode == config.ModeOpen }) {
			return true
		}
	}
	return false
}

func (s *Server) pluginSlot(r *http.Request, slot string, imageID int64, fileType string) pluginSlotView {
	back, gallery := s.pageURL(r), s.activeGallery()
	view := pluginSlotView{ImageID: imageID}
	for _, sp := range s.peers.Slot(s.cfgSnapshot(), slot, imageID, fileType, gallery, back, pluginMountBase) {
		g := pluginGroup{Peer: sp.Name}
		for _, b := range sp.Buttons {
			v := pluginButtonView{Label: b.Label, Mode: b.Mode, Href: b.Href, Index: b.Index, Off: sp.Off}
			if sp.Off {
				v.Why = sp.Name + " is " + sp.OffState
			}
			g.Buttons = append(g.Buttons, v)
		}
		view.Groups = append(view.Groups, g)
	}
	return view
}

func (s *Server) pageURL(r *http.Request) string { return s.browserOrigin(r) + r.URL.RequestURI() }

// The host is the one the browser reached: base_url defaults to
// localhost, which sends a LAN browser nowhere.
func (s *Server) browserOrigin(r *http.Request) string {
	s.cfgMu.RLock()
	configured := s.cfg.Server.BaseURL
	s.cfgMu.RUnlock()
	return api.ReachedOrigin(configured, r)
}
