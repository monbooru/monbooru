package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

// t must carry its category name and color, which GetTag fills and
// GetOrCreateTag does not.
func toTagResponse(t *models.Tag) tagResponse {
	resp := tagResponse{
		ID:         t.ID,
		Name:       t.Name,
		Category:   t.CategoryName,
		Color:      t.CategoryColor,
		UsageCount: t.UsageCount,
		IsAlias:    t.IsAlias,
		Rejected:   t.Rejected,
		Origin:     t.Origin,
	}
	if !t.LastUsedAt.IsZero() {
		resp.LastUsedAt = t.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

func resolveCategoryID(w http.ResponseWriter, g *library.Gallery, name string) (int64, bool) {
	id, found, err := tags.CategoryIDByName(g.DB, cmp.Or(strings.TrimSpace(name), "general"))
	if serverError(w, err) {
		return 0, false
	}
	if !found {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+name)
		return 0, false
	}
	return id, true
}

func splitTypedName(w http.ResponseWriter, g *library.Gallery, name string, catID int64) (string, int64, bool) {
	f, err := tags.SplitForm(g.DB, name)
	switch {
	case serverError(w, err):
		return "", 0, false
	case f.CategoryID != 0 && strings.TrimSpace(f.Name) == "":
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "name is empty after its category prefix")
		return "", 0, false
	case f.CategoryID != 0:
		return f.Name, f.CategoryID, true
	}
	return f.Name, catID, true
}

type sentinelStatus struct {
	err    error
	status int
	code   string
}

func writeSentinelError(w http.ResponseWriter, err error, table []sentinelStatus) bool {
	for _, e := range table {
		if errors.Is(err, e.err) {
			contract.WriteError(w, e.status, e.code, err.Error())
			return true
		}
	}
	return false
}

func writeTagError(w http.ResponseWriter, err error) {
	var coll *tags.ErrCategoryCollision
	if errors.As(err, &coll) {
		contract.WriteError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if !writeSentinelError(w, err, []sentinelStatus{
		{tags.ErrTagNotFound, http.StatusNotFound, "not_found"},
		{tags.ErrImplicationNotFound, http.StatusNotFound, "not_found"},
		{tags.ErrCategoryNotFound, http.StatusBadRequest, "invalid_request"},
		{tags.ErrTagNameTaken, http.StatusConflict, "conflict"},
		{tags.ErrTagRejected, http.StatusConflict, "rejected"},
		{tags.ErrAliasToRejected, http.StatusConflict, "rejected"},
		{tags.ErrAliasNameInUse, http.StatusConflict, "conflict"},
		{tags.ErrImplicationCycle, http.StatusConflict, "conflict"},
		{tags.ErrInvalidTagName, http.StatusBadRequest, "invalid_request"},
		{tags.ErrNonCanonicalRating, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingTagImmutable, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingCategoryClosed, http.StatusBadRequest, "invalid_request"},
		{tags.ErrAliasToAlias, http.StatusBadRequest, "invalid_request"},
		{tags.ErrAliasToItself, http.StatusBadRequest, "invalid_request"},
		{tags.ErrMergeIntoSelf, http.StatusBadRequest, "invalid_request"},
		{tags.ErrMergeIntoAlias, http.StatusBadRequest, "invalid_request"},
		{tags.ErrImplicationSelf, http.StatusBadRequest, "invalid_request"},
		{tags.ErrImplicationAlias, http.StatusBadRequest, "invalid_request"},
	}) {
		serverError(w, err)
	}
}

func (h *Handler) createTag(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	catID, ok := resolveCategoryID(w, g, body.Category)
	if !ok {
		return
	}
	name, catID, ok := splitTypedName(w, g, body.Name, catID)
	if !ok {
		return
	}
	// Refused before the create: a client creating a name is not an arrival
	// on an image, so it must not count as a dropped one.
	if found, err := g.TagSvc.FindTag(name, catID); err == nil && found.Rejected {
		writeTagError(w, tags.ErrTagRejected)
		return
	}
	tag, err := g.TagSvc.GetOrCreateTagFrom(name, catID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	full, err := g.TagSvc.GetTag(tag.ID)
	if serverError(w, err) {
		return
	}
	contract.WriteJSON(w, http.StatusCreated, toTagResponse(full))
}

func (h *Handler) patchTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Category *string `json:"category"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if body.Name == nil && body.Category == nil {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "name or category is required")
		return
	}
	var catID int64
	if body.Category != nil {
		if catID, ok = resolveCategoryID(w, g, *body.Category); !ok {
			return
		}
	}
	var name string
	if body.Name != nil {
		if name, catID, ok = splitTypedName(w, g, *body.Name, catID); !ok {
			return
		}
	}
	var err error
	switch {
	case body.Name != nil && catID != 0:
		err = g.TagSvc.RenameAndMoveTag(id, name, catID)
	case body.Name != nil:
		err = g.TagSvc.RenameTag(id, name)
	default:
		err = g.TagSvc.ChangeTagCategory(id, catID)
	}
	if err != nil {
		writeTagError(w, err)
		return
	}
	full, err := g.TagSvc.GetTag(id)
	if err != nil {
		writeTagError(w, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, toTagResponse(full))
}

func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if err := g.TagSvc.DeleteTag(id); err != nil {
		writeTagError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createAlias(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Category    string `json:"category"`
		CanonicalID int64  `json:"canonical_id"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	if body.CanonicalID == 0 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "canonical_id is required")
		return
	}
	catID, ok := resolveCategoryID(w, g, body.Category)
	if !ok {
		return
	}
	name, catID, ok := splitTypedName(w, g, body.Name, catID)
	if !ok {
		return
	}
	alias, err := g.TagSvc.CreateAliasFrom(name, catID, body.CanonicalID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	contract.WriteJSON(w, http.StatusCreated, toTagResponse(alias))
}

