package web

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/gallery"
)

func seedManga(t *testing.T, srv *Server, name string, pages [][]byte) int64 {
	t.Helper()
	entries := make([]mangaEntry, len(pages))
	for i, body := range pages {
		entries[i] = mangaEntry{fmt.Sprintf("page_%03d.png", i+1), body}
	}
	return seedMangaEntries(t, srv, name, entries)
}

type mangaEntry struct {
	name string
	body []byte
}

func seedMangaEntries(t *testing.T, srv *Server, name string, entries []mangaEntry) int64 {
	t.Helper()
	cx := srv.active()
	if cx == nil {
		t.Fatal("no active gallery")
	}
	cbzPath := filepath.Join(cx.GalleryPath, name)
	f, err := os.Create(cbzPath)
	if err != nil {
		t.Fatalf("create cbz: %v", err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			_ = f.Close()
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(e.body); err != nil {
			_ = f.Close()
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("zip close: %v", err)
	}
	_ = f.Close()
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, cbzPath, "")
	return rec.ID
}

func tinyPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	return makePNGBytes(t, w, h, c.R, c.G, c.B)
}

func blankPages(t *testing.T, n int) [][]byte {
	pages := make([][]byte, n)
	for i := range pages {
		pages[i] = tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	}
	return pages
}

func TestReader_RendersAtPage1(t *testing.T) {
	srv := newTestServer(t)
	pages := blankPages(t, 3)
	id := seedManga(t, srv, "m.cbz", pages)

	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/read?page=1")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	wantIn(t, body, `class="reader-img"`)
	if !strings.Contains(body, "1 / 3") {
		t.Errorf("page counter not rendered: %q", snippet(body, "reader-counter"))
	}
}

func TestReader_OutOfRangePageClamped(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", blankPages(t, 2))

	cases := map[string]string{
		"0":   "1",
		"-3":  "1",
		"999": "2",
	}
	for raw, want := range cases {
		w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/read?page="+raw)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("page=%s: status %d, want 303", raw, w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.Contains(loc, "page="+want) {
			t.Errorf("page=%s redirected to %q, want page=%s", raw, loc, want)
		}
	}
}

