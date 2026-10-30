package search

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

// fastTagTotal answers exactly or with an upper bound, never under: an
// over-count only leaves empty trailing pages.
func fastTagTotal(src Source, expr Expr) (int, bool) {
	if n, ok := fastCountCeiling(src, expr); ok {
		return n, true
	}
	switch e := expr.(type) {
	case TagExpr:
		return fastCountTag(src, e)
	case NotExpr:
		return fastCountNot(src, e)
	case AndExpr:
		return fastCountAnd(src, e)
	case OrExpr:
		return fastCountOr(src, e)
	case FilterExpr:
		return fastCountFilter(src, e)
	}
	return 0, false
}

// Whether an answer fastTagTotal gave is exact rather than a bound.
func fastTotalExact(expr Expr) bool {
	if user, _, ok := extractCeilingShape(expr); ok && user == nil {
		return true
	}
	switch e := expr.(type) {
	case TagExpr:
		return e.Wildcard == ""
	case NotExpr:
		return true
	case FilterExpr:
		switch e.Key {
		case "cat":
			return false
		case "rating":
			return strings.EqualFold(e.Val, contract.RatingLevels[len(contract.RatingLevels)-1])
		}
		return true
	}
	return false
}

// Unlike fastTagTotal, AND and OR are not gated here: the bucket decision
// needs exactly the small intersections that gate hides.
func adjacencyTotalEstimate(src Source, expr Expr) (int, bool) {
	// The ceiling only narrows, so the user part bounds the set; its lone
	// NOT rating:X leaves have no count of their own.
	if user, _, ok := extractCeilingShape(expr); ok && user != nil {
		return adjacencyTotalEstimate(src, user)
	}
	switch e := expr.(type) {
	case AndExpr:
		return fastCountBinary(src, e.Left, e.Right, adjacencyTotalEstimate, minInt)
	case OrExpr:
		sum, ok := fastCountBinary(src, e.Left, e.Right, adjacencyTotalEstimate, addInts)
		if !ok {
			return 0, false
		}
		return capToVisible(src, sum)
	}
	return fastTagTotal(src, expr)
}

// Counts rating_rank, exact for an image with several rating tags, which
// subtracting usage_counts takes out once per level. A lone tag ANDed on
// counts exactly in Execute: fastCountAnd's min would advertise phantom pages.
func fastCountCeiling(src Source, expr Expr) (int, bool) {
	user, rank, ok := extractCeilingShape(expr)
	if !ok || user != nil {
		return 0, false
	}
	// Pinned: rating_rank has five values, which the sampled sqlite_stat1
	// underestimates, and unpinned the planner counts through
	// idx_images_missing, reading every visible row.
	return countRow(src,
		`SELECT COUNT(*) FROM images INDEXED BY idx_images_rating_rank_visible
		 WHERE is_missing = 0 AND rating_rank <= ?`, rank,
	)
}

// extractCeilingShape turns this chain back into one rating_rank comparison,
// so it must keep excluding every level above the ceiling.
func ApplyCeiling(expr Expr, level string) Expr {
	rank := contract.RatingRank(level)
	if rank < 0 || rank >= len(contract.RatingLevels)-1 {
		return expr
	}
	var ce Expr
	for i := rank + 1; i < len(contract.RatingLevels); i++ {
		not := NotExpr{Expr: FilterExpr{Key: "rating", Val: contract.RatingLevels[i]}}
		if ce == nil {
			ce = not
		} else {
			ce = AndExpr{Left: ce, Right: not}
		}
	}
	if expr == nil {
		return ce
	}
	return AndExpr{Left: expr, Right: ce}
}

