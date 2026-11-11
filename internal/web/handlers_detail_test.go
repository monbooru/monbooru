package web

import (
	"bytes"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/markup"
	"github.com/monbooru/monbooru/internal/models"
)

func TestDetailPage_NamesTheRefusedPreview(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	oversize := filepath.Join(cx.GalleryPath, "huge.png")
	mustWriteFile(t, oversize, makePNGHeader(50000, 50000), 0o644)
	refused, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, oversize, "")

	body := detailBody(t, srv, refused.ID)
	wantIn(t, body, "exceeds the 3 GiB decode cap")
	wantNotIn(t, body, "[backfill]")

	hashable := seedImage(t, srv, "fine.png", 10, 10)
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = NULL WHERE id = ?`, hashable)
	if body := detailBody(t, srv, hashable); !strings.Contains(body, "[backfill]") {
		t.Error("[backfill] missing on a row whose thumbnail is present")
	}
}

func TestDetailPage_NamesWhatAnFFmpegStillNeeds(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	src := filepath.Join(cx.GalleryPath, "broken.avif")
	mustWriteFile(t, src, avifHeader, 0o644)
	img, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, src, "")

	want := "none - ffmpeg could not decode it"
	if !ffmpegOnPath() {
		want = "none - needs ffmpeg"
	}
	wantIn(t, detailBody(t, srv, img.ID), want)
}

func TestDetailPage_NamesWhatAVideoOrACbzCoverNeeds(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	ffmpegNote := "none - ffmpeg could not decode it"
	if !ffmpegOnPath() {
		ffmpegNote = "none - needs ffmpeg"
	}

	src := filepath.Join(cx.GalleryPath, "broken.mp4")
	mustWriteFile(t, src, append([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2"), make([]byte, 64)...), 0o644)
	video, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, src, "")
	wantIn(t, detailBody(t, srv, video.ID), ffmpegNote)

	avifCover := seedMangaEntries(t, srv, "avif.cbz", []mangaEntry{{"01.avif", avifHeader}})
	wantIn(t, detailBody(t, srv, avifCover), ffmpegNote)

	hugeCover := seedMangaEntries(t, srv, "huge.cbz", []mangaEntry{{"01.png", makePNGHeader(50000, 50000)}})
	wantIn(t, detailBody(t, srv, hugeCover), "none - image 50000x50000 exceeds the 3 GiB decode cap")
}

func detailBody(t *testing.T, srv *Server, id int64) string {
	t.Helper()
	w := do(srv, "GET", fmt.Sprintf("/images/%d", id))
	return w.Body.String()
}

func TestDetailPage_HoldsTheImageBoxBeforeItLoads(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	wantIn(t, detailBody(t, srv, seedImage(t, srv, "wide.png", 40, 10)), `<div class="detail-media-frame detail-media-sized" style="--w:40;--h:10">`)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 40, 10)), nil); err != nil {
		t.Fatal(err)
	}
	exif := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	app1 := append([]byte{0xff, 0xe1, 0, byte(len(exif) + 2)}, exif...)
	turned := filepath.Join(cx.GalleryPath, "turned.jpg")
	mustWriteFile(t, turned, slices.Concat(buf.Bytes()[:2], app1, buf.Bytes()[2:]), 0o644)
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, turned, "")
	wantIn(t, detailBody(t, srv, rec.ID), `style="--w:10;--h:40"`)

	wantNotIn(t, detailBody(t, srv, rec.ID), "background-image")

	unsized := seedImage(t, srv, "unsized.png", 10, 10)
	mustExec(t, cx.DB.Write, `UPDATE images SET width = NULL, height = NULL WHERE id = ?`, unsized)
	wantIn(t, detailBody(t, srv, unsized), `<div class="detail-media-frame">`)
}

func TestDetailPage_DrawsTheThumbnailWhileTheImageLoads(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	id := seedImage(t, srv, "wide.png", 40, 10)
	wantIn(t, detailBody(t, srv, id), fmt.Sprintf(`style="background-image:url('/thumbnails/default/%d.jpg');color:transparent" onload="this.style.backgroundImage='none'"`, id))

	if err := os.Remove(gallery.ThumbnailPath(cx.ThumbnailsPath, id)); err != nil {
		t.Fatal(err)
	}
	wantNotIn(t, detailBody(t, srv, id), "background-image")
}

func TestDetailPage_HoldsTheVideoBoxAndPostersItsThumbnail(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	src := filepath.Join(cx.GalleryPath, "clip.mp4")
	mustWriteFile(t, src, append([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2"), make([]byte, 64)...), 0o644)
	video, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, src, "")
	mustExec(t, cx.DB.Write, `UPDATE images SET width = 1920, height = 1080 WHERE id = ?`, video.ID)
	thumb := gallery.ThumbnailPath(cx.ThumbnailsPath, video.ID)
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, thumb, []byte("jpeg"), 0o644)

	body := detailBody(t, srv, video.ID)
	wantIn(t, body, `<div class="detail-media-frame detail-media-video detail-media-sized" style="--w:1920;--h:1080">`)
	wantIn(t, body, fmt.Sprintf(`poster="/thumbnails/default/%d.jpg"`, video.ID))

	if err := os.Remove(thumb); err != nil {
		t.Fatal(err)
	}
	wantNotIn(t, detailBody(t, srv, video.ID), "poster=")
}

func TestBuildAnnotationViews_ClampsToImageBounds(t *testing.T) {
	w, h := 40, 30
	img := &models.Image{Width: &w, Height: &h}
	views := buildAnnotationViews(img, []models.Annotation{
		{X: 10, Y: 20, W: 100, H: 40, Body: "overflow"},
		{X: -5, Y: -5, W: 10, H: 10, Body: "negative"},
	}, markup.NewRefs())
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}
	if got, want := string(views[0].Style), "left:25.0000%;top:66.6667%;width:75.0000%;height:33.3333%"; got != want {
		t.Errorf("oversized box style = %q, want %q (clamped to the far edge)", got, want)
	}
	if got, want := string(views[1].Style), "left:0.0000%;top:0.0000%;width:25.0000%;height:33.3333%"; got != want {
		t.Errorf("negative box style = %q, want %q (floored at the origin)", got, want)
	}
}

func TestMD5Cell_DefersARowOverTheSizeCap(t *testing.T) {
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		cfg.Gallery.MaxFileSizeMB = 1
	})
	id := seedImage(t, srv, "small.png", 10, 10)
	mustExec(t, srv.db().Write, `UPDATE images SET md5 = '' WHERE id = ?`, id)

	cell := func(id int64) string {
		w := do(srv, "GET", fmt.Sprintf("/internal/images/%d/md5", id))
		return w.Body.String()
	}

	if got := cell(id); !strings.Contains(got, "data-copy=") {
		t.Errorf("under the cap the cell should carry the digest, got %q", got)
	}

	mustExec(t, srv.db().Write, `UPDATE images SET md5 = '', file_size = ? WHERE id = ?`, 2*1024*1024, id)
	got := cell(id)
	wantIn(t, got, "over the size limit")
	var stored string
	if err := srv.db().Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Errorf("the deferred cell hashed the file anyway: md5 = %q", stored)
	}
}

func TestMD5Cell_DropsCachedSearches(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "a.png", 10, 10)
	mustExec(t, srv.db().Write, `UPDATE images SET md5 = '' WHERE id = ?`, id)
	if got := galleryResultCount(t, srv, "-md5:"); got != "1" {
		t.Fatalf("-md5: = %s before the cell, want 1", got)
	}
	mustCode(t, do(srv, "GET", fmt.Sprintf("/internal/images/%d/md5", id)), http.StatusOK)
	if got := galleryResultCount(t, srv, "-md5:"); got != "0" {
		t.Errorf("-md5: = %s after the cell stored the digest, want 0", got)
	}
}

func TestDeleteConfirmNamesTheDedupedCopies(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "dedup_one.png", 10, 10)

	body := detailBody(t, srv, id)
	wantNotIn(t, body, "Deletes 2 files")

	copyPath := filepath.Join(srv.active().GalleryPath, "shapes", "tiny.png")
	mustMkdirAll(t, filepath.Dir(copyPath), 0o755)
	mustWriteFile(t, copyPath, []byte("x"), 0o644)
	mustExec(t, srv.db().Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, copyPath)
	body = detailBody(t, srv, id)
	wantIn(t, body, "Deletes 2 files: this one and 1 identical copy filed elsewhere.")
}

func TestDetailPage_TitlesARootFileByItsNameAlone(t *testing.T) {
	srv := newTestServer(t)
	root := seedImage(t, srv, "root-probe.png", 10, 10)
	if !strings.Contains(detailBody(t, srv, root), "<title>root-probe.png - ") {
		t.Errorf("root-level image is not titled by its filename alone")
	}

	foldered := seedImage(t, srv, "in-folder.png", 12, 12)
	folder := "misc"
	if _, err := gallery.PlaceImage(srv.active().DB, srv.active().Boundary(), foldered, &folder, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detailBody(t, srv, foldered), "<title>misc/in-folder.png - ") {
		t.Errorf("foldered image lost its folder prefix")
	}
}

func TestDetailPage_NamesTheImageWhoseBytesItsFileNowHolds(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 12, 12)
	wantNotIn(t, detailBody(t, srv, a), "now holds the bytes of")

	mustExec(t, cx.DB.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, b)
	if err := os.Rename(filepath.Join(cx.GalleryPath, "b.png"), filepath.Join(cx.GalleryPath, "a.png")); err != nil {
		t.Fatal(err)
	}
	wantIn(t, detailBody(t, srv, a), fmt.Sprintf(`now holds the bytes of <a href="/images/%d">image #%d</a>`, b, b))
}

