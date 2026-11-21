package web

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

var validOrderModes = map[string]bool{
	"smallest_distance_first": true,
	"largest_file_first":      true,
	"random":                  true,
}

var validDetectors = map[string]bool{
	"phash": true,
	"tags":  true,
	"both":  true,
}

type sessionPairView struct {
	A                 sessionImageView
	B                 sessionImageView
	Distance          int
	Source            string
	Score             float64
	LeftID            int64
	SharedAncestor    int64
	AHasUniqueTags    bool
	BHasUniqueTags    bool
	AHasUniqueSources bool
	BHasUniqueSources bool
}

func (v sessionPairView) ScorePercent() int { return int(math.Round(v.Score * 100)) }

func (v sessionPairView) FromTags() bool {
	return v.Source == relations.SourceTags || v.Source == relations.SourceBoth
}

type sessionImageView struct {
	ID       int64
	Width    sql.NullInt64
	Height   sql.NullInt64
	FileSize int64
	Filename string
	FileType string
}

func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = cx.RelationsSvc.SessionOrder()
	}
	if !validOrderModes[order] {
		order = "smallest_distance_first"
	}
	if validOrderModes[r.URL.Query().Get("order")] {
		// Only a valid ?order= is saved, so a bogus one cannot replace
		// the stored mode with the fallback.
		cx.RelationsSvc.SetSessionOrder(order)
	}
	// Unlike the order, the scope is not stored: it lasts one sitting.
	tagPairs := s.tagPairsEnabled()
	detector := "phash"
	if tagPairs {
		detector = "both"
		if v := r.URL.Query().Get("detector"); validDetectors[v] {
			detector = v
		}
	}
	ceiling := resolveCeiling(r, cx)
	pinA, _ := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	pinB, _ := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	pair, counts, err := loadNextPair(cx, order, detector, ceiling, pinA, pinB)
	if err != nil {
		logx.Warnf("session next pair: %v", err)
		http.Error(w, "load pair", http.StatusInternalServerError)
		return
	}
	var leftFacts, rightFacts relationCompareFacts
	var sharedTags []tags.SharedTag
	var sharedTotal int
	if pair != nil {
		leftID := pair.LeftID
		rightID := pair.A.ID
		if leftID == pair.A.ID {
			rightID = pair.B.ID
		}
		leftFacts, rightFacts, err = loadCompareFacts(cx, leftID, rightID)
		if err != nil {
			logx.Debugf("session compare facts: %v", err)
		}
		if pair.FromTags() {
			visible, _ := cx.VisibleCount()
			shared, total, sErr := tags.SharedTags(cx.DB, leftID, rightID, sharedTagsShown, visible)
			if sErr != nil {
				logx.Debugf("session shared tags: %v", sErr)
			}
			sharedTags, sharedTotal = shared, total
		}
		if src, ok, sErr := relations.CommonDerivativeAncestor(cx.DB, pair.A.ID, pair.B.ID); sErr != nil {
			logx.Debugf("session shared ancestor: %v", sErr)
		} else if ok {
			pair.SharedAncestor = src
		}
		pair.AHasUniqueTags = s.hasUniqueTagsToCopy(cx, pair.B.ID, pair.A.ID)
		pair.BHasUniqueTags = s.hasUniqueTagsToCopy(cx, pair.A.ID, pair.B.ID)
		pair.AHasUniqueSources = s.hasUniqueSourcesToCopy(cx, pair.B.ID, pair.A.ID)
		pair.BHasUniqueSources = s.hasUniqueSourcesToCopy(cx, pair.A.ID, pair.B.ID)
	}
	s.renderTemplate(w, "relations_session.html", sessionPageData{
		baseData:        s.base(r, "relations", "Session - "+s.booruName()),
		Pair:            pair,
		Remaining:       counts.Open,
		HiddenByCeiling: counts.HiddenByCeiling,
		Skipped:         counts.Skipped,
		Ceiling:         ceiling.Level(),
		Order:           order,
		Detector:        detector,
		TagPairs:        tagPairs,
		Left:            leftFacts,
		Right:           rightFacts,
		SharedTags:      sharedTags,
		SharedTagsTotal: sharedTotal,
	})
}

