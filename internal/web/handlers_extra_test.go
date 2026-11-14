package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"html"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func TestMoveImage_RejectsAbsolutePath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "mv.png", 10, 10)

	form := url.Values{"folder": {"/etc/passwd"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/place", id), form)

	wantCode(t, w, http.StatusBadRequest)
	canonPath := canonicalPathOf(t, srv, id)
	if !strings.HasSuffix(canonPath, "mv.png") || strings.Contains(canonPath, "passwd") {
		t.Errorf("image moved to an unexpected path: %s", canonPath)
	}
}

func TestMoveImage_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "mv2.png", 10, 10)

	form := url.Values{"folder": {"../escape"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/place", id), form)

	wantCode(t, w, http.StatusBadRequest)
}

func TestMoveImage_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)

	form := url.Values{"folder": {"archive/2026"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/place", id), form)

	wantCode(t, w, http.StatusOK)
	if w.Header().Get("HX-Redirect") == "" {
		t.Error("HX move should set HX-Redirect")
	}
	cx := srv.active()
	var canonPath, folderPath string
	_ = cx.DB.Read.QueryRow(`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id).Scan(&canonPath, &folderPath)
	if folderPath != "archive/2026" {
		t.Errorf("folder_path = %q, want archive/2026", folderPath)
	}
	want := filepath.Join(cx.GalleryPath, "archive", "2026", "shot.png")
	if canonPath != want {
		t.Errorf("canonical_path = %q, want %q", canonPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
}

func TestPlaceImage_BothHalvesAndTheHalfLeftOut(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	pathOf := func(id int64) string {
		return strings.TrimPrefix(canonicalPathOf(t, srv, id), cx.GalleryPath+string(filepath.Separator))
	}

	both := seedImage(t, srv, "both.png", 10, 10)
	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/place", both), url.Values{
		"folder": {"archive/{gallery}"}, "name": {"{id}-cover"},
	})
	mustCode(t, w, http.StatusOK)
	if got, want := pathOf(both), fmt.Sprintf("archive/default/%d-cover.png", both); got != want {
		t.Errorf("both halves: path = %q, want %q", got, want)
	}

	kept := seedImage(t, srv, "kept.png", 11, 11)
	if err := placeInFolder(cx.DB, cx.GalleryPath, kept, "stay"); err != nil {
		t.Fatalf("place: %v", err)
	}
	w = postHTMXForm(t, srv, fmt.Sprintf("/images/%d/place", kept), url.Values{
		"name": {"renamed"},
	})
	mustCode(t, w, http.StatusOK)
	if got, want := pathOf(kept), "stay/renamed.png"; got != want {
		t.Errorf("name only: path = %q, want %q", got, want)
	}

	rooted := seedImage(t, srv, "rooted.png", 12, 12)
	if err := placeInFolder(cx.DB, cx.GalleryPath, rooted, "deep/down"); err != nil {
		t.Fatalf("place: %v", err)
	}
	w = postHTMXForm(t, srv, fmt.Sprintf("/images/%d/place", rooted), url.Values{
		"folder": {""}, "name": {""},
	})
	mustCode(t, w, http.StatusOK)
	if got, want := pathOf(rooted), "rooted.png"; got != want {
		t.Errorf("blank folder: path = %q, want %q", got, want)
	}
}

func TestBatchPlace_ScopeSearchNumbersInViewOrder(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	ids := make([]int64, 4)
	for i := range ids {
		ids[i] = seedImage(t, srv, fmt.Sprintf("src%d.png", i+1), 10+i, 10+i)
	}
	for i, id := range ids {
		mustExec(t, cx.DB.Write, `UPDATE images SET series = 'chapter', series_order = ? WHERE id = ?`, len(ids)-i, id)
	}
	cx.InvalidateCaches()

	w := postForm(t, srv, "/internal/batch-place", url.Values{
		"scope": {"search"}, "q": {"collection:any"},
		"sort": {"order"}, "order": {"asc"}, "name": {"page"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	for i, id := range ids {
		base := canonicalPathOf(t, srv, id)

		want := fmt.Sprintf("page%02d.png", len(ids)-i)
		if got := filepath.Base(base); got != want {
			t.Errorf("image %d (series_order %d): name = %q, want %q", id, len(ids)-i, got, want)
		}
	}
}

func TestBatchPlace_ScopeSearchNumbersInPinnedCollectionOrder(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	ids := make([]int64, 3)
	for i := range ids {
		ids[i] = seedImage(t, srv, fmt.Sprintf("arc%d.png", i+1), 10+i, 10+i)
		mustExec(t, cx.DB.Write, `UPDATE images SET series = 'home', series_order = ? WHERE id = ?`, i+1, ids[i])
		mustExec(t, cx.DB.Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?, 'arc', ?)`, ids[i], len(ids)-i)
	}
	cx.InvalidateCaches()

	w := postForm(t, srv, "/internal/batch-place", url.Values{
		"scope": {"search"}, "q": {"collection:arc"},
		"sort": {"order"}, "order": {"asc"}, "name": {"page"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	for i, id := range ids {
		path := canonicalPathOf(t, srv, id)
		if want := fmt.Sprintf("page%02d.png", len(ids)-i); filepath.Base(path) != want {
			t.Errorf("image %d (arc position %d): name = %q, want %q", id, len(ids)-i, filepath.Base(path), want)
		}
	}
}

func TestBatchPlaceCheck_CountsTheNamesTheRunWouldNumber(t *testing.T) {
	srv := newTestServer(t)
	form := func(name string) url.Values {
		v := url.Values{"scope": {"selection"}, "check": {"1"}, "name": {name}}
		for i := 0; i < 3; i++ {
			v.Add("ids", strconv.FormatInt(seedImage(t, srv, fmt.Sprintf("chk%d%s.png", i, name), 10+i, 10+i), 10))
		}
		return v
	}

	mustCode(t, postForm(t, srv, "/internal/batch-place", form("{year}")), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "2 would be numbered, their names are already taken.") {
		t.Errorf("summary = %q, want two collisions", got)
	}

	mustCode(t, postForm(t, srv, "/internal/batch-place", form("{name}")), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "no name is already taken") {
		t.Errorf("summary = %q, want a clean check", got)
	}
}

func TestBatchPlace_OneCollisionReadsInTheSingular(t *testing.T) {
	srv := newTestServer(t)
	form := url.Values{"scope": {"selection"}, "name": {"{year}"}}
	for i := range 2 {
		form.Add("ids", strconv.FormatInt(seedImage(t, srv, fmt.Sprintf("one%d.png", i), 10+i, 10+i), 10))
	}

	check := url.Values{"check": {"1"}}
	for k, v := range form {
		check[k] = v
	}
	mustCode(t, postForm(t, srv, "/internal/batch-place", check), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "1 would be numbered, its name is already taken.") {
		t.Errorf("check summary = %q", got)
	}

	mustCode(t, postForm(t, srv, "/internal/batch-place", form), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "1 had a taken name and was numbered.") {
		t.Errorf("run summary = %q", got)
	}
}

func TestBatchPlace_CountsTheFoldersItEmptied(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	ids := []int64{
		seedImage(t, srv, "one.png", 10, 10),
		seedImage(t, srv, "two.png", 11, 11),
	}
	form := url.Values{"scope": {"selection"}, "folder": {"src"}}
	for _, id := range ids {
		form.Add("ids", strconv.FormatInt(id, 10))
	}
	mustCode(t, postForm(t, srv, "/internal/batch-place", form), http.StatusAccepted)
	awaitJobsDrain(t, srv)

	form.Set("folder", "dst")
	mustCode(t, postForm(t, srv, "/internal/batch-place", form), http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if got := srv.jobs.Get().Summary; !strings.Contains(got, "1 folder is now empty.") {
		t.Errorf("summary = %q, want the emptied source counted", got)
	}
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "src")); err != nil {
		t.Errorf("the emptied folder was removed rather than reported: %v", err)
	}
}

func TestPlaceImage_InlineAnswersTheFinalName(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "taken.png", 10, 10)
	id := seedImage(t, srv, "other.png", 11, 11)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/place", id), url.Values{
		"name": {"taken"}, "inline": {"1"},
	})
	mustCode(t, w, http.StatusOK)
	if got := w.Body.String(); got != "taken01.png" {
		t.Errorf("inline body = %q, want taken01.png", got)
	}
}

func TestBatchPlaceCheck_CountsTakenNamesAndChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	ids := []int64{
		seedImage(t, srv, "one.png", 10, 10),
		seedImage(t, srv, "two.png", 11, 11),
		seedImage(t, srv, "three.png", 12, 12),
	}
	before := map[int64]string{}
	for _, id := range ids {
		before[id] = canonicalPathOf(t, srv, id)
	}

	check := func(name string) {
		form := url.Values{"scope": {"selection"}, "check": {"1"}, "name": {name}}
		for _, id := range ids {
			form.Add("ids", strconv.FormatInt(id, 10))
		}
		w := postForm(t, srv, "/internal/batch-place", form)
		mustCode(t, w, http.StatusAccepted)
		awaitJobsDrain(t, srv)
	}

	check("{gallery}")
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "2 would be numbered") {
		t.Errorf("summary = %q, want it to count the two taken names", got)
	}
	for _, id := range ids {
		if got := canonicalPathOf(t, srv, id); got != before[id] {
			t.Errorf("check moved image %d: %q -> %q", id, before[id], got)
		}
		if _, err := os.Stat(before[id]); err != nil {
			t.Errorf("file gone after a check: %v", err)
		}
	}

	check("{id}")
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "no name is already taken") {
		t.Errorf("summary = %q, want the clean verdict", got)
	}
}

func TestImageByHashRedirectsToCurrentID(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "hash.png", 10, 10)
	var sha string
	if err := srv.active().DB.Read.QueryRow(`SELECT sha256 FROM images WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatalf("read sha: %v", err)
	}

	w := do(srv, "GET", "/i/"+sha)
	mustCode(t, w, http.StatusFound)
	if got := w.Header().Get("Location"); got != fmt.Sprintf("/images/%d", id) {
		t.Errorf("Location = %q, want /images/%d", got, id)
	}

	w = do(srv, "GET", "/i/deadbeef")
	wantCode(t, w, http.StatusNotFound)
}

func TestImageByHashSwitchesToImageGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)

	if err := srv.switchGallery("stock"); err != nil {
		t.Fatalf("switch to stock: %v", err)
	}
	id := seedImage(t, srv, "elsewhere.png", 10, 10)
	var sha string
	if err := srv.active().DB.Read.QueryRow(`SELECT sha256 FROM images WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatalf("read sha: %v", err)
	}
	if err := srv.switchGallery("default"); err != nil {
		t.Fatalf("switch to default: %v", err)
	}

	w := do(srv, "GET", "/i/"+sha)

	mustCode(t, w, http.StatusFound)
	if got := w.Header().Get("Location"); got != fmt.Sprintf("/images/%d", id) {
		t.Errorf("Location = %q, want /images/%d", got, id)
	}
	if srv.activeGallery() != "stock" {
		t.Errorf("active gallery = %q, want stock (view should switch to the image's gallery)", srv.activeGallery())
	}
}

func TestMoveImage_FlashEscapesFolderName(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "esc.png", 10, 10)

	form := url.Values{"folder": {"<b>x</b>"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/place", id), form)

	mustCode(t, w, http.StatusOK)
	var trig struct {
		Flash struct {
			Text string `json:"text"`
		} `json:"action-flash"`
	}
	mustUnmarshal(t, []byte(w.Header().Get("HX-Trigger")), &trig)
	wantNotIn(t, trig.Flash.Text, "<b>")
	wantIn(t, trig.Flash.Text, "&lt;b&gt;")
}

func TestFoldersSuggest_CaseInsensitivePrefix(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustMkdirAll(t, filepath.Join(cx.GalleryPath, "Characters"), 0o755)
	seedImage(t, srv, "Characters/f.png", 9, 9)
	for _, prefix := range []string{"ch", "Ch", "CHAR"} {
		w := do(srv, "GET", "/internal/folders/suggest?prefix="+prefix)
		if w.Code != http.StatusOK {
			t.Fatalf("folders suggest prefix=%s: %d", prefix, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Characters") {
			t.Errorf("prefix=%s should surface Characters folder, got %s", prefix, w.Body.String())
		}
	}
}

func TestSourceSuggest_CaseInsensitivePrefix(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "src.png", 7, 7)
	if _, err := srv.active().DB.Write.Exec(
		`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'Pixiv', '', '')`, id,
	); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"pix", "Pix", "PIXIV"} {
		w := do(srv, "GET", "/internal/source/suggest?prefix="+prefix)
		if w.Code != http.StatusOK {
			t.Fatalf("source suggest prefix=%s: %d", prefix, w.Code)
		}
		if !strings.Contains(w.Body.String(), `data-series="Pixiv"`) {
			t.Errorf("prefix=%s should surface Pixiv source, got %s", prefix, w.Body.String())
		}
	}
}

func TestFoldersSuggest_PrefixFilter(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	for i, folder := range []string{"2024/jan", "2024/feb", "2025/mar"} {
		sub := filepath.Join(cx.GalleryPath, folder)
		mustMkdirAll(t, sub, 0o755)
		name := strings.ReplaceAll(folder, "/", "_") + ".png"
		p := filepath.Join(sub, name)
		img := image.NewRGBA(image.Rect(0, 0, 12+i*3, 12+i))
		for px := 0; px < len(img.Pix); px += 4 {
			img.Pix[px] = byte(i * 50)
		}
		f, _ := os.Create(p)
		_ = png.Encode(f, img)
		_ = f.Close()
		if _, _, err := gallery.Ingest(cx.Handle, p, ""); err != nil {
			t.Fatal(err)
		}
	}

	body := mustGetBody(t, srv, "/internal/folders/suggest?prefix=2024")
	if !strings.Contains(body, "2024/jan") || !strings.Contains(body, "2024/feb") {
		t.Errorf("expected both 2024 folders in response, got %s", body)
	}
	wantNotIn(t, body, "2025/mar")
}

func TestSidebarFolderLink_SinglePercentEncoded(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustMkdirAll(t, filepath.Join(cx.GalleryPath, "api-test"), 0o755)
	seedImage(t, srv, "api-test/f.png", 8, 8)

	w := do(srv, "GET", "/internal/sidebar-browse", folderTreeOpen)
	body := w.Body.String()

	wantIn(t, body, `href="/?q=folder%3A%22api-test%22"`)
	if strings.Contains(body, "folder%253A") || strings.Contains(body, "%2522") {
		t.Errorf("folder href is double-encoded\nbody: %s", body)
	}

	w2 := do(srv, "GET", `/?q=folder%3A%22api-test%22`)
	mustCode(t, w2, http.StatusOK)
	if !strings.Contains(w2.Body.String(), `<span id="result-count" class="result-count">1</span>`) {
		t.Errorf("folder:\"api-test\" should return 1 match, body excerpt:\n%s", resultCountSlice(t, w2.Body.String()))
	}
}

func TestSidebarFolderLink_QuotesAFolderNameWithAQuote(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustMkdirAll(t, filepath.Join(cx.GalleryPath, `my "best" pics`), 0o755)
	seedImage(t, srv, `my "best" pics/f.png`, 8, 8)
	q := `folder:"my \"best\" pics"`

	body := html.UnescapeString(do(srv, "GET", "/internal/sidebar-browse?q="+url.QueryEscape(q)).Body.String())

	wantIn(t, body, `href="/?q=`+url.QueryEscape(q)+`" class="folder-link active"`)
	wantIn(t, body, `href="/?q=`+url.QueryEscape(`folderonly:"my \"best\" pics"`)+`"`)
	w := do(srv, "GET", "/?q="+url.QueryEscape(q))
	mustCode(t, w, http.StatusOK)
	wantIn(t, resultCountSlice(t, w.Body.String()), `<span id="result-count" class="result-count">1</span>`)
}

var folderTreeOpen = [2]string{"Cookie", "monbooru_folders=__tree__"}

func mkFolder(t *testing.T, srv *Server, rel string) string {
	t.Helper()
	mustMkdirAll(t, filepath.Join(srv.active().GalleryPath, rel), 0o755)
	return rel
}

func seedFolderRows(t *testing.T, srv *Server, n int) {
	t.Helper()
	mustExec(t, srv.db().Write, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?)
		INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		SELECT 'sha-' || i, printf('f%03d/x.png', i), printf('f%03d', i), 'png', 5, datetime('now') FROM n`, n)
	srv.active().InvalidateCaches()
}

func TestSidebarFolders_AClosedSectionSendsNoRows(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, filepath.Join(mkFolder(t, srv, "2024/jan"), "a.png"), 8, 8)

	for _, path := range []string{"/internal/sidebar-browse", "/internal/sidebar?ids=", "/?q="} {
		body := do(srv, "GET", path, [2]string{"HX-Request", "true"}, [2]string{"HX-Target", "gallery-grid"}).Body.String()
		wantIn(t, body, `id="folder-tree-section"`)
		wantIn(t, body, `hx-get="/internal/folders/children?path=&q="`)
		wantNotIn(t, body, `class="folder-link`)
	}
}

