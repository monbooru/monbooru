package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
)

func collectionMirror(t *testing.T, srv *Server, id int64) (string, sql.NullInt64) {
	t.Helper()
	var series string
	var order sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT series, series_order FROM images WHERE id = ?`, id).Scan(&series, &order); err != nil {
		t.Fatalf("read mirror for %d: %v", id, err)
	}
	return series, order
}

func membershipNames(t *testing.T, srv *Server, id int64) []string {
	t.Helper()
	cols, err := gallery.CollectionsForImage(srv.db(), id)
	if err != nil {
		t.Fatalf("CollectionsForImage(%d): %v", id, err)
	}
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, c.Name)
	}
	return names
}

func TestCollectionsPage_ListsWithPreview(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	c := seedImage(t, srv, "c.png", 12, 12)

	seven, eight := 7, 8
	mustSetHome(t, srv.db(), a, "Alpha", &seven)
	mustAddCollection(t, srv.db(), b, "Alpha", &eight)
	mustSetHome(t, srv.db(), c, "Beta", nil)

	body := mustGetBody(t, srv, "/collections")
	for _, want := range []string{
		"Alpha", "Beta",
		"2 images", "1 image",
		"collection-preview",
		`relations-card-id">#7<`,
		"btn-rename-collection", "btn-dissolve-collection",
		fmt.Sprintf("/thumbnails/%s/%d.jpg", srv.activeGallery(), a),
		`href="/collections"`,
	} {
		wantIn(t, body, want)
	}
}

func TestCollectionsPage_FilterAndSort(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	c := seedImage(t, srv, "c.png", 12, 12)

	mustSetHome(t, srv.db(), a, "Zebra", nil)
	mustAddCollection(t, srv.db(), b, "Zebra", nil)
	mustSetHome(t, srv.db(), c, "Apple", nil)

	get := func(q string) string {
		w := do(srv, "GET", "/collections"+q)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /collections%s expected 200, got %d", q, w.Code)
		}
		return w.Body.String()
	}

	body := get("?q=Zeb")
	if !strings.Contains(body, "Zebra") || strings.Contains(body, "Apple") {
		t.Errorf("q=Zeb should list Zebra only")
	}

	body = get("?sort=size")
	if strings.Index(body, "Zebra") > strings.Index(body, "Apple") {
		t.Error("sort=size should place Zebra (2) before Apple (1)")
	}

	body = get("?sort=name")
	if strings.Index(body, "Apple") > strings.Index(body, "Zebra") {
		t.Error("sort=name should place Apple before Zebra")
	}
}

func TestCollectionsPage_PaginationEscapesTheFilter(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	mustSetHome(t, srv.db(), a, "Q&A #00", nil)
	for i := 1; i <= collectionsPerPage; i++ {
		mustAddCollection(t, srv.db(), a, fmt.Sprintf("Q&A #%02d", i), nil)
	}

	w := do(srv, "GET", "/collections?q="+url.QueryEscape("Q&A #"))

	mustCode(t, w, http.StatusOK)
	next := nextPageLink(t, w.Body.String())
	if got := next.Query().Get("q"); got != "Q&A #" {
		t.Errorf("next page q = %q, want %q (href %s)", got, "Q&A #", next)
	}
	if got := next.Query().Get("page"); got != "2" {
		t.Errorf("next page = %q, want 2 (href %s)", got, next)
	}
	wantIn(t, do(srv, "GET", next.String()).Body.String(), "Page 2 / 2")
}

func TestCollectionsPage_Empty(t *testing.T) {
	srv := newTestServer(t)
	w := do(srv, "GET", "/collections")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "No collections yet")
}

