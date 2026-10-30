package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

type categoryResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsBuiltin bool   `json:"is_builtin"`
	Position  int    `json:"position"`
}

func toCategoryResponse(c models.TagCategory) categoryResponse {
	return categoryResponse{ID: c.ID, Name: c.Name, Color: c.Color, IsBuiltin: c.IsBuiltin, Position: c.Position}
}

// ErrCategoryNotFound is a 404 here but a 400 for a tag op, where the
// category is only referenced.
func writeCategoryError(w http.ResponseWriter, err error) {
	if writeSentinelError(w, err, []sentinelStatus{
		{tags.ErrCategoryNotFound, http.StatusNotFound, "not_found"},
		{tags.ErrCategoryExists, http.StatusConflict, "conflict"},
		{tags.ErrBuiltinCategory, http.StatusBadRequest, "invalid_request"},
		{tags.ErrBuiltinCategoryName, http.StatusBadRequest, "invalid_request"},
		{tags.ErrInvalidCategoryName, http.StatusBadRequest, "invalid_request"},
		{tags.ErrInvalidCategoryColor, http.StatusBadRequest, "invalid_request"},
		{tags.ErrReservedCategoryName, http.StatusBadRequest, "invalid_request"},
		{tags.ErrInvalidMoveTarget, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingTagImmutable, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingCategoryClosed, http.StatusBadRequest, "invalid_request"},
	}) {
		return
	}
	var collision *tags.ErrCategoryMoveCollision
	if errors.As(err, &collision) {
		contract.WriteError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	serverError(w, err)
}

func (h *Handler) listCategories(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	cats, err := g.TagSvc.ListCategories()
	if serverError(w, err) {
		return
	}
	out := make([]categoryResponse, 0, len(cats))
	for _, c := range cats {
		out = append(out, toCategoryResponse(c))
	}
	contract.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) createCategory(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	color := cmp.Or(strings.TrimSpace(body.Color), "#888888")
	cat, err := g.TagSvc.CreateCategory(body.Name, color)
	if err != nil {
		writeCategoryError(w, err)
		return
	}
	contract.WriteJSON(w, http.StatusCreated, toCategoryResponse(*cat))
}

// The color is checked before the rename so a bad one can't leave a
// half-applied edit.
func (h *Handler) patchCategory(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name  *string `json:"name"`
		Color *string `json:"color"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if body.Name == nil && body.Color == nil {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "name or color is required")
		return
	}
	if body.Color != nil && !tags.IsValidCategoryColor(strings.TrimSpace(*body.Color)) {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", tags.ErrInvalidCategoryColor.Error())
		return
	}
	if body.Name != nil {
		if err := g.TagSvc.RenameCategory(id, *body.Name); err != nil {
			writeCategoryError(w, err)
			return
		}
	}
	if body.Color != nil {
		if err := g.TagSvc.UpdateCategoryColor(id, strings.TrimSpace(*body.Color)); err != nil {
			writeCategoryError(w, err)
			return
		}
	}
	cat, err := g.TagSvc.GetCategory(id)
	if err != nil {
		writeCategoryError(w, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, toCategoryResponse(cat))
}

func (h *Handler) deleteCategory(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		Action   string `json:"action"`
		TargetID int64  `json:"target_id"`
	}
	// An empty body is the default move to general.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		contract.RefuseJSONBody(w, err, maxJSONBody)
		return
	}
	action := cmp.Or(body.Action, "move")
	if action != "move" && action != "delete_all" {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "action must be 'move' or 'delete_all'")
		return
	}
	if err := g.TagSvc.DeleteCategoryMoveOrDelete(id, action, body.TargetID); err != nil {
		writeCategoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
