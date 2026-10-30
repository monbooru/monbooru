package search

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

func andDefaultVisible(where string, hasMissingFilter bool) string {
	if hasMissingFilter {
		return where
	}
	if where == "" {
		return "i.is_missing = 0"
	}
	return where + " AND i.is_missing = 0"
}

// '0' sorts right after '/', so [val+"/", val+"0") holds exactly the
// subfolders; starting the range at val would take in siblings like
// "anime-2024".
func detectPureFolder(expr Expr) (active bool, eq, lo, hi string) {
	f, ok := expr.(FilterExpr)
	if !ok || f.Key != "folder" || f.Val == "" {
		return false, "", "", ""
	}
	return true, f.Val, f.Val + "/", f.Val + "0"
}

// Unpinned, the planner walks idx_images_missing and temp-sorts the
// visible set for ORDER BY.
func sortIndexHint(expr Expr, sort string, hasMissingFilter, ceilingRewrote bool) string {
	if h := columnFilterIndexHint(expr, sort); h != "" {
		return h
	}
	if hasMissingFilter || (expr != nil && !isPureTagExpr(expr)) {
		return ""
	}
	switch sort {
	case "filesize":
		if ceilingRewrote {
			return " INDEXED BY idx_images_filesize_rating_visible"
		}
		return " INDEXED BY idx_images_filesize_visible"
	case "", "newest":
		if ceilingRewrote {
			return " INDEXED BY idx_images_ingested_rating_visible"
		}
		return " INDEXED BY idx_images_ingested_visible"
	}
	return ""
}

// WalkLeaves stops at the first leaf visit answers false for, and reports
// whether it got through.
func WalkLeaves(expr Expr, visit func(Expr) bool) bool {
	switch e := expr.(type) {
	case AndExpr:
		return WalkLeaves(e.Left, visit) && WalkLeaves(e.Right, visit)
	case OrExpr:
		return WalkLeaves(e.Left, visit) && WalkLeaves(e.Right, visit)
	case NotExpr:
		return WalkLeaves(e.Expr, visit)
	}
	return visit(expr)
}

// A nil expr reaches pred as a leaf, and every pred here answers false
// for it: allLeaves(nil) is false, not vacuously true.
func anyLeaf(expr Expr, pred func(Expr) bool) bool {
	return !WalkLeaves(expr, func(e Expr) bool { return !pred(e) })
}

func allLeaves(expr Expr, pred func(Expr) bool) bool { return WalkLeaves(expr, pred) }

// Pinning the sort index pays when every leaf is a cheap per-row test; a leaf
// with a selective index of its own would lose its seek to a full walk.
func isPureTagExpr(expr Expr) bool {
	return allLeaves(expr, func(e Expr) bool {
		switch v := e.(type) {
		case TagExpr:
			return true
		case FilterExpr:
			switch v.Key {
			case "cat", "rating", "tagged", "autotagged", "stale", "inbox",
				"width", "height", "date", "ratio", "pages", "tagcount":
				return true
			}
			return !searchkw.IsKeyword(v.Key)
		}
		return false
	})
}

func containsMissingFilter(expr Expr) bool {
	return anyLeaf(expr, func(e Expr) bool {
		v, ok := e.(FilterExpr)
		return ok && v.Key == "missing"
	})
}

// The sort-index pins are for values that match most of the library,
// where walking the sort index and testing each row beats the planner's
// idx_images_missing walk plus a temp sort.
func columnFilterIndexHint(expr Expr, sort string) string {
	sortHint := func() string {
		switch sort {
		case "filesize":
			return " INDEXED BY idx_images_filesize_visible"
		default:
			return " INDEXED BY idx_images_ingested_visible"
		}
	}
	// A workflow term matches few rows, so its negation matches most.
	if n, ok := expr.(NotExpr); ok {
		if f, ok := n.Expr.(FilterExpr); ok && f.Key == "comfyui" && f.Val != "" {
			return sortHint()
		}
		return ""
	}
	f, ok := expr.(FilterExpr)
	if !ok || f.Val == "" {
		return ""
	}
	switch f.Key {
	case "fav":
		// The index is partial on is_favorited = 1, so only fav:true can
		// use it.
		if strings.EqualFold(f.Val, "true") {
			return " INDEXED BY idx_images_favorited_visible"
		}
	case "type", "mime":
		// Common lists like type:image cover most rows, and the per-row
		// file_type test is cheap.
		return sortHint()
	case "ai":
		// none is the schema default, so it matches most rows.
		if strings.ToLower(f.Val) == "none" {
			return sortHint()
		}
	case "source", "upgrade":
		// none matches most of a library with few origins, and its
		// anti-join probe is covered. A site label must keep its
		// idx_image_sources_site seek.
		if strings.ToLower(f.Val) == "none" {
			return sortHint()
		}
	case "lookup":
		// never is the same anti-join. off stays unpinned: its OR reads two
		// columns the sort index lacks, so a pinned walk fetches every row.
		if strings.ToLower(f.Val) == "never" {
			return sortHint()
		}
	case "relation":
		// The named kinds seek one table and match few rows.
		switch strings.ToLower(f.Val) {
		case "collection", "any", "none":
			return sortHint()
		}
	}
	return ""
}

// Folder filters count: a popular root matches as much of the library as a tag.
func containsTagPredicate(expr Expr) bool {
	return anyLeaf(expr, func(e Expr) bool {
		switch v := e.(type) {
		case TagExpr:
			return true
		case FilterExpr:
			switch v.Key {
			case "cat", "tagged", "autotagged", "stale", "folder", "folderonly":
				return true
			}
			return !searchkw.IsKeyword(v.Key)
		}
		return false
	})
}

// Past this many image_tags rows, materialising a driver leg costs more
// than the EXISTS it replaces.
const andDriverThreshold = 50000

// Slack for id order drifting from ingested_at order and for gaps in the
// intersection near the recent end.
const driverIDBoundMargin = 100

// At density 1/20 or more, the page*limit*driverIDBoundMargin window
// holds about five times the matches the page needs.
const driverIDBoundDensityCutoff = 20

func collectAndedTags(expr Expr) []TagExpr {
	var out []TagExpr
	WalkAndedLeaves(expr, func(e Expr) {
		if v, ok := e.(TagExpr); ok && v.Tag != "" {
			out = append(out, v)
		}
	})
	return out
}

// WalkAndedLeaves visits only what every match must satisfy: an Or or Not
// node is handed to visit whole.
func WalkAndedLeaves(expr Expr, visit func(Expr)) {
	if v, ok := expr.(AndExpr); ok {
		WalkAndedLeaves(v.Left, visit)
		WalkAndedLeaves(v.Right, visit)
		return
	}
	visit(expr)
}

