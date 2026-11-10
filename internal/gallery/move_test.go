package gallery

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func moveTo(database *db.DB, galleryPath string, id int64, folder string) (*MoveImageResult, error) {
	return PlaceImage(database, NewBoundary(galleryPath, nil, nil), id, &folder, nil)
}

func renameTo(database *db.DB, galleryPath string, id int64, name string) (*MoveImageResult, error) {
	return PlaceImage(database, NewBoundary(galleryPath, nil, nil), id, nil, &name)
}

func crossFilesystemDir(t *testing.T, src string) string {
	t.Helper()
	for _, base := range []string{"/dev/shm", os.TempDir()} {
		dir, err := os.MkdirTemp(base, "monbooru-xdev")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		probe := filepath.Join(src, "xdev-probe")
		mustWriteFile(t, probe, []byte("x"), 0o644)
		moved := filepath.Join(dir, "xdev-probe")
		renameErr := os.Rename(probe, moved)
		_ = os.Remove(probe)
		_ = os.Remove(moved)
		if errors.Is(renameErr, syscall.EXDEV) {
			return dir
		}
	}
	t.Skip("no second filesystem here to move across")
	return ""
}

func TestPlaceImage_MovesAcrossAFilesystemBoundary(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	elsewhere := crossFilesystemDir(t, galleryDir)
	mustSymlink(t, elsewhere, filepath.Join(galleryDir, "otherdisk"))

	srcPath := createTestPNGFile(t, galleryDir, "original.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")

	folder := "otherdisk/2026"
	res := mustPlaceImage(t, database, galleryDir, rec.ID, &folder, nil)
	want := filepath.Join(galleryDir, "otherdisk", "2026", "original.png")
	if res.NewCanonicalPath != want {
		t.Fatalf("new path = %q, want %q", res.NewCanonicalPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the file did not land on the other filesystem: %v", err)
	}
	if _, err := os.Lstat(srcPath); err == nil {
		t.Error("the source was left behind, so the move was a copy")
	}

	canonPath, folderPath := pathsOf(t, database, rec.ID)
	if canonPath != want || folderPath != "otherdisk/2026" {
		t.Errorf("row = (%q, %q), want (%q, otherdisk/2026)", canonPath, folderPath, want)
	}

	root := ""
	back := mustPlaceImage(t, database, galleryDir, rec.ID, &root, nil)
	if back.NewCanonicalPath != srcPath {
		t.Errorf("moved back to %q, want %q", back.NewCanonicalPath, srcPath)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("the file did not come back: %v", err)
	}
}

func TestPlaceImage_RefusesANameOnTheIgnoreList(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "a.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	b := NewBoundary(galleryDir, nil, []string{"._*"})
	name := "._a"

	_, err := PlaceImage(database, b, rec.ID, nil, &name)

	if err == nil || !strings.Contains(err.Error(), "ignore list") {
		t.Fatalf("err = %v, want the name refused as on the ignore list", err)
	}
	if _, statErr := os.Stat(srcPath); statErr != nil {
		t.Errorf("the file moved anyway: %v", statErr)
	}
	if _, _, err := PlannedPath(database, b, rec.ID, nil, &name, map[string]bool{}); err == nil {
		t.Error("the dry run accepted a name on the ignore list")
	}
}

func TestCommitRename_AFailedUpdateMovesTheFileBack(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	src := createTestPNGFile(t, galleryDir, "a.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, src, "")
	other, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "b.png", 12, 12), "")
	target := filepath.Join(galleryDir, "c.png")

	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, other.ID, target)

	if err := commitRename(database, rec.ID, src, target, nil); err == nil {
		t.Fatal("the update was expected to fail")
	}

	if _, err := os.Stat(src); err != nil {
		t.Errorf("the file was not moved back: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the file is left at the destination: %v", err)
	}
	if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ?", rec.ID, src); n != 1 {
		t.Error("the row moved with a failed update")
	}
}

func TestMoveImage_IntoSubfolder(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")

	id := rec.ID

	res, err := moveTo(database, galleryDir, id, "2026/april")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.NewFolderPath != "2026/april" {
		t.Errorf("new folder = %q, want 2026/april", res.NewFolderPath)
	}

	canonPath, folderPath := pathsOf(t, database, id)
	if folderPath != "2026/april" {
		t.Errorf("folder_path = %q, want 2026/april", folderPath)
	}
	wantPath := filepath.Join(galleryDir, "2026", "april", "original.png")
	if canonPath != wantPath {
		t.Errorf("canonical_path = %q, want %q", canonPath, wantPath)
	}

	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Errorf("file still at old path (err=%v)", err)
	}

	aliasPath := canonicalPathRow(t, database, id)
	if aliasPath != wantPath {
		t.Errorf("canonical image_paths row = %q, want %q", aliasPath, wantPath)
	}
}

