package web

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/relations"
)

func makeUploadReq(t *testing.T, srv *Server, files map[string][]byte, folder string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, contents := range files {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if folder != "" {
		mustWriteField(t, mw, "folder", folder)
	}
	mustWriteField(t, mw, "_csrf", srv.sessions.CSRFToken("anon"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	return req
}

func uploadFiles(t *testing.T, srv *Server, files map[string][]byte, folder string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, makeUploadReq(t, srv, files, folder))
	return w
}

func makePNGHeader(w, h int) []byte {
	ihdr := []byte("IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 2, 0, 0, 0)

	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(ihdr)-4))
	buf.Write(ihdr)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(ihdr))
	return buf.Bytes()
}

func makePNGBytes(t *testing.T, w, h int, r, g, b uint8) []byte {
	t.Helper()
	plain := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(plain, plain.Bounds(), image.NewUniform(color.RGBA{r, g, b, 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, plain); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUploadPost_ReportsFilesLeftWithoutAPreview(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"huge.png": makePNGHeader(50000, 50000),
		"ok.png":   makePNGBytes(t, 16, 16, 10, 20, 30),
	}, "")

	body := w.Body.String()
	if !strings.Contains(body, "2 added") || !strings.Contains(body, "1 without a preview") {
		t.Errorf("want both counts in the flash, got: %s", body)
	}
}

var (
	avifHeader = append([]byte{0x00, 0x00, 0x00, 0x1C, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0}, make([]byte, 64)...)
	jxlHeader  = append([]byte{0xFF, 0x0A}, make([]byte, 64)...)
)

func TestUploadPost_TakesAVIFAndJPEGXL(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"a.avif": avifHeader,
		"b.jxl":  jxlHeader,
	}, "")
	if body := w.Body.String(); !strings.Contains(body, "2 added") {
		t.Fatalf("want both files added, got: %s", body)
	}

	got, err := db.QueryStrings(srv.db().Read, `SELECT file_type FROM images ORDER BY canonical_path`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "avif,jxl" {
		t.Errorf("file types = %v, want avif, jxl", got)
	}
}

func TestUploadPost_IngestsSinglePNG(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"shot.png": makePNGBytes(t, 16, 16, 10, 20, 30),
	}, "")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "1 added")
	cx := srv.active()
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "shot.png")); err != nil {
		t.Errorf("uploaded file not on disk: %v", err)
	}
	count := countRows(t, cx.DB, "images")
	if count != 1 {
		t.Errorf("image count = %d, want 1", count)
	}
}

func TestUploadPost_IngestsIntoSubfolder(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"nested.png": makePNGBytes(t, 12, 12, 50, 50, 50),
	}, "2026/april")

	mustCode(t, w, http.StatusOK)
	cx := srv.active()
	want := filepath.Join(cx.GalleryPath, "2026", "april", "nested.png")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at %s: %v", want, err)
	}
	var folderPath string
	_ = cx.DB.Read.QueryRow(`SELECT folder_path FROM images LIMIT 1`).Scan(&folderPath)
	if folderPath != "2026/april" {
		t.Errorf("folder_path = %q, want 2026/april", folderPath)
	}
}

func TestUploadPost_DefaultUploadFolderWhenBlank(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Gallery.DefaultUploadFolder = "incoming"

	w := uploadFiles(t, srv, map[string][]byte{
		"a.png": makePNGBytes(t, 12, 12, 10, 20, 30),
	}, "")
	mustCode(t, w, http.StatusOK)
	cx := srv.active()
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "incoming", "a.png")); err != nil {
		t.Errorf("file not under the default folder: %v", err)
	}
	var folderPath string
	_ = cx.DB.Read.QueryRow(`SELECT folder_path FROM images WHERE canonical_path LIKE '%a.png'`).Scan(&folderPath)
	if folderPath != "incoming" {
		t.Errorf("folder_path = %q, want incoming", folderPath)
	}

	w2 := uploadFiles(t, srv, map[string][]byte{
		"b.png": makePNGBytes(t, 12, 12, 40, 50, 60),
	}, "elsewhere")
	mustCode(t, w2, http.StatusOK)
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "elsewhere", "b.png")); err != nil {
		t.Errorf("explicit folder ignored: %v", err)
	}
}

