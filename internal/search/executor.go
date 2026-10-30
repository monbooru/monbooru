package search

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type Query struct {
	Expr        Expr
	Sort        string // "newest" | "filesize" | "random" | "order" | "similarity"
	Order       string // "asc" | "desc"
	RandomSeed  int64
	Page        int // 1-based
	Limit       int
	PresetTotal *int
	SkipCount   bool
	// Cached opts into the adjacency cache. Text, the query as typed, and
	// CeilingLevel key its list: Expr is already ceiling-wrapped.
	Cached          bool
	Text            string
	CeilingLevel    string
	OrderCollection string
}

func (q Query) listKey(src Source) string {
	if !q.Cached {
		return ""
	}
	return adjacencyKey(q.Text, q.Sort, q.Order, q.RandomSeed, q.CeilingLevel, src.LookupBackends)
}

func Execute(ctx context.Context, src Source, q Query) (*models.SearchResult, error) {
	database := src.DB
	started := time.Now()
	gen := src.Cache.gen()
	key := q.listKey(src)
	page := max(q.Page, 1)
	limit := q.Limit
	if limit < 1 {
		limit = 40
	}
	// page comes unbounded from the URL; page*limit*driverIDBoundMargin
	// must not overflow into a negative offset or slice bound.
	page = min(page, math.MaxInt/limit/driverIDBoundMargin)

	if key != "" && !q.SkipCount && q.PresetTotal == nil {
		if ids, ok := src.Cache.get(key); ok {
			return executeFromCachedIDs(ctx, database, ids, page, limit)
		}
	}

	driverLegs, _ := pickAndDriverTag(database, q.Expr, q.Sort == "random")

	// ids mostly rise with ingested_at, so a dense intersection's first pages
	// sit in the newest page*limit*driverIDBoundMargin visible ids. asc wants
	// the other end, and missing: rows fall outside that walk.
	idBounded := false
	var boundTotal int
	if len(driverLegs) >= 2 &&
		(q.Sort == "" || q.Sort == "newest") &&
		q.Order != "asc" &&
		!containsMissingFilter(q.Expr) {
		if total, ok := fastTagTotal(src, q.Expr); ok {
			if visible, vOk := src.Tallies.VisibleCount(); vOk &&
				total*driverIDBoundDensityCutoff >= visible {
				targetOffset := (page * limit) * driverIDBoundMargin
				var bound int64
				err := database.Read.QueryRowContext(ctx,
					`SELECT id FROM images INDEXED BY idx_images_ingested_visible
					 WHERE is_missing = 0
					 ORDER BY ingested_at DESC, id DESC
					 LIMIT 1 OFFSET ?`, targetOffset,
				).Scan(&bound)
				if err == nil {
					for i := range driverLegs {
						driverLegs[i].idBound = bound
					}
					idBounded = true
					boundTotal = total
				}
				// ErrNoRows: the library is smaller than the offset, so
				// there is nothing to cut.
			}
		}
	}

	where, args, hasMissingFilter, ceilingRewrote := visibleWhere(src, q.Expr, driverLegs)
	unbound := func() {
		for i := range driverLegs {
			driverLegs[i].idBound = 0
		}
		where, args, _, _ = visibleWhere(src, q.Expr, driverLegs)
		idBounded = false
	}

	orderClause, orderArgs, rankSeed, hasRankSeed := viewOrder(src, q.Expr, q.Sort, q.Order, q.RandomSeed, q.OrderCollection)

	// Similarity has no key column: COUNT and the scored ORDER BY would each
	// walk the whole match set for one page, so the fan runs first, on any
	// page. An empty or capped fan falls through rather than guess a total;
	// one that overran falls through too, and finishes in the background
	// under the fan lock so the next visit reads it from the cache.
	if q.Sort == "similarity" && hasRankSeed && key != "" &&
		!q.SkipCount && q.PresetTotal == nil && src.Cache.tryAcquireFan(key) {
		fanCtx, cancel := context.WithTimeout(context.Background(), fanBudget(time.Since(started), 0))
		ids := fanSimilarityIDs(fanCtx, src, rankSeed, q.Order, where, args)
		overran := fanCtx.Err() != nil
		cancel()
		switch {
		case len(ids) > 0 && len(ids) < adjacencyCacheMaxIDs:
			src.Cache.releaseFan(key)
			src.Cache.set(key, ids, gen)
			return executeFromCachedIDs(ctx, database, ids, page, limit)
		case overran:
			go finishSimilarityFan(src, key, gen, rankSeed, q.Order, where, slices.Clone(args))
		default:
			src.Cache.releaseFan(key)
			src.Cache.markFanOverBudget(key)
		}
	}

	offset := (page - 1) * limit

	var total int
	counted, totalIsBound := false, false
	switch {
	case q.SkipCount:
	case q.PresetTotal != nil:
		total = *q.PresetTotal
	default:
		counted = true
		// The fast counts are visible-only, which a missing: filter
		// doesn't want.
		wrapped := ceilingWrapsLoneLiteral(q.Expr)
		if !hasMissingFilter && !wrapped {
			if n, ok := fastTagTotal(src, q.Expr); ok {
				total, totalIsBound = n, !fastTotalExact(q.Expr)
				break
			}
		}
		countWhere, countArgs := where, args
		if wrapped {
			countWhere, countArgs = exactCountWhere(src, q.Expr)
		}
		countSQL := "SELECT COUNT(*) FROM images i WHERE " + countWhere
		if err := database.Read.QueryRowContext(ctx, countSQL, countArgs...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
	}

	if counted && total == 0 {
		return &models.SearchResult{Page: page, Limit: limit, Total: 0}, nil
	}

	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)

	// The planner drives from a multi-leg INTERSECT anyway; the pin only turns
	// each probe into a skip-scan of the partial index instead of a rowid seek.
	// The id-only fan keeps it: read to the end, its ordered covering scan wins.
	dataHint := indexHint
	if len(driverLegs) >= 2 {
		dataHint = ""
	}

	readPage := func() ([]models.Image, error) {
		dataSQL := fmt.Sprintf(
			"SELECT "+models.ImageRowColumns+`
			 FROM images i%s
			 WHERE %s
			 %s
			 LIMIT ? OFFSET ?`,
			dataHint, where, orderClause,
		)
		dataArgs := append(slices.Concat(args, orderArgs), limit, offset)
		images, err := db.QueryAllContext(ctx, database.Read, scanImageRow, dataSQL, dataArgs...)
		if err != nil {
			return nil, fmt.Errorf("data query: %w", err)
		}
		return images, nil
	}

	images, err := readPage()
	if err != nil {
		return nil, err
	}
	// The fast total is an upper bound, so a window that holds too few rows
	// may only mean the matches are older than it.
	if idBounded && len(images) < limit && boundTotal > offset+len(images) {
		unbound()
		if images, err = readPage(); err != nil {
			return nil, err
		}
	}
	// A short first page holds every match, so it is the exact total.
	if counted && totalIsBound && offset == 0 && len(images) < limit {
		total = len(images)
	}

	// Page 1 fans synchronously: an async fan loses the race to the next
	// page-flip or prev/next. The fan drops the recent-id bound, which only
	// holds for this page, and a short fan corrects the fast counter's total.
	if key != "" && total > 0 && total <= adjacencyCacheMaxIDs {
		if len(images) == total {
			ids := make([]int64, len(images))
			for i, img := range images {
				ids[i] = img.ID
			}
			src.Cache.set(key, ids, gen)
		} else if page == 1 && src.Cache.tryAcquireFan(key) {
			defer src.Cache.releaseFan(key)
			if idBounded {
				unbound()
			}
			budgetTotal := total
			if totalIsBound {
				budgetTotal = 0
			}
			fanCtx, cancel := context.WithTimeout(context.Background(), fanBudget(time.Since(started), budgetTotal))
			ids := fetchSortedMatchIDs(fanCtx, database, indexHint, where, args, orderClause, orderArgs, total)
			cancel()
			if len(ids) > 0 {
				src.Cache.set(key, ids, gen)
				if len(ids) < min(total, adjacencyCacheMaxIDs) {
					total = len(ids)
				}
			} else {
				src.Cache.markFanOverBudget(key)
			}
		}
	}

	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: images,
	}, nil
}

