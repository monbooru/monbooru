package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
)

func TestComputeInboxClusters_Empty(t *testing.T) {
	if got := computeInboxClusters(nil, "", false, false); got != nil {
		t.Fatalf("empty input: got %v, want nil", got)
	}
}

func TestComputeInboxClusters_Singleton(t *testing.T) {
	base := time.Date(2026, 5, 22, 14, 32, 0, 0, time.Local)
	imgs := []models.Image{{ID: 1, IngestedAt: base}}
	got := computeInboxClusters(imgs, "", false, false)
	if len(got) != 1 || got[0] == nil {
		t.Fatalf("singleton: got %v, want one cluster marker", got)
	}
	if got[0].Count != 1 || got[0].DateLabel != "2026-05-22" {
		t.Errorf("singleton header = %+v", got[0])
	}
}

func TestComputeInboxClusters_ExactBoundary(t *testing.T) {
	base := time.Date(2026, 5, 22, 14, 32, 0, 0, time.UTC)
	t.Run("equal-to-gap", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Duration(batchGapMinutes) * time.Minute)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "", false, false)
		if got[0] == nil || got[1] == nil {
			t.Fatalf("equal-to-gap: expected two clusters, got %+v / %+v", got[0], got[1])
		}
		if got[0].Count != 1 || got[1].Count != 1 {
			t.Errorf("counts = %d, %d; want 1, 1", got[0].Count, got[1].Count)
		}
	})
	t.Run("one-second-under-gap", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Duration(batchGapMinutes)*time.Minute - time.Second)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "", false, false)
		if got[0] == nil || got[1] != nil {
			t.Fatalf("one-second-under-gap: expected single cluster, got %+v / %+v", got[0], got[1])
		}
		if got[0].Count != 2 {
			t.Errorf("count = %d, want 2", got[0].Count)
		}
	})
}

func TestComputeInboxClusters_MixedSizes(t *testing.T) {
	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, 5, day, hour, minute, 0, 0, time.Local)
	}
	imgs := []models.Image{
		{ID: 1, IngestedAt: at(22, 14, 36)},
		{ID: 2, IngestedAt: at(22, 14, 34)},
		{ID: 3, IngestedAt: at(22, 14, 32)},
		{ID: 4, IngestedAt: at(22, 9, 14)},
		{ID: 5, IngestedAt: at(21, 22, 20)},
		{ID: 6, IngestedAt: at(21, 22, 8)},
	}
	got := computeInboxClusters(imgs, "inbox:true", false, false)
	wantStarts := map[int]int{0: 3, 3: 1, 4: 2}
	for i, c := range got {
		if want, ok := wantStarts[i]; ok {
			if c == nil {
				t.Errorf("idx %d: expected header marker, got nil", i)
				continue
			}
			if c.Count != want {
				t.Errorf("idx %d count = %d, want %d", i, c.Count, want)
			}
		} else if c != nil {
			t.Errorf("idx %d: expected nil, got %+v", i, c)
		}
	}
	if got[0] != nil && got[0].RangeLabel != "14:32 -> 14:36" {
		t.Errorf("cluster 0 range = %q", got[0].RangeLabel)
	}
	if got[4] != nil && got[4].RangeLabel != "22:08 -> 22:20" {
		t.Errorf("cluster 2 range = %q", got[4].RangeLabel)
	}
}

