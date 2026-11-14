package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/relations"
)

func seedDupGroup(t *testing.T, srv *Server, salt string) {
	t.Helper()
	sha := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	a := insertRawImage(t, srv, salt+"a.png", sha(salt+"a"))
	b := insertRawImage(t, srv, salt+"b.png", sha(salt+"b"))
	if err := srv.active().RelationsSvc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
}

func TestRelationAdd_RefreshesTheGalleryRelationFilter(t *testing.T) {
	srv := newTestServer(t)
	seedDupGroup(t, srv, "cached_")

	if got := galleryResultCount(t, srv, "relation:duplicate"); got != "2" {
		t.Fatalf("seeded relation:duplicate count = %q, want 2", got)
	}

	a := insertRawImage(t, srv, "fresh_a.png", "fresh_sha_a")
	b := insertRawImage(t, srv, "fresh_b.png", "fresh_sha_b")
	form := url.Values{
		"type": {"duplicate"},
		"a":    {strconv.FormatInt(a, 10)},
		"b":    {strconv.FormatInt(b, 10)},
	}
	w := postForm(t, srv, "/relations/add", form)
	mustCode(t, w, http.StatusOK)

	if got := galleryResultCount(t, srv, "relation:duplicate"); got != "4" {
		t.Errorf("relation:duplicate count after the add = %q, want 4", got)
	}
}

func postDissolve(t *testing.T, srv *Server, kind, field string, values ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for _, v := range values {
		form.Add(field, v)
	}
	return postForm(t, srv, "/relations/browse-groups/dissolve?kind="+kind, form)
}

func postMerge(t *testing.T, srv *Server, kind string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, srv, "/relations/browse-groups/merge?kind="+kind, form)
}

func TestMergeGroupsPost(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		srv := newTestServer(t)
		seedDupGroup(t, srv, "merge-a-")
		seedDupGroup(t, srv, "merge-b-")
		gids, err := db.QueryStrings(srv.db().Read, `SELECT id FROM dup_groups ORDER BY id`)
		if err != nil || len(gids) != 2 {
			t.Fatalf("dup_groups seed = %v (%v), want 2", gids, err)
		}
		var keptOriginal int64
		if err := srv.db().Read.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gids[1]).Scan(&keptOriginal); err != nil {
			t.Fatal(err)
		}
		w := postMerge(t, srv, "dup", url.Values{"group_id": gids, "keep_original_from": {gids[1]}})
		mustCode(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != "/relations/browse?kind=duplicate" {
			t.Errorf("Location = %q, want /relations/browse?kind=duplicate", loc)
		}
		if n := countRows(t, srv.db(), "dup_groups"); n != 1 {
			t.Errorf("dup_groups = %d, want the two merged into 1", n)
		}
		if n := countRows(t, srv.db(), "dup_group_members"); n != 4 {
			t.Errorf("dup_group_members = %d, want 4", n)
		}
		if n := countRows(t, srv.db(), "dup_groups WHERE original_image_id = ?", keptOriginal); n != 1 {
			t.Error("the merged group did not keep the original keep_original_from named")
		}
	})

	t.Run("alternate", func(t *testing.T) {
		srv := newTestServer(t)
		ids := make([]int64, 4)
		for i := range ids {
			ids[i] = insertRawImage(t, srv, "merge-alt-"+strconv.Itoa(i)+".png", "")
		}
		for _, pair := range [][2]int64{{ids[0], ids[1]}, {ids[2], ids[3]}} {
			if err := srv.active().RelationsSvc.AddAlternate(pair[0], pair[1]); err != nil {
				t.Fatal(err)
			}
		}
		gids, err := db.QueryStrings(srv.db().Read, `SELECT id FROM alt_groups ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		w := postMerge(t, srv, "alt", url.Values{"group_id": gids})
		mustCode(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != "/relations/browse?kind=alternate" {
			t.Errorf("Location = %q, want /relations/browse?kind=alternate", loc)
		}
		if n := countRows(t, srv.db(), "alt_groups"); n != 1 {
			t.Errorf("alt_groups = %d, want the two merged into 1", n)
		}
		if n := countRows(t, srv.db(), "alt_group_members"); n != 4 {
			t.Errorf("alt_group_members = %d, want 4", n)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		srv := newTestServer(t)
		seedDupGroup(t, srv, "refuse-a-")
		seedDupGroup(t, srv, "refuse-b-")
		gids, err := db.QueryStrings(srv.db().Read, `SELECT id FROM dup_groups ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, kind string
			form       url.Values
		}{
			{"an unknown kind", "bogus", url.Values{"group_id": gids}},
			{"a single group", "dup", url.Values{"group_id": gids[:1]}},
			{"an original from outside the merge", "dup", url.Values{"group_id": gids, "keep_original_from": {"999999"}}},
		} {
			if w := postMerge(t, srv, tc.kind, tc.form); w.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", tc.name, w.Code)
			}
		}
		if n := countRows(t, srv.db(), "dup_groups"); n != 2 {
			t.Errorf("dup_groups = %d after refused merges, want 2", n)
		}
	})
}

