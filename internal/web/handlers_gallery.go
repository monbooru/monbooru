package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// The count behind "N hidden" gets no longer than this.
var galleryHiddenIndicatorBudget = 300 * time.Millisecond

const batchGapMinutes = 15

type galleryData struct {
	baseData
	Query             string
	SearchWarning     string
	Sort              string
	Order             string
	ThumbnailFit      string
	ThumbSize         string
	RandomSeed        int64
	Page              int
	TotalPages        int
	PageSize          int
	PageSizeChoices   []PageSizeChoice
	StatusOOB         bool
	Result            *models.SearchResult
	SidebarTags       []models.Tag
	Folders           *folderLevel
	SourceLabelCounts []gallery.SourceLabelCount
	SavedSearches     []models.SavedSearch
	SidebarURL        string
	EnabledTaggers    []tagger.TaggerStatus
	TaggersPresent    bool
	TaggerReason      string
	ActiveTagTerms    map[string]bool
	InboxClusterAtIdx []*inboxCluster
	InboxUploadActive bool
	AcceptFileTypes   string
	SimilarityPercent map[int64]int
	SortSelectOOB     bool
	PluginSlot        pluginSlotView
	CountURL          string
	// What the searches behind this listing took on the server; the
	// deferred count adds its own.
	SearchTime time.Duration
	PagerPrev  string
	PagerNext  string
	// A cursor page's label drifts off its place while matches arrive or
	// leave above it; these say rows lie past an edge the label hides.
	PagerMorePrev bool
	PagerMoreNext bool
}

type inboxCluster struct {
	Count      int
	DateLabel  string
	RangeLabel string
	RangeLink  string
	RangeQuery string
	Whole      bool // every row of the batch is on this page
}