func TestSidebarFolders_RenderOnlyTheOpenLevels(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, filepath.Join(mkFolder(t, srv, "a/b/c"), "x.png"), 8, 8)

	body := do(srv, "GET", "/internal/sidebar-browse", [2]string{"Cookie", "monbooru_folders=__tree__,a%2Fb,gone"}).Body.String()
	wantIn(t, body, `title="a/">a/</a>`)
	wantIn(t, body, `hx-get="/internal/folders/children?path=a&q="`)
	wantNotIn(t, body, `title="b/"`)

	body = do(srv, "GET", "/internal/sidebar-browse", [2]string{"Cookie", "monbooru_folders=__tree__,a"}).Body.String()
	wantIn(t, body, `title="b/">b/</a>`)
	wantNotIn(t, body, `title="c/"`)

	body = do(srv, "GET", "/internal/sidebar-browse?q="+url.QueryEscape("cat folder:a/b")).Body.String()
	wantIn(t, body, `title="c/">c/</a>`)
}

func TestFolderChildren_CapsALevelAndPagesTheRest(t *testing.T) {
	srv := newTestServer(t)
	seedFolderRows(t, srv, 502)

	body := mustGetBody(t, srv, "/internal/folders/children?path=")
	if n := strings.Count(body, `class="folder-link`); n != 1+folderLevelRows {
		t.Errorf("first page has %d folder links, want the root and %d rows", n, folderLevelRows)
	}
	wantIn(t, body, `offset=500`)
	wantIn(t, body, `title="2 more folders">[more]`)
	wantNotIn(t, body, `title="f500/"`)

	body = mustGetBody(t, srv, "/internal/folders/children?path=&offset=500")
	if n := strings.Count(body, `class="folder-link`); n != 2 {
		t.Errorf("second page has %d folder links, want 2", n)
	}
	wantIn(t, body, `title="f501/"`)
	wantNotIn(t, body, `[more]`)
}

func TestFolderChildren_KeepsTheSearchedFolderPastTheCap(t *testing.T) {
	srv := newTestServer(t)
	seedFolderRows(t, srv, 502)
	q := url.QueryEscape("folder:f501")

	body := mustGetBody(t, srv, "/internal/folders/children?path=&q="+q)
	wantIn(t, body, `class="folder-link active"`)
	wantIn(t, body, `title="f501/"`)
	wantIn(t, body, `title="1 more folder">[more]`)

	body = mustGetBody(t, srv, "/internal/folders/children?path=&offset=500&q="+q)
	wantIn(t, body, `title="f500/"`)
	wantNotIn(t, body, `title="f501/"`)
	wantNotIn(t, body, `[more]`)
}

func TestFolderChildren_KeepsTheSearchedFolderInAnotherCase(t *testing.T) {
	srv := newTestServer(t)
	mustExec(t, srv.db().Write, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 502)
		INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		SELECT 'sha-' || i, printf('F%03d/x.png', i), printf('F%03d', i), 'png', 5, datetime('now') FROM n`)
	srv.active().InvalidateCaches()

	body := mustGetBody(t, srv, "/internal/folders/children?path=&q="+url.QueryEscape("folder:f501"))
	for _, want := range []string{`title="F501/"`, `title="1 more folder">[more]`} {
		if !strings.Contains(body, want) {
			t.Errorf("first page lacks %s", want)
		}
	}
}

func TestSidebarFolders_OpenTheSearchedPathBeforeTheOpenLevels(t *testing.T) {
	srv := newTestServer(t)
	mustExec(t, srv.db().Write, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 4 * 501)
		INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		SELECT 'sha-' || i, printf('%s/c%03d/x.png', char(97 + i / 501), i % 501), printf('%s/c%03d', char(97 + i / 501), i % 501), 'png', 5, datetime('now') FROM n`)
	mustExec(t, srv.db().Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		VALUES ('sha-zy', 'z/y/w/x.png', 'z/y/w', 'png', 5, datetime('now'))`)
	srv.active().InvalidateCaches()

	body := do(srv, "GET", "/internal/sidebar-browse?q="+url.QueryEscape("folder:z/y"), [2]string{"Cookie", "monbooru_folders=__tree__,a,b,c,d"}).Body.String()
	for _, want := range []string{`title="y/">y/</a>`, `title="w/">w/</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("tree lacks %s", want)
		}
	}
	if i, j := strings.Index(body, `title="d/"`), strings.Index(body, `title="z/"`); i < 0 || j < i {
		t.Errorf("z/ at %d, d/ at %d: want z/ after d/", j, i)
	}
}

func TestFolderChildren_LeavesOutWhatTheCeilingHides(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	seedImage(t, srv, filepath.Join(mkFolder(t, srv, "safe"), "a.png"), 8, 8)
	hidden := seedImage(t, srv, filepath.Join(mkFolder(t, srv, "nsfw/sub"), "b.png"), 9, 9)
	mustAddTag(t, cx.TagSvc, hidden, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil)
	cx.InvalidateCaches()
	ceiling := [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"}

	body := do(srv, "GET", "/internal/folders/children?path=", ceiling).Body.String()
	wantIn(t, body, `title="safe/"`)
	wantNotIn(t, body, `title="nsfw/"`)

	w := do(srv, "GET", "/internal/folders/children?path=nsfw", ceiling)
	mustCode(t, w, http.StatusOK)
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("a hidden folder's level answered rows: %s", w.Body.String())
	}
}

func TestFolderCookieKeys_ReadsWhatMainJSWrites(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{"", nil},
		{"__tree__,a%20b,c%2Cd", []string{"__tree__", "a b", "c,d"}},
		{"a%2520b%2Cc%252Cd", []string{"a b", "c,d"}},
		{"%E6%97%A5%E6%9C%AC", []string{"日本"}},
		{"bad%zz,a", nil},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: folderCookieName, Value: tc.value})
		got := slices.Sorted(maps.Keys(folderCookieKeys(r)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestDetailCollectionLink_SinglePercentEncoded(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "s.png", 9, 9)
	mustSetHome(t, srv.db(), id, "touhou series", nil)

	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	body := w.Body.String()

	want := []string{
		`href="/?q=collection%3A%22touhou+series%22"`,
		`href="/?q=collection%3A%22touhou&#43;series%22"`,
	}
	if !slices.ContainsFunc(want, func(w string) bool { return strings.Contains(body, w) }) {
		t.Errorf("collection href not single-encoded; want one of %v\nbody slice: %s", want, seriesLinkSlice(t, body))
	}
	if strings.Contains(body, "collection%253A") {
		t.Errorf("collection href is double-encoded\nbody slice: %s", seriesLinkSlice(t, body))
	}

	w2 := do(srv, "GET", `/?q=collection%3A%22touhou+series%22`)
	if !strings.Contains(w2.Body.String(), `<span id="result-count" class="result-count">1</span>`) {
		t.Errorf("collection:\"touhou series\" should return 1 match\n%s", resultCountSlice(t, w2.Body.String()))
	}
}

func resultCountSlice(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `id="gallery-status"`)
	if idx < 0 {
		return body[:min(len(body), 200)]
	}
	end := strings.Index(body[idx:], "</div>")
	if end < 0 {
		return body[idx:min(len(body), idx+200)]
	}
	return body[idx : idx+end+6]
}

func seriesLinkSlice(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `Series</th>`)
	if idx < 0 {
		return body[:min(len(body), 200)]
	}
	return body[idx:min(idx+400, len(body))]
}

func TestSearchSuggest_SystemCheatSheet(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.tagSvc().CreateCategory("custom_pal", "#aabbcc"); err != nil {
		t.Fatalf("seed custom category: %v", err)
	}

	for _, tc := range []struct {
		name, q      string
		want, absent []string
	}{
		{
			name: "top level",
			q:    "system:",
			want: []string{
				`data-tag-name="fav:"`,
				`data-tag-name="source:"`,
				`data-tag-name="date:"`,
				`data-tag-name="rating:"`,
				`data-tag-name="inbox:"`,
				`<span class="suggest-description">favorite images</span>`,
				`<span class="suggest-description">image width</span>`,
				`<span class="suggest-description">ingestion date</span>`,

				`<span class="suggest-description">tag category</span>`,
			},

			absent: []string{`class="suggest-count"`},
		},
		{
			name:   "top level, prefix filtered",
			q:      "system:fa",
			want:   []string{`data-tag-name="fav:"`},
			absent: []string{`data-tag-name="date:"`},
		},
		{
			name: "date operators",
			q:    "system:date:",
			want: []string{
				`data-tag-name="date:&gt;"`,
				`data-tag-name="date:&lt;"`,
				`data-tag-name="date:..`,
				`data-tag-name="date:&gt;="`,
				`data-tag-name="date:&lt;="`,
				`<span class="suggest-description">after</span>`,
				`<span class="suggest-description">before</span>`,
				`<span class="suggest-description">range</span>`,
			},
		},
		{
			name: "ai values",
			q:    "system:ai:",
			want: []string{
				`data-tag-name="ai:a1111"`,
				`data-tag-name="ai:comfyui"`,
				`data-tag-name="ai:none"`,
				`<span class="suggest-description">A1111 / Forge</span>`,
				`<span class="suggest-description">ComfyUI</span>`,
				`<span class="suggest-description">no metadata</span>`,
			},
		},
		{
			name: "categories",
			q:    "system:cat:",
			want: []string{`data-tag-name="cat:character"`, `data-tag-name="cat:custom_pal"`},
		},
		{
			name: "tagcount operators then categories",
			q:    "system:tagcount:",
			want: []string{
				`data-tag-name="tagcount:&gt;="`,
				`data-tag-name="tagcount:character="`,
				`<span class="suggest-description">number of character tags</span>`,
			},
		},
		{
			name:   "tagcount narrowed to a category",
			q:      "tagcount:ch",
			want:   []string{`data-tag-name="tagcount:character="`},
			absent: []string{`data-tag-name="tagcount:&gt;="`, `data-tag-name="tagcount:artist="`},
		},
		{
			name:   "rating values carry no description",
			q:      "system:rating:",
			absent: []string{`class="suggest-description"`},
		},
		{
			name: "bare date key",
			q:    "date:",
			want: []string{`data-tag-name="date:&gt;"`, `data-tag-name="date:&lt;"`, `data-tag-name="date:..`},
		},
		{
			name: "bare cat key",
			q:    "cat:",
			want: []string{`data-tag-name="cat:character"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mustGetBody(t, srv, "/internal/search/suggest?q="+url.QueryEscape(tc.q))
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("%q dropdown missing %q\nbody: %s", tc.q, want, body)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(body, absent) {
					t.Errorf("%q dropdown must not carry %q\nbody: %s", tc.q, absent, body)
				}
			}
		})
	}
}

func TestSearchSuggest_System_Level2_Source(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	var imgID int64
	if err := cx.DB.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES ('srcsug', '/g/srcsug.png', 'png', 10) RETURNING id`,
	).Scan(&imgID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, cx.DB.Write, `INSERT INTO image_sources (image_id, site, post_id) VALUES (?, 'gelbooru', '1')`, imgID)
	w := do(srv, "GET", "/internal/search/suggest?q=system:source:")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="source:none"`,
		`data-tag-name="source:any"`,
		`data-tag-name="source:&#34;gelbooru&#34;"`,
	} {
		wantIn(t, body, want)
	}
}

func TestSearchSuggest_System_TopLevel_IncludesCategories(t *testing.T) {
	srv := newTestServer(t)
	w := do(srv, "GET", "/internal/search/suggest?q=system:")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="character:"`,
		`data-tag-name="artist:"`,
		`data-tag-name="copyright:"`,
		`data-tag-name="general:"`,
	} {
		wantIn(t, body, want)
	}

	if got := strings.Count(body, `data-tag-name="rating:"`); got != 1 {
		t.Errorf("expected exactly one rating: row, got %d\nbody: %s", got, body)
	}
}

func TestSearchSuggest_BareFilterKey_SurfacesValueHint(t *testing.T) {
	srv := newTestServer(t)
	for _, key := range []string{"fav", "inbox"} {
		t.Run(key, func(t *testing.T) {
			body := mustGetBody(t, srv, "/internal/search/suggest?q="+key+":")
			wantIn(t, body, `data-tag-name="`+key+`:true"`)
			wantIn(t, body, `data-tag-name="`+key+`:false"`)
		})
	}
}

func TestSearchSuggest_System_Level2_RealCategoryDrillIn(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	charCatID := categoryIDByName(t, srv, "character")
	tag := mustTag(t, srv.tagSvc(), "bocchi_the_rock", charCatID)
	mustAddTag(t, srv.tagSvc(), id, tag.ID, false, nil)

	w := do(srv, "GET", "/internal/search/suggest?q=system:character:")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, `data-tag-name="character:bocchi_the_rock"`)

	wantIn(t, body, `class="suggest-count"`)
}

func TestSavedSearch_RoundtripsSortAndSeed(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{
		"name": {"rand_42"}, "query": {"bulk"},
		"sort": {"random"}, "order": {"asc"}, "seed": {"42"},
	}
	w := postHTMXForm(t, srv, "/search/saved", form)
	mustCode(t, w, http.StatusOK)

	var got struct {
		Sort, Order, Seed string
	}
	if err := srv.db().Read.QueryRow(
		`SELECT sort, sort_order, seed FROM saved_searches WHERE name='rand_42'`,
	).Scan(&got.Sort, &got.Order, &got.Seed); err != nil {
		t.Fatalf("saved search columns missing: %v", err)
	}
	if got.Sort != "random" || got.Order != "asc" || got.Seed != "42" {
		t.Errorf("sort/order/seed = %+v, want {random asc 42}", got)
	}

	ss := models.SavedSearch{Query: "bulk", Sort: "random", Order: "asc", Seed: "42"}
	want := "/?q=bulk&sort=random&order=asc&seed=42"
	if got := ss.HRef(); got != want {
		t.Errorf("HRef = %q, want %q", got, want)
	}
}

func TestSavedSearch_CreateAndDelete(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"name": {"my_cats"}, "query": {"cat"}}
	w := postHTMXForm(t, srv, "/search/saved", form)
	mustCode(t, w, http.StatusOK)

	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM saved_searches WHERE name='my_cats'`).Scan(&id); err != nil {
		t.Fatalf("saved search not persisted: %v", err)
	}

	w2 := deleteCSRF(t, srv, fmt.Sprintf("/search/saved/%d", id))
	wantCode(t, w2, http.StatusOK)
	count := countRows(t, srv.db(), "saved_searches")
	if count != 0 {
		t.Errorf("saved_searches should be empty after delete, got %d", count)
	}
}

func TestCreateCategory_Post(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"name": {"mood"}, "color": {"#abcdef"}}
	w := postHTMXForm(t, srv, "/tags/categories", form)
	mustCode(t, w, http.StatusNoContent)
	if loc := w.Header().Get("HX-Redirect"); loc != "/categories" {
		t.Errorf("HX-Redirect = %q, want /categories", loc)
	}
	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name='mood'`).Scan(&id); err != nil {
		t.Fatalf("category not persisted: %v", err)
	}
}

func TestJobDismissPost_ClearsDoneSummary(t *testing.T) {
	srv := newTestServer(t)
	mustStartJob(t, srv.jobs, "sync")
	srv.jobs.Complete("done")

	w := postForm(t, srv, "/internal/job/dismiss", url.Values{})
	wantCode(t, w, http.StatusNoContent)
	if state := srv.jobs.Get(); state != nil {
		t.Errorf("state should be nil after dismiss, got %+v", state)
	}
}

func TestJobCancelPost_CancelsRunning(t *testing.T) {
	srv := newTestServer(t)
	mustStartJob(t, srv.jobs, "delete")
	ctx := srv.jobs.Context()
	w := postForm(t, srv, "/internal/job/cancel", url.Values{})
	wantCode(t, w, http.StatusNoContent)
	if ctx.Err() == nil {
		t.Error("Cancel should have fired the job's context")
	}
	srv.jobs.Complete("cancelled test")
}

func TestReExtract_ReplacesExistingMetadata(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	id := seedImage(t, srv, "reext.png", 10, 10)
	mustExec(t, cx.DB.Write, `INSERT INTO sd_metadata (image_id, prompt, raw_params, generation_hash) VALUES (?, 'stale_prompt', 'stale_params', 'stale_hash12')`, id)
	mustExec(t, cx.DB.Write, `UPDATE images SET source_type = 'a1111' WHERE id = ?`, id)

	w := postForm(t, srv, "/settings/maintenance/re-extract-metadata", url.Values{})
	mustCode(t, w, http.StatusOK)

	awaitJobsDrain(t, srv)
	count := countRows(t, cx.DB, "sd_metadata WHERE image_id = ?", id)
	if count != 0 {
		t.Errorf("re-extract should have cleared the stale sd_metadata row for a plain PNG, count = %d", count)
	}
	var sourceType string
	_ = cx.DB.Read.QueryRow(`SELECT source_type FROM images WHERE id = ?`, id).Scan(&sourceType)
	if sourceType != "none" {
		t.Errorf("source_type after re-extract = %q, want 'none'", sourceType)
	}
}

