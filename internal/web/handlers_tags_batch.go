package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// Capped at three: the job summary is one line and the only place they show.
type skipReasons struct {
	seen    []string
	dropped int
}

func (s *skipReasons) add(err error) {
	msg := err.Error()
	if slices.Contains(s.seen, msg) {
		return
	}
	if len(s.seen) >= 3 {
		s.dropped++
		return
	}
	s.seen = append(s.seen, msg)
}

func (s *skipReasons) any() bool { return len(s.seen) > 0 }

func (s *skipReasons) String() string {
	out := strings.Join(s.seen, "; ")
	if s.dropped > 0 {
		out += fmt.Sprintf("; and %d more", s.dropped)
	}
	return out
}

// No change plus a refusal fails: every completion shows a green check.
func (s *Server) finishTagScopeJob(changed int, reasons skipReasons, cancelled bool, noun, summary string) {
	if reasons.any() {
		summary += ": " + reasons.String()
	}
	var failed error
	if !cancelled && changed == 0 && reasons.any() {
		failed = errors.New(summary)
	}
	s.jobs.Finish(failed, cancelled, fmt.Sprintf("%s cancelled (%s)", noun, summary), summary)
}

func (s *Server) resolveTagScope(r *http.Request) ([]int64, error) {
	q := r.Form
	// An empty ids is an empty selection; a missing one means the filter.
	if q.Has("ids") {
		idsStr := strings.TrimSpace(q.Get("ids"))
		if idsStr == "" {
			return nil, nil
		}
		var ids []int64
		for _, part := range strings.Split(idsStr, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad tag id %q", part)
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	// The listing's own filter, so the escalation acts on what the page shows.
	filter := s.tagListingFilter(tagListingParamsFrom(q))
	filter.PageIndex, filter.Limit = 0, 0
	return s.tagSvc().ListTagIDs(filter)
}

func (s *Server) startTagScopeRun(w http.ResponseWriter, r *http.Request, run func(ids []int64)) {
	if !parseFormOK(w, r) {
		return
	}
	ids, err := s.resolveTagScope(r)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	s.startIDsJob(w, models.JobTypeTag, ids, run)
}

// op returns true for a changed row, false for a skip; an error is a refusal.
func (s *Server) runTagScopeLoop(ids []int64, gerund string, stride int, op func(id int64) (bool, error)) (changed, skipped int, reasons skipReasons, cancelled bool) {
	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, gerund)
	for i, id := range ids {
		if ctx.Err() != nil {
			return changed, skipped, reasons, true
		}
		switch ok, err := op(id); {
		case err != nil:
			reasons.add(err)
			skipped++
		case ok:
			changed++
		default:
			skipped++
		}
		if (i+1)%stride == 0 || i+1 == total {
			s.jobs.Update(i+1, total, gerund)
		}
	}
	return changed, skipped, reasons, false
}

func skippedSuffix(summary string, skipped int) string {
	if skipped > 0 {
		summary += fmt.Sprintf(", skipped %d", skipped)
	}
	return summary
}

func (s *Server) batchTagCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	catID, err := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid category.")
		return
	}
	merge := r.FormValue("merge") == "1"
	keep := r.FormValue("keep_alias") == "1"
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagCategory(ids, catID, merge, keep) })
}

func (s *Server) runBatchTagCategory(ids []int64, catID int64, merge, keep bool) {
	mergedCount := 0
	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "moving tags…", 50, func(id int64) (bool, error) {
		var current int64
		if err := s.db().Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&current); err == nil && current == catID {
			return false, nil
		}
		if !merge {
			move := s.tagSvc().ChangeTagCategory
			if keep {
				move = s.tagSvc().ChangeTagCategoryKeepAlias
			}
			if err := move(id, catID); err != nil {
				logx.Warnf("batch category tag %d: %v", id, err)
				return false, err
			}
			return true, nil
		}
		didMerge, err := s.tagSvc().ChangeTagCategoryMerge(id, catID, keep)
		if err != nil {
			logx.Warnf("batch category tag %d: %v", id, err)
			return false, err
		}
		if didMerge {
			mergedCount++
		}
		return true, nil
	})

	summary := fmt.Sprintf("moved %d tag%s", changed-mergedCount, plural(changed-mergedCount))
	if mergedCount > 0 {
		summary += fmt.Sprintf(", merged %d", mergedCount)
	}
	s.finishTagScopeJob(changed, reasons, cancelled, "category move", skippedSuffix(summary, skipped))
}