func TestReader_TracksResumeBookmark(t *testing.T) {
	srv := newTestServer(t)
	pages := blankPages(t, 5)
	id := seedManga(t, srv, "resume.cbz", pages)

	read := func(page int) {
		t.Helper()
		w := do(srv, "GET", fmt.Sprintf("/images/%d/read?page=%d", id, page))
		if w.Code != 200 {
			t.Fatalf("read page %d: status %d", page, w.Code)
		}
	}
	stored := func() *int {
		t.Helper()
		var v *int
		if err := srv.db().Read.QueryRow(`SELECT last_read_page FROM images WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	read(1)
	if got := stored(); got != nil {
		t.Errorf("after page 1: last_read_page = %d, want NULL", *got)
	}
	read(3)
	if got := stored(); got == nil || *got != 3 {
		t.Errorf("after page 3: last_read_page = %v, want 3", got)
	}
	read(4)
	if got := stored(); got == nil || *got != 4 {
		t.Errorf("after page 4: last_read_page = %v, want 4", got)
	}
	read(5)
	if got := stored(); got != nil {
		t.Errorf("after the last page: last_read_page = %d, want NULL", *got)
	}
}

func TestMangaResume_StaleBookmarkClamped(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "shrunk.cbz", blankPages(t, 3))
	mustExec(t, srv.db().Write, `UPDATE images SET last_read_page = 99 WHERE id = ?`, id)

	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	if body := w.Body.String(); strings.Contains(body, "read?page=99") || strings.Contains(body, "(p. 99)") {
		t.Errorf("detail page offered the stale bookmark: %q", snippet(body, "btn-read"))
	}

	w = do(srv, "GET", fmt.Sprintf("/images/%d/pages", id))
	if body := w.Body.String(); strings.Contains(body, "manga-page-bookmark") {
		t.Errorf("pages grid marked a bookmark past the end")
	}
}

func TestReader_BarePageResumes(t *testing.T) {
	srv := newTestServer(t)
	pages := blankPages(t, 5)
	id := seedManga(t, srv, "bare-resume.cbz", pages)
	mustExec(t, srv.db().Write, `UPDATE images SET last_read_page = 3 WHERE id = ?`, id)

	w := do(srv, "GET", fmt.Sprintf("/images/%d/read", id))
	if w.Code != 200 {
		t.Fatalf("bare reader: status %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "page/3") {
		t.Errorf("bare reader did not render page 3: %q", snippet(body, "reader"))
	}
	var stored *int
	if err := srv.db().Read.QueryRow(`SELECT last_read_page FROM images WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == nil || *stored != 3 {
		t.Errorf("bare reader left last_read_page = %v, want 3", stored)
	}

	do(srv, "GET", fmt.Sprintf("/images/%d/read?page=1", id))
	if err := srv.db().Read.QueryRow(`SELECT last_read_page FROM images WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Errorf("explicit page=1 left last_read_page = %d, want NULL", *stored)
	}
}

func TestDetailPage_ResumeLink(t *testing.T) {
	srv := newTestServer(t)
	pages := blankPages(t, 4)
	id := seedManga(t, srv, "resume-detail.cbz", pages)

	detail := func() string {
		t.Helper()
		w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
		return w.Body.String()
	}
	if body := detail(); !strings.Contains(body, fmt.Sprintf("/images/%d/read?page=1", id)) {
		t.Errorf("unbookmarked detail lost the page-1 entry point: %q", snippet(body, "btn-read"))
	}
	mustExec(t, srv.db().Write, `UPDATE images SET last_read_page = 3 WHERE id = ?`, id)
	body := detail()
	if !strings.Contains(body, fmt.Sprintf("/images/%d/read?page=3", id)) {
		t.Errorf("detail did not resume at page 3: %q", snippet(body, "btn-read"))
	}
	if !strings.Contains(body, "(p. 3)") {
		t.Errorf("Open in reader label missing the resume page: %q", snippet(body, "btn-read"))
	}
}

func TestReader_404OnNonManga(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "regular.png", 8, 8)
	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/read?page=1")
	wantCode(t, w, http.StatusNotFound)
}

func TestPagesGrid_Renders(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", blankPages(t, 3))
	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/pages")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, frag := range []string{
		"manga-pages-grid",
		"/page/1/thumb",
		"/page/3/thumb",
	} {
		wantIn(t, body, frag)
	}
}

func TestServeMangaPage_404OnRange(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", blankPages(t, 1))
	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/page/2")
	wantCode(t, w, http.StatusNotFound)
}

func TestServeMangaPage_HitServesBytes(t *testing.T) {
	srv := newTestServer(t)
	pic := tinyPNG(t, 16, 16, color.RGBA{77, 77, 77, 255})
	id := seedManga(t, srv, "m.cbz", [][]byte{pic})
	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/page/1")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), pic) {
		t.Errorf("served bytes do not match the source page")
	}
}

func TestServeMangaPage_RevalidatesToNotModified(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", [][]byte{tinyPNG(t, 16, 16, color.RGBA{77, 77, 77, 255})})
	page := "/images/" + strconv.FormatInt(id, 10) + "/page/1"
	etag := do(srv, "GET", page).Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the page")
	}
	w := do(srv, "GET", page, [2]string{"If-None-Match", etag})
	wantCode(t, w, http.StatusNotModified)
}

func TestServeMangaPage_NegotiatesAJPEGXLPage(t *testing.T) {
	src := filepath.Join(t.TempDir(), "p.jxl")
	generateStill(t, src, 64, 48)
	jxl, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	id := seedMangaEntries(t, srv, "m.cbz", []mangaEntry{
		{"01.jxl", jxl},
		{"02.png", tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})},
	})

	lastThumb := filepath.Join(gallery.MangaCacheDir(srv.active().ThumbnailsPath), strconv.FormatInt(id, 10), "page_0002_thumb.jpg")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(lastThumb); err == nil {
			break
		}
	}
	varies := func(w *httptest.ResponseRecorder) bool { return slices.Contains(w.Header().Values("Vary"), "Accept") }

	page := fmt.Sprintf("/images/%d/page/1", id)
	orig := getAccepting(t, srv, page, "image/jxl,image/*")
	if ct := orig.Header().Get("Content-Type"); ct != "image/jxl" || !bytes.Equal(orig.Body.Bytes(), jxl) || !varies(orig) {
		t.Errorf("original = %s (vary %v), want the page's own bytes as image/jxl varying on Accept", ct, varies(orig))
	}
	for _, accept := range []string{"image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8", ""} {
		w := getAccepting(t, srv, page, accept)
		if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" || !varies(w) {
			t.Fatalf("Accept %q: %s (vary %v), want image/jpeg varying on Accept", accept, ct, varies(w))
		}
		if cfg, err := jpeg.DecodeConfig(w.Body); err != nil || cfg.Width != 64 || cfg.Height != 48 {
			t.Errorf("Accept %q: copy = %dx%d (%v), want 64x48", accept, cfg.Width, cfg.Height, err)
		}
		if w.Header().Get("ETag") == orig.Header().Get("ETag") {
			t.Errorf("Accept %q: the copy shares the original's ETag", accept)
		}
	}

	for _, target := range []string{page + "/thumb", fmt.Sprintf("/images/%d/page/2", id)} {
		if w := getAccepting(t, srv, target, "image/jxl"); varies(w) {
			t.Errorf("%s varies on Accept", target)
		}
	}
}

func TestServeMangaPage_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	pic := tinyPNG(t, 16, 16, color.RGBA{77, 77, 77, 255})
	id := seedManga(t, srv, "m.cbz", [][]byte{pic})

	outside := filepath.Join(t.TempDir(), "evil.cbz")
	cx := srv.active()
	mustExec(t, cx.DB.Write, `UPDATE images SET canonical_path = ? WHERE id = ?`, outside, id)
	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/page/1")
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestSetCollection_Adds(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", blankPages(t, 1))
	w := postForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/collections/set", url.Values{"collection": {"Naruto"}})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("set collection status = %d, body = %s", w.Code, w.Body.String())
	}
	cx := srv.active()
	var got string
	if err := cx.DB.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "Naruto" {
		t.Errorf("home collection (DB col 'series') = %q, want %q", got, "Naruto")
	}
	cols, err := gallery.CollectionsForImage(cx.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].Name != "Naruto" {
		t.Errorf("memberships = %#v, want one Naruto", cols)
	}
}

func TestCollectionMembership_PromoteHomeOnRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "multi.png", 7, 7)
	d := srv.active().DB
	mustAddCollection(t, d, id, "First", nil)
	mustAddCollection(t, d, id, "Second", nil)
	home := func() string {
		var s string
		if err := d.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if home() != "First" {
		t.Errorf("home = %q, want First", home())
	}
	if cols, _ := gallery.CollectionsForImage(d, id); len(cols) != 2 {
		t.Fatalf("memberships = %d, want 2", len(cols))
	}
	if err := gallery.RemoveCollectionMembership(d, id, "First"); err != nil {
		t.Fatal(err)
	}
	if home() != "Second" {
		t.Errorf("after removing home, series = %q, want Second", home())
	}
	if err := gallery.RemoveCollectionMembership(d, id, "Second"); err != nil {
		t.Fatal(err)
	}
	if home() != "" {
		t.Errorf("after removing all, series = %q, want empty", home())
	}
}

func snippet(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	end := i + 120
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}

func TestSetCollection_NonMangaWithOrder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "non-manga.png", 16, 16)
	w := postForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/collections/set", url.Values{"collection": {"Vol1"}, "collection_order": {"7"}})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("update collection status = %d, body = %s", w.Code, w.Body.String())
	}

	var collectionGot string
	var orderGot sql.NullInt64
	if err := srv.active().DB.Read.QueryRow(
		`SELECT series, series_order FROM images WHERE id = ?`, id,
	).Scan(&collectionGot, &orderGot); err != nil {
		t.Fatal(err)
	}
	if collectionGot != "Vol1" {
		t.Errorf("collection = %q, want Vol1", collectionGot)
	}
	if !orderGot.Valid || orderGot.Int64 != 7 {
		t.Errorf("collection_order = %v (valid=%v), want 7", orderGot.Int64, orderGot.Valid)
	}
}

func TestCollectionSuggest_PrefixMatch(t *testing.T) {
	srv := newTestServer(t)
	id1 := seedImage(t, srv, "a.png", 4, 4)
	id2 := seedImage(t, srv, "b.png", 5, 5)
	mustSetHome(t, srv.active().DB, id1, "Naruto", nil)
	mustSetHome(t, srv.active().DB, id2, "Bleach", nil)
	w := do(srv, "GET", "/internal/collection/suggest?prefix=Nar")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `data-series="Naruto"`)
	wantNotIn(t, w.Body.String(), `data-series="Bleach"`)
}

func TestCollectionSuggest_CaseInsensitivePrefix(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "a.png", 4, 4)
	mustSetHome(t, srv.active().DB, id, "Cat Series", nil)
	for _, prefix := range []string{"cat", "Cat", "CAT"} {
		w := do(srv, "GET", "/internal/collection/suggest?prefix="+prefix)
		if w.Code != http.StatusOK {
			t.Fatalf("collection suggest prefix=%s: %d", prefix, w.Code)
		}
		if !strings.Contains(w.Body.String(), `data-series="Cat Series"`) {
			t.Errorf("prefix=%s should surface Cat Series, got %s", prefix, w.Body.String())
		}
	}
}

func TestBatchCollection_AssignsLabel(t *testing.T) {
	srv := newTestServer(t)
	var ids []int64
	for i := 0; i < 3; i++ {

		ids = append(ids, seedImage(t, srv, fmt.Sprintf("p%d.png", i), 4+i, 4+i))
	}
	form := url.Values{
		"scope":      {"selection"},
		"collection": {"Bundle"},
	}
	for _, id := range ids {
		form.Add("ids", strconv.FormatInt(id, 10))
	}
	w := postForm(t, srv, "/internal/batch-collection", form)
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	for i, id := range ids {
		var collection string
		var order sql.NullInt64
		if err := srv.active().DB.Read.QueryRow(
			`SELECT series, series_order FROM images WHERE id = ?`, id,
		).Scan(&collection, &order); err != nil {
			t.Fatal(err)
		}
		if collection != "Bundle" {
			t.Errorf("ids[%d]: collection = %q, want Bundle", i, collection)
		}
		if order.Valid {
			t.Errorf("ids[%d]: collection_order should remain NULL, got %d", i, order.Int64)
		}
	}
}

func TestBatchCollection_AddsKeepingExisting(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "existing.png", 9, 9)
	mustSetHome(t, srv.active().DB, id, "Original", nil)
	form := url.Values{
		"scope":      {"selection"},
		"collection": {"Added"},
		"ids":        {strconv.FormatInt(id, 10)},
	}
	w := postForm(t, srv, "/internal/batch-collection", form)
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	var home string
	if err := srv.active().DB.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != "Original" {
		t.Errorf("home = %q, want Original (unchanged)", home)
	}
	names := map[string]bool{}
	cols, err := gallery.CollectionsForImage(srv.active().DB, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		names[c.Name] = true
	}
	if !names["Original"] || !names["Added"] {
		t.Errorf("memberships = %#v, want both Original and Added", cols)
	}
}

func TestBatchCollection_RemoveMode(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "rm.png", 11, 11)
	d := srv.active().DB
	mustSetHome(t, d, id, "Drop", nil)
	mustAddCollection(t, d, id, "Keep", nil)
	form := url.Values{
		"scope":      {"selection"},
		"collection": {"Drop"},
		"mode":       {"remove"},
		"ids":        {strconv.FormatInt(id, 10)},
	}
	w := postForm(t, srv, "/internal/batch-collection", form)
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	cols, err := gallery.CollectionsForImage(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].Name != "Keep" {
		t.Errorf("memberships = %#v, want only Keep", cols)
	}
	var home string
	if err := d.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != "Keep" {
		t.Errorf("home = %q, want Keep (promoted)", home)
	}
}

func TestDetailPage_MangaActionAnchorsForwardAllBackParams(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "ctx.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{1, 2, 3, 255}),
		tinyPNG(t, 8, 8, color.RGBA{4, 5, 6, 255}),
	})

	url := fmt.Sprintf(
		"/images/%d?back_q=foo&back_sort=newest&back_order=desc&back_page=2&back_seed=42",
		id,
	)
	body := mustGetBody(t, srv, url)

	for _, must := range []string{
		`btn-read`,
		`back_q=foo`,
		`back_sort=newest`,
		`back_order=desc`,
		`back_page=`,
		`back_seed=42`,
	} {
		wantIn(t, body, must)
	}
	mangaSec := body[strings.Index(body, "manga-actions"):]
	for _, must := range []string{`/pages?`, `back_seed=42`, `back_sort=newest`} {
		if !strings.Contains(mangaSec, must) {
			t.Errorf("manga-actions section missing %q; section was %s", must, mangaSec[:min(len(mangaSec), 600)])
		}
	}
	if strings.Contains(mangaSec, "%26") || strings.Contains(mangaSec, "%3d") {
		t.Errorf("manga-actions href URL-encoded a separator - browser will reparse the trailing back_* as the page value: %s", mangaSec[:min(len(mangaSec), 600)])
	}
}

func TestReaderNavHrefs_DoNotURLEncodeBackParams(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "nav.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{1, 1, 1, 255}),
		tinyPNG(t, 8, 8, color.RGBA{2, 2, 2, 255}),
		tinyPNG(t, 8, 8, color.RGBA{3, 3, 3, 255}),
	})
	url := fmt.Sprintf(
		"/images/%d/read?page=2&back_q=foo&back_sort=newest&back_order=desc&back_seed=42&from=pages",
		id,
	)
	body := mustGetBody(t, srv, url)
	for _, banned := range []string{"%26back_q", "%26back_sort", "%26back_order", "%26back_seed", "%26from", "%3D"} {
		wantNotIn(t, body, banned)
	}

	if !strings.Contains(body, `?page=3&amp;`) || !strings.Contains(body, `back_q=foo`) {
		extract := body[max(0, strings.Index(body, "reader-side-next")-50):min(len(body), strings.Index(body, "reader-side-next")+250)]
		t.Errorf("next-chevron href missing `?page=3&amp;…back_q=foo`. extract: %s", extract)
	}
}

func extractPage(t *testing.T, srv *Server, id int64, n int) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}}
	req := httptest.NewRequest(
		"POST",
		fmt.Sprintf("/images/%d/page/%d/extract", id, n),
		strings.NewReader(form.Encode()),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func redirectedImageID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	mustCode(t, w, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	id, err := strconv.ParseInt(strings.TrimPrefix(loc, "/images/"), 10, 64)
	if err != nil {
		t.Fatalf("extract redirected to %q", loc)
	}
	return id
}

func TestExtractMangaPage_CreatesLinkedImage(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "extract.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{10, 20, 30, 255}),
		tinyPNG(t, 8, 8, color.RGBA{40, 50, 60, 255}),
		tinyPNG(t, 8, 8, color.RGBA{70, 80, 90, 255}),
	})

	newID := redirectedImageID(t, extractPage(t, srv, id, 2))
	if newID == id {
		t.Fatal("extract landed back on the archive row")
	}

	var canonPath, origin, fileType string
	var isInbox int
	if err := srv.db().Read.QueryRow(
		`SELECT canonical_path, origin, file_type, is_inbox FROM images WHERE id = ?`, newID,
	).Scan(&canonPath, &origin, &fileType, &isInbox); err != nil {
		t.Fatalf("load extracted row: %v", err)
	}
	if origin != "extract" {
		t.Errorf("origin = %q, want extract", origin)
	}
	if isInbox != 1 {
		t.Error("extracted row did not land in the inbox")
	}
	if fileType != "png" {
		t.Errorf("file_type = %q, want png", fileType)
	}
	if want := "extract_p0002.png"; filepath.Base(canonPath) != want {
		t.Errorf("filename = %q, want %q", filepath.Base(canonPath), want)
	}
	if _, err := os.Stat(canonPath); err != nil {
		t.Errorf("extracted file not on disk: %v", err)
	}

	var source int64
	if err := srv.db().Read.QueryRow(
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, newID,
	).Scan(&source); err != nil {
		t.Fatalf("derivative edge missing: %v", err)
	}
	if source != id {
		t.Errorf("edge source = %d, want the archive %d", source, id)
	}
}

func TestExtractMangaPage_RepeatFoldsOntoExistingRow(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "again.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{11, 21, 31, 255}),
		tinyPNG(t, 8, 8, color.RGBA{41, 51, 61, 255}),
	})

	first := redirectedImageID(t, extractPage(t, srv, id, 1))
	second := redirectedImageID(t, extractPage(t, srv, id, 1))
	if first != second {
		t.Fatalf("re-extract created a second row: %d then %d", first, second)
	}

	rows := countRows(t, srv.db(), "images WHERE origin = 'extract'")
	if rows != 1 {
		t.Errorf("extracted rows = %d, want 1", rows)
	}
	paths := countRows(t, srv.db(), "image_paths WHERE image_id = ?", first)
	if paths != 1 {
		t.Errorf("image_paths rows = %d, want 1 (the redundant alias should be dropped)", paths)
	}
	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "again_p0001_1.png")); !os.IsNotExist(err) {
		t.Error("the redundant copy stayed on disk")
	}
}

func TestExtractMangaPage_HTMXAnswersHXRedirect(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "hx.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{14, 24, 34, 255}),
	})
	w := postForm(t, srv, fmt.Sprintf("/images/%d/page/1/extract", id), url.Values{},
		[2]string{"HX-Request", "true"})
	mustCode(t, w, http.StatusNoContent)
	var newID int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM images WHERE origin = 'extract'`).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if got, want := w.Header().Get("HX-Redirect"), fmt.Sprintf("/images/%d", newID); got != want {
		t.Errorf("HX-Redirect = %q, want %q", got, want)
	}
}

func TestExtractMangaPage_RejectsOutOfRangeAndNonManga(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "range.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{12, 22, 32, 255}),
		tinyPNG(t, 8, 8, color.RGBA{42, 52, 62, 255}),
	})
	for _, n := range []int{0, -1, 3, 999} {
		if w := extractPage(t, srv, id, n); w.Code != http.StatusNotFound {
			t.Errorf("page %d: status %d, want 404", n, w.Code)
		}
	}

	plain := seedImage(t, srv, "plain.png", 10, 10)
	if w := extractPage(t, srv, plain, 1); w.Code != http.StatusNotFound {
		t.Errorf("non-cbz row: status %d, want 404", w.Code)
	}
}

