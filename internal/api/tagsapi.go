package api

import (
	"cmp"
	"net/http"

	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

type tagResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Color      string `json:"color"`
	UsageCount int    `json:"usage_count"`
	IsAlias    bool   `json:"is_alias"`
	Rejected   bool   `json:"rejected"`
	Origin     string `json:"origin"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

type tagDetailResponse struct {
	tagResponse
	Note         string        `json:"note"`
	Links        []string      `json:"links"`
	DistinctFrom []distinctRef `json:"distinct_from"`
}

type distinctRef struct {
	ID       int64  `json:"id"`
	Category string `json:"category"`
}

func (h *Handler) getTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	t, err := g.TagSvc.GetTag(id)
	if err != nil {
		writeTagError(w, err)
		return
	}
	note, err := g.TagSvc.TagNote(id)
	if serverError(w, err) {
		return
	}
	if note.Links == nil {
		note.Links = []string{}
	}
	partners, err := g.TagSvc.DistinctPartners([]int64{id})
	if serverError(w, err) {
		return
	}
	distinct := []distinctRef{}
	for _, p := range partners[id] {
		distinct = append(distinct, distinctRef{ID: p.ID, Category: p.Category})
	}
	contract.WriteJSON(w, http.StatusOK, tagDetailResponse{tagResponse: toTagResponse(t), Note: note.Body, Links: note.Links, DistinctFrom: distinct})
}

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("q")
	catName := q.Get("category")
	sortStr := cmp.Or(q.Get("sort"), "usage")

	offset, limit := parsePage(r, 100, 500)

	filter := tags.TagFilter{
		Prefix:    prefix,
		Sort:      sortStr,
		Order:     q.Get("order"),
		PageIndex: offset / limit,
		Limit:     limit,
		Origin:    q.Get("origin"),
		UsedBy:    q.Get("used_by"),
		Type:      q.Get("type"),
		ShowZero:  q.Get("show_zero") != "0",
	}

	if filter.Type != "" && filter.Type != "tag" && filter.Type != "alias" && filter.Type != "rejected" {
		writePage(w, offset/limit+1, limit, 0, []tagResponse{})
		return
	}
	if catName != "" {
		catID, ok, err := tags.CategoryIDByName(g.DB, catName)
		if serverError(w, err) {
			return
		}
		if !ok {
			writePage(w, offset/limit+1, limit, 0, []tagResponse{})
			return
		}
		filter.CategoryID = &catID
	}

	tagList, total, err := g.TagSvc.ListTags(filter)
	if serverError(w, err) {
		return
	}

	results := make([]tagResponse, 0, len(tagList))
	for _, t := range tagList {
		results = append(results, toTagResponse(&t))
	}

	writePage(w, offset/limit+1, limit, total, results)
}
