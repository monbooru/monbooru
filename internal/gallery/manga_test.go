package gallery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureMangaPage_HitAndMiss(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic, "03.png": pic,
	})
	thumbDir := t.TempDir()

	page2, err := EnsureMangaPage(thumbDir, cbz, 42, 2)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	st1 := mustStat(t, page2)

	past := time.Now().Add(-2 * time.Minute)
	mustChtimes(t, page2, past, past)

	page2Again, err := EnsureMangaPage(thumbDir, cbz, 42, 2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if page2Again != page2 {
		t.Errorf("second call returned %q, want %q", page2Again, page2)
	}
	st2 := mustStat(t, page2Again)
	if !st2.ModTime().After(st1.ModTime()) {
		t.Errorf("mtime after hit did not advance")
	}
}

func TestEnsureMangaPage_OutOfRange(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	thumbDir := t.TempDir()
	if _, err := EnsureMangaPage(thumbDir, cbz, 1, 2); err == nil {
		t.Error("expected error for page 2 of a 1-page archive")
	}
}

func TestEnsureMangaPageThumb_GeneratesAndCaches(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 64, 32, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	thumbDir := t.TempDir()

	thumb1, err := EnsureMangaPageThumb(thumbDir, cbz, 7, 1)
	if err != nil {
		t.Fatalf("thumb generate: %v", err)
	}
	if _, err := os.Stat(thumb1); err != nil {
		t.Fatalf("thumb stat: %v", err)
	}
	thumb2, err := EnsureMangaPageThumb(thumbDir, cbz, 7, 1)
	if err != nil {
		t.Fatalf("thumb hit: %v", err)
	}
	if thumb1 != thumb2 {
		t.Errorf("hit returned different path %q vs %q", thumb1, thumb2)
	}
}

func TestRemoveMangaCache_WithoutThumbnailsPathTouchesNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	mustMkdirAll(t, "7", 0o755)
	removeMangaCache("", 7)
	if _, err := os.Stat("7"); err != nil {
		t.Errorf("./7 is gone: %v", err)
	}
}

func TestRemoveMangaCache_DropsDirectory(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	thumbDir := t.TempDir()
	if _, err := EnsureMangaPage(thumbDir, cbz, 99, 1); err != nil {
		t.Fatalf("extract: %v", err)
	}
	imageDir := mangaImageDir(thumbDir, 99)
	if _, err := os.Stat(imageDir); err != nil {
		t.Fatalf("image dir missing: %v", err)
	}
	removeMangaCache(thumbDir, 99)
	if _, err := os.Stat(imageDir); !os.IsNotExist(err) {
		t.Errorf("image dir still exists after removeMangaCache: %v", err)
	}
}

func TestMangaCacheReclaimer_EvictsStale(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic,
	})
	thumbDir := t.TempDir()
	page1, err := EnsureMangaPage(thumbDir, cbz, 5, 1)
	if err != nil {
		t.Fatalf("extract page 1: %v", err)
	}
	page2, err := EnsureMangaPage(thumbDir, cbz, 5, 2)
	if err != nil {
		t.Fatalf("extract page 2: %v", err)
	}

	old := time.Now().Add(-2 * mangaPageCacheTTL)
	mustChtimes(t, page2, old, old)
	r := NewMangaCacheReclaimer(MangaCacheDir(thumbDir))
	r.sweepOnce()
	if _, err := os.Stat(page1); err != nil {
		t.Errorf("warm page evicted: %v", err)
	}
	if _, err := os.Stat(page2); !os.IsNotExist(err) {
		t.Errorf("stale page survived reclaim: %v", err)
	}
}

func TestMangaCacheReclaimer_StartStop(t *testing.T) {
	r := NewMangaCacheReclaimer(filepath.Join(t.TempDir(), "manga"))
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	r.Start(ctx)
	cancel()
	r.Stop()
	r.Stop()
}

func waitForPageThumb(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("page thumb %q not generated within %s", path, timeout)
}

func TestGenerate_CBZ_PrecomputesEveryPageThumbnail(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 32, 32, [3]uint8{12, 34, 56})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic, "03.png": pic,
	})
	thumbDir := t.TempDir()
	if err := generate(cbz, thumbDir, 77, "cbz"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := os.Stat(ThumbnailPath(thumbDir, 77)); err != nil {
		t.Errorf("cover thumb missing: %v", err)
	}
	imgDir := mangaImageDir(thumbDir, 77)
	for n := 1; n <= 3; n++ {
		waitForPageThumb(t, mangaPageThumbPath(imgDir, n), 5*time.Second)
	}
}

