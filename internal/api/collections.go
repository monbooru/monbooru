package api

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/gallery"
)

type collectionResponse struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (h *Handler) listCollections(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := q.Get("q")
	offset, limit := parsePage(r, 100, 500)

	total, err := gallery.CountCollections(g.DB, filter, gallery.RankCeiling{})
	if serverError(w, err) {
		return
	}
	list, err := gallery.ListCollections(g.DB, filter, q.Get("sort"), limit, offset, gallery.RankCeiling{})
	if serverError(w, err) {
		return
	}
	results := make([]collectionResponse, 0, len(list))
	for _, c := range list {
		results = append(results, collectionResponse{Name: c.Name, Count: c.Count})
	}
	writePage(w, offset/limit+1, limit, total, results)
}
