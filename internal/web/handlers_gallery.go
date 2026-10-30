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
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// A first search slower than this skips the second count behind "N hidden",
// and that count gets no longer than this either.
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
	FolderTree        []gallery.FolderNode
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
		ctx, cancel := context.WithTimeout(r.Context(), rankPageBudget)
		if rank, err := search.RankInQuery(ctx, s.source(), sq, at); err == nil && rank >= 0 {
			page = rank/pageSize + 1
			sq.Page = page
		}
		cancel()
	}

	htmxGridTarget := isHTMXRequest(r) && r.Header.Get("HX-Target") == "gallery-grid"

	firstStart := time.Now()
	result, err := search.Execute(context.Background(), s.source(), sq)
	if err != nil {
		logx.Errorf("gallery search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}
	firstElapsed := time.Since(firstStart)

	// The detail back link's page is a slot counted from the grid, which a
	// walk off the listing or a changed result set leaves pointing elsewhere.
	if at, err := strconv.ParseInt(q.Get("at"), 10, 64); err == nil && pageStr != "" && pageSize > 0 && !isHTMXRequest(r) &&
		!slices.ContainsFunc(result.Results, func(img models.Image) bool { return img.ID == at }) {
		if held := s.pageHoldingImage(r.Context(), sq, at, page, pageSize); held > 0 && held != page {
			movedQ := r.URL.Query()
			movedQ.Set("page", strconv.Itoa(held))
			movedQ.Del("at")
			http.Redirect(w, r, "/?"+movedQ.Encode(), http.StatusSeeOther)
			return
		}
	}

	totalPages := 1
	if pageSize > 0 {
		totalPages = (result.Total + pageSize - 1) / pageSize
	}

	if (result.Total > 0 && page > totalPages) || pageNonPositive {
		if page > totalPages {
			page = totalPages
			sq.Page = page
			result, err = search.Execute(context.Background(), s.source(), sq)
			if err != nil {
				logx.Errorf("gallery search (clamped): %v", err)
				http.Error(w, "search error", http.StatusInternalServerError)
				return
			}
		}
		if page < 1 {
			// No results leave totalPages at 0; a redirect to page=0 loops.
			page = 1
		}
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedURL := "/?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}

	hiddenByCeiling := 0
	if ceiling.IsActive() {
		bareExpr := search.Parse(queryStr)
		switch {
		case bareExpr == nil:
			if cx := s.active(); cx != nil {
				if n, ok := cx.VisibleCount(); ok && n > result.Total {
					hiddenByCeiling = n - result.Total
				}
			}
		case firstElapsed < galleryHiddenIndicatorBudget:
			ctx, cancel := context.WithTimeout(r.Context(), galleryHiddenIndicatorBudget)
			if n, err := search.HiddenByCeiling(ctx, s.source(), queryStr, ceiling.Level()); err == nil {
				hiddenByCeiling = n
			}
			cancel()
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
		sb = s.sidebarLoad(ids, ceiling)
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
		FolderTree:        sb.Folders,
		SourceLabelCounts: sb.SourceLabels,
		SavedSearches:     sb.Saved,
		EnabledTaggers:    enabledTaggers,
		TaggersPresent:    taggersPresent,
		TaggerReason:      taggerReason,
		PluginSlot:        s.pluginSlot(r, config.SlotBatchBar, 0, ""),
		ActiveTagTerms:    computeActiveTagTerms(queryStr),
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
		data.InboxClusterAtIdx = computeInboxClusters(result.Results, queryStr, page > 1, page < totalPages)
	}
	if inboxFilterActive(expr) {
		data.InboxUploadActive = true
		data.AcceptFileTypes = gallery.SupportedMIMETypes
	}
	data.HiddenByCeiling = hiddenByCeiling
	// The fragment uses it too, for its collapsed sidebar placeholder.
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"), ids)

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

type sidebarBundle struct {
	Tags         []models.Tag
	Folders      []gallery.FolderNode
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
func (s *Server) sidebarLoad(pageImageIDs []int64, ceiling *Ceiling) sidebarBundle {
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
		sb.Folders, _ = cx.FolderTreeUnder(ceiling)
		sb.SourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
	}
	wg.Wait()
	return sb
}

func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	ceiling := resolveCeiling(r, s.active())

	// Empty ids is an empty page; only a missing ids param re-runs the search.
	var ids []int64
	if q.Has("ids") {
		ids = parseIDList(q["ids"])
	} else {
		sortStr := cmp.Or(q.Get("sort"), "newest")
		orderStr := cmp.Or(q.Get("order"), search.DefaultOrder(sortStr))
		page := 1
		if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
			page = p
		}
		var randomSeed int64
		if sortStr == "random" {
			if seed, err := strconv.ParseInt(q.Get("seed"), 10, 64); err == nil {
				randomSeed = seed
			}
		}
		expr := search.Parse(queryStr)
		expr = ceiling.Apply(expr)
		sq := search.Query{
			Expr:       expr,
			Sort:       sortStr,
			Order:      orderStr,
			RandomSeed: randomSeed,
			Page:       page,
			Limit:      s.pageSize(r),
			SkipCount:  true,
		}
		result, err := search.Execute(context.Background(), s.source(), sq)
		if err != nil {
			logx.Errorf("sidebar search: %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		ids = make([]int64, 0, len(result.Results))
		for _, img := range result.Results {
			ids = append(ids, img.ID)
		}
	}

	sb := s.sidebarLoad(ids, ceiling)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":             queryStr,
		"SidebarTags":       sb.Tags,
		"FolderTree":        sb.Folders,
		"SourceLabelCounts": sb.SourceLabels,
		"SavedSearches":     sb.Saved,
		"ActiveTagTerms":    computeActiveTagTerms(queryStr),
	})
}

func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	sb := s.sidebarLoad(nil, resolveCeiling(r, s.active()))
	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":             r.URL.Query().Get("q"),
		"FolderTree":        sb.Folders,
		"SourceLabelCounts": sb.SourceLabels,
		"SavedSearches":     sb.Saved,
	})
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

func buildSidebarURL(q, sort, order, page, seed string, ids []int64) string {
	v := setNonEmpty(url.Values{}, "q", q, "sort", sort, "order", order, "page", page, "seed", seed)
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