func TestExtractMangaPage_KeepsOperatorRelation(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "ruled.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{13, 23, 33, 255}),
		tinyPNG(t, 8, 8, color.RGBA{43, 53, 63, 255}),
	})
	pageID := redirectedImageID(t, extractPage(t, srv, id, 2))

	svc := srv.active().RelationsSvc
	if err := svc.Overwrite("alternate", id, pageID); err != nil {
		t.Fatalf("Overwrite: %v", err)
	}

	if again := redirectedImageID(t, extractPage(t, srv, id, 2)); again != pageID {
		t.Fatalf("re-extract landed on %d, want %d", again, pageID)
	}
	edges := countRows(t, srv.db(), "derivative_edges WHERE derivative_image_id = ?", pageID)
	if edges != 0 {
		t.Errorf("derivative edge overrode the operator's alternate mark")
	}
}

func TestExtractMangaPage_FeedsABuiltBKTree(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "tree.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{10, 20, 30, 255}),
		tinyPNG(t, 8, 8, color.RGBA{200, 50, 60, 255}),
	})
	tree := builtTree(t, srv)

	newID := redirectedImageID(t, extractPage(t, srv, id, 2))
	if !treeHolds(t, srv, tree, newID) {
		t.Error("an extracted page is missing from the built BK-tree")
	}
}