func TestReExtract_RewritesASeedTheHashLeavesOut(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	path := filepath.Join(cx.GalleryPath, "seeded.png")
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	body := []byte("tEXtparameters\x00a lighthouse\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 42, Model: m")
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(body)-4))
	chunk.Write(body)
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(body))
	const afterIHDR = 8 + 4 + 4 + 13 + 4
	mustWriteFile(t, path, slices.Concat(raw.Bytes()[:afterIHDR], chunk.Bytes(), raw.Bytes()[afterIHDR:]), 0o644)
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, path, "")
	mustExec(t, cx.DB.Write, `UPDATE sd_metadata SET seed = NULL WHERE image_id = ?`, rec.ID)

	w := postForm(t, srv, "/settings/maintenance/re-extract-metadata", url.Values{})
	mustCode(t, w, http.StatusOK)
	awaitJobsDrain(t, srv)

	var seed sql.NullInt64
	_ = cx.DB.Read.QueryRow(`SELECT seed FROM sd_metadata WHERE image_id = ?`, rec.ID).Scan(&seed)
	if !seed.Valid || seed.Int64 != 42 {
		t.Errorf("seed after re-extract = %v, want 42", seed)
	}
}

func TestReExtract_WithdrawsTheAIGeneratedTag(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	id := seedImage(t, srv, "reext-ai.png", 10, 10)
	mustExec(t, cx.DB.Write, `UPDATE images SET source_type = 'a1111' WHERE id = ?`, id)
	if err := gallery.ApplyMetaTags(cx.Handle, id); err != nil {
		t.Fatal(err)
	}
	if !imageHasTag(t, cx.DB, id, "ai-generated") {
		t.Fatal("seed did not derive ai-generated")
	}

	w := postForm(t, srv, "/settings/maintenance/re-extract-metadata", url.Values{})
	mustCode(t, w, http.StatusOK)
	awaitJobsDrain(t, srv)

	if imageHasTag(t, cx.DB, id, "ai-generated") {
		t.Error("the plain PNG kept ai-generated after source_type fell back to none")
	}

	if !imageHasTag(t, cx.DB, id, "lowres") {
		t.Error("the re-extract dropped a tag the file still implies")
	}
}

func TestReExtract_LeavesAnUnchangedVideoAlone(t *testing.T) {
	if !ffmpegOnPath() {
		t.Skip("ffmpeg not available")
	}
	srv := newTestServer(t)
	cx := srv.active()
	src := filepath.Join(cx.GalleryPath, "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "color=c=black:s=32x18:d=0.5:r=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "--", src).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
	}
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, src, "")
	if rec.DurationSec == nil {
		t.Skip("ffprobe could not read the fixture's duration")
	}

	w := postForm(t, srv, "/settings/maintenance/re-extract-metadata", url.Values{})
	mustCode(t, w, http.StatusOK)
	awaitJobsDrain(t, srv)

	if st := srv.jobs.Get(); st == nil || !strings.Contains(st.Summary, "(0 updated)") {
		t.Errorf("re-extract summary = %+v, want the unchanged video not counted as updated", st)
	}
}

func TestReExtract_ReadsAVideosComfyUITags(t *testing.T) {
	if !ffmpegOnPath() {
		t.Skip("ffmpeg not available")
	}
	srv := newTestServer(t)
	cx := srv.active()
	src := filepath.Join(cx.GalleryPath, "clip.webm")
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "color=c=black:s=32x18:d=0.5:r=10", "-c:v", "libvpx-vp9",
		"-metadata", `prompt={"1":{"inputs":{"text":"a lighthouse"},"class_type":"CLIPTextEncode"}}`,
		"--", src).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
	}
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, src, "")
	mustExec(t, cx.DB.Write, `DELETE FROM comfyui_metadata WHERE image_id = ?`, rec.ID)
	mustExec(t, cx.DB.Write, `UPDATE images SET source_type = 'none' WHERE id = ?`, rec.ID)

	reExtract(t, srv)

	var sourceType, prompt string
	_ = cx.DB.Read.QueryRow(`SELECT i.source_type, COALESCE(cm.prompt, '') FROM images i
		LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id WHERE i.id = ?`, rec.ID).Scan(&sourceType, &prompt)
	if sourceType != models.SourceTypeComfyUI || prompt != "a lighthouse" {
		t.Errorf("after re-extract: source_type %q, prompt %q, want comfyui and the tag's prompt", sourceType, prompt)
	}
	if !imageHasTag(t, cx.DB, rec.ID, "ai-generated") {
		t.Error("the re-extracted video did not take ai-generated")
	}
}

func seedComicInfoManga(t *testing.T, srv *Server, name string) int64 {
	t.Helper()
	return seedMangaEntries(t, srv, name, []mangaEntry{
		{"page_001.png", tinyPNG(t, 8, 8, color.RGBA{1, 2, 3, 255})},
		{"ComicInfo.xml", []byte(`<?xml version="1.0"?><ComicInfo><Title>Real Title</Title><Series>S</Series><Count>3</Count><CommunityRating>4.5</CommunityRating></ComicInfo>`)},
	})
}

func mangaTitle(m *models.MangaMetadata) string {
	if m == nil {
		return "no row"
	}
	return fmt.Sprintf("title %q", m.Title)
}

func reExtract(t *testing.T, srv *Server) {
	t.Helper()
	mustCode(t, postForm(t, srv, "/settings/maintenance/re-extract-metadata", url.Values{}), http.StatusOK)
	awaitJobsDrain(t, srv)
}

func TestReExtract_RereadsComicInfo(t *testing.T) {
	for name, damage := range map[string]string{
		"stale title": `UPDATE manga_metadata SET title = 'stale' WHERE image_id = ?`,
		"deleted row": `DELETE FROM manga_metadata WHERE image_id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t)
			cx := srv.active()
			id := seedComicInfoManga(t, srv, "book.cbz")
			mustExec(t, cx.DB.Write, damage, id)

			reExtract(t, srv)
			if m := loadMangaMeta(context.Background(), cx.DB, id); m == nil || m.Title != "Real Title" {
				t.Errorf("manga_metadata after re-extract = %v, want the archive's title", mangaTitle(m))
			}
		})
	}
}

func TestReExtract_LeavesAnUnchangedComicInfoAlone(t *testing.T) {
	srv := newTestServer(t)
	seedComicInfoManga(t, srv, "book.cbz")
	seedManga(t, srv, "bare.cbz", [][]byte{tinyPNG(t, 8, 8, color.RGBA{9, 2, 3, 255})})

	reExtract(t, srv)
	if st := srv.jobs.Get(); st == nil || !strings.Contains(st.Summary, "(0 updated)") {
		t.Errorf("re-extract summary = %+v, want the unchanged archives not counted as updated", st)
	}
}

func TestReExtract_KeepsComicInfoAnArchiveNoLongerOpensFor(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	id := seedComicInfoManga(t, srv, "book.cbz")
	mustExec(t, cx.DB.Write, `UPDATE manga_metadata SET title = 'kept' WHERE image_id = ?`, id)
	mustWriteFile(t, filepath.Join(cx.GalleryPath, "book.cbz"), []byte("not a zip"), 0o644)

	reExtract(t, srv)
	if m := loadMangaMeta(context.Background(), cx.DB, id); m == nil || m.Title != "kept" {
		t.Errorf("manga_metadata after re-extract = %v, want the stored row kept", mangaTitle(m))
	}
}

func imageHasTag(t *testing.T, database *db.DB, imageID int64, name string) bool {
	t.Helper()
	return countRows(t, database, `image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = ?`, imageID, name) > 0
}

func TestGallerySwitch_RejectedWhileJobRunning(t *testing.T) {
	srv := newMultiGalleryServer(t)
	mustStartJob(t, srv.jobs, "sync")
	defer srv.jobs.Complete("test")

	form := url.Values{"name": {"stock"}}
	w := postForm(t, srv, "/internal/gallery/switch", form)

	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "job is running")
	if srv.activeGallery() != "default" {
		t.Errorf("the active gallery should not have changed, got %q", srv.activeGallery())
	}
}

func TestGallerySwitch_UnknownGalleryRejected(t *testing.T) {
	srv := newMultiGalleryServer(t)
	form := url.Values{"name": {"does-not-exist"}}
	w := postForm(t, srv, "/internal/gallery/switch", form)

	if w.Code < 400 || w.Code >= 500 {
		t.Errorf("unknown gallery switch expected 4xx, got %d: %s", w.Code, w.Body.String())
	}
}

func imageTagCategory(t *testing.T, srv *Server, id int64, want string) string {
	t.Helper()
	cx := srv.active()
	var cat string
	err := cx.DB.Read.QueryRow(
		`SELECT tc.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.name = ?`, id, want).Scan(&cat)
	if err != nil {
		return ""
	}
	return cat
}

func TestAddTagToImage_PartialDup_SurfacesDupList(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "partial_dup.png", 10, 10)

	post := func(tag string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"tag": {tag}}
		return postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), form)
	}

	if w := post("existing_one"); w.Code != http.StatusOK {
		t.Fatalf("seed add: expected 200, got %d", w.Code)
	}

	w := post("brand_new existing_one")
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "already on image: existing_one")
}

func TestAddTagToImage_FlashNamesTheStoredTagsOnce(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "stored_names.png", 10, 10)
	genID := srv.active().GeneralCategoryID
	cat := mustTag(t, srv.tagSvc(), "cat", genID)
	mustCreateAlias(t, srv.tagSvc(), "kitty", genID, cat.ID)
	flash := regexp.MustCompile(`class="flash [^"]*"[^>]*>([^<]*)</div>`)
	post := func(tag, want string) {
		t.Helper()
		w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {tag}})
		mustCode(t, w, http.StatusOK)
		m := flash.FindStringSubmatch(w.Body.String())
		if m == nil || m[1] != want {
			t.Errorf("%q: flash = %q, want %q", tag, m, want)
		}
	}
	post("cat outdoors", "added: cat, outdoors")
	post("kitty", "tag already on image: cat")
	post("outdoors outdoors", "tag already on image: outdoors")
	post("MixedCase  Spaces spaces", "added: mixedcase, spaces")
}

func TestAddTagToImage_RefreshesTheGalleryCount(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "fresh_count.png", 10, 10)
	seedImage(t, srv, "fresh_other.png", 12, 12)
	mustTag(t, srv.tagSvc(), "fresh_tag", srv.active().GeneralCategoryID)
	if got := galleryResultCount(t, srv, "-fresh_tag"); got != "2" {
		t.Fatalf("-fresh_tag = %s before the add, want 2", got)
	}

	mustCode(t, postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"fresh_tag"}}), http.StatusOK)

	if got := galleryResultCount(t, srv, "-fresh_tag"); got != "1" {
		t.Errorf("-fresh_tag = %s after the add, want 1", got)
	}
}

func TestTagEdit_KeepsThePagesDeleteContext(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ctx_keep.png", 10, 10)
	page := fmt.Sprintf("http://localhost/images/%d?back_q=cat&back_sort=newest&back_order=desc&back_page=2&ref=7", id)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"ctx_tag"}},
		[2]string{"HX-Request", "true"}, [2]string{"HX-Current-URL", page})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantDeleteQuery(t, body, id, map[string]string{"back_q": "cat", "back_sort": "newest", "back_order": "desc", "back_page": "2", "ref": "7"})
	wantIn(t, body, `hx-get="/internal/sidebar-browse?q=cat"`)

	selfRef := fmt.Sprintf("http://localhost/images/%d?back_q=dog&ref=%d", id, id)
	w = postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"ctx_tag2"}},
		[2]string{"HX-Request", "true"}, [2]string{"HX-Current-URL", selfRef})
	mustCode(t, w, http.StatusOK)
	wantDeleteQuery(t, w.Body.String(), id, map[string]string{"back_q": "dog", "back_sort": "", "back_order": "", "back_page": "", "ref": ""})
}

func wantDeleteQuery(t *testing.T, body string, id int64, want map[string]string) {
	t.Helper()
	m := regexp.MustCompile(fmt.Sprintf(`hx-delete="(/images/%d(?:\?[^"]*)?)"`, id)).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no hx-delete for image %d in %q", id, body)
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if got := u.Query().Get(k); got != v {
			t.Errorf("hx-delete %s = %q, want %q (%s)", k, got, v, m[1])
		}
	}
}

func TestAddTagToImage_PartialReject_ShowsWarnFlash(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "partial_reject.png", 10, 10)

	form := url.Values{"tag": {"general: brand_new artist: other_new"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), form)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, `class="flash flash-warn"`)
	wantIn(t, body, "added: brand_new, other_new; rejected: general:")
	wantNotIn(t, body, `class="flash flash-err"`)
	wantNotIn(t, body, `class="flash flash-ok"`)
	wantIn(t, body, `i.value="general: artist:";`)
}

func TestAddTagToImage_ColonFallbackLiteral(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "colon_literal.png", 10, 10)

	form := url.Values{"tag": {"nier:automata"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), form)

	mustCode(t, w, http.StatusOK)
	if cat := imageTagCategory(t, srv, id, "nier:automata"); cat != "general" {
		t.Errorf("nier:automata category = %q, want general", cat)
	}
}

func TestGalleryHandler_RandomSeedFitsInt32(t *testing.T) {
	srv := newTestServer(t)
	w := do(srv, "GET", "/?sort=random")

	mustCode(t, w, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	seedStr := parsed.Query().Get("seed")
	if seedStr == "" {
		t.Fatalf("Location missing seed: %q", loc)
	}
	seed, err := strconv.ParseInt(seedStr, 10, 64)
	if err != nil {
		t.Fatalf("parse seed %q: %v", seedStr, err)
	}
	if seed <= 0 || seed > (1<<32)-1 {
		t.Errorf("seed %d outside [1, 2^32-1]; multiplication can overflow int64", seed)
	}
	if seed&1 == 0 {
		t.Errorf("seed %d is even; (id*seed) mod 2^31 is not a permutation", seed)
	}
}

func TestGalleryHandler_NonPositivePageRedirects(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "p0.png", 10, 10)

	for _, raw := range []string{"0", "-1"} {
		w := do(srv, "GET", "/?page="+raw)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("page=%s expected 303 redirect, got %d", raw, w.Code)
		}
		loc := w.Header().Get("Location")
		parsed, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse Location %q: %v", loc, err)
		}
		if got := parsed.Query().Get("page"); got != "1" {
			t.Errorf("page=%s redirected to page=%q, want 1", raw, got)
		}
	}

	w := do(srv, "GET", "/?q=nothingmatchesthis&page=0")
	mustCode(t, w, http.StatusSeeOther)
	parsed, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("page"); got != "1" {
		t.Errorf("empty result page=0 redirected to page=%q, want 1", got)
	}
}