// HiddenByCeiling counts exactly the images the query text matches that
// level hides.
func HiddenByCeiling(ctx context.Context, src Source, text, level string) (int, error) {
	rank := contract.RatingRank(level)
	if rank < 0 || rank >= len(contract.RatingLevels)-1 {
		return 0, nil
	}
	key := adjacencyKey(text, "", "", 0, level, src.LookupBackends)
	if entry, ok := src.Cache.hiddenCount(key); ok {
		if entry.overran {
			return 0, context.DeadlineExceeded
		}
		return entry.n, nil
	}
	gen := src.Cache.gen()
	where, args := exactCountWhere(src, Parse(text))
	var n int
	if err := src.DB.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM images i WHERE "+where+" AND i.rating_rank > ?",
		append(args, rank)...,
	).Scan(&n); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			src.Cache.setHiddenCount(key, hiddenCountEntry{overran: true}, gen)
		}
		return 0, err
	}
	src.Cache.setHiddenCount(key, hiddenCountEntry{n: n}, gen)
	return n, nil
}

// A lone literal drives from image_tags rather than walking every visible
// row with an EXISTS.
func exactCountWhere(src Source, expr Expr) (string, []any) {
	driverLegs, _ := pickAndDriverTag(src.DB, expr, true)
	where, args, _, _ := visibleWhere(src, expr, driverLegs)
	return where, args
}

// Only a lone literal tag, bare or category-qualified, counts exactly on
// its own; a wildcard or an AND is already a bound without the ceiling,
// and its exact COUNT can take seconds.
func ceilingWrapsLoneLiteral(expr Expr) bool {
	user, _, ok := extractCeilingShape(expr)
	if !ok {
		return false
	}
	switch u := user.(type) {
	case TagExpr:
		return u.Wildcard == ""
	case FilterExpr:
		return u.Val != "" && !strings.ContainsRune(u.Val, '*') && !searchkw.IsKeyword(u.Key)
	}
	return false
}

// Only negations excluding every level above some rank equal rating_rank
// <= rank; any other rating negation stays in user with its own meaning.
func extractCeilingShape(expr Expr) (Expr, int, bool) {
	if expr == nil {
		return nil, 0, false
	}
	excluded := map[string]bool{}
	WalkAndedLeaves(expr, func(leaf Expr) {
		if level, ok := ratingNegation(leaf); ok {
			excluded[level] = true
		}
	})
	top := len(contract.RatingLevels) - 1
	rank := top
	for rank >= 0 && excluded[contract.RatingLevels[rank]] {
		rank--
	}
	if rank == top {
		return nil, 0, false
	}
	return peelCeilingChain(expr, rank), rank, true
}

func ratingNegation(expr Expr) (string, bool) {
	if n, ok := expr.(NotExpr); ok {
		if f, ok := n.Expr.(FilterExpr); ok && f.Key == "rating" {
			return strings.ToLower(f.Val), true
		}
	}
	return "", false
}

func peelCeilingChain(expr Expr, rank int) Expr {
	switch v := expr.(type) {
	case NotExpr:
		if level, ok := ratingNegation(v); ok && contract.RatingRank(level) > rank {
			return nil
		}
		return expr
	case AndExpr:
		left := peelCeilingChain(v.Left, rank)
		right := peelCeilingChain(v.Right, rank)
		switch {
		case left == nil && right == nil:
			return nil
		case left == nil:
			return right
		case right == nil:
			return left
		default:
			return AndExpr{Left: left, Right: right}
		}
	}
	return expr
}

// Under this many rows the exact COUNT is fast enough, so the bounded
// shortcuts stand down.
const fastApproxThreshold = 50000

// Five default pages. A rank costs its depth, prev/next strays a page or
// two from where the operator arrived, and deeper than that the URL's
// page is already right.
const rankInQueryMaxRank = 200

func fastCountTag(src Source, t TagExpr) (int, bool) {
	if t.Tag == "" {
		return 0, false
	}
	canonIDs, usage, ok := resolveDriverCanonicals(src.DB, t)
	if !ok {
		return 0, false
	}
	if len(canonIDs) == 0 {
		return 0, true
	}
	if len(canonIDs) == 1 {
		return int(usage), true
	}
	// Same name in two categories: a sum over-counts, and an exact name
	// wants an exact count.
	if t.Wildcard == "" {
		return 0, false
	}
	n := int(usage)
	if n < fastApproxThreshold {
		return 0, false
	}
	// Summed per tag, a broad pattern can total several times the library.
	return capToVisible(src, n)
}