func (s *Server) galleryHandler(w http.ResponseWriter, r *http.Request) {
	// The grid swap pushes this URL, so a Back that misses the bfcache
	// would otherwise be served the cached fragment as the page.
	w.Header().Add("Vary", "HX-Target")
	q := r.URL.Query()
	queryStr := q.Get("q")
	expr := search.Parse(queryStr)
	sortStr := q.Get("sort")
	orderStr := q.Get("order")
	if sortStr == "" && search.HasSimilarTerm(expr) {
		sortStr = "similarity"
	}
	// The executor falls back to newest too; this keeps the Sort select honest.
	if sortStr == "similarity" && !search.HasSimilarTerm(expr) {
		sortStr = "newest"
	}
	if sortStr == "" && orderStr == "" && collectionFilterActive(expr) {
		sortStr, orderStr = "order", "asc"
	}
	sortStr = cmp.Or(sortStr, "newest")
	orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
	pageStr := q.Get("page")
	page := 1
	pageNonPositive := false
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil {
			if p > 0 {
				page = p
			} else {
				pageNonPositive = true
			}
		}
	}

	var randomSeed int64
	if sortStr == "random" {
		if seedStr := q.Get("seed"); seedStr != "" {
			if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
				randomSeed = s
			}
		}
		if randomSeed == 0 {
			var seed [4]byte
			_, _ = rand.Read(seed[:])
			randomSeed = int64(binary.BigEndian.Uint32(seed[:]) | 1)
			newQ := r.URL.Query()
			newQ.Set("seed", strconv.FormatInt(randomSeed, 10))
			if isHTMXRequest(r) {
				// Push URL with seed so the next poll keeps the same order.
				w.Header().Set("HX-Push-Url", "/?"+newQ.Encode())
			} else {
				http.Redirect(w, r, "/?"+newQ.Encode(), http.StatusSeeOther)
				return
			}
		}
	}

	ceiling := resolveCeiling(r, s.active())
	pageSize := s.pageSize(r)
	sq := viewQuery(expr, queryStr, sortStr, orderStr, ceiling)
	sq.RandomSeed, sq.Page, sq.Limit = randomSeed, page, pageSize
	sq.DeferCount = true
	sq.After, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	sq.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	expr = sq.Expr
	// Checked after ceiling.Apply: the cached count ignores the ceiling.
	if expr == nil {
		if cx := s.active(); cx != nil {
			if n, ok := cx.VisibleCount(); ok {
				sq.PresetTotal = &n
			}
		}
	}

	if at, err := strconv.ParseInt(q.Get("at"), 10, 64); err == nil && pageStr == "" && pageSize > 0 {
		// A cursor would still pick the rows under the page the rank names.
		sq.After, sq.Before = 0, 0
		ctx, cancel := context.WithTimeout(r.Context(), rankPageBudget)
		if rank, err := search.RankInQuery(ctx, s.source(), sq, at); err == nil && rank >= 0 {
			page = rank/pageSize + 1
			sq.Page = page
		}
		cancel()
	}

	htmxGridTarget := isHTMXRequest(r) && r.Header.Get("HX-Target") == "gallery-grid"

	searchStart := time.Now()
	result, err := search.Execute(r.Context(), s.source(), sq)
	took := time.Since(searchStart)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		logx.Errorf("gallery search: %v", err)
		hxRefuse(w, r, http.StatusInternalServerError, "Search error.")
		return
	}

	// The detail back link's page is a slot counted from the grid, which a
	// walk off the listing or a changed result set leaves pointing elsewhere.
	if at, err := strconv.ParseInt(q.Get("at"), 10, 64); err == nil && pageStr != "" && pageSize > 0 && !isHTMXRequest(r) &&
		!slices.ContainsFunc(result.Results, func(img models.Image) bool { return img.ID == at }) {
		if held := s.pageHoldingImage(r.Context(), sq, at, page, pageSize); held > 0 && held != page {
			movedQ := r.URL.Query()
			movedQ.Set("page", strconv.Itoa(held))
			movedQ.Del("at")
			movedQ.Del("after")
			movedQ.Del("before")
			http.Redirect(w, r, "/?"+movedQ.Encode(), http.StatusSeeOther)
			return
		}
	}

	totalPages := 1
	switch {
	case result.TotalUnknown:
		totalPages = page
	case pageSize > 0:
		totalPages = (result.Total + pageSize - 1) / pageSize
	}

	// A cursor read that found rows is the page the link promised, whatever
	// the count now makes of its label.
	cursorRows := (sq.After != 0 || sq.Before != 0) && len(result.Results) > 0
	if cursorRows {
		totalPages = max(totalPages, page)
	}
	if (!result.TotalUnknown && result.Total > 0 && page > totalPages) || pageNonPositive {
		if page > totalPages {
			page = totalPages
			sq.Page, sq.After, sq.Before = page, 0, 0
			searchStart = time.Now()
			result, err = search.Execute(r.Context(), s.source(), sq)
			took += time.Since(searchStart)
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				logx.Errorf("gallery search (clamped): %v", err)
				hxRefuse(w, r, http.StatusInternalServerError, "Search error.")
				return
			}
		}
		if page < 1 {
			// No results leave totalPages at 0; a redirect to page=0 loops.
			page = 1
		}
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedQ.Del("after")
		clampedQ.Del("before")
		clampedURL := "/?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}

	hiddenByCeiling, hiddenLater := 0, false
	if ceiling.IsActive() {
		if search.Parse(queryStr) != nil {
			hiddenLater = true
		} else if cx := s.active(); cx != nil {
			if n, ok := cx.VisibleCount(); ok && n > result.Total {
				hiddenByCeiling = n - result.Total
			}
		}
	}

	// Only the uncollapsed HTMX fragment renders the sidebar inline; a full
	// page lazy-loads it so first paint doesn't wait on the sidebar reads.
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}

	var sb sidebarBundle
	if htmxGridTarget && !sidebarCollapsed(r) {
		sb = s.sidebarLoad(r, queryStr, ids, ceiling)
	}

	enabledTaggers, taggersPresent, taggerReason := tagger.GalleryTaggers(s.cfgSnapshot(), s.activeGallery())
	data := galleryData{
		baseData:          s.base(r, "gallery", "Images - "+s.booruName()),
		Query:             queryStr,
		SearchWarning:     searchWarning(queryStr, expr),
		Sort:              sortStr,
		Order:             orderStr,
		ThumbnailFit:      s.thumbnailFit(),
		ThumbSize:         thumbSize(r),
		RandomSeed:        randomSeed,
		Page:              page,
		TotalPages:        totalPages,
		PageSize:          pageSize,
		PageSizeChoices:   s.pageSizeChoices(r),
		Result:            result,
		SidebarTags:       sb.Tags,
		Folders:           sb.Folders,
		SourceLabelCounts: sb.SourceLabels,
		SavedSearches:     sb.Saved,
		EnabledTaggers:    enabledTaggers,
		TaggersPresent:    taggersPresent,
		TaggerReason:      taggerReason,
		PluginSlot:        s.pluginSlot(r, config.SlotBatchBar, 0, ""),
		ActiveTagTerms:    computeActiveTagTerms(queryStr),
		SearchTime:        took,
	}
	if seedID, ok := search.SimilaritySeedID(expr); ok {
		visible, _ := s.active().VisibleCount()
		scores, err := tags.OverlapPercentsAgainst(s.db(), seedID, ids, visible)
		if err != nil {
			logx.Debugf("gallery similarity scores: %v", err)
		}
		data.SimilarityPercent = scores
	}
	if inboxClustersActive(sortStr, orderStr, expr) {
		data.InboxClusterAtIdx = computeInboxClusters(result.Results, queryStr, page > 1, page < totalPages || result.TotalUnknown)
	}
	if inboxFilterActive(expr) {
		data.InboxUploadActive = true
		data.AcceptFileTypes = gallery.SupportedMIMETypes
	}
	data.HiddenByCeiling = hiddenByCeiling
	var first, last int64
	if n := len(result.Results); n > 0 && search.CursorPaging(sq) {
		first, last = result.Results[0].ID, result.Results[n-1].ID
		data.PagerPrev, data.PagerNext = pagerCursor(first, last)
		if page == 1 && cursorRows {
			data.PagerMorePrev = s.cursorPageEdge(r.Context(), sq, first, 0)
		}
		if cursorRows && !result.TotalUnknown && n >= pageSize && page >= totalPages {
			data.PagerMoreNext = s.cursorPageEdge(r.Context(), sq, 0, last)
		}
	}
	if result.TotalUnknown || hiddenLater {
		data.CountURL = buildCountURL(queryStr, sortStr, orderStr, page, randomSeed, result.TotalUnknown, first, last, data.PagerMorePrev, cursorRows && len(result.Results) >= pageSize, took)
	}
	// The fragment uses it too, for its collapsed sidebar placeholder.
	data.SidebarURL = buildSidebarURL(queryStr, ids)

	if htmxGridTarget {
		// The sort select and status bar sit outside the swap target, so
		// only the fragment sends them out of band.
		data.SortSelectOOB = true
		data.StatusOOB = true
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	// The batch-strip dialog needs source labels the lazy sidebar won't bring.
	if cx := s.active(); cx != nil {
		data.SourceLabelCounts, _ = cx.SourceLabelCounts()
	}
	s.renderTemplate(w, "gallery.html", data)
}

