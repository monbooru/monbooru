package search

import (
	"cmp"
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
	// DeferCount leaves Total unknown rather than run the exact COUNT, when
	// the page itself can't tell it; Count answers it afterwards.
	DeferCount bool
	// After and Before read the page past or ahead of that image, under a
	// sort CursorPaging allows; Page then only labels it.
	After  int64
	Before int64
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
			return executeFromCachedIDs(ctx, database, ids, page, limit, pageStart(ids, q, page, limit))
		}
	}

	offset := (page - 1) * limit

	var cursor *keyCursor
	reverse := false
	if cursorID := cmp.Or(q.After, q.Before); cursorID != 0 && CursorPaging(q) {
		// A row deleted since the link was drawn falls back to the offset.
		if row, err := readCursorRow(ctx, database, cursorID); err == nil {
			c, _ := row.cursor(q.Sort, q.Order, 0)
			cursor, reverse = &c, q.After == 0
		}
	}

	var walkLegs [][]int64
	walking := false
	if cursor != nil || offset == 0 {
		walkLegs, walking = tagWalkLegs(src, q)
	}

	driverLegs, _ := pickAndDriverTag(database, q.Expr, q.Sort == "random")

	// ids mostly rise with ingested_at, so a dense intersection's first pages
	// sit in the newest page*limit*driverIDBoundMargin visible ids. asc wants
	// the other end, and missing: rows fall outside that walk.
	idBounded := false
	var boundTotal int
	if len(driverLegs) >= 2 && !walking &&
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
			return executeFromCachedIDs(ctx, database, ids, page, limit, (page-1)*limit)
		case overran:
			go finishSimilarityFan(src, key, gen, rankSeed, q.Order, where, slices.Clone(args))
		default:
			src.Cache.releaseFan(key)
			src.Cache.markFanOverBudget(key)
		}
	}

	var total int
	counted, totalIsBound, deferred := false, false, false
	switch {
	case q.SkipCount:
	case q.PresetTotal != nil:
		total = *q.PresetTotal
	default:
		counted = true
		var fast bool
		if total, totalIsBound, fast = fastTotal(src, q.Expr, hasMissingFilter); fast {
			break
		}
		if q.DeferCount {
			counted, deferred = false, true
			break
		}
		n, err := exactTotal(ctx, src, q.Expr, where, args)
		if err != nil {
			return nil, err
		}
		total = n
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
		if cursor != nil {
			cmpSQL, sort, cursorArgs := cursor.nextCmp, cursor.nextSort, cursor.nextArgs
			if reverse {
				cmpSQL, sort, cursorArgs = cursor.prevCmp, cursor.prevSort, cursor.prevArgs
			}
			dataSQL = fmt.Sprintf(
				"SELECT "+models.ImageRowColumns+`
				 FROM images i%s
				 WHERE %s AND %s
				 %s
				 LIMIT ?`,
				dataHint, where, cmpSQL, sort,
			)
			dataArgs = append(slices.Concat(args, cursorArgs), limit)
		}
		images, err := db.QueryAllContext(ctx, database.Read, scanImageRow, dataSQL, dataArgs...)
		if err != nil {
			return nil, fmt.Errorf("data query: %w", err)
		}
		if reverse {
			slices.Reverse(images)
		}
		return images, nil
	}

	var images []models.Image
	var err error
	walked := false
	if walking {
		up := (q.Order == "asc") != reverse
		var from int64
		if cursor != nil {
			from = cmp.Or(q.After, q.Before)
		}
		residual, residualArgs := buildWherePerRow(q.Expr, src)
		w := newTagWalk(database, walkLegs, up, walkStart(from, up))
		if images, walked, err = walkPage(ctx, database, w, residual, residualArgs, limit); err != nil {
			return nil, err
		}
		if walked && reverse {
			slices.Reverse(images)
		}
	}
	if !walked {
		if images, err = readPage(); err != nil {
			return nil, err
		}
	}
	// The fast total is an upper bound, so a window that holds too few rows
	// may only mean the matches are older than it.
	if idBounded && len(images) < limit && boundTotal > offset+len(images) {
		unbound()
		if images, err = readPage(); err != nil {
			return nil, err
		}
	}
	// A short first page holds every match, so it is the exact total. A
	// page read back from a cursor ends at the cursor, not at the last row.
	if counted && totalIsBound && offset == 0 && len(images) < limit && !reverse {
		total = len(images)
	}
	unknown := false
	if deferred {
		switch {
		case len(images) < limit && (len(images) > 0 || offset == 0) && !reverse:
			total = offset + len(images)
		case len(images) == 0:
			// A page past the end needs the total to clamp to.
			if total, err = exactTotal(ctx, src, q.Expr, where, args); err != nil {
				return nil, err
			}
		default:
			unknown = true
		}
	}

	// Page 1 fans after the answer: a page flip or prev/next that beats it
	// takes the cursor path. The fan drops the recent-id bound, which only
	// holds for this page.
	if key != "" && (unknown || total > 0 && total <= adjacencyCacheMaxIDs) {
		if !unknown && len(images) == total {
			ids := make([]int64, len(images))
			for i, img := range images {
				ids[i] = img.ID
			}
			src.Cache.set(key, ids, gen)
		} else if page == 1 && src.Cache.tryAcquireBackgroundFan(key) {
			if idBounded {
				unbound()
			}
			n := total
			if unknown {
				n = adjacencyCacheMaxIDs + 1
			}
			go finishFan(src, key, gen, indexHint, where, slices.Clone(args), orderClause, slices.Clone(orderArgs), n)
		}
	}

	return &models.SearchResult{
		Page:         page,
		Limit:        limit,
		Total:        total,
		TotalUnknown: unknown,
		Results:      images,
	}, nil
}