// Exact counts only: visible minus an upper bound would under-count and
// hide real pages.
func fastCountNot(src Source, e NotExpr) (int, bool) {
	inner, ok := e.Expr.(TagExpr)
	if !ok || inner.Wildcard != "" || inner.Tag == "" {
		return 0, false
	}
	used, ok := fastCountTag(src, inner)
	if !ok {
		return 0, false
	}
	visible, ok := src.Tallies.VisibleCount()
	if !ok {
		return 0, false
	}
	return max(visible-used, 0), true
}

func fastCountBinary(src Source, l, r Expr, recurse func(Source, Expr) (int, bool), combine func(int, int) int) (int, bool) {
	lv, ok := recurse(src, l)
	if !ok {
		return 0, false
	}
	rv, ok := recurse(src, r)
	if !ok {
		return 0, false
	}
	return combine(lv, rv), true
}

func addInts(a, b int) int { return a + b }

func minInt(a, b int) int { return min(a, b) }

func capToVisible(src Source, sum int) (int, bool) {
	v, ok := src.Tallies.VisibleCount()
	if !ok {
		return 0, false
	}
	return min(sum, v), true
}

func fastCountAnd(src Source, e AndExpr) (int, bool) {
	l, lOK := fastTagTotal(src, e.Left)
	r, rOK := fastTagTotal(src, e.Right)
	// Every fast count is an upper bound, so either leg's 0 is exact.
	if lOK && l == 0 || rOK && r == 0 {
		return 0, true
	}
	if !lOK || !rOK || min(l, r) < fastApproxThreshold {
		return 0, false
	}
	return min(l, r), true
}

func fastCountOr(src Source, e OrExpr) (int, bool) {
	sum, ok := fastCountBinary(src, e.Left, e.Right, fastTagTotal, addInts)
	if !ok || sum < fastApproxThreshold {
		return 0, false
	}
	return capToVisible(src, sum)
}

var fastCountFilters = map[string]func(Source, FilterExpr) (int, bool){
	"cat":        fastCountCat,
	"generated":  fastCountGenerated,
	"rating":     fastCountRating,
	"tagged":     fastCountTagged,
	"autotagged": fastCountTagged,
	"inbox":      fastCountInbox,
	"ai":         fastCountAI,
	"folder":     fastCountFolder,
	"lookup":     fastCountLookup,
}

// lookup:never is visible minus looked-up: its NOT EXISTS walks the whole
// library on a gallery that never ran a lookup. The other values' EXISTS
// is already selective.
func fastCountLookup(src Source, e FilterExpr) (int, bool) {
	if strings.ToLower(e.Val) != "never" {
		return 0, false
	}
	visible, ok := src.Tallies.VisibleCount()
	if !ok {
		return 0, false
	}
	recorded, ok := countRow(src,
		`SELECT COUNT(*) FROM (SELECT DISTINCT image_id FROM image_lookups) l
		 JOIN images i ON i.id = l.image_id AND i.is_missing = 0`,
	)
	if !ok {
		return 0, false
	}
	return max(visible-recorded, 0), true
}