// withoutAndedLeaves is nil when drop takes every leaf.
func withoutAndedLeaves(expr Expr, drop func(Expr) bool) Expr {
	if v, ok := expr.(AndExpr); ok {
		left, right := withoutAndedLeaves(v.Left, drop), withoutAndedLeaves(v.Right, drop)
		switch {
		case left == nil:
			return right
		case right == nil:
			return left
		}
		return AndExpr{Left: left, Right: right}
	}
	if expr == nil || drop(expr) {
		return nil
	}
	return expr
}

// A wildcard's many canonicals or 3+ ANDed tags leave the cursor no seekable
// leg, so it temp-sorts a broad match set; a lone exact tag keeps its seek.
func expensiveAdjacencyTags(expr Expr) bool {
	if len(collectAndedTags(expr)) >= 3 {
		return true
	}
	return anyLeaf(expr, func(e Expr) bool {
		v, ok := e.(TagExpr)
		return ok && v.Wildcard != ""
	})
}

// The key may name no category; resolving the leg weeds those out.
func collectAndedFilterLeaves(expr Expr) []FilterExpr {
	var out []FilterExpr
	WalkAndedLeaves(expr, func(e Expr) {
		v, ok := e.(FilterExpr)
		if !ok || v.Val == "" || searchkw.IsKeyword(v.Key) {
			return
		}
		out = append(out, v)
	})
	return out
}

// idBound and idBoundHi cut the leg's image ids to a range; 0 leaves that
// side open.
type andDriverLeg struct {
	leaf      Expr
	ids       []int64
	idBound   int64
	idBoundHi int64
}

// The legs must go to both buildWhereDBDriverFull, which drops their
// EXISTS, and applyAndDriver, which adds the driver in their place.
func pickAndDriverTag(database *db.DB, expr Expr, allowSingleLiteral bool) ([]andDriverLeg, bool) {
	if database == nil {
		return nil, false
	}
	tagLeaves := collectAndedTags(expr)
	filterLeaves := collectAndedFilterLeaves(expr)
	if len(tagLeaves)+len(filterLeaves) == 0 {
		return nil, false
	}
	hasWildcard := slices.ContainsFunc(tagLeaves, func(leaf TagExpr) bool { return leaf.Wildcard != "" })
	// A lone literal is one EXISTS the planner handles well; a wildcard's
	// LIST SUBQUERY scans every tag row, so even one pays to materialise.
	if !hasWildcard && (len(tagLeaves)+len(filterLeaves)) < 2 && !allowSingleLiteral {
		return nil, false
	}

	type resolved struct {
		leaf  Expr
		ids   []int64
		usage int64
	}
	seenTag := make(map[TagExpr]bool, len(tagLeaves))
	seenFilter := make(map[FilterExpr]bool, len(filterLeaves))
	var legs []resolved
	for _, leaf := range tagLeaves {
		if seenTag[leaf] {
			continue
		}
		seenTag[leaf] = true
		ids, usage, ok := resolveDriverCanonicals(database, leaf)
		if !ok {
			return nil, false
		}
		if len(ids) == 0 {
			continue
		}
		legs = append(legs, resolved{leaf: leaf, ids: ids, usage: usage})
	}
	for _, leaf := range filterLeaves {
		if seenFilter[leaf] {
			continue
		}
		seenFilter[leaf] = true
		ids, usage, ok := resolveFilterDriverCanonicals(database, leaf.Key, tags.NormalizeTagName(leaf.Val))
		if !ok {
			continue
		}
		if len(ids) == 0 {
			continue
		}
		legs = append(legs, resolved{leaf: leaf, ids: ids, usage: usage})
	}
	if len(legs) == 0 {
		return nil, false
	}

	smallest := slices.MinFunc(legs, func(a, b resolved) int { return cmp.Compare(a.usage, b.usage) })
	if smallest.usage <= andDriverThreshold {
		return []andDriverLeg{{leaf: smallest.leaf, ids: smallest.ids}}, true
	}

	// Random sort temp-sorts every match anyway and a bucket caps the
	// leg, so for those callers a popular lone leg still beats
	// EXISTS-probing every visible row.
	if allowSingleLiteral && len(legs) == 1 {
		return []andDriverLeg{{leaf: legs[0].leaf, ids: legs[0].ids}}, true
	}

	// Every leg is popular: their INTERSECT beats EXISTS-probing every
	// visible row.
	if len(legs) < 2 {
		return nil, false
	}
	// Past two legs the extra narrowing no longer pays for the rows each
	// leg materialises; the dropped leaves keep their EXISTS.
	sort.Slice(legs, func(i, j int) bool { return legs[i].usage < legs[j].usage })
	const maxIntersectLegs = 2
	if len(legs) > maxIntersectLegs {
		legs = legs[:maxIntersectLegs]
	}
	out := make([]andDriverLeg, len(legs))
	for i, l := range legs {
		out[i] = andDriverLeg{leaf: l.leaf, ids: l.ids}
	}
	return out, true
}

func tagNamePredicate(col, wildcard, tag string) (pred string, arg any, ok bool) {
	switch wildcard {
	case "":
		return col + " = ?", tag, true
	case "prefix":
		return col + ` LIKE ? ESCAPE '\'`, db.EscapeLike(tag) + "%", true
	case "suffix":
		return col + ` LIKE ? ESCAPE '\'`, "%" + db.EscapeLike(tag), true
	case "substring":
		return col + ` LIKE ? ESCAPE '\'`, "%" + db.EscapeLike(tag) + "%", true
	}
	return "", nil, false
}

func resolveDriverCanonicals(database *db.DB, leaf TagExpr) ([]int64, int64, bool) {
	pred, arg, ok := tagNamePredicate("t.name", leaf.Wildcard, leaf.Tag)
	if !ok {
		return nil, 0, false
	}
	return drainCanonicalUsage(database,
		`SELECT canon.id, canon.usage_count
		 FROM tags t
		 JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE `+pred,
		arg,
	)
}

func resolveFilterDriverCanonicals(database *db.DB, category, name string) ([]int64, int64, bool) {
	if category == "" || name == "" {
		return nil, 0, false
	}
	return drainCanonicalUsage(database,
		`SELECT DISTINCT COALESCE(t.canonical_tag_id, t.id), canon.usage_count
		   FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		   JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		  WHERE t.name = ? AND tc.name = ?`,
		name, category,
	)
}

func drainCanonicalUsage(database *db.DB, query string, args ...any) ([]int64, int64, bool) {
	rows, err := database.Read.Query(query, args...)
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[int64]bool)
	var ids []int64
	var usage int64
	for rows.Next() {
		var id, count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, 0, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		usage += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false
	}
	return ids, usage, true
}

func visibleWhere(src Source, expr Expr, legs []andDriverLeg) (where string, args []any, hasMissingFilter, ceilingRewrote bool) {
	where, args, hasMissingFilter, ceilingRewrote = buildWhereDBDriverFull(expr, src, legs)
	where, args = applyAndDriver(where, args, legs)
	return andDefaultVisible(where, hasMissingFilter), args, hasMissingFilter, ceilingRewrote
}