func TestComputeInboxClusters_UploadBatch(t *testing.T) {
	base := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	batch := func(v int64) *int64 { return &v }

	t.Run("different batches within the gap split", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Minute), UploadBatch: batch(200)},
			{ID: 1, IngestedAt: base, UploadBatch: batch(100)},
		}
		got := computeInboxClusters(imgs, "inbox:true", false, false)
		if got[0] == nil || got[1] == nil {
			t.Fatalf("expected two clusters, got %+v / %+v", got[0], got[1])
		}
	})

	t.Run("same batch beyond the gap stays one", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(30 * time.Minute), UploadBatch: batch(100)},
			{ID: 1, IngestedAt: base, UploadBatch: batch(100)},
		}
		got := computeInboxClusters(imgs, "inbox:true", false, false)
		if got[0] == nil || got[1] != nil {
			t.Fatalf("expected one cluster, got %+v / %+v", got[0], got[1])
		}
		if got[0].Count != 2 {
			t.Errorf("count = %d, want 2", got[0].Count)
		}
	})

	t.Run("upload row adjacent to a watcher row splits", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Minute), UploadBatch: batch(100)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "inbox:true", false, false)
		if got[0] == nil || got[1] == nil {
			t.Fatalf("expected two clusters, got %+v / %+v", got[0], got[1])
		}
	})

	t.Run("the query names the batch, not the minute it landed in", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Second), UploadBatch: batch(200)},
			{ID: 1, IngestedAt: base, UploadBatch: batch(100)},
		}
		got := computeInboxClusters(imgs, "inbox:true", false, false)
		if got[0].RangeQuery != "inbox:true batch:200" || got[1].RangeQuery != "inbox:true batch:100" {
			t.Fatalf("queries = %q / %q", got[0].RangeQuery, got[1].RangeQuery)
		}
	})

	t.Run("a tokenless cluster keeps the time range", func(t *testing.T) {
		got := computeInboxClusters([]models.Image{{ID: 1, IngestedAt: base}}, "inbox:true", false, false)
		if !strings.Contains(got[0].RangeQuery, "date:") {
			t.Fatalf("query = %q, want a date range", got[0].RangeQuery)
		}
	})

	t.Run("tokenless rows still use the time gap", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Minute)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "inbox:true", false, false)
		if got[0] == nil || got[1] != nil {
			t.Fatalf("expected one cluster, got %+v / %+v", got[0], got[1])
		}
	})
}

func TestComputeInboxClusters_Whole(t *testing.T) {
	base := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	gap := time.Duration(batchGapMinutes) * time.Minute
	imgs := []models.Image{
		{ID: 3, IngestedAt: base.Add(2 * gap)},
		{ID: 2, IngestedAt: base.Add(gap)},
		{ID: 1, IngestedAt: base},
	}

	t.Run("one page holds every batch", func(t *testing.T) {
		got := computeInboxClusters(imgs, "inbox:true", false, false)
		for i, c := range got {
			if c == nil || !c.Whole {
				t.Errorf("cluster at %d: whole = %v, want true", i, c)
			}
		}
	})

	t.Run("the edges the listing runs past are open", func(t *testing.T) {
		got := computeInboxClusters(imgs, "inbox:true", true, true)
		if got[0].Whole {
			t.Error("top cluster: whole = true, want false with newer rows on the page before")
		}
		if !got[1].Whole {
			t.Error("middle cluster: whole = false, want true")
		}
		if got[2].Whole {
			t.Error("bottom cluster: whole = true, want false with older rows on the page after")
		}
	})
}