func fastCountCat(src Source, e FilterExpr) (int, bool) {
	n, ok := countRow(src,
		`SELECT COALESCE(SUM(usage_count), 0) FROM tags
		 WHERE is_alias = 0
		   AND category_id = (SELECT id FROM tag_categories WHERE name = ?)`,
		strings.ToLower(e.Val),
	)
	if !ok || n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

func fastCountFilter(src Source, e FilterExpr) (int, bool) {
	if e.Val == "" {
		return 0, false
	}
	if fn, ok := fastCountFilters[e.Key]; ok {
		return fn(src, e)
	}
	if searchkw.IsKeyword(e.Key) {
		return 0, false
	}
	if _, ok, err := tags.CategoryIDByName(src.DB, e.Key); !ok || err != nil {
		return 0, false
	}
	_, usage, ok := resolveFilterDriverCanonicals(src.DB, e.Key, tags.NormalizeTagName(e.Val))
	if !ok {
		return 0, false
	}
	return int(usage), true
}

// usage_count bounds rating:X from above, exactly for the top level; an
// image carrying two ratings only over-counts.
func fastCountRating(src Source, e FilterExpr) (int, bool) {
	level := strings.ToLower(e.Val)
	rank := contract.RatingRank(level)
	if rank < 0 {
		return 0, true
	}
	var n int
	err := src.DB.Read.QueryRow(
		`SELECT t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0 AND t.name = ?`,
		level,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	// Zero and the top level are exact; other levels are bounds and wait
	// for the threshold.
	if n == 0 || rank == len(contract.RatingLevels)-1 {
		return n, true
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

// '0' sorts right after '/', so [X/, X0) holds exactly the paths under X.
func fastCountFolder(src Source, e FilterExpr) (int, bool) {
	rangeLo := e.Val + "/"
	rangeHi := e.Val + "0"
	return countRow(src,
		`SELECT (
		     (SELECT COUNT(*) FROM images
		        WHERE folder_path = ? COLLATE NOCASE AND is_missing = 0)
		   + (SELECT COUNT(*) FROM images INDEXED BY idx_images_folder_nocase_visible
		        WHERE folder_path >= ? COLLATE NOCASE
		          AND folder_path < ? COLLATE NOCASE
		          AND is_missing = 0)
		 )`,
		e.Val, rangeLo, rangeHi,
	)
}

// The LIKE-OR matches no index, so the few distinct source_type values
// are matched in Go and the count takes an IN over them.
func fastCountAI(src Source, e FilterExpr) (int, bool) {
	parts := aiParts(e.Val)
	if len(parts) < 2 {
		return 0, false
	}
	present, err := db.QueryStrings(src.DB.Read, `SELECT DISTINCT source_type FROM images`)
	if err != nil {
		return 0, false
	}
	var matching []string
	for _, s := range present {
		if slices.ContainsFunc(parts, func(p string) bool { return aiValueMatches(s, p) }) {
			matching = append(matching, s)
		}
	}
	if len(matching) == 0 {
		return 0, true
	}
	placeholders, args := db.InPlaceholders(matching)
	return countRow(src,
		`SELECT COUNT(*) FROM images WHERE source_type IN (`+placeholders+`) AND is_missing = 0`,
		args...,
	)
}

// Visible minus the cached untagged count; the NOT EXISTS behind it is
// too slow to run per search.
func fastCountTagged(src Source, e FilterExpr) (int, bool) {
	if val, ok := searchkw.ParseBool(e.Val); !ok || !val {
		return 0, false
	}
	visible, ok := src.Tallies.VisibleCount()
	if !ok {
		return 0, false
	}
	var untagged int
	if e.Key == "autotagged" {
		untagged, ok = src.Tallies.AutoUntaggedVisibleCount()
	} else {
		untagged, ok = src.Tallies.UntaggedVisibleCount()
	}
	if !ok {
		return 0, false
	}
	return max(visible-untagged, 0), true
}

// Ungated: idx_images_inbox_visible is exact at any size.
func fastCountInbox(src Source, e FilterExpr) (int, bool) {
	val, ok := searchkw.ParseBool(e.Val)
	if !ok {
		// The filter emits 1=0 for a non-boolean, so zero is exact.
		return 0, true
	}
	target := 0
	if val {
		target = 1
	}
	return countRow(src, `SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = ?`, target)
}

// Seeks the genhash indexes instead of an EXISTS per visible image.
// UNION, not UNION ALL: an image can carry both metadata kinds.
func fastCountGenerated(src Source, e FilterExpr) (int, bool) {
	return countRow(src,
		`SELECT COUNT(*) FROM (
		     SELECT sm.image_id FROM sd_metadata sm
		       JOIN images i ON i.id = sm.image_id
		       WHERE sm.generation_hash = ? AND i.is_missing = 0
		     UNION
		     SELECT cm.image_id FROM comfyui_metadata cm
		       JOIN images i ON i.id = cm.image_id
		       WHERE cm.generation_hash = ? AND i.is_missing = 0
		 )`,
		e.Val, e.Val,
	)
}

func countRow(src Source, query string, args ...any) (int, bool) {
	var n int
	err := src.DB.Read.QueryRow(query, args...).Scan(&n)
	return n, err == nil
}