// cursorPageEdge tells whether one match lies before or after a cursor
// page's edge row, where its label says there is none.
func (s *Server) cursorPageEdge(ctx context.Context, sq search.Query, before, after int64) bool {
	q := sq
	q.Page, q.Limit, q.Before, q.After = 1, 1, before, after
	q.SkipCount, q.DeferCount, q.Cached, q.PresetTotal = true, false, false, nil
	res, err := search.Execute(ctx, s.source(), q)
	return err == nil && len(res.Results) > 0
}

// Next and Prev read on from the page's last or first row rather than an
// offset, so new images landing on top don't shift the pages being walked.
func pagerCursor(first, last int64) (prev, next string) {
	return "&before=" + strconv.FormatInt(first, 10), "&after=" + strconv.FormatInt(last, 10)
}

// The count fragment re-renders the pagers, so it takes the listing's
// resolved sort, order and seed and the page's first and last ids. The
// count's time adds to the rows' time, which rides along in microseconds.
func buildCountURL(q, sort, order string, page int, seed int64, count bool, first, last int64, morePrev, fullCursorPage bool, took time.Duration) string {
	v := setNonEmpty(url.Values{}, "q", q, "sort", sort, "order", order, "page", strconv.Itoa(page))
	if seed != 0 {
		v.Set("seed", strconv.FormatInt(seed, 10))
	}
	if count {
		v.Set("count", "1")
		v.Set("took", strconv.FormatInt(took.Microseconds(), 10))
	}
	if first != 0 {
		v.Set("first", strconv.FormatInt(first, 10))
		v.Set("last", strconv.FormatInt(last, 10))
	}
	if morePrev {
		v.Set("more_prev", "1")
	}
	if fullCursorPage {
		v.Set("full_cursor", "1")
	}
	return "/internal/search/count?" + v.Encode()
}