func (s *Server) batchTagAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	canon, msg := s.resolveCanonicalTagInput(r.FormValue("canonical_id"), true)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) {
		canonID, err := s.canonicalID(canon)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.runBatchTagAlias(ids, canonID)
	})
}

func (s *Server) runBatchTagAlias(ids []int64, canonID int64) {
	ids = slices.DeleteFunc(ids, func(id int64) bool { return id == canonID })
	aliased, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "aliasing tags…", 50, func(id int64) (bool, error) {
		if err := s.tagSvc().MergeTags(id, canonID); err != nil {
			logx.Warnf("batch alias tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	summary := skippedSuffix(fmt.Sprintf("aliased %d tag%s to %s", aliased, plural(aliased), s.qualifiedTagName(canonID)), skipped)
	s.finishTagScopeJob(aliased, reasons, cancelled, "alias", summary)
}

func (s *Server) batchMergeFoldedPost(w http.ResponseWriter, r *http.Request) {
	s.startTagScopeRun(w, r, s.runMergeFolded)
}

func (s *Server) runMergeFolded(ids []int64) {
	s.jobs.Update(0, len(ids), "merging folded tags…")
	res, err := s.tagSvc().MergeFolded(s.jobs.Context(), ids)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	var reasons skipReasons
	for _, e := range res.Refused {
		reasons.add(e)
	}
	summary := skippedSuffix(fmt.Sprintf("merged %d folded tag%s", res.Merged, plural(res.Merged)), res.Skipped)
	s.finishTagScopeJob(res.Merged, reasons, res.Cancelled, "folded merge", summary)
}

func (s *Server) batchTagImplyPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	remove := r.FormValue("mode") == "remove"
	tokens, err := splitTagTokens(r.FormValue("target"))
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(tokens) == 0 {
		flashStatus(w, http.StatusBadRequest, "Tag name is required.")
		return
	}
	// Every token resolves before the job starts: one bad token refuses the
	// run rather than leaving it half applied.
	targets := make([]canonicalRef, 0, len(tokens))
	for _, token := range tokens {
		ref, msg := s.resolveCanonicalTagInput(token, !remove)
		if msg != "" {
			if len(tokens) > 1 {
				msg = token + ": " + msg
			}
			flashStatus(w, http.StatusBadRequest, msg)
			return
		}
		targets = append(targets, ref)
	}
	s.startTagScopeRun(w, r, func(ids []int64) {
		var targetIDs []int64
		for _, ref := range targets {
			id, err := s.canonicalID(ref)
			if err != nil {
				s.jobs.Fail(err.Error())
				return
			}
			if !slices.Contains(targetIDs, id) {
				targetIDs = append(targetIDs, id)
			}
		}
		s.runBatchTagImply(ids, targetIDs, remove)
	})
}

func (s *Server) runBatchTagImply(ids, targetIDs []int64, remove bool) {
	ctx := s.jobs.Context()
	verb := "declaring implications…"
	if remove {
		verb = "removing implications…"
	}

	var removeClosure []int64
	if remove {
		for _, targetID := range targetIDs {
			closure, err := s.tagSvc().ImplicationClosure(targetID)
			if err != nil {
				s.jobs.Fail(err.Error())
				return
			}
			for _, id := range closure {
				if !slices.Contains(removeClosure, id) {
					removeClosure = append(removeClosure, id)
				}
			}
		}
	}

	edges, reapplied := 0, 0
	var partial skipReasons
	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, verb, 10, func(parentID int64) (bool, error) {
		touched := 0
		var refusal error
		for _, targetID := range targetIDs {
			if targetID == parentID {
				continue
			}
			var err error
			if remove {
				err = s.tagSvc().RemoveImplication(parentID, targetID)
			} else {
				var created bool
				if created, err = s.tagSvc().AddImplicationFrom(parentID, targetID, "user"); err == nil && !created {
					reapplied++
				}
			}
			if err != nil {
				logx.Warnf("batch imply parent %d -> %d: %v", parentID, targetID, err)
				if refusal == nil {
					refusal = err
				}
				continue
			}
			touched++
		}
		edges += touched
		if touched > 0 {
			var err error
			if remove {
				_, err = s.tagSvc().SweepImplicationRemoval(ctx, parentID, removeClosure, nil)
			} else {
				_, err = s.tagSvc().PropagateImplication(ctx, parentID, nil)
			}
			if err != nil {
				logx.Warnf("batch imply walk parent %d: %v", parentID, err)
			}
		}
		if refusal != nil && touched == 0 {
			return false, refusal
		}
		if refusal != nil {
			partial.add(refusal)
		}
		return touched > 0, nil
	})
	for _, msg := range partial.seen {
		reasons.add(errors.New(msg))
	}

	names := make([]string, len(targetIDs))
	for i, id := range targetIDs {
		names[i] = s.qualifiedTagName(id)
	}
	noun := "declared"
	if remove {
		noun = "removed"
	}
	summary := fmt.Sprintf("%s %d implication%s of %s", noun, edges-reapplied, plural(edges-reapplied), strings.Join(names, ", "))
	if reapplied > 0 {
		summary += fmt.Sprintf(", re-applied %d", reapplied)
	}
	summary = skippedSuffix(summary, skipped)
	s.finishTagScopeJob(changed, reasons, cancelled, "implication batch", summary)
}

func cleanTagName(name, chars string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(chars, r) {
			return -1
		}
		return r
	}, name)
}

