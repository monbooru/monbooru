package web

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) findPairsDistance() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Relations.DefaultDistance
}

func (s *Server) tagPairsEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Relations.TagPairs
}

func applyRelationsConfig(rc config.RelationsConfig) {
	relations.IncrementalProbeDistance.Store(int32(rc.DefaultDistance))
	relations.IncrementalProbeEnabled.Store(rc.IncrementalOnIngest)
}

// The on-ingest probe has no switch: a save always turns it on.
func (s *Server) settingsRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	d, err := strconv.Atoi(r.FormValue("default_distance"))
	if err != nil || d < 0 || d > config.MaxPhashDistance {
		writeInlineFlash(w, "err", "Distance must be an integer 0..12.")
		return
	}
	order := r.FormValue("default_session_order")
	if !validOrderModes[order] {
		writeInlineFlash(w, "err", "Unknown session order.")
		return
	}
	threshold := config.DefaultTagPairThreshold
	if raw := r.FormValue("tag_pair_threshold"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) {
			writeInlineFlash(w, "err", "Match strength must be a number between 70 and 100.")
			return
		}
		threshold = config.ClampTagPairThreshold(v / 100)
	}
	tagPairs := r.FormValue("tag_pairs") == "on"
	var before, rc config.RelationsConfig
	if err := s.withConfig(func(c *config.Config) error {
		before = c.Relations
		c.Relations.DefaultDistance = d
		c.Relations.DefaultSessionOrder = order
		c.Relations.IncrementalOnIngest = true
		c.Relations.TagPairs = tagPairs
		c.Relations.TagPairThreshold = threshold
		rc = c.Relations
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	applyRelationsConfig(rc)
	pruned := ""
	if before.DefaultDistance != d || before.TagPairs != tagPairs || before.TagPairThreshold != threshold {
		if n := s.pruneRelationQueues(r.Context(), rc); n > 0 {
			pruned = fmt.Sprintf(" %d stale pair%s dropped.", n, plural(n))
		}
	}
	logx.Infof("settings: relations { distance=%d order=%s tag_pairs=%v threshold=%.2f }", d, order, tagPairs, threshold)
	writeInlineFlash(w, "ok", fmt.Sprintf("Saved. distance=%d order=%s tag pairs=%v (%.0f%%)%s",
		d, order, tagPairs, threshold*100, pruned))
}

// Every gallery, not only the active one: the settings are app-wide but
// each gallery keeps its own queue.
func (s *Server) pruneRelationQueues(ctx context.Context, rc config.RelationsConfig) int {
	ctxs := s.allContexts()
	opts := relations.FindPairsOptions{
		Distance:         rc.DefaultDistance,
		TagPairs:         rc.TagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(rc.TagPairThreshold),
	}
	total := 0
	for _, cx := range ctxs {
		if cx.DB == nil {
			continue
		}
		n, err := relations.PruneQueue(ctx, cx.DB, opts)
		if err != nil {
			logx.Warnf("prune pair queue %q: %v", cx.Name, err)
			continue
		}
		total += n
	}
	return total
}

type relationsCounts struct {
	PhashMissing    int
	QueueOpen       int
	QueueSkipped    int
	QueueBySource   []queueSourceCount
	DupGroups       int
	AltGroups       int
	VersionChains   int
	DerivativeTrees int
	NotRelatedPairs int
}

type queueSourceCount struct {
	Label string
	Count int
}

var queueSourceLabels = []struct{ source, label string }{
	{relations.SourcePhash, "image similarity"},
	{relations.SourceTags, "rare tag similarity"},
	{relations.SourceBoth, "both"},
	{relations.SourceReview, "reopened"},
}

func queueCounts(cx *library.Gallery, ceiling *Ceiling) (open, skipped int, bySource []queueSourceCount) {
	var rank *int
	if r, active := ceiling.RankCeiling(); active {
		rank = &r
	}
	open, skipped, counts, err := cx.RelationsSvc.QueueBySource(rank)
	if err != nil {
		logx.Debugf("relations queue counts: %v", err)
		return 0, 0, nil
	}
	if len(counts) < 2 {
		return open, skipped, nil
	}
	bySource = make([]queueSourceCount, 0, len(counts))
	for _, s := range queueSourceLabels {
		if n := counts[s.source]; n > 0 {
			bySource = append(bySource, queueSourceCount{Label: s.label, Count: n})
		}
	}
	return open, skipped, bySource
}

type browseCard struct {
	Kind             string
	GroupID          int64
	Members          []int64
	Original         int64
	CreatedAt        string
	MemberIngestedAt map[int64]string
	Graph            *derivGraph
	Roots            []int64
}

func (s *Server) relationsPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	ceiling := resolveCeiling(r, cx)
	counts := loadRelationsCounts(cx, ceiling, "")
	s.renderTemplate(w, "relations.html", relationsPageData{
		baseData: s.base(r, "relations", "Relations - "+s.booruName()),
		Counts:   counts,
	})
}