func TestAFolderNamedWithTwoLeadingDotsIsInsideTheGallery(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	dir := filepath.Join(galleryDir, "..drafts")
	mustMkdirAll(t, dir, 0o755)
	srcPath := createTestPNGFile(t, dir, "pic.png")
	if r := env.sync(t, database); r.Added != 1 {
		t.Fatalf("sync added %d, want 1", r.Added)
	}
	if !NamedInside(galleryDir, srcPath) {
		t.Error("the serve gate put the file outside the gallery")
	}
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := moveTo(database, galleryDir, id, "sorted"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(galleryDir, "sorted", "pic.png")); err != nil {
		t.Errorf("file not at its new path: %v", err)
	}
}

func TestMoveImage_RefusesSourceOutsideGallery(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "drift.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID
	outside := filepath.Join(t.TempDir(), "outside.png")
	mustWriteFile(t, outside, []byte("x"), 0o644)
	mustExec(t, database.Write, `UPDATE images SET canonical_path = ? WHERE id = ?`, outside, id)
	if _, err := moveTo(database, galleryDir, id, "dst"); err == nil {
		t.Fatal("expected a move to refuse a source outside the gallery root")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside file should be untouched: %v", err)
	}
}

func TestMoveImage_FilenameCollisionAutosuffixes(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFileSize(t, galleryDir, "pic.png", 10, 10)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), srcPath, ""); err != nil {
		t.Fatalf("ingest src: %v", err)
	}

	dstDir := filepath.Join(galleryDir, "dst")
	mustMkdirAll(t, dstDir, 0o755)
	createTestPNGFileSize(t, dstDir, "pic.png", 11, 10)

	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	res, err := moveTo(database, galleryDir, id, "dst")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	want := filepath.Join(dstDir, "pic_1.png")
	if res.NewCanonicalPath != want {
		t.Errorf("new canonical = %q, want %q", res.NewCanonicalPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at auto-suffixed path: %v", err)
	}
}

func TestMoveImage_RenameFailureRollsBackTx(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "rollme.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID

	dstDir := filepath.Join(galleryDir, "ro")
	mustMkdirAll(t, dstDir, 0o555)
	t.Cleanup(func() { _ = os.Chmod(dstDir, 0o755) })

	if _, err := moveTo(database, galleryDir, id, "ro"); err == nil {
		t.Fatal("expected a move to fail when destination is not writable")
	}

	canonPath, folderPath := pathsOf(t, database, id)
	if canonPath != srcPath {
		t.Errorf("canonical_path = %q, want %q (rename failure should roll back)", canonPath, srcPath)
	}
	if folderPath != "" {
		t.Errorf("folder_path = %q, want empty", folderPath)
	}
	pathRow := canonicalPathRow(t, database, id)
	if pathRow != srcPath {
		t.Errorf("image_paths.path = %q, want %q", pathRow, srcPath)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("source file gone after failed move: %v", err)
	}
}

func TestMoveImage_RejectsAliasPathCollision(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "src.png")
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), srcPath, ""); err != nil {
		t.Fatalf("ingest src: %v", err)
	}
	var srcID int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(galleryDir, "dst")
	mustMkdirAll(t, dstDir, 0o755)
	otherPath := createTestPNGFileSize(t, galleryDir, "other.png", 11, 11)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), otherPath, ""); err != nil {
		t.Fatal(err)
	}
	var otherID int64
	if err := database.Read.QueryRow(`SELECT id FROM images WHERE id != ?`, srcID).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, otherID, filepath.Join(dstDir, "src.png"))

	_, err := moveTo(database, galleryDir, srcID, "dst")
	if err == nil {
		t.Fatal("expected move to fail on alias collision")
	}
	if !strings.Contains(err.Error(), "collides with an existing alias") {
		t.Errorf("error %v should call out the alias collision shape", err)
	}
}

func TestMoveImage_AFailedAliasProbeRefusesTheMove(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "pic.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID
	mustExec(t, database.Write, `DROP TABLE image_paths`)

	if _, err := moveTo(database, galleryDir, id, "dst"); err == nil {
		t.Fatal("a move whose collision probe failed should refuse")
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("the refused move took the file anyway: %v", err)
	}
}