const sharedTagsShown = 6

type sessionPageData struct {
	baseData
	Pair            *sessionPairView
	Remaining       int
	HiddenByCeiling int
	Skipped         int
	Ceiling         string
	Order           string
	Detector        string
	TagPairs        bool
	Left            relationCompareFacts
	Right           relationCompareFacts
	SharedTags      []tags.SharedTag
	SharedTagsTotal int
}

func loadNextPair(cx *library.Gallery, order, detector string, ceiling *Ceiling, pinA, pinB int64) (*sessionPairView, relations.QueueCounts, error) {
	var rank *int
	if r, active := ceiling.RankCeiling(); active {
		rank = &r
	}
	counts, err := cx.RelationsSvc.QueueCountsFor(detector, rank)
	if err != nil {
		return nil, counts, err
	}
	pinned := pinA > 0 && pinB > 0
	if counts.Open == 0 && !pinned {
		return nil, counts, nil
	}
	pair, err := cx.RelationsSvc.NextPair(order, detector, rank, pinA, pinB)
	if err != nil || pair == nil {
		return nil, counts, err
	}

	view := sessionPairView{
		Distance: pair.Distance, Source: pair.Source, Score: pair.Score,
		A: sessionImageView{
			ID: pair.A.ID, Width: pair.A.Width, Height: pair.A.Height,
			FileSize: pair.A.FileSize, FileType: pair.A.FileType,
			Filename: filepath.Base(pair.A.CanonicalPath),
		},
		B: sessionImageView{
			ID: pair.B.ID, Width: pair.B.Width, Height: pair.B.Height,
			FileSize: pair.B.FileSize, FileType: pair.B.FileType,
			Filename: filepath.Base(pair.B.CanonicalPath),
		},
	}
	// The larger file is the likelier original; a tag match is more
	// likely a derivative, so the older image leads - A, as the queue
	// stores a pair in id order.
	view.LeftID = view.A.ID
	if view.Source != relations.SourceTags &&
		(view.B.FileSize > view.A.FileSize ||
			(view.B.FileSize == view.A.FileSize && view.B.ID < view.A.ID)) {
		view.LeftID = view.B.ID
	}
	return &view, counts, nil
}

type relationCompareFacts struct {
	ImageID         int64
	ResolutionW     int64
	ResolutionH     int64
	FileSize        int64
	AddedAt         string
	TagCount        int
	UniqueTags      []compareTag
	UniqueTagsTotal int
	Format          string
	Collection      string
	PageCount       int
}

type compareTag struct {
	Name     string
	Category string
	Color    string
}

func loadCompareFacts(cx *library.Gallery, leftID, rightID int64) (relationCompareFacts, relationCompareFacts, error) {
	left := relationCompareFacts{ImageID: leftID}
	right := relationCompareFacts{ImageID: rightID}
	if err := scanCompareFacts(cx, leftID, &left); err != nil {
		return left, right, err
	}
	if err := scanCompareFacts(cx, rightID, &right); err != nil {
		return left, right, err
	}
	const shown = 5
	var err error
	left.UniqueTags, right.UniqueTags, left.UniqueTagsTotal, right.UniqueTagsTotal, err = loadTagDelta(cx, leftID, rightID, shown)
	return left, right, err
}