type cleanStep struct {
	ID      int64
	Name    string
	Cleaned string
	// rename, merge or skip; empty when the name holds none of the characters.
	Action string
	Into   int64
	// The merge target's name, or why the tag is skipped.
	Note string
}

// planClean decides one tag's fate against the catalog as it stands, plus
// planned, the names earlier steps of a dry run would have taken.
func (s *Server) planClean(t *models.Tag, chars string, planned map[string]int64) cleanStep {
	step := cleanStep{ID: t.ID, Name: t.Name}
	cleaned := cleanTagName(t.Name, chars)
	if cleaned == t.Name {
		return step
	}
	step.Action = "skip"
	switch {
	case t.IsAlias:
		step.Note = "an alias"
		return step
	case t.CategoryID == s.tagSvc().RatingCategoryID():
		step.Note = "a rating"
		return step
	}
	normalized, err := tags.ValidateTagName(cleaned)
	if err != nil {
		step.Note = err.Error()
		return step
	}
	step.Cleaned = normalized
	key := strconv.FormatInt(t.CategoryID, 10) + ":" + normalized
	into, ok := planned[key]
	if !ok {
		var clash int64
		switch err := s.db().Read.QueryRow(
			`SELECT id, COALESCE(canonical_tag_id, id) FROM tags WHERE name = ? AND category_id = ? AND id != ?`,
			normalized, t.CategoryID, t.ID,
		).Scan(&clash, &into); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			step.Note = err.Error()
			return step
		}
	}
	switch into {
	case 0:
		step.Action = "rename"
		if planned != nil {
			planned[key] = t.ID
		}
	case t.ID:
		step.Note = "its own alias holds " + normalized
	default:
		step.Action, step.Into = "merge", into
		step.Note = s.qualifiedTagName(into)
		if ok {
			step.Note = t.CategoryName + ":" + normalized
		}
	}
	return step
}

const cleanPreviewRows = 50

