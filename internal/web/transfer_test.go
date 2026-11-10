package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
)

func transferRequest(target string, extra url.Values) *http.Request {
	form := url.Values{"target": {target}}
	for k, v := range extra {
		form[k] = v
	}
	req := httptest.NewRequest("POST", "/images/1/transfer", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestTransferTarget_Rejections(t *testing.T) {
	srv := newMultiGalleryServer(t)
	srv.get("stock").Degraded = true
	cases := []struct{ name, target, wantMsg string }{
		{"empty", "", "Pick a target gallery."},
		{"same_gallery", "default", "must be a different gallery"},
		{"unknown", "ghost", "Unknown target gallery."},
		{"degraded", "stock", "target gallery is unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if _, _, ok := srv.transferTarget(w, transferRequest(tc.target, nil)); ok {
				t.Fatalf("target %q should be rejected", tc.target)
			}
			wantCode(t, w, http.StatusBadRequest)
			wantIn(t, w.Body.String(), tc.wantMsg)
		})
	}
}

func TestTransferTarget_AcceptsLiveOtherGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	w := httptest.NewRecorder()
	dst, removeAfter, ok := srv.transferTarget(w, transferRequest("stock", url.Values{"remove_after": {"1"}}))
	if !ok {
		t.Fatalf("a live other gallery should be accepted: %s", w.Body.String())
	}
	if dst == nil || dst.Name != "stock" {
		t.Errorf("dst = %v, want stock", dst)
	}
	if !removeAfter {
		t.Error("remove_after=1 should parse as a move")
	}
}

func TestRunBatchTransfer_IsolatesFailures(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.get("default"), srv.get("stock")
	good := seedRichImage(t, src)

	mustStartJob(t, srv.jobs, models.JobTypeTransfer)

	srv.runBatchTransfer([]int64{good.ID, 999999}, dst, false)

	n := countRows(t, dst.DB, "images WHERE sha256 = ?", good.SHA256)
	if n != 1 {
		t.Errorf("good image landed %d times, want 1", n)
	}
	if s := srv.jobs.Get().Summary; !strings.Contains(s, "Transferred 1") || !strings.Contains(s, "1 failed") {
		t.Errorf("summary = %q, want one transferred and one failed", s)
	}
}

func TestRunBatchTransfer_Cancellation(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.get("default"), srv.get("stock")
	good := seedRichImage(t, src)

	mustStartJob(t, srv.jobs, models.JobTypeTransfer)
	srv.jobs.Cancel()
	srv.runBatchTransfer([]int64{good.ID}, dst, false)

	if s := srv.jobs.Get().Summary; !strings.Contains(s, "cancelled") {
		t.Errorf("summary = %q, want a cancelled report", s)
	}
	n := countRows(t, dst.DB, "images WHERE sha256 = ?", good.SHA256)
	if n != 0 {
		t.Errorf("cancelled batch transferred %d, want 0", n)
	}
}

func seedRichImage(t *testing.T, cx *library.Gallery) *models.Image {
	t.Helper()
	p := filepath.Join(cx.GalleryPath, "sub", "seed.png")
	mustMkdirAll(t, filepath.Dir(p), 0o755)
	mustWriteFile(t, p, makePNGBytes(t, 8, 8, 5, 6, 7), 0o644)
	img, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "upload")
	cat, err := cx.TagSvc.CreateCategory("mycat", "#123456")
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	tg, err := cx.TagSvc.GetOrCreateTag("special", cat.ID)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := cx.TagSvc.AddTagsToImageFromTagger(img.ID, []int64{tg.ID, ratingTagIDWeb(t, cx.DB, "explicit")}, false, ""); err != nil {
		t.Fatalf("add tags: %v", err)
	}
	srcTag, err := cx.TagSvc.GetOrCreateTag("fromdanbooru", galleryio.LookupCategoryID(cx.DB, "general"))
	if err != nil {
		t.Fatalf("create source tag: %v", err)
	}
	if err := cx.TagSvc.AddTagsToImageFromTagger(img.ID, []int64{srcTag.ID}, false, "danbooru"); err != nil {
		t.Fatalf("add source tag: %v", err)
	}
	if err := gallery.AddSourceMembership(cx.DB, img.ID, "danbooru", "123", "https://example.test/posts/123"); err != nil {
		t.Fatalf("add source: %v", err)
	}
	if err := gallery.SetSourceCommentary(cx.DB, img.ID, "danbooru", "123", "artist words"); err != nil {
		t.Fatalf("set commentary: %v", err)
	}
	if err := gallery.ReplaceSourceAnnotations(cx.DB, img.ID, "danbooru", "123",
		[]models.Annotation{{Site: "danbooru", PostID: "123", X: 1, Y: 2, W: 3, H: 4, Body: "a box"}}); err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if err := gallery.AddManualAnnotation(cx.DB, img.ID, 5, 6, 7, 8, "my box"); err != nil {
		t.Fatalf("manual annotation: %v", err)
	}
	mustExec(t, cx.DB.Write, `UPDATE images SET note = 'mynote', is_favorited = 1, is_inbox = 0 WHERE id = ?`, img.ID)
	return img
}
