package gallery

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/tags"
)

func TestApplyReplacedFile_ClearsPhashWhenThumbnailRegenFails(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	orig := createTestPNGFileSize(t, galleryDir, "orig.png", 40, 40)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, orig, "")
	if _, err := RecomputeAndStorePhash(t.Context(), database, rec.ID, env.thumbnailsPath); err != nil {
		t.Fatal(err)
	}
	var before sql.NullInt64
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, rec.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !before.Valid {
		t.Fatal("test premise broken: the row has no phash to go stale")
	}

	staged := createTestPNGFileSize(t, t.TempDir(), "new.png", 60, 60)
	blocked := filepath.Join(t.TempDir(), "thumbs")
	mustWriteFile(t, blocked, []byte("not a directory"), 0o644)

	sha, sum := mustDigests(t, staged)
	if _, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), blocked), rec.ID, staged, sha, sum, "png"); err != nil {
		t.Fatalf("ApplyReplacedFile: %v", err)
	}

	var after sql.NullInt64
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, rec.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after.Valid {
		t.Errorf("phash = %d after a failed regen, want NULL so the backfill picks it up", after.Int64)
	}
}

func TestApplyReplacedFile_ClearsTheAutoTagStamp(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	orig := createTestPNGFileSize(t, galleryDir, "orig.png", 40, 40)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, orig, "")
	if _, err := database.Write.Exec(`UPDATE images SET auto_tagged_at = '2026-01-01T00:00:00Z' WHERE id = ?`, rec.ID); err != nil {
		t.Fatal(err)
	}

	staged := createTestPNGFileSize(t, t.TempDir(), "new.png", 60, 60)
	sha, sum := mustDigests(t, staged)
	if _, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, staged, sha, sum, "png"); err != nil {
		t.Fatalf("ApplyReplacedFile: %v", err)
	}

	var stamp sql.NullString
	if err := database.Read.QueryRow(`SELECT auto_tagged_at FROM images WHERE id = ?`, rec.ID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp.Valid {
		t.Errorf("auto_tagged_at = %q after a replace, want NULL so the nightly auto-tag takes the new bytes", stamp.String)
	}
}

func TestApplyReplacedFile_KeepsTheOldFileWhenTheNewNameCannotBeCreated(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	oldPath := createTestPNGFile(t, galleryDir, strings.Repeat("a", 251)+".png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, oldPath, "")
	oldBytes := mustReadFile(t, oldPath)
	staged := filepath.Join(t.TempDir(), "new.webp")
	mustWriteFile(t, staged, append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 52)...), 0o600)
	sha, sum := mustDigests(t, staged)

	_, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, staged, sha, sum, "webp")

	if err == nil {
		t.Fatal("the replace was expected to fail")
	}
	if got, err := os.ReadFile(oldPath); err != nil || !bytes.Equal(got, oldBytes) {
		t.Fatalf("the old file is not in place: %v", err)
	}
	if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ?", rec.ID, oldPath); n != 1 {
		t.Error("the row does not point at the old file")
	}
	env.sync(t, database)
	if n := countRows(t, database, "images WHERE id = ? AND sha256 = ? AND is_missing = 0", rec.ID, rec.SHA256); n != 1 {
		t.Error("the sync did not keep the row on the old bytes")
	}
}

func TestPutOldFileBack_RemovesTheReservedNewName(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	oldPath := createTestPNGFile(t, galleryDir, "old.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, oldPath, "")
	backupPath := filepath.Join(t.TempDir(), "staged.old")
	mustRename(t, oldPath, backupPath)
	newPath := filepath.Join(galleryDir, "old.webp")
	mustWriteFile(t, newPath, nil, 0o644)
	mustExec(t, database.Write, `UPDATE images SET canonical_path = ? WHERE id = ?`, newPath, rec.ID)
	mustExec(t, database.Write, `UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`, newPath, rec.ID)

	putOldFileBack(database, galleryDir, rec.ID, oldPath, newPath, backupPath, true)

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("the reserved %s is still there (stat err %v)", filepath.Base(newPath), err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("the old file is not back: %v", err)
	}
	if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ?", rec.ID, oldPath); n != 1 {
		t.Error("the row does not point at the old file")
	}
}