func TestBrowseGroupsPost_RedirectKeepsSortAndPage(t *testing.T) {
	srv := newTestServer(t)
	for _, salt := range []string{"keep-a-", "keep-b-", "keep-c-"} {
		seedDupGroup(t, srv, salt)
	}
	gids, err := db.QueryStrings(srv.db().Read, `SELECT id FROM dup_groups ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	const want = "/relations/browse?kind=duplicate&page=3&sort=size"

	w := postMerge(t, srv, "dup", url.Values{"group_id": gids[:2], "sort": {"size"}, "page": {"3"}})
	mustCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != want {
		t.Errorf("merge: Location = %q, want %q", loc, want)
	}

	w = postForm(t, srv, "/relations/browse-groups/dissolve?kind=duplicate", url.Values{"group_id": {gids[2]}, "sort": {"size"}, "page": {"3"}})
	mustCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != want {
		t.Errorf("dissolve: Location = %q, want %q", loc, want)
	}
}

func TestDissolveGroupsPost(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		srv := newTestServer(t)
		seedDupGroup(t, srv, "bulk-dup-")
		var gid int64
		if err := srv.db().Read.QueryRow(`SELECT id FROM dup_groups LIMIT 1`).Scan(&gid); err != nil {
			t.Fatal(err)
		}
		w := postDissolve(t, srv, "duplicate", "group_id", strconv.FormatInt(gid, 10))
		mustCode(t, w, http.StatusSeeOther)
		n := countRows(t, srv.db(), "dup_groups")
		if n != 0 {
			t.Errorf("dup_groups remaining = %d, want 0", n)
		}
	})

	t.Run("alternate", func(t *testing.T) {
		srv := newTestServer(t)
		a := insertRawImage(t, srv, "alt-a.png", "")
		b := insertRawImage(t, srv, "alt-b.png", "")
		c := insertRawImage(t, srv, "alt-c.png", "")
		d := insertRawImage(t, srv, "alt-d.png", "")
		if err := srv.active().RelationsSvc.AddAlternate(a, b); err != nil {
			t.Fatal(err)
		}
		if err := srv.active().RelationsSvc.AddAlternate(c, d); err != nil {
			t.Fatal(err)
		}
		gids, err := db.QueryStrings(srv.db().Read, `SELECT id FROM alt_groups ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		if len(gids) != 2 {
			t.Fatalf("alt_groups seed = %d, want 2", len(gids))
		}
		w := postDissolve(t, srv, "alternate", "group_id", gids...)
		mustCode(t, w, http.StatusSeeOther)
		n := countRows(t, srv.db(), "alt_groups")
		if n != 0 {
			t.Errorf("alt_groups remaining = %d, want 0", n)
		}
	})

	pairValue := func(a, b int64) string { return strconv.FormatInt(a, 10) + ":" + strconv.FormatInt(b, 10) }
	rootValue := func(a, _ int64) string { return strconv.FormatInt(a, 10) }
	for _, tc := range []struct {
		kind, field, prefix, table string
		add                        func(*relations.Service, int64, int64) error
		value                      func(a, b int64) string
	}{
		{"version", "root_id", "v-", "version_edges", (*relations.Service).AddVersionEdge, rootValue},
		{"derivative", "root_id", "d-", "derivative_edges", (*relations.Service).AddDerivativeEdge, rootValue},
		{"not_related", "pair", "nr-", "not_related_pairs", (*relations.Service).AddNotRelated, pairValue},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			srv := newTestServer(t)
			a := insertRawImage(t, srv, tc.prefix+"a.png", "")
			b := insertRawImage(t, srv, tc.prefix+"b.png", "")
			if err := tc.add(srv.active().RelationsSvc, a, b); err != nil {
				t.Fatal(err)
			}
			w := postDissolve(t, srv, tc.kind, tc.field, tc.value(a, b))
			mustCode(t, w, http.StatusSeeOther)
			if n := countRows(t, srv.db(), tc.table); n != 0 {
				t.Errorf("%s remaining = %d, want 0", tc.table, n)
			}
		})
	}

	t.Run("unknown kind 400", func(t *testing.T) {
		srv := newTestServer(t)
		w := postDissolve(t, srv, "garbage", "group_id", "1")
		wantCode(t, w, http.StatusBadRequest)
	})
}

