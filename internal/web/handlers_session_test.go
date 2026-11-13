package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/relations"
)

func addRatingTag(t *testing.T, database *db.DB, imageID int64, ratingName string) {
	t.Helper()
	var tagID int64
	err := database.Read.QueryRow(
		`SELECT id FROM tags
		 WHERE category_id = (SELECT id FROM tag_categories WHERE name = 'rating')
		   AND name = ?`,
		ratingName,
	).Scan(&tagID)
	if err != nil {
		t.Fatalf("lookup rating tag %q: %v", ratingName, err)
	}
	mustExec(t, database.Write, `INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, created_at) VALUES (?, ?, 0, 0, datetime('now'))`, imageID, tagID)
}

func queueRow(t *testing.T, database *db.DB, a, b int64, distance int) {
	t.Helper()
	lo, hi := a, b
	if hi < lo {
		lo, hi = hi, lo
	}
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, datetime('now'))`, lo, hi, distance)
}

func TestSessionLoadNextPair_RatingCeilingFiltersBothSides(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := insertTestImage(t, srv)
	bID := insertBareImage(t, srv, "/tmp/explicit.jpg", "jpg", "sha_explicit")
	cID := insertBareImage(t, srv, "/tmp/sensitive.jpg", "jpg", "sha_sensitive")

	addRatingTag(t, srv.db(), bID, "explicit")
	addRatingTag(t, srv.db(), cID, "sensitive")

	queueRow(t, srv.db(), a, cID, 3)

	queueRow(t, srv.db(), a, bID, 4)

	ceiling := library.NewCeiling("sensitive", cx)
	excludeIDs := ceiling.ExcludedTagIDs()
	if len(excludeIDs) != 2 {
		t.Fatalf("excludeIDs len = %d, want 2 (questionable + explicit)", len(excludeIDs))
	}

	pair, counts := mustLoadNextPair(t, cx, "smallest_distance_first", "both", ceiling, 0, 0)
	if counts.HiddenByCeiling != 1 {
		t.Errorf("hidden by ceiling = %d, want 1", counts.HiddenByCeiling)
	}
	if counts.Open != 1 {
		t.Errorf("open remaining = %d, want 1 (the sensitive pair only)", counts.Open)
	}
	if pair == nil {
		t.Fatal("expected the sensitive pair to surface, got nil")
	}
	if pair.A.ID != a && pair.B.ID != a {
		t.Errorf("returned pair does not include the untagged image: A=%d B=%d", pair.A.ID, pair.B.ID)
	}
	if pair.A.ID == bID || pair.B.ID == bID {
		t.Errorf("returned pair includes the explicit-rated image: A=%d B=%d", pair.A.ID, pair.B.ID)
	}

	none := library.NewCeiling("", cx)
	if got := none.ExcludedTagIDs(); len(got) != 0 {
		t.Errorf("ExcludedTagIDs(empty) returned %d ids, want 0", len(got))
	}
	_, countsNo := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if countsNo.Open != 2 || countsNo.HiddenByCeiling != 0 {
		t.Errorf("no-filter counts: open=%d hidden=%d, want 2/0", countsNo.Open, countsNo.HiddenByCeiling)
	}
}

func pairHas(p *sessionPairView, x, y int64) bool {
	if p == nil {
		return false
	}
	return (p.A.ID == x && p.B.ID == y) || (p.A.ID == y && p.B.ID == x)
}

func TestSessionLoadNextPair_PinsRequestedPair(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := insertRawImage(t, srv, "pin_a.png", "")
	b := insertRawImage(t, srv, "pin_b.png", "")
	c := insertRawImage(t, srv, "pin_c.png", "")

	queueRow(t, srv.db(), a, c, 0)
	queueRow(t, srv.db(), a, b, 5)
	none := library.NewCeiling("", cx)

	top, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if !pairHas(top, a, c) {
		t.Fatalf("unpinned pick should be (a,c), got A=%d B=%d", top.A.ID, top.B.ID)
	}

	pinned, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, a, b)
	if !pairHas(pinned, a, b) {
		t.Fatalf("pinned pick should be (a,b) despite sorting later, got A=%d B=%d", pinned.A.ID, pinned.B.ID)
	}

	fallback, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, a, 999999)
	if !pairHas(fallback, a, c) {
		t.Fatalf("missing pin should fall back to (a,c), got A=%d B=%d", fallback.A.ID, fallback.B.ID)
	}
}

func TestSessionLoadNextPair_SkippedLeaveTheWalk(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := insertRawImage(t, srv, "skip_a.png", "")
	b := insertRawImage(t, srv, "skip_b.png", "")
	c := insertRawImage(t, srv, "skip_c.png", "")

	queueRow(t, srv.db(), a, b, 0)
	queueRow(t, srv.db(), a, c, 5)
	none := library.NewCeiling("", cx)

	skip := func(x, y int64) {
		t.Helper()
		lo, hi := x, y
		if hi < lo {
			lo, hi = hi, lo
		}
		mustExec(t, srv.db().Write, `UPDATE potential_relation_pairs SET skipped_at = datetime('now') WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
	}

	skip(a, b)
	next, counts := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if !pairHas(next, a, c) {
		t.Fatalf("skipped pair came back instead of (a,c), got A=%d B=%d", next.A.ID, next.B.ID)
	}
	if counts.Open != 1 || counts.Skipped != 1 {
		t.Errorf("counts after one skip: open=%d skipped=%d, want 1/1", counts.Open, counts.Skipped)
	}

	skip(a, c)
	last, counts := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if last != nil {
		t.Fatalf("a skipped pair surfaced: A=%d B=%d", last.A.ID, last.B.ID)
	}
	if counts.Open != 0 || counts.Skipped != 2 {
		t.Errorf("counts with everything skipped: open=%d skipped=%d, want 0/2", counts.Open, counts.Skipped)
	}

	pinned, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, a, b)
	if !pairHas(pinned, a, b) {
		t.Fatal("pinned skipped pair did not surface")
	}
}