type relationsPageData struct {
	baseData
	Counts relationsCounts
}

var browseKindByShortName = map[string]string{"alt": "alternate", "version": "version", "derivative": "derivative"}

func (s *Server) browseGroupsRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/relations/browse?kind=" + cmp.Or(browseKindByShortName[r.URL.Query().Get("kind")], "duplicate")
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// The skipKind count (version or derivative) is left at zero for the
// caller to fill from its own card walk.
func loadRelationsCounts(cx *library.Gallery, ceiling *Ceiling, skipKind string) relationsCounts {
	var c relationsCounts
	count := func(table, where string, args []any, dst *int) {
		if where != "" {
			where = ` WHERE ` + where
		}
		q := `SELECT COUNT(*) FROM ` + table + where
		if err := cx.DB.Read.QueryRow(q, args...).Scan(dst); err != nil {
			logx.Debugf("relations counts %q: %v", q, err)
		}
	}
	if n, err := cx.PhashMissingUnder(ceiling); err == nil {
		c.PhashMissing = n
	}
	c.QueueOpen, c.QueueSkipped, c.QueueBySource = queueCounts(cx, ceiling)
	where, args := ceiling.WhereGroupClean("dup_group_members", "dup_groups.id")
	count("dup_groups", where, args, &c.DupGroups)
	where, args = ceiling.WhereGroupClean("alt_group_members", "alt_groups.id")
	count("alt_groups", where, args, &c.AltGroups)
	if skipKind != "version" {
		if _, total, err := loadVersionChainCards(cx, ceiling); err == nil {
			c.VersionChains = total
		} else {
			logx.Debugf("relations counts version chains: %v", err)
		}
	}
	if skipKind != "derivative" {
		if _, _, total, err := loadDerivativeTreeCards(cx, ceiling); err == nil {
			c.DerivativeTrees = total
		} else {
			logx.Debugf("relations counts derivative trees: %v", err)
		}
	}
	where, args = ceiling.WhereTwo("a_image_id", "b_image_id")
	count("not_related_pairs", where, args, &c.NotRelatedPairs)
	return c
}

