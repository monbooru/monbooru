package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func (s *Server) resolveBatchScope(w http.ResponseWriter, r *http.Request, errLabel string, viewOrder bool) ([]int64, bool) {
	scope := strings.TrimSpace(r.FormValue("scope"))
	switch scope {
	case "selection":
		return parseIDList(r.Form["ids"]), true
	case "search":
		return s.searchScopeIDs(w, r, errLabel, viewOrder)
	default:
		flashStatus(w, http.StatusBadRequest, "scope must be search or selection")
		return nil, false
	}
}

func (s *Server) searchScopeIDs(w http.ResponseWriter, r *http.Request, errLabel string, viewOrder bool) ([]int64, bool) {
	sc := search.NewScope(search.Parse(r.FormValue("q")), readRatingCookie(r))
	if viewOrder {
		seed, _ := strconv.ParseInt(r.FormValue("seed"), 10, 64)
		sc = sc.InViewOrder(r.FormValue("sort"), r.FormValue("order"), seed)
	}
	ids, err := sc.IDs(s.source())
	if err != nil {
		logx.Errorf("%s search: %v", errLabel, err)
		flashStatus(w, http.StatusInternalServerError, "Search error.")
		return nil, false
	}
	return ids, true
}

func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids := parseIDList(r.Form["ids"])
	// Chunked IN lists: a query per row costs a read per checkbox, and
	// one list overruns SQLite's parameter limit.
	var known []int64
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		part, err := db.QueryIDs(s.db().Read, `SELECT id FROM images WHERE id IN (`+placeholders+`)`, args...)
		known = append(known, part...)
		return err
	})
	if err != nil {
		logx.Warnf("batch delete: load targets: %v", err)
		flashStatus(w, http.StatusInternalServerError, "Could not load the selected images.")
		return
	}
	s.startIDsJob(w, models.JobTypeDelete, known, s.runBulkDelete)
}

func (s *Server) deleteSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.searchScopeIDs(w, r, "delete", false)
	if !ok {
		return
	}
	s.startIDsJob(w, models.JobTypeDelete, ids, s.runBulkDelete)
}

func (s *Server) runBulkDelete(ids []int64) {
	total := len(ids)
	s.jobs.Update(0, total, "deleting…")
	res, err := s.active().DeleteImages(s.jobs.Context(), ids, library.DeleteOptions{}, func(done, total int) {
		s.jobs.Update(done, total, "deleting…")
	})
	deleted := len(res.Deleted)
	summary := fmt.Sprintf("Deleted %d image%s%s.", deleted, plural(deleted), keptChanged(res.Changed))
	s.jobs.Finish(err, res.Cancelled, fmt.Sprintf("delete cancelled (%d/%d deleted)", deleted, total), summary)
}

// keptChanged is the clause a delete adds for the files it left on disk.
func keptChanged(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; kept %d file%s changed since indexing", n, plural(n))
}

func (s *Server) batchPlace(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))
	wantFolder := r.Form.Has("folder")

	folderTmpl, err := gallery.ParseNameTemplate(targetFolder, gallery.ScopeMoveBatch)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	if wantFolder && !folderTmpl.HasTokens() {
		if _, err := s.boundary().ResolveSubdir(targetFolder); err != nil {
			flashStatus(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	nameTmpl, err := gallery.ParseBatchRenameTemplate(strings.TrimSpace(r.FormValue("name")))
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	if !wantFolder && nameTmpl == nil {
		flashStatus(w, http.StatusBadRequest, "Name or folder required.")
		return
	}

	folder := &targetFolder
	if !wantFolder {
		folder = nil
	}
	// The check takes its own job type so its end does not refresh the
	// grid and drop the selection behind the open dialog. It walks the
	// run's display order, so {n} matches.
	if r.FormValue("check") == "1" {
		s.startScopedJob(w, r, "batch-place-check", models.JobTypeCheck, true, func(ids []int64) {
			s.runBatchPlaceCheck(ids, folder, folderTmpl, nameTmpl)
		})
		return
	}
	s.startScopedJob(w, r, "batch-place", models.JobTypeMove, true, func(ids []int64) {
		s.runBatchPlace(ids, folder, folderTmpl, nameTmpl)
	})
}

func (s *Server) perImageLoop(ids []int64, verb, gerund string, op func(i int, id int64) error) (done, failed int, cancelled bool) {
	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, gerund+"…")
	for i, id := range ids {
		if ctx.Err() != nil {
			return done, failed, true
		}
		if err := op(i, id); err != nil {
			logx.Warnf("batch %s %d: %v", verb, id, err)
			failed++
			continue
		}
		done++
		if (i+1)%25 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, gerund+"…")
		}
	}
	return done, failed, false
}