func TestMangaCacheReclaimer_PreservesPageThumbnails(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 16, 16, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic,
	})
	thumbDir := t.TempDir()
	t.Cleanup(func() { WaitPageThumbs(10 * time.Second) })
	if err := generate(cbz, thumbDir, 5, "cbz"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rawPage, err := EnsureMangaPage(thumbDir, cbz, 5, 1)
	if err != nil {
		t.Fatalf("ensure raw page: %v", err)
	}
	thumb := mangaPageThumbPath(mangaImageDir(thumbDir, 5), 1)
	waitForPageThumb(t, thumb, 5*time.Second)
	old := time.Now().Add(-2 * mangaPageCacheTTL)
	mustChtimes(t, rawPage, old, old)
	mustChtimes(t, thumb, old, old)
	r := NewMangaCacheReclaimer(MangaCacheDir(thumbDir))
	r.sweepOnce()
	if _, err := os.Stat(rawPage); !os.IsNotExist(err) {
		t.Errorf("stale raw page survived reclaim: %v", err)
	}
	if _, err := os.Stat(thumb); err != nil {
		t.Errorf("page thumb evicted by reclaim - precompute would never warm again: %v", err)
	}
}

func TestGenerate_CBZ_FFmpegPages(t *testing.T) {
	cbz := writeTestZip(t, t.TempDir(), "m.cbz", map[string][]byte{
		"01.avif": stillBytes(t, ".avif", 640, 360),
		"02.jxl":  stillBytes(t, ".jxl", 360, 640),
	})
	thumbDir := t.TempDir()
	if err := generate(cbz, thumbDir, 9, "cbz"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if w, h := imageSize(t, ThumbnailPath(thumbDir, 9), "jpeg"); w != 300 || h != 169 {
		t.Errorf("cover thumbnail = %dx%d, want 300x169", w, h)
	}
	imgDir := mangaImageDir(thumbDir, 9)
	for n, want := range map[int][2]int{1: {300, 169}, 2: {169, 300}} {
		thumb := mangaPageThumbPath(imgDir, n)
		waitForPageThumb(t, thumb, 10*time.Second)
		if w, h := imageSize(t, thumb, "jpeg"); w != want[0] || h != want[1] {
			t.Errorf("page %d thumbnail = %dx%d, want %dx%d", n, w, h, want[0], want[1])
		}
	}
}

func TestEnsureMangaPageThumb_FFmpegPage(t *testing.T) {
	cbz := writeTestZip(t, t.TempDir(), "m.cbz", map[string][]byte{"01.jxl": stillBytes(t, ".jxl", 640, 360)})
	thumb, err := EnsureMangaPageThumb(t.TempDir(), cbz, 3, 1)
	if err != nil {
		t.Fatalf("EnsureMangaPageThumb: %v", err)
	}
	if w, h := imageSize(t, thumb, "jpeg"); w != 300 || h != 169 {
		t.Errorf("thumbnail = %dx%d, want 300x169", w, h)
	}
}

func TestEnsureMangaPageView(t *testing.T) {
	cbz := writeTestZip(t, t.TempDir(), "m.cbz", map[string][]byte{"01.jxl": stillBytes(t, ".jxl", 640, 360)})
	thumbDir := t.TempDir()
	view, err := EnsureMangaPageView(thumbDir, cbz, 4, 1)
	if err != nil {
		t.Fatalf("EnsureMangaPageView: %v", err)
	}
	if w, h := imageSize(t, view, "jpeg"); w != 640 || h != 360 {
		t.Errorf("copy = %dx%d, want 640x360", w, h)
	}
	if again, err := EnsureMangaPageView(thumbDir, cbz, 4, 1); err != nil || again != view {
		t.Errorf("second call = %q (%v), want the cached %q", again, err, view)
	}
	raw, err := EnsureMangaPage(thumbDir, cbz, 4, 1)
	if err != nil || filepath.Ext(raw) != ".jxl" {
		t.Errorf("EnsureMangaPage = %q (%v), want the .jxl page", raw, err)
	}

	old := time.Now().Add(-2 * mangaPageCacheTTL)
	mustChtimes(t, view, old, old)
	NewMangaCacheReclaimer(MangaCacheDir(thumbDir)).sweepOnce()
	if _, err := os.Stat(view); !os.IsNotExist(err) {
		t.Errorf("an idle copy survived the reclaim: %v", err)
	}
}