// galleryCount fills in what the gallery render left out: a total its page
// could not tell, and the images the rating ceiling hides from the search.
func (s *Server) galleryCount(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	data := galleryData{Query: queryStr, Sort: cmp.Or(q.Get("sort"), "newest"), Page: 1}
	data.Order = cmp.Or(q.Get("order"), search.DefaultOrder(data.Sort))
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		data.Page = p
	}
	if seed, err := strconv.ParseInt(q.Get("seed"), 10, 64); err == nil {
		data.RandomSeed = seed
	}
	first, _ := strconv.ParseInt(q.Get("first"), 10, 64)
	last, _ := strconv.ParseInt(q.Get("last"), 10, 64)
	if first != 0 && last != 0 {
		data.PagerPrev, data.PagerNext = pagerCursor(first, last)
	}
	ceiling := resolveCeiling(r, s.active())
	data.PagerMorePrev = q.Get("more_prev") == "1"
	if q.Get("count") == "1" {
		sq := viewQuery(search.Parse(queryStr), queryStr, data.Sort, data.Order, ceiling)
		sq.RandomSeed = data.RandomSeed
		took, _ := strconv.ParseInt(q.Get("took"), 10, 64)
		countStart := time.Now()
		total, err := search.Count(r.Context(), s.source(), sq)
		data.SearchTime = time.Duration(max(took, 0))*time.Microsecond + time.Since(countStart)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			logx.Errorf("gallery count: %v", err)
			hxRefuse(w, r, http.StatusInternalServerError, "The match count failed.")
			return
		}
		data.Result = &models.SearchResult{Total: total}
		if pageSize := s.pageSize(r); pageSize > 0 {
			data.TotalPages = (total + pageSize - 1) / pageSize
		}
		if q.Get("full_cursor") == "1" {
			data.TotalPages = max(data.TotalPages, data.Page)
			if data.Page >= data.TotalPages {
				data.PagerMoreNext = s.cursorPageEdge(r.Context(), sq, 0, last)
			}
		}
	}
	if ceiling.IsActive() && search.Parse(queryStr) != nil {
		ctx, cancel := context.WithTimeout(r.Context(), galleryHiddenIndicatorBudget)
		if n, err := search.HiddenByCeiling(ctx, s.source(), queryStr, ceiling.Level()); err == nil {
			data.HiddenByCeiling = n
		}
		cancel()
	}
	s.renderTemplate(w, "partials/gallery_count.html", data)
}

type sidebarBundle struct {
	Tags         []models.Tag
	Folders      *folderLevel
	SourceLabels []gallery.SourceLabelCount
	Saved        []models.SavedSearch
}

func (s *Server) pageHoldingImage(ctx context.Context, sq search.Query, id int64, arrived, pageSize int) int {
	if page, listed := search.PageOf(s.source(), sq, id, pageSize); listed {
		return page
	}
	ctx, cancel := context.WithTimeout(ctx, rankPageBudget)
	defer cancel()
	if page, ok := s.pageHoldingMatch(ctx, sq, id, arrived, pageSize); ok {
		return page
	}
	rank, err := search.RankInQuery(ctx, s.source(), sq, id)
	if err != nil || rank < 0 {
		return 0
	}
	return rank/pageSize + 1
}

