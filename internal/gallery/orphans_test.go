package gallery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeThumbFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneOrphanThumbnails_KeepsTheLiveAndDropsTheOrphans(t *testing.T) {
	database := newCollectionsTestDB(t)
	thumbs := t.TempDir()
	live := insertImage(t, database, false)
	liveFiles := []string{fmt.Sprintf("%d.jpg", live), fmt.Sprintf("%d_hover.webp", live), fmt.Sprintf("%d_view.jpg", live)}
	writeThumbFiles(t, thumbs, liveFiles...)
	writeThumbFiles(t, thumbs, "9999.jpg", "9999_hover.webp", "9999_view.jpg", "notes.txt")
	var last int
	removed, processed, total, err := PruneOrphanThumbnails(context.Background(), database, thumbs, func(done, _ int) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 || processed != 7 || total != 7 || last != 7 {
		t.Errorf("removed %d, processed %d of %d, last progress %d; want 3, 7, 7, 7", removed, processed, total, last)
	}
	for _, n := range append(liveFiles, "notes.txt") {
		if !fileExists(filepath.Join(thumbs, n)) {
			t.Errorf("%s went", n)
		}
	}
	if fileExists(filepath.Join(thumbs, "9999_view.jpg")) {
		t.Error("an orphan's view stayed")
	}
}

func TestPruneOrphanThumbnails_RemovesTheMangaCacheOfAGoneRow(t *testing.T) {
	database := newCollectionsTestDB(t)
	thumbs := filepath.Join(t.TempDir(), "thumbnails")
	mustMkdirAll(t, thumbs, 0o755)
	live := insertImage(t, database, false)
	liveCache, goneCache := mangaImageDir(thumbs, live), mangaImageDir(thumbs, 9999)
	for _, dir := range []string{liveCache, goneCache} {
		mustMkdirAll(t, dir, 0o755)
		writeThumbFiles(t, dir, "page_0001_thumb.jpg")
	}

	removed, _, _, err := PruneOrphanThumbnails(context.Background(), database, thumbs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || fileExists(goneCache) {
		t.Errorf("removed %d, gone row's cache present %v; want the one cache folder removed", removed, fileExists(goneCache))
	}
	if !fileExists(filepath.Join(liveCache, "page_0001_thumb.jpg")) {
		t.Error("a live row's page cache went")
	}
}

func TestPruneOrphanThumbnails_RemovesAStaleAutoTagPageFolder(t *testing.T) {
	database := newCollectionsTestDB(t)
	thumbs := filepath.Join(t.TempDir(), "thumbnails")
	mustMkdirAll(t, thumbs, 0o755)
	stale := filepath.Join(filepath.Dir(thumbs), "autotag-123")
	mustMkdirAll(t, stale, 0o755)
	writeThumbFiles(t, stale, "0001.jpg")

	removed, _, _, err := PruneOrphanThumbnails(context.Background(), database, thumbs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || fileExists(stale) {
		t.Errorf("removed %d, stale page folder present %v; want it removed", removed, fileExists(stale))
	}
	if !fileExists(thumbs) {
		t.Error("the sweep removed the thumbnails folder")
	}
}

func TestPruneOrphanThumbnails_AFailedIDReadFailsTheSweep(t *testing.T) {
	database := newCollectionsTestDB(t)
	thumbs := t.TempDir()
	writeThumbFiles(t, thumbs, "9999.jpg")
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := PruneOrphanThumbnails(context.Background(), database, thumbs, nil); err == nil {
		t.Fatal("the sweep ran without the ids")
	}
	if !fileExists(filepath.Join(thumbs, "9999.jpg")) {
		t.Error("a sweep that could not read the ids removed a thumbnail")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestSweepStaleTemps_RemovesOnlyTheTempsOlderThanTheCutoff(t *testing.T) {
	dir := t.TempDir()
	writeThumbFiles(t, dir, "monbooru.db", "export-1.db", "import-2.db-wal", "merge-3.upload", "import-4.upload")
	if err := os.MkdirAll(filepath.Join(dir, "thumbnails-import-5", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now()
	old := cutoff.Add(-time.Hour)
	for _, n := range []string{"monbooru.db", "export-1.db", "import-2.db-wal", "merge-3.upload", "thumbnails-import-5"} {
		if err := os.Chtimes(filepath.Join(dir, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	fresh := cutoff.Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "import-4.upload"), fresh, fresh); err != nil {
		t.Fatal(err)
	}

	SweepStaleTemps(dir, cutoff)

	for _, gone := range []string{"export-1.db", "import-2.db-wal", "merge-3.upload", "thumbnails-import-5"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep (stat err %v)", gone, err)
		}
	}
	for _, kept := range []string{"monbooru.db", "import-4.upload"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was swept: %v", kept, err)
		}
	}
}