func TestMoveIntoPlace_OnlyFallsBackAcrossDevices(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	mustWriteFile(t, src, []byte("bytes"), 0o644)
	locked := filepath.Join(dir, "locked")
	mustMkdirAll(t, locked, 0o755)
	dst := filepath.Join(locked, "dst.png")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if err := moveIntoPlace(src, dst); err == nil {
		t.Fatal("a rename into an unwritable directory should surface, not copy")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the source went missing after a refused move: %v", err)
	}
}

func TestApplyReplacedFile_RefusesASourceOutsideTheGallery(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	orig := createTestPNGFileSize(t, galleryDir, "orig.png", 40, 40)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, orig, "")
	outside := createTestPNGFileSize(t, t.TempDir(), "outside.png", 40, 40)
	mustExec(t, database.Write, `UPDATE images SET canonical_path = ? WHERE id = ?`, outside, rec.ID)

	staged := createTestPNGFileSize(t, t.TempDir(), "new.png", 60, 60)
	sha, sum := mustDigests(t, staged)
	if _, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, staged, sha, sum, "png"); err == nil {
		t.Fatal("a replace onto a path outside the gallery root should refuse")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the refused replace moved the outside file anyway: %v", err)
	}
}

func TestApplyReplacedFile_LeavesTheFileGroupAndWorldReadable(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	orig := createTestPNGFileSize(t, galleryDir, "orig.png", 40, 40)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, orig, "")

	staged, err := os.CreateTemp(t.TempDir(), "replace-*.png")
	if err != nil {
		t.Fatal(err)
	}
	stagedPath := staged.Name()
	_ = staged.Close()
	src := createTestPNGFileSize(t, t.TempDir(), "new.png", 60, 60)
	bytes := mustReadFile(t, src)
	mustWriteFile(t, stagedPath, bytes, 0o600)

	sha, sum := mustDigests(t, stagedPath)
	if _, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, stagedPath, sha, sum, "png"); err != nil {
		t.Fatalf("ApplyReplacedFile: %v", err)
	}
	var storedMD5 string
	if err := database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, rec.ID).Scan(&storedMD5); err != nil {
		t.Fatal(err)
	}
	if storedMD5 != sum {
		t.Errorf("md5 = %q, want the replacement's digest %q", storedMD5, sum)
	}

	var canonical string
	if err := database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, rec.ID).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	fi := mustStat(t, canonical)
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("replaced file mode = %04o, want 0644", got)
	}
}

func TestMoveIntoPlace_UndoesTheCopyWhenTheSourceCannotGo(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the directory's write bit")
	}
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src, dst := filepath.Join(srcDir, "a.png"), filepath.Join(dstDir, "a.png")
	mustWriteFile(t, src, []byte("bytes"), 0o644)
	if err := os.Chmod(srcDir, 0o555); err != nil {
		t.Skipf("cannot make the source directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(srcDir, 0o755) })

	if err := moveIntoPlace(src, dst); err == nil {
		t.Fatal("a move that could not unlink its source reported success")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("the copy was left behind, so the file is in two places")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the source went anyway: %v", err)
	}
}

func TestApplyReplacedFile_ShowsAMissingRowWithItsTagCounts(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	orig := createTestPNGFileSize(t, galleryDir, "orig.png", 40, 40)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, orig, "")
	tagSvc := tags.New(database, nil)
	general, _, err := tags.CategoryIDByName(database, "general")
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := tagSvc.GetOrCreateTag("kept", general)
	if err := tagSvc.AddTagToImage(rec.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := SetMissing(database, rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := tagUsage(t, database, tag.ID); got != 0 {
		t.Fatalf("usage while missing = %d, want 0", got)
	}

	staged := createTestPNGFileSize(t, t.TempDir(), "new.png", 60, 60)
	sha, sum := mustDigests(t, staged)
	if _, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, staged, sha, sum, "png"); err != nil {
		t.Fatalf("ApplyReplacedFile: %v", err)
	}
	if got := imageMissing(t, database, rec.ID); got != 0 {
		t.Errorf("is_missing after the replace = %d, want 0", got)
	}
	if got := tagUsage(t, database, tag.ID); got != 1 {
		t.Errorf("usage after the replace = %d, want 1", got)
	}
}

func TestApplyReplacedFile_ReplacesARowWhoseFileIsGone(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	orig := createTestPNGFileSize(t, galleryDir, "orig.png", 40, 40)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, orig, "")
	mustRemove(t, orig)
	if _, err := SetMissing(database, rec.ID, true); err != nil {
		t.Fatal(err)
	}

	staged := createTestPNGFileSize(t, t.TempDir(), "new.png", 60, 60)
	sha, sum := mustDigests(t, staged)
	if _, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, staged, sha, sum, "png"); err != nil {
		t.Fatalf("ApplyReplacedFile: %v", err)
	}
	if got := mustReadFile(t, orig); len(got) == 0 {
		t.Error("the new file is not at the row's path")
	}
	if n := countRows(t, database, "images WHERE id = ? AND sha256 = ? AND is_missing = 0", rec.ID, sha); n != 1 {
		t.Error("the row does not carry the new bytes, shown")
	}
}