func applyAndDriver(where string, args []any, legs []andDriverLeg) (string, []any) {
	if len(legs) == 0 {
		return where, args
	}
	driverArgs := make([]any, 0)
	parts := make([]string, len(legs))
	for i, leg := range legs {
		parts[i] = "SELECT image_id FROM image_tags WHERE tag_id IN (" + inlineIDs(leg.ids) + ")"
		if leg.idBound > 0 {
			parts[i] += " AND image_id >= ?"
			driverArgs = append(driverArgs, leg.idBound)
		}
		if leg.idBoundHi > 0 {
			parts[i] += " AND image_id <= ?"
			driverArgs = append(driverArgs, leg.idBoundHi)
		}
	}
	driverWhere := "i.id IN (" + strings.Join(parts, " INTERSECT ") + ")"
	if where == "" || where == "1=1" {
		return driverWhere, driverArgs
	}
	return driverWhere + " AND " + where, append(driverArgs, args...)
}

type whereBuilder struct {
	parts            []string
	args             []any
	hasMissingFilter bool
	db               *db.DB
	tree             *relations.BKTree
	tallies          *counts.Tallies
	ratingIDs        map[string]int64
	ratingUsage      map[string]int64
	ratingResolved   bool
	// Keyed by leaf value, so blue does not silence blue*, nor
	// character:miku artist:miku.
	driverLeaves        map[Expr]bool
	underOrNot          int
	ceilingRewrote      bool
	relPresence         relationPresence
	relPresenceResolved bool
	similarSeeds        map[int64]tags.OverlapSeed
	lookupBackends      []string
	perRow              bool
}

func (b *whereBuilder) similaritySeed(imageID int64) (tags.OverlapSeed, bool) {
	if seed, ok := b.similarSeeds[imageID]; ok {
		return seed, true
	}
	if b.db == nil {
		return tags.OverlapSeed{}, false
	}
	visible, ok := b.tallies.VisibleCount()
	if !ok {
		return tags.OverlapSeed{}, false
	}
	seed, err := tags.LoadOverlapSeed(b.db, imageID, visible)
	if err != nil {
		return tags.OverlapSeed{}, false
	}
	if b.similarSeeds == nil {
		b.similarSeeds = map[int64]tags.OverlapSeed{}
	}
	b.similarSeeds[imageID] = seed
	return seed, true
}

func (b *whereBuilder) resolveRatingIDs() {
	if b.ratingResolved {
		return
	}
	if b.db == nil {
		b.ratingResolved = true
		return
	}
	rows, err := b.db.Read.Query(
		`SELECT t.name, t.id, t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0
		   AND t.name IN ('` + strings.Join(contract.RatingLevels, "','") + `')`,
	)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]int64, 4)
	usage := make(map[string]int64, 4)
	for rows.Next() {
		var name string
		var id, count int64
		if err := rows.Scan(&name, &id, &count); err != nil {
			return
		}
		ids[name] = id
		usage[name] = count
	}
	// A torn read must not latch: a partial map turns rating:X into 1=0.
	if err := rows.Err(); err != nil {
		return
	}
	b.ratingIDs = ids
	b.ratingUsage = usage
	b.ratingResolved = true
}

func (b *whereBuilder) resolveCategoryTagByName(category, name string) ([]int64, bool) {
	if b.db == nil {
		return nil, false
	}
	ids, _, ok := resolveFilterDriverCanonicals(b.db, category, name)
	return ids, ok
}

func inlineImageTagsTagIDExists(ids []int64) string {
	if len(ids) == 0 {
		return "1=0"
	}
	return "EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.tag_id IN (" + inlineIDs(ids) + "))"
}

// Literals, not one parameter each: the planner sees a constant list, and a
// wide wildcard or phash match stays under SQLite's 32766-variable cap.
func inlineIDs(ids []int64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String()
}

// where must leave out the alias.image_id = i.id link, which this adds.
func (b *whereBuilder) imageIDExists(fromBody, alias, where string, negate bool) string {
	op := "EXISTS"
	if negate {
		op = "NOT EXISTS"
	}
	if where == "" {
		return fmt.Sprintf("%s (SELECT 1 FROM %s WHERE %s.image_id = i.id)", op, fromBody, alias)
	}
	return fmt.Sprintf("%s (SELECT 1 FROM %s WHERE %s.image_id = i.id AND %s)", op, fromBody, alias, where)
}

func (b *whereBuilder) imageTagsPredicate(where string, negate bool) string {
	return b.imageIDExists("image_tags it", "it", where, negate)
}

// buildWhereDBDriverFull also reports whether a missing: filter was seen
// and whether the ceiling became a rating_rank predicate.
func buildWhereDBDriverFull(expr Expr, src Source, legs []andDriverLeg) (string, []any, bool, bool) {
	var leaves map[Expr]bool
	if len(legs) > 0 {
		leaves = make(map[Expr]bool, len(legs))
		for _, l := range legs {
			leaves[l.leaf] = true
		}
	}
	b := newWhereBuilder(src)
	b.driverLeaves = leaves
	return b.build(expr)
}

// buildWherePerRow is for a caller testing a bounded set of rows: a
// predicate that would materialise a set over the whole library first
// tests each row on its own instead.
func buildWherePerRow(expr Expr, src Source) (string, []any) {
	b := newWhereBuilder(src)
	b.perRow = true
	where, args, hasMissingFilter, _ := b.build(expr)
	return andDefaultVisible(where, hasMissingFilter), args
}

func newWhereBuilder(src Source) *whereBuilder {
	return &whereBuilder{db: src.DB, tree: src.Tree, tallies: src.Tallies, lookupBackends: src.LookupBackends}
}

func (b *whereBuilder) build(expr Expr) (string, []any, bool, bool) {
	expr = b.peelCeilingForColumnRewrite(expr)
	if expr != nil {
		part := b.buildExpr(expr)
		if part != "" {
			b.parts = append(b.parts, part)
		}
	}
	where := cmp.Or(strings.Join(b.parts, " AND "), "1=1")
	return where, b.args, b.hasMissingFilter, b.ceilingRewrote
}

// One rating_rank comparison, covered by the rating-aware sort indexes,
// replaces a NOT EXISTS per excluded level.
func (b *whereBuilder) peelCeilingForColumnRewrite(expr Expr) Expr {
	if b.db == nil {
		return expr
	}
	user, rank, ok := extractCeilingShape(expr)
	if !ok || rank < 0 {
		return expr
	}
	b.parts = append(b.parts, "i.rating_rank <= ?")
	b.args = append(b.args, rank)
	b.ceilingRewrote = true
	return user
}

func (b *whereBuilder) categoryExists(name string) bool {
	if b.db == nil {
		return true
	}
	var n int
	if err := b.db.Read.QueryRow(
		`SELECT 1 FROM tag_categories WHERE name = ? LIMIT 1`, name,
	).Scan(&n); err != nil {
		return false
	}
	return true
}