// fastTotal's ok is false when only the exact COUNT can answer.
func fastTotal(src Source, expr Expr, hasMissingFilter bool) (total int, bound, ok bool) {
	// The fast counts are visible-only, which a missing: filter doesn't
	// want.
	if hasMissingFilter || ceilingWrapsLoneLiteral(expr) {
		return 0, false, false
	}
	n, ok := fastTagTotal(src, expr)
	return n, ok && !fastTotalExact(expr), ok
}

func exactTotal(ctx context.Context, src Source, expr Expr, where string, args []any) (int, error) {
	if ceilingWrapsLoneLiteral(expr) {
		where, args = exactCountWhere(src, expr)
	}
	var total int
	if err := src.DB.Read.QueryRowContext(ctx, "SELECT COUNT(*) FROM images i WHERE "+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count query: %w", err)
	}
	return total, nil
}

// CursorPaging reports whether q's sort can read a page from a row's key:
// random and similarity have none to seek on, and a pinned collection sorts
// by a position only its own legs walk.
func CursorPaging(q Query) bool {
	switch q.Sort {
	case "", "newest", "filesize":
		return true
	case "order":
		return q.OrderCollection == ""
	}
	return false
}

// pageStart is where q's page begins in its cached list: next to its cursor
// when the list holds it, else at the page's offset.
func pageStart(ids []int64, q Query, page, limit int) int {
	if CursorPaging(q) {
		if i := slices.Index(ids, q.After); q.After != 0 && i >= 0 {
			return i + 1
		}
		if i := slices.Index(ids, q.Before); q.Before != 0 && i >= 0 {
			return max(i-limit, 0)
		}
	}
	return (page - 1) * limit
}

// Count is the total an Execute under DeferCount left unknown: the cached
// list's length once a fan has filled it, else the exact COUNT.
func Count(ctx context.Context, src Source, q Query) (int, error) {
	if ids, ok := src.Cache.get(q.listKey(src)); ok {
		return len(ids), nil
	}
	where, args := exactCountWhere(src, q.Expr)
	return exactTotal(ctx, src, q.Expr, where, args)
}

func scanImageRow(rows *sql.Rows) (models.Image, error) { return models.ScanImageRow(rows) }