func TestApplyReplacedFile_RefusesARekeyedNameOnTheIgnoreList(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	oldPath := createTestPNGFile(t, galleryDir, "shot.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, oldPath, "")
	oldBytes := mustReadFile(t, oldPath)
	staged := createTestGIF(t, t.TempDir(), "new.gif", 20, 20)
	sha, sum := mustDigests(t, staged)

	h := boundHandle(database, NewBoundary(galleryDir, nil, []string{"*.gif"}), env.thumbnailsPath)
	_, err := ApplyReplacedFile(h, rec.ID, staged, sha, sum, "gif")
	if _, ok := errors.AsType[Exclusion](err); !ok {
		t.Fatalf("err = %v, want the ignore list's refusal", err)
	}
	if got := mustReadFile(t, oldPath); !bytes.Equal(got, oldBytes) {
		t.Error("the old file was not left in place")
	}
	if _, err := os.Stat(filepath.Join(galleryDir, "shot.gif")); !os.IsNotExist(err) {
		t.Errorf("a file landed on the ignored name (%v)", err)
	}
	var path, sha256 string
	if err := database.Read.QueryRow(`SELECT canonical_path, sha256 FROM images WHERE id = ?`, rec.ID).Scan(&path, &sha256); err != nil {
		t.Fatal(err)
	}
	if path != oldPath || sha256 != rec.SHA256 {
		t.Errorf("row = (%q, %s), want it unchanged", path, sha256)
	}
}

func TestApplyReplacedFile_RefusesAnotherImagesStaleAlias(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	oldPath := createTestPNGFile(t, galleryDir, "shot.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, oldPath, "")
	other, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, createTestPNGFileSize(t, galleryDir, "other.png", 11, 11), "")
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, other.ID, filepath.Join(galleryDir, "shot.gif"))
	oldBytes := mustReadFile(t, oldPath)
	staged := createTestGIF(t, t.TempDir(), "new.gif", 20, 20)
	sha, sum := mustDigests(t, staged)

	_, err := ApplyReplacedFile(boundHandle(database, NewBoundary(galleryDir, nil, nil), env.thumbnailsPath), rec.ID, staged, sha, sum, "gif")

	if err == nil || !strings.Contains(err.Error(), "collides with an existing alias") {
		t.Fatalf("err = %v, want the alias collision named", err)
	}
	if got := mustReadFile(t, oldPath); !bytes.Equal(got, oldBytes) {
		t.Error("the old file was not left in place")
	}
	if _, err := os.Stat(filepath.Join(galleryDir, "shot.gif")); !os.IsNotExist(err) {
		t.Errorf("the reservation was left behind (%v)", err)
	}
}

func mustDigests(t *testing.T, path string) (sha, sum string) {
	t.Helper()
	sha, sum, err := hashFileDigests(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha, sum
}