func scanImageRow(rows *sql.Rows) (models.Image, error) { return models.ScanImageRow(rows) }

func executeFromCachedIDs(ctx context.Context, database *db.DB, ids []int64, page, limit int) (*models.SearchResult, error) {
	total := len(ids)
	offset := (page - 1) * limit
	if offset >= total {
		return &models.SearchResult{Page: page, Limit: limit, Total: total}, nil
	}
	end := min(offset+limit, total)
	pageIDs := ids[offset:end]

	placeholders, args := db.InPlaceholders(pageIDs)
	sql := fmt.Sprintf(
		"SELECT "+models.ImageRowColumns+" FROM images i WHERE i.id IN (%s)", placeholders,
	)
	images, err := db.QueryAllContext(ctx, database.Read, scanImageRow, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cached id fetch: %w", err)
	}
	byID := make(map[int64]models.Image, len(images))
	for _, img := range images {
		byID[img.ID] = img
	}
	out := make([]models.Image, 0, len(pageIDs))
	for _, id := range pageIDs {
		if img, ok := byID[id]; ok {
			out = append(out, img)
		}
	}
	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: out,
	}, nil
}

// fetchSortedMatchIDs returns nil on any error, a cancelled ctx included:
// a truncated list would shrink the total and cut prev/next off mid-set.
func fetchSortedMatchIDs(ctx context.Context, database *db.DB, indexHint, where string, args []any, orderClause string, orderArgs []any, total int) []int64 {
	n := min(total, adjacencyCacheMaxIDs)
	sql := fmt.Sprintf(
		`SELECT i.id FROM images i%s WHERE %s %s LIMIT ?`,
		indexHint, where, orderClause,
	)
	qargs := append(slices.Concat(args, orderArgs), n)
	ids, err := db.QueryIDsContext(ctx, database.Read, sql, qargs...)
	if err != nil {
		return nil
	}
	return ids
}