func (s *Server) batchTagCleanPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	chars := r.FormValue("chars")
	if strings.TrimSpace(chars) == "" {
		if r.FormValue("check") == "1" {
			return
		}
		flashStatus(w, http.StatusBadRequest, "Type the characters to remove.")
		return
	}
	if r.FormValue("check") == "1" {
		s.batchTagCleanPreview(w, r, chars)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagClean(ids, chars) })
}

func (s *Server) batchTagCleanPreview(w http.ResponseWriter, r *http.Request, chars string) {
	ids, err := s.resolveTagScope(r)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	data := struct {
		Steps                                   []cleanStep
		Renames, Merges, Skips, Unchanged, More int
	}{}
	planned := map[string]int64{}
	err = db.Chunked(ids, 500, func(chunk []int64) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		found, err := s.tagSvc().GetTags(chunk)
		if err != nil {
			return err
		}
		for _, id := range chunk {
			t := found[id]
			if t == nil {
				continue
			}
			step := s.planClean(t, chars, planned)
			switch step.Action {
			case "":
				data.Unchanged++
				continue
			case "rename":
				data.Renames++
			case "merge":
				data.Merges++
			default:
				data.Skips++
			}
			if len(data.Steps) == cleanPreviewRows {
				data.More++
				continue
			}
			data.Steps = append(data.Steps, step)
		}
		return nil
	})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	s.renderTemplate(w, "partials/tag_clean_preview.html", data)
}

// A merge leaves the old name behind as an alias, and a rename keeps it
// as one, so a source still sending the old spelling lands on the
// cleaned tag.
func (s *Server) runBatchTagClean(ids []int64, chars string) {
	renamed, merged := 0, 0
	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "cleaning names…", 20, func(id int64) (bool, error) {
		t, err := s.tagSvc().GetTag(id)
		if err != nil {
			return false, err
		}
		switch step := s.planClean(t, chars, nil); step.Action {
		case "rename":
			if err := s.tagSvc().RenameTagKeepAlias(id, step.Cleaned); err != nil {
				return false, err
			}
			renamed++
		case "merge":
			if err := s.tagSvc().MergeTags(id, step.Into); err != nil {
				return false, err
			}
			merged++
		default:
			return false, nil
		}
		return true, nil
	})
	summary := fmt.Sprintf("renamed %d tag%s, merged %d", renamed, plural(renamed), merged)
	s.finishTagScopeJob(changed, reasons, cancelled, "name cleanup", skippedSuffix(summary, skipped))
}

func (s *Server) batchTagDistinctPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, err := s.resolveTagScope(r)
	if err != nil {
		flashHeaderErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.FormValue("mode") == "remove" {
		n, err := s.tagSvc().UnmarkDistinct(ids)
		if err != nil {
			flashHeaderErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n == 0 {
			flashHeaderErr(w, http.StatusBadRequest, "None of the selected tags is marked distinct.")
			return
		}
		hxDone(w, r, fmt.Sprintf("Unmarked %d distinct pair%s.", n, plural(n)), "", "/tags")
		return
	}
	marked, skipped, err := s.tagSvc().MarkDistinct(ids)
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if marked == 0 {
		msg := "The selected tags are already marked distinct."
		if len(ids)-skipped < 2 {
			msg = "Select two or more tags that share a name."
		}
		flashHeaderErr(w, http.StatusBadRequest, msg)
		return
	}
	msg := fmt.Sprintf("Marked %d pair%s distinct.", marked, plural(marked))
	if skipped > 0 {
		msg += fmt.Sprintf(" Skipped %d tag%s with no same-name tag selected.", skipped, plural(skipped))
	}
	hxDone(w, r, msg, "", "/tags")
}

func (s *Server) unmarkTagDistinctDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	otherID, ok := pathInt64(w, r, "otherID")
	if !ok {
		return
	}
	if _, err := s.tagSvc().UnmarkDistinctPair(id, otherID); err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hxDone(w, r, "Unmarked: the pair counts as a conflict again.", "", fmt.Sprintf("/tags/%d", id))
}