func TestUploadPost_DuplicateFilenameAutoSuffix(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"dup.png": makePNGBytes(t, 8, 8, 1, 1, 1),
	}, "")
	mustCode(t, w, http.StatusOK)

	w2 := uploadFiles(t, srv, map[string][]byte{
		"dup.png": makePNGBytes(t, 8, 8, 200, 200, 200),
	}, "")
	mustCode(t, w2, http.StatusOK)

	cx := srv.active()
	want := filepath.Join(cx.GalleryPath, "dup_1.png")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("auto-suffixed file missing: %v", err)
	}
}

func TestUploadPost_FoldsANameAFilesystemRefuses(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"a:b?.png": makePNGBytes(t, 8, 8, 1, 1, 1),
	}, "")
	mustCode(t, w, http.StatusOK)

	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "a_b_.png")); err != nil {
		t.Errorf("folded name missing: %v", err)
	}
}

func TestUploadPost_SameShaReturnsDuplicate(t *testing.T) {
	srv := newTestServer(t)
	png := makePNGBytes(t, 8, 8, 1, 1, 1)
	uploadFiles(t, srv, map[string][]byte{"x.png": png}, "")
	var id int64
	if err := srv.active().DB.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("look up ingested image: %v", err)
	}

	w := uploadFiles(t, srv, map[string][]byte{"y.png": png}, "")
	body := w.Body.String()
	wantIn(t, body, "duplicate")
	want := fmt.Sprintf(`<a href="/images/%d">#%d</a>`, id, id)
	wantIn(t, body, want)

	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "y.png")); !os.IsNotExist(err) {
		t.Errorf("duplicate upload left its copy on disk (err=%v)", err)
	}
	paths := countRows(t, srv.active().DB, "image_paths WHERE image_id = ?", id)
	if paths != 1 {
		t.Errorf("image_paths rows = %d, want 1 (canonical only)", paths)
	}
}

func TestUploadPost_RestoringTheCanonicalFileKeepsIt(t *testing.T) {
	srv := newTestServer(t)
	png := makePNGBytes(t, 8, 8, 3, 3, 3)
	uploadFiles(t, srv, map[string][]byte{"back.png": png}, "")

	var id int64
	var canonPath string
	if err := srv.active().DB.Read.QueryRow(`SELECT id, canonical_path FROM images ORDER BY id LIMIT 1`).Scan(&id, &canonPath); err != nil {
		t.Fatalf("look up ingested image: %v", err)
	}
	if err := os.Remove(canonPath); err != nil {
		t.Fatal(err)
	}

	w := uploadFiles(t, srv, map[string][]byte{filepath.Base(canonPath): png}, "")
	mustCode(t, w, http.StatusOK)
	if _, err := os.Stat(canonPath); err != nil {
		t.Fatalf("the upload unlinked the file it restored: %v", err)
	}
	if paths := countRows(t, srv.active().DB, "image_paths WHERE image_id = ?", id); paths != 1 {
		t.Errorf("image_paths rows = %d, want 1 (canonical only)", paths)
	}
}

func TestUploadPost_RejectsAbsoluteFolder(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"x.png": makePNGBytes(t, 8, 8, 0, 0, 0),
	}, "/etc/passwd")

	body := w.Body.String()
	wantIn(t, body, "relative to the gallery root")

	cx := srv.active()
	count := countRows(t, cx.DB, "images")
	if count != 0 {
		t.Errorf("images table should be empty after rejected upload, got %d", count)
	}
}

func TestUploadPost_RejectsTraversalFolder(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"x.png": makePNGBytes(t, 8, 8, 0, 0, 0),
	}, "../escape")

	body := w.Body.String()
	wantIn(t, body, "escapes the gallery root")
	cx := srv.active()
	if _, err := os.Stat(filepath.Join(filepath.Dir(cx.GalleryPath), "escape")); !os.IsNotExist(err) {
		t.Error("traversal attempt should not have created the escape directory")
	}
}