func TestCreateSavedSearch_RejectsDuplicateName(t *testing.T) {
	srv := newTestServer(t)

	post := func(name, query string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"name": {name}, "query": {query}}
		return postHTMXForm(t, srv, "/search/saved", form)
	}

	if w := post("girls", "1girl"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "flash-ok") {
		t.Fatalf("first save: code=%d body=%q", w.Code, w.Body.String())
	}

	w := post("girls", "blue_eyes")
	wantIn(t, w.Body.String(), "flash-err")

	cx := srv.active()
	var stored string
	if err := cx.DB.Read.QueryRow(`SELECT query FROM saved_searches WHERE name = 'girls'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "1girl" {
		t.Errorf("original query should survive duplicate-name attempt, got %q", stored)
	}
}

func TestRenameTag_HTMXCollisionSurfacesError(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	first, _ := cx.TagSvc.GetOrCreateTag("first", generalID)
	mustTag(t, cx.TagSvc, "second", generalID)

	form := url.Values{"name": {"second"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", first.ID), form)

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-err")
	wantIn(t, w.Body.String(), "second")
	var stillName string
	_ = cx.DB.Read.QueryRow(`SELECT name FROM tags WHERE id = ?`, first.ID).Scan(&stillName)
	if stillName != "first" {
		t.Errorf("collision should leave tag untouched, got name %q", stillName)
	}
}

func TestBatchTagAlias_RefusedRunFailsAndNamesTheReason(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	ratingGeneral := ratingTagIDWeb(t, cx.DB, "general")
	plain := mustTag(t, cx.TagSvc, "general", cx.GeneralCategoryID)

	w := postForm(t, srv, "/tags/batch-alias", url.Values{
		"ids":          {strconv.FormatInt(ratingGeneral, 10)},
		"canonical_id": {"#" + strconv.FormatInt(plain.ID, 10)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	state := srv.jobs.Get()
	if state.Error == "" {
		t.Errorf("refused run completed with summary %q, want a failure", state.Summary)
	}
	if !strings.Contains(state.Error, tags.ErrRatingTagImmutable.Error()) {
		t.Errorf("job error = %q, want it to name %q", state.Error, tags.ErrRatingTagImmutable)
	}
}

func TestMergeFolded_RefusedRunFailsAndNamesTheReason(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	ratingGeneral := ratingTagIDWeb(t, cx.DB, "general")
	var wider int64
	if err := cx.DB.Write.QueryRow(`INSERT INTO tags (name, category_id, origin) VALUES ('general%', (SELECT category_id FROM tags WHERE id = ?), 'user') RETURNING id`, ratingGeneral).Scan(&wider); err != nil {
		t.Fatal(err)
	}
	mustExec(t, cx.DB.Write, `INSERT INTO folded_tag_pairs (old_id, new_id, category_id, ambiguous) VALUES (?, ?, (SELECT category_id FROM tags WHERE id = ?), 0)`, ratingGeneral, wider, ratingGeneral)

	w := postForm(t, srv, "/tags/merge-folded", url.Values{
		"ids": {strconv.FormatInt(ratingGeneral, 10)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	state := srv.jobs.Get()
	if state.Error == "" {
		t.Errorf("refused run completed with summary %q, want a failure", state.Summary)
	}
	if !strings.Contains(state.Error, tags.ErrRatingTagImmutable.Error()) {
		t.Errorf("job error = %q, want it to name %q", state.Error, tags.ErrRatingTagImmutable)
	}
}

func TestBatchTagAlias_PartialRunCompletesWithTheReason(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	canon := mustTag(t, cx.TagSvc, "canon", cx.GeneralCategoryID)
	ok := mustTag(t, cx.TagSvc, "mergeable", cx.GeneralCategoryID)
	ratingExplicit := ratingTagIDWeb(t, cx.DB, "explicit")

	w := postForm(t, srv, "/tags/batch-alias", url.Values{
		"ids":          {fmt.Sprintf("%d,%d", ok.ID, ratingExplicit)},
		"canonical_id": {"#" + strconv.FormatInt(canon.ID, 10)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	state := srv.jobs.Get()
	if state.Error != "" {
		t.Fatalf("partial run failed with %q, want a completion", state.Error)
	}
	if !strings.Contains(state.Summary, "aliased 1 tag") ||
		!strings.Contains(state.Summary, tags.ErrRatingTagImmutable.Error()) {
		t.Errorf("summary = %q, want the count plus the refusal reason", state.Summary)
	}
}

func TestTagDetailPage(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := cx.GeneralCategoryID
	parent, _ := cx.TagSvc.GetOrCreateTagFrom("detail_parent", general, "danbooru")
	implied, _ := cx.TagSvc.GetOrCreateTag("detail_child", general)
	if _, err := cx.TagSvc.AddImplicationFrom(parent.ID, implied.ID, "ptr"); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.TagSvc.CreateAliasFrom("detail_alias", general, parent.ID, "ptr"); err != nil {
		t.Fatal(err)
	}
	imgID := seedImage(t, srv, "detail.png", 8, 8)
	mustAddTag(t, cx.TagSvc, imgID, parent.ID, false, nil)

	get := func(path string) *httptest.ResponseRecorder {
		return do(srv, "GET", path)
	}
	getHX := func(path string) *httptest.ResponseRecorder {
		return do(srv, "GET", path, [2]string{"HX-Request", "true"})
	}

	w := get(fmt.Sprintf("/tags/%d", parent.ID))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{"detail_parent", "danbooru", "Aliases pointing here", "detail_alias", "detail_child", "Recent images"} {
		wantIn(t, body, want)
	}

	w = getHX(fmt.Sprintf("/tags/%d/usage", parent.ID))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Applied by") {
		t.Errorf("usage panel = %d, missing the applied-by table", w.Code)
	}

	explicitID := ratingTagIDWeb(t, srv.db(), "explicit")
	mustAddTag(t, cx.TagSvc, imgID, explicitID, false, nil)
	ceilW := do(srv, "GET", fmt.Sprintf("/tags/%d", parent.ID), [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
	wantNotIn(t, ceilW.Body.String(), fmt.Sprintf("/images/%d", imgID))
	wantIn(t, get(fmt.Sprintf("/tags/%d", parent.ID)).Body.String(), fmt.Sprintf("/images/%d", imgID))

	w = get(fmt.Sprintf("/tags/%d", implied.ID))
	if !strings.Contains(w.Body.String(), "Implied by") || !strings.Contains(w.Body.String(), "detail_parent") {
		t.Errorf("implied tag's page missing the reverse edge")
	}

	var aliasID int64
	_ = srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = 'detail_alias'`).Scan(&aliasID)
	w = get(fmt.Sprintf("/tags/%d", aliasID))
	aliasBody := w.Body.String()
	if !strings.Contains(aliasBody, "Resolves to") || !strings.Contains(aliasBody, fmt.Sprintf("/tags/%d", parent.ID)) {
		t.Errorf("alias page missing the Resolves-to card")
	}
	wantIn(t, aliasBody, fmt.Sprintf(`<a href="/tags/%d#tag-path">[where it comes from ›]</a>`, parent.ID))
	wantNotIn(t, aliasBody, "/usage\"")

	if w = get("/tags/999999"); w.Code != http.StatusNotFound {
		t.Errorf("unknown tag id = %d, want 404", w.Code)
	}

	w = getHX(fmt.Sprintf("/tags/%d/usage", parent.ID))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "tag-usage-graph") {
		t.Errorf("usage panel fragment = %d, body %q", w.Code, w.Body.String())
	}

	w = get(fmt.Sprintf("/tags/%d/usage", parent.ID))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != fmt.Sprintf("/tags/%d", parent.ID) {
		t.Errorf("plain usage GET = %d -> %q, want 303 to the tag page", w.Code, w.Header().Get("Location"))
	}
}

