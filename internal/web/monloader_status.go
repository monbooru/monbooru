package web

import (
	"context"
	"net/http"
	"time"

	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

func (s *Server) monloaderStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.pairedWith(monloader.App) {
		s.renderTemplate(w, "webui/peer_light", s.monloaderLight("", "", "", ""))
		return
	}
	if s.ml.Paused() {
		s.renderMonloaderLight(w, r, peer.ConnPaused, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st := s.ml.Refresh(ctx)
	// Panels seeded from stale flags re-mount on this event. Compared with
	// what the page was last told, not with the cache before the probe: the
	// background probe refreshes the cache too.
	seen := s.ml.Status().Panels()
	if r.FormValue("seen") != seen {
		w.Header().Set("HX-Trigger", "monloader-status-changed")
	}
	s.renderTemplate(w, "webui/peer_light", s.monloaderLight(st.Conn, st.Version, st.APIMismatch, seen))
}

func (s *Server) monloaderLight(conn, version, mismatch, seen string) webui.PeerLight {
	paired := s.pairedWith(monloader.App)
	l := webui.PeerLight{
		Peer: monloader.App, Conn: conn, Version: version, Mismatch: mismatch, Seen: seen,
		StatusURL: "/internal/monloader-status",
		PauseURL:  "/internal/monloader/disconnect", ResumeURL: "/internal/monloader/reconnect",
		CheckNow: paired, Paired: paired,
	}
	if base := s.monloaderWebBase(); base != "" {
		l.Href = base + "/queue"
	}
	if conn == peer.ConnPaused || conn == peer.ConnDown || conn == peer.ConnRejected {
		l.OffReason = monloaderOffReason(conn)
	}
	return l
}