// The int is the version or derivative total past the ceiling and before
// the limit; 0 for the other kinds.
func loadBrowseCardsByKind(cx *library.Gallery, kind, sort string, limit, offset int, ceiling *Ceiling) ([]browseCard, int, error) {
	groupCardsWhere := func(membersTable, groupCol string) (string, []any) {
		w, a := ceiling.WhereGroupClean(membersTable, groupCol)
		if w == "" {
			return "", nil
		}
		return " AND " + w, a
	}
	var cards []browseCard
	var walkedTotal int
	switch kind {
	case "duplicate", "alternate":
		g := browseGroupKinds[kind]
		where, args := groupCardsWhere(g.membersTable, g.groupTable+".id")
		var err error
		cards, err = db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (browseCard, error) {
			var id, original int64
			var createdAt string
			if err := rows.Scan(&id, &original, &createdAt); err != nil {
				return browseCard{}, err
			}
			members, err := scanGroupMembers(cx, g.membersTable, id)
			return browseCard{Kind: kind, GroupID: id, Members: members, Original: original, CreatedAt: humanISOTime(createdAt)}, err
		}, `SELECT `+g.cols+` FROM `+g.groupTable+` WHERE 1=1`+where+` `+g.sortClause(sort)+` LIMIT ? OFFSET ?`,
			append(args, limit, offset)...)
		if err != nil {
			return nil, 0, err
		}
	case "version":
		// Annotated before the sort: newest_member reads the ingest dates.
		chains, total, cErr := loadVersionChainCards(cx, ceiling)
		if cErr != nil {
			return nil, 0, cErr
		}
		if sort == "newest_member" {
			if aErr := annotateBrowseCardIngestedAt(cx, chains); aErr != nil {
				logx.Warnf("browse cards ingest-dates version: %v", aErr)
			}
		}
		sortVersionCards(chains, sort)
		cards = append(cards, sliceWindow(chains, offset, limit)...)
		walkedTotal = total
	case "derivative":
		trees, sourcesOf, total, tErr := loadDerivativeTreeCards(cx, ceiling)
		if tErr != nil {
			return nil, 0, tErr
		}
		sortVersionCards(trees, sort)
		cards = append(cards, sliceWindow(trees, offset, limit)...)
		for i := range cards {
			cards[i].Graph = layOutDerivatives(cards[i].Members, sourcesOf)
		}
		walkedTotal = total
	case "not_related":
		where, args := ceiling.WhereTwo("a_image_id", "b_image_id")
		q := `SELECT a_image_id, b_image_id, created_at FROM not_related_pairs`
		if where != "" {
			q += ` WHERE ` + where
		}
		q += ` ORDER BY rowid DESC LIMIT ? OFFSET ?`
		var err error
		cards, err = db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (browseCard, error) {
			var a, b int64
			var createdAt string
			err := rows.Scan(&a, &b, &createdAt)
			return browseCard{Kind: "not_related", Members: []int64{a, b}, CreatedAt: humanISOTime(createdAt)}, err
		}, q, append(args, limit, offset)...)
		if err != nil {
			return nil, 0, err
		}
	default:
		return nil, 0, nil
	}
	if err := annotateBrowseCardIngestedAt(cx, cards); err != nil {
		logx.Warnf("browse cards ingest-dates %s: %v", kind, err)
	}
	return cards, walkedTotal, nil
}

var browseGroupKinds = map[string]struct {
	groupTable   string
	membersTable string
	cols         string
	sortClause   func(string) string
}{
	"duplicate": {"dup_groups", "dup_group_members", "dup_groups.id, dup_groups.original_image_id, dup_groups.created_at", dupSortClause},
	"alternate": {"alt_groups", "alt_group_members", "alt_groups.id, 0, alt_groups.created_at", altSortClause},
}

func dupSortClause(sort string) string {
	switch sort {
	case "size":
		return `ORDER BY (SELECT COUNT(*) FROM dup_group_members WHERE group_id = dup_groups.id) DESC, dup_groups.id DESC`
	case "original_added":
		return `ORDER BY (SELECT ingested_at FROM images WHERE id = dup_groups.original_image_id) DESC, dup_groups.id DESC`
	}
	return `ORDER BY dup_groups.id DESC`
}

func altSortClause(sort string) string {
	if sort == "size" {
		return `ORDER BY (SELECT COUNT(*) FROM alt_group_members WHERE group_id = alt_groups.id) DESC, alt_groups.id DESC`
	}
	return `ORDER BY alt_groups.id DESC`
}

func sortVersionCards(cards []browseCard, sortKey string) {
	switch sortKey {
	case "length", "size":
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	case "newest_member":
		newest := func(c browseCard) string {
			var best string
			for _, m := range c.Members {
				best = max(best, c.MemberIngestedAt[m])
			}
			return best
		}
		sort.SliceStable(cards, func(i, j int) bool {
			return newest(cards[i]) > newest(cards[j])
		})
	}
}

func sliceWindow(cards []browseCard, offset, limit int) []browseCard {
	if offset >= len(cards) {
		return nil
	}
	return cards[offset:min(offset+limit, len(cards))]
}