// The random key has no index, so the cursor temp-sorts every match; an
// id bucket bounds that sort, at the cost of prev/next stopping at the
// bucket's edge.
const randomAdjacencyBucketSize = 2000

// Without a bucket the sort-key cursor can walk arbitrarily far between
// sparse matches. Wider than the random bucket because newest and
// filesize are the usual navigation sorts.
const andAdjacencyBucketSize = 10000

// orderCursor must match buildOrder's sort=order total order.
func orderCursor(series string, order sql.NullInt64, id int64, desc bool) (before, after, fwd, rev string, beforeArgs, afterArgs []any) {
	if desc {
		fwd = "ORDER BY i.series DESC, i.series_order IS NULL, i.series_order DESC, i.id DESC"
		rev = "ORDER BY i.series ASC, i.series_order IS NULL DESC, i.series_order ASC, i.id ASC"
		if order.Valid {
			before = "(i.series > ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) > (?, ?)))"
			after = "(i.series < ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) < (?, ?))))"
			beforeArgs = []any{series, series, order.Int64, id}
			afterArgs = []any{series, series, order.Int64, id}
		} else {
			before = "(i.series > ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id > ?))))"
			after = "(i.series < ? OR (i.series = ? AND i.series_order IS NULL AND i.id < ?))"
			beforeArgs = []any{series, series, id}
			afterArgs = []any{series, series, id}
		}
		return
	}
	fwd = "ORDER BY i.series ASC, i.series_order IS NULL, i.series_order ASC, i.id ASC"
	rev = "ORDER BY i.series DESC, i.series_order IS NULL DESC, i.series_order DESC, i.id DESC"
	if order.Valid {
		before = "(i.series < ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) < (?, ?)))"
		after = "(i.series > ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) > (?, ?))))"
		beforeArgs = []any{series, series, order.Int64, id}
		afterArgs = []any{series, series, order.Int64, id}
	} else {
		before = "(i.series < ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id < ?))))"
		after = "(i.series > ? OR (i.series = ? AND i.series_order IS NULL AND i.id > ?))"
		beforeArgs = []any{series, series, id}
		afterArgs = []any{series, series, id}
	}
	return
}

type collectionLeg struct {
	cmp  string
	sort string
	args []any
}

const (
	positionedGroup = "(pc.position IS NULL) = 0"
	// The second term pins the position column, so the reading index seeks pc.image_id.
	unpositionedGroup = "(pc.position IS NULL) = 1 AND pc.position IS NULL"
)

// The legs run nearest group first and must match collectionOrderClause. Each
// group seeks on its own: a desc walk mixes directions one index scan can't follow.
func collectionCursor(pos sql.NullInt64, id int64, desc bool) (prev, next []collectionLeg) {
	fwd, rev, after, before := "ASC", "DESC", ">", "<"
	if desc {
		fwd, rev, after, before = rev, fwd, before, after
	}
	byPosition := func(dir string) string { return "ORDER BY pc.position " + dir + ", pc.image_id " + dir }
	byID := func(dir string) string { return "ORDER BY pc.image_id " + dir }
	if pos.Valid {
		args := []any{pos.Int64, id}
		prev = []collectionLeg{{positionedGroup + " AND (pc.position, pc.image_id) " + before + " (?, ?)", byPosition(rev), args}}
		next = []collectionLeg{
			{positionedGroup + " AND (pc.position, pc.image_id) " + after + " (?, ?)", byPosition(fwd), args},
			{unpositionedGroup, byID(fwd), nil},
		}
		return prev, next
	}
	prev = []collectionLeg{
		{unpositionedGroup + " AND pc.image_id " + before + " ?", byID(rev), []any{id}},
		{positionedGroup, byPosition(rev), nil},
	}
	next = []collectionLeg{{unpositionedGroup + " AND pc.image_id " + after + " ?", byID(fwd), []any{id}}}
	return prev, next
}

type adjacencyPlan struct {
	where        string
	args         []any
	indexHint    string
	keyCol       string
	folderActive bool
	folderEq     string
	folderLo     string
	folderHi     string
	collOrder    bool
	collName     string
	collPrev     []collectionLeg
	collNext     []collectionLeg
	prevCmp      string
	nextCmp      string
	prevSort     string
	nextSort     string
	prevArgs     []any
	nextArgs     []any
	ok           bool // false for an unseeded random sort
}

