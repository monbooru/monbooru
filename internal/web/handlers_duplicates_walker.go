package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type sha256DuplicateRow struct {
	ImageID       int64
	CanonicalPath string
	PathID        int64
	AliasPath     string
}

// A copy the gallery leaves out is another gallery's file, and only Go can
// tell, so every row is scanned; total counts the kept ones and only the
// window [offset, offset+limit) is held.
func duplicatePaths(r *http.Request, cx *library.Gallery, offset, limit int) (window []sha256DuplicateRow, total int, err error) {
	query := `SELECT i.id, i.canonical_path, ip.id, ip.path FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereOne("i.id"); where != "" {
		query += ` WHERE ` + where
		args = append(args, wargs...)
	}
	rows, err := cx.DB.Read.Query(query+` ORDER BY i.id, ip.id`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	bound := cx.Boundary()
	for rows.Next() {
		var dr sha256DuplicateRow
		if err := rows.Scan(&dr.ImageID, &dr.CanonicalPath, &dr.PathID, &dr.AliasPath); err != nil {
			return nil, 0, err
		}
		if bound.Excludes(dr.AliasPath) {
			continue
		}
		if total >= offset && total < offset+limit {
			window = append(window, dr)
		}
		total++
	}
	return window, total, rows.Err()
}

type markedDuplicateRow struct {
	GroupID          int64
	OriginalID       int64
	OriginalMissing  bool
	DuplicateID      int64
	MarkedAt         string
	HasTagsToCopy    bool
	HasSourcesToCopy bool
}

type relationsWalkerData struct {
	baseData
	Kind       string
	Sha256Rows []sha256DuplicateRow
	MarkedRows []markedDuplicateRow
	Total      int
	Page       int
	TotalPages int
}

func (d relationsWalkerData) HasRows() bool { return len(d.Sha256Rows) > 0 || len(d.MarkedRows) > 0 }

const duplicatesWalkerPageSize = 100

func requestedWalkerPage(r *http.Request) int {
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		return min(p, math.MaxInt/duplicatesWalkerPageSize)
	}
	return 1
}

func walkerPageOffset(r *http.Request, total int) (page, totalPages, offset int) {
	totalPages = max(1, (total+duplicatesWalkerPageSize-1)/duplicatesWalkerPageSize)
	page = min(requestedWalkerPage(r), totalPages)
	return page, totalPages, (page - 1) * duplicatesWalkerPageSize
}

func (s *Server) sha256WalkerPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	requested := (requestedWalkerPage(r) - 1) * duplicatesWalkerPageSize
	rows, total, err := duplicatePaths(r, cx, requested, duplicatesWalkerPageSize)
	page, totalPages, offset := walkerPageOffset(r, total)
	if err == nil && offset != requested {
		rows, total, err = duplicatePaths(r, cx, offset, duplicatesWalkerPageSize)
	}
	if err != nil {
		logx.Warnf("sha256 walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	s.renderTemplate(w, "relations_duplicates_sha256.html", relationsWalkerData{
		baseData:   s.base(r, "relations", "Duplicate files - "+s.booruName()),
		Kind:       "sha256",
		Sha256Rows: rows,
		Total:      total,
		Page:       page,
		TotalPages: totalPages,
	})
}

func markedDuplicatesFrom(ceiling *Ceiling) (string, []any) {
	from := ` FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		JOIN images o ON o.id = g.original_image_id
		WHERE m.image_id != g.original_image_id`
	where, args := ceiling.WhereTwo("g.original_image_id", "m.image_id")
	if where != "" {
		from += ` AND ` + where
	}
	return from, args
}

func (s *Server) markedWalkerPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	from, args := markedDuplicatesFrom(resolveCeiling(r, cx))
	var total int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		logx.Warnf("marked walker count: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	page, totalPages, offset := walkerPageOffset(r, total)
	gone := originalsGone(cx)
	out, err := db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (markedDuplicateRow, error) {
		var dr markedDuplicateRow
		var marked, originalPath string
		var originalMissing bool
		if err := rows.Scan(&dr.GroupID, &dr.OriginalID, &originalMissing, &originalPath, &dr.DuplicateID, &marked); err != nil {
			return dr, err
		}
		dr.OriginalMissing = gone(dr.OriginalID, originalMissing, originalPath)
		dr.MarkedAt = humanISOTime(marked)
		return dr, nil
	}, `SELECT g.id, g.original_image_id, o.is_missing, o.canonical_path, m.image_id, m.created_at`+from+
		` ORDER BY m.created_at DESC, g.id DESC, m.image_id LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), duplicatesWalkerPageSize, offset)...)
	if err != nil {
		logx.Warnf("marked walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	if err := annotateCopyable(cx, out); err != nil {
		logx.Warnf("marked walker copyable tags and sources: %v", err)
	}
	s.renderTemplate(w, "relations_duplicates_marked.html", relationsWalkerData{
		baseData:   s.base(r, "relations", "Duplicate images - "+s.booruName()),
		Kind:       "marked",
		MarkedRows: out,
		Total:      total,
		Page:       page,
		TotalPages: totalPages,
	})
}

// The flag misses a file removed while the watcher was off.
func originalsGone(cx *library.Gallery) func(id int64, missing bool, path string) bool {
	seen := map[int64]bool{}
	return func(id int64, missing bool, path string) bool {
		gone, ok := seen[id]
		if !ok {
			gone = missing || gallery.CanonicalGone(cx.Boundary(), path)
			seen[id] = gone
		}
		return gone
	}
}