func TestLoadMangaMeta_ReadsNullColumnsAsEmpty(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	if _, err := srv.db().Write.Exec(`INSERT INTO manga_metadata (image_id, title, year, month, community_rating, raw_xml)
		VALUES (?, 'Vol One', 2021, 0, 4.5, '<x/>')`, id); err != nil {
		t.Fatal(err)
	}

	m := loadMangaMeta(t.Context(), srv.db(), id)
	if m == nil {
		t.Fatal("loadMangaMeta = nil")
	}
	if m.ImageID != id || m.Title != "Vol One" || m.Series != "" || m.Writer != "" || m.RawXML != "<x/>" {
		t.Errorf("strings = %+v", m)
	}
	if m.Year == nil || *m.Year != 2021 || m.Month == nil || *m.Month != 0 || m.Count != nil || m.Day != nil || m.XMLPageCount != nil {
		t.Errorf("ints: year %v month %v count %v day %v xml pages %v", m.Year, m.Month, m.Count, m.Day, m.XMLPageCount)
	}
	if m.CommunityRating == nil || *m.CommunityRating != 4.5 {
		t.Errorf("community rating = %v, want 4.5", m.CommunityRating)
	}
	if loadMangaMeta(t.Context(), srv.db(), id+1) != nil {
		t.Error("an image without a row should read nil")
	}
}