func TestBatchTagAlias_SelectionMergesIntoCanonical(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	srcA, _ := cx.TagSvc.GetOrCreateTag("alias_src_a", generalID)
	srcB, _ := cx.TagSvc.GetOrCreateTag("alias_src_b", generalID)
	dst, _ := cx.TagSvc.GetOrCreateTag("alias_dst", generalID)

	w := postForm(t, srv, "/tags/batch-alias", url.Values{
		"ids":          {fmt.Sprintf("%d,%d,%d", srcA.ID, srcB.ID, dst.ID)},
		"canonical_id": {fmt.Sprintf("#%d", dst.ID)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if st := srv.jobs.Get(); st == nil || st.Summary != "aliased 2 tags to general:alias_dst" {
		t.Errorf("summary = %+v, want the two aliased and nothing skipped", st)
	}

	for _, src := range []int64{srcA.ID, srcB.ID} {
		full, err := cx.TagSvc.GetTag(src)
		if err != nil {
			t.Fatal(err)
		}
		if !full.IsAlias || full.CanonicalTagID == nil || *full.CanonicalTagID != dst.ID {
			t.Errorf("tag %d after batch alias = %+v, want alias of %d", src, full, dst.ID)
		}
	}
}

func batchAliasTo(t *testing.T, srv *Server, srcID int64, canonical string) *httptest.ResponseRecorder {
	t.Helper()
	w := postForm(t, srv, "/tags/batch-alias", url.Values{
		"ids":          {strconv.FormatInt(srcID, 10)},
		"canonical_id": {canonical},
	})
	if w.Code == http.StatusAccepted {
		awaitJobsDrain(t, srv)
	}
	return w
}

func wantAliasOf(t *testing.T, srv *Server, srcID, canonID int64) {
	t.Helper()
	full, err := srv.active().TagSvc.GetTag(srcID)
	if err != nil {
		t.Fatal(err)
	}
	if !full.IsAlias || full.CanonicalTagID == nil || *full.CanonicalTagID != canonID {
		t.Errorf("tag %d = alias %v of %v, want an alias of %d", srcID, full.IsAlias, full.CanonicalTagID, canonID)
	}
}

func TestBatchTagAlias_TypedNameIsNormalisedAndFollowsAliases(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	miku := mustTag(t, cx.TagSvc, "hatsune_miku", categoryIDByName(t, srv, "character"))
	mustCreateAlias(t, cx.TagSvc, "miku", cx.GeneralCategoryID, miku.ID)
	typed := mustTag(t, cx.TagSvc, "typed_src", cx.GeneralCategoryID)
	viaAlias := mustTag(t, cx.TagSvc, "alias_src", cx.GeneralCategoryID)

	mustCode(t, batchAliasTo(t, srv, typed.ID, "Hatsune_Miku"), http.StatusAccepted)
	wantAliasOf(t, srv, typed.ID, miku.ID)
	if state := srv.jobs.Get(); !strings.Contains(state.Summary, "aliased 1 tag to character:hatsune_miku") {
		t.Errorf("summary = %q, want it to name the target", state.Summary)
	}
	mustCode(t, batchAliasTo(t, srv, viaAlias.ID, "miku"), http.StatusAccepted)
	wantAliasOf(t, srv, viaAlias.ID, miku.ID)
	if n := countRows(t, cx.DB, "tags WHERE name = 'hatsune_miku' AND category_id = ?", cx.GeneralCategoryID); n != 0 {
		t.Errorf("a general hatsune_miku was created (%d row)", n)
	}
}

func TestBatchTagAlias_DigitsAreANameAndHashIsAnID(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	year := mustTag(t, cx.TagSvc, "2019", categoryIDByName(t, srv, "year"))
	byName := mustTag(t, cx.TagSvc, "digits_src", cx.GeneralCategoryID)
	byID := mustTag(t, cx.TagSvc, "hash_src", cx.GeneralCategoryID)
	target := mustTag(t, cx.TagSvc, "hash_target", cx.GeneralCategoryID)

	mustCode(t, batchAliasTo(t, srv, byName.ID, "2019"), http.StatusAccepted)
	wantAliasOf(t, srv, byName.ID, year.ID)
	mustCode(t, batchAliasTo(t, srv, byID.ID, fmt.Sprintf("#%d", target.ID)), http.StatusAccepted)
	wantAliasOf(t, srv, byID.ID, target.ID)
}

func TestBatchTagAlias_AFailedIDReadIsNotATagNotFound(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	src := mustTag(t, cx.TagSvc, "read_fail_src", cx.GeneralCategoryID)
	target := mustTag(t, cx.TagSvc, "read_fail_target", cx.GeneralCategoryID)
	if err := cx.DB.Read.Close(); err != nil {
		t.Fatal(err)
	}

	w := batchAliasTo(t, srv, src.ID, fmt.Sprintf("#%d", target.ID))
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "Tag lookup failed")
	if strings.Contains(w.Body.String(), "Tag not found") {
		t.Errorf("a failed read was reported as a missing tag: %s", w.Body.String())
	}
}

func TestBatchTagAlias_AmbiguousBareNameIsRefusedWithItsChoices(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustTag(t, cx.TagSvc, "cat", cx.GeneralCategoryID)
	mustTag(t, cx.TagSvc, "cat", categoryIDByName(t, srv, "species"))
	src := mustTag(t, cx.TagSvc, "ambiguous_src", cx.GeneralCategoryID)

	w := batchAliasTo(t, srv, src.ID, "cat")
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "use general:cat or species:cat")
	if full, _ := cx.TagSvc.GetTag(src.ID); full.IsAlias {
		t.Error("the refused alias went through")
	}
}

func TestBatchTagScope_FilterDefaultsToPlainTags(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	metaID := categoryIDByName(t, srv, "meta")
	plain, _ := cx.TagSvc.GetOrCreateTag("scopeplain", generalID)
	src, _ := cx.TagSvc.GetOrCreateTag("scopealias", generalID)
	canon, _ := cx.TagSvc.GetOrCreateTag("scopecanon", generalID)
	if err := cx.TagSvc.MergeTags(src.ID, canon.ID); err != nil {
		t.Fatal(err)
	}

	w := postForm(t, srv, "/tags/batch-category", url.Values{
		"q":           {"scope"},
		"category_id": {fmt.Sprintf("%d", metaID)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	for _, id := range []int64{plain.ID, canon.ID} {
		full, _ := cx.TagSvc.GetTag(id)
		if full.CategoryID != metaID {
			t.Errorf("plain tag %d category = %d, want meta %d", id, full.CategoryID, metaID)
		}
	}
	aliased, _ := cx.TagSvc.GetTag(src.ID)
	if !aliased.IsAlias || aliased.CategoryID != generalID {
		t.Errorf("alias row should stay out of the default scope; tag = %+v", aliased)
	}
}

func TestBatchTagHandlers_RejectBadInput(t *testing.T) {
	srv := newTestServer(t)
	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"category: non-numeric id", "/tags/batch-category", url.Values{"category_id": {"abc"}, "ids": {"1"}}},
		{"category: bad ids list", "/tags/batch-category", url.Values{"category_id": {"1"}, "ids": {"1,x"}}},
		{"alias: empty canonical", "/tags/batch-alias", url.Values{"canonical_id": {""}, "ids": {"1"}}},
		{"alias: unknown canonical id", "/tags/batch-alias", url.Values{"canonical_id": {"#999999"}, "ids": {"1"}}},
		{"imply: empty target", "/tags/batch-imply", url.Values{"target": {""}, "ids": {"1"}}},
		{"imply remove: unknown target", "/tags/batch-imply", url.Values{"mode": {"remove"}, "target": {"nosuchtag"}, "ids": {"1"}}},
	}
	for _, tc := range cases {
		if w := postForm(t, srv, tc.path, tc.form); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", tc.name, w.Code, w.Body.String())
		}
	}
	if srv.jobs.IsRunning() {
		t.Error("a rejected batch should not have claimed the job slot")
	}
}

func TestBatchTagHandlers_EmptySelectionIsNoOp(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	keeper, _ := cx.TagSvc.GetOrCreateTag("empty_scope_keeper", generalID)

	w := postForm(t, srv, "/tags/delete-search", url.Values{"ids": {""}})
	mustCode(t, w, http.StatusAccepted)
	if srv.jobs.IsRunning() {
		t.Error("an empty selection should not have claimed the job slot")
	}
	if _, err := cx.TagSvc.GetTag(keeper.ID); err != nil {
		t.Errorf("tag outside the empty selection was deleted: %v", err)
	}
}

func patchCategory(t *testing.T, srv *Server, id int64, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return sendForm(t, srv, "PATCH", fmt.Sprintf("/tags/%d/category", id), form, [2]string{"HX-Request", "true"})
}

func TestChangeTagCategory_OffersOrKeepsTheOldSpellingAsAnAlias(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	character := categoryIDByName(t, srv, "character")
	offered := mustTag(t, cx.TagSvc, "mr_smith", cx.GeneralCategoryID)
	kept := mustTag(t, cx.TagSvc, "ms_smith", cx.GeneralCategoryID)

	w := patchCategory(t, srv, offered.ID, url.Values{"category_id": {strconv.FormatInt(character, 10)}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`hx-post="/tags/%d/aliases?name=general%%3Amr_smith"`, offered.ID))
	wantIn(t, w.Body.String(), "Keep general:mr_smith as an alias")
	if n := countRows(t, srv.db(), "tags WHERE is_alias = 1"); n != 0 {
		t.Errorf("a plain move kept %d aliases, want 0 until asked", n)
	}
	w = postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/aliases?name=general%%3Amr_smith", offered.ID), url.Values{})
	mustCode(t, w, http.StatusNoContent)
	if n := countRows(t, srv.db(), "tags WHERE name = 'mr_smith' AND category_id = ? AND canonical_tag_id = ?", cx.GeneralCategoryID, offered.ID); n != 1 {
		t.Errorf("general:mr_smith aliases = %d, want 1", n)
	}

	w = patchCategory(t, srv, kept.ID, url.Values{"category_id": {strconv.FormatInt(character, 10)}, "keep_alias": {"1"}})
	mustCode(t, w, http.StatusOK)
	wantNotIn(t, w.Body.String(), "as an alias")
	if n := countRows(t, srv.db(), "tags WHERE name = 'ms_smith' AND category_id = ? AND canonical_tag_id = ?", cx.GeneralCategoryID, kept.ID); n != 1 {
		t.Errorf("general:ms_smith aliases = %d, want 1", n)
	}
}

func TestBatchTagCategory_KeepsTheOldSpellingsAsAliases(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	character := categoryIDByName(t, srv, "character")
	a := mustTag(t, cx.TagSvc, "keep_a", cx.GeneralCategoryID)
	b := mustTag(t, cx.TagSvc, "keep_b", cx.GeneralCategoryID)
	w := postForm(t, srv, "/tags/batch-category", url.Values{
		"ids":         {fmt.Sprintf("%d,%d", a.ID, b.ID)},
		"category_id": {strconv.FormatInt(character, 10)},
		"keep_alias":  {"1"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if n := countRows(t, srv.db(), "tags WHERE category_id = ? AND is_alias = 0 AND name IN ('keep_a', 'keep_b')", character); n != 2 {
		t.Errorf("moved tags = %d, want 2", n)
	}
	if n := countRows(t, srv.db(), "tags WHERE category_id = ? AND is_alias = 1 AND name IN ('keep_a', 'keep_b')", cx.GeneralCategoryID); n != 2 {
		t.Errorf("kept aliases = %d, want 2", n)
	}
}

func TestBatchTagCategory_MergeOnCollision(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	metaID := categoryIDByName(t, srv, "meta")
	mover, _ := cx.TagSvc.GetOrCreateTag("collide", generalID)
	survivor, _ := cx.TagSvc.GetOrCreateTag("collide", metaID)
	imgID := seedImage(t, srv, "collide.png", 8, 8)
	mustAddTag(t, cx.TagSvc, imgID, mover.ID, false, nil)

	w := postForm(t, srv, "/tags/batch-category", url.Values{
		"ids":         {fmt.Sprintf("%d", mover.ID)},
		"category_id": {fmt.Sprintf("%d", metaID)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	full, _ := cx.TagSvc.GetTag(mover.ID)
	if full.CategoryID != generalID || full.IsAlias {
		t.Fatalf("collision without merge should skip; tag = %+v", full)
	}

	w = postForm(t, srv, "/tags/batch-category", url.Values{
		"ids":         {fmt.Sprintf("%d", mover.ID)},
		"category_id": {fmt.Sprintf("%d", metaID)},
		"merge":       {"1"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	full, _ = cx.TagSvc.GetTag(mover.ID)
	if !full.IsAlias || full.CanonicalTagID == nil || *full.CanonicalTagID != survivor.ID {
		t.Fatalf("merge-on-collision should alias into the survivor; tag = %+v", full)
	}
	surv, _ := cx.TagSvc.GetTag(survivor.ID)
	if surv.UsageCount != 1 {
		t.Errorf("survivor usage = %d, want 1 (the mover's image)", surv.UsageCount)
	}
}

func TestBatchTagImply_AddAndRemove(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	parent, _ := cx.TagSvc.GetOrCreateTag("imply_parent", generalID)
	target, _ := cx.TagSvc.GetOrCreateTag("imply_target", generalID)
	imgID := seedImage(t, srv, "imply.png", 8, 8)
	mustAddTag(t, cx.TagSvc, imgID, parent.ID, false, nil)

	w := postForm(t, srv, "/tags/batch-imply", url.Values{
		"ids":    {fmt.Sprintf("%d", parent.ID)},
		"mode":   {"add"},
		"target": {fmt.Sprintf("#%d", target.ID)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if implied := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imgID, target.ID); implied != 1 {
		t.Fatalf("implied row after batch add = %d, want 1", implied)
	}

	w = postForm(t, srv, "/tags/batch-imply", url.Values{
		"ids":    {fmt.Sprintf("%d", parent.ID)},
		"mode":   {"remove"},
		"target": {fmt.Sprintf("#%d", target.ID)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if implied := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", imgID, target.ID); implied != 0 {
		t.Errorf("implied row after batch remove = %d, want 0", implied)
	}
	if edges := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ?", parent.ID); edges != 0 {
		t.Errorf("edges after batch remove = %d, want 0", edges)
	}
}

func TestBatchTagImply_DeclaresAndRemovesSeveralTargets(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	parentA := mustTag(t, cx.TagSvc, "multi_parent_a", cx.GeneralCategoryID)
	parentB := mustTag(t, cx.TagSvc, "multi_parent_b", cx.GeneralCategoryID)
	known := mustTag(t, cx.TagSvc, "multi_known", cx.GeneralCategoryID)
	imgA := seedImage(t, srv, "multi_a.png", 8, 8)
	imgB := seedImage(t, srv, "multi_b.png", 9, 9)
	mustAddTag(t, cx.TagSvc, imgA, parentA.ID, false, nil)
	mustAddTag(t, cx.TagSvc, imgB, parentB.ID, false, nil)
	scope := fmt.Sprintf("%d,%d", parentA.ID, parentB.ID)

	w := postForm(t, srv, "/tags/batch-imply", url.Values{"ids": {scope}, "mode": {"add"}, "target": {"multi_known #999999 multi_fresh"}})
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "#999999: Tag not found")
	if n := countRows(t, srv.db(), "tags WHERE name = 'multi_fresh'"); n != 0 {
		t.Errorf("a refused run created multi_fresh (%d rows)", n)
	}

	w = postForm(t, srv, "/tags/batch-imply", url.Values{"ids": {scope}, "mode": {"add"}, "target": {"multi_known multi_fresh"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	fresh, err := cx.TagSvc.FindTag("multi_fresh", cx.GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, srv.db(), "tags WHERE name = 'multi_known_multi_fresh'"); n != 0 {
		t.Errorf("the two names were folded into one tag")
	}
	for _, img := range []int64{imgA, imgB} {
		if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id IN (?, ?) AND is_implied = 1", img, known.ID, fresh.ID); n != 2 {
			t.Errorf("image %d implied rows = %d, want 2", img, n)
		}
	}
	wantIn(t, srv.jobs.Get().Summary, "declared 4 implications of general:multi_known, general:multi_fresh")

	w = postForm(t, srv, "/tags/batch-imply", url.Values{"ids": {scope}, "mode": {"remove"}, "target": {"multi_known multi_fresh"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if n := countRows(t, srv.db(), "image_tags WHERE tag_id IN (?, ?)", known.ID, fresh.ID); n != 0 {
		t.Errorf("implied rows after remove = %d, want 0", n)
	}
	if n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id IN (?, ?)", parentA.ID, parentB.ID); n != 0 {
		t.Errorf("edges after remove = %d, want 0", n)
	}
}

func TestRenameTag_ACollisionOffersTheMergeOrTheAliasDrop(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mover := mustTag(t, cx.TagSvc, "mr._smith", cx.GeneralCategoryID)
	holder := mustTag(t, cx.TagSvc, "mr_smith", cx.GeneralCategoryID)
	mustCreateAlias(t, cx.TagSvc, "agent_smith", cx.GeneralCategoryID, holder.ID)
	rename := func(id int64, name string) string {
		w := postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", id), url.Values{"name": {name}})
		mustCode(t, w, http.StatusOK)
		return w.Body.String()
	}

	body := rename(mover.ID, "mr_smith")
	wantIn(t, body, "flash-err")
	wantIn(t, body, fmt.Sprintf(`data-id="%d" data-target="%d"`, mover.ID, holder.ID))
	wantIn(t, body, "Merge into general:mr_smith")

	wantIn(t, rename(mover.ID, "agent_smith"), "Merge into general:mr_smith")

	if err := cx.TagSvc.RenameTagKeepAlias(mover.ID, "mister_smith"); err != nil {
		t.Fatal(err)
	}
	var kept int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = 'mr._smith' AND canonical_tag_id = ?`, mover.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	body = rename(mover.ID, "mr._smith")
	wantIn(t, body, fmt.Sprintf(`data-alias="%d"`, kept))
	wantIn(t, body, "Drop the alias and rename")
}

func TestBatchTagClean_PreviewsThenRenamesAndMerges(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	g := cx.GeneralCategoryID
	dva := mustTag(t, cx.TagSvc, "d.va", g)
	mover := mustTag(t, cx.TagSvc, "mr._smith", g)
	holder := mustTag(t, cx.TagSvc, "mr_smith", g)
	dot := mustTag(t, cx.TagSvc, "a.b", g)
	comma := mustTag(t, cx.TagSvc, "a,b", g)
	face := mustTag(t, cx.TagSvc, "^_^", g)
	plain := mustTag(t, cx.TagSvc, "plain", g)
	img := seedImage(t, srv, "clean.png", 8, 8)
	mustAddTag(t, cx.TagSvc, img, mover.ID, false, nil)
	scope := fmt.Sprintf("%d,%d,%d,%d,%d,%d", dva.ID, mover.ID, dot.ID, comma.ID, face.ID, plain.ID)

	w := postHTMXForm(t, srv, "/tags/batch-clean", url.Values{"ids": {scope}, "chars": {".,^"}, "check": {"1"}})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "2 to rename, 2 to merge, 1 skipped, 1 unchanged")
	wantIn(t, body, "mr_smith, merging into general:mr_smith")
	wantIn(t, body, "ab, merging into general:ab")
	wantIn(t, body, "skipped: ")
	if n := countRows(t, srv.db(), "tags WHERE name IN ('dva', 'ab')"); n != 0 {
		t.Errorf("the preview renamed %d tags", n)
	}

	w = postForm(t, srv, "/tags/batch-clean", url.Values{"ids": {scope}, "chars": {".,^"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	wantIn(t, srv.jobs.Get().Summary, "renamed 2 tags, merged 2, skipped 2")
	for _, c := range []struct {
		name  string
		canon int64
	}{{"d.va", dva.ID}, {"mr._smith", holder.ID}, {"a.b", dot.ID}, {"a,b", dot.ID}} {
		if n := countRows(t, srv.db(), "tags WHERE name = ? AND is_alias = 1 AND canonical_tag_id = ?", c.name, c.canon); n != 1 {
			t.Errorf("%s: alias of %d rows = %d, want 1", c.name, c.canon, n)
		}
	}
	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", img, holder.ID); n != 1 {
		t.Errorf("the merged tag's image did not move onto mr_smith")
	}
	if got, _ := cx.TagSvc.GetTag(dva.ID); got.Name != "dva" {
		t.Errorf("d.va renamed to %q, want dva", got.Name)
	}
}

func TestRenameTag_HTMXSuccessRefreshes(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	tag, _ := cx.TagSvc.GetOrCreateTag("renameme", generalID)

	form := url.Values{"name": {"Pale Moon"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", tag.ID), form)

	wantCode(t, w, http.StatusNoContent)
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}
	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want empty (HX-Refresh handles reload)", got)
	}
	wantIn(t, w.Header().Get("HX-Trigger"), "Renamed to pale_moon.")
}

func TestCreateTagPost_CreatesEachNameAndRefreshesTheListing(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	canon := mustTag(t, cx.TagSvc, "crimson_moon", cx.GeneralCategoryID)
	mustCreateAlias(t, cx.TagSvc, "blood_moon", cx.GeneralCategoryID, canon.ID)
	general := strconv.FormatInt(cx.GeneralCategoryID, 10)

	for _, c := range []struct{ input, kind, flash string }{
		{"red_moon", "ok", "Tag red_moon created."},
		{"Red_Moon", "ok", "Tag red_moon already exists."},
		{"blood_moon", "ok", "blood_moon is an alias of general:crimson_moon."},
		{`new moon "Half Moon" character:luna`, "ok", "Created 4 tags: new, moon, half_moon, luna."},
		{"waxing_moon ---", "warn", "Tag waxing_moon created. Refused ---: "},
		{"dup_moon nier:automata Dup_Moon", "ok", "Created 2 tags: dup_moon, nier:automata; no category named nier.\""},
		{"dup_sun bogus:qux --- dup_sun", "warn", "Created 2 tags: dup_sun, bogus:qux; no category named bogus. Refused ---: "},
	} {
		w := postHTMXForm(t, srv, "/tags/new", url.Values{"name": {c.input}, "category_id": {general}})
		wantCode(t, w, http.StatusNoContent)
		if w.Header().Get("HX-Refresh") != "true" || w.Header().Get("HX-Redirect") != "" {
			t.Errorf("%s: want an HX-Refresh and no redirect, got %v", c.input, w.Header())
		}
		wantIn(t, w.Header().Get("HX-Trigger"), `"kind":"`+c.kind+`"`)
		wantIn(t, w.Header().Get("HX-Trigger"), c.flash)
	}
	if _, err := cx.TagSvc.FindTag("luna", categoryIDByName(t, srv, "character")); err != nil {
		t.Errorf("character:luna: %v", err)
	}

	w := postHTMXForm(t, srv, "/tags/new", url.Values{"name": {"--- ..."}, "category_id": {general}})
	wantIn(t, w.Body.String(), "flash-err")
	wantIn(t, w.Body.String(), "---: ")
}

func TestCreateAliasPost_FlashesTheStoredName(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustTag(t, cx.TagSvc, "full_moon", cx.GeneralCategoryID)
	general := strconv.FormatInt(cx.GeneralCategoryID, 10)

	w := postHTMXForm(t, srv, "/tags/aliases", url.Values{"name": {"Big Moon"}, "category_id": {general}, "canonical_id": {"full_moon"}})
	wantCode(t, w, http.StatusNoContent)
	if got, want := w.Header().Get("HX-Redirect"), "/tags?type=alias&q=big_moon"; got != want {
		t.Errorf("HX-Redirect = %q, want %q", got, want)
	}
	wantIn(t, w.Header().Get("HX-Trigger"), "Alias big_moon created, resolving to general:full_moon.")
}

func TestCreateTagPost_ARealCategoryPrefixWinsOverTheSelect(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := strconv.FormatInt(cx.GeneralCategoryID, 10)
	character := categoryIDByName(t, srv, "character")

	w := postHTMXForm(t, srv, "/tags/new", url.Values{"name": {"Character:Soandso"}, "category_id": {general}})
	wantCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Tag soandso created.")
	if _, err := cx.TagSvc.FindTag("soandso", character); err != nil {
		t.Errorf("character:soandso: %v", err)
	}
	if got := countRows(t, srv.db(), "tags WHERE name LIKE '%:soandso'"); got != 0 {
		t.Errorf("rows = %d, want 0", got)
	}

	w = postHTMXForm(t, srv, "/tags/new", url.Values{"name": {"re:zero"}, "category_id": {general}})
	wantCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Tag re:zero created; no category named re.")
	if _, err := cx.TagSvc.FindTag("re:zero", cx.GeneralCategoryID); err != nil {
		t.Errorf("general:re:zero: %v", err)
	}

	w = postHTMXForm(t, srv, "/tags/new", url.Values{"name": {"artist:"}, "category_id": {general}})
	wantIn(t, w.Body.String(), tags.ErrEmptyFormName.Error())
}

func TestCreateAliasPost_ARealCategoryPrefixFilesTheAlias(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustTag(t, cx.TagSvc, "new_moon", cx.GeneralCategoryID)
	general := strconv.FormatInt(cx.GeneralCategoryID, 10)
	character := categoryIDByName(t, srv, "character")

	w := postHTMXForm(t, srv, "/tags/aliases", url.Values{"name": {"character:luna"}, "category_id": {general}, "canonical_id": {"new_moon"}})
	wantCode(t, w, http.StatusNoContent)
	if got := countRows(t, srv.db(), "tags WHERE name = 'luna' AND category_id = ? AND is_alias = 1", character); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
	if got := countRows(t, srv.db(), "tags WHERE name LIKE '%:luna'"); got != 0 {
		t.Errorf("rows = %d, want 0", got)
	}
}

func TestRenameTag_ARealCategoryPrefixMovesTheTag(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	character := categoryIDByName(t, srv, "character")
	moved := mustTag(t, cx.TagSvc, "mr_smith", cx.GeneralCategoryID)
	kept := mustTag(t, cx.TagSvc, "ms_smith", cx.GeneralCategoryID)

	w := postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", moved.ID), url.Values{"name": {"character:agent_smith"}})
	wantCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Renamed to character:agent_smith.")
	if got := countRows(t, srv.db(), "tags WHERE id = ? AND name = 'agent_smith' AND category_id = ?", moved.ID, character); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}

	w = postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", kept.ID), url.Values{"name": {"character:ms_smith"}, "keep_alias": {"1"}})
	wantCode(t, w, http.StatusNoContent)
	if got := countRows(t, srv.db(), "tags WHERE id = ? AND category_id = ?", kept.ID, character); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
	if got := countRows(t, srv.db(), "tags WHERE name = 'ms_smith' AND category_id = ? AND canonical_tag_id = ?", cx.GeneralCategoryID, kept.ID); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}

	mustTag(t, cx.TagSvc, "zed", cx.GeneralCategoryID)
	foo := mustTag(t, cx.TagSvc, "foo", cx.GeneralCategoryID)
	w = postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", foo.ID), url.Values{"name": {"character:zed"}})
	wantCode(t, w, http.StatusNoContent)

	bar := mustTag(t, cx.TagSvc, "bar", cx.GeneralCategoryID)
	w = postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/rename", bar.ID), url.Values{"name": {"character:zed"}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`data-id="%d" data-target="%d"`, bar.ID, foo.ID))
	wantIn(t, w.Body.String(), "Merge into character:zed")
}

func TestAddTagToImage_RealCategoryPrefixStillSplits(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "colon_artist.png", 10, 10)

	form := url.Values{"tag": {"artist:john_doe"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), form)

	mustCode(t, w, http.StatusOK)
	if cat := imageTagCategory(t, srv, id, "john_doe"); cat != "artist" {
		t.Errorf("john_doe category = %q, want artist", cat)
	}
	if cat := imageTagCategory(t, srv, id, "artist:john_doe"); cat != "" {
		t.Errorf("literal artist:john_doe must not be stored, got category %q", cat)
	}
}

func TestUpdateExternal_HTMXBadURLSurfacesFlash(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext.png", 10, 10)

	form := url.Values{"url": {"ftp://example.com"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/sources/set", id), form)

	wantCode(t, w, http.StatusOK)
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html so htmx swaps as a fragment", got)
	}
	wantIn(t, w.Body.String(), `class="flash flash-err"`)
	wantIn(t, w.Body.String(), "url must start with")
}

func TestUpdateExternal_HTMXSuccessRefreshes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_ok.png", 10, 10)

	form := url.Values{"url": {"https://example.com/art"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/sources/set", id), form)

	wantCode(t, w, http.StatusNoContent)
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}
	var stored string
	_ = srv.active().DB.Read.QueryRow(`SELECT url FROM images WHERE id = ?`, id).Scan(&stored)
	if stored != "https://example.com/art" {
		t.Errorf("images.url = %q, want https://example.com/art", stored)
	}
}

func TestVacuumDBPost_RunsToCompletion(t *testing.T) {
	srv := newTestServer(t)

	for i := 0; i < 4; i++ {
		seedImage(t, srv, fmt.Sprintf("vac%d.png", i), 6+i, 6+i)
	}

	form := url.Values{}
	w := postForm(t, srv, "/settings/maintenance/vacuum-db", form)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Vacuum started")

	awaitJobsDrain(t, srv)
	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("expected job state after vacuum drain")
	}
	wantIn(t, state.Summary, "Vacuumed")
}

func TestDeleteSearch_BulkDeleteReconcilesUsage(t *testing.T) {
	srv := newTestServer(t)

	const n = 6
	var ids []int64
	for i := 0; i < n; i++ {
		ids = append(ids, seedImage(t, srv, fmt.Sprintf("bd%d.png", i), 4+i, 4+i))
	}
	general := srv.active().GeneralCategoryID
	sharedID, _ := mustExec(t, srv.db().Write,
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, "shared", general).LastInsertId()
	for _, id := range ids {
		mustAddTag(t, srv.tagSvc(), id, sharedID, false, nil)
	}
	untouchedID, _ := mustExec(t, srv.db().Write,
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 99)`, "untouched", general).LastInsertId()

	form := url.Values{"q": {""}}
	w := postForm(t, srv, "/internal/delete-search", form)
	mustCode(t, w, http.StatusAccepted)

	awaitJobsDrain(t, srv)

	imgCount := countRows(t, srv.db(), "images")
	if imgCount != 0 {
		t.Errorf("images count after bulk delete = %d, want 0", imgCount)
	}
	var sharedUsage, untouchedUsage int
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, sharedID).Scan(&sharedUsage)
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, untouchedID).Scan(&untouchedUsage)
	if sharedUsage != 0 {
		t.Errorf("shared tag usage_count = %d after wiping every carrier, want 0", sharedUsage)
	}
	if untouchedUsage != 99 {
		t.Errorf("untouched tag usage_count = %d, want 99 (unchanged)", untouchedUsage)
	}
}

func TestRemoveUserTagsFromImageHandler_DropsManualOnly(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ut.png", 10, 10)

	manualID := insertProvenanceTag(t, srv, id, "manual_a", 0, nil)
	autoID := insertProvenanceTag(t, srv, id, "auto_b", 1, "tagger-A")
	sourceID := insertProvenanceTag(t, srv, id, "source_c", 0, "danbooru")

	w := deleteCSRF(t, srv, fmt.Sprintf("/images/%d/user-tags", id))
	mustCode(t, w, http.StatusOK)

	manualLeft := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND is_auto = 0 AND (tagger_name IS NULL OR tagger_name = '')", id)
	sourceLeft := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND is_auto = 0 AND tagger_name = 'danbooru'", id)
	autoLeft := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND is_auto = 1", id)
	if manualLeft != 0 {
		t.Errorf("manual user-tags should be 0 after delete, got %d", manualLeft)
	}
	if sourceLeft != 1 {
		t.Errorf("source tags should survive a user-tags delete, got %d", sourceLeft)
	}
	if autoLeft != 1 {
		t.Errorf("auto-tags should remain 1 (left alone), got %d", autoLeft)
	}
	var manualUsage, sourceUsage, autoUsage int
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, manualID).Scan(&manualUsage)
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, sourceID).Scan(&sourceUsage)
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, autoID).Scan(&autoUsage)
	if manualUsage != 0 {
		t.Errorf("manual_a usage_count = %d after user-tags delete, want 0", manualUsage)
	}
	if sourceUsage != 1 {
		t.Errorf("source_c usage_count = %d after user-tags delete, want 1 (unchanged)", sourceUsage)
	}
	if autoUsage != 1 {
		t.Errorf("auto_b usage_count = %d after user-tags delete, want 1 (unchanged)", autoUsage)
	}
}

func TestRemoveAutoTagsFromImageHandler_RespectsTaggerFilter(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "at.png", 10, 10)

	aID := insertProvenanceTag(t, srv, id, "auto_from_a", 1, "tagger-A")
	bID := insertProvenanceTag(t, srv, id, "auto_from_b", 1, "tagger-B")

	w := deleteCSRF(t, srv, fmt.Sprintf("/images/%d/auto-tags?taggers=tagger-A", id))
	mustCode(t, w, http.StatusOK)

	leftA := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, aID)
	leftB := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, bID)
	if leftA != 0 {
		t.Errorf("tagger-A row should be removed, got %d", leftA)
	}
	if leftB != 1 {
		t.Errorf("tagger-B row should survive (out of scope), got %d", leftB)
	}
}

func insertProvenanceTag(t *testing.T, srv *Server, id int64, name string, isAuto int, tagger any) int64 {
	t.Helper()
	general := srv.active().GeneralCategoryID
	tagID, _ := mustExec(t, srv.db().Write, `INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 1)`, name, general).LastInsertId()
	mustExec(t, srv.db().Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, ?, ?)`, id, tagID, isAuto, tagger)
	return tagID
}

func imageTagCount(t *testing.T, srv *Server, id, tagID int64) int {
	t.Helper()
	return countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, tagID)
}

func TestRemoveSourceTagsFromImageHandler(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "st.png", 10, 10)
	danb := insertProvenanceTag(t, srv, id, "from_danbooru", 0, "danbooru")
	gelb := insertProvenanceTag(t, srv, id, "from_gelbooru", 0, "gelbooru")
	manual := insertProvenanceTag(t, srv, id, "manual_tag", 0, nil)
	auto := insertProvenanceTag(t, srv, id, "auto_tag", 1, "tagger-A")

	dw := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	body := dw.Body.String()
	wantIn(t, body, "Remove tags...")
	wantIn(t, body, `<option value="danbooru">`)
	wantIn(t, body, `<option value="tagger-A">`)

	w := deleteCSRF(t, srv, fmt.Sprintf("/images/%d/source-tags?sources=danbooru", id))
	mustCode(t, w, http.StatusOK)

	if imageTagCount(t, srv, id, danb) != 0 {
		t.Error("danbooru's tag should be removed")
	}
	if imageTagCount(t, srv, id, gelb) != 1 {
		t.Error("gelbooru's tag should survive (out of scope)")
	}
	if imageTagCount(t, srv, id, manual) != 1 {
		t.Error("the operator's manual tag should survive")
	}
	if imageTagCount(t, srv, id, auto) != 1 {
		t.Error("auto-tag should survive")
	}
}

func TestRemoveSourceTagsFromImageHandler_CommaInLabel(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "stc.png", 10, 10)
	site := "foo,bar"
	tagged := insertProvenanceTag(t, srv, id, "from_foobar", 0, &site)
	other := insertProvenanceTag(t, srv, id, "from_gelbooru", 0, "gelbooru")

	w := deleteCSRF(t, srv, fmt.Sprintf("/images/%d/source-tags?sources=%s", id, url.QueryEscape(site)))
	mustCode(t, w, http.StatusOK)

	if imageTagCount(t, srv, id, tagged) != 0 {
		t.Error("the comma-labelled source's tag should be removed")
	}
	if imageTagCount(t, srv, id, other) != 1 {
		t.Error("the other source's tag should survive")
	}
}

func TestRemoveTags_FlashesWhatWent(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "rmf.png", 10, 10)
	one := insertProvenanceTag(t, srv, id, "rmf_one", 0, nil)
	insertProvenanceTag(t, srv, id, "rmf_two", 0, nil)
	insertProvenanceTag(t, srv, id, "rmf_three", 0, nil)

	del := func(path string) string {
		t.Helper()
		w := deleteCSRF(t, srv, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	body := del(fmt.Sprintf("/images/%d/tags/%d", id, one))
	if !strings.Contains(body, "flash-ok") || !strings.Contains(body, "removed: rmf_one") {
		t.Errorf("single remove should flash the tag name, got %s", body)
	}

	body = del(fmt.Sprintf("/images/%d/user-tags", id))
	if !strings.Contains(body, "flash-ok") || !strings.Contains(body, "removed 2 tags") {
		t.Errorf("group remove should flash the count, got %s", body)
	}

	body = del(fmt.Sprintf("/images/%d/user-tags", id))
	wantNotIn(t, body, "flash-ok")
}

func TestRemoveSourceTagsFromImageHandler_StaleFilter(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "stf.png", 10, 10)
	current := insertProvenanceTag(t, srv, id, "still_listed", 0, "danbooru")
	dropped := insertProvenanceTag(t, srv, id, "dropped", 0, "danbooru")
	mustExec(t, srv.db().Write, `UPDATE image_tags SET stale = 1 WHERE image_id = ? AND tag_id = ?`, id, dropped)

	w := deleteCSRF(t, srv, fmt.Sprintf("/images/%d/source-tags?sources=danbooru&stale=1", id))
	mustCode(t, w, http.StatusOK)
	if imageTagCount(t, srv, id, dropped) != 0 {
		t.Error("the stale row should be removed")
	}
	if imageTagCount(t, srv, id, current) != 1 {
		t.Error("the same source's current row should survive a stale-scoped removal")
	}
}

func TestBatchStrip_SourceModes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "bs.png", 10, 10)
	danb := insertProvenanceTag(t, srv, id, "bs_danbooru", 0, "danbooru")
	gelb := insertProvenanceTag(t, srv, id, "bs_gelbooru", 0, "gelbooru")
	manual := insertProvenanceTag(t, srv, id, "bs_manual", 0, nil)
	auto := insertProvenanceTag(t, srv, id, "bs_auto", 1, "tagger-A")

	ids := strconv.FormatInt(id, 10)
	post := func(form url.Values) {
		t.Helper()
		form.Set("scope", "selection")
		form.Set("ids", ids)
		w := postForm(t, srv, "/internal/batch-strip", form)
		mustCode(t, w, http.StatusAccepted)
		awaitJobsDrain(t, srv)
	}

	post(url.Values{"mode": {"source"}, "source": {"danbooru"}})
	if imageTagCount(t, srv, id, danb) != 0 {
		t.Error("mode=source should strip danbooru's tag")
	}
	if imageTagCount(t, srv, id, gelb) != 1 || imageTagCount(t, srv, id, manual) != 1 || imageTagCount(t, srv, id, auto) != 1 {
		t.Error("mode=source should strip only the named source")
	}

	post(url.Values{"mode": {"source-all"}})
	if imageTagCount(t, srv, id, gelb) != 0 {
		t.Error("mode=source-all should strip gelbooru's tag")
	}
	if imageTagCount(t, srv, id, manual) != 1 {
		t.Error("mode=source-all must leave manual tags")
	}
	if imageTagCount(t, srv, id, auto) != 1 {
		t.Error("mode=source-all must leave auto tags")
	}
}

func TestBatchStrip_SweepsOrphanedImplied(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "bso.png", 10, 10)
	cat := srv.active().GeneralCategoryID
	child := mustTag(t, srv.tagSvc(), "bso_child", cat)
	parent := mustTag(t, srv.tagSvc(), "bso_parent", cat)
	mustAddImply(t, srv.tagSvc(), parent.ID, child.ID)
	mustAddTag(t, srv.tagSvc(), id, parent.ID, false, nil)
	if imageTagCount(t, srv, id, child.ID) != 1 {
		t.Fatal("fan-out should have landed the implied row")
	}

	w := postForm(t, srv, "/internal/batch-strip", url.Values{"scope": {"selection"}, "ids": {strconv.FormatInt(id, 10)}, "mode": {"user"}})
	mustCode(t, w, http.StatusAccepted)
	drainJob(t, srv)

	if imageTagCount(t, srv, id, parent.ID) != 0 {
		t.Error("the user tag should be stripped")
	}
	if imageTagCount(t, srv, id, child.ID) != 0 {
		t.Error("the implied row lost its only parent and should be gone")
	}
	got, err := srv.tagSvc().GetTag(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 0 {
		t.Errorf("implied tag usage_count = %d, want 0", got.UsageCount)
	}
}

func TestBatchStrip_SummaryReportsRowChanges(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "bsr.png", 10, 10)
	insertProvenanceTag(t, srv, id, "bsr_manual", 0, nil)

	strip := func(mode string) string {
		t.Helper()
		w := postForm(t, srv, "/internal/batch-strip", url.Values{"scope": {"selection"}, "ids": {strconv.FormatInt(id, 10)}, "mode": {mode}})
		if w.Code != http.StatusAccepted {
			t.Fatalf("batch-strip %s: %d %s", mode, w.Code, w.Body.String())
		}
		awaitJobsDrain(t, srv)
		state := srv.jobs.Get()
		if state == nil {
			t.Fatalf("batch-strip %s left no job state", mode)
		}
		return state.Summary
	}

	if msg := strip("stale"); !strings.Contains(msg, "(0 row changes)") {
		t.Errorf("zero-stale strip summary = %q, want a 0 row-change report", msg)
	}
	if msg := strip("user"); !strings.Contains(msg, "(1 row change)") {
		t.Errorf("user strip summary = %q, want a 1 row-change report", msg)
	}
}

func TestBatchTag_SummaryNamesReplacedRatings(t *testing.T) {
	srv := newTestServer(t)
	rated := seedImage(t, srv, "brr_rated.png", 10, 10)
	unrated := seedImage(t, srv, "brr_unrated.png", 11, 11)
	addRatingTag(t, srv.db(), rated, "explicit")

	batchAdd := func(tag string, ids ...int64) string {
		t.Helper()
		form := url.Values{
			"scope": {"selection"}, "op": {"add"}, "tags": {tag},
		}
		for _, id := range ids {
			form.Add("ids", strconv.FormatInt(id, 10))
		}
		w := postForm(t, srv, "/internal/batch-tag", form)
		mustCode(t, w, http.StatusAccepted)
		awaitJobsDrain(t, srv)
		return srv.jobs.Get().Summary
	}

	if msg := batchAdd("brr_plain", rated, unrated); strings.Contains(msg, "Replaced the rating") {
		t.Errorf("non-rating add summary = %q, want no rating report", msg)
	}

	msg := batchAdd("rating:general", rated, unrated)
	wantIn(t, msg, "Replaced the rating on 1 image.")
}

func TestBatchTag_SummaryNamesTheRefusedTokens(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "brt_one.png", 10, 10)

	batchAdd := func(tags string) *models.JobState {
		t.Helper()
		w := postForm(t, srv, "/internal/batch-tag", url.Values{
			"scope": {"selection"}, "op": {"add"},
			"tags": {tags}, "ids": {strconv.FormatInt(id, 10)},
		})
		mustCode(t, w, http.StatusAccepted)
		awaitJobsDrain(t, srv)
		return srv.jobs.Get()
	}

	st := batchAdd(`brt_ok star*tag`)
	wantIn(t, st.Summary, "Tagged 1 of 1 image ")
	wantIn(t, st.Summary, "star*tag")

	st = batchAdd(`only*bad`)
	if st.Error == "" {
		t.Errorf("an all-refused run completed with summary %q, want a failure", st.Summary)
	}
	wantIn(t, st.Error, "only*bad")
}

func TestRunBatchTag_RemoveCountsOnlyTheImagesThatChanged(t *testing.T) {
	srv := newTestServer(t)
	mustTag(t, srv.tagSvc(), "water", srv.active().GeneralCategoryID)
	var ids []string
	for i, name := range []string{"dry_a.png", "dry_b.png", "dry_c.png"} {
		ids = append(ids, strconv.FormatInt(seedImage(t, srv, name, 10+i, 10), 10))
	}

	w := postForm(t, srv, "/internal/batch-tag", url.Values{
		"scope": {"selection"}, "op": {"remove"}, "tags": {"water"}, "ids": ids,
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	wantIn(t, srv.jobs.Get().Summary, "Untagged 0 of 3 images (0 row changes).")
}

func TestRunBatchTag_RemoveNamesTheTokensThatMatchedNothing(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "strip_one.png", 10, 10)
	cats, err := tags.CategoryIDsByName(srv.db().Read)
	if err != nil {
		t.Fatal(err)
	}
	present := mustTag(t, srv.tagSvc(), "strip_demo", srv.active().GeneralCategoryID)
	character := mustTag(t, srv.tagSvc(), "hakurei_reimu", cats["character"])
	mustAddTag(t, srv.tagSvc(), id, present.ID, false, nil)
	mustAddTag(t, srv.tagSvc(), id, character.ID, false, nil)

	w := postForm(t, srv, "/internal/batch-tag", url.Values{
		"scope": {"selection"}, "op": {"remove"},
		"tags": {"strip_demo general:hakurei_reimu totally_made_up"},
		"ids":  {strconv.FormatInt(id, 10)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	st := srv.jobs.Get()
	wantIn(t, st.Summary, "Untagged 1 of 1 image ")
	wantIn(t, st.Summary, "general:hakurei_reimu")
	wantIn(t, st.Summary, "totally_made_up")
	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, character.ID); n != 1 {
		t.Errorf("the character tag is gone (%d rows), so the test is not measuring the unmatched token", n)
	}
}

func TestServeImageFile_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "served.png", 10, 10)

	w := do(srv, "GET", fmt.Sprintf("/images/%d/file", id))
	mustCode(t, w, http.StatusOK)

	outside := filepath.Join(t.TempDir(), "evil.txt")
	mustWriteFile(t, outside, []byte("leaked"), 0o644)
	if _, err := srv.active().DB.Write.Exec(
		`UPDATE images SET canonical_path = ? WHERE id = ?`, outside, id,
	); err != nil {
		t.Fatal(err)
	}

	w2 := do(srv, "GET", fmt.Sprintf("/images/%d/file", id))
	wantCode(t, w2, http.StatusNotFound)
}

func TestServeImageFile_NamesTheDownload(t *testing.T) {
	srv := newTestServer(t)
	page := tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	cbzID := seedManga(t, srv, "vol 1.cbz", [][]byte{page, page})
	pngID := seedImage(t, srv, "café.png", 8, 8)

	cases := []struct {
		url  string
		want string
	}{
		{fmt.Sprintf("/images/%d/file", cbzID), `inline; filename="vol 1.cbz"`},
		{fmt.Sprintf("/images/%d/file", pngID), "inline; filename*=utf-8''caf%C3%A9.png"},
		{fmt.Sprintf("/images/%d/view", pngID), "inline; filename*=utf-8''caf%C3%A9.png"},
	}
	for _, tc := range cases {
		w := do(srv, "GET", tc.url)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s expected 200, got %d", tc.url, w.Code)
		}
		if got := w.Header().Get("Content-Disposition"); got != tc.want {
			t.Errorf("GET %s Content-Disposition = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestAcceptsMediaType(t *testing.T) {
	for _, tc := range []struct {
		accept string
		want   bool
	}{
		{"image/jxl,image/*", true},
		{"image/webp,image/avif,image/jxl,image/heic,image/heic-sequence,video/*;q=0.8,image/png,image/svg+xml,image/*;q=0.8,*/*;q=0.5", true},
		{"image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8", false},
		{"image/*,*/*", false},
		{"", false},
		{"image/jxl;q=0", false},
		{"image/jxl;q=0.000", false},
		{"image/jxl;q=0.5", true},
		{"IMAGE/JXL", true},
		{" image/jxl ; q=1 ", true},
		{"image/jxl;q=high", false},
		{"image/jxl-x", false},
	} {
		if got := acceptsMediaType(tc.accept, "image/jxl"); got != tc.want {
			t.Errorf("acceptsMediaType(%q) = %v, want %v", tc.accept, got, tc.want)
		}
	}
}

func generateStill(t *testing.T, dst string, w, h int) {
	t.Helper()
	if !ffmpegOnPath() {
		t.Skip("ffmpeg not available")
	}
	codec := map[string]string{".avif": "libaom-av1", ".jxl": "libjxl"}[filepath.Ext(dst)]
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d", w, h),
		"-frames:v", "1", "-c:v", codec, "--", dst)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg cannot encode %s: %v", codec, err)
	}
}

func getView(t *testing.T, srv *Server, id int64, accept string) *httptest.ResponseRecorder {
	t.Helper()
	return getAccepting(t, srv, fmt.Sprintf("/images/%d/view", id), accept)
}

func getAccepting(t *testing.T, srv *Server, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	mustCode(t, w, http.StatusOK)
	return w
}

func TestServeImageView_NegotiatesAVIFAndJPEGXL(t *testing.T) {
	for _, tc := range []struct {
		name, mediaType, other string
	}{
		{"pic.avif", "image/avif", "image/webp,image/*,*/*;q=0.8"},
		{"pic.jxl", "image/jxl", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			cx := srv.active()
			src := filepath.Join(cx.GalleryPath, tc.name)
			generateStill(t, src, 64, 48)
			img, _, err := gallery.Ingest(cx.Handle, src, "")
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}

			orig := getView(t, srv, img.ID, tc.mediaType+",image/*")
			if ct := orig.Header().Get("Content-Type"); ct != tc.mediaType {
				t.Errorf("Content-Type = %q, want %q", ct, tc.mediaType)
			}
			if want, _ := os.ReadFile(src); !bytes.Equal(orig.Body.Bytes(), want) {
				t.Error("the original was not served as is")
			}
			if !slices.Contains(orig.Header().Values("Vary"), "Accept") {
				t.Errorf("Vary = %q, want Accept", orig.Header().Values("Vary"))
			}

			for _, accept := range []string{tc.other, tc.mediaType + ";q=0", ""} {
				w := getView(t, srv, img.ID, accept)
				if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
					t.Fatalf("Accept %q: Content-Type = %q, want image/jpeg", accept, ct)
				}
				if cfg, err := jpeg.DecodeConfig(w.Body); err != nil || cfg.Width != 64 || cfg.Height != 48 {
					t.Errorf("Accept %q: copy = %dx%d (%v), want 64x48", accept, cfg.Width, cfg.Height, err)
				}
				if !slices.Contains(w.Header().Values("Vary"), "Accept") {
					t.Errorf("Accept %q: Vary = %q, want Accept", accept, w.Header().Values("Vary"))
				}
				if w.Header().Get("ETag") == orig.Header().Get("ETag") {
					t.Errorf("Accept %q: the copy shares the original's ETag", accept)
				}
			}
		})
	}
}

func TestServeImageView_FFmpegStillPastTheCeiling(t *testing.T) {
	for _, name := range []string{"wide.avif", "wide.jxl"} {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t)
			cx := srv.active()
			src := filepath.Join(cx.GalleryPath, name)
			generateStill(t, src, 4100, 64)
			img, _, err := gallery.Ingest(cx.Handle, src, "")
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			mustExec(t, cx.DB.Write, `UPDATE images SET width = 20000, height = 5001 WHERE id = ?`, img.ID)

			w := getView(t, srv, img.ID, gallery.MIMEForFileType(img.FileType))
			if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
				t.Fatalf("Content-Type = %q, want image/jpeg", ct)
			}
			if cfg, err := jpeg.DecodeConfig(w.Body); err != nil || cfg.Width != gallery.ViewMaxDim {
				t.Errorf("rendition width = %d (%v), want %d", cfg.Width, err, gallery.ViewMaxDim)
			}
		})
	}
}

func TestServeImageView_OtherStillsDoNotVary(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "plain.png", 8, 8)
	w := getView(t, srv, id, "image/jxl")
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if slices.Contains(w.Header().Values("Vary"), "Accept") {
		t.Error("a PNG answer varies on Accept")
	}
}

func TestServeThumbnail_InvalidatesOnIDReuse(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "first.png", 8, 8)

	thumbURL := fmt.Sprintf("/thumbnails/%s/%d.jpg", srv.activeGallery(), id)

	w := do(srv, "GET", thumbURL)
	mustCode(t, w, http.StatusOK)
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want it to include no-cache", cc)
	}
	firstETag := w.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("ETag missing on initial thumbnail serve")
	}

	cx := srv.active()
	firstThumbPath := gallery.ThumbnailPath(cx.ThumbnailsPath, id)
	firstInfo, err := os.Stat(firstThumbPath)
	if err != nil {
		t.Fatalf("stat first thumbnail: %v", err)
	}

	if _, err := cx.DeleteImages(context.Background(), []int64{id}, library.DeleteOptions{}, nil); err != nil {
		t.Fatalf("DeleteImages: %v", err)
	}
	newID := seedImage(t, srv, "second.png", 12, 12)
	if newID != id {
		t.Fatalf("test premise broken: id was not reused (got %d, want %d); "+
			"the only image was deleted so the re-ingest must reuse the id", newID, id)
	}

	newThumbPath := gallery.ThumbnailPath(cx.ThumbnailsPath, newID)
	forcedMtime := firstInfo.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(newThumbPath, forcedMtime, forcedMtime); err != nil {
		t.Fatalf("chtimes new thumbnail: %v", err)
	}

	w2 := do(srv, "GET", thumbURL, [2]string{"If-None-Match", firstETag})
	if w2.Code == http.StatusNotModified {
		t.Fatalf("re-ingest at reused id should not 304; the cached ETag must be stale")
	}
	mustCode(t, w2, http.StatusOK)
	if newETag := w2.Header().Get("ETag"); newETag == "" || newETag == firstETag {
		t.Errorf("ETag must change on id-reuse rewrite: old=%q new=%q", firstETag, newETag)
	}
}

func TestServeImageFile_InvalidatesWithinOneSecond(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "turned.png", 8, 8)

	canonPath := canonicalPathOf(t, srv, id)
	fileURL := fmt.Sprintf("/images/%d/file", id)

	second := time.Now().Truncate(time.Second)
	if err := os.Chtimes(canonPath, second, second); err != nil {
		t.Fatal(err)
	}
	w := do(srv, "GET", fileURL)
	mustCode(t, w, http.StatusOK)
	firstETag := w.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("ETag missing on initial file serve")
	}

	mustWriteFile(t, canonPath, []byte("the rotated bytes"), 0o644)
	sameSecond := second.Add(500 * time.Millisecond)
	if err := os.Chtimes(canonPath, sameSecond, sameSecond); err != nil {
		t.Fatal(err)
	}

	w2 := do(srv, "GET", fileURL, [2]string{"If-None-Match", firstETag})
	if w2.Code == http.StatusNotModified {
		t.Fatal("a rewrite inside the same second served the cached copy")
	}
	mustCode(t, w2, http.StatusOK)
	if newETag := w2.Header().Get("ETag"); newETag == firstETag {
		t.Errorf("ETag unchanged across a same-second rewrite: %q", newETag)
	}
}

func TestSetSource_RenameLeavesCollectionAndKeepsURL(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_isolation.png", 10, 10)

	postSetSourceForm(t, srv, id, url.Values{"site": {"danbooru"}, "url": {"https://example.com/art"}})
	seedOrder := 3
	mustSetHome(t, srv.active().DB, id, "my set", &seedOrder)

	postSetSourceForm(t, srv, id, url.Values{"site": {"updated"}, "url": {"https://example.com/art"}, "prev_site": {"danbooru"}})

	var src, urlVal, collection string
	var order sql.NullInt64
	if err := srv.active().DB.Read.QueryRow(
		`SELECT source, url, series, series_order FROM images WHERE id = ?`, id,
	).Scan(&src, &urlVal, &collection, &order); err != nil {
		t.Fatal(err)
	}
	if src != "updated" {
		t.Errorf("source = %q, want updated", src)
	}
	if urlVal != "https://example.com/art" {
		t.Errorf("url = %q, want unchanged https://example.com/art", urlVal)
	}
	if collection != "my set" {
		t.Errorf("collection = %q, want unchanged 'my set'", collection)
	}
	if !order.Valid || order.Int64 != 3 {
		t.Errorf("collection_order = %v (valid=%v), want unchanged 3", order.Int64, order.Valid)
	}
}

func postSetSourceForm(t *testing.T, srv *Server, id int64, form url.Values) {
	t.Helper()
	w := postForm(t, srv, fmt.Sprintf("/images/%d/sources/set", id), form)
	if w.Code/100 != 2 && w.Code != http.StatusSeeOther {
		t.Fatalf("sources/set: %d %s", w.Code, w.Body.String())
	}
}

func imageSourceRows(t *testing.T, srv *Server, id int64) []string {
	t.Helper()
	rows, err := srv.active().DB.Read.Query(
		`SELECT site, post_id, url FROM image_sources WHERE image_id = ? ORDER BY rowid`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var site, post, u string
		if err := rows.Scan(&site, &post, &u); err != nil {
			t.Fatal(err)
		}
		out = append(out, site+"|"+post+"|"+u)
	}
	return out
}

func TestSetSource_LabelUnlabeledEditsInPlace(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_relabel.png", 10, 10)

	postSetSourceForm(t, srv, id, url.Values{"url": {"https://example.com/posts/1"}})
	postSetSourceForm(t, srv, id, url.Values{
		"site": {"danbooru"}, "url": {"https://example.com/posts/1"},
		"has_prev": {"1"}, "prev_site": {""}, "prev_post": {""},
	})

	got := imageSourceRows(t, srv, id)
	want := []string{"danbooru||https://example.com/posts/1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

func TestSetSource_AddKeepsUnlabeledSource(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_addkeep.png", 10, 10)

	postSetSourceForm(t, srv, id, url.Values{"url": {"https://example.com/original"}})
	postSetSourceForm(t, srv, id, url.Values{
		"site": {"gelbooru"}, "url": {"https://gelbooru.com/222"},
		"has_prev": {""}, "prev_site": {""}, "prev_post": {""},
	})

	got := imageSourceRows(t, srv, id)
	want := []string{"||https://example.com/original", "gelbooru||https://gelbooru.com/222"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

func TestSetCollection_RejectsEmptyLabel(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_so.png", 10, 10)

	form := url.Values{"collection": {""}, "collection_order": {"5"}}
	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/collections/set", id), form)

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "collection label required")
	if cols, _ := gallery.CollectionsForImage(srv.active().DB, id); len(cols) != 0 {
		t.Errorf("no membership should be created, got %#v", cols)
	}
}

func TestRemoveCollection_ClearsHome(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_clear.png", 10, 10)

	order := 5
	mustSetHome(t, srv.active().DB, id, "My Set", &order)

	clr := url.Values{"collection": {"My Set"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/collections/remove", id), clr)
	if w.Code/100 != 2 && w.Code != http.StatusSeeOther {
		t.Fatalf("remove collection: %d %s", w.Code, w.Body.String())
	}

	var collection string
	var ord sql.NullInt64
	if err := srv.active().DB.Read.QueryRow(
		`SELECT series, series_order FROM images WHERE id = ?`, id,
	).Scan(&collection, &ord); err != nil {
		t.Fatal(err)
	}
	if collection != "" {
		t.Errorf("collection = %q, want empty", collection)
	}
	if ord.Valid {
		t.Errorf("collection_order = %d (valid=true), want NULL", ord.Int64)
	}
	if cols, _ := gallery.CollectionsForImage(srv.active().DB, id); len(cols) != 0 {
		t.Errorf("memberships = %#v, want none", cols)
	}
}

func TestSetCollection_RejectsZeroOrNegativeOrder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_neg.png", 10, 10)

	for _, raw := range []string{"0", "-3"} {
		form := url.Values{"collection": {"My Set"}, "collection_order": {raw}}
		w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/collections/set", id), form)
		if w.Code != http.StatusOK {
			t.Fatalf("collection_order=%q expected 200, got %d: %s", raw, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "1 or higher") {
			t.Errorf("collection_order=%q body missing validation message: %s", raw, w.Body.String())
		}
	}
	if cols, _ := gallery.CollectionsForImage(srv.active().DB, id); len(cols) != 0 {
		t.Errorf("no membership should be created, got %#v", cols)
	}
}

func ratingTagIDWeb(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

func TestAddTagToImage_NamesAnUnknownCategoryPrefix(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "prefix_typo.png", 10, 10)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"artits:mucha"}})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "no category named artits")

	w = postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"artist:mucha"}})
	wantNotIn(t, w.Body.String(), "no category named")
}

func TestAddTagToImage_ReadsTheCategoryPrefixInAnyCase(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "prefix_case.png", 10, 10)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"Artist:Mucha"}})
	mustCode(t, w, http.StatusOK)
	wantNotIn(t, w.Body.String(), "no category named")
	if cat := imageTagCategory(t, srv, id, "mucha"); cat != "artist" {
		t.Errorf("mucha category = %q, want artist", cat)
	}
}

func TestAddTagToImage_CategoryPrefixOnlyRejected(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "prefix_only.png", 10, 10)

	form := url.Values{"tag": {"general: 1girl"}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), form)

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "empty tag name after category prefix")
	wantIn(t, body, "general:")
	if cat := imageTagCategory(t, srv, id, "1girl"); cat != "general" {
		t.Errorf("1girl category = %q, want general (token after rejected prefix should still apply)", cat)
	}
}

func TestDeleteAlias_FlashesConfirmation(t *testing.T) {
	srv := newTestServer(t)
	genID := srv.active().GeneralCategoryID
	canon := mustTag(t, srv.tagSvc(), "blonde_hair", genID)
	alias, err := srv.tagSvc().CreateAlias("blond_hair", genID, canon.ID)
	if err != nil {
		t.Fatal(err)
	}

	del := func(id int64) string {
		w := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d", id))
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete %d = %d", id, w.Code)
		}
		return w.Header().Get("HX-Trigger")
	}

	if trig := del(alias.ID); !strings.Contains(trig, "action-flash") || !strings.Contains(trig, "Alias removed") {
		t.Errorf("alias delete should flash a confirmation, HX-Trigger = %q", trig)
	}

	plain := mustTag(t, srv.tagSvc(), "standalone_tag", genID)
	if trig := del(plain.ID); strings.Contains(trig, "action-flash") {
		t.Errorf("plain tag delete should not flash, HX-Trigger = %q", trig)
	}
}

func TestTagsPage_AliasRowExposesCategorySelect(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	canon, _ := cx.TagSvc.GetOrCreateTag("alias_canon", generalID)
	src, _ := cx.TagSvc.GetOrCreateTag("alias_src", generalID)
	if err := cx.TagSvc.MergeTags(src.ID, canon.ID); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	body := mustGetBody(t, srv, "/tags?q=alias_src&show_zero=1&origin=alias")
	idx := strings.Index(body, `data-name="alias_src"`)
	if idx < 0 {
		t.Fatalf("alias row not in body: %s", body[:min(len(body), 600)])
	}
	rowStart := strings.LastIndex(body[:idx], "<tr")
	rowEnd := strings.Index(body[idx:], "</tr>")
	row := body[rowStart : idx+rowEnd]
	wantIn(t, row, `<select class="cat-select"`)

	wantIn(t, row, `hx-patch="/tags/`)
}

func TestTagsPage_ConflictViewsMarkTheNameOrderTheyList(t *testing.T) {
	srv := newTestServer(t)

	for _, view := range []string{"conflicts=1", "conflicts=distinct"} {
		body := do(srv, "GET", "/tags?sort=usage&order=desc&"+view).Body.String()
		wantIn(t, body, `Name<span class="sort-arrow">▲</span>`)
		wantNotIn(t, body, `Usage<span class="sort-arrow">`)
	}
	wantIn(t, do(srv, "GET", "/tags?sort=usage&order=desc").Body.String(), `Usage<span class="sort-arrow">▼</span>`)
}

func TestTagsPage_ClampsPastTheEndPage(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/tags?page=999")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if strings.Contains(body, "No tags found.") {
		t.Errorf("clamp failed: body still renders empty-state message: %s", body[:min(len(body), 600)])
	}

	if !strings.Contains(body, `data-name="general"`) && !strings.Contains(body, `data-name="explicit"`) {
		t.Errorf("clamp didn't surface any rating row; body: %s", body[:min(len(body), 600)])
	}
}

func TestTagsPage_PaginationKeepsAPlusAndAmpersandFilter(t *testing.T) {
	srv := newTestServer(t)
	genID := srv.active().GeneralCategoryID
	for i := range 101 {
		mustTag(t, srv.tagSvc(), fmt.Sprintf("c++&q_%03d", i), genID)
	}

	w := do(srv, "GET", "/tags?q="+url.QueryEscape("c++&q"))

	mustCode(t, w, http.StatusOK)
	next := nextPageLink(t, w.Body.String())
	if got := next.Query().Get("q"); got != "c++&q" {
		t.Errorf("next page q = %q, want %q (href %s)", got, "c++&q", next)
	}
	if got := next.Query().Get("page"); got != "2" {
		t.Errorf("next page = %q, want 2 (href %s)", got, next)
	}
	wantIn(t, do(srv, "GET", next.String()).Body.String(), "Page 2 / 2")
}

func nextPageLink(t *testing.T, body string) *url.URL {
	t.Helper()
	m := regexp.MustCompile(`<a href="([^"]*)">Next →</a>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Next link in the pagination")
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatalf("parse %q: %v", m[1], err)
	}
	return u
}

func TestTagsPage_LiveSearchAnswersTheListingOnly(t *testing.T) {
	srv := newTestServer(t)
	mustTag(t, srv.tagSvc(), "live_needle", srv.active().GeneralCategoryID)
	get := func(trigger string) string {
		req := httptest.NewRequest("GET", "/tags?q=live_need&show_zero=1", nil)
		req.Header.Set("HX-Request", "true")
		if trigger != "" {
			req.Header.Set("HX-Trigger", trigger)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		mustCode(t, w, http.StatusOK)
		return w.Body.String()
	}

	body := get("tags-q-input")
	wantIn(t, body, `id="tags-page"`)
	wantIn(t, body, `data-name="live_needle"`)
	wantNotIn(t, body, `class="filter-group"`)

	body = get("")
	wantIn(t, body, `data-name="live_needle"`)
	wantIn(t, body, `<span class="filter-label">Created</span>`)
}

func TestTagsPage_DiagnosticFiltersShowOnlyWithSomethingToList(t *testing.T) {
	srv := newTestServer(t)
	mustTag(t, srv.tagSvc(), "lone_tag", srv.active().GeneralCategoryID)

	body := do(srv, "GET", "/tags?show_zero=1").Body.String()
	for _, label := range []string{">Stale</span>", ">Conflicts</span>", ">Old spellings</span>"} {
		wantNotIn(t, body, label)
	}

	wantIn(t, do(srv, "GET", "/tags?show_zero=1&conflicts=1").Body.String(), ">Conflicts</span>")
}

func TestTagsPage_CategoryPrefixRedirectsToCatFilter(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/tags?q=character:")

	mustCode(t, w, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "cat=") || strings.Contains(loc, "q=character%3A") {
		t.Errorf("redirect Location = %q; want cat=<id> with q dropped", loc)
	}
}

func TestTagsPage_CategoryPrefixRedirectStaysOnSite(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Server.BindAddress = "0.0.0.0:8455" })

	w := do(srv, "GET", "http://nas.lan/tags?q=character:&page=2")

	mustCode(t, w, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/tags?") || !strings.Contains(loc, "page=2") {
		t.Errorf("redirect Location = %q; want a /tags?... path keeping page=2", loc)
	}
}

func TestTagsPage_RatingRowOmitsImmutableActions(t *testing.T) {
	srv := newTestServer(t)

	w := do(srv, "GET", "/tags?q=explicit&show_zero=1")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.Contains(body, `data-name="explicit"`) {
		t.Fatalf("rating row for `explicit` not in /tags?q=explicit response: %s", body[:min(len(body), 600)])
	}

	idx := strings.Index(body, `data-name="explicit"`)
	rowStart := strings.LastIndex(body[:idx], "<tr")
	rowEnd := strings.Index(body[idx:], "</tr>")
	if rowStart < 0 || rowEnd < 0 {
		t.Fatalf("could not isolate the explicit row in body")
	}
	row := body[rowStart : idx+rowEnd]
	for _, forbidden := range []string{
		`btn-rename-trigger" title="Rename this tag"`,
		`btn-merge-trigger"`,
		`<select class="cat-select"`,
	} {
		wantNotIn(t, row, forbidden)
	}

	wantIn(t, row, `btn-delete-tag"`)
	wantIn(t, row, `>Clear</button>`)
}

func TestDetailPage_DuplicateFilePathsHeaderReadsDuplicates(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "dup.png", 10, 10)
	cx := srv.active()

	dupPath := filepath.Join(cx.GalleryPath, "dup-alt.png")
	mustWriteFile(t, dupPath, []byte("dup"), 0o644)
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, dupPath)

	w := do(srv, "GET", fmt.Sprintf("/internal/images/%d/related-entries", id))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, `<h3>Duplicates</h3>`)
	wantNotIn(t, body, `<h3>Aliases</h3>`)
	wantIn(t, body, `duplicates-section`)
}

func TestDuplicatesPanel_HidesGoneFileAlias(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "live.png", 10, 10)
	cx := srv.active()
	ghost := filepath.Join(cx.GalleryPath, "moved-away.png")
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, ghost)

	w := do(srv, "GET", fmt.Sprintf("/internal/images/%d/related-entries", id))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantNotIn(t, body, "moved-away.png")
	wantNotIn(t, body, `<h3>Duplicates</h3>`)
}

func TestDuplicatesPanel_HidesACopyInAnotherGallerysFolder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "live.png", 10, 10)
	inner := filepath.Join(srv.active().GalleryPath, ".H")
	mustMkdirAll(t, inner, 0o755)
	seedAliasCopy(t, srv, id, filepath.Join(".H", "theirs.png"))
	seedAliasCopy(t, srv, id, "ours.png")

	related := func() string {
		w := do(srv, "GET", fmt.Sprintf("/internal/images/%d/related-entries", id))
		mustCode(t, w, http.StatusOK)
		return w.Body.String()
	}
	wantIn(t, related(), "theirs.png")
	addRemovableGallery(t, srv, "H", inner)
	body := related()
	wantNotIn(t, body, "theirs.png")
	wantIn(t, body, "ours.png")
}