func TestSessionPage_VideoPairExposesFileType(t *testing.T) {
	srv := newTestServer(t)
	aID := insertBareImage(t, srv, "/tmp/clip_a.mp4", "mp4", "sha_clip_a")
	bID := insertBareImage(t, srv, "/tmp/clip_b.webm", "webm", "sha_clip_b")
	queueRow(t, srv.db(), aID, bID, 3)

	w := do(srv, "GET", "/relations/session")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, `data-a-type="mp4"`)
	wantIn(t, body, `data-b-type="webm"`)
	if !strings.Contains(body, `id="compare-slot-left"`) || !strings.Contains(body, `id="compare-slot-right"`) {
		t.Errorf("expected compare-slot-left / compare-slot-right containers in the dialog")
	}
	if strings.Contains(body, `id="compare-img-left"`) || strings.Contains(body, `id="compare-img-right"`) {
		t.Errorf("legacy <img id=\"compare-img-{left,right}\"> elements still in the dialog")
	}
}

func TestSessionPage_CellMediaSrc(t *testing.T) {
	srv := newTestServer(t)
	exec := func(path, ftype, sha string) int64 { return insertBareImage(t, srv, path, ftype, sha) }

	staticID := exec("/tmp/cell_static.png", "png", "sha_cell_static")
	videoID := exec("/tmp/cell_video.mp4", "mp4", "sha_cell_video")
	queueRow(t, srv.db(), staticID, videoID, 3)

	body := mustGetBody(t, srv, "/relations/session")
	if !strings.Contains(body, `<img src="/images/`+strconv.FormatInt(staticID, 10)+`/view"`) {
		t.Errorf("expected static-side <img> wired to /images/%d/view, body: %s", staticID, body)
	}
	if !strings.Contains(body, `<video class="session-cell-img" src="/images/`+strconv.FormatInt(videoID, 10)+`/file"`) {
		t.Errorf("expected mp4-side <video> wired to /images/%d/file, body: %s", videoID, body)
	}
	wantNotIn(t, body, `src="/thumbnails/`+srv.activeGallery()+`/`+strconv.FormatInt(staticID, 10)+`.jpg" alt="image `+strconv.FormatInt(staticID, 10)+`" class="session-cell-img"`)
	wantNotIn(t, body, `src="/thumbnails/`+srv.activeGallery()+`/`+strconv.FormatInt(videoID, 10)+`.jpg" alt="image `+strconv.FormatInt(videoID, 10)+`" class="session-cell-img"`)
}

func TestSessionPage_DecisionFormSwapsIntoTheFlashSlot(t *testing.T) {
	srv := newTestServer(t)
	a := insertBareImage(t, srv, "/tmp/flash_a.png", "png", "sha_flash_a")
	b := insertBareImage(t, srv, "/tmp/flash_b.png", "png", "sha_flash_b")
	queueRow(t, srv.db(), a, b, 3)

	body := mustGetBody(t, srv, "/relations/session")
	wantIn(t, body, `<div id="flash-relations" data-action-flash></div>`)
	wantIn(t, body, `hx-post="/relations/session/decide" hx-target="#flash-relations" hx-swap="innerHTML"`)
}