func (s *Server) runBatchPlace(ids []int64, targetFolder *string, folderTmpl, nameTmpl *gallery.NameTemplate) {
	total := len(ids)
	verb, gerund, _ := placeVerbs(targetFolder != nil, nameTmpl != nil)
	cx := s.active()
	tally := newPlaceTally()
	done, failed, cancelled := s.perImageLoop(ids, verb, gerund, func(i int, id int64) error {
		folder, name, err := s.batchDestination(targetFolder, folderTmpl, nameTmpl, i, total, id)
		if err != nil {
			return err
		}
		return tally.place(cx, id, folder, name)
	})

	// Named from what the run did: the folder field opens on {folder}, so
	// most renames move nothing.
	past := tally.past()
	summary := fmt.Sprintf("%s %d image%s.", titleCase(past), done, plural(done))
	if failed > 0 {
		summary = fmt.Sprintf("%s %d image%s, %d failed.", titleCase(past), done, plural(done), failed)
	}
	summary += tally.tail()
	s.jobs.Finish(nil, cancelled, fmt.Sprintf("%s cancelled (%d/%d %s)", verb, done, total, past), summary)
}

type placeTally struct {
	moved, renamed, suffixed int
	sources                  map[string]struct{}
}

func newPlaceTally() *placeTally { return &placeTally{sources: map[string]struct{}{}} }

func (t *placeTally) place(cx *library.Gallery, id int64, folder, name *string) error {
	res, err := cx.PlaceImage(id, folder, name)
	if err != nil {
		return err
	}
	if res.PrevDir != "" {
		t.sources[res.PrevDir] = struct{}{}
	}
	if res.Moved {
		t.moved++
	}
	if res.Renamed {
		t.renamed++
	}
	if res.Suffixed {
		t.suffixed++
	}
	return nil
}

func (t *placeTally) past() string {
	_, _, past := placeVerbs(t.moved > 0, t.renamed > 0)
	return past
}

func (t *placeTally) tail() string {
	var tail string
	if t.suffixed > 0 {
		tail += fmt.Sprintf(" %d had a taken name and %s numbered.", t.suffixed, models.Plural(t.suffixed, "was", "were"))
	}
	if n := countEmptiedDirs(t.sources); n > 0 {
		tail += fmt.Sprintf(" %d %s now empty.", n, models.Plural(n, "folder is", "folders are"))
	}
	return tail
}

func countEmptiedDirs(dirs map[string]struct{}) int {
	n := 0
	for dir := range dirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			n++
		}
	}
	return n
}

func (s *Server) runBatchPlaceCheck(ids []int64, targetFolder *string, folderTmpl, nameTmpl *gallery.NameTemplate) {
	total := len(ids)
	planned := make(map[string]bool, total)
	collisions := 0
	done, failed, cancelled := s.perImageLoop(ids, "check", "checking", func(i int, id int64) error {
		folder, name, err := s.batchDestination(targetFolder, folderTmpl, nameTmpl, i, total, id)
		if err != nil {
			return err
		}
		_, numbered, err := gallery.PlannedPath(s.db(), s.boundary(), id, folder, name, planned)
		if err != nil {
			return err
		}
		if numbered {
			collisions++
		}
		return nil
	})

	summary := fmt.Sprintf("Checked %d image%s: no name is already taken.", done, plural(done))
	if collisions > 0 {
		summary = fmt.Sprintf("Checked %d image%s: %d would be numbered, %s already taken.", done, plural(done), collisions, models.Plural(collisions, "its name is", "their names are"))
	}
	if failed > 0 {
		summary += fmt.Sprintf(" %d could not be checked.", failed)
	}
	s.jobs.Finish(nil, cancelled, fmt.Sprintf("check cancelled (%d/%d checked)", done, total), summary)
}