func buildWhereDB(expr Expr, src Source) (string, []any, bool) {
	where, args, hasMissing, _ := buildWhereDBDriverFull(expr, src, nil)
	return where, args, hasMissing
}

func (b *whereBuilder) buildExpr(expr Expr) string {
	switch e := expr.(type) {
	case AndExpr:
		left := b.buildExpr(e.Left)
		right := b.buildExpr(e.Right)
		if left == "" {
			return right
		}
		if right == "" {
			return left
		}
		return "(" + left + " AND " + right + ")"

	case OrExpr:
		b.underOrNot++
		left := b.buildExpr(e.Left)
		right := b.buildExpr(e.Right)
		b.underOrNot--
		return "(" + left + " OR " + right + ")"

	case NotExpr:
		b.underOrNot++
		inner := b.buildExpr(e.Expr)
		b.underOrNot--
		return "NOT (" + inner + ")"

	case TagExpr:
		return b.buildTagExpr(e)

	case FilterExpr:
		return b.buildFilterExpr(e)
	}
	return ""
}

func (b *whereBuilder) buildTagExpr(e TagExpr) string {
	// The driver covers only the AND-reachable copy of a leaf; a copy
	// under an OR or a NOT still needs its own predicate.
	if b.underOrNot == 0 && b.driverLeaves[e] {
		return ""
	}
	pred, arg, _ := tagNamePredicate("name", e.Wildcard, e.Tag)
	b.args = append(b.args, arg)
	// COALESCE maps an alias name onto its canonical, which is what
	// image_tags rows carry.
	return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE `+pred+`)`, false)
}

// template must end in `%s ?`: the %s takes the operator, or
// `BETWEEN ? AND` for a range.
func (b *whereBuilder) buildCompFilter(template, val string, parseVal func(string) (any, bool)) string {
	if s, ok := b.tryRangeComp(template, val, parseVal, parseVal); ok {
		return s
	}
	op, n, ok := compOf(val, parseVal)
	return b.scalarComp(template, op, n, ok)
}

func (b *whereBuilder) scalarComp(template, op string, n any, ok bool) string {
	if !ok {
		return "1=0"
	}
	b.args = append(b.args, n)
	return fmt.Sprintf(template, op)
}

// Uncorrelated: an EXISTS would probe the metadata tables once per
// visible image, where this scans the much smaller metadata tables once.
func (b *whereBuilder) dualMetadataLike(sdCol, comfyCol, val string) string {
	if val == "" {
		return "1=0"
	}
	pat := "%" + db.EscapeLike(val) + "%"
	b.args = append(b.args, pat, pat)
	return `(i.id IN (SELECT image_id FROM sd_metadata WHERE ` + sdCol + ` LIKE ? ESCAPE '\')` +
		` OR i.id IN (SELECT image_id FROM comfyui_metadata WHERE ` + comfyCol + ` LIKE ? ESCAPE '\'))`
}

// Every file_type ingest stores: type: naming them all emits 1=1.
var fileTypeBuckets = func() map[string]bool {
	all := map[string]bool{}
	for _, ft := range models.FileTypes {
		all[ft.Name] = true
	}
	return all
}()

// A nil tautologyCap keeps the IN list even when every bucket is named.
func fileTypeInClause(seen, tautologyCap map[string]bool) string {
	if len(seen) == 0 {
		return "1=0"
	}
	if tautologyCap != nil && len(seen) == len(tautologyCap) {
		return "1=1"
	}
	quoted := make([]string, 0, len(seen))
	for _, ft := range slices.Sorted(maps.Keys(seen)) {
		quoted = append(quoted, "'"+ft+"'")
	}
	return "i.file_type IN (" + strings.Join(quoted, ", ") + ")"
}

var filterBuilders = map[string]func(*whereBuilder, FilterExpr) string{
	"system":     (*whereBuilder).buildSystemFilter,
	"fav":        boolCol("i.is_favorited"),
	"inbox":      boolCol("i.is_inbox"),
	"ai":         (*whereBuilder).buildAIFilter,
	"source":     (*whereBuilder).buildSourceFilter,
	"cat":        (*whereBuilder).buildCatFilter,
	"width":      comp("i.width %s ?", parseIntValue),
	"height":     comp("i.height %s ?", parseIntValue),
	"date":       func(b *whereBuilder, e FilterExpr) string { return b.buildDateFilter(e.Val) },
	"missing":    (*whereBuilder).buildMissingFilter,
	"type":       (*whereBuilder).buildTypeFilter,
	"collection": (*whereBuilder).buildCollectionFilter,
	// NULL page_count (not a manga) reads as 0, so pages:>=1 excludes images.
	"pages":    comp("COALESCE(i.page_count, 0) %s ?", parseIntValue),
	"name":     (*whereBuilder).buildNameFilter,
	"size":     comp("i.file_size %s ?", parseSizeValue),
	"mime":     (*whereBuilder).buildMimeFilter,
	"ratio":    comp("(CAST(i.width AS REAL) / NULLIF(i.height, 0)) %s ?", parseFloatValue),
	"tagcount": (*whereBuilder).buildTagCountFilter,
	// Not COALESCE like pages:, or a non-video would match duration:<5 as
	// a 0-second clip.
	"duration": comp("(i.duration_seconds IS NOT NULL AND i.duration_seconds %s ?)", parseFloatValue),
	"hash":     (*whereBuilder).buildHashFilter,
	"md5":      (*whereBuilder).buildMD5Filter,
	"id":       intEq("i.id"),
	// A token, not a time range: two batches can land in the same minute.
	"batch":      intEq("i.upload_batch"),
	"phash":      (*whereBuilder).buildPhashFilter,
	"relation":   (*whereBuilder).buildRelationFilter,
	"similar":    (*whereBuilder).buildSimilarFilter,
	"comfyui":    (*whereBuilder).buildComfyUIFilter,
	"prompt":     dual("prompt", "prompt"),
	"model":      dual("model", "model_checkpoint"),
	"sampler":    dual("sampler", "sampler"),
	"seed":       (*whereBuilder).buildSeedFilter,
	"via":        nocaseEq("i.origin", "1=0"),
	"tagged":     (*whereBuilder).buildTaggedFilter,
	"autotagged": (*whereBuilder).buildAutotaggedFilter,
	"stale":      (*whereBuilder).buildStaleFilter,
	"folder":     (*whereBuilder).buildFolderFilter,
	"folderonly": nocaseEq("i.folder_path", "i.folder_path = ''"),
	"generated":  (*whereBuilder).buildGeneratedFilter,
	"rating":     (*whereBuilder).buildRatingFilter,
	"lookup":     (*whereBuilder).buildLookupFilter,
	"upgrade":    (*whereBuilder).buildUpgradeFilter,
}

func comp(sqlTemplate string, parseVal func(string) (any, bool)) func(*whereBuilder, FilterExpr) string {
	return func(b *whereBuilder, e FilterExpr) string {
		return b.buildCompFilter(sqlTemplate, e.Val, parseVal)
	}
}

// Equality only: through comp, id:>5 would start matching.
func intEq(col string) func(*whereBuilder, FilterExpr) string {
	return func(b *whereBuilder, e FilterExpr) string {
		n, ok := parseIntValue(e.Val)
		return b.scalarComp(col+" %s ?", "=", n, ok)
	}
}

func boolCol(col string) func(*whereBuilder, FilterExpr) string {
	return func(_ *whereBuilder, e FilterExpr) string { return boolColumnFilter(col, e.Val) }
}

func dual(sdCol, comfyCol string) func(*whereBuilder, FilterExpr) string {
	return func(b *whereBuilder, e FilterExpr) string { return b.dualMetadataLike(sdCol, comfyCol, e.Val) }
}

func nocaseEq(col, whenEmpty string) func(*whereBuilder, FilterExpr) string {
	return func(b *whereBuilder, e FilterExpr) string {
		if e.Val == "" {
			return whenEmpty
		}
		b.args = append(b.args, e.Val)
		return col + " = ? COLLATE NOCASE"
	}
}

// A value a filter cannot parse emits 1=0: bound as a string, SQLite
// coerces it to 0 and matches every row, and dropping the predicate would
// widen the search.
func (b *whereBuilder) buildFilterExpr(e FilterExpr) string {
	if h, ok := filterBuilders[e.Key]; ok {
		return h(b, e)
	}
	return b.buildDefaultFilter(e)
}

// system: only opens the autocomplete cheat sheet; unhandled it would
// match everything.
func (b *whereBuilder) buildSystemFilter(_ FilterExpr) string { return "1=0" }

// A value that isn't a boolean matches nothing rather than reading as false.
func boolColumnFilter(col, val string) string {
	b, ok := searchkw.ParseBool(val)
	if !ok {
		return "1=0"
	}
	if b {
		return col + " = 1"
	}
	return col + " = 0"
}

func aiParts(val string) []string {
	var parts []string
	for _, p := range strings.Split(strings.ToLower(val), ",") {
		p = strings.TrimSpace(p)
		if p == "sd" {
			p = "a1111"
		}
		if p != "" && !slices.Contains(parts, p) {
			parts = append(parts, p)
		}
	}
	return parts
}

func (b *whereBuilder) buildAIFilter(e FilterExpr) string {
	parts := aiParts(e.Val)
	switch len(parts) {
	case 0:
		return "1=0"
	case 1:
		return b.aiClause(parts[0])
	}
	clauses := make([]string, len(parts))
	for i, p := range parts {
		clauses[i] = b.aiClause(p)
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// none never shares source_type with a tool, so it takes the equality that
// can seek idx_images_source_type_visible; the four LIKEs can't use an index.
// val is lowercased so ai:NONE takes the branch the index hint assumes.
func (b *whereBuilder) aiClause(val string) string {
	if val == "any" {
		// Literals so the seek keeps the index. A new models.SourceType*
		// must be added here, in aiValueMatches and in searchkw's ai entry
		// by hand.
		return "(i.source_type = 'a1111' OR i.source_type = 'comfyui' OR i.source_type = 'a1111,comfyui')"
	}
	if val == "none" {
		return "i.source_type = 'none'"
	}
	esc := db.EscapeLike(val)
	b.args = append(b.args, val, "%,"+esc, esc+",%", "%,"+esc+",%")
	return `(i.source_type = ? OR i.source_type LIKE ? ESCAPE '\' OR i.source_type LIKE ? ESCAPE '\' OR i.source_type LIKE ? ESCAPE '\')`
}

// aiValueMatches must accept exactly what aiClause's SQL does.
func aiValueMatches(sourceType, val string) bool {
	switch val {
	case "any":
		return sourceType == "a1111" || sourceType == "comfyui" || sourceType == "a1111,comfyui"
	case "none":
		return sourceType == "none"
	}
	return sourceType == val ||
		strings.HasPrefix(sourceType, val+",") ||
		strings.HasSuffix(sourceType, ","+val) ||
		strings.Contains(sourceType, ","+val+",")
}

// Uncorrelated on purpose: an EXISTS would probe image_sources once per
// visible image however few origins exist; the IN seeks
// idx_image_sources_site once.
func (b *whereBuilder) buildSourceFilter(e FilterExpr) string {
	switch strings.ToLower(e.Val) {
	case "", "none":
		return "i.id NOT IN (SELECT image_id FROM image_sources)"
	case "any":
		return "i.id IN (SELECT image_id FROM image_sources)"
	}
	b.args = append(b.args, e.Val)
	return "i.id IN (SELECT image_id FROM image_sources WHERE site = ? COLLATE NOCASE)"
}

func (b *whereBuilder) buildCatFilter(e FilterExpr) string {
	b.args = append(b.args, strings.ToLower(e.Val))
	return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)
}

// tagcount:<category><op>N counts one category's tags, a tag implied by
// another of that category on the image counting with it, so a costume
// variant and its base character are one character.
func (b *whereBuilder) buildTagCountFilter(e FilterExpr) string {
	category, comparison, ok := tagCountCategory(e.Val)
	if !ok {
		return b.buildCompFilter("i.tag_count %s ?", e.Val, parseIntValue)
	}
	count := "COUNT(*)"
	if b.perRow {
		count = tagCountPerRow
	}
	sub := &whereBuilder{}
	compared := sub.buildCompFilter(count+" %s ?", comparison, parseIntValue)
	if compared == "1=0" || !b.categoryExists(category) {
		return "1=0"
	}
	if b.perRow {
		b.args = append(b.args, category)
		b.args = append(b.args, sub.args...)
		return compared
	}
	b.args = append(b.args, category, category)
	b.args = append(b.args, sub.args...)
	// Grouped once, uncorrelated: a count per visible image walks every
	// tag row of the library.
	const counted = `SELECT it.image_id FROM image_tags it
		WHERE it.tag_id IN (SELECT id FROM tags WHERE category_id = (SELECT id FROM tag_categories WHERE name = ?))
		  AND NOT EXISTS (SELECT 1 FROM tag_implications ti
			JOIN tags pt ON pt.id = ti.parent_tag_id
			JOIN image_tags p ON p.tag_id = ti.parent_tag_id AND p.image_id = it.image_id
			WHERE ti.implied_tag_id = it.tag_id
			  AND pt.category_id = (SELECT id FROM tag_categories WHERE name = ?))
		GROUP BY it.image_id HAVING `
	// An image with no tag in the category has no group to match.
	if zeroSatisfies(comparison) {
		return "i.id NOT IN (" + counted + "NOT (" + compared + "))"
	}
	return "i.id IN (" + counted + compared + ")"
}

func tagCountCategory(val string) (category, comparison string, ok bool) {
	i := strings.IndexAny(val, "=<>")
	if i <= 0 {
		return "", "", false
	}
	return strings.ToLower(val[:i]), strings.TrimPrefix(val[i:], "="), true
}

func isTagCountCategory(e Expr) bool {
	f, ok := e.(FilterExpr)
	if !ok || f.Key != "tagcount" {
		return false
	}
	_, _, ok = tagCountCategory(f.Val)
	return ok
}

// The category is bound through a subquery, not as an id: given a literal
// the planner probes the implications of every tag on the image before
// testing its category.
const tagCountPerRow = `(SELECT COUNT(*) FROM image_tags it JOIN tags t ON t.id = it.tag_id
	WHERE it.image_id = i.id
	  AND t.category_id = (SELECT id FROM tag_categories WHERE name = ?)
	  AND NOT EXISTS (SELECT 1 FROM tag_implications ti
		JOIN tags pt ON pt.id = ti.parent_tag_id
		JOIN image_tags p ON p.tag_id = ti.parent_tag_id AND p.image_id = i.id
		WHERE ti.implied_tag_id = it.tag_id AND pt.category_id = t.category_id))`

// comparison must already parse.
func zeroSatisfies(comparison string) bool {
	if lo, hi, ok := strings.Cut(comparison, ".."); ok {
		return zeroHolds(">=", lo) && zeroHolds("<=", hi)
	}
	return zeroHolds(parseCompOp(comparison))
}

// An empty bound is a range's open end.
func zeroHolds(op, raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	n, _ := strconv.ParseInt(raw, 10, 64)
	switch op {
	case ">=":
		return 0 >= n
	case "<=":
		return 0 <= n
	case ">":
		return 0 > n
	case "<":
		return 0 < n
	}
	return n == 0
}

// The flag drops the default is_missing = 0, which would empty -missing:false.
func (b *whereBuilder) buildMissingFilter(e FilterExpr) string {
	b.hasMissingFilter = true
	return boolColumnFilter("i.is_missing", e.Val)
}

// archive is cbz alone: ingest stores zip archives as cbz too.
func (b *whereBuilder) buildTypeFilter(e FilterExpr) string {
	buckets := map[string][]string{}
	for _, ft := range models.FileTypes {
		buckets[ft.Kind] = append(buckets[ft.Kind], ft.Name)
	}
	seen := map[string]bool{}
	for _, v := range strings.Split(strings.ToLower(e.Val), ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		fts, ok := buckets[v]
		if !ok {
			continue
		}
		for _, ft := range fts {
			seen[ft] = true
		}
	}
	return fileTypeInClause(seen, fileTypeBuckets)
}

func (b *whereBuilder) buildCollectionFilter(e FilterExpr) string {
	// images.series mirrors the collections and is empty exactly when an
	// image has none, so the bare and any forms keep the indexed column.
	if e.Val == "" {
		return "i.series = ''"
	}
	if strings.ToLower(e.Val) == "any" {
		return "i.series != ''"
	}
	b.args = append(b.args, e.Val)
	return "i.id IN (SELECT image_id FROM image_collections WHERE name = ?)"
}

// Alias paths count too: a duplicate found under another name still
// matches that name.
func (b *whereBuilder) buildNameFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=0"
	}
	// A trigram index needs at least three characters; shorter input
	// takes the LIKE.
	if len([]rune(e.Val)) >= 3 {
		// One FTS5 phrase, so spaces and punctuation stay literal; a
		// quote is escaped by doubling it.
		ftsQuery := `"` + strings.ReplaceAll(strings.ToLower(e.Val), `"`, `""`) + `"`
		b.args = append(b.args, ftsQuery, ftsQuery)
		// The alias table is contentless, so its image_id reads NULL; its
		// rowid is the image_paths row.
		return `(i.id IN (SELECT rowid FROM image_basename_canonical_fts WHERE image_basename_canonical_fts MATCH ?) ` +
			`OR i.id IN (SELECT image_id FROM image_paths WHERE id IN (SELECT rowid FROM image_basename_alias_fts WHERE image_basename_alias_fts MATCH ?)))`
	}
	pat := "%" + db.EscapeLike(db.ASCIILower(e.Val)) + "%"
	// Pinned to the partial alias index; unpinned, the planner takes
	// idx_image_paths_image and tests is_canonical per row.
	b.args = append(b.args, pat, pat)
	return `(i.basename_lower LIKE ? ESCAPE '\' ` +
		`OR EXISTS (SELECT 1 FROM image_paths ip INDEXED BY idx_image_paths_aliases WHERE ip.image_id = i.id AND ip.is_canonical = 0 AND ip.basename_lower LIKE ? ESCAPE '\'))`
}