func TestUploadPost_RejectsNestedTraversal(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, map[string][]byte{
		"x.png": makePNGBytes(t, 8, 8, 0, 0, 0),
	}, "ok/../../escape")

	wantIn(t, w.Body.String(), "escapes the gallery root")
}

func TestUploadPost_NoFilesRejected(t *testing.T) {
	srv := newTestServer(t)
	w := uploadFiles(t, srv, nil, "")

	wantIn(t, w.Body.String(), "No files selected")
}

func TestUploadPost_UnsupportedTypeCountsAsError(t *testing.T) {
	for _, name := range []string{"malicious.exe", "clip.mp4"} {
		srv := newTestServer(t)
		w := uploadFiles(t, srv, map[string][]byte{
			name: []byte("MZ\x90\x00" + strings.Repeat("\x00", 32)),
		}, "")

		body := w.Body.String()
		wantIn(t, body, "1 error: unsupported file type")

		cx := srv.active()
		if _, err := os.Stat(filepath.Join(cx.GalleryPath, name)); !os.IsNotExist(err) {
			t.Errorf("unsupported-type file %s should be removed from disk", name)
		}
	}
}

func TestUploadErrorReasons(t *testing.T) {
	tests := []struct {
		unsupported, ignored, unsaved int
		refused                       []string
		want                          string
	}{
		{1, 0, 0, nil, "unsupported file type"},
		{9, 0, 0, nil, "unsupported file type"},
		{0, 0, 1, nil, "could not be saved"},
		{0, 1, 0, nil, "on the ignore list"},
		{2, 0, 3, nil, "2 unsupported file types, 3 could not be saved"},
		{1, 2, 0, nil, "1 unsupported file type, 2 on the ignore list"},
		{1, 0, 0, []string{"notes.txt"}, "unsupported file type (notes.txt)"},
		{0, 0, 2, []string{"a&b.png", "c.png"}, "could not be saved (a&amp;b.png, c.png)"},

		{4, 0, 0, []string{"a", "b", "c", "d"}, "unsupported file type"},
	}
	for _, tt := range tests {
		if got := uploadErrorReasons(tt.unsupported, tt.ignored, tt.unsaved, tt.refused); got != tt.want {
			t.Errorf("uploadErrorReasons(%d, %d, %d, %v) = %q, want %q", tt.unsupported, tt.ignored, tt.unsaved, tt.refused, got, tt.want)
		}
	}
}

func TestUploadPost_ManyFailuresStayOnOneReasonLine(t *testing.T) {
	srv := newTestServer(t)
	files := map[string][]byte{}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("a-fairly-long-rejected-filename-%02d.exe", i)] = []byte("MZ\x90\x00" + strings.Repeat("\x00", 32))
	}
	w := uploadFiles(t, srv, files, "")

	body := w.Body.String()
	wantIn(t, body, "30 errors: unsupported file type")
	for name := range files {
		wantNotIn(t, body, name)
	}
}

func TestUploadPost_NamesTheFileItSkippedForSize(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Gallery.MaxFileSizeMB = 1
	srv.cfgMu.Unlock()

	w := uploadFiles(t, srv, map[string][]byte{
		"ok.png":  makePNGBytes(t, 16, 16, 10, 20, 30),
		"big.png": bytes.Repeat([]byte{7}, 2<<20),
	}, "")

	body := w.Body.String()
	if !strings.Contains(body, "1 added") || !strings.Contains(body, "1 skipped over 1 MB (big.png)") {
		t.Errorf("want the added count and the skipped file named, got: %s", body)
	}
}

func TestUpload_RefusesANameOnTheIgnoreList(t *testing.T) {
	srv := newTestServer(t)

	w := uploadFiles(t, srv, map[string][]byte{
		"._shot.png": makePNGBytes(t, 9, 9, 1, 2, 3),
	}, "")

	wantIn(t, w.Body.String(), "0 added, 1 error: on the ignore list (._shot.png)")
	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "._shot.png")); err == nil {
		t.Error("a file named on the ignore list was written into the gallery")
	}
}