func annotateBrowseCardIngestedAt(cx *library.Gallery, cards []browseCard) error {
	if len(cards) == 0 {
		return nil
	}
	idSet := map[int64]struct{}{}
	for _, c := range cards {
		for _, m := range c.Members {
			idSet[m] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	type dateRow struct {
		id       int64
		ingested string
	}
	dates := map[int64]string{}
	err := db.Chunked(slices.Collect(maps.Keys(idSet)), 500, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		rows, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(r *dateRow) []any { return []any{&r.id, &r.ingested} }),
			`SELECT id, ingested_at FROM images WHERE id IN (`+placeholders+`)`,
			args...,
		)
		if err != nil {
			return err
		}
		for _, r := range rows {
			dates[r.id] = humanISOTime(r.ingested)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range cards {
		out := make(map[int64]string, len(cards[i].Members))
		for _, m := range cards[i].Members {
			if d, ok := dates[m]; ok {
				out[m] = d
			}
		}
		cards[i].MemberIngestedAt = out
	}
	return nil
}

func humanISOTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}

func humanISODate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if i := strings.IndexByte(s, 'T'); i > 0 {
			return s[:i]
		}
		return s
	}
	return t.In(time.Local).Format("2006-01-02")
}

func sortedRootsDesc(rootSet map[int64]bool) []int64 {
	roots := slices.Sorted(maps.Keys(rootSet))
	slices.Reverse(roots)
	return roots
}

func finalizeCards(cards []browseCard) ([]browseCard, int) {
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].CreatedAt > cards[j].CreatedAt
	})
	return cards, len(cards)
}

// A chain with any member above the ceiling is dropped whole, so a hidden
// image's relations never show.
func loadVersionChainCards(cx *library.Gallery, ceiling *Ceiling) ([]browseCard, int, error) {
	type edgeMeta struct {
		child, parent int64
		createdAt     string
	}
	list, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(e *edgeMeta) []any { return []any{&e.child, &e.parent, &e.createdAt} }),
		`SELECT child_image_id, parent_image_id, created_at FROM version_edges`)
	if err != nil {
		return nil, 0, err
	}
	edges := map[int64]edgeMeta{}
	childOf := map[int64]int64{} // parent -> child (UNIQUE per schema)
	for _, e := range list {
		edges[e.child] = e
		childOf[e.parent] = e.child
	}
	rootSet := map[int64]bool{}
	for _, em := range edges {
		if _, hasParent := edges[em.parent]; !hasParent {
			rootSet[em.parent] = true
		}
	}
	roots := sortedRootsDesc(rootSet)
	tainted := ceiling.TaintedAmong("version_edges", "child_image_id", "parent_image_id")
	cards := make([]browseCard, 0, len(roots))
	for _, root := range roots {
		members := []int64{root}
		latestTS := ""
		cur := root
		for {
			next, ok := childOf[cur]
			if !ok {
				break
			}
			members = append(members, next)
			latestTS = max(latestTS, edges[next].createdAt)
			cur = next
		}
		if slices.ContainsFunc(members, func(id int64) bool { return tainted[id] }) {
			continue
		}
		cards = append(cards, browseCard{
			Kind:      "version",
			Members:   members,
			CreatedAt: humanISOTime(latestTS),
		})
	}
	cards, total := finalizeCards(cards)
	return cards, total, nil
}