func TestCollectionsPage_HonorsCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	mixedSafe := seedImage(t, srv, "mixed_safe.png", 10, 10)
	mixedExpl := seedImage(t, srv, "mixed_expl.png", 11, 11)
	spicy := seedImage(t, srv, "spicy.png", 12, 12)
	tag := func(id int64, level string) {
		mustAddTag(t, cx.TagSvc, id, ratingTagIDWeb(t, cx.DB, level), false, nil)
	}
	tag(mixedSafe, "general")
	tag(mixedExpl, "explicit")
	tag(spicy, "explicit")
	for _, id := range []int64{mixedSafe, mixedExpl} {
		mustSetHome(t, srv.db(), id, "Mixed", nil)
	}
	mustSetHome(t, srv.db(), spicy, "Spicy", nil)
	cx.InvalidateCaches()

	get := func(ceiling string) string {
		req := httptest.NewRequest("GET", "/collections", nil)
		if ceiling != "" {
			req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: ceiling})
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /collections (ceiling=%q) expected 200, got %d", ceiling, w.Code)
		}
		return w.Body.String()
	}

	body := get("")
	for _, want := range []string{`data-name="Mixed"`, `data-name="Spicy"`, "2 images"} {
		wantIn(t, body, want)
	}

	body = get("sensitive")
	if !strings.Contains(body, `data-name="Mixed"`) || !strings.Contains(body, "1 image") {
		t.Error("ceiling=sensitive: expected Mixed with its one visible member")
	}
	wantNotIn(t, body, `data-name="Spicy"`)
	wantNotIn(t, body, "2 images")
}