func TestRelationsBrowse_MultiSourceComponentIsOneCard(t *testing.T) {
	srv := newTestServer(t)
	rel := srv.active().RelationsSvc
	s1 := seedImage(t, srv, "multi-s1", 11, 11)
	s2 := seedImage(t, srv, "multi-s2", 12, 12)
	s3 := seedImage(t, srv, "multi-s3", 13, 13)
	composite := seedImage(t, srv, "multi-composite", 14, 14)
	edit := seedImage(t, srv, "multi-edit", 15, 15)
	for _, e := range []struct{ src, der int64 }{
		{s1, composite}, {s2, composite}, {s3, composite}, {composite, edit},
	} {
		if err := rel.AddDerivativeEdge(e.src, e.der); err != nil {
			t.Fatalf("edge (%d -> %d): %v", e.src, e.der, err)
		}
	}

	body := ceilingGet(t, srv, "/relations/browse?kind=derivative", "")
	if got := strings.Count(body, `relations-card-derivative`); got != 1 {
		t.Fatalf("derivative cards = %d, want 1 for the whole component", got)
	}
	for _, want := range []string{
		"(5 images, sources",
		fmt.Sprintf("#%d #%d #%d)", s1, s2, s3),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("browse card missing %q", want)
		}
	}

	if got := strings.Count(body, `class="deriv-row"`); got != 3 {
		t.Errorf("rows = %d, want 3 generations", got)
	}
	if got := strings.Count(body, `class="deriv-cell"`); got != 5 {
		t.Errorf("cells = %d, want one per member", got)
	}
	if got := strings.Count(body, "<line "); got != 4 {
		t.Errorf("wires = %d, want one per edge", got)
	}
	if got := strings.Count(body, `"review-again"`); got != 4 {
		t.Errorf("review-again forms = %d, want one per edge", got)
	}
	for _, src := range []int64{s1, s2, s3} {
		edge := fmt.Sprintf(`data-edge="%d-%d"`, src, composite)
		if got := strings.Count(body, edge); got != 2 {
			t.Errorf("%s appears %d times, want the wire and its action", edge, got)
		}
	}
}