func (h *Handler) mergeTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		AliasID     int64 `json:"alias_id"`
		CanonicalID int64 `json:"canonical_id"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if body.AliasID == 0 || body.CanonicalID == 0 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "alias_id and canonical_id are required")
		return
	}
	if err := g.TagSvc.MergeTags(body.AliasID, body.CanonicalID); err != nil {
		writeTagError(w, err)
		return
	}
	canon, err := g.TagSvc.GetTag(body.CanonicalID)
	if err != nil {
		writeTagError(w, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, toTagResponse(canon))
}

type implicationJSON struct {
	ParentID        int64  `json:"parent_id"`
	ImpliedID       int64  `json:"implied_id"`
	ImpliedName     string `json:"implied_name"`
	ImpliedCategory string `json:"implied_category"`
}

func (h *Handler) listImplications(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if _, err := g.TagSvc.GetTag(id); err != nil {
		writeTagError(w, err)
		return
	}
	imps, err := g.TagSvc.ListImplications(id)
	if serverError(w, err) {
		return
	}
	out := make([]implicationJSON, 0, len(imps))
	for _, im := range imps {
		out = append(out, implicationJSON{
			ParentID:        im.ParentID,
			ImpliedID:       im.ImpliedID,
			ImpliedName:     im.ImpliedName,
			ImpliedCategory: im.ImpliedCategoryName,
		})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) addImplication(w http.ResponseWriter, r *http.Request) {
	g, parentID, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		ImpliedID int64 `json:"implied_id"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if body.ImpliedID == 0 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "implied_id is required")
		return
	}
	propagate := r.URL.Query().Get("propagate") == "true"
	var res *jobs.Reservation
	if propagate {
		if res = h.reserveImplicationJob(w); res == nil {
			return
		}
		defer res.Release()
	}
	isNew, err := g.TagSvc.AddImplicationFrom(parentID, body.ImpliedID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	if propagate {
		if !startImplicationJob(w, res, g) {
			return
		}
		go h.propagateImplication(g, parentID, body.ImpliedID, false)
		contract.WriteJSON(w, http.StatusAccepted, map[string]any{"created": isNew, "job": "implication job started"})
		return
	}
	if isNew {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) removeImplication(w http.ResponseWriter, r *http.Request) {
	g, parentID, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	impliedID, ok := contract.PathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	propagate := r.URL.Query().Get("propagate") == "true"
	var res *jobs.Reservation
	if propagate {
		if res = h.reserveImplicationJob(w); res == nil {
			return
		}
		defer res.Release()
	}
	if err := g.TagSvc.RemoveImplication(parentID, impliedID); err != nil {
		writeTagError(w, err)
		return
	}
	if propagate {
		if !startImplicationJob(w, res, g) {
			return
		}
		go h.propagateImplication(g, parentID, impliedID, true)
		contract.WriteJSON(w, http.StatusAccepted, map[string]any{"job": "implication job started"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The job lane is held before the edge is written, so no edge is stored
// whose fan-out cannot run, and a refused edge leaves the status bar as it was.
func (h *Handler) reserveImplicationJob(w http.ResponseWriter) *jobs.Reservation {
	res, err := h.jobs.Reserve()
	if err != nil {
		contract.WriteError(w, http.StatusConflict, "conflict", "a job is already running")
		return nil
	}
	return res
}

func startImplicationJob(w http.ResponseWriter, res *jobs.Reservation, g *library.Gallery) bool {
	if err := res.Start(models.JobTypeTag, g.Name); err != nil {
		contract.WriteError(w, http.StatusConflict, "conflict", "a job is already running")
		return false
	}
	return true
}

func (h *Handler) propagateImplication(g *library.Gallery, parentID, impliedID int64, remove bool) {
	ctx := h.jobs.Context()
	g.WaitIngests(ctx)
	verb, done := "applying implication", "Implication applied to %d %s."
	if remove {
		verb, done = "removing implication", "Implication removed from %d %s."
	}
	progress := func(n, total int) { h.jobs.Update(n, total, verb+"…") }
	var p tags.Propagation
	var err error
	if remove {
		var closure []int64
		if closure, err = g.TagSvc.ImplicationClosure(impliedID); err == nil {
			p, err = g.TagSvc.SweepImplicationRemoval(ctx, parentID, closure, progress)
		}
	} else {
		p, err = g.TagSvc.PropagateImplication(ctx, parentID, progress)
	}
	n := len(p.Walked)
	h.jobs.Finish(err, p.Cancelled,
		fmt.Sprintf("%s cancelled (%d/%d)", verb, n, p.Total),
		fmt.Sprintf(done, n, models.Plural(n, "image", "images")))
}