func TestMoveImage_SameFolderIsNoop(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	sub := filepath.Join(galleryDir, "here")
	mustMkdirAll(t, sub, 0o755)
	srcPath := createTestPNGFile(t, sub, "x.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID

	res, err := moveTo(database, galleryDir, id, "here")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.NewCanonicalPath != srcPath {
		t.Errorf("new canonical = %q, want unchanged %q", res.NewCanonicalPath, srcPath)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("file moved unexpectedly: %v", err)
	}
}

func TestRenameImage_InPlace(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID

	res, err := renameTo(database, galleryDir, id, "renamed")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	wantPath := filepath.Join(galleryDir, "renamed.png")
	if res.NewCanonicalPath != wantPath || res.NewFolderPath != "" {
		t.Errorf("result = %+v, want path %q in root", res, wantPath)
	}

	canonPath, folderPath := pathsOf(t, database, id)
	if canonPath != wantPath || folderPath != "" {
		t.Errorf("row = %q %q, want %q in root", canonPath, folderPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Errorf("file still at old path (err=%v)", err)
	}
	aliasPath := canonicalPathRow(t, database, id)
	if aliasPath != wantPath {
		t.Errorf("canonical image_paths row = %q, want %q", aliasPath, wantPath)
	}
}

func TestRenameImage_OntoItsOwnStaleAlias(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "a.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID
	wantPath := filepath.Join(galleryDir, "b.png")
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, wantPath)

	res, err := renameTo(database, galleryDir, id, "b.png")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if res.NewCanonicalPath != wantPath || res.Suffixed {
		t.Errorf("result = %+v, want %q unsuffixed", res, wantPath)
	}
	var n, canonical int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(is_canonical), 0) FROM image_paths WHERE image_id = ?`, id,
	).Scan(&n, &canonical); err != nil {
		t.Fatal(err)
	}
	if n != 1 || canonical != 1 {
		t.Errorf("image_paths rows = %d (canonical %d), want the one canonical row", n, canonical)
	}
}

func TestRenameImage_ValidatesAndKeepsExtension(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "pic.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID

	for _, bad := range []string{"", "sub/dir", "..", "."} {
		if _, err := renameTo(database, galleryDir, id, bad); err == nil {
			t.Errorf("a rename to %q should refuse", bad)
		}
	}

	if res, err := renameTo(database, galleryDir, id, "pic"); err != nil || res.NewCanonicalPath != srcPath {
		t.Errorf("no-op rename = %+v, %v", res, err)
	}

	res, err := renameTo(database, galleryDir, id, "cover.jpg")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if want := filepath.Join(galleryDir, "cover.jpg.png"); res.NewCanonicalPath != want {
		t.Errorf("canonical = %q, want %q", res.NewCanonicalPath, want)
	}
}

func TestRenameImage_BoundsAnOverlongName(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "pic.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "")
	id := rec.ID

	done := make(chan error, 1)
	go func() {
		_, err := renameTo(database, galleryDir, id, strings.Repeat("a", 5000))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rename did not return: the numbering loop has no exit")
	}

	var canonical string
	if err := database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if got := len(filepath.Base(canonical)); got > maxNameBytes+len(".png") {
		t.Errorf("filed under a %d-byte name, want it bounded at %d", got, maxNameBytes)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("the renamed file is not where the row says: %v", err)
	}
}

func TestRenameImage_CollisionAutosuffixes(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFileSize(t, galleryDir, "pic.png", 10, 10)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), srcPath, ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	createTestPNGFileSize(t, galleryDir, "taken.png", 11, 10)
	createTestPNGFileSize(t, galleryDir, "taken01.png", 12, 10)
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	res, err := renameTo(database, galleryDir, id, "taken")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if want := filepath.Join(galleryDir, "taken02.png"); res.NewCanonicalPath != want {
		t.Errorf("canonical = %q, want %q", res.NewCanonicalPath, want)
	}
}

func TestPlaceImage_FolderAndNameInOneStep(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "incoming.PNG")

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")

	folder, name := "inbox/2026-08", "filed"
	res := mustPlaceImage(t, database, galleryDir, img.ID, &folder, &name)
	want := filepath.Join(galleryDir, "inbox", "2026-08", "filed.PNG")
	if res.NewCanonicalPath != want {
		t.Errorf("new path = %q, want %q", res.NewCanonicalPath, want)
	}
	if res.NewFolderPath != "inbox/2026-08" {
		t.Errorf("folder = %q, want inbox/2026-08", res.NewFolderPath)
	}
	canonical, folderPath := pathsOf(t, database, img.ID)
	if canonical != want || folderPath != "inbox/2026-08" {
		t.Errorf("row = %q / %q, want %q / inbox/2026-08", canonical, folderPath, want)
	}
	if _, statErr := os.Stat(want); statErr != nil {
		t.Errorf("file not at the placed path: %v", statErr)
	}
	if _, statErr := os.Stat(srcPath); !os.IsNotExist(statErr) {
		t.Errorf("file still at the old path (err=%v)", statErr)
	}
}

func TestPlaceImage_HalfAtATimeLeavesTheOtherHalf(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "half.png")
	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")

	name := "renamed"
	res := mustPlaceImage(t, database, galleryDir, img.ID, nil, &name)
	if want := filepath.Join(galleryDir, "renamed.png"); res.NewCanonicalPath != want {
		t.Errorf("new path = %q, want %q", res.NewCanonicalPath, want)
	}

	folder := "shelf"
	res = mustPlaceImage(t, database, galleryDir, img.ID, &folder, nil)
	if want := filepath.Join(galleryDir, "shelf", "renamed.png"); res.NewCanonicalPath != want {
		t.Errorf("new path = %q, want %q", res.NewCanonicalPath, want)
	}

	res = mustPlaceImage(t, database, galleryDir, img.ID, &folder, &name)
	if want := filepath.Join(galleryDir, "shelf", "renamed.png"); res.NewCanonicalPath != want {
		t.Errorf("no-op moved the file to %q", res.NewCanonicalPath)
	}
}

func TestPlaceImage_CollisionSuffixesInsteadOfOverwriting(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	first := createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
	second := createTestPNGFileSize(t, galleryDir, "b.png", 11, 12)

	imgA, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, first, "upload")
	imgB, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, second, "upload")

	folder, name := "box", "same"
	if _, err := PlaceImage(database, NewBoundary(galleryDir, nil, nil), imgA.ID, &folder, &name); err != nil {
		t.Fatalf("place a: %v", err)
	}
	resB := mustPlaceImage(t, database, galleryDir, imgB.ID, &folder, &name)
	if resB.NewCanonicalPath == filepath.Join(galleryDir, "box", "same.png") {
		t.Fatal("the second file took the first one's path")
	}
	for _, p := range []string{
		filepath.Join(galleryDir, "box", "same.png"),
		resB.NewCanonicalPath,
	} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%q is gone: %v", p, statErr)
		}
	}
}

func TestPlaceImage_NameThatDoesNotChangeIsNotARename(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFileSize(t, galleryDir, "shot.png", 10, 10)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), srcPath, ""); err != nil {
		t.Fatalf("ingest src: %v", err)
	}
	dstDir := filepath.Join(galleryDir, "dst")
	mustMkdirAll(t, dstDir, 0o755)
	createTestPNGFileSize(t, dstDir, "shot.png", 11, 10)

	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	folder, same := "dst", "shot"
	res := mustPlaceImage(t, database, galleryDir, id, &folder, &same)
	if res.Renamed {
		t.Error("a name equal to the one the file has reported a rename")
	}
	if !res.Moved {
		t.Error("the folder changed but Moved is false")
	}
	if got := filepath.Base(res.NewCanonicalPath); got != "shot_1.png" {
		t.Errorf("collision suffix = %q, want the move's shot_1.png", got)
	}

	otherPath := createTestPNGFileSize(t, galleryDir, "other.png", 12, 10)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), otherPath, ""); err != nil {
		t.Fatalf("ingest other: %v", err)
	}
	var otherID int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id DESC LIMIT 1`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	res = mustPlaceImage(t, database, galleryDir, otherID, &folder, &same)
	if !res.Renamed || !res.Moved {
		t.Errorf("Moved/Renamed = %v/%v, want both", res.Moved, res.Renamed)
	}
	if got := filepath.Base(res.NewCanonicalPath); got != "shot01.png" {
		t.Errorf("collision suffix = %q, want the rename's shot01.png", got)
	}
}

