package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func (s *Server) implicationsDialogHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags/%d", id), http.StatusSeeOther)
		return
	}
	parent, err := s.tagSvc().GetTag(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	imps, err := s.tagSvc().ListImplications(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels := make([]string, 0, len(imps))
	for _, im := range imps {
		labels = append(labels, im.Origin)
	}
	data := map[string]any{
		"Parent":       parent,
		"Implications": imps,
		"OriginKinds":  s.originKinds(labels),
	}
	s.renderTemplate(w, "partials/implications_dialog.html", data)
}

// A nil res means the response is written; otherwise the caller holds the
// job lane for the fan-out and hands it to propagateDeclared. The lane is
// held before the first write, so no edge is stored whose fan-out cannot run.
func (s *Server) declareImplications(w http.ResponseWriter, r *http.Request, field string, edge func(tagID int64) (parent, implied int64)) (added int, edges []models.Implication, failures []string, res *jobs.Reservation) {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		writeInlineFlash(w, "err", "Tag name is required.")
		return 0, nil, nil, nil
	}
	tokens, err := splitTagTokens(raw)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return 0, nil, nil, nil
	}
	if res = s.reserveJob(w); res == nil {
		return 0, nil, nil, nil
	}
	for _, token := range tokens {
		ref, msg := s.resolveCanonicalTagInput(token, true)
		if msg != "" {
			failures = append(failures, msg)
			continue
		}
		if ref.id == 0 {
			tag, err := s.tagSvc().GetOrCreateTag(ref.name, ref.catID)
			if err != nil {
				failures = append(failures, ref.name+": "+err.Error())
				continue
			}
			ref.id = tag.ID
		}
		parent, implied := edge(ref.id)
		isNew, err := s.tagSvc().AddImplication(parent, implied)
		if err != nil {
			failures = append(failures, token+": "+err.Error())
			continue
		}
		if isNew {
			added++
		}
		// Re-declaring an edge re-applies it, repairing images that lack what it implies.
		edges = append(edges, models.Implication{ParentID: parent, ImpliedID: implied})
	}
	return added, edges, failures, res
}

func (s *Server) propagateDeclared(res *jobs.Reservation, edges []models.Implication, op string) {
	defer res.Release()
	if len(edges) == 0 || !s.startReserved(res, models.JobTypeTag) {
		return
	}
	s.goJobWork(models.JobTypeTag, func(context.Context) { s.runImplicationEdges(edges, op) })
}

func implicationsAddedMsg(added int) string {
	return fmt.Sprintf("%d implication%s added.", added, plural(added))
}