func (b *whereBuilder) buildMimeFilter(e FilterExpr) string {
	seen := map[string]bool{}
	for _, v := range strings.Split(strings.ToLower(e.Val), ",") {
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(strings.TrimPrefix(v, "image/"), "video/")
		if fileTypeBuckets[v] {
			seen[v] = true
		}
	}
	return fileTypeInClause(seen, nil)
}

func (b *whereBuilder) buildHashFilter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	if !isHexDigest(val, md5HexLen) && !isHexDigest(val, sha256HexLen) {
		return "1=0"
	}
	b.args = append(b.args, val)
	if len(val) == md5HexLen {
		return md5Exact
	}
	return "i.sha256 = ?"
}

func (b *whereBuilder) buildMD5Filter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	if val == "" {
		return "i.md5 != ''"
	}
	if !isHexDigest(val, md5HexLen) {
		return "1=0"
	}
	b.args = append(b.args, val)
	return md5Exact
}

const (
	md5HexLen    = 32
	sha256HexLen = 64

	// idx_images_md5 is partial on md5 != '', which a bound ? can't prove;
	// without the spelled-out predicate the planner skips the index.
	md5Exact = "i.md5 != '' AND i.md5 = ?"
)

func isHexDigest(s string, want int) bool {
	return len(s) == want && !strings.ContainsFunc(s, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') })
}

// IN, not EXISTS: the IN seeks the seed indexes, where an EXISTS walks
// images and probes each metadata table by image_id.
func (b *whereBuilder) buildSeedFilter(e FilterExpr) string {
	seed, ok := metadata.ParseSeed(strings.TrimSpace(e.Val))
	if !ok {
		return "1=0"
	}
	b.args = append(b.args, seed, seed)
	return "(i.id IN (SELECT image_id FROM sd_metadata WHERE seed = ?) OR i.id IN (SELECT image_id FROM comfyui_metadata WHERE seed = ?))"
}

// The booleans leave out monbooru's derived meta tags, which nobody
// tagged; tagged:monbooru still finds them through the ledger.
func (b *whereBuilder) buildTaggedFilter(e FilterExpr) string {
	return b.boolTagsPredicate(notDerivedMeta, "", e.Val)
}

// IS NOT, not !=: a UI add leaves tagger_name NULL, and NULL != 'x' is NULL.
const notDerivedMeta = `it.tagger_name IS NOT '` + models.TagSourceMonbooru + `'`

func (b *whereBuilder) buildAutotaggedFilter(e FilterExpr) string {
	return b.boolTagsPredicate("it.is_auto = 1", "it.is_auto = 1", e.Val)
}

func (b *whereBuilder) boolTagsPredicate(boolExtra, ledgerExtra, val string) string {
	if val == "" {
		return b.imageTagsPredicate(boolExtra, false)
	}
	v, ok := searchkw.ParseBool(val)
	if !ok {
		b.args = append(b.args, val)
		return b.sourceLedgerPredicate(ledgerExtra)
	}
	return b.imageTagsPredicate(boolExtra, !v)
}

// Off the ledger: image_tags.tagger_name keeps only the first applier, so
// a tag ptr re-confirmed would not answer tagged:ptr.
func (b *whereBuilder) sourceLedgerPredicate(extra string) string {
	if extra == "" {
		return b.imageIDExists("image_tag_sources s", "s", "s.source = ? COLLATE NOCASE", false)
	}
	return b.imageIDExists(
		"image_tag_sources s JOIN image_tags it ON it.image_id = s.image_id AND it.tag_id = s.tag_id",
		"s", extra+" AND s.source = ? COLLATE NOCASE", false)
}

func (b *whereBuilder) buildLookupFilter(e FilterExpr) string {
	switch strings.ToLower(e.Val) {
	case "never":
		return `NOT EXISTS (SELECT 1 FROM image_lookups l WHERE l.image_id = i.id)`
	case "due":
		// The opt-in is per backend, so each half carries its own
		// candidate clause.
		var halves []string
		for _, backend := range b.lookupBackends {
			due, args := lookup.DueClause(backend, time.Now())
			b.args = append(b.args, args...)
			halves = append(halves, "("+lookup.CandidateClause(backend)+" AND ("+due+"))")
		}
		if len(halves) == 0 {
			return "1=0"
		}
		return "(" + strings.Join(halves, " OR ") + ")"
	case "missed":
		return `EXISTS (SELECT 1 FROM image_lookups l WHERE l.image_id = i.id
		          AND l.last_result = 'miss' AND l.next_due_at IS NOT NULL)`
	case "exhausted":
		return `EXISTS (SELECT 1 FROM image_lookups l WHERE l.image_id = i.id
		          AND l.backend = 'booru' AND l.last_result = 'miss' AND l.next_due_at IS NULL)`
	case "off":
		// Either backend: half an opt-out still counts.
		return "(i.scheduled_lookup = 0 OR i.scheduled_lookup_ptr = 0)"
	}
	return "1=0"
}

// Uncorrelated so idx_image_sources_upgradable is sought once rather than
// probed per visible image.
func (b *whereBuilder) buildUpgradeFilter(e FilterExpr) string {
	candidates := "SELECT s.image_id FROM image_sources s WHERE " + upgrade.CandidateWhere("s")
	switch strings.ToLower(e.Val) {
	case "", "any":
		return "i.id IN (" + candidates + ")"
	case "none":
		return "i.id NOT IN (" + candidates + ")"
	case "unknown":
		// A candidate without an md5 claim is already an offer, not a
		// question a refresh would answer.
		return `(i.id IN (SELECT s.image_id FROM image_sources s WHERE s.url <> '' AND s.md5 = '')
		         AND i.id NOT IN (SELECT s.image_id FROM image_sources s WHERE s.url <> '' AND s.md5 <> '')
		         AND i.id NOT IN (` + candidates + `))`
	case "kept":
		return "i.id IN (SELECT s.image_id FROM image_sources s WHERE s.upgrade_kept = 1)"
	case "sample":
		return b.sampleSuspects(candidates)
	case "bigger":
		return `i.id IN (SELECT s.image_id FROM image_sources s JOIN images im ON im.id = s.image_id
		                 WHERE ` + upgrade.CandidateWhere("s") + `
		                   AND ((s.post_width > 0 AND s.post_height > 0 AND im.width > 0 AND im.height > 0
		                         AND s.post_width * s.post_height > im.width * im.height)
		                     OR ((s.post_width = 0 OR s.post_height = 0) AND s.post_size > im.file_size)))`
	}
	b.args = append(b.args, e.Val)
	return "i.id IN (" + candidates + " AND s.site = ? COLLATE NOCASE)"
}

// The booru sample prefix, pixiv's resized master, and the suffixes
// scaled or small variants are served under.
var samplePatterns = []string{`sample\_%`, `%\_master1200%`, `%-scaled%`, `%:small%`, `%:medium%`, `%preview%`}

// A suspicion, never a verdict, so it must not feed upgrade:any. 850 and
// 1200 are the edges previews are cut to.
func (b *whereBuilder) sampleSuspects(candidates string) string {
	names := make([]string, 0, len(samplePatterns))
	for _, p := range samplePatterns {
		b.args = append(b.args, p)
		names = append(names, `i.basename_lower LIKE ? ESCAPE '\'`)
	}
	return `(i.id IN (SELECT s.image_id FROM image_sources s WHERE s.url <> '')
	         AND i.id NOT IN (SELECT s.image_id FROM image_sources s WHERE s.md5_match = 'match')
	         AND i.id NOT IN (` + candidates + `)
	         AND ((` + strings.Join(names, " OR ") + `)
	              OR i.width = 850 OR i.width = 1200 OR i.height = 1200))`
}

func (b *whereBuilder) buildStaleFilter(e FilterExpr) string {
	switch strings.ToLower(e.Val) {
	case "", "any":
		return b.imageTagsPredicate("it.stale = 1", false)
	case "none":
		return b.imageTagsPredicate("it.stale = 1", true)
	}
	b.args = append(b.args, tags.NormalizeTagName(e.Val))
	return b.imageTagsPredicate("it.stale = 1 AND it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)", false)
}

func (b *whereBuilder) buildFolderFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=1"
	}
	b.args = append(b.args, e.Val, db.EscapeLike(e.Val)+"/%")
	return `(i.folder_path = ? COLLATE NOCASE OR i.folder_path LIKE ? ESCAPE '\' COLLATE NOCASE)`
}

