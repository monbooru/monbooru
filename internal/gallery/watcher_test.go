package gallery

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/tags"
)

func newTestWatcher(t *testing.T) (*Watcher, *db.DB, string, *jobs.Manager) {
	t.Helper()
	tmp := t.TempDir()
	galleryDir := filepath.Join(tmp, "gallery")
	thumbDir := filepath.Join(tmp, "thumbs")
	for _, d := range []string{galleryDir, thumbDir} {
		mustMkdirAll(t, d, 0o755)
	}
	database := newCollectionsTestDB(t)

	jm := jobs.NewManager()
	w, err := NewWatcher(Handle{Name: "default", GalleryPath: galleryDir, ThumbnailsPath: thumbDir, DB: database, TagSvc: tags.New(database, nil)}, jm)
	if err != nil {
		t.Fatal(err)
	}
	return w, database, galleryDir, jm
}

func namingOnly(n Naming) func() (int, Naming) {
	return func() (int, Naming) { return 0, n }
}

func startWatcher(t *testing.T) (*Watcher, *db.DB, string) {
	t.Helper()
	w, database, galleryDir, _ := newTestWatcher(t)
	runWatcher(t, w)
	return w, database, galleryDir
}

func runWatcher(t *testing.T, w *Watcher) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx) //nolint:errcheck
}

