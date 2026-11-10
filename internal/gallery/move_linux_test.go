package gallery

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCommitRename_CopiesAcrossFilesystemsWithoutHoldingTheWriter(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	elsewhere := crossFilesystemDir(t, galleryDir)
	src := createTestPNGFile(t, galleryDir, "a.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, src, "")

	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(src, 0o644); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	var feed sync.Once
	release := func() {
		feed.Do(func() {
			if f, err := os.OpenFile(src, os.O_WRONLY, 0); err == nil {
				_, _ = f.Write([]byte("bytes"))
				_ = f.Close()
			}
		})
	}
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- commitRename(database, rec.ID, src, filepath.Join(elsewhere, "a.png"), nil) }()

	time.Sleep(300 * time.Millisecond)

	wrote := make(chan error, 1)
	go func() {
		_, err := database.Write.Exec(`UPDATE images SET note = 'during the copy' WHERE id = ?`, rec.ID)
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a write waited for the copy")
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("commitRename: %v", err)
	}
}