func TestUploadPost_EnforcesMaxFileSize(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Gallery.MaxFileSizeMB = 1
	srv.cfgMu.Unlock()

	w := uploadFiles(t, srv, map[string][]byte{
		"big.png": bytes.Repeat([]byte{7}, 2<<20),
	}, "")

	body := w.Body.String()
	wantIn(t, body, "flash-err")
	wantIn(t, body, "0 added, 1 skipped over 1 MB (big.png)")
	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "big.png")); err == nil {
		t.Error("oversize file should not have been written to disk")
	}
}

func TestCSRFMiddleware_RejectsMultipartWithoutHeader(t *testing.T) {
	srv := newTestServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mustWriteField(t, mw, "_csrf", srv.sessions.CSRFToken("anon"))
	mustWriteField(t, mw, "name", "newgal")
	mustWriteField(t, mw, "gallery_path", t.TempDir())
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/settings/galleries", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	wantCode(t, w, http.StatusForbidden)
}

func builtTree(t *testing.T, srv *Server) *relations.BKTree {
	t.Helper()
	cx := srv.active()
	tree := cx.Source().Tree
	if err := tree.EnsureBuilt(cx.DB); err != nil {
		t.Fatalf("build BK-tree: %v", err)
	}
	return tree
}

func treeHolds(t *testing.T, srv *Server, tree *relations.BKTree, id int64) bool {
	t.Helper()
	var phash sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&phash); err != nil || !phash.Valid {
		t.Fatalf("phash of image %d: %v (set: %t)", id, err, phash.Valid)
	}
	ids, _ := tree.SearchWithinDistance(phash.Int64, 0)
	return slices.Contains(ids, id)
}

func TestUploadPost_FeedsABuiltBKTree(t *testing.T) {
	srv := newTestServer(t)
	tree := builtTree(t, srv)
	w := uploadFiles(t, srv, map[string][]byte{
		"shot.png": makePNGBytes(t, 32, 32, 10, 200, 30),
	}, "")
	mustCode(t, w, http.StatusOK)

	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !treeHolds(t, srv, tree, id) {
		t.Error("an uploaded image is missing from the built BK-tree")
	}
}

func TestUploadLetsASwitchThroughAndCountsTheInboxItLandedIn(t *testing.T) {
	srv := newMultiGalleryServer(t)
	files := map[string][]byte{}
	for i := range 50 {
		files[fmt.Sprintf("f%02d.png", i)] = makePNGBytes(t, 64, 64, uint8(i*5), uint8(255-i*5), uint8(i))
	}
	req := makeUploadReq(t, srv, files, "")
	w := httptest.NewRecorder()
	uploaded := make(chan struct{})
	go func() {
		srv.uploadPost(w, req)
		close(uploaded)
	}()

	original := srv.get("default")
	landed := func(g string) int {
		return countRows(t, srv.get(g).DB, "images")
	}
	for landed("default") == 0 {
		select {
		case <-uploaded:
			t.Fatal("the upload finished before the switch could be asked for; the fixture is too small")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if err := srv.switchGallery("stock"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-uploaded:
		t.Error("the switch waited for the whole upload")
	default:
	}
	<-uploaded

	wantIn(t, w.Body.String(), "50 added")
	wantIn(t, w.Body.String(), `<span id="inbox-nav-count" hx-swap-oob="true"> (50)</span>`)
	if srv.get("default") != original || landed("default") != 50 || landed("stock") != 0 {
		t.Errorf("default holds %d, stock %d; want all 50 in the gallery the upload began in", landed("default"), landed("stock"))
	}
}

func TestUploadPost_ARefusedNameCreatesNoFolder(t *testing.T) {
	srv := newTestServer(t)
	uploadFiles(t, srv, map[string][]byte{"._x.png": makePNGBytes(t, 12, 12, 1, 2, 3)}, "newdir")

	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "newdir")); !os.IsNotExist(err) {
		t.Errorf("the refused upload left its folder behind: %v", err)
	}
	if n := countRows(t, srv.db(), "images"); n != 0 {
		t.Errorf("%d image(s) landed under an ignored name", n)
	}
}