func TestRenameCollection_PlainAndCase(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	ord := 3
	mustSetHome(t, srv.db(), a, "Old", &ord)
	mustAddCollection(t, srv.db(), b, "Old", nil)

	w := postForm(t, srv, "/collections/rename", url.Values{"prev": {"Old"}, "name": {"New"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Old"); len(ids) != 0 {
		t.Errorf("Old should have no members after rename, got %v", ids)
	}
	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "New"); len(ids) != 2 {
		t.Errorf("New should have 2 members, got %v", ids)
	}
	if s, o := collectionMirror(t, srv, a); s != "New" || !o.Valid || o.Int64 != 3 {
		t.Errorf("a mirror = %q/%v, want New/3", s, o)
	}
	if s, _ := collectionMirror(t, srv, b); s != "New" {
		t.Errorf("b mirror = %q, want New", s)
	}

	w = postForm(t, srv, "/collections/rename", url.Values{"prev": {"New"}, "name": {"new"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "new"); len(ids) != 2 {
		t.Errorf("case rename: collection should still have 2 members, got %v", ids)
	}
	if names := membershipNames(t, srv, a); len(names) != 1 || names[0] != "new" {
		t.Errorf("a membership after case rename = %v, want [new]", names)
	}
}

func TestRenameCollection_MergesIntoExisting(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	src, dst := 1, 5

	mustSetHome(t, srv.db(), a, "Src", &src)
	mustAddCollection(t, srv.db(), a, "Dst", &dst)
	mustSetHome(t, srv.db(), b, "Src", nil)

	w := postForm(t, srv, "/collections/rename", url.Values{"prev": {"Src"}, "name": {"Dst"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Src"); len(ids) != 0 {
		t.Errorf("Src should be gone after merge, got %v", ids)
	}
	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Dst"); len(ids) != 2 {
		t.Errorf("Dst should hold both images, got %v", ids)
	}

	if names := membershipNames(t, srv, a); len(names) != 1 || names[0] != "Dst" {
		t.Errorf("a memberships = %v, want [Dst]", names)
	}
	if s, o := collectionMirror(t, srv, a); s != "Dst" || !o.Valid || o.Int64 != 5 {
		t.Errorf("a mirror = %q/%v, want Dst/5", s, o)
	}
	if s, o := collectionMirror(t, srv, b); s != "Dst" || o.Valid {
		t.Errorf("b mirror = %q/%v, want Dst/NULL", s, o)
	}
}

func TestRenameCollection_MergeOntoACaseVariantKeepsTheTargetsSpelling(t *testing.T) {
	srv := newTestServer(t)
	var ids []int64
	for i, label := range []string{"Src", "Src", "Dst Set", "Dst Set"} {
		id := seedImage(t, srv, fmt.Sprintf("m%d.png", i), 10+i, 10+i)
		mustSetHome(t, srv.db(), id, label, nil)
		ids = append(ids, id)
	}

	w := postForm(t, srv, "/collections/rename", url.Values{"prev": {"Src"}, "name": {"dst set"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	for _, id := range ids {
		if names := membershipNames(t, srv, id); len(names) != 1 || names[0] != "Dst Set" {
			t.Errorf("image %d memberships = %v, want [Dst Set]", id, names)
		}
		if s, _ := collectionMirror(t, srv, id); s != "Dst Set" {
			t.Errorf("image %d home = %q, want Dst Set", id, s)
		}
	}
}

func TestRenameCollection_MergeAppendsPositions(t *testing.T) {
	srv := newTestServer(t)
	pos := func(n int) *int { return &n }
	var srcIDs, dstIDs []int64
	for i := 1; i <= 3; i++ {
		id := seedImage(t, srv, fmt.Sprintf("src%d.png", i), 10+i, 10+i)
		mustSetHome(t, srv.db(), id, "Src", pos(i))
		srcIDs = append(srcIDs, id)
	}
	for i := 1; i <= 2; i++ {
		id := seedImage(t, srv, fmt.Sprintf("dst%d.png", i), 20+i, 20+i)
		mustSetHome(t, srv.db(), id, "Dst", pos(i))
		dstIDs = append(dstIDs, id)
	}

	w := postForm(t, srv, "/collections/rename", url.Values{"prev": {"Src"}, "name": {"Dst"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	position := func(id int64) int {
		t.Helper()
		var p sql.NullInt64
		if err := srv.db().Read.QueryRow(
			`SELECT position FROM image_collections WHERE image_id = ? AND name = 'Dst'`, id,
		).Scan(&p); err != nil {
			t.Fatalf("position for %d: %v", id, err)
		}
		if !p.Valid {
			t.Fatalf("image %d lost its position", id)
		}
		return int(p.Int64)
	}
	for i, id := range dstIDs {
		if got, want := position(id), i+1; got != want {
			t.Errorf("target member %d position = %d, want %d (untouched)", id, got, want)
		}
	}
	for i, id := range srcIDs {
		if got, want := position(id), len(dstIDs)+i+1; got != want {
			t.Errorf("merged member %d position = %d, want %d (appended)", id, got, want)
		}
	}

	if _, o := collectionMirror(t, srv, srcIDs[0]); !o.Valid || o.Int64 != 3 {
		t.Errorf("merged member mirror order = %v, want 3", o)
	}
}

func TestDissolveCollection_ACancelledDissolveKeepsTheFindRelationsFlag(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	mustSetHome(t, srv.db(), a, "Gone", nil)
	if err := gallery.SetCollectionFindRelations(srv.db(), "Gone", true); err != nil {
		t.Fatal(err)
	}
	flagged := func() bool { return countRows(t, srv.db(), "collection_find_relations WHERE name = 'Gone'") == 1 }

	mustExec(t, srv.db().Write, `CREATE TRIGGER refuse_removal BEFORE DELETE ON image_collections BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	mustCode(t, postForm(t, srv, "/collections/dissolve", url.Values{"collection": {"Gone"}}), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if !flagged() {
		t.Error("a failed dissolve cleared the find-relations flag")
	}
	mustExec(t, srv.db().Write, `DROP TRIGGER refuse_removal`)

	mustStartJob(t, srv.jobs, models.JobTypeTag)
	srv.jobs.Cancel()
	srv.runBatchCollection([]int64{a}, "Gone", "remove", true)
	if !flagged() {
		t.Error("a cancelled dissolve cleared the find-relations flag")
	}

	mustCode(t, postForm(t, srv, "/collections/dissolve", url.Values{"collection": {"Gone"}}), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if flagged() {
		t.Error("a completed dissolve kept the find-relations flag")
	}
}

func TestRenameCollection_ACancelledRenameKeepsTheFindRelationsFlag(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	mustSetHome(t, srv.db(), a, "Old", nil)
	if err := gallery.SetCollectionFindRelations(srv.db(), "Old", true); err != nil {
		t.Fatal(err)
	}
	flagged := func(name string) bool {
		return countRows(t, srv.db(), "collection_find_relations WHERE name = ?", name) == 1
	}

	mustStartJob(t, srv.jobs, models.JobTypeTag)
	srv.jobs.Cancel()
	srv.runRenameCollection([]int64{a}, "Old", "New")
	if !flagged("Old") || flagged("New") {
		t.Error("a cancelled rename moved the find-relations flag")
	}

	mustCode(t, postForm(t, srv, "/collections/rename", url.Values{"prev": {"Old"}, "name": {"New"}}), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if flagged("Old") || !flagged("New") {
		t.Error("a completed rename left the find-relations flag behind")
	}
}

func TestDissolveCollection_DropsLabelKeepsImages(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)

	c, _ := mustExec(t, srv.db().Write,
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		 VALUES ('/gone/c.png', 'png', 10, 'sha_missing_c', 1, datetime('now'))`).LastInsertId()

	pos := 2
	mustSetHome(t, srv.db(), a, "Gone", nil)
	mustAddCollection(t, srv.db(), a, "Keep", &pos)
	mustSetHome(t, srv.db(), b, "Gone", nil)
	mustAddCollection(t, srv.db(), c, "Gone", nil)

	w := postForm(t, srv, "/collections/dissolve", url.Values{"collection": {"Gone"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Gone"); len(ids) != 0 {
		t.Errorf("Gone should have no members after dissolve, got %v", ids)
	}

	if names := membershipNames(t, srv, a); len(names) != 1 || names[0] != "Keep" {
		t.Errorf("a memberships = %v, want [Keep]", names)
	}
	if s, o := collectionMirror(t, srv, a); s != "Keep" || !o.Valid || o.Int64 != 2 {
		t.Errorf("a mirror = %q/%v, want Keep/2", s, o)
	}

	if s, _ := collectionMirror(t, srv, b); s != "" {
		t.Errorf("b mirror = %q, want empty", s)
	}
	if s, _ := collectionMirror(t, srv, c); s != "" {
		t.Errorf("c mirror = %q, want empty", s)
	}
	for _, id := range []int64{a, b, c} {
		n := countRows(t, srv.db(), "images WHERE id = ?", id)
		if n != 1 {
			t.Errorf("image %d should still exist after dissolve", id)
		}
	}
}

func TestReorderCollection_KeepsCeilingHiddenPositions(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)
	one, two := 1, 2
	mustAddCollection(t, srv.db(), safe, "Ceil", &one)
	mustAddCollection(t, srv.db(), explicit, "Ceil", &two)

	w := postCeilingRequest(t, srv, "/collections/order", url.Values{
		"collection": {"Ceil"},
		"ids":        {fmt.Sprint(safe)},
	})
	mustCode(t, w, http.StatusOK)

	if body := w.Body.String(); !strings.Contains(body, "Ordered 1 image.") {
		t.Errorf("flash = %q, want the clicked count", body)
	}

	position := func(id int64) sql.NullInt64 {
		t.Helper()
		var p sql.NullInt64
		if err := srv.db().Read.QueryRow(
			`SELECT position FROM image_collections WHERE image_id = ? AND name = 'Ceil'`, id,
		).Scan(&p); err != nil {
			t.Fatalf("position for %d: %v", id, err)
		}
		return p
	}
	if p := position(safe); !p.Valid || p.Int64 != 1 {
		t.Errorf("clicked member position = %v, want 1", p)
	}
	if p := position(explicit); !p.Valid {
		t.Error("ceiling-hidden member lost its position")
	} else if p.Int64 != 2 {
		t.Errorf("ceiling-hidden member position = %d, want 2 (behind the arranged block)", p.Int64)
	}
}

func TestCollectionOrderDialogCapsTheLimit(t *testing.T) {
	srv := newTestServer(t)
	mustExec(t, srv.db().Write, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 5001)
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		SELECT 'c' || i || '.png', 'png', 1, 'sha-c' || i, datetime('now') FROM n`)
	mustExec(t, srv.db().Write, `INSERT INTO image_collections (image_id, name) SELECT id, 'Big' FROM images`)

	w := do(srv, "GET", "/collections/order?name=Big&limit=9223372036854775807", [2]string{"HX-Request", "true"})

	body := w.Body.String()
	if n := strings.Count(body, `class="collection-order-tile"`); n != 5000 {
		t.Errorf("rendered %d tiles, want the 5000 cap", n)
	}
	wantIn(t, body, "Showing the first 5000 images.")
	if strings.Contains(body, "collection-order-more") {
		t.Error("[Show more] offered at the cap")
	}
}
