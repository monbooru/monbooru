package search

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

// Sharing one tag already scores above 0, so the bare form skips scoring.
func (b *whereBuilder) buildSimilarFilter(e FilterExpr) string {
	seedID, threshold, ok := parseSimilarValue(e.Val)
	if !ok {
		return "1=0"
	}
	seed, ok := b.similaritySeed(seedID)
	if !ok || len(seed.TagIDs) == 0 {
		return "1=0"
	}
	placeholders, idArgs := db.InPlaceholders(seed.TagIDs)
	b.args = append(b.args, idArgs...)
	b.args = append(b.args, seedID)
	member := "i.id IN (SELECT it.image_id FROM image_tags it" +
		" WHERE it.tag_id IN (" + placeholders + ") AND it.image_id != ?"
	if threshold < 0 {
		return member + ")"
	}
	// Only rows sharing enough tags to reach the threshold get scored.
	if need := seed.MinShared(threshold); need > 1 {
		member += " GROUP BY it.image_id HAVING count(*) >= ?"
		b.args = append(b.args, need)
	}
	member += ")"
	// Membership first, so the score only prices rows that share something.
	scoreExpr, scoreArgs := seed.ScoreExpr("i.id", "its")
	b.args = append(b.args, scoreArgs...)
	b.args = append(b.args, threshold)
	return member + " AND " + scoreExpr + " >= ?"
}

// threshold is -1 for the bare form.
func parseSimilarValue(val string) (seedID int64, threshold float64, ok bool) {
	val = strings.TrimSpace(val)
	idPart, scorePart, scored := strings.Cut(val, "~")
	scorePart = strings.TrimSpace(scorePart)
	id, err := strconv.ParseInt(strings.TrimSpace(idPart), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, false
	}
	if !scored {
		return id, -1, true
	}
	s, err := strconv.ParseFloat(scorePart, 64)
	if err != nil || s < 0 || s > 1 {
		return 0, 0, false
	}
	return id, s, true
}

func similarityOrderClause(seed tags.OverlapSeed, order string) (string, []any) {
	dir := sqlDir(order, "DESC")
	sub, args := seed.ScoreExpr("i.id", "it")
	return "ORDER BY " + sub + " " + dir + ", i.id " + dir, args
}

func similarityRankSeed(src Source, expr Expr) (tags.OverlapSeed, bool) {
	if src.DB == nil {
		return tags.OverlapSeed{}, false
	}
	id, ok := SimilaritySeedID(expr)
	if !ok {
		return tags.OverlapSeed{}, false
	}
	visible, ok := src.Tallies.VisibleCount()
	if !ok {
		return tags.OverlapSeed{}, false
	}
	seed, err := tags.LoadOverlapSeed(src.DB, id, visible)
	if err != nil || len(seed.TagIDs) == 0 {
		return tags.OverlapSeed{}, false
	}
	return seed, true
}

// SimilaritySeedID skips negated terms: ranking by an excluded seed is
// never meant.
func SimilaritySeedID(expr Expr) (int64, bool) {
	switch e := expr.(type) {
	case AndExpr:
		if id, ok := SimilaritySeedID(e.Left); ok {
			return id, true
		}
		return SimilaritySeedID(e.Right)
	case OrExpr:
		if id, ok := SimilaritySeedID(e.Left); ok {
			return id, true
		}
		return SimilaritySeedID(e.Right)
	case FilterExpr:
		if e.Key != "similar" {
			return 0, false
		}
		id, _, ok := parseSimilarValue(e.Val)
		return id, ok
	}
	return 0, false
}

// HasSimilarTerm reports whether expr carries a positive similar: term.
func HasSimilarTerm(expr Expr) bool {
	_, ok := SimilaritySeedID(expr)
	return ok
}

// A list at the cap is partial, so it stays out of the cache. ctx is the
// only bound on this scan: no fast counter recognises similar:.
func similarityMatchIDs(ctx context.Context, src Source, q Query) []int64 {
	database := src.DB
	gen := src.Cache.gen()
	key := q.listKey(src)
	seed, ok := similarityRankSeed(src, q.Expr)
	if !ok {
		return nil
	}
	// One scored pass per key: a loser renders without prev/next.
	if key != "" {
		if !src.Cache.tryAcquireFan(key) {
			return nil
		}
		defer src.Cache.releaseFan(key)
	}
	driverLegs, _ := pickAndDriverTag(database, q.Expr, false)
	where, args, _, _ := visibleWhere(src, q.Expr, driverLegs)
	ids := fanSimilarityIDs(ctx, src, seed, q.Order, where, args)
	if len(ids) < adjacencyCacheMaxIDs {
		src.Cache.set(key, ids, gen)
	}
	return ids
}

// Scored in Go off the cached tallies: the SQL score re-derives each
// candidate's total through both tag joins.
func fanSimilarityIDs(ctx context.Context, src Source, seed tags.OverlapSeed, order, where string, args []any) []int64 {
	database := src.DB
	totals, err := src.Tallies.CountedTagTotals(ctx, seed.MaxUsage)
	if err != nil {
		orderClause, orderArgs := similarityOrderClause(seed, order)
		return fetchSortedMatchIDs(ctx, database, "", where, args, orderClause, orderArgs, adjacencyCacheMaxIDs)
	}
	ids, err := db.QueryIDsContext(ctx, database.Read,
		"SELECT i.id FROM images i WHERE "+where+" LIMIT ?",
		append(slices.Clone(args), adjacencyCacheMaxIDs)...)
	if err != nil || len(ids) == 0 {
		return nil
	}
	shared, err := sharedTagCounts(ctx, database, seed)
	if err != nil {
		return nil
	}

	type ranked struct {
		id    int64
		score float64
	}
	rows := make([]ranked, len(ids))
	for i, id := range ids {
		// SQL scores a candidate with no counted tags NULL, which sorts
		// below every number; -1 reproduces that.
		score := -1.0
		if total := totals.Total(id); total > 0 {
			score = tags.OverlapScore(int(shared[id]), len(seed.TagIDs), int(total))
		}
		rows[i] = ranked{id: id, score: score}
	}
	asc := order == "asc"
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return (rows[i].score < rows[j].score) == asc
		}
		return (rows[i].id < rows[j].id) == asc
	})
	for i, r := range rows {
		ids[i] = r.id
	}
	return ids
}

func sharedTagCounts(ctx context.Context, database *db.DB, seed tags.OverlapSeed) (map[int64]int32, error) {
	placeholders, args := db.InPlaceholders(seed.TagIDs)
	rows, err := database.Read.QueryContext(ctx,
		"SELECT it.image_id, count(*) FROM image_tags it WHERE it.tag_id IN ("+
			placeholders+") GROUP BY it.image_id", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int32)
	for rows.Next() {
		var id int64
		var n int32
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