func (b *whereBuilder) buildGeneratedFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val, e.Val)
	sm := b.imageIDExists("sd_metadata sm", "sm", "sm.generation_hash = ?", false)
	cm := b.imageIDExists("comfyui_metadata cm", "cm", "cm.generation_hash = ?", false)
	return "(" + sm + " OR " + cm + ")"
}

// Highest wins: rating:X also requires no higher rating on the image.
func (b *whereBuilder) buildRatingFilter(e FilterExpr) string {
	val := strings.ToLower(e.Val)
	rank := contract.RatingRank(val)
	if rank < 0 {
		return "1=0"
	}
	b.resolveRatingIDs()
	selfID, ok := b.ratingIDs[val]
	if !ok {
		return "1=0"
	}
	// An unused level: its EXISTS would scan every visible row to find nothing.
	if b.ratingUsage[val] == 0 {
		return "1=0"
	}
	b.args = append(b.args, selfID)
	parts := []string{b.imageTagsPredicate("it.tag_id = ?", false)}
	for i := rank + 1; i < len(contract.RatingLevels); i++ {
		higherID, ok := b.ratingIDs[contract.RatingLevels[i]]
		if !ok {
			continue
		}
		b.args = append(b.args, higherID)
		parts = append(parts, b.imageTagsPredicate("it.tag_id = ?", true))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// A key that names no category makes the whole key:val a literal tag
// name, like nier:automata.
func (b *whereBuilder) buildDefaultFilter(e FilterExpr) string {
	if b.categoryExists(e.Key) {
		if e.Val == "" {
			b.args = append(b.args, e.Key)
			return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)
		}
		if b.underOrNot == 0 && b.driverLeaves[e] {
			return ""
		}
		// Resolved once here, the per-row test is an image_tags seek
		// instead of a join through tags and tag_categories.
		if ids, ok := b.resolveCategoryTagByName(e.Key, tags.NormalizeTagName(e.Val)); ok {
			if len(ids) == 0 {
				return "1=0"
			}
			return inlineImageTagsTagIDExists(ids)
		}
		b.args = append(b.args, tags.NormalizeTagName(e.Val), e.Key)
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(t.canonical_tag_id, t.id) FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE t.name = ? AND tc.name = ?)`, false)
	}
	if e.Val == "" {
		return "1=1"
	}
	b.args = append(b.args, tags.NormalizeTagName(e.Key+":"+e.Val))
	return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)`, false)
}