func TestDetailDeleteConfirm_RewrittenFileSaysTheFileStays(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 12, 12)
	mustExec(t, cx.DB.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, b)
	if err := os.Rename(filepath.Join(cx.GalleryPath, "b.png"), filepath.Join(cx.GalleryPath, "a.png")); err != nil {
		t.Fatal(err)
	}
	stays := fmt.Sprintf("The file stays on disk: it holds image #%d's bytes, and the next sync gives it to #%d. This cannot be undone.", b, b)

	body := detailBody(t, srv, a)
	wantIn(t, body, stays)
	wantNotIn(t, body, "Files are permanently deleted from disk")

	w := postHTMXForm(t, srv, fmt.Sprintf("/images/%d/tags", a), url.Values{"tag": {"after_rewrite"}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), stays)
	wantNotIn(t, w.Body.String(), "Files are permanently deleted from disk")
}

func ffmpegOnPath() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func TestRelatedEntries_CollectionStepsWalkTheCollection(t *testing.T) {
	srv := newTestServer(t)
	name := `Saga "two"`
	var ids []int64
	for i := 1; i <= 3; i++ {
		id := seedImage(t, srv, fmt.Sprintf("s%d.png", i), 10+i, 10)
		mustAddCollection(t, srv.db(), id, name, &i)
		ids = append(ids, id)
	}
	lone := seedImage(t, srv, "lone.png", 20, 20)
	mustAddCollection(t, srv.db(), lone, "Solo", nil)

	w := do(srv, "GET", fmt.Sprintf("/internal/images/%d/related-entries?back_q=1girl&back_sort=newest", ids[1]),
		[2]string{"Cookie", "monbooru_page_size=20"})
	mustCode(t, w, http.StatusOK)
	body := html.UnescapeString(w.Body.String())
	back := url.Values{"back_q": {`collection:"Saga \"two\""`}, "back_sort": {"order"}, "back_order": {"asc"}, "back_page": {"1"}}
	for i, step := range []struct {
		id  int64
		dir string
	}{{ids[0], "prev"}, {ids[2], "next"}} {
		v := maps.Clone(back)
		v.Set("back_idx", strconv.Itoa(2*i))
		wantIn(t, body, fmt.Sprintf(`data-dir="%s" href="/images/%d?%s"`, step.dir, step.id, v.Encode()))
	}
	wantIn(t, body, `<span class="coll-nav-count">2 / 3</span>`)
	wantNotIn(t, body, "?ref=")

	w = do(srv, "GET", fmt.Sprintf("/internal/images/%d/related-entries", ids[2]))
	wantIn(t, w.Body.String(), `<span class="coll-step-off" aria-disabled="true">[next &rsaquo;]</span>`)

	w = do(srv, "GET", fmt.Sprintf("/internal/images/%d/related-entries", lone))
	wantNotIn(t, w.Body.String(), "Same collection")
}