func TestSessionPage_OrderFormKeepsTheDetector(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Relations.TagPairs = true
	a := insertBareImage(t, srv, "/tmp/order_a.png", "png", "sha_order_a")
	b := insertBareImage(t, srv, "/tmp/order_b.png", "png", "sha_order_b")
	queueRow(t, srv.db(), a, b, 3)

	body := mustGetBody(t, srv, "/relations/session?detector=tags")
	wantIn(t, body, `<input type="hidden" name="detector" value="tags">
      <span class="field-hint">Order</span>`)
}

func TestSessionDecide_DuplicateOffersTheSideMarkedDuplicate(t *testing.T) {
	srv := newTestServer(t)
	original := insertBareImage(t, srv, "/tmp/decide_o.png", "png", "sha_decide_o")
	dup := insertBareImage(t, srv, "/tmp/decide_l.png", "png", "sha_decide_l")
	other := insertBareImage(t, srv, "/tmp/decide_r.png", "png", "sha_decide_r")
	if err := srv.active().RelationsSvc.AddDuplicate(dup, other); err != nil {
		t.Fatal(err)
	}
	queueRow(t, srv.db(), original, dup, 3)

	w := postForm(t, srv, "/relations/session/decide", url.Values{
		"a": {strconv.FormatInt(min(original, dup), 10)}, "b": {strconv.FormatInt(max(original, dup), 10)},
		"left": {strconv.FormatInt(original, 10)}, "type": {"duplicate"},
	})
	mustCode(t, w, http.StatusNoContent)
	if got := w.Header().Get("X-Relations-Duplicate-ID"); got != strconv.FormatInt(dup, 10) {
		t.Errorf("offered for deletion = %s, want the image marked duplicate %d", got, dup)
	}
	if got := w.Header().Get("X-Relations-Duplicate-OriginalID"); got != strconv.FormatInt(original, 10) {
		t.Errorf("original = %s, want the image marked original %d", got, original)
	}
}