func TestRelationsBrowse_AHugePageIsClampedNotAPanic(t *testing.T) {
	srv := newTestServer(t)
	seedDupGroup(t, srv, "huge_")
	var ids []int64
	for i := range 4 {
		ids = append(ids, seedImage(t, srv, fmt.Sprintf("huge_edge_%d.png", i), 10+i, 10))
	}
	svc := srv.active().RelationsSvc
	if err := svc.AddVersionEdge(ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDerivativeEdge(ids[2], ids[3]); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"version", "derivative", "duplicate"} {
		for _, page := range []int{math.MaxInt/2 + 1, math.MaxInt} {
			w := do(srv, "GET", fmt.Sprintf("/relations/browse?kind=%s&page=%d", kind, page))
			mustCode(t, w, http.StatusSeeOther)
			if loc := w.Header().Get("Location"); !strings.Contains(loc, "page=1") {
				t.Errorf("%s page %d redirected to %q, want page=1", kind, page, loc)
			}
		}
	}
}

func TestBrowseGroupsRedirect_MapsTheOldKindNames(t *testing.T) {
	srv := newTestServer(t)
	for kind, want := range map[string]string{
		"dup":        "duplicate",
		"alt":        "alternate",
		"version":    "version",
		"derivative": "derivative",
	} {
		w := do(srv, "GET", "/relations/browse-groups?kind="+kind)
		mustCode(t, w, http.StatusMovedPermanently)
		if loc := w.Header().Get("Location"); loc != "/relations/browse?kind="+want {
			t.Errorf("kind %s redirected to %q, want kind=%s", kind, loc, want)
		}
	}
}

func TestAddRelationPost_ForcedUnknownTypeClearsNothing(t *testing.T) {
	srv := newTestServer(t)
	seedDupGroup(t, srv, "forced_")
	var a, b int64
	if err := srv.db().Read.QueryRow(
		`SELECT MIN(image_id), MAX(image_id) FROM dup_group_members`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}

	w := postForm(t, srv, "/relations/add", url.Values{
		"a": {strconv.FormatInt(a, 10)}, "b": {strconv.FormatInt(b, 10)},
		"type": {"bogus"}, "force": {"true"},
	})
	mustCode(t, w, http.StatusBadRequest)
	if n := countRows(t, srv.db(), "dup_group_members"); n != 2 {
		t.Errorf("dup group members after the refused overwrite = %d, want 2", n)
	}
}

func TestLayOutDerivatives_ColumnCentres(t *testing.T) {

	g := layOutDerivatives([]int64{1, 2, 3, 4}, map[int64][]int64{2: {1}, 3: {1}, 4: {1}})
	if g.Cols != 3 || g.Width != derivSpanX {
		t.Fatalf("cols/width = %d/%d, want 3/%d", g.Cols, g.Width, derivSpanX)
	}
	want := map[int64]int{2: derivSpanX / 6, 3: derivSpanX / 2, 4: 5 * derivSpanX / 6}
	for _, seg := range g.Bands[0] {
		if seg.From != 1 {
			t.Errorf("segment from %d, want the source", seg.From)
		}
		if seg.X1 != derivSpanX/2 {
			t.Errorf("edge %d-%d starts at %d, want the source centred over the row", seg.From, seg.To, seg.X1)
		}
		if seg.X2 != want[seg.To] {
			t.Errorf("edge %d-%d lands at %d, want %d", seg.From, seg.To, seg.X2, want[seg.To])
		}
	}
}