func TestPromoteCanonical_RefusesMissingFile(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "real.png", 10, 10)
	cx := srv.active()
	ghost := filepath.Join(cx.GalleryPath, "ghost.png")
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, ghost)

	form := url.Values{"path": {ghost}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/canonical-path", id), form)
	mustCode(t, w, http.StatusBadRequest)

	if canonPath := canonicalPathOf(t, srv, id); !strings.HasSuffix(canonPath, "real.png") {
		t.Errorf("canonical_path = %q, want it left at real.png", canonPath)
	}
}

func TestPromoteCanonical_RefusesAnUntrackedPath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "real.png", 10, 10)
	stray := filepath.Join(t.TempDir(), "stray.png")
	mustWriteFile(t, stray, tinyPNG(t, 8, 8, color.RGBA{4, 5, 6, 255}), 0o644)

	form := url.Values{"path": {stray}}
	w := postForm(t, srv, fmt.Sprintf("/images/%d/canonical-path", id), form)
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "not an alias")
	wantNotIn(t, w.Body.String(), `class="flash`)
	hx := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/canonical-path", id), url.Values{"path": {stray}})
	mustCode(t, hx, http.StatusBadRequest)
	wantIn(t, hx.Header().Get("HX-Trigger"), `"text":"path is not an alias of this image"`)

	if canonPath := canonicalPathOf(t, srv, id); !strings.HasSuffix(canonPath, "real.png") {
		t.Errorf("canonical_path = %q, want it left at real.png", canonPath)
	}
}