func TestSessionDecideDuplicate_UniqueTagsHeaderFollowsTheCopy(t *testing.T) {
	cases := []struct {
		name string
		tag  func(t *testing.T, srv *Server, dup, other, tagID int64)
		want string
	}{
		{"only an implied row differs", func(t *testing.T, srv *Server, dup, _, tagID int64) {
			mustExec(t, srv.db().Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?, ?, 1)`, dup, tagID)
		}, "0"},
		{"another member has a tag", func(t *testing.T, srv *Server, _, other, tagID int64) {
			mustAddTag(t, srv.tagSvc(), other, tagID, false, nil)
		}, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			original := insertBareImage(t, srv, "/tmp/unique_o.png", "png", "sha_unique_o")
			dup := insertBareImage(t, srv, "/tmp/unique_l.png", "png", "sha_unique_l")
			other := insertBareImage(t, srv, "/tmp/unique_r.png", "png", "sha_unique_r")
			if err := srv.active().RelationsSvc.AddDuplicate(dup, other); err != nil {
				t.Fatal(err)
			}
			tc.tag(t, srv, dup, other, mustTag(t, srv.tagSvc(), "extra", srv.active().GeneralCategoryID).ID)
			queueRow(t, srv.db(), original, dup, 3)

			w := postForm(t, srv, "/relations/session/decide", url.Values{
				"a": {strconv.FormatInt(min(original, dup), 10)}, "b": {strconv.FormatInt(max(original, dup), 10)},
				"left": {strconv.FormatInt(original, 10)}, "type": {"duplicate"},
			})
			mustCode(t, w, http.StatusNoContent)
			if got := w.Header().Get("X-Relations-Duplicate-HasUniqueTags"); got != tc.want {
				t.Errorf("HasUniqueTags = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionDecideDuplicate_UniqueSourcesHeader(t *testing.T) {
	cases := []struct {
		name   string
		source func(t *testing.T, srv *Server, original, dup int64)
		want   string
	}{
		{"no sources", func(*testing.T, *Server, int64, int64) {}, "0"},
		{"the duplicate has one", func(t *testing.T, srv *Server, _, dup int64) {
			mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'site_b', '7', 'https://b.example/7')`, dup)
		}, "1"},
		{"the original has it already", func(t *testing.T, srv *Server, original, dup int64) {
			for _, id := range []int64{original, dup} {
				mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'site_b', '7', 'https://b.example/7')`, id)
			}
		}, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			original := insertBareImage(t, srv, "/tmp/src_o.png", "png", "sha_src_o")
			dup := insertBareImage(t, srv, "/tmp/src_d.png", "png", "sha_src_d")
			tc.source(t, srv, original, dup)
			queueRow(t, srv.db(), original, dup, 3)

			w := postForm(t, srv, "/relations/session/decide", url.Values{
				"a": {strconv.FormatInt(min(original, dup), 10)}, "b": {strconv.FormatInt(max(original, dup), 10)},
				"left": {strconv.FormatInt(original, 10)}, "type": {"duplicate"},
			})
			mustCode(t, w, http.StatusNoContent)
			if got := w.Header().Get("X-Relations-Duplicate-HasUniqueSources"); got != tc.want {
				t.Errorf("HasUniqueSources = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarkedWalkerDeleteOne_RefusesTheOriginal(t *testing.T) {
	srv := newTestServer(t)
	original := insertBareImage(t, srv, "/tmp/walker_o.png", "png", "sha_walker_o")
	dup := insertBareImage(t, srv, "/tmp/walker_d.png", "png", "sha_walker_d")
	if err := srv.active().RelationsSvc.AddDuplicate(original, dup); err != nil {
		t.Fatal(err)
	}

	w := postForm(t, srv, "/relations/duplicates/marked/delete-one", url.Values{"image_id": {strconv.FormatInt(original, 10)}})
	mustCode(t, w, http.StatusConflict)
	wantIn(t, w.Body.String(), "original, so it was kept")
	if n := countRows(t, srv.db(), "images WHERE id = ?", original); n != 1 {
		t.Error("the original was deleted")
	}
}

func TestSessionPage_CellMediaArchiveKeepsThumbnail(t *testing.T) {
	srv := newTestServer(t)
	exec := func(path, ftype, sha string) int64 { return insertBareImage(t, srv, path, ftype, sha) }
	zipA := exec("/tmp/cell_zip_a.cbz", "cbz", "sha_zip_a")
	zipB := exec("/tmp/cell_zip_b.cbz", "cbz", "sha_zip_b")
	queueRow(t, srv.db(), zipA, zipB, 4)

	body := mustGetBody(t, srv, "/relations/session")
	for _, id := range []int64{zipA, zipB} {
		want := `src="/thumbnails/` + srv.activeGallery() + `/` + strconv.FormatInt(id, 10) + `.jpg"`
		wantIn(t, body, want)
	}
}

func TestSessionPage_HiddenByCeilingPromptsRaise(t *testing.T) {
	srv := newTestServer(t)
	a := insertTestImage(t, srv)
	overID := insertBareImage(t, srv, "/tmp/over.jpg", "jpg", "sha_over")
	addRatingTag(t, srv.db(), overID, "explicit")
	queueRow(t, srv.db(), a, overID, 5)

	w := do(srv, "GET", "/relations/session", [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "No pairs at your")
	wantIn(t, body, "Raise ceiling to explicit")
	wantIn(t, body, "/internal/rating-ceiling?level=explicit")
}

func TestSessionLoadNextPair_SameCollectionHiddenUntilOptIn(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := insertTestImage(t, srv)
	bID := insertBareImage(t, srv, "/tmp/album-b.jpg", "jpg", "sha_album_b")
	for _, id := range []int64{a, bID} {
		mustExec(t, srv.db().Write, `INSERT INTO image_collections (image_id, name) VALUES (?, 'Album')`, id)
	}
	queueRow(t, srv.db(), a, bID, 2)

	none := library.NewCeiling("", cx)
	pair, counts := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if pair != nil || counts.Open != 0 {
		t.Fatalf("same-collection pair surfaced by default: pair=%v open=%d", pair, counts.Open)
	}
	if c := loadRelationsCounts(cx, none, ""); c.QueueOpen != 0 {
		t.Fatalf("hub QueueOpen = %d, want 0 while the collection is opted out", c.QueueOpen)
	}

	mustExec(t, srv.db().Write, `INSERT INTO collection_find_relations (name) VALUES ('album')`)
	pair, counts, err := loadNextPair(cx, "smallest_distance_first", "both", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair after opt-in: %v", err)
	}
	if !pairHas(pair, a, bID) || counts.Open != 1 {
		t.Fatalf("opted-in pair missing: pair=%v open=%d", pair, counts.Open)
	}
	if c := loadRelationsCounts(cx, none, ""); c.QueueOpen != 1 {
		t.Fatalf("hub QueueOpen = %d, want 1 after opt-in", c.QueueOpen)
	}

	for _, id := range []int64{a, bID} {
		mustExec(t, srv.db().Write, `INSERT INTO image_collections (image_id, name) VALUES (?, 'Other')`, id)
	}
	pair, counts, err = loadNextPair(cx, "smallest_distance_first", "both", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair with mixed collections: %v", err)
	}
	if pair != nil || counts.Open != 0 {
		t.Fatalf("pair sharing a disabled collection surfaced: pair=%v open=%d", pair, counts.Open)
	}
}

func queueTagRow(t *testing.T, database *db.DB, a, b int64, distance int, source string, score float64) {
	t.Helper()
	lo, hi := a, b
	if hi < lo {
		lo, hi = hi, lo
	}
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, source, score) VALUES (?, ?, ?, datetime('now'), ?, ?)`, lo, hi, distance, source, score)
}

func TestSessionLoadNextPair_LeftSlotBySource(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	older := insertRawImage(t, srv, "left_older.png", "")
	newer := insertRawImage(t, srv, "left_newer.png", "")

	mustExec(t, srv.db().Write, `UPDATE images SET file_size = 100 WHERE id = ?`, older)
	mustExec(t, srv.db().Write, `UPDATE images SET file_size = 900 WHERE id = ?`, newer)
	none := library.NewCeiling("", cx)

	queueTagRow(t, srv.db(), older, newer, 3, relations.SourcePhash, 0)
	phashPair, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if phashPair.LeftID != newer {
		t.Fatalf("phash pair left = %d, want the bigger file %d", phashPair.LeftID, newer)
	}
	if phashPair.FromTags() {
		t.Error("a phash row reported itself as tag-sourced")
	}

	mustExec(t, srv.db().Write, `DELETE FROM potential_relation_pairs`)
	queueTagRow(t, srv.db(), older, newer, 18, relations.SourceTags, 0.91)
	tagPair, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "both", none, 0, 0)
	if tagPair.LeftID != older {
		t.Fatalf("tag pair left = %d, want the older image %d", tagPair.LeftID, older)
	}
	if !tagPair.FromTags() {
		t.Error("a tags row did not report itself as tag-sourced")
	}
	if got := tagPair.ScorePercent(); got != 91 {
		t.Fatalf("score percent = %d, want 91", got)
	}
}