func scanCompareFacts(cx *library.Gallery, id int64, dst *relationCompareFacts) error {
	var addedAt, canonical string
	if err := cx.DB.Read.QueryRow(
		`SELECT COALESCE(width, 0), COALESCE(height, 0), file_size, ingested_at, canonical_path, series, COALESCE(page_count, 0)
		 FROM images WHERE id = ?`, id,
	).Scan(&dst.ResolutionW, &dst.ResolutionH, &dst.FileSize, &addedAt, &canonical, &dst.Collection, &dst.PageCount); err != nil {
		return err
	}
	dst.AddedAt = humanISODate(addedAt)
	if dot := strings.LastIndexByte(canonical, '.'); dot >= 0 {
		dst.Format = strings.ToLower(canonical[dot:])
	}
	dst.TagCount = countTags(cx, id)
	return nil
}

// limit caps each side's list; the totals count every tag the side holds alone.
func loadTagDelta(cx *library.Gallery, leftID, rightID int64, limit int) (left, right []compareTag, leftTotal, rightTotal int, err error) {
	type deltaRow struct {
		owner int64
		total int
		compareTag
	}
	rows, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(r *deltaRow) []any {
		return []any{&r.owner, &r.total, &r.Name, &r.Category, &r.Color}
	}), `
		WITH delta AS (
			SELECT it.tag_id, MAX(it.image_id) AS owner_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id IN (?, ?)
			  AND (tc.name IS NULL OR tc.name != 'rating')
			GROUP BY it.tag_id
			HAVING COUNT(*) = 1
		), ranked AS (
			SELECT delta.owner_id, t.name, COALESCE(tc.name, '') AS category, COALESCE(tc.color, '') AS color,
			       ROW_NUMBER() OVER (PARTITION BY delta.owner_id ORDER BY t.name) AS rn,
			       COUNT(*) OVER (PARTITION BY delta.owner_id) AS total
			FROM delta
			JOIN tags t ON t.id = delta.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
		)
		SELECT owner_id, total, name, category, color
		FROM ranked
		WHERE rn <= ?
		ORDER BY owner_id, rn`, leftID, rightID, limit)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	for _, r := range rows {
		r.Color = tags.SafeCategoryColor(r.Color)
		switch r.owner {
		case leftID:
			left = append(left, r.compareTag)
			leftTotal = r.total
		case rightID:
			right = append(right, r.compareTag)
			rightTotal = r.total
		}
	}
	return left, right, leftTotal, rightTotal, nil
}

func countTags(cx *library.Gallery, id int64) int {
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, id).Scan(&n); err != nil {
		return 0
	}
	return n
}