func writeImplicationFailures(w http.ResponseWriter, added, declared int, failures []string) bool {
	switch {
	case len(failures) == 0 && added > 0:
		return false
	case len(failures) == 0 && declared == 1:
		writeInlineFlash(w, "ok", "Already declared; re-applying it.")
	case len(failures) == 0:
		writeInlineFlash(w, "ok", "Already declared; re-applying them.")
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
	return true
}

func (s *Server) addImplicationPost(w http.ResponseWriter, r *http.Request) {
	parentID, ok := idAndForm(w, r)
	if !ok {
		return
	}
	added, edges, failures, res := s.declareImplications(w, r, "implied_id",
		func(tagID int64) (int64, int64) { return parentID, tagID })
	if res == nil {
		return
	}
	s.propagateDeclared(res, edges, "add")
	if added > 0 {
		setFlashHeader(w, implicationsAddedMsg(added), "ok", map[string]any{"implication-added": ""})
	}
	if !writeImplicationFailures(w, added, len(edges), failures) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) addImpliedByPost(w http.ResponseWriter, r *http.Request) {
	impliedID, ok := idAndForm(w, r)
	if !ok {
		return
	}
	added, edges, failures, res := s.declareImplications(w, r, "parent_id",
		func(tagID int64) (int64, int64) { return tagID, impliedID })
	if res == nil {
		return
	}
	s.propagateDeclared(res, edges, "add")
	if !writeImplicationFailures(w, added, len(edges), failures) {
		hxDone(w, r, implicationsAddedMsg(added), "", fmt.Sprintf("/tags/%d", impliedID))
	}
}

func (s *Server) removeImplicationDelete(w http.ResponseWriter, r *http.Request) {
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	impliedID, ok := pathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	// Claimed before the edge goes: without its sweep the implied rows
	// would stay on the images.
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	if err := s.tagSvc().RemoveImplication(parentID, impliedID); err != nil {
		s.jobs.Fail(err.Error())
		flashHeaderErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.goJobWork(models.JobTypeTag, func(context.Context) {
		s.runImplicationEdges([]models.Implication{{ParentID: parentID, ImpliedID: impliedID}}, "remove")
	})
	setFlashHeader(w, "Implication removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeImplicationsDelete(w http.ResponseWriter, r *http.Request) {
	s.removeImplicationGroup(w, r, s.tagSvc().ListImplications)
}

func (s *Server) removeImpliedByDelete(w http.ResponseWriter, r *http.Request) {
	s.removeImplicationGroup(w, r, s.tagSvc().ImpliedBy)
}

// One sweep job for the group: a job per edge would be refused after the
// first and leave implied rows behind.
func (s *Server) removeImplicationGroup(w http.ResponseWriter, r *http.Request, list func(int64) ([]models.Implication, error)) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	origin, stale := relationGroupFilter(r)
	edges, err := list(id)
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	edges = slices.DeleteFunc(edges, func(im models.Implication) bool { return im.Origin != origin || im.Stale != stale })
	s.removeImplicationsAndSweep(w, edges, map[string]any{"tag-relations-changed": ""})
}

func (s *Server) removeImplicationsAndSweep(w http.ResponseWriter, edges []models.Implication, extras map[string]any) {
	res := s.reserveJob(w)
	if res == nil {
		return
	}
	defer res.Release()
	var removed []models.Implication
	for _, im := range edges {
		if err := s.tagSvc().RemoveImplication(im.ParentID, im.ImpliedID); err != nil {
			logx.Warnf("remove implication %d -> %d: %v", im.ParentID, im.ImpliedID, err)
			continue
		}
		removed = append(removed, im)
	}
	if len(removed) > 0 && s.startReserved(res, models.JobTypeTag) {
		s.goJobWork(models.JobTypeTag, func(context.Context) { s.runImplicationGroupSweep(removed) })
	}
	setFlashHeader(w, fmt.Sprintf("%d implication%s removed.", len(removed), plural(len(removed))), "ok", extras)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runImplicationGroupSweep(edges []models.Implication) {
	ctx := s.jobs.Context()
	total := len(edges)
	closures := map[int64][]int64{}
	processed := 0
	cancelled := false

	s.jobs.Update(0, total, "removing implications…")
	for i, im := range edges {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		closure, ok := closures[im.ImpliedID]
		if !ok {
			var err error
			if closure, err = s.tagSvc().ImplicationClosure(im.ImpliedID); err != nil {
				s.jobs.Fail(err.Error())
				return
			}
			closures[im.ImpliedID] = closure
		}
		if _, err := s.tagSvc().SweepImplicationRemoval(ctx, im.ParentID, closure, nil); err != nil {
			logx.Warnf("implication sweep %d -> %d: %v", im.ParentID, im.ImpliedID, err)
		}
		processed = i + 1
		s.jobs.Update(processed, total, "removing implications…")
	}
	s.jobs.Finish(nil, cancelled,
		fmt.Sprintf("implication sweep cancelled (%d/%d)", processed, total),
		fmt.Sprintf("swept %d removed implication%s", processed, plural(processed)))
}

// One job for the whole request: jobs.Start refuses a second, so a job
// per edge would drop every fan-out after the first.
func (s *Server) runImplicationEdges(edges []models.Implication, op string) {
	ctx := s.jobs.Context()
	verb := "applying implication"
	if op == "remove" {
		verb = "removing implication"
	}
	progress := func(done, total int) { s.jobs.Update(done, total, verb+"…") }
	var processed, total int
	cancelled := false
	// Edges from one parent, or parents on one image, walk an image more than once.
	reached := map[int64]struct{}{}
	for _, e := range edges {
		var p tags.Propagation
		var err error
		if op == "remove" {
			var closure []int64
			if closure, err = s.tagSvc().ImplicationClosure(e.ImpliedID); err == nil {
				p, err = s.tagSvc().SweepImplicationRemoval(ctx, e.ParentID, closure, progress)
			}
		} else {
			p, err = s.tagSvc().PropagateImplication(ctx, e.ParentID, progress)
		}
		processed += len(p.Walked)
		total += p.Total
		for _, id := range p.Walked {
			reached[id] = struct{}{}
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if p.Cancelled {
			cancelled = true
			break
		}
	}
	done := fmt.Sprintf("Implication applied to %d image%s.", len(reached), plural(len(reached)))
	if op == "remove" {
		done = fmt.Sprintf("Implication removed from %d image%s.", len(reached), plural(len(reached)))
	}
	s.jobs.Finish(nil, cancelled, fmt.Sprintf("%s cancelled (%d/%d)", verb, processed, total), done)
}