func TestInboxClustersActive(t *testing.T) {
	cases := []struct {
		name  string
		q     string
		sort  string
		order string
		want  bool
	}{
		{"newest desc + inbox:true", "inbox:true", "newest", "desc", true},
		{"newest desc + inbox:true with extra leaf", "inbox:true cat:character", "newest", "desc", true},
		{"newest asc + inbox:true", "inbox:true", "newest", "asc", false},
		{"filesize + inbox:true", "inbox:true", "filesize", "desc", false},
		{"negated inbox", "-inbox:true", "newest", "desc", false},
		{"inbox:false", "inbox:false", "newest", "desc", false},
		{"empty query", "", "newest", "desc", false},
	}
	for _, c := range cases {
		expr := search.Parse(c.q)
		if got := inboxClustersActive(c.sort, c.order, expr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInboxFilterActive(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"bare inbox:true", "inbox:true", true},
		{"inbox:true with extra leaf", "inbox:true cat:character", true},
		{"extra leaf first", "1girl inbox:true", true},
		{"negated inbox", "-inbox:true", false},
		{"inbox:false", "inbox:false", false},
		{"inbox under OR", "inbox:true OR fav:true", false},
		{"empty query", "", false},
		{"unrelated query", "1girl", false},
	}
	for _, c := range cases {
		expr := search.Parse(c.q)
		if got := inboxFilterActive(expr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCollectionFilterActive(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"bare collection", `collection:"my comic"`, true},
		{"collection with extra leaf", `collection:"my comic" 1girl`, true},
		{"extra leaf first", `1girl collection:"my comic"`, true},
		{"negated collection", `-collection:"my comic"`, false},
		{"collection under OR", `collection:"my comic" OR fav:true`, false},
		{"empty value", "collection:", false},
		{"empty query", "", false},
		{"unrelated query", "1girl", false},
	}
	for _, c := range cases {
		expr := search.Parse(c.q)
		if got := collectionFilterActive(expr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSearchWarning(t *testing.T) {
	cases := []struct {
		name, query, want string
	}{
		{"clean", "type:image 1girl", ""},
		{"unknown type", "type:video", "Unknown filter value: type:video"},
		{"negated unknown", "-mime:zzzz", "Unknown filter value: mime:zzzz"},
		{"two unknown", "type:video fav:maybe", "Unknown filter value: type:video, fav:maybe"},
		{"valid union", "type:image,archive", ""},
		{"bad union element", "type:image,video", "Unknown filter value: type:image,video"},
		{"open key untouched", "name:video source:booru", ""},
		{"origin an API client named", "via:monloader", ""},
		{"unterminated quote", `"red hair`, "Could not read: unterminated quote"},
		{"unterminated quoted value", `name:"red`, "Could not read: unterminated quote"},
		{"closed quote", `name:"red hair" 1girl`, ""},
		{"comparison that does not parse", "width:>abc", "Could not read: width:>abc"},
		{"impossible date", "date:2026-13-45", "Could not read: date:2026-13-45"},
		{"range end that does not parse", "size:1mb..lots", "Could not read: size:1mb..lots"},
		{"category count that does not parse", "tagcount:character=many", "Could not read: tagcount:character=many"},
		{"good comparisons", "width:>=100 date:2026-01-01..2026-02-01 size:<2mb tagcount:character=1", ""},
		{"both kinds", `width:>abc type:video "x`, "Could not read: unterminated quote, width:>abc. Unknown filter value: type:video"},
	}
	for _, c := range cases {
		expr := search.Parse(c.query)
		if got := searchWarning(c.query, expr); got != c.want {
			t.Errorf("%s: searchWarning(%q) = %q, want %q", c.name, c.query, got, c.want)
		}
	}
}

func TestSearchTime(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.00 ms"},
		{420 * time.Microsecond, "0.42 ms"},
		{42*time.Millisecond + 370*time.Microsecond, "42.37 ms"},
		{999 * time.Millisecond, "999.00 ms"},
		{time.Second, "1.00 s"},
		{1240 * time.Millisecond, "1.24 s"},
	}
	for _, c := range cases {
		if got := searchTime(c.in); got != c.want {
			t.Errorf("searchTime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGallery_TheCountAddsItsTimeToTheRows(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 2
	for i := range 5 {
		seedImage(t, srv, "took_"+strconv.Itoa(i)+".png", 10+i, 10)
	}

	body := mustGetBody(t, srv, "/?q=width:>0&page=2")
	wantIn(t, body, `class="result-time"`)
	m := countURLRe.FindStringSubmatch(body)
	if m == nil || !strings.Contains(m[1], "took=") {
		t.Fatalf("the deferred count carries no row time: %v", m)
	}

	frag := mustGetBody(t, srv, "/internal/search/count?q=width:>0&count=1&took=1500000")
	got := regexp.MustCompile(`\((\d+\.\d\d) s\)`).FindStringSubmatch(frag)
	if got == nil {
		t.Fatalf("no time in seconds in the count fragment: %s", sliceAround(frag, "result-time", 120))
	}
	if secs, _ := strconv.ParseFloat(got[1], 64); secs < 1.5 {
		t.Errorf("count time = %s s, want at least the 1.5 s the rows took", got[1])
	}
}

func TestBuildInboxCluster_LabelsAndBoundsLocal(t *testing.T) {
	if !inTZProbe(t) {
		return
	}

	imgs := []models.Image{{ID: 1, IngestedAt: time.Date(2026, 5, 22, 1, 30, 0, 0, time.UTC)}}
	got := computeInboxClusters(imgs, "", false, false)
	if len(got) != 1 || got[0] == nil {
		t.Fatalf("got %v, want one cluster", got)
	}
	if got[0].DateLabel != "2026-05-21" || got[0].RangeLabel != "22:30" {
		t.Errorf("labels = %q / %q, want 2026-05-21 / 22:30", got[0].DateLabel, got[0].RangeLabel)
	}
	u, err := url.Parse(got[0].RangeLink)
	if err != nil {
		t.Fatal(err)
	}
	if q := u.Query().Get("q"); !strings.Contains(q, "date:2026-05-21T22:30..2026-05-21T22:30") {
		t.Errorf("RangeLink q = %q, want local date bounds", q)
	}
}

func TestGallery_AtOpensThePageHoldingTheImage(t *testing.T) {
	raiseRankPageBudget(t)
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "at", 12)
	card := fmt.Sprintf(`data-id="%d"`, ids[3])

	w := do(srv, "GET", fmt.Sprintf("/?at=%d", ids[3]))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), card)

}

func TestGallery_AtIgnoresTheCursor(t *testing.T) {
	raiseRankPageBudget(t)
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "atcursor", 12)

	w := do(srv, "GET", fmt.Sprintf("/?after=%d&at=%d", ids[11], ids[3]))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`data-id="%d"`, ids[3]))
}

func TestGallery_AtMovesAPageThatDoesNotHoldTheImage(t *testing.T) {
	raiseRankPageBudget(t)
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "atpage", 12)
	card := fmt.Sprintf(`data-id="%d"`, ids[3])

	w := do(srv, "GET", fmt.Sprintf("/?at=%d&page=1", ids[3]))
	mustCode(t, w, http.StatusSeeOther)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Query().Get("page") != "2" || loc.Query().Has("at") {
		t.Errorf("Location = %q, want page=2 and no at", loc)
	}

	w = do(srv, "GET", fmt.Sprintf("/?at=%d&page=2", ids[3]))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), card)

	w = do(srv, "GET", "/?at=999999&page=1")
	mustCode(t, w, http.StatusOK)
}