func (s *Server) batchDestination(targetFolder *string, folderTmpl, nameTmpl *gallery.NameTemplate, i, total int, id int64) (folder, name *string, err error) {
	if targetFolder != nil {
		rendered, renderErr := s.renderName(s.jobs.Context(), folderTmpl, *targetFolder, i, total, id)
		if renderErr != nil {
			return nil, nil, renderErr
		}
		folder = &rendered
	}
	if nameTmpl != nil {
		rendered, renderErr := s.renderName(s.jobs.Context(), nameTmpl, "", i, total, id)
		if renderErr != nil {
			return nil, nil, renderErr
		}
		name = &rendered
	}
	return folder, name, nil
}

func (s *Server) renderName(ctx context.Context, tmpl *gallery.NameTemplate, literal string, i, total int, id int64) (string, error) {
	if !tmpl.HasTokens() {
		return literal, nil
	}
	facts, err := s.active().NameFacts(ctx, id, 0, tmpl)
	if err != nil {
		return "", err
	}
	facts.N, facts.NWidth = i+1, max(len(strconv.Itoa(total)), 2)
	return tmpl.Render(facts)
}

func (s *Server) batchTag(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	op := strings.TrimSpace(r.FormValue("op"))
	if op != "add" && op != "remove" {
		flashStatus(w, http.StatusBadRequest, "op must be add or remove")
		return
	}
	tagInput, ok := requiredFormFlash(w, r, "tags", "No tags provided.")
	if !ok {
		return
	}
	catTags, unknownCats, parseErrMsg := s.parseTagInput(tagInput)
	if parseErrMsg != "" {
		flashStatus(w, http.StatusBadRequest, parseErrMsg)
		return
	}
	if len(catTags) == 0 {
		flashStatus(w, http.StatusBadRequest, "No tags to apply.")
		return
	}

	note := unknownCategoryNote(unknownCats)
	s.startScopedJob(w, r, "batch-tag", models.JobTypeTag, false, func(ids []int64) {
		s.runBatchTag(ids, op, catTags, note)
	})
}

func (s *Server) runBatchTag(ids []int64, op string, catTags []catTag, note string) {
	var tagIDs []int64
	var refused skipReasons
	var unmatched []string
	if op == "add" {
		for _, ct := range catTags {
			t, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
			if err != nil {
				logx.Warnf("batch-tag get-or-create %q: %v", ct.name, err)
				refused.add(fmt.Errorf("%s: %w", ct.name, err))
				continue
			}
			tagIDs = append(tagIDs, t.ID)
		}
	} else {
		for _, ct := range catTags {
			t, err := s.tagSvc().FindTag(ct.name, ct.catID)
			if err != nil {
				unmatched = append(unmatched, s.tagTokenLabel(ct))
				continue
			}
			tagIDs = append(tagIDs, t.ID)
		}
	}
	if len(tagIDs) == 0 {
		if refused.any() {
			s.jobs.Fail("nothing to add: " + refused.String())
			return
		}
		s.jobs.Complete(fmt.Sprintf("nothing to %s (no matching tags)", op))
		return
	}

	label, summary := "tagging", "Tagged"
	if op == "remove" {
		label, summary = "untagging", "Untagged"
	}

	total := len(ids)
	progress := func(done, total int) { s.jobs.Update(done, total, label+"…") }
	progress(0, total)
	batch := s.tagSvc().BatchRemoveTags
	if op == "add" {
		batch = s.tagSvc().BatchAddTags
	}
	res, err := batch(s.jobs.Context(), ids, tagIDs, progress)

	done := fmt.Sprintf("%s %d of %d image%s (%d row change%s).", summary, res.Touched, res.Processed, plural(res.Processed), res.Changed, plural(res.Changed))
	if note != "" {
		done += " " + note + "."
	}
	if refused.any() {
		done += " Refused: " + refused.String() + "."
	}
	if note := joinLabeled("no tag matched: ", ", ", unmatched); note != "" {
		done += " " + note + "."
	}
	if res.Replaced > 0 {
		done += fmt.Sprintf(" Replaced the rating on %d image%s.", res.Replaced, plural(res.Replaced))
	}
	if res.Kept > 0 {
		done += fmt.Sprintf(" Kept %d row%s another tag on the image implies.", res.Kept, plural(res.Kept))
	}
	s.jobs.Finish(err, res.Cancelled, fmt.Sprintf("%s cancelled (%d/%d processed)", label, res.Processed, total), done)
}

