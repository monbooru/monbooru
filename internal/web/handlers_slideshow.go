package web

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) slideshowItems(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.active()
	if cx == nil || cx.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Slideshow unavailable: gallery path is unreadable.")
		return
	}
	ids, ok := s.resolveBatchScope(w, r, "slideshow", true)
	if !ok {
		return
	}
	types, err := gallery.PresentFileTypes(cx.DB, ids)
	if err != nil {
		logx.Errorf("slideshow: %v", err)
		flashStatus(w, http.StatusInternalServerError, "Search error.")
		return
	}
	// A start that cannot play, or has left the scope, opens on the first
	// playable item after its place, or after its next neighbour's.
	from := -1
	for _, key := range []string{"start", "next"} {
		if id, err := strconv.ParseInt(r.FormValue(key), 10, 64); err == nil {
			if from = slices.Index(ids, id); from >= 0 {
				break
			}
		}
	}
	play, videos, start := []int64{}, []int64{}, 0
	for i, id := range ids {
		if i == from {
			start = len(play)
		}
		ft, ok := types[id]
		if !ok || models.MediaKind(ft) == "archive" {
			continue
		}
		play = append(play, id)
		if gallery.IsVideoType(ft) {
			videos = append(videos, id)
		}
	}
	start = max(min(start, len(play)-1), 0)
	contract.WriteJSON(w, http.StatusOK, map[string]any{"ids": play, "videos": videos, "start": start})
}