// Only the reads that always hit the DB fan out: a goroutine per cached
// cx read takes a read-pool slot and costs more than it saves.
func (s *Server) sidebarLoad(r *http.Request, query string, pageImageIDs []int64, ceiling *Ceiling) sidebarBundle {
	var sb sidebarBundle
	var wg sync.WaitGroup
	wg.Go(func() {
		sb.Tags, _ = search.SidebarTagsWithGlobalCount(s.source(), pageImageIDs)
	})
	wg.Go(func() {
		var err error
		if sb.Saved, err = search.SavedSearches(s.db()); err != nil {
			logx.Warnf("sidebar saved searches: %v", err)
		}
	})
	if cx := s.active(); cx != nil {
		sb.Folders = folderSection(r, cx, ceiling, query)
		sb.SourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
	}
	wg.Wait()
	return sb
}

func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	ceiling := resolveCeiling(r, s.active())

	ids := parseIDList(q["ids"])
	sb := s.sidebarLoad(r, queryStr, ids, ceiling)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":             queryStr,
		"SidebarTags":       sb.Tags,
		"Folders":           sb.Folders,
		"SourceLabelCounts": sb.SourceLabels,
		"SavedSearches":     sb.Saved,
		"ActiveTagTerms":    computeActiveTagTerms(queryStr),
	})
}

func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	queryStr := r.URL.Query().Get("q")
	sb := s.sidebarLoad(r, queryStr, nil, resolveCeiling(r, s.active()))
	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":             queryStr,
		"Folders":           sb.Folders,
		"SourceLabelCounts": sb.SourceLabels,
		"SavedSearches":     sb.Saved,
	})
}

const (
	folderLevelRows     = 500
	folderRowsPerRender = 2000
)

// A level that is not Loaded renders as an empty list that fetches its rows
// from folderChildren once main.js shows it.
type folderLevel struct {
	Path   string
	Query  string
	Loaded bool
	// The root level's first page leads with the "/" row.
	Root  bool
	Count int
	Rows  []folderRow
	More  int
	Left  int
}

type folderRow struct {
	gallery.FolderNode
	Sub *folderLevel
}

// The levels the server renders: those the cookie holds open, and every
// one on the way to the folder the search names.
type folderOpen struct {
	keys    map[string]bool
	current string
}

func readFolderOpen(r *http.Request, query string) folderOpen {
	return folderOpen{keys: folderCookieKeys(r), current: queryFolder(query)}
}

func (o folderOpen) onPath(path string) bool {
	return o.current == path || strings.HasPrefix(o.current, path+"/")
}

// A closed section reads no tree: on a {hash:4} layout it holds tens of
// thousands of rows.
func folderSection(r *http.Request, cx *library.Gallery, ceiling *Ceiling, query string) *folderLevel {
	o := readFolderOpen(r, query)
	if !o.keys["__tree__"] && o.current == "" {
		return &folderLevel{Query: query}
	}
	tree, err := cx.FolderTreeUnder(ceiling)
	root := gallery.FindFolder(tree, "")
	if err != nil || root == nil {
		return &folderLevel{Query: query}
	}
	budget := folderRowsPerRender
	return o.level(root, 0, query, &budget)
}

// The folder the search names stays among the first page's rows past the
// cap, so the later pages leave it out.
func (o folderOpen) level(node *gallery.FolderNode, offset int, query string, budget *int) *folderLevel {
	lv := &folderLevel{Path: node.Path, Query: query, Loaded: true}
	if node.Path == "" && offset == 0 {
		lv.Root, lv.Count = true, node.Count
	}
	kids := node.Children
	pin := -1
	if o.current != "" {
		pin = slices.IndexFunc(kids, func(c gallery.FolderNode) bool { return o.onPath(c.Path) })
	}
	end := min(offset+folderLevelRows, len(kids))
	var shown []int
	for i := offset; i < end; i++ {
		if offset == 0 || i != pin {
			shown = append(shown, i)
		}
	}
	left := len(kids) - max(end, offset)
	if pin >= end {
		left--
		if offset == 0 {
			shown = append(shown, pin)
		}
	}
	if left > 0 {
		lv.More, lv.Left = end, left
	}
	*budget -= len(shown)
	lv.Rows = make([]folderRow, len(shown))
	for j, i := range shown {
		lv.Rows[j] = o.row(&kids[i], query, budget)
	}
	return lv
}