func TestResetSkippedPost(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	ids := make([]int64, 8)
	for i := range ids {

		ids[i] = seedImage(t, srv, fmt.Sprintf("rs%d.png", i+1), 8+i, 8)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	mustExec(t, cx.DB.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?,?,3,?), (?,?,4,?)`, ids[0], ids[1], now, ids[2], ids[3], now)
	mustExec(t, cx.DB.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, skipped_at) VALUES (?,?,5,?,?), (?,?,6,?,?)`, ids[4], ids[5], now, now, ids[6], ids[7], now, now)

	form := url.Values{}
	w := postForm(t, srv, "/relations/reset-skipped", form)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "Reset 2 skipped pairs.")

	skipped := countRows(t, cx.DB, "potential_relation_pairs WHERE skipped_at IS NOT NULL")
	open := countRows(t, cx.DB, "potential_relation_pairs WHERE skipped_at IS NULL")
	if skipped != 0 {
		t.Errorf("after reset, skipped count = %d, want 0", skipped)
	}
	if open != 4 {
		t.Errorf("after reset, open count = %d, want 4 (2 original + 2 reset)", open)
	}
}

func TestTagMutationsEmitTagsChanged(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "trig.png", 10, 10)

	addTag := func(tag string) *httptest.ResponseRecorder {
		form := url.Values{"tag": {tag}}
		return postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), form)
	}

	if got := addTag("blonde_hair").Header().Get("HX-Trigger"); got != "tags-changed" {
		t.Errorf("add HX-Trigger = %q, want tags-changed", got)
	}
	if got := addTag("blonde_hair").Header().Get("HX-Trigger"); got != "" {
		t.Errorf("duplicate add HX-Trigger = %q, want empty", got)
	}

	w := deleteCSRF(t, srv, fmt.Sprintf("/images/%d/tags", id))
	if got := w.Header().Get("HX-Trigger"); got != "tags-changed" {
		t.Errorf("remove-all HX-Trigger = %q, want tags-changed", got)
	}
}