func pollForRow(t *testing.T, database *db.DB, predicate string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := database.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE ` + predicate).Scan(&n); err == nil && n == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("row matching %s never appeared", predicate)
}

func TestWatcher_DebounceExtendsOnWrite(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	out, err := zw.Create("01.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(solidPNG(t, 8, 8, [3]uint8{55, 66, 77})); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	full := zbuf.Bytes()
	mid := len(full) / 2

	dst := filepath.Join(galleryDir, "slow.cbz")
	f := mustCreate(t, dst)
	if _, err := f.Write(full[:mid]); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := f.Write(full[mid:]); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	pollForRow(t, database, `file_type = 'cbz' AND canonical_path LIKE '%slow.cbz'`, 5*time.Second)
}

func TestWatcher_IngestsFilesInNewSubdir(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	sub := filepath.Join(galleryDir, "batch", "deep")
	mustMkdirAll(t, sub, 0o755)
	for i, name := range []string{"a.png", "b.png"} {
		p := filepath.Join(sub, name)
		mustWriteFile(t, p, solidPNG(t, 8, 8, [3]uint8{uint8(30 + i*40), 10, 10}), 0o644)
	}

	pollForRow(t, database, `canonical_path LIKE '%batch/deep/a.png'`, 5*time.Second)
	pollForRow(t, database, `canonical_path LIKE '%batch/deep/b.png'`, 5*time.Second)
}

func TestWatcher_SeesTheSubfoldersOfARenamedFolder(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)
	mustMkdirAll(t, filepath.Join(galleryDir, "a", "sub"), 0o755)
	mustWriteFile(t, filepath.Join(galleryDir, "a", "sub", "old.png"), solidPNG(t, 8, 8, [3]uint8{60, 10, 90}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%/a/sub/old.png'`, 5*time.Second)

	mustRename(t, filepath.Join(galleryDir, "a"), filepath.Join(galleryDir, "b"))
	pollForRow(t, database, `canonical_path LIKE '%/b/sub/old.png' AND is_missing = 0`, 5*time.Second)

	mustWriteFile(t, filepath.Join(galleryDir, "b", "sub", "new.png"), solidPNG(t, 8, 8, [3]uint8{90, 60, 10}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%/b/sub/new.png'`, 5*time.Second)
	mustRemove(t, filepath.Join(galleryDir, "b", "sub", "old.png"))
	pollForRow(t, database, `canonical_path LIKE '%/b/sub/old.png' AND is_missing = 1`, 6*time.Second)
}

func TestWatcher_PicksUpAFolderLinkedIntoTheGallery(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	mustWriteFile(t, filepath.Join(elsewhere, "already.png"), solidPNG(t, 8, 8, [3]uint8{70, 10, 10}), 0o644)
	mustSymlink(t, elsewhere, filepath.Join(galleryDir, "linked"))
	pollForRow(t, database, `canonical_path LIKE '%/linked/already.png'`, 5*time.Second)

	mustWriteFile(t, filepath.Join(elsewhere, "later.png"), solidPNG(t, 8, 8, [3]uint8{10, 70, 10}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%/linked/later.png'`, 5*time.Second)
}

func TestWatcher_RefusesALinkToATreeTheGallerySitsIn(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	parent := filepath.Dir(galleryDir)
	outside := filepath.Join(parent, "thumbs", "not_an_image_of_ours.png")
	mustWriteFile(t, outside, solidPNG(t, 8, 8, [3]uint8{90, 20, 20}), 0o644)
	mustSymlink(t, parent, filepath.Join(galleryDir, "up"))

	time.Sleep(2 * time.Second)
	if n := countRows(t, database, "images"); n != 0 {
		t.Fatalf("images = %d, want 0: the walk left the gallery", n)
	}
}

func TestWatcher_JobSuppressesTimerIngest(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, jm := newTestWatcher(t)
	runWatcher(t, w)

	p := filepath.Join(galleryDir, "suppressed.png")
	mustWriteFile(t, p, solidPNG(t, 8, 8, [3]uint8{90, 20, 20}), 0o644)

	time.Sleep(150 * time.Millisecond)
	if err := jm.Start("sync"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if n := countRows(t, database, "images"); n != 0 {
		t.Fatalf("ingest ran during a suppressing job: %d image(s)", n)
	}
}

func TestWatcher_WaitIngestsReturnsOnceTheRunningIngestEnds(t *testing.T) {
	t.Parallel()
	w, _, _, _ := newTestWatcher(t)
	t.Cleanup(w.shutdown)
	w.mu.Lock()
	w.ingesting = 1
	w.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- w.WaitIngests(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("WaitIngests returned while an ingest was running: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	w.mu.Lock()
	w.ingesting = 0
	w.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitIngests still waiting after the ingest ended")
	}

	w.mu.Lock()
	w.ingesting = 1
	w.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.WaitIngests(ctx); err == nil {
		t.Error("WaitIngests ignored a cancelled context")
	}
	w.mu.Lock()
	w.ingesting = 0
	w.mu.Unlock()
}

func TestWatcher_AnotherGallerysJobDoesNotHoldItsEvents(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, jm := newTestWatcher(t)
	runWatcher(t, w)
	if err := jm.Start("sync", "other"); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(galleryDir, "during.png"), solidPNG(t, 8, 8, [3]uint8{20, 90, 20}), 0o644)

	pollForRow(t, database, `canonical_path LIKE '%during.png' AND is_missing = 0`, 5*time.Second)
}

func TestWatcher_WatchesAFolderCreatedDuringAJob(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, jm := newTestWatcher(t)
	runWatcher(t, w)
	if err := jm.Start("sync", "default"); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(galleryDir, "made_during"), 0o755)
	time.Sleep(300 * time.Millisecond)
	jm.Complete("done")

	mustWriteFile(t, filepath.Join(galleryDir, "made_during", "after.png"), solidPNG(t, 8, 8, [3]uint8{20, 20, 90}), 0o644)

	pollForRow(t, database, `canonical_path LIKE '%after.png' AND is_missing = 0`, 5*time.Second)
}

func takeWatcherSlots(t *testing.T) func() {
	t.Helper()
	for range cap(watcherSlots) {
		watcherSlots <- struct{}{}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for range cap(watcherSlots) {
				<-watcherSlots
			}
		})
	}
	t.Cleanup(release)
	return release
}

func TestWatcher_IngestsWaitForAFreeSlot(t *testing.T) {
	_, database, galleryDir := startWatcher(t)
	release := takeWatcherSlots(t)

	mustWriteFile(t, filepath.Join(galleryDir, "waited.png"), solidPNG(t, 8, 8, [3]uint8{70, 70, 20}), 0o644)

	time.Sleep(1200 * time.Millisecond)
	if n := countRows(t, database, "images"); n != 0 {
		t.Fatalf("ingested with every slot taken: %d image(s)", n)
	}
	release()
	pollForRow(t, database, `canonical_path LIKE '%waited.png' AND is_missing = 0`, 5*time.Second)
}

func TestWatcher_AWaitingDropParksNoGoroutinePerFile(t *testing.T) {
	_, database, galleryDir := startWatcher(t)
	release := takeWatcherSlots(t)
	before := runtime.NumGoroutine()

	const files = 300
	for i := range files {
		mustWriteFile(t, filepath.Join(galleryDir, fmt.Sprintf("q%03d.png", i)), solidPNG(t, 8, 8, [3]uint8{uint8(i), uint8(i >> 8), 7}), 0o644)
	}
	time.Sleep(debounceDelay + time.Second)
	if grew := runtime.NumGoroutine() - before; grew >= 20 {
		t.Errorf("goroutines grew by %d for %d waiting files", grew, files)
	}

	release()
	deadline := time.Now().Add(30 * time.Second)
	for countRows(t, database, "images") < files && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := countRows(t, database, "images"); n != files {
		t.Errorf("images = %d, want every queued file ingested (%d)", n, files)
	}
}

func TestWatcher_ShutdownDoesNotWaitForASlot(t *testing.T) {
	w, _, galleryDir, _ := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	takeWatcherSlots(t)
	mustWriteFile(t, filepath.Join(galleryDir, "parked.png"), solidPNG(t, 8, 8, [3]uint8{20, 70, 70}), 0o644)

	time.Sleep(900 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown waited for a slot")
	}
}

func TestWatcher_MarksMovedOutFileMissing(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	p := filepath.Join(galleryDir, "leaving.png")
	mustWriteFile(t, p, solidPNG(t, 8, 8, [3]uint8{40, 40, 120}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%leaving.png' AND is_missing = 0`, 5*time.Second)

	mustRename(t, p, filepath.Join(t.TempDir(), "leaving.png"))
	pollForRow(t, database, `canonical_path LIKE '%leaving.png' AND is_missing = 1`, 6*time.Second)
}

func TestWatcher_MoveInsideTreeKeepsOneRow(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	src := filepath.Join(galleryDir, "before.png")
	mustWriteFile(t, src, solidPNG(t, 8, 8, [3]uint8{120, 40, 40}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%before.png' AND is_missing = 0`, 5*time.Second)

	mustRename(t, src, filepath.Join(galleryDir, "after.png"))
	pollForRow(t, database, `canonical_path LIKE '%after.png' AND is_missing = 0`, 5*time.Second)

	time.Sleep(movedOutGrace + time.Second)
	if n := countRows(t, database, "images"); n != 1 {
		t.Fatalf("images = %d, want 1", n)
	}
	var missing, stale int
	if err := database.Read.QueryRow(`SELECT is_missing FROM images`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Error("the repointed row was marked missing")
	}
	stale = countRows(t, database, "image_paths WHERE path LIKE '%before.png'")
	if stale != 0 {
		t.Errorf("old path rows = %d, want 0", stale)
	}
}

func TestWatcher_ReplacedFileStaysPresent(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	p := filepath.Join(galleryDir, "swapped.png")
	img := solidPNG(t, 8, 8, [3]uint8{40, 120, 40})
	mustWriteFile(t, p, img, 0o644)
	pollForRow(t, database, `canonical_path LIKE '%swapped.png' AND is_missing = 0`, 5*time.Second)

	mustRemove(t, p)
	mustWriteFile(t, p, img, 0o644)

	deadline := time.Now().Add(movedOutGrace + time.Second)
	for time.Now().Before(deadline) {
		var missing int
		if err := database.Read.QueryRow(`SELECT is_missing FROM images`).Scan(&missing); err != nil {
			t.Fatal(err)
		}
		if missing != 0 {
			t.Fatal("the replaced row was marked missing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcher_ShutdownDropsPendingIngest(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()

	p := filepath.Join(galleryDir, "pending.png")
	mustWriteFile(t, p, solidPNG(t, 8, 8, [3]uint8{20, 90, 20}), 0o644)
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	time.Sleep(700 * time.Millisecond)
	if n := countRows(t, database, "images"); n != 0 {
		t.Fatalf("ingest ran after shutdown: %d image(s)", n)
	}
}

func TestWatcher_NamesWhatItPicksUp(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	tmpl := mustParseNameTemplate(t, "{gallery}-{id}", ScopeUploadName)
	w.Settings = namingOnly(Naming{Name: tmpl, Gallery: "default"})
	runWatcher(t, w)

	createTestPNGFile(t, galleryDir, "dropped.png")
	pollForRow(t, database, `canonical_path LIKE '%/default-1.png' AND is_missing = 0`, 5*time.Second)

	time.Sleep(movedOutGrace + debounceDelay)

	var rows, missing int
	if err := database.Read.QueryRow(`SELECT COUNT(*), COALESCE(SUM(is_missing), 0) FROM images`).Scan(&rows, &missing); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || missing != 0 {
		t.Errorf("rows=%d missing=%d, want 1 and 0", rows, missing)
	}

	paths := countRows(t, database, "image_paths")
	if paths != 1 {
		t.Errorf("image_paths rows = %d, want 1 (the rename re-entered ingest)", paths)
	}
}

func TestWatcher_LeavesAnOperatorsRenameAlone(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	w.Settings = namingOnly(Naming{Name: mustParseNameTemplate(t, "{gallery}-{id}", ScopeUploadName), Gallery: "default"})
	w.ingestFile(createTestPNGFile(t, galleryDir, "dropped.png"))
	named := filepath.Join(galleryDir, "default-1.png")
	pollForRow(t, database, `canonical_path LIKE '%/default-1.png'`, time.Second)

	keep := filepath.Join(galleryDir, "keep", "my-favourite.png")
	mustMkdirAll(t, filepath.Dir(keep), 0o755)
	mustRename(t, named, keep)
	w.ingestFile(keep)

	pollForRow(t, database, `canonical_path LIKE '%/keep/my-favourite.png' AND is_missing = 0`, time.Second)
}

func TestWatcher_DoesNotRenameAFileEditedInPlace(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	w.Settings = namingOnly(Naming{Name: mustParseNameTemplate(t, "{hash}", ScopeUploadName), Gallery: "default"})
	w.ingestFile(createTestPNGFileSize(t, galleryDir, "dropped.png", 10, 10))
	var path string
	if err := database.Read.QueryRow(`SELECT canonical_path FROM images`).Scan(&path); err != nil {
		t.Fatal(err)
	}

	mustRemove(t, path)
	createTestPNGFileSize(t, galleryDir, filepath.Base(path), 30, 20)
	w.ingestFile(path)

	var now string
	var width int
	if err := database.Read.QueryRow(`SELECT canonical_path, width FROM images`).Scan(&now, &width); err != nil {
		t.Fatal(err)
	}
	if width != 30 || now != path {
		t.Errorf("row = %q %dpx wide, want the new bytes read at %q", now, width, path)
	}
}

func TestWatcher_TakesAFileWithItsIndexedSizeAndMtimeAsUnchanged(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	path := createTestPNGFile(t, galleryDir, "landed.png")
	w.ingestFile(path)
	var sha string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&sha); err != nil {
		t.Fatal(err)
	}

	info := mustStat(t, path)
	body := mustReadFile(t, path)
	body[len(body)-14] ^= 0xff
	mustWriteFile(t, path, body, 0o644)
	mustChtimes(t, path, info.ModTime(), info.ModTime())
	w.ingestFile(path)

	var now string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if now != sha {
		t.Errorf("sha256 = %s, want the indexed %s kept without a re-hash", now, sha)
	}
}

func TestWatcher_ReportsAMoveAsMoved(t *testing.T) {
	t.Parallel()
	w, _, galleryDir, _ := newTestWatcher(t)
	var events []string
	w.OnEvent = func(msg string) { events = append(events, msg) }
	src := createTestPNGFile(t, galleryDir, "dropped.png")
	w.ingestFile(src)
	dst := filepath.Join(galleryDir, "my-favourite.png")
	mustRename(t, src, dst)
	w.ingestFile(dst)

	want := []string{"watcher [default]: added dropped.png", "watcher [default]: moved my-favourite.png"}
	if !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestWatcher_ReportsAFileRenamedOverAnotherAsAConflict(t *testing.T) {
	t.Parallel()
	w, _, galleryDir, _ := newTestWatcher(t)
	a := createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
	b := createTestPNGFileSize(t, galleryDir, "b.png", 12, 12)
	w.ingestFile(a)
	w.ingestFile(b)
	var events []string
	w.OnEvent = func(msg string) { events = append(events, msg) }

	mustRename(t, b, a)
	w.ingestFile(a)

	want := []string{"watcher [default]: conflict a.png"}
	if !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestWatcher_AnEditWhoseThumbnailFailsClearsItsIndexEntry(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	path := createTestPNGFileSize(t, galleryDir, "rehash.png", 10, 10)
	w.ingestFile(path)
	id := imageIDByPath(t, database, path)
	thumb := ThumbnailPath(w.handle.ThumbnailsPath, id)
	mustRemove(t, thumb)
	mustMkdirAll(t, filepath.Join(thumb, "blocker"), 0o755)
	var reported []*int64
	w.OnPhash = func(imageID int64, phash *int64) { reported = append(reported, phash) }

	mustRemove(t, path)
	createTestPNGFileSize(t, galleryDir, "rehash.png", 20, 20)
	w.ingestFile(path)

	if len(reported) != 1 || reported[0] != nil {
		t.Errorf("the sink got %v, want one cleared phash", reported)
	}
}

func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWatcher_ReadsTheSizeCapAtEachIngest(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	capMB := 0
	w.Settings = func() (int, Naming) { return capMB, Naming{} }

	first := filepath.Join(galleryDir, "first.png")
	mustWriteFile(t, first, noisePNG(t, 700, 700), 0o644)
	w.ingestFile(first)

	capMB = 1
	second := filepath.Join(galleryDir, "second.png")
	mustWriteFile(t, second, noisePNG(t, 700, 700), 0o644)
	w.ingestFile(second)

	if n := countRows(t, database, "images WHERE canonical_path LIKE '%first.png'"); n != 1 {
		t.Errorf("rows for the file under no cap = %d, want 1", n)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%second.png'"); n != 0 {
		t.Errorf("rows for the file past the 1 MB cap set since = %d, want 0", n)
	}
}

func TestWatcher_NamesWithTheTemplateCurrentAtIngest(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	naming := Naming{}
	w.Settings = func() (int, Naming) { return 0, naming }
	w.ingestFile(createTestPNGFileSize(t, galleryDir, "kept.png", 10, 10))

	naming = Naming{Name: mustParseNameTemplate(t, "{gallery}-{id}", ScopeUploadName), Gallery: "default"}
	w.ingestFile(createTestPNGFileSize(t, galleryDir, "named.png", 20, 20))

	if n := countRows(t, database, "images WHERE canonical_path LIKE '%/kept.png'"); n != 1 {
		t.Errorf("rows at kept.png = %d, want the file ingested before the template left alone", n)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%/default-2.png'"); n != 1 {
		t.Errorf("rows at default-2.png = %d, want the template applied once set", n)
	}
}

func TestWatcher_LeavesNamesAloneByDefault(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)
	createTestPNGFile(t, galleryDir, "untouched.png")
	pollForRow(t, database, `canonical_path LIKE '%/untouched.png'`, 5*time.Second)
}

func TestWatcher_FilesWhatItPicksUp(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	w.Settings = namingOnly(IngestNaming("default", "inbox/{year}", "{gallery}-{id}"))
	runWatcher(t, w)

	mustMkdirAll(t, filepath.Join(galleryDir, "drop"), 0o755)
	createTestPNGFile(t, galleryDir, "drop/raw.png")

	year := time.Now().UTC().Format("2006")
	pollForRow(t, database,
		`folder_path = 'inbox/`+year+`' AND canonical_path LIKE '%/default-1.png' AND is_missing = 0`,
		5*time.Second)

	time.Sleep(movedOutGrace + debounceDelay)

	var rows, missing, paths int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(is_missing), 0) FROM images`,
	).Scan(&rows, &missing); err != nil {
		t.Fatal(err)
	}
	paths = countRows(t, database, "image_paths")
	if rows != 1 || missing != 0 || paths != 1 {
		t.Errorf("rows=%d missing=%d paths=%d, want 1/0/1", rows, missing, paths)
	}
	if _, err := os.Stat(filepath.Join(galleryDir, "drop", "raw.png")); !os.IsNotExist(err) {
		t.Errorf("the file is still at the path it was dropped at (err=%v)", err)
	}
}

func TestWatcher_KeepsImageWhoseCopySurvives(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)

	img := solidPNG(t, 8, 8, [3]uint8{60, 90, 150})
	first := filepath.Join(galleryDir, "one.png")
	mustWriteFile(t, first, img, 0o644)
	pollForRow(t, database, `canonical_path LIKE '%one.png' AND is_missing = 0`, 5*time.Second)

	second := filepath.Join(galleryDir, "two.png")
	mustWriteFile(t, second, img, 0o644)
	pollForRow(t, database, "EXISTS (SELECT 1 FROM image_paths p WHERE p.image_id = images.id AND p.path = '"+second+"' AND p.is_canonical = 0)", 5*time.Second)

	mustRemove(t, first)
	pollForRow(t, database, `canonical_path LIKE '%two.png' AND is_missing = 0`, 6*time.Second)

	if n := countRows(t, database, "images"); n != 1 {
		t.Fatalf("images = %d, want 1", n)
	}

	stale := 1
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stale = countRows(t, database, "image_paths WHERE path = ?", first); stale == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stale != 0 {
		t.Errorf("rows still naming the deleted copy = %d, want 0", stale)
	}
}

func TestWatcher_RefusesToPromoteACopyOfTheWrongLength(t *testing.T) {
	t.Parallel()
	w, database, galleryDir := startWatcher(t)

	img := solidPNG(t, 8, 8, [3]uint8{60, 90, 150})
	first := filepath.Join(galleryDir, "one.png")
	mustWriteFile(t, first, img, 0o644)
	pollForRow(t, database, `canonical_path LIKE '%one.png' AND is_missing = 0`, 5*time.Second)
	id := imageIDByPath(t, database, first)
	other := filepath.Join(galleryDir, "two.png")
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, other)
	mustWriteFile(t, other, append(img, "trailing"...), 0o644)

	if w.promoteSurvivingCopy(id, first) {
		t.Fatal("promoted a copy whose length does not match the row")
	}
	var canonical string
	if err := database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical != first {
		t.Errorf("canonical_path = %q, want the original %q", canonical, first)
	}
}

func TestWatcher_PromotesOnlyACopyHoldingTheRowsBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		other    func([]byte) []byte
		promoted bool
	}{
		{"the row's bytes", func(b []byte) []byte { return b }, true},
		{"other bytes of the same length", func(b []byte) []byte {
			b = slices.Clone(b)
			b[len(b)-1] ^= 0xff
			return b
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, database, galleryDir, _ := newTestWatcher(t)
			img := solidPNG(t, 8, 8, [3]uint8{62, 92, 152})
			first := filepath.Join(galleryDir, "one.png")
			mustWriteFile(t, first, img, 0o644)
			rec, _ := mustIngest(t, database, galleryDir, w.handle.ThumbnailsPath, first, "")
			other := filepath.Join(galleryDir, "two.png")
			mustWriteFile(t, other, tc.other(img), 0o644)
			mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, rec.ID, other)
			mustRemove(t, first)

			if got := w.promoteSurvivingCopy(rec.ID, first); got != tc.promoted {
				t.Fatalf("promoted %t, want %t", got, tc.promoted)
			}
		})
	}
}

func TestWatcher_DoesNotPromoteACopyInAFencedFolder(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	img := solidPNG(t, 8, 8, [3]uint8{61, 91, 151})
	first := filepath.Join(galleryDir, "one.png")
	mustWriteFile(t, first, img, 0o644)
	rec, _ := mustIngest(t, database, galleryDir, w.handle.ThumbnailsPath, first, "")
	other := t.TempDir()
	mustWriteFile(t, filepath.Join(other, "two.png"), img, 0o644)
	mustSymlink(t, other, filepath.Join(galleryDir, "lnk"))
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, rec.ID, filepath.Join(galleryDir, "lnk", "two.png"))
	w.boundary = func() *Boundary {
		return NewBoundary(galleryDir, []Fence{{Path: other, Owner: "other", Kind: FenceGallery}}, nil)
	}

	if w.promoteSurvivingCopy(rec.ID, first) {
		t.Fatal("promoted a copy that lies in another gallery's folder")
	}
}

func TestWatcher_DropsEventsFromAFolderFencedOffWhileRunning(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	galleryDir := filepath.Join(tmp, "gallery")
	inner := filepath.Join(galleryDir, "inner")
	thumbDir := filepath.Join(tmp, "thumbs")
	for _, d := range []string{inner, thumbDir} {
		mustMkdirAll(t, d, 0o755)
	}
	database := newCollectionsTestDB(t)
	bounds := new(atomic.Pointer[Boundary])
	w, err := NewWatcher(Handle{GalleryPath: galleryDir, ThumbnailsPath: thumbDir, DB: database, Bounds: bounds, TagSvc: tags.New(database, nil)}, jobs.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	runWatcher(t, w)

	bounds.Store(NewBoundary(galleryDir, []Fence{{Path: inner, Owner: "inner", Kind: FenceGallery}}, nil))
	mustWriteFile(t, filepath.Join(inner, "in.png"), solidPNG(t, 8, 8, [3]uint8{11, 22, 33}), 0o644)
	mustWriteFile(t, filepath.Join(galleryDir, "top.png"), solidPNG(t, 8, 8, [3]uint8{44, 55, 66}), 0o644)

	pollForRow(t, database, `canonical_path LIKE '%top.png'`, 5*time.Second)
	time.Sleep(debounceDelay)
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%in.png'"); n != 0 {
		t.Errorf("the fenced folder's file was ingested (%d row)", n)
	}
}

func TestWatcher_ALinkMadeWhileRunningSkipsAFencedFolderInIt(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	galleryDir := filepath.Join(tmp, "gallery")
	outside := filepath.Join(tmp, "outside")
	thumbDir := filepath.Join(tmp, "thumbs")
	for _, d := range []string{galleryDir, filepath.Join(outside, "B"), thumbDir} {
		mustMkdirAll(t, d, 0o755)
	}
	mustWriteFile(t, filepath.Join(outside, "B", "x.png"), solidPNG(t, 8, 8, [3]uint8{12, 34, 56}), 0o644)
	mustWriteFile(t, filepath.Join(outside, "y.png"), solidPNG(t, 8, 8, [3]uint8{65, 43, 21}), 0o644)
	database := newCollectionsTestDB(t)
	bounds := new(atomic.Pointer[Boundary])
	bounds.Store(NewBoundary(galleryDir, []Fence{{Path: filepath.Join(outside, "B"), Owner: "b", Kind: FenceGallery}}, nil))
	w, err := NewWatcher(Handle{GalleryPath: galleryDir, ThumbnailsPath: thumbDir, DB: database, Bounds: bounds, TagSvc: tags.New(database, nil)}, jobs.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	runWatcher(t, w)

	mustSymlink(t, outside, filepath.Join(galleryDir, "ext"))

	pollForRow(t, database, `canonical_path LIKE '%/ext/y.png'`, 5*time.Second)
	time.Sleep(debounceDelay)
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%/ext/B/%'"); n != 0 {
		t.Errorf("the fenced folder's file was ingested through the link (%d row)", n)
	}
}

func TestWatcher_ARevivedRowKeepsItsIndexEntry(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	var cleared []int64
	w.OnPhash = func(id int64, phash *int64) {
		if phash == nil {
			cleared = append(cleared, id)
		}
	}
	path := createTestPNGFileSize(t, galleryDir, "back.png", 40, 40)
	w.ingestFile(path)
	id := imageIDByPath(t, database, path)
	if _, err := SetMissing(database, id, true); err != nil {
		t.Fatal(err)
	}

	w.ingestFile(path)
	if got := imageMissing(t, database, id); got != 0 {
		t.Fatalf("is_missing = %d after the file came back, want 0", got)
	}
	if len(cleared) != 0 {
		t.Errorf("the revived row's index entry was cleared: %v", cleared)
	}
}

func TestWatcher_RenamingOneFileOverAnotherLeavesOneRowOnIt(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)
	a := filepath.Join(galleryDir, "a.png")
	b := filepath.Join(galleryDir, "b.png")
	mustWriteFile(t, a, solidPNG(t, 8, 8, [3]uint8{200, 40, 40}), 0o644)
	mustWriteFile(t, b, solidPNG(t, 8, 8, [3]uint8{40, 40, 200}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%/a.png' AND is_missing = 0`, 5*time.Second)
	pollForRow(t, database, `canonical_path LIKE '%/b.png' AND is_missing = 0`, 5*time.Second)

	mustRename(t, b, a)
	time.Sleep(movedOutGrace + time.Second)

	if n := countRows(t, database, "images WHERE canonical_path = ?", a); n != 1 {
		t.Errorf("images on a.png = %d, want 1", n)
	}
	pollForRow(t, database, `canonical_path LIKE '%/b.png' AND is_missing = 1`, 5*time.Second)
}

func TestWatcher_MarksAFolderMovedOutMissingWithoutASync(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)
	album := filepath.Join(galleryDir, "album")
	mustMkdirAll(t, filepath.Join(album, "sub"), 0o755)
	mustWriteFile(t, filepath.Join(album, "a.png"), solidPNG(t, 8, 8, [3]uint8{10, 200, 30}), 0o644)
	mustWriteFile(t, filepath.Join(album, "sub", "b.png"), solidPNG(t, 8, 8, [3]uint8{10, 30, 200}), 0o644)
	mustWriteFile(t, filepath.Join(galleryDir, "c.png"), solidPNG(t, 8, 8, [3]uint8{200, 30, 10}), 0o644)
	pollForRow(t, database, `canonical_path LIKE '%a.png' AND is_missing = 0`, 5*time.Second)
	pollForRow(t, database, `canonical_path LIKE '%b.png' AND is_missing = 0`, 5*time.Second)
	pollForRow(t, database, `canonical_path LIKE '%c.png' AND is_missing = 0`, 5*time.Second)

	mustRename(t, album, filepath.Join(t.TempDir(), "album"))
	pollForRow(t, database, `canonical_path LIKE '%a.png' AND is_missing = 1`, 6*time.Second)
	pollForRow(t, database, `canonical_path LIKE '%b.png' AND is_missing = 1`, 6*time.Second)
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%c.png' AND is_missing = 0"); n != 1 {
		t.Error("the image outside the folder was marked missing too")
	}
}

func TestWatcher_AFolderRenamedInsideTheTreeStaysShown(t *testing.T) {
	t.Parallel()
	_, database, galleryDir := startWatcher(t)
	from := filepath.Join(galleryDir, "from")
	mustMkdirAll(t, from, 0o755)
	for i := range 6 {
		mustWriteFile(t, filepath.Join(from, fmt.Sprintf("p%d.png", i)), solidPNG(t, 8, 8, [3]uint8{uint8(20 * i), 120, 40}), 0o644)
	}
	for i := range 6 {
		pollForRow(t, database, fmt.Sprintf(`canonical_path LIKE '%%from%cp%d.png' AND is_missing = 0`, filepath.Separator, i), 5*time.Second)
	}

	mustRename(t, from, filepath.Join(galleryDir, "to"))
	time.Sleep(movedOutGrace + 2*time.Second)
	if n := countRows(t, database, "images WHERE is_missing = 1"); n != 0 {
		t.Errorf("%d image(s) of the renamed folder were marked missing", n)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE ?", "%"+string(filepath.Separator)+"to"+string(filepath.Separator)+"%"); n != 6 {
		t.Errorf("%d image(s) moved with the folder, want 6", n)
	}
}

func TestWatcher_AVanishCheckWaitsForTheIngestsStillDue(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	t.Cleanup(w.shutdown)
	from, to := filepath.Join(galleryDir, "from"), filepath.Join(galleryDir, "to")
	mustMkdirAll(t, from, 0o755)
	mustWriteFile(t, filepath.Join(from, "p.png"), solidPNG(t, 8, 8, [3]uint8{70, 130, 50}), 0o644)
	rec, _ := mustIngest(t, database, galleryDir, w.handle.ThumbnailsPath, filepath.Join(from, "p.png"), "")
	mustRename(t, from, to)

	w.debounce(filepath.Join(to, "p.png"))
	w.reconcileVanished(from)
	if n := countRows(t, database, "images WHERE id = ? AND is_missing = 1", rec.ID); n != 0 {
		t.Fatal("the image was marked missing while the ingest that moves it was still due")
	}
	pollForRow(t, database, fmt.Sprintf("id = %d AND canonical_path = '%s'", rec.ID, filepath.Join(to, "p.png")), 5*time.Second)
	time.Sleep(movedOutGrace + time.Second)
	if n := countRows(t, database, "images WHERE is_missing = 1"); n != 0 {
		t.Errorf("%d image(s) marked missing once the deferred check ran", n)
	}
}

func TestWatcher_AFolderGoneKeepsWhatACopyOutsideHolds(t *testing.T) {
	t.Parallel()
	w, database, galleryDir, _ := newTestWatcher(t)
	h := testHandle(database, galleryDir, w.handle.ThumbnailsPath)
	album := filepath.Join(galleryDir, "album")
	mustMkdirAll(t, album, 0o755)
	ingest := func(path string, img []byte) int64 {
		t.Helper()
		mustWriteFile(t, path, img, 0o644)
		rec, _, err := Ingest(h, path, "")
		if err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}
	kept := solidPNG(t, 8, 8, [3]uint8{11, 22, 33})
	keptID := ingest(filepath.Join(album, "kept.png"), kept)
	ingest(filepath.Join(galleryDir, "kept-copy.png"), kept)
	outside := solidPNG(t, 8, 8, [3]uint8{44, 55, 66})
	outsideID := ingest(filepath.Join(galleryDir, "outside.png"), outside)
	ingest(filepath.Join(album, "outside-copy.png"), outside)
	goneID := ingest(filepath.Join(album, "gone.png"), solidPNG(t, 8, 8, [3]uint8{77, 88, 99}))

	mustRename(t, album, filepath.Join(t.TempDir(), "album"))
	w.reconcileVanished(album)

	missing := func(id int64) bool { return imageMissing(t, database, id) == 1 }
	if !missing(goneID) {
		t.Error("an image whose only file left with the folder is still shown")
	}
	if missing(keptID) || missing(outsideID) {
		t.Errorf("an image with a file left in the gallery was marked missing: kept %v, outside %v", missing(keptID), missing(outsideID))
	}
	if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ?", keptID, filepath.Join(galleryDir, "kept-copy.png")); n != 1 {
		t.Error("the image whose file left did not move onto its copy in the gallery")
	}
	if n := countRows(t, database, "image_paths WHERE path LIKE ? AND image_id != ?", album+string(filepath.Separator)+"%", goneID); n != 0 {
		t.Errorf("%d path rows of images still shown name files under the folder that left", n)
	}
}
