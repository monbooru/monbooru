package search

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tags"
)

const postingChunk = 128

// Past this many candidates the rest of the query rejected, the walk is
// reading a list the sort index would serve better.
var walkMaxCandidates = 20000

// postingList is one tag's images from a position on, in walk order.
type postingList struct {
	tagID int64
	up    bool
	buf   []int64
	ended bool // nothing lies past buf
}

// seek moves to the first image at or past target in walk order.
func (p *postingList) seek(ctx context.Context, database *db.DB, target int64) (int64, bool, error) {
	i := sort.Search(len(p.buf), func(i int) bool {
		if p.up {
			return p.buf[i] >= target
		}
		return p.buf[i] <= target
	})
	p.buf = p.buf[i:]
	if len(p.buf) > 0 {
		return p.buf[0], true, nil
	}
	if p.ended {
		return 0, false, nil
	}
	cmpOp, dir := "<=", "DESC"
	if p.up {
		cmpOp, dir = ">=", "ASC"
	}
	ids, err := db.QueryIDsContext(ctx, database.Read,
		`SELECT image_id FROM image_tags INDEXED BY idx_image_tags_tag_image
		 WHERE tag_id = ? AND image_id `+cmpOp+` ? ORDER BY image_id `+dir+` LIMIT ?`,
		p.tagID, target, postingChunk)
	if err != nil {
		return 0, false, fmt.Errorf("tag walk: %w", err)
	}
	p.buf, p.ended = ids, len(ids) < postingChunk
	if len(ids) == 0 {
		return 0, false, nil
	}
	return ids[0], true, nil
}

// tagWalk reads a newest-sort tag query off idx_image_tags_tag_image, whose
// per-tag id order is ingest order: one leg per ANDed term, the union of
// its canonicals' lists, stepped together until every leg agrees on an id.
type tagWalk struct {
	database *db.DB
	legs     [][]*postingList
	up       bool
	pos      int64
}

// from is the first image the walk may yield, in walk order.
func newTagWalk(database *db.DB, legs [][]int64, up bool, from int64) *tagWalk {
	w := &tagWalk{database: database, up: up, pos: from}
	for _, ids := range legs {
		leg := make([]*postingList, len(ids))
		for i, id := range ids {
			leg[i] = &postingList{tagID: id, up: up}
		}
		w.legs = append(w.legs, leg)
	}
	return w
}

func (w *tagWalk) seekLeg(ctx context.Context, leg []*postingList, target int64) (int64, bool, error) {
	var head int64
	found := false
	for _, p := range leg {
		id, ok, err := p.seek(ctx, w.database, target)
		if err != nil {
			return 0, false, err
		}
		if ok && (!found || (w.up && id < head) || (!w.up && id > head)) {
			head, found = id, true
		}
	}
	return head, found, nil
}

func (w *tagWalk) next(ctx context.Context) (int64, bool, error) {
	target := w.pos
	for {
		agreed := true
		for _, leg := range w.legs {
			head, ok, err := w.seekLeg(ctx, leg, target)
			if err != nil || !ok {
				return 0, false, err
			}
			if head != target {
				target, agreed = head, false
			}
		}
		if agreed {
			if w.up {
				w.pos = target + 1
			} else {
				w.pos = target - 1
			}
			return target, true, nil
		}
	}
}

// walkPage reads up to limit rows matching where, in walk order; ok is
// false when the walk gave up and the caller should read the page by SQL.
func walkPage(ctx context.Context, database *db.DB, w *tagWalk, where string, args []any, limit int) ([]models.Image, bool, error) {
	var out []models.Image
	seen, want := 0, limit
	for len(out) < limit {
		batch := make([]int64, 0, want)
		for len(batch) < want {
			id, ok, err := w.next(ctx)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				break
			}
			batch = append(batch, id)
		}
		if len(batch) == 0 {
			break
		}
		if seen += len(batch); seen > walkMaxCandidates {
			return nil, false, nil
		}
		rows, err := db.QueryAllContext(ctx, database.Read, scanImageRow,
			"SELECT "+models.ImageRowColumns+" FROM images i WHERE i.id IN ("+inlineIDs(batch)+") AND "+where, args...)
		if err != nil {
			return nil, false, fmt.Errorf("tag walk rows: %w", err)
		}
		slices.SortFunc(rows, func(a, b models.Image) int {
			if w.up {
				return cmp.Compare(a.ID, b.ID)
			}
			return cmp.Compare(b.ID, a.ID)
		})
		out = append(out, rows...)
		if len(batch) < want {
			break
		}
		want = min(want*2, 1024)
	}
	return out[:min(len(out), limit)], true, nil
}

