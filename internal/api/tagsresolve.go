package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

const maxResolveNames = 500

type resolvedTag struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	ResolvesTo string   `json:"resolves_to,omitempty"`
	Implies    []string `json:"implies"`
	Reason     string   `json:"reason,omitempty"`
}

// resolveTags answers what the gallery would do with each name on arrival,
// without creating anything.
func (h *Handler) resolveTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Names []string `json:"names"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if len(body.Names) == 0 || len(body.Names) > maxResolveNames {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "names takes 1 to 500 entries")
		return
	}
	results := make([]resolvedTag, 0, len(body.Names))
	implied := make([][]int64, len(body.Names))
	var impliedIDs []int64
	for i, raw := range body.Names {
		res := resolvedTag{Name: raw, Implies: []string{}}
		catID, bare, err := gallery.ResolveCategoryTag(g.DB, raw)
		if err != nil && !errors.Is(err, tags.ErrEmptyFormName) {
			serverError(w, err)
			return
		}
		name, verr := bare, err
		if verr == nil {
			name, verr = g.TagSvc.ValidateTagIn(bare, catID)
		}
		if verr != nil {
			res.Status, res.Reason = "refused", verr.Error()
			results = append(results, res)
			continue
		}
		tag, err := g.TagSvc.FindTag(name, catID)
		switch {
		case errors.Is(err, tags.ErrTagNotFound):
			res.Status = "new"
		case err != nil:
			serverError(w, err)
			return
		case tag.Rejected:
			res.Status = "rejected"
		case tag.Name != name || tag.CategoryID != catID:
			res.Status, res.ResolvesTo = "alias", qualifiedName(tag.CategoryName, tag.Name)
		default:
			res.Status = "exists"
		}
		if err == nil && !tag.Rejected {
			closure, err := g.TagSvc.ImplicationClosure(tag.ID)
			if serverError(w, err) {
				return
			}
			implied[i] = closure[1:]
			impliedIDs = append(impliedIDs, closure[1:]...)
		}
		results = append(results, res)
	}
	byID, err := g.TagSvc.GetTags(impliedIDs)
	if serverError(w, err) {
		return
	}
	for i, ids := range implied {
		names := make([]*models.Tag, 0, len(ids))
		for _, id := range ids {
			if t := byID[id]; t != nil {
				names = append(names, t)
			}
		}
		slices.SortFunc(names, func(a, b *models.Tag) int { return cmp.Compare(a.Name, b.Name) })
		for _, t := range names {
			results[i].Implies = append(results[i].Implies, qualifiedName(t.CategoryName, t.Name))
		}
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

func qualifiedName(category, name string) string {
	if category == "general" || strings.TrimSpace(category) == "" {
		return name
	}
	return category + ":" + name
}