func TestSessionLoadNextPair_DetectorScope(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := insertRawImage(t, srv, "det_a.png", "")
	b := insertRawImage(t, srv, "det_b.png", "")
	c := insertRawImage(t, srv, "det_c.png", "")
	d := insertRawImage(t, srv, "det_d.png", "")
	queueTagRow(t, srv.db(), a, b, 3, relations.SourcePhash, 0)
	queueTagRow(t, srv.db(), a, c, 18, relations.SourceTags, 0.9)
	queueTagRow(t, srv.db(), a, d, 4, relations.SourceBoth, 0.9)
	queueTagRow(t, srv.db(), b, c, 0, relations.SourceReview, 0)
	none := library.NewCeiling("", cx)

	for _, tc := range []struct {
		detector string
		want     int
	}{
		{"both", 4},
		{"phash", 3},
		{"tags", 3},
	} {
		_, counts := mustLoadNextPair(t, cx, "smallest_distance_first", tc.detector, none, 0, 0)
		if counts.Open != tc.want {
			t.Errorf("detector %q sees %d pair(s), want %d", tc.detector, counts.Open, tc.want)
		}
	}

	pinned, _ := mustLoadNextPair(t, cx, "smallest_distance_first", "phash", none, a, c)
	if !pairHas(pinned, a, c) {
		t.Fatalf("pinned tag pair was hidden by the phash scope, got A=%d B=%d", pinned.A.ID, pinned.B.ID)
	}
}