func executeFromCachedIDs(ctx context.Context, database *db.DB, ids []int64, page, limit, offset int) (*models.SearchResult, error) {
	total := len(ids)
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

// finishFan releases the background fan its caller took. An empty fan holds
// the key off like an overrun: either way the next page 1 would pay it again.
func finishFan(src Source, key string, gen uint64, indexHint, where string, args []any, orderClause string, orderArgs []any, n int) {
	defer src.Cache.releaseBackgroundFan(key)
	ctx, cancel := context.WithTimeout(context.Background(), fanBackgroundBudget)
	defer cancel()
	ids := fetchSortedMatchIDs(ctx, src.DB, indexHint, where, args, orderClause, orderArgs, n)
	if len(ids) == 0 || len(ids) > adjacencyCacheMaxIDs {
		src.Cache.markFanOverBudget(key)
		return
	}
	src.Cache.set(key, ids, gen)
}

// fetchSortedMatchIDs returns nil on any error, a cancelled ctx included:
// a truncated list would shrink the total and cut prev/next off mid-set.
func fetchSortedMatchIDs(ctx context.Context, database *db.DB, indexHint, where string, args []any, orderClause string, orderArgs []any, total int) []int64 {
	n := min(total, adjacencyCacheMaxIDs+1)
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

// keyCursor holds the rows before and after one image in a view order: the
// predicate each side seeks with and the ORDER BY that walks away from it.
type keyCursor struct {
	keyCol   string
	prevCmp  string
	nextCmp  string
	prevSort string
	nextSort string
	prevArgs []any
	nextArgs []any
}

type cursorRow struct {
	id          int64
	ingestedAt  string
	fileSize    int64
	series      string
	seriesOrder sql.NullInt64
}

func readCursorRow(ctx context.Context, database *db.DB, id int64) (cursorRow, error) {
	r := cursorRow{id: id}
	err := database.Read.QueryRowContext(ctx,
		`SELECT ingested_at, file_size, series, series_order FROM images WHERE id = ?`, id,
	).Scan(&r.ingestedAt, &r.fileSize, &r.series, &r.seriesOrder)
	return r, err
}

// cursor's ok is false for an unseeded random sort, which has no key.
func (r cursorRow) cursor(sort, order string, seed int64) (keyCursor, bool) {
	var c keyCursor
	if sort == "order" {
		before, after, fwd, rev, bArgs, aArgs := orderCursor(r.series, r.seriesOrder, r.id, order == "desc")
		c.prevCmp, c.prevSort, c.prevArgs = before, rev, bArgs
		c.nextCmp, c.nextSort, c.nextArgs = after, fwd, aArgs
		return c, true
	}
	var keyVal any
	switch sort {
	case "random":
		if seed == 0 {
			return c, false
		}
		// %d emits only digits, so the seed interpolation is
		// injection-safe. db.RandomSortKey must hash as random_key()
		// does, or the cursor compares two key spaces.
		c.keyCol = fmt.Sprintf("random_key(i.id, %d)", seed)
		keyVal = int64(db.RandomSortKey(r.id, seed))
	case "filesize":
		c.keyCol = "i.file_size"
		keyVal = r.fileSize
	default: // "newest"
		c.keyCol = "i.ingested_at"
		keyVal = r.ingestedAt
	}

	// Row values, not the equivalent OR: only `(A, id) < (?, ?)`
	// seeks the (A, id) index.
	if order == "asc" || sort == "random" {
		c.prevCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", c.keyCol)
		c.nextCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", c.keyCol)
		c.prevSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", c.keyCol)
		c.nextSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", c.keyCol)
	} else {
		c.prevCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", c.keyCol)
		c.nextCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", c.keyCol)
		c.prevSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", c.keyCol)
		c.nextSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", c.keyCol)
	}
	c.prevArgs = []any{keyVal, r.id}
	c.nextArgs = []any{keyVal, r.id}
	return c, true
}

type adjacencyPlan struct {
	keyCursor
	where        string
	args         []any
	indexHint    string
	folderActive bool
	folderEq     string
	folderLo     string
	folderHi     string
	collOrder    bool
	collName     string
	collPrev     []collectionLeg
	collNext     []collectionLeg
	ok           bool // false for an unseeded random sort
}

func buildAdjacencyPlan(ctx context.Context, src Source, q Query, currentID int64, driverLegs []andDriverLeg, bucketed bool, bucketLo, bucketHi int64) (adjacencyPlan, error) {
	database := src.DB
	var p adjacencyPlan
	row, err := readCursorRow(ctx, database, currentID)
	if err != nil {
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
	} else {
		c, ok := row.cursor(q.Sort, q.Order, q.RandomSeed)
		if !ok {
			return p, nil
		}
		p.keyCursor = c
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

	if legs, ok := tagWalkLegs(src, q); ok {
		if prev, next, walked, err := walkAdjacent(ctx, src, q, legs, currentID); err != nil || walked {
			return prev, next, err
		}
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
	ctx, cancel := context.WithTimeout(context.Background(), fanBackgroundBudget)
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
	expr, order, orderArgs := sc.ordered(src)
	return streamScope(src, expr, order, orderArgs, visit)
}

// FirstIDs reads the scope's head under a LIMIT, so a sort without a
// covering index keeps the top n rather than sorting the whole set out.
func (sc Scope) FirstIDs(src Source, n int) ([]int64, error) {
	expr, order, orderArgs := sc.ordered(src)
	var ids []int64
	return ids, streamScope(src, expr, order+" LIMIT ?", append(orderArgs, n), func(id int64) error {
		ids = append(ids, id)
		return nil
	})
}

func (sc Scope) ordered(src Source) (Expr, string, []any) {
	expr := ApplyCeiling(sc.expr, sc.level)
	order, orderArgs := "ORDER BY i.id", []any(nil)
	if sc.viewOrder {
		order, orderArgs, _, _ = viewOrder(src, expr, sc.sort, sc.order, sc.randomSeed, sc.orderCollection)
	}
	return expr, order, orderArgs
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
