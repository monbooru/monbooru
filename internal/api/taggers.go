package api

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tagger"
)

type taggerResponse struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Applies   bool   `json:"applies"`
	Reason    string `json:"reason,omitempty"`
}

func (h *Handler) listTaggers(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	list := tagger.AvailableTaggers(h.cfg())
	out := make([]taggerResponse, 0, len(list))
	for _, t := range list {
		out = append(out, taggerResponse{
			Name:      t.Name,
			Enabled:   t.Enabled,
			Available: t.Available,
			Applies:   t.Enabled && t.Available && t.AppliesToGallery(g.Name),
			Reason:    t.Reason,
		})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}