func TestBatchTagCategory_SkipsSameCategoryNoOps(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	generalID := cx.GeneralCategoryID
	metaID := categoryIDByName(t, srv, "meta")
	inGeneral, _ := cx.TagSvc.GetOrCreateTag("split_here", generalID)
	alreadyThere, _ := cx.TagSvc.GetOrCreateTag("split_here", metaID)

	w := postForm(t, srv, "/tags/batch-category", url.Values{
		"ids":         {fmt.Sprintf("%d,%d", inGeneral.ID, alreadyThere.ID)},
		"category_id": {fmt.Sprintf("%d", metaID)},
		"merge":       {"1"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("no job state after the run")
	}
	wantIn(t, state.Summary, "skipped 1")
	wantNotIn(t, state.Summary, "moved 2")
}

func TestSkipReasons_MarksATruncatedList(t *testing.T) {
	var r skipReasons
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		r.add(errors.New(msg))
	}
	if got, want := r.String(), "one; two; three; and 2 more"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestBatchTagAliasAndImply_CreateNothingWhenRefusedOrEmpty(t *testing.T) {
	srv := newTestServer(t)
	src := mustTag(t, srv.tagSvc(), "batch_source", srv.active().GeneralCategoryID)
	ids := strconv.FormatInt(src.ID, 10)
	posts := func(ids, alias, target string) []struct {
		path string
		form url.Values
	} {
		return []struct {
			path string
			form url.Values
		}{
			{"/tags/batch-alias", url.Values{"ids": {ids}, "canonical_id": {alias}}},
			{"/tags/batch-imply", url.Values{"ids": {ids}, "mode": {"add"}, "target": {target}}},
		}
	}

	mustStartJob(t, srv.jobs, models.JobTypeDelete)
	for _, p := range posts(ids, "busy_canon", "busy_target") {
		mustCode(t, postForm(t, srv, p.path, p.form), http.StatusConflict)
	}
	srv.jobs.Complete("done")
	for _, p := range posts("", "empty_canon", "empty_target") {
		mustCode(t, postForm(t, srv, p.path, p.form), http.StatusAccepted)
	}
	if srv.jobs.IsRunning() {
		t.Error("an empty selection claimed the job lane")
	}
	for _, name := range []string{"busy_canon", "busy_target", "empty_canon", "empty_target"} {
		if n := countRows(t, srv.db(), "tags WHERE name = ?", name); n != 0 {
			t.Errorf("%s rows = %d, want none", name, n)
		}
	}

	mustCode(t, postForm(t, srv, "/tags/batch-imply", url.Values{"ids": {ids}, "mode": {"add"}, "target": {"fresh_target"}}), http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if n := countRows(t, srv.db(), "tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id WHERE t.name = 'fresh_target'"); n != 1 {
		t.Errorf("edges to the new target = %d, want 1", n)
	}
}

func TestCreateAliasPost_ARefusedAliasCreatesNothing(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	carried := mustTag(t, cx.TagSvc, "carried_name", cx.GeneralCategoryID)
	mustAddTag(t, cx.TagSvc, seedImage(t, srv, "carried.png", 8, 8), carried.ID, false, nil)
	general := strconv.FormatInt(cx.GeneralCategoryID, 10)

	for _, name := range []string{"carried_name", "bad*name"} {
		w := postForm(t, srv, "/tags/aliases", url.Values{"name": {name}, "category_id": {general}, "canonical_id": {"new_canon"}})
		mustCode(t, w, http.StatusBadRequest)
	}
	if n := countRows(t, srv.db(), "tags WHERE name = 'new_canon'"); n != 0 {
		t.Fatalf("new_canon rows = %d after refused aliases, want none", n)
	}

	w := postForm(t, srv, "/tags/aliases", url.Values{"name": {"fresh_alias"}, "category_id": {general}, "canonical_id": {"new_canon"}})
	if w.Code >= 400 {
		t.Fatalf("accepted alias answered %d: %s", w.Code, w.Body.String())
	}
	if n := countRows(t, srv.db(), "tags a JOIN tags c ON c.id = a.canonical_tag_id WHERE a.name = 'fresh_alias' AND c.name = 'new_canon'"); n != 1 {
		t.Errorf("fresh_alias -> new_canon rows = %d, want 1", n)
	}
}

func TestBatchTagImply_ReDeclaringReAppliesTheEdge(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	parent := mustTag(t, cx.TagSvc, "reimply_parent", cx.GeneralCategoryID)
	target := mustTag(t, cx.TagSvc, "reimply_target", cx.GeneralCategoryID)
	imgID := seedImage(t, srv, "reimply.png", 8, 8)
	mustAddTag(t, cx.TagSvc, imgID, parent.ID, false, nil)
	mustAddImply(t, cx.TagSvc, parent.ID, target.ID)

	w := postForm(t, srv, "/tags/batch-imply", url.Values{
		"ids":    {strconv.FormatInt(parent.ID, 10)},
		"mode":   {"add"},
		"target": {"#" + strconv.FormatInt(target.ID, 10)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imgID, target.ID); n != 1 {
		t.Errorf("implied rows after re-declaring = %d, want 1", n)
	}
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "re-applied 1") {
		t.Errorf("summary = %q, want it to count the re-applied edge", got)
	}
}

func TestRunBatchTag_RemoveResolvesAnAliasAndTheCase(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	canon := mustTag(t, cx.TagSvc, "blue_sky", cx.GeneralCategoryID)
	mustCreateAlias(t, cx.TagSvc, "azure_sky", cx.GeneralCategoryID, canon.ID)

	for _, token := range []string{"azure_sky", "Blue_Sky"} {
		id := seedImage(t, srv, token+".png", 8, 8)
		mustAddTag(t, cx.TagSvc, id, canon.ID, false, nil)
		w := postForm(t, srv, "/internal/batch-tag", url.Values{
			"scope": {"selection"}, "op": {"remove"},
			"tags": {token},
			"ids":  {strconv.FormatInt(id, 10)},
		})
		mustCode(t, w, http.StatusAccepted)
		awaitJobsDrain(t, srv)

		if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, canon.ID); n != 0 {
			t.Errorf("%s: blue_sky still on the image", token)
		}
		wantIn(t, srv.jobs.Get().Summary, "(1 row change)")
	}
}