// One card per component, which has several roots when an image was made
// from several sources.
func loadDerivativeTreeCards(cx *library.Gallery, ceiling *Ceiling) ([]browseCard, map[int64][]int64, int, error) {
	type edgeRow struct {
		derivative, source int64
		createdAt          string
	}
	list, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(e *edgeRow) []any { return []any{&e.derivative, &e.source, &e.createdAt} }),
		`SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges`)
	if err != nil {
		return nil, nil, 0, err
	}
	derivativesOf := map[int64][]int64{}
	sourcesOf := map[int64][]int64{}
	derivCreated := map[int64]string{}
	for _, e := range list {
		derivativesOf[e.source] = append(derivativesOf[e.source], e.derivative)
		sourcesOf[e.derivative] = append(sourcesOf[e.derivative], e.source)
		derivCreated[e.derivative] = max(derivCreated[e.derivative], e.createdAt)
	}
	for src := range derivativesOf {
		sort.Slice(derivativesOf[src], func(i, j int) bool {
			return derivativesOf[src][i] < derivativesOf[src][j]
		})
	}
	rootSet := map[int64]bool{}
	for src := range derivativesOf {
		if len(sourcesOf[src]) == 0 {
			rootSet[src] = true
		}
	}
	tainted := ceiling.TaintedAmong("derivative_edges", "derivative_image_id", "source_image_id")
	emitted := map[int64]bool{}
	cards := make([]browseCard, 0, len(rootSet))
	for _, root := range sortedRootsDesc(rootSet) {
		if emitted[root] {
			continue
		}
		members, compRoots := componentOf(root, derivativesOf, sourcesOf)
		for _, m := range members {
			emitted[m] = true
		}
		if slices.ContainsFunc(members, func(id int64) bool { return tainted[id] }) {
			continue
		}
		latestTS := ""
		for _, m := range members {
			latestTS = max(latestTS, derivCreated[m])
		}
		cards = append(cards, browseCard{
			Kind:      "derivative",
			Members:   members,
			Roots:     compRoots,
			CreatedAt: humanISOTime(latestTS),
		})
	}
	cards, total := finalizeCards(cards)
	return cards, sourcesOf, total, nil
}

// X coordinates are per-mille of the graph's width, not pixels, so the
// wires follow the CSS columns at any card width.
const (
	derivSpanX = 1000
	// Must match .deriv-band's height in main.css.
	derivBandH = 44
)

type derivNode struct {
	ID      int64
	Row     int
	Col     int
	Sources []int64
}

type derivSeg struct {
	X1, X2   int
	From, To int64
}

type derivGraph struct {
	Rows  [][]derivNode
	Bands [][]derivSeg
	Cols  int
	Width int
	BandH int
}

func layOutDerivatives(members []int64, sourcesOf map[int64][]int64) *derivGraph {
	if len(members) == 0 {
		return nil
	}
	row := make(map[int64]int, len(members))
	for _, id := range members {
		row[id] = 0
	}
	// Repeated until nothing moves: members are not in dependency order.
	for pass := 0; pass <= relations.MaxVersionChainDepth; pass++ {
		moved := false
		for _, id := range members {
			want := 0
			for _, src := range sourcesOf[id] {
				if r, ok := row[src]; ok && r+1 > want {
					want = r + 1
				}
			}
			if want > row[id] {
				row[id] = want
				moved = true
			}
		}
		if !moved {
			break
		}
	}

	byRow := map[int][]int64{}
	depth := 0
	for _, id := range members {
		byRow[row[id]] = append(byRow[row[id]], id)
		depth = max(depth, row[id])
	}
	g := &derivGraph{Rows: make([][]derivNode, depth+1), Bands: make([][]derivSeg, depth), BandH: derivBandH}
	col := make(map[int64]int, len(members))
	widest := 0
	// Top down, so every source already has its column when parentMean
	// reads it.
	for r := 0; r <= depth; r++ {
		ids := byRow[r]
		slices.Sort(ids)
		if r > 0 {
			slices.SortStableFunc(ids, func(a, b int64) int {
				return cmp.Compare(parentMean(a, sourcesOf, col), parentMean(b, sourcesOf, col))
			})
		}
		g.Rows[r] = make([]derivNode, len(ids))
		for c, id := range ids {
			col[id] = c
			g.Rows[r][c] = derivNode{ID: id, Row: r, Col: c, Sources: sourcesOf[id]}
		}
		widest = max(widest, len(ids))
	}
	g.Cols, g.Width = widest, derivSpanX

	// Counted in half-columns: a shorter row is centred under the widest,
	// as the CSS does, which can leave half a column over.
	centreX := func(id int64) int {
		halves := widest - len(g.Rows[row[id]]) + 2*col[id] + 1
		return halves * derivSpanX / (2 * widest)
	}
	for _, id := range members {
		for _, src := range sourcesOf[id] {
			from, to := row[src], row[id]
			if to <= from {
				continue
			}
			x1, x2, span := centreX(src), centreX(id), to-from
			for b := from; b < to; b++ {
				g.Bands[b] = append(g.Bands[b], derivSeg{
					X1:   x1 + (x2-x1)*(b-from)/span,
					X2:   x1 + (x2-x1)*(b+1-from)/span,
					From: src,
					To:   id,
				})
			}
		}
	}
	return g
}

