package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

const collectionsPerPage = 60

// Generous on purpose: the template clips the strip to one line.
const collectionPreviewSamples = 16

const collectionOrderWindow = 200

const collectionOrderMax = 25 * collectionOrderWindow

type collectionsPageData struct {
	baseData
	Collections []gallery.CollectionSummary
	Total       int
	Page        int
	TotalPages  int
	Prefix      string
	Sort        string
}

func (s *Server) collectionsHandler(w http.ResponseWriter, r *http.Request) {
	// A rename or dissolve job changes the listing a reload must show.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	sortStr := q.Get("sort")
	if sortStr != "name" {
		sortStr = "size"
	}
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	ceiling := resolveCeiling(r, s.active()).RankFilter()

	total, err := gallery.CountCollections(s.db(), prefix, ceiling)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totalPages := (total + collectionsPerPage - 1) / collectionsPerPage
	if total > 0 && page > totalPages {
		page = totalPages
	}

	list, err := gallery.ListCollections(s.db(), prefix, sortStr, collectionsPerPage, (page-1)*collectionsPerPage, ceiling)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, len(list))
	for i := range list {
		names[i] = list[i].Name
	}
	samples, err := gallery.CollectionSamples(s.db(), names, collectionPreviewSamples, ceiling)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].Samples = samples[strings.ToLower(list[i].Name)]
	}

	s.renderTemplate(w, "collections.html", collectionsPageData{
		baseData:    s.base(r, "collections", "Collections - "+s.booruName()),
		Collections: list,
		Total:       total,
		Page:        page,
		TotalPages:  totalPages,
		Prefix:      prefix,
		Sort:        sortStr,
	})
}

// Renaming onto an existing label merges the two.
func (s *Server) renameCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := strings.TrimSpace(r.FormValue("prev"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if oldName == "" || newName == "" {
		flashStatus(w, http.StatusBadRequest, "Both the current and the new collection name are required.")
		return
	}
	if utf8.RuneCountInString(newName) > contract.MaxCollection {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return
	}
	if oldName == newName {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.startCollectionJob(w, oldName, func(ids []int64) {
		s.runRenameCollection(ids, oldName, newName)
	})
}

func (s *Server) collectionFindRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	enabled := r.FormValue("enabled") == "1"
	if err := s.active().Edit(func(d *db.DB) error { return gallery.SetCollectionFindRelations(d, name, enabled) }); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not update the collection.")
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	setFlashHeader(w, "Find relations "+verb+" for "+name+".", "ok", nil)
	s.renderTemplate(w, "partials/collection_find_relations.html", map[string]any{
		"Name":          name,
		"FindRelations": enabled,
	})
}

func (s *Server) collectionOrderDialog(w http.ResponseWriter, r *http.Request) {
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/collections", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "collection required", http.StatusBadRequest)
		return
	}
	limit := collectionOrderWindow
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, collectionOrderMax)
	}
	ceiling := resolveCeiling(r, s.active()).RankFilter()
	members, err := gallery.CollectionMembers(s.db(), name, ceiling, limit+1, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasMore := len(members) > limit
	if hasMore {
		members = members[:limit]
	}
	ceilingHidden, _ := gallery.CollectionCeilingHidden(s.db(), name, ceiling)
	s.renderTemplate(w, "partials/collection_order.html", map[string]any{
		"Name":          name,
		"Members":       members,
		"Gallery":       s.activeGallery(),
		"HasMore":       hasMore,
		"CanGrow":       limit < collectionOrderMax,
		"NextLimit":     limit + collectionOrderWindow,
		"CeilingHidden": ceilingHidden,
	})
}

func (s *Server) reorderCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	if r.FormValue("mode") == "filename" {
		if err := s.active().Edit(func(d *db.DB) error { return gallery.SortCollectionByFilename(d, name) }); err != nil {
			flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
			return
		}
		writeInlineFlash(w, "ok", "Ordered by filename.")
		return
	}
	raw, ok := requiredFormFlash(w, r, "ids", "Click at least one image first.")
	if !ok {
		return
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			flashStatus(w, http.StatusBadRequest, "Bad image id list.")
			return
		}
		ids = append(ids, id)
	}
	clicked := len(ids)
	hidden, err := gallery.CollectionHiddenOrderedIDs(s.db(), name, resolveCeiling(r, s.active()).RankFilter())
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
		return
	}
	ids = append(ids, hidden...)
	if err := s.active().Edit(func(d *db.DB) error { return gallery.ReorderCollection(d, name, ids) }); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Ordered %d image%s.", clicked, plural(clicked)))
}

func (s *Server) startCollectionJob(w http.ResponseWriter, name string, run func([]int64)) {
	ids, err := gallery.CollectionMemberIDs(s.db(), name)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not read the collection.")
		return
	}
	if len(ids) == 0 {
		flashStatus(w, http.StatusBadRequest, "That collection no longer exists.")
		return
	}
	s.startIDsJob(w, models.JobTypeTag, ids, run)
}

func (s *Server) dissolveCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name, ok := requiredFormFlash(w, r, "collection", "Collection label required.")
	if !ok {
		return
	}
	s.startCollectionJob(w, name, func(ids []int64) { s.runBatchCollection(ids, name, "remove", true) })
}

func (s *Server) runRenameCollection(ids []int64, oldName, newName string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	// A case-only rename hits the same NOCASE label: merging would delete
	// the very rows it is meant to recase.
	merging := !strings.EqualFold(oldName, newName)

	// Read once before any chunk: the target's own members are never
	// renumbered, so the offset holds for the whole job.
	var posOffset int
	if merging {
		var spelled sql.NullString
		if err := s.db().Read.QueryRow(
			`SELECT COALESCE(MAX(position), 0), MIN(name) FROM image_collections WHERE name = ?`, newName,
		).Scan(&posOffset, &spelled); err != nil {
			logx.Debugf("rename collection position offset: %v", err)
		}
		if spelled.Valid {
			newName = spelled.String
		}
	}

	cx := s.active()
	processed, cancelled, err := jobs.Chunked(ctx, s.jobs, ids, chunkSize, "renaming collection", func(chunk []int64) error {
		return cx.Edit(func(d *db.DB) error {
			return gallery.RenameCollectionForImages(d, chunk, oldName, newName, posOffset, merging)
		})
	})
	if err == nil && !cancelled {
		if e := cx.Edit(func(d *db.DB) error { return gallery.RenameCollectionFindRelations(d, oldName, newName, merging) }); e != nil {
			logx.Debugf("rename collection find-relations flag: %v", e)
		}
	}
	s.jobs.Finish(err, cancelled, fmt.Sprintf("rename cancelled (%d/%d processed)", processed, total), fmt.Sprintf("Renamed collection across %d image%s.", processed, plural(processed)))
}