func annotateCopyable(cx *library.Gallery, rows []markedDuplicateRow) error {
	var groupIDs []int64
	for _, dr := range rows {
		if !slices.Contains(groupIDs, dr.GroupID) {
			groupIDs = append(groupIDs, dr.GroupID)
		}
	}
	withTags, err := cx.RelationsSvc.GroupsWithTagsToCopy(groupIDs)
	if err != nil {
		return err
	}
	withSources, err := cx.RelationsSvc.GroupsWithSourcesToCopy(groupIDs)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].HasTagsToCopy = withTags[rows[i].GroupID]
		rows[i].HasSourcesToCopy = withSources[rows[i].GroupID]
	}
	return nil
}

func (s *Server) sha256WalkerRemoveOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	pathID, err := strconv.ParseInt(r.FormValue("path_id"), 10, 64)
	if err != nil {
		flashHeaderErr(w, http.StatusBadRequest, "Invalid path id.")
		return
	}
	var aliasPath string
	if err := s.db().Read.QueryRow(
		`SELECT path FROM image_paths WHERE id = ? AND is_canonical = 0`, pathID,
	).Scan(&aliasPath); err != nil {
		flashHeaderErr(w, http.StatusNotFound, "Not a non-canonical path.")
		return
	}
	res, err := s.active().DeleteCopies(context.Background(), []int64{pathID}, nil)
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(res.Kept) > 0 {
		replyKeptCopy(w, r, res.Kept[0])
		return
	}
	hxDone(w, r, fmt.Sprintf("Deleted %s.", aliasPath), "", "/relations/duplicates/sha256")
}

func (s *Server) markedWalkerDeleteOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	imageID, err := strconv.ParseInt(r.FormValue("image_id"), 10, 64)
	if err != nil {
		flashHeaderErr(w, http.StatusBadRequest, "Invalid image id.")
		return
	}
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	var originalID int64
	var originalMissing bool
	var originalPath string
	err = cx.DB.Read.QueryRow(`
		SELECT g.original_image_id, o.is_missing, o.canonical_path
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		JOIN images o ON o.id = g.original_image_id
		WHERE m.image_id = ?`, imageID,
	).Scan(&originalID, &originalMissing, &originalPath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		flashHeaderErr(w, http.StatusNotFound, fmt.Sprintf("Image #%d is not a marked duplicate.", imageID))
		return
	case err != nil:
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	case originalID == imageID:
		flashStatus(w, http.StatusConflict, fmt.Sprintf("Image #%d is its group's original, so it was kept.", imageID))
		return
	case originalMissing || gallery.CanonicalGone(cx.Boundary(), originalPath):
		flashStatus(w, http.StatusConflict, fmt.Sprintf("Image #%d's original #%d is missing, so it was kept.", imageID, originalID))
		return
	}
	res, err := cx.DeleteImages(context.Background(), []int64{imageID}, library.DeleteOptions{}, nil)
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(res.Deleted) == 0 {
		flashHeaderErr(w, http.StatusNotFound, fmt.Sprintf("Image #%d is already gone.", imageID))
		return
	}
	hxDone(w, r, fmt.Sprintf("Deleted image #%d%s.", imageID, keptChanged(res.Changed)), "", "/relations/duplicates/marked")
}

// Ceiling-filtered, so delete-all never removes a member the operator
// cannot see.
func (s *Server) markedWalkerDeleteAllPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	from, args := markedDuplicatesFrom(resolveCeiling(r, cx))
	members, err := queryMarkedMembers(cx, `SELECT m.image_id, g.original_image_id, o.is_missing, o.canonical_path`+from, args...)
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(members) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 marked duplicates.")
		return
	}
	s.goJob(w, models.JobTypeDelete, "Marked duplicate removal started.", func(ctx context.Context) {
		gone := originalsGone(cx)
		var victims []int64
		for _, m := range members {
			if !gone(m.originalID, m.originalMissing, m.originalPath) {
				victims = append(victims, m.imageID)
			}
		}
		kept := len(members) - len(victims)
		total := len(victims)
		s.jobs.Update(0, total, "removing…")
		res, err := cx.DeleteImages(ctx, victims, library.DeleteOptions{ContinuePastFailure: true}, func(done, total int) {
			s.jobs.Update(done, total, "removing…")
		})
		removed := len(res.Deleted)
		done := fmt.Sprintf("Removed %d marked duplicate%s", removed, plural(removed))
		if kept > 0 {
			done += fmt.Sprintf("; kept %d whose original is missing", kept)
		}
		done += keptChanged(res.Changed) + "."
		s.jobs.Finish(err, res.Cancelled, fmt.Sprintf("marked delete-all cancelled (%d/%d)", removed, total), done)
	})
}

type markedMember struct {
	imageID, originalID int64
	originalMissing     bool
	originalPath        string
}

func queryMarkedMembers(cx *library.Gallery, q string, args ...any) ([]markedMember, error) {
	return db.QueryAll(cx.DB.Read, db.ScanRow(func(m *markedMember) []any {
		return []any{&m.imageID, &m.originalID, &m.originalMissing, &m.originalPath}
	}), q, args...)
}