func TestImageRelationsPage_CapsEachCollectionOnItsOwn(t *testing.T) {
	srv := newTestServer(t)
	ids := insertRankRows(t, srv, "collcap", collectionSiblingCap+6)
	self := ids[0]
	for i, id := range ids[:collectionSiblingCap+4] {
		mustExec(t, srv.db().Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?, 'a', ?)`, id, i)
	}
	for _, id := range []int64{self, ids[len(ids)-2], ids[len(ids)-1]} {
		mustExec(t, srv.db().Write, `INSERT INTO image_collections (image_id, name) VALUES (?, 'b')`, id)
	}

	w := do(srv, "GET", "/images/"+strconv.FormatInt(self, 10)+"/relations")
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "+3 more in a")
	wantIn(t, body, fmt.Sprintf(`href="/images/%d"`, ids[len(ids)-1]))
	if strings.Contains(body, "more in b") {
		t.Error("the uncut collection says it left members out")
	}
}

func TestImageRelationsPage_ListsEverySource(t *testing.T) {
	srv := newTestServer(t)
	rel := srv.active().RelationsSvc
	s1 := seedImage(t, srv, "page-s1", 21, 21)
	s2 := seedImage(t, srv, "page-s2", 22, 22)
	composite := seedImage(t, srv, "page-composite", 23, 23)
	for _, s := range []int64{s1, s2} {
		if err := rel.AddDerivativeEdge(s, composite); err != nil {
			t.Fatalf("source %d: %v", s, err)
		}
	}

	w := do(srv, "GET", "/images/"+strconv.FormatInt(composite, 10)+"/relations")
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		"Image based on 2 images",
		fmt.Sprintf("[unlink source #%d]", s1),
		fmt.Sprintf("[unlink source #%d]", s2),
		fmt.Sprintf(`Make #%d based on this image`, s1),
		`hx-post="/relations/reverse"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("relations page missing %q", want)
		}
	}
	if got := strings.Count(body, `class="deriv-row"`); got != 2 {
		t.Errorf("rows = %d, want 2 generations", got)
	}
	if got := strings.Count(body, `class="deriv-cell"`); got != 3 {
		t.Errorf("cells = %d, want one per member", got)
	}
	if got := strings.Count(body, "<line "); got != 2 {
		t.Errorf("wires = %d, want one per edge", got)
	}
	for _, src := range []int64{s1, s2} {
		edge := fmt.Sprintf(`data-edge="%d-%d"`, src, composite)
		if got := strings.Count(body, edge); got != 2 {
			t.Errorf("%s appears %d times, want the wire and its unlink", edge, got)
		}
	}
	if got := strings.Count(body, `^ This image`); got != 1 {
		t.Errorf("this-image labels = %d, want 1", got)
	}
}

func TestRelationsBrowse_Pagination(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < browseRelationsPageSize+1; i++ {
		seedDupGroup(t, srv, fmt.Sprintf("d%03d-", i))
	}

	count := func(path string) int {
		w := do(srv, "GET", path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code=%d", path, w.Code)
		}

		return strings.Count(w.Body.String(), `class="relations-card `)
	}

	p1 := count("/relations/browse?kind=duplicate")
	p2 := count("/relations/browse?kind=duplicate&page=2")
	if p1 != browseRelationsPageSize {
		t.Errorf("page 1 cards = %d, want %d", p1, browseRelationsPageSize)
	}
	if p2 != 1 {
		t.Errorf("page 2 cards = %d, want 1", p2)
	}
}

func TestRelationsBrowse_ClampsPastEndPage(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < browseRelationsPageSize+1; i++ {
		seedDupGroup(t, srv, fmt.Sprintf("c%03d-", i))
	}
	w := do(srv, "GET", "/relations/browse?kind=duplicate&page=99")
	mustCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "page=2") {
		t.Errorf("Location = %q, want it to carry the clamped page=2", loc)
	}
}

func TestAnnotateBrowseCardIngestedAt_PastTheVariableCap(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	cx := srv.active()

	const n = 32767
	if _, err := cx.DB.Write.Exec(`INSERT INTO images (sha256, canonical_path, file_type, file_size)
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		SELECT 'cap' || n, '/cap/' || n || '.png', 'png', 1 FROM seq`, n); err != nil {
		t.Fatal(err)
	}
	ids, err := db.QueryIDs(cx.DB.Read, `SELECT id FROM images WHERE sha256 LIKE 'cap%'`)
	if err != nil || len(ids) != n {
		t.Fatalf("seeded %d ids (%v), want %d", len(ids), err, n)
	}
	cards := []browseCard{{Kind: "version", Members: ids[:n/2]}, {Kind: "version", Members: ids[n/2:]}}

	if err := annotateBrowseCardIngestedAt(cx, cards); err != nil {
		t.Fatal(err)
	}
	for i, c := range cards {
		if len(c.MemberIngestedAt) != len(c.Members) {
			t.Errorf("card %d: %d of %d members annotated", i, len(c.MemberIngestedAt), len(c.Members))
		}
	}
}