func TestPlaceImage_RefusesAFolderAnotherGalleryOwns(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	inner := filepath.Join(galleryDir, ".H")
	mustMkdirAll(t, inner, 0o755)
	ownPath := createTestPNGFileSize(t, galleryDir, "own.png", 10, 10)
	theirPath := createTestPNGFileSize(t, inner, "theirs.png", 12, 12)
	own, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, ownPath, "")
	theirs, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, theirPath, "")
	b := NewBoundary(galleryDir, []Fence{{Path: inner, Owner: "H", Kind: FenceGallery}}, nil)

	into := ".H/sub"
	if _, err := PlaceImage(database, b, own.ID, &into, nil); err == nil || !strings.Contains(err.Error(), "belongs to gallery H") {
		t.Errorf("move into the nested gallery = %v, want a refusal naming it", err)
	}
	out := "elsewhere"
	if _, err := PlaceImage(database, b, theirs.ID, &out, nil); err == nil || !strings.Contains(err.Error(), "belongs to gallery H") {
		t.Errorf("move out of the nested gallery = %v, want a refusal naming it", err)
	}
	for _, p := range []string{ownPath, theirPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s moved: %v", p, err)
		}
	}
}

func TestSetMissingTx_MovesUsageOncePerFlippedImage(t *testing.T) {
	database := newCollectionsTestDB(t)
	tagSvc := tags.New(database, nil)
	general, _, err := tags.CategoryIDByName(database, "general")
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := tagSvc.GetOrCreateTag("shared", general)
	solo, _ := tagSvc.GetOrCreateTag("solo", general)

	ids := make([]int64, 600)
	for i := range ids {
		ids[i] = insertImage(t, database, false)
		if err := tagSvc.AddTagToImage(ids[i], shared.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tagSvc.AddTagToImage(ids[0], solo.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	exact := func(label string) {
		t.Helper()
		if n, err := tags.RecalcDBCount(database); err != nil || n != 0 {
			t.Errorf("%s: the recount changed %d tags (%v), want none", label, n, err)
		}
	}
	for _, step := range []struct {
		name    string
		ids     []int64
		missing bool
		flipped int
	}{
		{"hide every image", ids, true, 600},
		{"hide them again", ids, true, 0},
		{"show one", ids[:1], false, 1},
		{"show all, one already shown", ids, false, 599},
		{"hide some", ids[:300], true, 300},
	} {
		var n int
		if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			var err error
			n, err = setMissingTx(tx, step.ids, step.missing)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if n != step.flipped {
			t.Errorf("%s: flipped %d, want %d", step.name, n, step.flipped)
		}
		exact(step.name)
	}

	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return repointCanonical(tx, ids[0], "/y.png", "", "", 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	exact("repoint a missing row")
	if got := tagUsage(t, database, solo.ID); got != 1 {
		t.Errorf("solo usage after the repoint = %d, want 1", got)
	}
}

func TestRepointCanonical_RefusesAPathAnotherImageOwns(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	a := createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
	b := createTestPNGFileSize(t, galleryDir, "b.png", 12, 12)
	recA, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, a, "")
	recB, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, b, "")

	err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return repointCanonical(tx, recB.ID, a, "", b, 0, 0)
	})
	if err == nil || !strings.Contains(err.Error(), "is a path of image") {
		t.Fatalf("repoint onto a.png = %v, want it refused", err)
	}
	if n := countRows(t, database, "image_paths WHERE path = ? AND image_id = ? AND is_canonical = 1", a, recA.ID); n != 1 {
		t.Error("a.png's row changed hands")
	}
	if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ?", recB.ID, b); n != 1 {
		t.Error("the refused image's row moved")
	}
}