func TestAdjacentSearchQueryPinsTheCollectionBeforeTheCeiling(t *testing.T) {
	srv := newTestServer(t)
	ceiling := resolveCeiling(httptest.NewRequest("GET", "/", nil), srv.active())
	sensitive := library.NewCeiling("sensitive", srv.active())
	cases := []struct {
		q, sort, order, seed string
		ceiling              *Ceiling
		want                 search.Query
	}{
		{"blue", "newest", "desc", "", ceiling, search.Query{Sort: "newest", Order: "desc"}},
		{`collection:Saga`, "order", "asc", "", sensitive, search.Query{Sort: "order", Order: "asc", OrderCollection: "Saga"}},
		{`collection:Saga`, "newest", "desc", "", sensitive, search.Query{Sort: "newest", Order: "desc"}},
		{"blue", "random", "desc", "7", ceiling, search.Query{Sort: "random", Order: "desc", RandomSeed: 7}},
		{"blue", "random", "desc", "x", ceiling, search.Query{Sort: "random", Order: "desc"}},
		{"blue", "newest", "desc", "7", ceiling, search.Query{Sort: "newest", Order: "desc"}},
	}
	for _, c := range cases {
		want := c.want
		want.Expr = c.ceiling.Apply(search.Parse(c.q))
		want.Cached = true
		want.Text = c.q
		want.CeilingLevel = c.ceiling.Level()
		if got := adjacentSearchQuery(c.q, c.sort, c.order, c.seed, c.ceiling); !reflect.DeepEqual(got, want) {
			t.Errorf("adjacentSearchQuery(%q, %q, %q, %q) = %+v, want %+v", c.q, c.sort, c.order, c.seed, got, want)
		}
	}
}

func TestGalleryCachesItsListUnderTheDetailNavQuery(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)
	first := seedImage(t, srv, "first.png", 12, 12)
	mustAddCollection(t, srv.db(), first, "Alpha", nil)
	for i, id := range []int64{safe, first, explicit} {
		order := i + 1
		mustAddCollection(t, srv.db(), id, "Saga", &order)
	}
	cookie := [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(cookie[0], cookie[1])
	ceiling := resolveCeiling(req, srv.active())

	w := do(srv, "GET", "/?q="+url.QueryEscape("collection:Saga")+"&sort=order&order=asc", cookie)
	mustCode(t, w, http.StatusOK)
	sq := adjacentSearchQuery("collection:Saga", "order", "asc", "", ceiling)
	for id, want := range map[int64]int{safe: 1, first: 2, explicit: 0} {
		if page, listed := search.PageOf(srv.source(), sq, id, 1); !listed || page != want {
			t.Errorf("collection order: PageOf(%d) = %d, %v; want %d, true", id, page, listed, want)
		}
	}

	w = do(srv, "GET", "/?sort=random&seed=7", cookie)
	mustCode(t, w, http.StatusOK)
	sq = adjacentSearchQuery("", "random", "desc", "7", ceiling)
	if page, listed := search.PageOf(srv.source(), sq, first, 1); !listed || page == 0 {
		t.Errorf("random: PageOf(%d) = %d, %v; want a page, true", first, page, listed)
	}
	if page, listed := search.PageOf(srv.source(), sq, explicit, 1); !listed || page != 0 {
		t.Errorf("random: PageOf(%d) = %d, %v; want 0, true", explicit, page, listed)
	}
}