func parentMean(id int64, sourcesOf map[int64][]int64, col map[int64]int) float64 {
	sum, n := 0, 0
	for _, src := range sourcesOf[id] {
		if c, ok := col[src]; ok {
			sum += c
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

func componentOf(start int64, derivativesOf, sourcesOf map[int64][]int64) (members, roots []int64) {
	seen := map[int64]bool{start: true}
	members = []int64{start}
	for i := 0; i < len(members); i++ {
		n := members[i]
		if len(sourcesOf[n]) == 0 {
			roots = append(roots, n)
		}
		for _, m := range slices.Concat(derivativesOf[n], sourcesOf[n]) {
			if !seen[m] {
				seen[m] = true
				members = append(members, m)
			}
		}
	}
	slices.Sort(members)
	slices.Sort(roots)
	return members, roots
}

func scanGroupMembers(cx *library.Gallery, table string, groupID int64) ([]int64, error) {
	return db.QueryIDs(cx.DB.Read, `SELECT image_id FROM `+table+` WHERE group_id = ? ORDER BY image_id`, groupID)
}

// The first entry is each kind's default.
var browseSortsByKind = map[string][]string{
	"duplicate":   {"recent", "size", "original_added"},
	"alternate":   {"recent", "size"},
	"version":     {"recent", "length", "newest_member"},
	"derivative":  {"recent", "size"},
	"not_related": {"recent"},
}

func resolveBrowseSort(kind, requested string) string {
	allowed := browseSortsByKind[kind]
	if slices.Contains(allowed, requested) {
		return requested
	}
	return allowed[0]
}

const browseRelationsPageSize = 60

func (s *Server) browseRelationsPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	kind := cmp.Or(r.URL.Query().Get("kind"), "duplicate")
	if _, ok := browseSortsByKind[kind]; !ok {
		s.notFoundHandler(w, r)
		return
	}
	page := 1
	requested := 0
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		requested, page = p, p
	}
	// Past this the offset wraps negative and slices out of range.
	page = min(max(page, 1), math.MaxInt/browseRelationsPageSize)
	sort := resolveBrowseSort(kind, r.URL.Query().Get("sort"))
	ceiling := resolveCeiling(r, cx)
	// A version or derivative count comes from the card walk, so the cards
	// load before the page clamp.
	counts := loadRelationsCounts(cx, ceiling, kind)
	offset := (page - 1) * browseRelationsPageSize
	cards, walkedTotal, err := loadBrowseCardsByKind(cx, kind, sort, browseRelationsPageSize, offset, ceiling)
	if err != nil {
		logx.Warnf("browse cards %s: %v", kind, err)
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	switch kind {
	case "version":
		counts.VersionChains = walkedTotal
	case "derivative":
		counts.DerivativeTrees = walkedTotal
	}
	total := kindTotal(counts, kind)
	totalPages := max(1, (total+browseRelationsPageSize-1)/browseRelationsPageSize)
	page = min(page, totalPages)
	if requested != 0 && requested != page {
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		http.Redirect(w, r, "/relations/browse?"+clampedQ.Encode(), http.StatusSeeOther)
		return
	}
	s.renderTemplate(w, "relations_browse.html", browseRelationsData{
		baseData:    s.base(r, "relations", "Browse relations - "+s.booruName()),
		Kind:        kind,
		Cards:       cards,
		Counts:      counts,
		Page:        page,
		TotalPages:  totalPages,
		Sort:        sort,
		SortOptions: browseSortsByKind[kind],
	})
}

func kindTotal(c relationsCounts, kind string) int {
	switch kind {
	case "duplicate":
		return c.DupGroups
	case "alternate":
		return c.AltGroups
	case "version":
		return c.VersionChains
	case "derivative":
		return c.DerivativeTrees
	case "not_related":
		return c.NotRelatedPairs
	}
	return 0
}

type browseRelationsData struct {
	baseData
	Kind        string
	Cards       []browseCard
	Counts      relationsCounts
	Page        int
	TotalPages  int
	Sort        string
	SortOptions []string
}