func insertBareImage(t *testing.T, srv *Server, path, ftype, sha string) int64 {
	t.Helper()
	id, _ := mustExec(t, srv.db().Write,
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES (?, ?, 1024, ?, datetime('now'))`,
		path, ftype, sha).LastInsertId()
	return id
}

func TestLoadCompareFacts_ReadsTheImageRows(t *testing.T) {
	srv := newTestServer(t)
	a := insertBareImage(t, srv, "/tmp/facts_a.PNG", "png", "sha_facts_a")
	b := insertBareImage(t, srv, "/tmp/facts_b", "jpeg", "sha_facts_b")
	mustExec(t, srv.db().Write, `UPDATE images SET width = 640, height = 480, file_size = 4096, ingested_at = '2026-03-04T12:00:00Z', series = 'saga' WHERE id = ?`, a)

	left, right, err := loadCompareFacts(srv.active(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := relationCompareFacts{ImageID: a, ResolutionW: 640, ResolutionH: 480, FileSize: 4096,
		AddedAt: humanISODate("2026-03-04T12:00:00Z"), Format: ".png", Collection: "saga"}
	if left.ImageID != want.ImageID || left.ResolutionW != want.ResolutionW || left.ResolutionH != want.ResolutionH ||
		left.FileSize != want.FileSize || left.AddedAt != want.AddedAt || left.Format != want.Format || left.Collection != want.Collection ||
		left.PageCount != want.PageCount {
		t.Errorf("left facts = %+v, want %+v", left, want)
	}
	if right.ResolutionW != 0 || right.ResolutionH != 0 || right.FileSize != 1024 || right.AddedAt == "" || right.Format != "" || right.Collection != "" ||
		right.PageCount != 0 {
		t.Errorf("right facts = %+v, want 0x0, 1024 bytes, a date, no format, no collection, no pages", right)
	}
}

func TestSessionPage_PageCountRowMarksTheLongerArchiveOnly(t *testing.T) {
	srv := newTestServer(t)
	short := insertBareImage(t, srv, "/tmp/pages_short.cbz", "cbz", "sha_pages_short")
	long := insertBareImage(t, srv, "/tmp/pages_long.cbz", "cbz", "sha_pages_long")
	still := insertBareImage(t, srv, "/tmp/pages_still.png", "png", "sha_pages_still")
	mustExec(t, srv.db().Write, `UPDATE images SET page_count = 24 WHERE id = ?`, short)
	mustExec(t, srv.db().Write, `UPDATE images SET page_count = 31 WHERE id = ?`, long)
	queueRow(t, srv.db(), short, long, 3)
	queueRow(t, srv.db(), short, still, 3)

	pageRow := func(a, b int64) string {
		body := mustGetBody(t, srv, "/relations/session?a="+strconv.FormatInt(a, 10)+"&b="+strconv.FormatInt(b, 10))
		start := strings.Index(body, "<th>Page count</th>")
		if start < 0 {
			t.Fatalf("no Page count row for #%d and #%d", a, b)
		}
		return body[start : start+strings.Index(body[start:], "</tr>")]
	}

	row := pageRow(short, long)
	wantIn(t, row, `<td>24</td>`)
	wantIn(t, row, `<td class="cmp-win" title="More pages">31 ←</td>`)

	row = pageRow(short, still)
	wantIn(t, row, `<td>24</td>`)
	wantIn(t, row, `<td>—</td>`)
	wantNotIn(t, row, "cmp-win")
}

func TestLoadCompareFacts_CapsEachSidesUniqueTagsOnItsOwn(t *testing.T) {
	srv := newTestServer(t)
	a := insertBareImage(t, srv, "/tmp/delta_a.png", "png", "sha_delta_a")
	b := insertBareImage(t, srv, "/tmp/delta_b.png", "png", "sha_delta_b")
	general := srv.active().GeneralCategoryID
	for _, side := range []struct {
		prefix string
		n      int
		image  int64
	}{{"aa_", 210, a}, {"zz_", 3, b}} {
		mustExec(t, srv.db().Write, `
			INSERT INTO tags (name, category_id)
			WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
			SELECT printf('%s%03d', ?, i), ? FROM n`, side.n, side.prefix, general)
		mustExec(t, srv.db().Write, `INSERT INTO image_tags (image_id, tag_id) SELECT ?, id FROM tags WHERE name LIKE ? || '%'`, side.image, side.prefix)
	}

	left, right, err := loadCompareFacts(srv.active(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(left.UniqueTags) != 5 || left.UniqueTagsTotal != 210 {
		t.Errorf("left unique = %d of %d, want 5 of 210", len(left.UniqueTags), left.UniqueTagsTotal)
	}
	if len(right.UniqueTags) != 3 || right.UniqueTagsTotal != 3 {
		t.Errorf("right unique = %d of %d, want 3 of 3", len(right.UniqueTags), right.UniqueTagsTotal)
	}
	if len(left.UniqueTags) > 0 && left.UniqueTags[0].Name != "aa_001" {
		t.Errorf("left's first unique tag = %q, want aa_001", left.UniqueTags[0].Name)
	}
}