func TestGallery_NextAndPrevCarryTheEdgeRows(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "cursor", 12)

	body := mustGetBody(t, srv, "/?page=2")
	wantIn(t, body, fmt.Sprintf(`page=3&amp;after=%d"`, ids[2]))
	wantIn(t, body, fmt.Sprintf(`page=1&amp;before=%d"`, ids[6]))

	w := do(srv, "GET", fmt.Sprintf("/?page=3&after=%d", ids[2]))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`data-id="%d"`, ids[1]))

	if random := mustGetBody(t, srv, "/?sort=random&seed=7&page=2"); strings.Contains(random, "&amp;after=") {
		t.Errorf("a random sort has no key to page from; got a cursor link: %s", sliceAround(random, "&amp;after=", 200))
	}
}

func TestGallery_ACursorPage1WithRowsAboveOffersPrev(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "above", 12)
	insertRankRows(t, srv, "landed", 3)
	srv.active().InvalidateCaches()

	body := mustGetBody(t, srv, fmt.Sprintf("/?page=1&before=%d", ids[6]))
	wantIn(t, body, fmt.Sprintf(`data-id="%d"`, ids[11]))
	wantIn(t, body, `page=1" hx-get="/?q=&amp;sort=newest&amp;order=desc&amp;page=1">← Prev</a>`)

	top := mustGetBody(t, srv, "/?page=1")
	if strings.Contains(top, "← Prev") {
		t.Error("the true first page offers Prev")
	}
}

func TestGallery_ACursorPageKeepsItsRowsWhenMatchesLeaveAbove(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "leave", 16)
	if _, err := srv.db().Write.Exec(`UPDATE images SET is_missing = 1 WHERE id >= ?`, ids[10]); err != nil {
		t.Fatal(err)
	}
	srv.active().InvalidateCaches()

	w := do(srv, "GET", fmt.Sprintf("/?page=2&after=%d", ids[11]))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, fmt.Sprintf(`data-id="%d"`, ids[5]))
	wantIn(t, body, fmt.Sprintf(`page=3&amp;after=%d"`, ids[5]))

	w = do(srv, "GET", fmt.Sprintf("/?page=3&after=%d", ids[5]))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`data-id="%d"`, ids[0]))
	if strings.Contains(w.Body.String(), fmt.Sprintf(`data-id="%d"`, ids[5])) {
		t.Error("Next repeated the page just left")
	}
}

func TestGalleryCount_AFullCursorPageWithRowsPastItKeepsNext(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "countnext", 10)
	if _, err := srv.db().Write.Exec(`UPDATE images SET is_missing = 1 WHERE id >= ?`, ids[8]); err != nil {
		t.Fatal(err)
	}
	srv.active().InvalidateCaches()

	body := mustGetBody(t, srv, fmt.Sprintf("/internal/search/count?count=1&page=2&first=%d&last=%d&full_cursor=1", ids[7], ids[3]))
	wantIn(t, body, "Page 2 / 2")
	wantIn(t, body, fmt.Sprintf(`page=3&amp;after=%d"`, ids[3]))
}

func TestGallery_AnAbandonedRequestStopsItsSearch(t *testing.T) {
	srv := newTestServer(t)
	insertRankRows(t, srv, "gone", 3)
	srv.active().InvalidateCaches()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/?q=-nothing", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusInternalServerError || strings.Contains(w.Body.String(), "thumb-card") {
		t.Errorf("code %d, body %q: the abandoned search ran or failed loudly", w.Code, sliceAround(w.Body.String(), "thumb-card", 80))
	}
}

func TestGallery_AClampDropsTheCursor(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	ids := insertRankRows(t, srv, "clampcur", 6)

	w := do(srv, "GET", fmt.Sprintf("/?page=4&after=%d", ids[0]))
	mustCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); strings.Contains(loc, "after=") || !strings.Contains(loc, "page=2") {
		t.Errorf("Location = %q, want page=2 without the cursor", loc)
	}
}