func (s *Server) sessionDecidePost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	a, ok := formInt64(w, r, "a")
	if !ok {
		return
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return
	}
	decision := r.FormValue("type")
	left := a
	if v, err := strconv.ParseInt(r.FormValue("left"), 10, 64); err == nil && (v == a || v == b) {
		left = v
	}
	right := a
	if left == a {
		right = b
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if decision == "skip" {
		if err := cx.RelationsSvc.SkipPair(a, b, now); err != nil {
			logx.Warnf("session skip: %v", err)
			writeRelationError(w, err)
			return
		}
		sessionRedirect(w, r)
		return
	}

	// Left is the original, the parent or the source; the symmetric kinds
	// ignore the order.
	var err error
	if decision == "duplicate" {
		err = cx.RelationsSvc.AddDuplicateOf(left, right)
	} else {
		err = cx.RelationsSvc.Add(decision, left, right)
	}
	if errors.Is(err, relations.ErrUnknownKind) {
		externalErr(w, r, "Unknown decision.", http.StatusBadRequest)
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	if err := cx.RelationsSvc.DropQueuedPair(a, b); err != nil {
		logx.Warnf("session queue drop: %v", err)
	}
	if decision == "duplicate" {
		var gid int64
		_ = cx.DB.Read.QueryRow(`
			SELECT group_id FROM dup_group_members WHERE image_id = ? LIMIT 1`, left).Scan(&gid)
		if gid > 0 {
			if r.FormValue("copy_tags") == "1" {
				if _, err := cx.RelationsSvc.CopyTagsFromDuplicatesToOriginal(gid); err != nil {
					logx.Warnf("session dup copy tags: %v", err)
				}
			}
			if r.FormValue("copy_sources") == "1" {
				if _, err := cx.RelationsSvc.CopySourcesFromDuplicatesToOriginal(gid); err != nil {
					logx.Warnf("session dup copy sources: %v", err)
				}
			}
		}
		if r.FormValue("delete") == "1" {
			if _, err := cx.DeleteImages(context.Background(), []int64{right}, library.DeleteOptions{}, nil); err != nil {
				logx.Warnf("session dup delete: %v", err)
			}
		}
	}
	sessionRedirect(w, r)
}

func (s *Server) hasUniqueTagsToCopy(cx *library.Gallery, original, dup int64) bool {
	rc := int64(0)
	if s != nil {
		if svc := s.tagSvc(); svc != nil {
			rc = svc.RatingCategoryID()
		}
	}
	var exists int
	err := cx.DB.Read.QueryRow(`
		WITH dup_imgs AS (
			SELECT member.image_id FROM dup_group_members m JOIN dup_group_members member ON member.group_id = m.group_id WHERE m.image_id = ?1
			UNION SELECT ?1
		),
		orig AS (
			SELECT COALESCE((SELECT g.original_image_id FROM dup_group_members m JOIN dup_groups g ON g.id = m.group_id WHERE m.image_id = ?2), ?2) AS id
		)
		SELECT 1
		FROM image_tags it
		JOIN dup_imgs d ON d.image_id = it.image_id
		LEFT JOIN tags t ON t.id = it.tag_id
		JOIN orig o ON 1=1
		WHERE d.image_id != o.id
		  AND it.is_implied = 0
		  AND (?3 = 0 OR t.category_id != ?3)
		  AND (NOT EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id AND s.source = '`+models.TagSourceMonbooru+`')
		       OR EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id AND s.source != '`+models.TagSourceMonbooru+`'))
		  AND NOT EXISTS (SELECT 1 FROM image_tags oit WHERE oit.image_id = o.id AND oit.tag_id = it.tag_id)
		LIMIT 1`, dup, original, rc,
	).Scan(&exists)
	return err == nil && exists == 1
}

func (s *Server) hasUniqueSourcesToCopy(cx *library.Gallery, original, dup int64) bool {
	var exists int
	err := cx.DB.Read.QueryRow(`
		WITH dup_imgs AS (
			SELECT member.image_id FROM dup_group_members m JOIN dup_group_members member ON member.group_id = m.group_id WHERE m.image_id = ?1
			UNION SELECT ?1
		),
		orig AS (
			SELECT COALESCE((SELECT g.original_image_id FROM dup_group_members m JOIN dup_groups g ON g.id = m.group_id WHERE m.image_id = ?2), ?2) AS id
		)
		SELECT 1
		FROM image_sources s
		JOIN dup_imgs d ON d.image_id = s.image_id
		JOIN orig o ON 1=1
		WHERE d.image_id != o.id
		  AND lower(s.site) != '`+contract.SourcePTR+`'
		  AND NOT EXISTS (SELECT 1 FROM image_sources os WHERE os.image_id = o.id AND os.site = s.site AND os.post_id = s.post_id)
		  AND (s.url = '' OR NOT EXISTS (SELECT 1 FROM image_sources os WHERE os.image_id = o.id AND os.url = s.url))
		LIMIT 1`, dup, original,
	).Scan(&exists)
	return err == nil && exists == 1
}

func sessionRedirect(w http.ResponseWriter, r *http.Request) {
	dest := "/relations/session?order=" + url.QueryEscape(r.FormValue("order"))
	// Carried through, or a decision inside one detector's walk drops
	// back to both.
	if d := r.FormValue("detector"); validDetectors[d] {
		dest += "&detector=" + url.QueryEscape(d)
	}
	hxRedirect(w, r, dest)
}