// A leg reads one list per tag before its first match; past this many the
// sort index finds a page sooner.
const walkMaxLegTags = 600

// tagWalkLegs resolves each of q's ANDed tag terms - a literal or wildcard
// tag, bare or category-qualified, a cat:, or an OR of those - to its
// canonical ids, one leg each; ok is false when q is no walk.
func tagWalkLegs(src Source, q Query) ([][]int64, bool) {
	if src.DB == nil || (q.Sort != "" && q.Sort != "newest") || containsMissingFilter(q.Expr) {
		return nil, false
	}
	visible, _ := src.Tallies.VisibleCount()
	var legs [][]int64
	failed := false
	WalkAndedLeaves(q.Expr, func(e Expr) {
		ids, usage, ok, err := walkLegIDs(src.DB, e)
		failed = failed || err != nil
		// Several tags whose usage adds up to the library cover most of it,
		// and the sort index fills a page within a few rows.
		dense := len(ids) > 1 && usage >= int64(visible)
		if ok && len(ids) <= walkMaxLegTags && !dense {
			legs = append(legs, ids)
		}
	})
	return legs, !failed && len(legs) > 0
}

// walkLegIDs' ok is false for a term that is no leg; the walk leaves it to
// the per-candidate check.
func walkLegIDs(database *db.DB, e Expr) (ids []int64, usage int64, ok bool, err error) {
	switch v := e.(type) {
	case TagExpr:
		ids, usage, ok := resolveDriverCanonicals(database, v)
		return ids, usage, true, resolveErr(ok)
	case FilterExpr:
		if v.Key == "cat" {
			ids, usage, ok := drainCanonicalUsage(database,
				`SELECT t.id, t.usage_count FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
				 WHERE tc.name = ? AND t.is_alias = 0`, strings.ToLower(v.Val))
			return ids, usage, true, resolveErr(ok)
		}
		if v.Val == "" || searchkw.IsKeyword(v.Key) {
			return nil, 0, false, nil
		}
		if _, ok, err := tags.CategoryIDByName(database, v.Key); !ok || err != nil {
			return nil, 0, false, err
		}
		ids, usage, ok := resolveFilterDriverCanonicals(database, v.Key, tags.NormalizeTagName(v.Val))
		return ids, usage, true, resolveErr(ok)
	case OrExpr:
		left, lu, lok, err := walkLegIDs(database, v.Left)
		if !lok || err != nil {
			return nil, 0, false, err
		}
		right, ru, rok, err := walkLegIDs(database, v.Right)
		if !rok || err != nil {
			return nil, 0, false, err
		}
		ids := slices.Concat(left, right)
		slices.Sort(ids)
		return slices.Compact(ids), lu + ru, true, nil
	}
	return nil, 0, false, nil
}

func resolveErr(ok bool) error {
	if !ok {
		return errors.New("tag walk: resolve canonicals")
	}
	return nil
}

// walkAdjacent steps the walk once each way from currentID; walked is false
// when either side gave up.
func walkAdjacent(ctx context.Context, src Source, q Query, legs [][]int64, currentID int64) (prev, next *int64, walked bool, err error) {
	where, args := buildWherePerRow(q.Expr, src)
	step := func(up bool) (*int64, bool, error) {
		w := newTagWalk(src.DB, legs, up, walkStart(currentID, up))
		rows, ok, err := walkPage(ctx, src.DB, w, where, args, 1)
		if err != nil || !ok || len(rows) == 0 {
			return nil, ok, err
		}
		return &rows[0].ID, true, nil
	}
	// Newest first, the previous image is the newer one.
	newer := q.Order != "asc"
	if prev, walked, err = step(newer); err != nil || !walked {
		return nil, nil, false, err
	}
	if next, walked, err = step(!newer); err != nil || !walked {
		return nil, nil, false, err
	}
	return prev, next, true, nil
}

// walkStart is the first image a walk from cursor may yield, or the
// list's end when there is no cursor.
func walkStart(cursor int64, up bool) int64 {
	switch {
	case cursor == 0 && up:
		return 1
	case cursor == 0:
		return math.MaxInt64
	case up:
		return cursor + 1
	}
	return cursor - 1
}