func (o folderOpen) row(n *gallery.FolderNode, query string, budget *int) folderRow {
	row := folderRow{FolderNode: *n}
	switch {
	case len(n.Children) == 0:
	case *budget > 0 && (o.keys[n.Path] || (o.current != "" && o.onPath(n.Path))):
		row.Sub = o.level(n, 0, query, budget)
	default:
		row.Sub = &folderLevel{Path: n.Path, Query: query}
	}
	return row
}

// A path no visible image sits under answers no rows: the folder can go
// away between the render and the click.
func (s *Server) folderChildren(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	if cx == nil {
		return
	}
	tree, err := cx.FolderTreeUnder(resolveCeiling(r, cx))
	if err != nil {
		logx.Errorf("folder children: %v", err)
		hxRefuse(w, r, http.StatusInternalServerError, "The folder list failed.")
		return
	}
	q := r.URL.Query()
	node := gallery.FindFolder(tree, q.Get("path"))
	if node == nil {
		return
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	query := q.Get("q")
	budget := folderRowsPerRender
	s.renderTemplate(w, "folder_rows", readFolderOpen(r, query).level(node, max(offset, 0), query, &budget))
}

func queryFolder(query string) string {
	var path string
	search.WalkAndedLeaves(search.Parse(query), func(e search.Expr) {
		if v, ok := e.(search.FilterExpr); ok && path == "" && (v.Key == "folder" || v.Key == "folderonly") {
			path = v.Val
		}
	})
	return path
}

type PageSizeChoice struct {
	Value    string
	Label    string
	Title    string
	Selected bool
}

// The Default button is the only way back to a configured size the ramp
// lacks; it sits among the sizes in order and stands in for an equal one.
func (s *Server) pageSizeChoices(r *http.Request) []PageSizeChoice {
	s.cfgMu.RLock()
	configured := s.cfg.UI.PageSize
	s.cfgMu.RUnlock()

	override := pageSizeOverride(r)
	def := PageSizeChoice{
		Value:    "default",
		Label:    strconv.Itoa(configured),
		Title:    "Default, set in Settings",
		Selected: override == 0 || override == configured,
	}
	out := make([]PageSizeChoice, 0, len(PageSizeOptions)+1)
	placed := false
	for _, n := range PageSizeOptions {
		if !placed && configured <= n {
			out = append(out, def)
			placed = true
			if configured == n {
				continue
			}
		}
		label := strconv.Itoa(n)
		out = append(out, PageSizeChoice{Value: label, Label: label, Selected: override == n})
	}
	if !placed {
		out = append(out, def)
	}
	return out
}

func buildSidebarURL(q string, ids []int64) string {
	v := setNonEmpty(url.Values{}, "q", q)
	var sb strings.Builder
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	v.Set("ids", sb.String())
	return "/internal/sidebar?" + v.Encode()
}

// NOT, OR and wildcard leaves stay out: the toggle can't remove them cleanly.
func computeActiveTagTerms(query string) map[string]bool {
	set := make(map[string]bool)
	expr := search.Parse(query)
	if expr == nil {
		return set
	}
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		switch v := e.(type) {
		case search.TagExpr:
			if v.Tag != "" && v.Wildcard == "" {
				set[v.Tag] = true
			}
		case search.FilterExpr:
			set[v.Key+":"+strings.ToLower(v.Val)] = true
		}
	})
	return set
}

func inboxFilterActive(expr search.Expr) bool {
	return filterActive(expr, "inbox", func(val string) bool { return strings.ToLower(val) == "true" })
}