func buildAdjacencyPlan(ctx context.Context, src Source, q Query, currentID int64, driverLegs []andDriverLeg, bucketed bool, bucketLo, bucketHi int64) (adjacencyPlan, error) {
	database := src.DB
	var p adjacencyPlan
	var ingestedAt string
	var fileSize int64
	var series string
	var seriesOrder sql.NullInt64
	if err := database.Read.QueryRowContext(ctx,
		`SELECT ingested_at, file_size, series, series_order FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize, &series, &seriesOrder); err != nil {
		return p, err
	}

	p.collOrder = q.Sort == "order" && q.OrderCollection != ""
	expr := q.Expr
	if p.collOrder {
		expr = withoutAndedLeaves(expr, func(e Expr) bool {
			f, ok := e.(FilterExpr)
			return ok && f.Key == "collection" && strings.EqualFold(f.Val, q.OrderCollection)
		})
	}
	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(expr, src, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// The folder legs carry the folder predicate, so the per-image where
	// would repeat it.
	p.folderActive, p.folderEq, p.folderLo, p.folderHi = detectPureFolder(q.Expr)
	// sort=order has no single key column for the folder UNION-ALL legs.
	if q.Sort == "order" {
		p.folderActive = false
	}
	if p.folderActive {
		where = ""
		args = args[:0]
	}

	where = andDefaultVisible(where, hasMissingFilter)

	if bucketed {
		where = where + " AND i.id BETWEEN ? AND ?"
		args = append(args, bucketLo, bucketHi)
	}

	var pcPos sql.NullInt64
	if p.collOrder {
		// No membership reads as a NULL position, which sorts in the
		// trailing group.
		_ = database.Read.QueryRowContext(ctx,
			`SELECT position FROM image_collections WHERE image_id = ? AND name = ?`,
			currentID, q.OrderCollection).Scan(&pcPos)
	}

	if p.collOrder {
		p.collName = q.OrderCollection
		p.collPrev, p.collNext = collectionCursor(pcPos, currentID, q.Order == "desc")
	} else if q.Sort == "order" {
		before, after, fwd, rev, bArgs, aArgs := orderCursor(series, seriesOrder, currentID, q.Order == "desc")
		p.prevCmp, p.prevSort, p.prevArgs = before, rev, bArgs
		p.nextCmp, p.nextSort, p.nextArgs = after, fwd, aArgs
	} else {
		var keyVal any
		switch q.Sort {
		case "random":
			if q.RandomSeed == 0 {
				return p, nil
			}
			// %d emits only digits, so the seed interpolation is
			// injection-safe. db.RandomSortKey must hash as random_key()
			// does, or the cursor compares two key spaces.
			p.keyCol = fmt.Sprintf("random_key(i.id, %d)", q.RandomSeed)
			keyVal = int64(db.RandomSortKey(currentID, q.RandomSeed))
		case "filesize":
			p.keyCol = "i.file_size"
			keyVal = fileSize
		default: // "newest"
			p.keyCol = "i.ingested_at"
			keyVal = ingestedAt
		}

		// Row values, not the equivalent OR: only `(A, id) < (?, ?)`
		// seeks the (A, id) index.
		if q.Order == "asc" || q.Sort == "random" {
			p.prevCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", p.keyCol)
			p.nextCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", p.keyCol)
			p.prevSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", p.keyCol)
			p.nextSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", p.keyCol)
		} else {
			p.prevCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", p.keyCol)
			p.nextCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", p.keyCol)
			p.prevSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", p.keyCol)
			p.nextSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", p.keyCol)
		}
		p.prevArgs = []any{keyVal, currentID}
		p.nextArgs = []any{keyVal, currentID}
	}

	p.indexHint = sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)
	p.where, p.args = where, args
	p.ok = true
	return p, nil
}

// CROSS JOIN drives from the membership, so the walk rides the label's reading index.
func (p adjacencyPlan) collectionRows(sel string, leg collectionLeg, tail string) (string, []any) {
	query := "SELECT " + sel + " FROM image_collections pc CROSS JOIN images i ON i.id = pc.image_id WHERE pc.name = ? AND " +
		p.where + " AND " + leg.cmp + tail
	return query, slices.Concat([]any{p.collName}, p.args, leg.args)
}

func (p adjacencyPlan) collectionNeighbour(legs []collectionLeg) (string, []any) {
	subs := make([]string, 0, len(legs)+1)
	var args []any
	for _, leg := range legs {
		sub, subArgs := p.collectionRows("i.id", leg, " "+leg.sort+" LIMIT 1")
		subs = append(subs, "("+sub+")")
		args = append(args, subArgs...)
	}
	return "SELECT COALESCE(" + strings.Join(append(subs, "NULL"), ", ") + ")", args
}

func (p adjacencyPlan) collectionPreceding() (string, []any) {
	legs := make([]string, 0, len(p.collPrev))
	var args []any
	for _, leg := range p.collPrev {
		rows, legArgs := p.collectionRows("1", leg, "")
		legs = append(legs, rows)
		args = append(args, legArgs...)
	}
	return strings.Join(legs, " UNION ALL "), args
}

func ExecuteAdjacent(ctx context.Context, src Source, q Query, currentID int64) (*int64, *int64, error) {
	database := src.DB
	if ids, ok := src.Cache.get(q.listKey(src)); ok {
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	// No key column to seek on, so the neighbours come from the ranked list.
	if q.Sort == "similarity" {
		ids := similarityMatchIDs(ctx, src, q)
		if len(ids) == 0 {
			return nil, nil, nil
		}
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	if anyLeaf(q.Expr, isTagCountCategory) {
		if ids := fanAdjacencyIDs(src, q); slices.Contains(ids, currentID) {
			prev, next := findInAdjacencyList(ids, currentID)
			return prev, next, nil
		}
	}

	// Decided first so the driver legs can be cut to the bucket: unbounded, an
	// INTERSECT of popular legs materialises far more rows than a bucket holds.
	// Small candidate sets skip it, as a bucket would rarely hold a second match.
	smallCandidate := false
	if total, ok := adjacencyTotalEstimate(src, q.Expr); ok && total < fastApproxThreshold {
		smallCandidate = true
	}

	bucketLo, bucketHi := int64(0), int64(0)
	bucketed := false
	switch {
	case smallCandidate:
	case q.Sort == "random" && containsTagPredicate(q.Expr):
		bucketLo = (currentID / randomAdjacencyBucketSize) * randomAdjacencyBucketSize
		bucketHi = bucketLo + randomAdjacencyBucketSize - 1
		bucketed = true
	case (q.Sort == "" || q.Sort == "newest" || q.Sort == "filesize") &&
		expensiveAdjacencyTags(q.Expr):
		bucketLo = (currentID / andAdjacencyBucketSize) * andAdjacencyBucketSize
		bucketHi = bucketLo + andAdjacencyBucketSize - 1
		bucketed = true
	}

	var driverLegs []andDriverLeg
	if !bucketed {
		driverLegs, _ = pickAndDriverTag(database, q.Expr, q.Sort == "random")
	} else {
		driverLegs, _ = pickAndDriverTag(database, q.Expr, true)
		for i := range driverLegs {
			driverLegs[i].idBound = bucketLo
			driverLegs[i].idBoundHi = bucketHi
		}
	}

	p, err := buildAdjacencyPlan(ctx, src, q, currentID, driverLegs, bucketed, bucketLo, bucketHi)
	if err != nil || !p.ok {
		return nil, nil, nil
	}

	if p.collOrder {
		neighbour := func(legs []collectionLeg) *int64 {
			query, qargs := p.collectionNeighbour(legs)
			var id sql.NullInt64
			if err := database.Read.QueryRowContext(ctx, query, qargs...).Scan(&id); err != nil || !id.Valid {
				return nil
			}
			return &id.Int64
		}
		return neighbour(p.collPrev), neighbour(p.collNext), nil
	}

	lookup := func(cursorCmp, sort string, cursorArgs []any) *int64 {
		var sql string
		var qargs []any
		if p.folderActive {
			// Two seeks on idx_images_folder_nocase_visible; SQLite plans
			// the OR of the equality and the range as a full index scan
			// plus a temp sort.
			outer := strings.ReplaceAll(strings.ReplaceAll(sort, p.keyCol, "k"), "i.id", "id")
			legSQL := "SELECT i.id AS id, " + p.keyCol + " AS k FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + p.where + " AND " + cursorCmp + " " + sort + " LIMIT 1"
			sql = "SELECT id FROM (SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
				") UNION ALL SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE") +
				")) " + outer + " LIMIT 1"
			qargs = slices.Concat(
				[]any{p.folderEq}, p.args, cursorArgs,
				[]any{p.folderLo, p.folderHi}, p.args, cursorArgs,
			)
		} else {
			qargs = slices.Concat(p.args, cursorArgs)
			sql = fmt.Sprintf("SELECT i.id FROM images i%s WHERE %s AND %s %s LIMIT 1",
				p.indexHint, p.where, cursorCmp, sort)
		}
		var id int64
		if err := database.Read.QueryRowContext(ctx, sql, qargs...).Scan(&id); err != nil {
			return nil
		}
		return &id
	}
	return lookup(p.prevCmp, p.prevSort, p.prevArgs), lookup(p.nextCmp, p.nextSort, p.nextArgs), nil
}

// finishSimilarityFan releases the fan lock its caller took.
func finishSimilarityFan(src Source, key string, gen uint64, seed tags.OverlapSeed, order, where string, args []any) {
	defer src.Cache.releaseFan(key)
	ctx, cancel := context.WithTimeout(context.Background(), similarityFanBackgroundBudget)
	defer cancel()
	ids := fanSimilarityIDs(ctx, src, seed, order, where, args)
	if len(ids) == 0 || len(ids) >= adjacencyCacheMaxIDs {
		src.Cache.markFanOverBudget(key)
		return
	}
	src.Cache.set(key, ids, gen)
}

// Each cursor statement would redo the grouped tagcount:<category> pass, so
// a miss fans the whole list once and the walk that follows reads it from the
// cache. nil when another request holds the fan or it overran its budget.
func fanAdjacencyIDs(src Source, q Query) []int64 {
	key := q.listKey(src)
	if !src.Cache.tryAcquireFan(key) {
		return nil
	}
	defer src.Cache.releaseFan(key)
	gen := src.Cache.gen()
	driverLegs, _ := pickAndDriverTag(src.DB, q.Expr, q.Sort == "random")
	where, args, hasMissingFilter, ceilingRewrote := visibleWhere(src, q.Expr, driverLegs)
	orderClause, orderArgs, _, _ := viewOrder(src, q.Expr, q.Sort, q.Order, q.RandomSeed, q.OrderCollection)
	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)
	fanCtx, cancel := context.WithTimeout(context.Background(), fanBudget(0, 0))
	ids := fetchSortedMatchIDs(fanCtx, src.DB, indexHint, where, args, orderClause, orderArgs, adjacencyCacheMaxIDs)
	cancel()
	if len(ids) == 0 || len(ids) >= adjacencyCacheMaxIDs {
		src.Cache.markFanOverBudget(key)
		return nil
	}
	src.Cache.set(key, ids, gen)
	return ids
}

// RankInQuery is 0-based and -1 when it has no useful answer. A sparse
// predicate can walk far before reaching the rank cap, so callers pass a
// deadline.
func RankInQuery(ctx context.Context, src Source, q Query, currentID int64) (int, error) {
	database := src.DB
	// The cursor COUNT has no score column to compare against.
	if q.Sort == "similarity" {
		return slices.Index(similarityMatchIDs(ctx, src, q), currentID), nil
	}

	// A lone leaf is materialised only while that is cheaper than the
	// cursor's walk to currentID; random sort, with no index to walk,
	// takes it regardless.
	allowSingle := q.Sort == "random"
	if total, ok := fastTagTotal(src, q.Expr); !ok || total <= fastApproxThreshold {
		allowSingle = true
	}
	driverLegs, _ := pickAndDriverTag(database, q.Expr, allowSingle)

	// No bucket: the COUNT must see every row before currentID.
	p, err := buildAdjacencyPlan(ctx, src, q, currentID, driverLegs, false, 0, 0)
	if err != nil {
		return -1, err
	}
	if !p.ok {
		return -1, nil
	}

	var rows string
	var qargs []any
	if p.collOrder {
		rows, qargs = p.collectionPreceding()
	} else if p.folderActive {
		legSQL := "SELECT 1 FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + p.where + " AND " + p.prevCmp
		rows = fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
			" UNION ALL " +
			fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE")
		qargs = slices.Concat(
			[]any{p.folderEq}, p.args, p.prevArgs,
			[]any{p.folderLo, p.folderHi}, p.args, p.prevArgs,
		)
	} else {
		rows = fmt.Sprintf(
			"SELECT 1 FROM images i%s WHERE %s AND %s",
			p.indexHint, p.where, p.prevCmp,
		)
		qargs = slices.Concat(p.args, p.prevArgs)
	}

	var rank int
	if err := database.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ("+rows+" LIMIT ?)",
		append(qargs, rankInQueryMaxRank+1)...,
	).Scan(&rank); err != nil {
		return -1, err
	}
	if rank > rankInQueryMaxRank {
		return -1, nil
	}
	return rank, nil
}

func viewOrder(src Source, expr Expr, sort, order string, seed int64, collection string) (string, []any, tags.OverlapSeed, bool) {
	switch {
	case sort == "order" && collection != "":
		clause, args := collectionOrderClause(collection, order)
		return clause, args, tags.OverlapSeed{}, false
	case sort == "similarity":
		if rankSeed, ok := similarityRankSeed(src, expr); ok {
			clause, args := similarityOrderClause(rankSeed, order)
			return clause, args, rankSeed, true
		}
	}
	return buildOrder(sort, order, seed), nil, tags.OverlapSeed{}, false
}

// Scope names images by query under the viewer's rating ceiling, so it never
// takes in rows the operator cannot see; delete-all walks it, so a term the
// executor honours and the scope ignores would delete everything.
type Scope struct {
	expr            Expr
	level           string
	viewOrder       bool
	sort            string
	order           string
	randomSeed      int64
	orderCollection string
}

// NewScope takes the ceiling level as the rating cookie holds it; "" means
// none.
func NewScope(expr Expr, level string) Scope {
	return Scope{expr: expr, level: level}
}

// InViewOrder is only for jobs that depend on position: a sort without a
// covering index temp-sorts the whole set.
func (sc Scope) InViewOrder(sort, order string, seed int64) Scope {
	sc.viewOrder, sc.sort, sc.order, sc.randomSeed = true, sort, order, seed
	if sort == "order" {
		sc.orderCollection = PinnedCollectionName(sc.expr)
	}
	return sc
}

func (sc Scope) Stream(src Source, visit func(id int64) error) error {
	expr := ApplyCeiling(sc.expr, sc.level)
	order, orderArgs := "ORDER BY i.id", []any(nil)
	if sc.viewOrder {
		order, orderArgs, _, _ = viewOrder(src, expr, sc.sort, sc.order, sc.randomSeed, sc.orderCollection)
	}
	return streamScope(src, expr, order, orderArgs, visit)
}

func (sc Scope) IDs(src Source) ([]int64, error) {
	var ids []int64
	return ids, sc.Stream(src, func(id int64) error {
		ids = append(ids, id)
		return nil
	})
}

func streamScope(src Source, expr Expr, orderBy string, orderArgs []any, visit func(id int64) error) error {
	database := src.DB
	driverLegs, _ := pickAndDriverTag(database, expr, false)
	where, args, _, _ := visibleWhere(src, expr, driverLegs)

	rows, err := database.Read.Query(
		"SELECT i.id FROM images i WHERE "+where+" "+orderBy,
		append(args, orderArgs...)...,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if err := visit(id); err != nil {
			return err
		}
	}
	return rows.Err()
}

const sidebarMaxPerCategory = 25

// SidebarTagsWithGlobalCount ranks by count on the page but reports the
// library-wide usage_count.
func SidebarTagsWithGlobalCount(src Source, imageIDs []int64) ([]models.Tag, error) {
	database := src.DB
	if len(imageIDs) == 0 {
		return nil, nil
	}

	placeholders, args := db.InPlaceholders(imageIDs)

	return db.QueryAll(database.Read, tags.ScanTag,
		fmt.Sprintf(
			`WITH tag_counts AS (
			     SELECT t.id AS tag_id, t.name AS tag_name, tc.name AS cat_name,
			            tc.color AS cat_color, tc.position AS cat_pos, t.usage_count,
			            COUNT(DISTINCT it.image_id) AS page_count
			     FROM image_tags it INDEXED BY idx_image_tags_image
			     JOIN tags t ON t.id = it.tag_id
			     JOIN tag_categories tc ON tc.id = t.category_id
			     WHERE it.image_id IN (%s) AND t.is_alias = 0
			     GROUP BY t.id
			 )
			 SELECT tag_id, tag_name, cat_name, cat_color, usage_count
			 FROM (
			     SELECT tag_id, tag_name, cat_name, cat_color, cat_pos, usage_count, page_count,
			            ROW_NUMBER() OVER (PARTITION BY cat_name
			                               ORDER BY page_count DESC, tag_name ASC) AS rn
			     FROM tag_counts
			 )
			 WHERE rn <= ?
			 ORDER BY cat_pos, cat_name, page_count DESC, tag_name ASC`,
			placeholders,
		),
		append(args, sidebarMaxPerCategory)...)
}

// The 10 suggestions shown, with headroom for the candidates that match
// no image in the context.
const suggestCandidateCap = 25

// Each side of a combination count reads at most this many rows: the
// images matching the context, and the images carrying a candidate.
const suggestContextCap = 1000

// SuggestTagsWithFilter's UsageCount counts the images matching both expr
// and the tag.
func SuggestTagsWithFilter(src Source, expr Expr, prefix, categoryName string, limit int) ([]models.Tag, error) {
	database := src.DB
	prefix = tags.NormalizeTagName(prefix)
	// With no context the combination count is the global usage count.
	if expr == nil {
		return tags.SuggestUsageRanked(database, prefix, categoryName, true, limit)
	}

	where, args, hasMissingFilter := buildWhereDB(expr, src)
	where = andDefaultVisible(where, hasMissingFilter)
	ownWhere, ownArgs := buildWherePerRow(expr, src)

	// Past the cap ctx is a sample that can miss a rare candidate, so one on
	// at most cap images is tested image by image (CROSS JOIN keeps the walk
	// on its rows); the sampled floors of more used candidates rank first.
	baseSQL := `WITH ctx AS MATERIALIZED (
	                SELECT i.id AS image_id FROM images i WHERE %[1]s LIMIT ?
	            ),
	            cand AS (
	                SELECT id, category_id, usage_count
	                FROM tags
	                WHERE is_alias = 0
	                  AND name LIKE ? ESCAPE '\'
	                  %[2]s
	                ORDER BY usage_count DESC
	                LIMIT ?
	            ),
	            own AS (
	                SELECT it.tag_id, COUNT(*) AS n
	                FROM image_tags it CROSS JOIN images i ON i.id = it.image_id
	                WHERE (SELECT COUNT(*) FROM ctx) > ?
	                  AND it.tag_id IN (SELECT id FROM cand WHERE usage_count <= ?)
	                  AND %[3]s
	                GROUP BY it.tag_id
	            )
	            SELECT id, name, cat, color, combo FROM (
	                SELECT c.id, t.name, tc.name AS cat, tc.color, c.usage_count,
	                    c.usage_count > ? AND (SELECT COUNT(*) FROM ctx) > ? AS sampled, MAX(
	                    COALESCE(own.n, 0),
	                    (SELECT COUNT(*) FROM image_tags it
	                      WHERE it.tag_id = c.id AND it.image_id IN (SELECT image_id FROM ctx))) AS combo
	                FROM cand c
	                JOIN tags t ON t.id = c.id
	                JOIN tag_categories tc ON tc.id = c.category_id
	                LEFT JOIN own ON own.tag_id = c.id
	            )
	            WHERE combo > 0
	            ORDER BY sampled DESC, combo DESC, usage_count DESC
	            LIMIT ?`

	catClause := ""
	catArgs := []any{}
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	return tags.SuggestTwoPass(db.EscapeLike(prefix), limit, func(pat, notLike string, n int) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 7+len(args)+len(ownArgs)+len(catArgs))
		qargs = append(qargs, args...)
		// One row past the cap tells a sample from the whole context.
		qargs = append(qargs, suggestContextCap+1)
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if notLike != "" {
			extra = extra + ` AND name NOT LIKE ? ESCAPE '\'`
			qargs = append(qargs, notLike)
		}
		qargs = append(qargs, suggestCandidateCap, suggestContextCap, suggestContextCap)
		qargs = append(qargs, ownArgs...)
		qargs = append(qargs, suggestContextCap, suggestContextCap, n)
		return db.QueryAll(database.Read, db.ScanRow(func(t *models.Tag) []any {
			return []any{&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount}
		}), fmt.Sprintf(baseSQL, where, extra, ownWhere), qargs...)
	})
}