var dateFilterRe = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2}(T\d{2}(:\d{2}(:\d{2})?)?)?)?)?$`)

// In time.Local, the zone every rendered timestamp uses, so a date means
// the operator's day.
func parseDatePrecision(val string) (start time.Time, next func(time.Time) time.Time, ok bool) {
	for _, p := range []struct {
		layout string
		next   func(time.Time) time.Time
	}{
		{"2006-01-02T15:04:05", func(t time.Time) time.Time { return t.Add(time.Second) }},
		{"2006-01-02T15:04", func(t time.Time) time.Time { return t.Add(time.Minute) }},
		{"2006-01-02T15", func(t time.Time) time.Time { return t.Add(time.Hour) }},
		{"2006-01-02", func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }},
		{"2006-01", func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }},
		{"2006", func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }},
	} {
		if t, err := time.ParseInLocation(p.layout, val, time.Local); err == nil {
			return t, p.next, true
		}
	}
	return time.Time{}, nil, false
}

// In ingested_at's stored UTC format, so the bounds compare as plain strings.
func dateBound(val string, end bool) string {
	t, next, ok := parseDatePrecision(val)
	if !ok {
		return ""
	}
	if end {
		t = next(t).Add(-time.Second)
	}
	return t.UTC().Format("2006-01-02T15:04:05") + "Z"
}

func (b *whereBuilder) buildDateFilter(val string) string {
	// Two-character operators first, or >= would match the > arm. <= and >
	// compare against the window's last second, so <=X takes in all of day X
	// and >X starts after it.
	for _, op := range []string{">=", "<=", ">", "<"} {
		if !strings.HasPrefix(val, op) {
			continue
		}
		date := val[len(op):]
		if !dateFilterRe.MatchString(date) {
			return "1=0"
		}
		bound := dateBound(date, op == "<=" || op == ">")
		if bound == "" {
			return "1=0"
		}
		b.args = append(b.args, bound)
		return "i.ingested_at " + op + " ?"
	}
	if s, ok := b.tryRangeComp("i.ingested_at %s ?", val,
		dateRangeBound(false), dateRangeBound(true)); ok {
		return s
	}
	val = strings.TrimPrefix(val, "=")
	if !dateFilterRe.MatchString(val) {
		return "1=0"
	}
	start, end := dateBound(val, false), dateBound(val, true)
	if start == "" || end == "" {
		return "1=0"
	}
	b.args = append(b.args, start, end)
	return "i.ingested_at BETWEEN ? AND ?"
}

func parseCompOp(val string) (string, string) {
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(val, op) {
			return op, val[len(op):]
		}
	}
	return "=", val
}

func compOf(val string, parse func(string) (any, bool)) (string, any, bool) {
	op, raw := parseCompOp(val)
	v, ok := parse(strings.TrimSpace(raw))
	return op, v, ok
}

// KB and KiB alike are 1024 bytes, the unit the UI displays.
func parseSizeValue(raw string) (any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	rest := strings.TrimLeft(raw, "0123456789.-+")
	numStr := raw[:len(raw)-len(rest)]
	unit := strings.TrimSpace(strings.ToLower(rest))
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return nil, false
	}
	var mult int64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	case "t", "tb", "tib":
		mult = 1 << 40
	default:
		return nil, false
	}
	v := n * float64(mult)
	switch {
	case v >= math.MaxInt64:
		return int64(math.MaxInt64), true
	case v <= math.MinInt64:
		return int64(math.MinInt64), true
	}
	return int64(v), true
}

// ok reports that val is a range, even one that parses to 1=0.
func (b *whereBuilder) tryRangeComp(template, val string, parseFrom, parseTo func(string) (any, bool)) (string, bool) {
	fromS, toS, ok := strings.Cut(val, "..")
	if !ok {
		return "", false
	}
	fromS, toS = strings.TrimSpace(fromS), strings.TrimSpace(toS)
	if fromS == "" && toS == "" {
		return "1=0", true
	}
	switch {
	case fromS == "":
		toV, ok := parseTo(toS)
		if !ok {
			return "1=0", true
		}
		b.args = append(b.args, toV)
		return fmt.Sprintf(template, "<="), true
	case toS == "":
		fromV, ok := parseFrom(fromS)
		if !ok {
			return "1=0", true
		}
		b.args = append(b.args, fromV)
		return fmt.Sprintf(template, ">="), true
	}
	fromV, ok := parseFrom(fromS)
	if !ok {
		return "1=0", true
	}
	toV, ok := parseTo(toS)
	if !ok {
		return "1=0", true
	}
	b.args = append(b.args, fromV, toV)
	return fmt.Sprintf(template, "BETWEEN ? AND"), true
}

func dateRangeBound(end bool) func(string) (any, bool) {
	return func(raw string) (any, bool) {
		if !dateFilterRe.MatchString(raw) {
			return "", false
		}
		v := dateBound(raw, end)
		return v, v != ""
	}
}

func parseIntValue(s string) (any, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil, false
	}
	return n, true
}

func parseFloatValue(s string) (any, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil, false
	}
	return n, true
}