func collectionFilterActive(expr search.Expr) bool {
	return filterActive(expr, "collection", func(val string) bool { return val != "" })
}

func filterActive(expr search.Expr, key string, match func(string) bool) bool {
	if expr == nil {
		return false
	}
	var found bool
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		if v, ok := e.(search.FilterExpr); ok && v.Key == key && match(v.Val) {
			found = true
		}
	})
	return found
}

func searchWarning(query string, expr search.Expr) string {
	var parts []string
	if unreadable := search.Unreadable(query); len(unreadable) > 0 {
		parts = append(parts, "Could not read: "+strings.Join(unreadable, ", "))
	}
	if unknown := unknownFilterValues(expr); len(unknown) > 0 {
		parts = append(parts, "Unknown filter value: "+strings.Join(unknown, ", "))
	}
	return strings.Join(parts, ". ")
}

func unknownFilterValues(expr search.Expr) []string {
	if expr == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	search.WalkLeaves(expr, func(e search.Expr) bool {
		if v, ok := e.(search.FilterExpr); ok && !searchkw.ValueKnown(v.Key, v.Val) {
			token := v.Key + ":" + v.Val
			if !seen[token] {
				seen[token] = true
				out = append(out, token)
			}
		}
		return true
	})
	return out
}

// Clusters need newest-first rows; the upload zone shows under any sort.
func inboxClustersActive(sort, order string, expr search.Expr) bool {
	if sort != "newest" || order != "desc" {
		return false
	}
	return inboxFilterActive(expr)
}

func computeInboxClusters(images []models.Image, queryStr string, moreAbove, moreBelow bool) []*inboxCluster {
	if len(images) == 0 {
		return nil
	}
	markers := make([]*inboxCluster, len(images))
	gap := time.Duration(batchGapMinutes) * time.Minute
	start := 0
	for i := 1; i <= len(images); i++ {
		closeCluster := i == len(images)
		if !closeCluster {
			prevB, nextB := images[i-1].UploadBatch, images[i].UploadBatch
			switch {
			case prevB != nil || nextB != nil:
				closeCluster = !sameBatch(prevB, nextB)
			default:
				// >= so a 15-minute schedule gets one cluster per run.
				closeCluster = images[i-1].IngestedAt.Sub(images[i].IngestedAt) >= gap
			}
		}
		if closeCluster {
			spills := (start == 0 && moreAbove) || (i == len(images) && moreBelow)
			markers[start] = buildInboxCluster(images[start:i], queryStr, !spills)
			start = i
		}
	}
	return markers
}

func sameBatch(a, b *int64) bool { return a != nil && b != nil && *a == *b }

func buildInboxCluster(rows []models.Image, queryStr string, whole bool) *inboxCluster {
	// Local time: the date: filter reads its bounds in the local zone.
	newest := rows[0].IngestedAt.In(time.Local)
	oldest := rows[len(rows)-1].IngestedAt.In(time.Local)
	dateLabel := newest.Format("2006-01-02")
	rangeLabel := oldest.Format("15:04")
	if len(rows) > 1 && !oldest.Equal(newest) {
		rangeLabel = oldest.Format("15:04") + " -> " + newest.Format("15:04")
	}
	// Minute-precise date bounds would take other uploads from that minute too.
	leaf := " date:" + oldest.Format("2006-01-02T15:04") + ".." + newest.Format("2006-01-02T15:04")
	if rows[0].UploadBatch != nil {
		leaf = " batch:" + strconv.FormatInt(*rows[0].UploadBatch, 10)
	}
	clusterQ := "inbox:true" + leaf
	if queryStr != "" && queryStr != "inbox:true" {
		clusterQ = queryStr + leaf
	}
	return &inboxCluster{
		Count:      len(rows),
		DateLabel:  dateLabel,
		RangeLabel: rangeLabel,
		RangeLink:  "/?" + url.Values{"q": []string{clusterQ}}.Encode(),
		RangeQuery: clusterQ,
		Whole:      whole,
	}
}