func TestPlaceImage_TwoPlacementsOntoOneNameKeepBoth(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	b := NewBoundary(galleryDir, nil, nil)
	const n = 8
	var imgs []*models.Image
	for i := range n {
		img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath,
			createTestPNGFileSize(t, galleryDir, fmt.Sprintf("src%d.png", i), 10+i, 10), "")
		imgs = append(imgs, img)
	}
	mustMkdirAll(t, filepath.Join(galleryDir, "box"), 0o755)

	folder, name := "box", "same"
	var wg sync.WaitGroup
	for _, img := range imgs {
		wg.Go(func() {
			if _, err := PlaceImage(database, b, img.ID, &folder, &name); err != nil {
				t.Errorf("place %d: %v", img.ID, err)
			}
		})
	}
	wg.Wait()

	for _, img := range imgs {
		var path string
		if err := database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, img.ID).Scan(&path); err != nil {
			t.Fatal(err)
		}
		if sha, err := HashFile(path); err != nil || sha != img.SHA256 {
			t.Errorf("image %d's file %s holds other bytes (err=%v)", img.ID, path, err)
		}
	}
}

func TestPlannedPath_TakesAPathAnEarlierRowFrees(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	y, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "2.png", 10, 10), "")
	x, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "1.png", 11, 11), "")
	b := NewBoundary(galleryDir, nil, nil)
	planned := map[string]bool{}
	three, two := "3.png", "2.png"
	if _, _, err := PlannedPath(database, b, y.ID, nil, &three, planned); err != nil {
		t.Fatal(err)
	}

	got, numbered, err := PlannedPath(database, b, x.ID, nil, &two, planned)

	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(galleryDir, "2.png"); got != want || numbered {
		t.Errorf("planned %q (numbered %v), want %q as the run places it", got, numbered, want)
	}
}