func sqlDir(order, def string) string {
	switch order {
	case "asc":
		return "ASC"
	case "desc":
		return "DESC"
	}
	return def
}

// Read from the join table: an image in several collections sorts by the
// pinned one's position, not its home order.
func collectionOrderClause(name, order string) (string, []any) {
	dir := sqlDir(order, "ASC")
	sub := "(SELECT position FROM image_collections WHERE image_id = i.id AND name = ?)"
	clause := "ORDER BY " + sub + " IS NULL, " + sub + " " + dir + ", i.id " + dir
	return clause, []any{name, name}
}

// PinnedCollectionName is "" unless exactly one collection: value is
// ANDed at the top level; collection:any names no collection.
func PinnedCollectionName(expr Expr) string {
	seen := map[string]struct{}{}
	last := ""
	WalkAndedLeaves(expr, func(e Expr) {
		if v, ok := e.(FilterExpr); ok && v.Key == "collection" && v.Val != "" && !strings.EqualFold(v.Val, "any") {
			seen[strings.ToLower(v.Val)] = struct{}{}
			last = v.Val
		}
	})
	if len(seen) == 1 {
		return last
	}
	return ""
}

// Collection order reads forwards, 1..N; every other sort leads with the
// newest or largest.
func DefaultOrder(sort string) string {
	if sort == "order" {
		return "asc"
	}
	return "desc"
}

func buildOrder(sort, order string, randomSeed int64) string {
	switch sort {
	case "filesize":
		dir := sqlDir(order, "DESC")
		return "ORDER BY i.file_size " + dir + ", i.id " + dir
	case "order":
		// NULL positions sort last in both directions; id makes the order
		// total.
		dir := sqlDir(order, "ASC")
		return "ORDER BY i.series " + dir + ", i.series_order IS NULL, i.series_order " + dir + ", i.id " + dir
	case "random":
		if randomSeed != 0 {
			// %d emits only digits, so the seed interpolation is
			// injection-safe.
			return fmt.Sprintf("ORDER BY random_key(i.id, %d), i.id", randomSeed)
		}
		return "ORDER BY RANDOM(), i.id"
	default: // "newest"
		dir := sqlDir(order, "DESC")
		return "ORDER BY i.ingested_at " + dir + ", i.id " + dir
	}
}