func (s *Server) batchStrip(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	switch mode {
	case "user", "auto", "all", "source", "source-all", "stale":
	default:
		flashStatus(w, http.StatusBadRequest, "mode must be user, auto, all, source, source-all, or stale")
		return
	}
	var filterName string
	switch mode {
	case "auto":
		filterName = strings.TrimSpace(r.FormValue("tagger_name"))
	case "source":
		var ok bool
		if filterName, ok = requiredFormFlash(w, r, "source", "pick a source"); !ok {
			return
		}
	}

	s.startScopedJob(w, r, "batch-strip", models.JobTypeTag, false, func(ids []int64) {
		s.runBatchStrip(ids, mode, filterName)
	})
}

func (s *Server) runBatchStrip(ids []int64, mode, filterName string) {
	var label, summary string
	switch mode {
	case "user":
		label, summary = "removing user tags", "Removed user tags from"
	case "auto":
		if filterName != "" {
			label = fmt.Sprintf("removing %s auto-tags", filterName)
			summary = fmt.Sprintf("Removed %s auto-tags from", filterName)
		} else {
			label, summary = "removing auto-tags", "Removed auto-tags from"
		}
	case "source":
		label = fmt.Sprintf("removing %s tags", filterName)
		summary = fmt.Sprintf("Removed %s tags from", filterName)
	case "source-all":
		label, summary = "removing source tags", "Removed source tags from"
	case "stale":
		label, summary = "removing stale tags", "Removed stale tags from"
	case "all":
		label, summary = "removing tags", "Removed all tags from"
	}

	total := len(ids)
	s.jobs.Update(0, total, label+"…")
	res, err := s.tagSvc().StripTags(s.jobs.Context(), ids, tags.StripSpec{Mode: mode, Name: filterName}, func(done, total int) {
		s.jobs.Update(done, total, label+"…")
	})
	done := fmt.Sprintf("%s %d of %d image%s (%d row change%s).", summary, res.Touched, res.Processed, plural(res.Processed), res.Removed, plural(res.Removed))
	if res.Kept > 0 {
		done += fmt.Sprintf(" Kept %d row%s another tag on the image implies.", res.Kept, plural(res.Kept))
	}
	s.jobs.Finish(err, res.Cancelled, fmt.Sprintf("%s cancelled (%d/%d processed)", label, res.Processed, total), done)
}

func (s *Server) batchInbox(w http.ResponseWriter, r *http.Request) {
	s.startScopedJob(w, r, "batch-inbox", models.JobTypeTag, false, func(ids []int64) {
		s.runBulkToggle(ids, gallery.FlagInbox, "inbox state", "inbox toggle", "Toggled inbox state")
	})
}

func (s *Server) batchFavorite(w http.ResponseWriter, r *http.Request) {
	s.startScopedJob(w, r, "batch-favorite", models.JobTypeTag, false, func(ids []int64) {
		s.runBulkToggle(ids, gallery.FlagFavorited, "favorite state", "favorite toggle", "Toggled favorite state")
	})
}

func (s *Server) startScopedJob(w http.ResponseWriter, r *http.Request, scopeLabel, jobType string, viewOrder bool, run func([]int64), others ...string) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.resolveBatchScope(w, r, scopeLabel, viewOrder)
	if !ok {
		return
	}
	s.startIDsJob(w, jobType, ids, run, others...)
}

func (s *Server) runBulkToggle(ids []int64, flag gallery.ImageFlag, progressNoun, cancelNoun, successNoun string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	cx := s.active()

	processed, cancelled, err := jobs.Chunked(ctx, s.jobs, ids, chunkSize, "toggling "+progressNoun, func(chunk []int64) error {
		return cx.ToggleFlags(chunk, flag)
	})
	s.jobs.Finish(err, cancelled, fmt.Sprintf("%s cancelled (%d/%d toggled)", cancelNoun, processed, total), fmt.Sprintf("%s for %d image%s.", successNoun, processed, plural(processed)))
}