func TestPlannedPath_RefusesAnotherImagesStaleAlias(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFile(t, galleryDir, "a.png"), "")
	other, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "other.png", 11, 11), "")
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, other.ID, filepath.Join(galleryDir, "b.png"))
	name := "b.png"

	_, _, err := PlannedPath(database, NewBoundary(galleryDir, nil, nil), rec.ID, nil, &name, map[string]bool{})

	if err == nil || !strings.Contains(err.Error(), "collides with an existing alias") {
		t.Errorf("err = %v, want the refusal the run gives", err)
	}
}

func TestFollows_ANumberATakenNameEarnedStillFollows(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	first, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "a.png", 10, 10), "")
	second, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "b.png", 11, 11), "")
	b := NewBoundary(galleryDir, nil, nil)
	same := "same"
	mustPlaceImage(t, database, galleryDir, first.ID, nil, &same)
	if res := mustPlaceImage(t, database, galleryDir, second.ID, nil, &same); !res.Suffixed {
		t.Fatalf("second placement = %q, want it numbered", res.NewCanonicalPath)
	}

	for _, id := range []int64{first.ID, second.ID} {
		if ok, err := Follows(database, b, id, nil, &same); err != nil || !ok {
			t.Errorf("Follows(%d) = %v, %v; want true, or a re-run renumbers it", id, ok, err)
		}
	}
	other, box := "other", "box"
	if ok, _ := Follows(database, b, first.ID, nil, &other); ok {
		t.Error("a different name reported as followed")
	}
	if ok, _ := Follows(database, b, first.ID, &box, &same); ok {
		t.Error("a row in another folder reported as followed")
	}
}

func TestFollows_ANameEndingInDigitsFollowsOnlyWhileTheBareNameIsTaken(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "img2024.png", 10, 10), "")
	b := NewBoundary(galleryDir, nil, nil)
	want := "img"
	if ok, err := Follows(database, b, rec.ID, nil, &want); err != nil || ok {
		t.Errorf("Follows with no img.png = %v, %v; want false, as nothing numbered it", ok, err)
	}
	createTestPNGFileSize(t, galleryDir, "img.png", 11, 11)
	if ok, err := Follows(database, b, rec.ID, nil, &want); err != nil || !ok {
		t.Errorf("Follows with img.png taken = %v, %v; want true", ok, err)
	}
}

func TestNumberedFrom(t *testing.T) {
	for _, tc := range []struct {
		base, want string
		ok         bool
	}{
		{"same01.png", "same.png", true},
		{"same123.png", "same.png", true},
		{"same_1.png", "same.png", true},
		{"same.png", "same.png", false},
		{"same1.png", "same.png", false},
		{"same_.png", "same.png", false},
		{"same01.jpg", "same.png", false},
		{"samex1.png", "same.png", false},
		{"other01.png", "same.png", false},
	} {
		if got := numberedFrom(tc.base, tc.want); got != tc.ok {
			t.Errorf("numberedFrom(%q, %q) = %v, want %v", tc.base, tc.want, got, tc.ok)
		}
	}
}

func pathsOf(t *testing.T, database *db.DB, id int64) (canonical, folder string) {
	t.Helper()
	if err := database.Read.QueryRow(`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id).Scan(&canonical, &folder); err != nil {
		t.Fatal(err)
	}
	return canonical, folder
}

func canonicalPathRow(t *testing.T, database *db.DB, id int64) string {
	t.Helper()
	var path string
	if err := database.Read.QueryRow(`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	return path
}