func (s *Server) batchCollection(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	collectionVal, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	if utf8.RuneCountInString(collectionVal) > contract.MaxCollection {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return
	}
	mode := r.FormValue("mode")
	if mode != "remove" {
		mode = "add"
	}

	s.startScopedJob(w, r, "batch-collection", models.JobTypeTag, false, func(ids []int64) {
		s.runBatchCollection(ids, collectionVal, mode, false)
	})
}

func (s *Server) runBatchCollection(ids []int64, label, mode string, dissolve bool) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	remove := mode == "remove"
	verb := "adding to collection"
	if remove {
		verb = "removing from collection"
	}

	cx := s.active()
	processed, cancelled, err := jobs.Chunked(ctx, s.jobs, ids, chunkSize, verb, func(chunk []int64) error {
		return cx.Edit(func(d *db.DB) error {
			if remove {
				return gallery.RemoveCollectionFromImages(d, chunk, label)
			}
			return gallery.AddCollectionToImages(d, chunk, label)
		})
	})
	// So a later collection reusing the label starts opted out.
	if dissolve && err == nil && !cancelled {
		if e := cx.Edit(func(d *db.DB) error { return gallery.SetCollectionFindRelations(d, label, false) }); e != nil {
			logx.Debugf("dissolve collection find-relations flag: %v", e)
		}
	}
	done := fmt.Sprintf("Added %d image%s to collection.", processed, plural(processed))
	if remove {
		done = fmt.Sprintf("Removed %d image%s from collection.", processed, plural(processed))
	}
	s.jobs.Finish(err, cancelled, fmt.Sprintf("collection cancelled (%d/%d processed)", processed, total), done)
}

func (s *Server) batchLookup(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	opts := monloader.BatchLookupOpts{
		Mode:      r.FormValue("mode"),
		PTR:       r.FormValue("ptr") == "1",
		Booru:     r.FormValue("booru") == "1",
		Unsourced: r.FormValue("unsourced") == "1",
	}
	on := r.FormValue("on") == "1"
	switch opts.Mode {
	case "refresh", "schedule":
	case "hash":
		if !opts.PTR && !opts.Booru {
			http.Error(w, "pick at least one lookup backend", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unknown lookup mode", http.StatusBadRequest)
		return
	}
	s.startScopedJob(w, r, "batch-lookup", models.JobTypeTag, false, func(ids []int64) {
		s.runBatchLookup(opts, on, ids)
	})
}

func (s *Server) batchLookupCount(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.resolveBatchScope(w, r, "batch-lookup-count", false)
	if !ok {
		return
	}
	// One read of every sourced id, intersected in memory: a chunked EXISTS
	// probe over a whole-library scope is thousands of round trips.
	withURL, err := db.QueryIDs(s.db().Read,
		`SELECT DISTINCT image_id FROM image_sources WHERE url <> ''`)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Count failed.")
		return
	}
	slices.Sort(withURL)
	sourced := 0
	for _, id := range ids {
		if _, found := slices.BinarySearch(withURL, id); found {
			sourced++
		}
	}
	contract.WriteJSON(w, http.StatusOK, map[string]int{"total": len(ids), "sourced": sourced, "unsourced": len(ids) - sourced})
}

func (s *Server) runBatchLookup(opts monloader.BatchLookupOpts, on bool, ids []int64) {
	ctx := s.jobs.Context()
	cx := s.active()
	if cx == nil {
		s.jobs.Fail("no active gallery")
		return
	}
	if opts.Mode == "schedule" {
		s.runBatchSchedule(ctx, cx, on, ids)
		return
	}
	s.ml.BatchLookup(ctx, cx, opts, ids)
}

func (s *Server) runBatchSchedule(ctx context.Context, cx *library.Gallery, on bool, ids []int64) {
	processed, cancelled, err := jobs.Chunked(ctx, s.jobs, ids, 500, "setting scheduled lookup", func(chunk []int64) error {
		return cx.SetScheduledLookup(chunk, []string{contract.LookupBooru, contract.LookupPTR}, on)
	})
	onOff := "off"
	if on {
		onOff = "on"
	}
	s.jobs.Finish(err, cancelled, fmt.Sprintf("Scheduled lookup cancelled (%d/%d set)", processed, len(ids)),
		fmt.Sprintf("Scheduled lookup turned %s for %d image%s.", onOff, processed, plural(processed)))
}
