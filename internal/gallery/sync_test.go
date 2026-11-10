package gallery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type syncEnv struct {
	galleryPath    string
	thumbnailsPath string
	maxFileSizeMB  int
	naming         Naming
	fences         []Fence
}

func (e *syncEnv) boundary() *Boundary { return NewBoundary(e.galleryPath, e.fences, nil) }

func setupSyncTest(t *testing.T) (*db.DB, *syncEnv, string) {
	t.Helper()
	tmpDir := t.TempDir()
	galleryDir := filepath.Join(tmpDir, "gallery")
	mustMkdirAll(t, galleryDir, 0755)
	thumbDir := filepath.Join(tmpDir, "thumbs")
	mustMkdirAll(t, thumbDir, 0755)

	database := newCollectionsTestDB(t)

	env := &syncEnv{
		galleryPath:    galleryDir,
		thumbnailsPath: thumbDir,
		maxFileSizeMB:  100,
	}
	return database, env, galleryDir
}

func (e *syncEnv) sync(t *testing.T, database *db.DB) SyncResult {
	t.Helper()
	r, err := Sync(context.Background(), boundHandle(database, e.boundary(), e.thumbnailsPath), e.maxFileSizeMB, e.naming, func(int, int, string) {}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func createTestPNGFile(t *testing.T, dir, name string) string {
	t.Helper()
	return createTestPNGFileSize(t, dir, name, 10, 10)
}

func createTestPNGFileSize(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(dir, name)
	f := mustCreate(t, path)
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSync_NewFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")

	result := env.sync(t, database)
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1", result.Added)
	}
}

func TestSyncSignalsAChangeOnlyWhenRowsChanged(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	for i := range 60 {
		createTestPNGFileSize(t, galleryDir, fmt.Sprintf("f%02d.png", i), 4+i, 4)
	}
	signals := 0
	run := func() {
		t.Helper()
		if _, err := Sync(context.Background(), boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, env.naming, func(int, int, string) {}, func() { signals++ }, nil); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if signals == 0 {
		t.Error("a sync that added 60 rows never signalled a change")
	}
	signals = 0
	run()
	if signals != 0 {
		t.Errorf("a sync that changed nothing signalled %d change(s)", signals)
	}
}

func TestIngest_RecordsOrigin(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	for i, tc := range []struct {
		name   string
		origin string
		want   string
	}{
		{"default", "", "ingest"},
		{"upload", "upload", "upload"},
		{"url", "https://example.com/pic", "https://example.com/pic"},
	} {
		t.Run(tc.name, func(t *testing.T) {

			path := createTestPNGFileSize(t, galleryDir, tc.name+".png", 10+i, 10+i)
			rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, tc.origin)
			var got string
			if err := database.Read.QueryRow(`SELECT origin FROM images WHERE id = ?`, rec.ID).Scan(&got); err != nil {
				t.Fatalf("scan origin: %v", err)
			}
			if got != tc.want {
				t.Errorf("origin = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIngest_RefusesBytesThatAreNotMedia(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	path := filepath.Join(galleryDir, "not_really_a_png.png")
	mustWriteFile(t, path, []byte("not an image"), 0o644)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err == nil {
		t.Fatal("ingest of non-media bytes should fail")
	}
	rows := countRows(t, database, "images")
	if rows != 0 {
		t.Errorf("images = %d, want 0", rows)
	}
}

func TestIngest_RederivesAFileRewrittenInPlace(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "edited.png", 10, 10)

	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	tagID := insertTag(t, database, "kept", "general", 0)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, rec.ID, tagID)

	_ = os.Remove(path)
	createTestPNGFileSize(t, galleryDir, "edited.png", 40, 25)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err != nil {
		t.Fatalf("re-ingest after the edit: %v", err)
	}

	rows := countRows(t, database, "images")
	if rows != 1 {
		t.Errorf("images = %d, want the one row re-derived in place", rows)
	}
	var sha string
	var w, h int
	if err := database.Read.QueryRow(
		`SELECT sha256, width, height FROM images WHERE id = ?`, rec.ID,
	).Scan(&sha, &w, &h); err != nil {
		t.Fatal(err)
	}
	if sha == rec.SHA256 || w != 40 || h != 25 {
		t.Errorf("row = %s %dx%d, want the new bytes' hash and 40x25", sha[:8], w, h)
	}
	kept := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", rec.ID, tagID)
	if kept != 1 {
		t.Errorf("seeded tag on the row = %d, want the edit to keep it", kept)
	}
}

func TestIngest_AMissingRowRewrittenInPlaceIsShownAgain(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "back.png", 10, 10)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")

	tagID := insertTag(t, database, "back_tag", "general", 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`, rec.ID, tagID)
	if _, err := SetMissing(database, rec.ID, true); err != nil {
		t.Fatal(err)
	}

	mustRemove(t, path)
	createTestPNGFileSize(t, galleryDir, "back.png", 40, 25)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err != nil {
		t.Fatalf("ingest of the rewritten file: %v", err)
	}

	if got := imageMissing(t, database, rec.ID); got != 0 {
		t.Errorf("is_missing = %d, want the re-read row shown again", got)
	}
	if got := tagUsage(t, database, tagID); got != 1 {
		t.Errorf("usage_count = %d, want 1", got)
	}
}

func TestIngest_RecordsTheTypeTheBytesDeclare(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	path := createTestPNGFile(t, galleryDir, "mismatch.jpg")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	if rec.FileType != "png" {
		t.Errorf("FileType = %q, want %q", rec.FileType, "png")
	}
	var stored, canonical string
	if err := database.Read.QueryRow(
		`SELECT file_type, canonical_path FROM images WHERE id = ?`, rec.ID,
	).Scan(&stored, &canonical); err != nil {
		t.Fatal(err)
	}
	if stored != "png" {
		t.Errorf("stored file_type = %q, want %q", stored, "png")
	}
	if canonical != path {
		t.Errorf("canonical_path = %q, want %q", canonical, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file no longer at the path the operator gave it: %v", err)
	}
}

func TestSync_MismatchedExtensionStaysVisible(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "mismatch.jpg")

	result := env.sync(t, database)
	if result.Added != 1 || result.Removed != 0 {
		t.Errorf("added = %d, missing = %d, want 1 and 0", result.Added, result.Removed)
	}
	var fileType string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT file_type, is_missing FROM images`,
	).Scan(&fileType, &isMissing); err != nil {
		t.Fatal(err)
	}
	if fileType != "png" {
		t.Errorf("file_type = %q, want %q", fileType, "png")
	}
	if isMissing != 0 {
		t.Errorf("is_missing = %d, want 0", isMissing)
	}
}

func writeA1111PNG(t *testing.T, path, params string) {
	t.Helper()
	raw := mustReadFile(t, createTestPNGFileSize(t, t.TempDir(), "src.png", 12, 12))
	body := append([]byte("tEXt"), append([]byte("parameters\x00"), params...)...)
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(body)-4))
	chunk.Write(body)
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(body))

	const afterIHDR = 8 + 4 + 4 + 13 + 4
	out := append([]byte{}, raw[:afterIHDR]...)
	out = append(out, chunk.Bytes()...)
	out = append(out, raw[afterIHDR:]...)
	mustWriteFile(t, path, out, 0o644)
}

func TestSync_InPlaceEditRefreshesGenerationMetadata(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "recipe.png")
	env.sync(t, database)

	var id int64
	var sourceType string
	if err := database.Read.QueryRow(`SELECT id, source_type FROM images`).Scan(&id, &sourceType); err != nil {
		t.Fatal(err)
	}
	if sourceType != "none" {
		t.Fatalf("seeded source_type = %q, want none", sourceType)
	}

	writeA1111PNG(t, path, "a prompt\nNegative prompt: bad\nSteps: 12, Sampler: Euler, CFG scale: 6, Seed: 7, Model: mdl")
	env.sync(t, database)

	if err := database.Read.QueryRow(`SELECT source_type FROM images WHERE id = ?`, id).Scan(&sourceType); err != nil {
		t.Fatal(err)
	}
	if sourceType != "a1111" {
		t.Errorf("source_type = %q, want a1111", sourceType)
	}
	var prompt, model string
	if err := database.Read.QueryRow(
		`SELECT prompt, model FROM sd_metadata WHERE image_id = ?`, id,
	).Scan(&prompt, &model); err != nil {
		t.Fatalf("sd_metadata was not re-extracted: %v", err)
	}
	if prompt != "a prompt" || model != "mdl" {
		t.Errorf("sd_metadata = (%q, %q), want (\"a prompt\", \"mdl\")", prompt, model)
	}

	plain := mustReadFile(t, createTestPNGFileSize(t, t.TempDir(), "plain.png", 17, 17))
	mustWriteFile(t, path, plain, 0o644)
	env.sync(t, database)

	if err := database.Read.QueryRow(`SELECT source_type FROM images WHERE id = ?`, id).Scan(&sourceType); err != nil {
		t.Fatal(err)
	}
	if sourceType != "none" {
		t.Errorf("post-strip source_type = %q, want none", sourceType)
	}
	rows := countRows(t, database, "sd_metadata WHERE image_id = ?", id)
	if rows != 0 {
		t.Errorf("sd_metadata rows = %d, want 0", rows)
	}
}

func TestIngest_AcceptsMP4BrandedOnlyAsCompatible(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	path := filepath.Join(galleryDir, "clip.mp4")
	if err := os.WriteFile(path, []byte{
		0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, 'i', 's', 'o', '5', 0x00, 0x00, 0x02, 0x00,
		'i', 's', 'o', '6', 'm', 'p', '4', '1',
	}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	rows := countRows(t, database, "images")
	if rows != 1 {
		t.Errorf("images = %d, want 1", rows)
	}
}

func TestIngest_ReactivationRestoresUsageCount(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	path := createTestPNGFile(t, galleryDir, "reappear.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")

	tagID := insertTag(t, database, "reappear_tag", "general", 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`, rec.ID, tagID)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, rec.ID)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = 0 WHERE id = ?`, tagID)

	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err != nil {
		t.Fatalf("re-Ingest: %v", err)
	}
	if got := tagUsage(t, database, tagID); got != 1 {
		t.Errorf("usage_count after reactivation = %d, want 1", got)
	}
}

func TestIngest_ReactivatesAnImportPlaceholderWithItsSizeAndDimensions(t *testing.T) {
	for _, name := range []string{"placed.png", "elsewhere.png"} {
		t.Run(name, func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			body, id := insertImportPlaceholder(t, database, galleryDir, "placed.png")
			path := filepath.Join(galleryDir, name)
			mustWriteFile(t, path, body, 0o644)

			img, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, "")
			if err != nil {
				t.Fatal(err)
			}
			if img.ID != id || img.FileSize != int64(len(body)) || img.Phash == nil {
				t.Errorf("Ingest = id %d, size %d, phash %v; want id %d, size %d and a phash", img.ID, img.FileSize, img.Phash, id, len(body))
			}
			checkPlaceholderRead(t, database, env, id, path, body)
		})
	}
}

func TestIngest_AnAliasRecordsItsMtime(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	original := createTestPNGFile(t, galleryDir, "original.png")
	mustIngest(t, database, galleryDir, env.thumbnailsPath, original, "")
	copyPath := filepath.Join(galleryDir, "copy.png")
	mustWriteFile(t, copyPath, mustReadFile(t, original), 0o644)

	mustIngest(t, database, galleryDir, env.thumbnailsPath, copyPath, "")

	if got, want := indexedAt(t, database, copyPath).MtimeNano, mustStat(t, copyPath).ModTime().UnixNano(); got != want {
		t.Errorf("alias mtime_nsec = %d, want %d (file mtime)", got, want)
	}
}

func TestIngest_PromotesAliasWhenCanonicalGone(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	oldPath := createTestPNGFile(t, galleryDir, "before.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, oldPath, "")

	movedDir := filepath.Join(galleryDir, "relocated")
	mustMkdirAll(t, movedDir, 0o755)
	bytes := mustReadFile(t, oldPath)
	newPath := filepath.Join(movedDir, "before.png")
	mustWriteFile(t, newPath, bytes, 0o644)
	mustRemove(t, oldPath)

	if _, isDup, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), newPath, ""); err != nil {
		t.Fatalf("re-Ingest: %v", err)
	} else if isDup {
		t.Errorf("expected promotion (not isDup) when canonical was gone")
	}

	var canonPath, folderPath string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, rec.ID,
	).Scan(&canonPath, &folderPath, &isMissing); err != nil {
		t.Fatal(err)
	}
	if canonPath != newPath {
		t.Errorf("canonical_path = %q, want %q", canonPath, newPath)
	}
	if folderPath != "relocated" {
		t.Errorf("folder_path = %q, want %q", folderPath, "relocated")
	}
	if isMissing != 0 {
		t.Errorf("is_missing = %d, want 0", isMissing)
	}

	if n := countRows(t, database, `image_paths WHERE path = ? AND is_canonical = 1`, newPath); n != 1 {
		t.Errorf("canonical rows at the new path = %d, want 1", n)
	}
	if n := countRows(t, database, `image_paths WHERE path = ? AND is_canonical = 1`, oldPath); n != 0 {
		t.Errorf("canonical rows at the old path = %d, want 0 (demoted or gone)", n)
	}
}

func TestIngest_AFileRenamedOverAnotherIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
	}{
		{"renamed over it", false},
		{"a missing image's bytes copied over it", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			a := createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
			b := createTestPNGFileSize(t, galleryDir, "b.png", 12, 12)
			recA, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, a, "")
			recB, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, b, "")
			if tc.missing {
				mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, recB.ID)
				mustWriteFile(t, a, mustReadFile(t, b), 0o644)
				mustRemove(t, b)
			} else {
				mustRename(t, b, a)
			}

			if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), a, ""); err == nil {
				t.Error("Ingest(a.png) = nil error, want the conflict refused")
			}

			if n := countRows(t, database, "images WHERE canonical_path = ?", a); n != 1 {
				t.Errorf("images on a.png = %d, want 1", n)
			}
			if n := countRows(t, database, "image_paths WHERE path = ? AND image_id = ? AND is_canonical = 1", a, recA.ID); n != 1 {
				t.Error("a.png's row no longer belongs to its image")
			}
			if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ? AND is_missing = ?", recB.ID, b, tc.missing); n != 1 {
				t.Error("the other image's row was moved onto a.png")
			}
		})
	}
}

func TestIngest_ARewrittenAliasTakesTheBytesOwner(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ownerGone     bool
		wantCanonical bool
	}{
		{"the owner's file is there", false, false},
		{"the owner's file is gone", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			c := createTestPNGFileSize(t, galleryDir, "c.png", 10, 10)
			d := createTestPNGFileSize(t, galleryDir, "d.png", 12, 12)
			recC, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, c, "")
			recD, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, d, "")
			alias := filepath.Join(galleryDir, "d-copy.png")
			mustWriteFile(t, alias, mustReadFile(t, d), 0o644)
			if _, dup := mustIngest(t, database, galleryDir, env.thumbnailsPath, alias, ""); !dup {
				t.Fatal("expected the copy to ingest as a duplicate")
			}
			mustWriteFile(t, alias, mustReadFile(t, c), 0o644)
			if tc.ownerGone {
				mustRemove(t, c)
			}

			mustIngest(t, database, galleryDir, env.thumbnailsPath, alias, "")

			if n := countRows(t, database, "image_paths WHERE path = ? AND image_id = ? AND is_canonical = ?", alias, recC.ID, tc.wantCanonical); n != 1 {
				t.Errorf("the rewritten copy is not image %d's (canonical %v)", recC.ID, tc.wantCanonical)
			}
			if n := countRows(t, database, "image_paths WHERE image_id = ?", recD.ID); n != 1 {
				t.Errorf("image %d's paths = %d, want its own file only", recD.ID, n)
			}
		})
	}
}

func TestDropDuplicateCopy_KeepsTheRowsOwnFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	canonical := createTestPNGFile(t, galleryDir, "kept.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, canonical, "")

	DropDuplicateCopy(database, rec.ID, canonical, "test")
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical file was unlinked: %v", err)
	}
	if n := countRows(t, database, "image_paths WHERE image_id = ? AND is_canonical = 1", rec.ID); n != 1 {
		t.Errorf("canonical image_paths rows = %d, want 1", n)
	}

	copyPath := filepath.Join(galleryDir, "copy.png")
	mustWriteFile(t, copyPath, mustReadFile(t, canonical), 0o644)
	if _, isDup := mustIngest(t, database, galleryDir, env.thumbnailsPath, copyPath, ""); !isDup {
		t.Fatal("expected the copy to ingest as a duplicate")
	}

	DropDuplicateCopy(database, rec.ID, copyPath, "test")
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Errorf("duplicate copy survived (err=%v)", err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("canonical file went with the copy: %v", err)
	}
}

func TestDropDuplicateCopy_KeepsTheOnlyCopyWhenTheCanonicalIsGone(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	canonical := createTestPNGFile(t, galleryDir, "gone.png")
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, canonical, "")
	copyPath := filepath.Join(galleryDir, "copy.png")
	mustWriteFile(t, copyPath, mustReadFile(t, canonical), 0o644)
	if _, isDup := mustIngest(t, database, galleryDir, env.thumbnailsPath, copyPath, ""); !isDup {
		t.Fatal("expected the copy to ingest as a duplicate")
	}

	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}

	DropDuplicateCopy(database, rec.ID, copyPath, "test")

	if _, err := os.Stat(copyPath); err != nil {
		t.Errorf("the only copy was deleted: %v", err)
	}
	if n := countRows(t, database, "image_paths WHERE image_id = ? AND path = ?", rec.ID, copyPath); n != 1 {
		t.Errorf("the copy's path row is gone")
	}
}

func TestSync_ARewrittenCopyBecomesAnImageOfItsOwn(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	original := createTestPNGFile(t, galleryDir, "b.png")
	env.sync(t, database)
	var sha string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&sha); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(galleryDir, "a.png")
	mustWriteFile(t, copyPath, mustReadFile(t, original), 0o644)
	env.sync(t, database)
	if n := countRows(t, database, "image_paths"); n != 2 {
		t.Fatalf("image_paths = %d, want the copy recorded beside its original", n)
	}
	createTestPNGFileSize(t, galleryDir, "a.png", 12, 12)

	env.sync(t, database)

	if n := countRows(t, database, "images"); n != 2 {
		t.Errorf("images = %d, want the rewritten copy as an image of its own", n)
	}
	if n := countRows(t, database, "images WHERE sha256 = ? AND canonical_path = ?", sha, original); n != 1 {
		t.Error("the original's row no longer describes its own file")
	}
	if again := env.sync(t, database); again != (SyncResult{}) {
		t.Errorf("the next sync = %+v, want nothing to do", again)
	}
}

func TestSync_AFileRenamedOverAnotherIsAConflict(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	a := createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
	b := createTestPNGFileSize(t, galleryDir, "b.png", 12, 12)
	env.sync(t, database)
	bID := imageIDByPath(t, database, b)
	mustRename(t, b, a)

	if r := env.sync(t, database); r.Conflicts != 1 || !strings.Contains(r.Summary(), "1 conflicted (a.png)") {
		t.Errorf("conflicts = %d, summary %q, want 1 naming a.png", r.Conflicts, r.Summary())
	}
	if n := countRows(t, database, "images WHERE canonical_path = ?", a); n != 1 {
		t.Errorf("images on a.png = %d, want 1", n)
	}
	if n := countRows(t, database, "images WHERE id = ? AND canonical_path = ? AND is_missing = 1", bID, b); n != 1 {
		t.Error("the renamed image's row is not left missing at its own path")
	}
	aID := imageIDByPath(t, database, a)
	if got := RewrittenBy(database.Read, aID); got != bID {
		t.Errorf("RewrittenBy(a) = %d, want %d, whose bytes a.png now holds", got, bID)
	}
	if got := RewrittenBy(database.Read, bID); got != 0 {
		t.Errorf("RewrittenBy(missing b) = %d, want 0", got)
	}
}

func TestSync_ARewrittenAliasTakesTheBytesOwner(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ownerGone     bool
		wantCanonical bool
	}{
		{"the owner's file is there", false, false},
		{"the owner's file is gone", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			c := createTestPNGFileSize(t, galleryDir, "c.png", 10, 10)
			d := createTestPNGFileSize(t, galleryDir, "d.png", 12, 12)
			alias := filepath.Join(galleryDir, "z-copy.png")
			mustWriteFile(t, alias, mustReadFile(t, d), 0o644)
			env.sync(t, database)
			cID, dID := imageIDByPath(t, database, c), imageIDByPath(t, database, d)
			mustWriteFile(t, alias, mustReadFile(t, c), 0o644)
			if tc.ownerGone {
				mustRemove(t, c)
			}

			env.sync(t, database)

			if n := countRows(t, database, "image_paths WHERE path = ? AND image_id = ? AND is_canonical = ?", alias, cID, tc.wantCanonical); n != 1 {
				t.Errorf("the rewritten copy is not image %d's (canonical %v)", cID, tc.wantCanonical)
			}
			if n := countRows(t, database, "image_paths WHERE image_id = ?", dID); n != 1 {
				t.Errorf("image %d's paths = %d, want its own file only", dID, n)
			}
		})
	}
}

func TestSync_AnAliasSplitOffAnEditedCanonicalGetsItsMD5(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	a := createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
	b := filepath.Join(galleryDir, "b.png")
	mustWriteFile(t, b, mustReadFile(t, a), 0o644)
	env.sync(t, database)

	createTestPNGFileSize(t, galleryDir, "a.png", 13, 13)

	env.sync(t, database)

	want, err := Md5File(b)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, "images WHERE canonical_path = ? AND md5 = ?", b, want); n != 1 {
		t.Errorf("the row split off for b.png lacks its md5 %s", want)
	}
}

func TestSync_SummaryCountsRestoredAndReReadRows(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "back.png")
	createTestPNGFileSize(t, galleryDir, "edited.png", 11, 11)
	env.sync(t, database)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE canonical_path LIKE '%back.png'`)
	createTestPNGFileSize(t, galleryDir, "edited.png", 13, 13)

	res := env.sync(t, database)

	if res.Reactivated != 1 || res.Edited != 1 {
		t.Errorf("result = %+v, want one restored and one re-read", res)
	}
	if got, want := res.Summary(), "0 added, 0 missing, 0 moved, 1 restored, 1 re-read"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestSync_AnImageBackUnderTwoNamesIsRestoredOnce(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	original := createTestPNGFile(t, galleryDir, "b.png")
	env.sync(t, database)

	mustWriteFile(t, filepath.Join(galleryDir, "a.png"), mustReadFile(t, original), 0o644)
	env.sync(t, database)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1`)

	res := env.sync(t, database)

	if res.Reactivated != 1 {
		t.Errorf("restored = %d, want the one image once", res.Reactivated)
	}
	if n := countRows(t, database, "images WHERE is_missing = 0"); n != 1 {
		t.Errorf("visible images = %d, want 1", n)
	}
}

func TestSync_AFileOverALoweredCapKeepsItsRow(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	mustWriteFile(t, filepath.Join(galleryDir, "big.png"), noisePNG(t, 700, 700), 0o644)
	env.sync(t, database)
	env.maxFileSizeMB = 1

	res := env.sync(t, database)

	if res.Removed != 0 {
		t.Errorf("Removed = %d, want the skipped file's row kept", res.Removed)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%big.png' AND is_missing = 0"); n != 1 {
		t.Error("the file over the cap was marked missing")
	}
}

func TestSync_AnUnreadableFolderKeepsItsRows(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	database, env, galleryDir := setupSyncTest(t)
	sub := filepath.Join(galleryDir, "sub")
	mustMkdirAll(t, sub, 0o755)
	createTestPNGFile(t, sub, "inside.png")
	env.sync(t, database)
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	res := env.sync(t, database)

	if res.Removed != 0 {
		t.Errorf("Removed = %d, want the unreadable folder's row kept", res.Removed)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%inside.png' AND is_missing = 0"); n != 1 {
		t.Error("a file in an unreadable folder was marked missing")
	}
}

func TestSync_ACanonicalBehindALinkIntoAnotherGalleryMovesOntoItsCopy(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	other := t.TempDir()
	createTestPNGFile(t, other, "pic.png")
	mustSymlink(t, other, filepath.Join(galleryDir, "lnk"))
	env.sync(t, database)
	copyPath := filepath.Join(galleryDir, "copy.png")
	mustWriteFile(t, copyPath, mustReadFile(t, filepath.Join(other, "pic.png")), 0o644)
	env.sync(t, database)
	env.fences = []Fence{{Path: other, Owner: "other", Kind: FenceGallery}}

	if res := env.sync(t, database); res.Moved != 1 {
		t.Errorf("result = %+v, want the row moved onto its copy", res)
	}
	if n := countRows(t, database, "images WHERE canonical_path = ? AND is_missing = 0", copyPath); n != 1 {
		t.Error("the row does not stand on its copy")
	}
	if again := env.sync(t, database); again != (SyncResult{}) {
		t.Errorf("the next sync = %+v, want nothing to do", again)
	}
}

func TestSync_DropsACopyBehindALinkIntoAnotherGallery(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	original := createTestPNGFile(t, galleryDir, "pic.png")
	other := t.TempDir()
	mustWriteFile(t, filepath.Join(other, "copy.png"), mustReadFile(t, original), 0o644)
	mustSymlink(t, other, filepath.Join(galleryDir, "lnk"))
	env.sync(t, database)
	if n := countRows(t, database, "image_paths"); n != 2 {
		t.Fatalf("image_paths = %d, want the copy recorded", n)
	}
	env.fences = []Fence{{Path: other, Owner: "other", Kind: FenceGallery}}

	env.sync(t, database)

	if n := countRows(t, database, "image_paths WHERE path LIKE '%lnk%'"); n != 0 {
		t.Error("the copy in another gallery's folder kept its row")
	}
}

func TestSync_DropsASecondNameForTheCanonicalFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	outer := t.TempDir()
	mustMkdirAll(t, filepath.Join(outer, "sub"), 0o755)
	createTestPNGFile(t, filepath.Join(outer, "sub"), "x.png")
	mustSymlink(t, outer, filepath.Join(galleryDir, "a_link"))
	env.sync(t, database)

	mustSymlink(t, filepath.Join(outer, "sub"), filepath.Join(galleryDir, "b_link"))
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) SELECT id, ?, 0 FROM images`, filepath.Join(galleryDir, "b_link", "x.png"))

	env.sync(t, database)

	if n := countRows(t, database, "image_paths"); n != 1 {
		t.Errorf("image_paths = %d, want only the canonical", n)
	}
}

func TestSync_RunsPastADanglingLinkOnTheIgnoreList(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "local.png")
	elsewhere := filepath.Join(t.TempDir(), "share")
	mustMkdirAll(t, elsewhere, 0o755)
	createTestPNGFileSize(t, elsewhere, "shared.png", 12, 12)
	mustSymlink(t, elsewhere, filepath.Join(galleryDir, "linked"))
	env.sync(t, database)
	mustRemoveAll(t, elsewhere)

	_, err := Sync(context.Background(), boundHandle(database, NewBoundary(galleryDir, nil, []string{"linked"}), env.thumbnailsPath), env.maxFileSizeMB, Naming{}, func(int, int, string) {}, nil, nil)

	if err != nil {
		t.Fatalf("a dangling link the gallery ignores stopped the sync: %v", err)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%local.png' AND is_missing = 0"); n != 1 {
		t.Error("the local image did not stay present")
	}
}

func TestSync_NoChange(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")

	env.sync(t, database)
	result := env.sync(t, database)
	if result.Added != 0 || result.Removed != 0 {
		t.Errorf("expected no changes, got Added=%d Removed=%d", result.Added, result.Removed)
	}
}

func TestSync_AnUnchangedLibraryWritesNoPathRow(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFileSize(t, galleryDir, "a.png", 10, 10)
	b := createTestPNGFileSize(t, galleryDir, "b.png", 12, 12)
	mustWriteFile(t, filepath.Join(galleryDir, "b-copy.png"), mustReadFile(t, b), 0644)
	env.sync(t, database)

	mustExec(t, database.Write, `CREATE TABLE path_updates (n INTEGER)`)
	mustExec(t, database.Write, `CREATE TRIGGER count_path_updates AFTER UPDATE ON image_paths BEGIN INSERT INTO path_updates VALUES (1); END`)
	env.sync(t, database)
	if n := countRows(t, database, "path_updates"); n != 0 {
		t.Errorf("second sync updated %d image_paths rows, want 0", n)
	}
}

func TestSync_MoveDropsPriorPath(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "moved")
	mustMkdirAll(t, subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "wander.png")
	env.sync(t, database)

	newPath := filepath.Join(subDir, "wander.png")
	mustRename(t, original, newPath)
	env.sync(t, database)

	paths, err := db.QueryStrings(database.Read, `SELECT path FROM image_paths ORDER BY is_canonical DESC, path`)
	if err != nil {
		t.Fatal(err)
	}
	canonicals := countRows(t, database, "image_paths WHERE is_canonical = 1")
	if len(paths) != 1 {
		t.Errorf("image_paths rows = %d, want 1 (old path dropped); got paths = %v", len(paths), paths)
	}
	if canonicals != 1 {
		t.Errorf("canonical rows = %d, want exactly 1; paths = %v", canonicals, paths)
	}
	if len(paths) == 1 && paths[0] != newPath {
		t.Errorf("surviving path = %q, want %q", paths[0], newPath)
	}
}

func TestSync_PrunesStaleAliasPath(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	canonical := createTestPNGFile(t, galleryDir, "keep.png")
	data := mustReadFile(t, canonical)
	mustWriteFile(t, filepath.Join(galleryDir, "copy.png"), data, 0o644)
	env.sync(t, database)

	var imgID int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&imgID); err != nil {
		t.Fatal(err)
	}

	var aliasPath, canonPath string
	if err := database.Read.QueryRow(`SELECT path FROM image_paths WHERE is_canonical = 0`).Scan(&aliasPath); err != nil {
		t.Fatalf("expected one non-canonical alias after sync: %v", err)
	}
	if err := database.Read.QueryRow(`SELECT path FROM image_paths WHERE is_canonical = 1`).Scan(&canonPath); err != nil {
		t.Fatal(err)
	}

	mustRemove(t, aliasPath)
	env.sync(t, database)

	remaining := countRows(t, database, "image_paths WHERE path = ?", aliasPath)
	if remaining != 0 {
		t.Errorf("stale alias row survived sync: %d rows for %q", remaining, aliasPath)
	}
	canonCount := countRows(t, database, "image_paths WHERE path = ? AND is_canonical = 1", canonPath)
	if canonCount != 1 {
		t.Errorf("canonical row count = %d, want 1", canonCount)
	}
	if isMissing := imageMissing(t, database, imgID); isMissing != 0 {
		t.Errorf("image marked missing = %d, want 0 (canonical still on disk)", isMissing)
	}
}

func TestPruneStaleAliasPaths_KeepsPresentButUnwalked(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	canonical := createTestPNGFile(t, galleryDir, "keep.png")
	env.sync(t, database)

	var imgID int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&imgID); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(galleryDir, "present-dup.png")
	mustWriteFile(t, present, []byte("x"), 0o644)
	gone := filepath.Join(galleryDir, "gone-dup.png")
	for _, p := range []string{present, gone} {
		mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, imgID, p)
	}

	foundPaths := map[string]struct{}{canonical: {}}
	if err := pruneStaleAliasPaths(context.Background(), database, env.boundary(), foundPaths); err != nil {
		t.Fatal(err)
	}

	presentCount := countRows(t, database, "image_paths WHERE path = ?", present)
	if presentCount != 1 {
		t.Errorf("present alias row count = %d, want 1 (file still on disk)", presentCount)
	}
	goneCount := countRows(t, database, "image_paths WHERE path = ?", gone)
	if goneCount != 0 {
		t.Errorf("gone alias row count = %d, want 0", goneCount)
	}
}

func TestSync_DetectsSameSizeInPlaceEdit(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "edit.png")

	env.sync(t, database)
	var origSHA string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&origSHA); err != nil {
		t.Fatal(err)
	}

	info := mustStat(t, path)

	overwrite := mustReadFile(t, path)
	for i := 8; i < len(overwrite); i++ {
		overwrite[i] = byte((i * 7) ^ 0x5a)
	}
	mustWriteFile(t, path, overwrite, 0o644)

	future := info.ModTime().Add(2 * time.Second)
	mustChtimes(t, path, future, future)

	env.sync(t, database)

	var nextSHA string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&nextSHA); err != nil {
		t.Fatal(err)
	}
	if nextSHA == origSHA {
		t.Errorf("post-edit SHA still %q; expected the row to be re-hashed", nextSHA)
	}
}

func TestInPlaceEdit_AFailedThumbnailClearsThePhash(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "rehash.png", 10, 10)
	env.sync(t, database)
	var id int64
	var before sql.NullInt64
	if err := database.Read.QueryRow(`SELECT id, phash FROM images`).Scan(&id, &before); err != nil {
		t.Fatal(err)
	}
	if !before.Valid {
		t.Fatal("the first sync stored no phash")
	}

	info := mustStat(t, path)
	createTestPNGFileSize(t, galleryDir, "rehash.png", 20, 20)
	future := info.ModTime().Add(2 * time.Second)
	mustChtimes(t, path, future, future)
	thumb := ThumbnailPath(env.thumbnailsPath, id)
	mustRemove(t, thumb)
	mustMkdirAll(t, filepath.Join(thumb, "blocker"), 0o755)

	var reported []*int64
	sink := PhashSink(func(imageID int64, phash *int64) {
		if imageID == id {
			reported = append(reported, phash)
		}
	})
	if _, err := Sync(context.Background(), boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, env.naming, func(int, int, string) {}, nil, sink); err != nil {
		t.Fatal(err)
	}
	var after sql.NullInt64
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after.Valid {
		t.Errorf("phash = %d, want NULL: the old bytes' hash outlived the edit", after.Int64)
	}
	if len(reported) != 1 || reported[0] != nil {
		t.Errorf("the sink got %v, want one cleared phash", reported)
	}
}

func TestSync_InPlaceEditOntoAnExistingSHAIsReported(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	keep := createTestPNGFileSize(t, galleryDir, "keep.png", 12, 12)
	edited := createTestPNGFile(t, galleryDir, "edited.png")
	env.sync(t, database)

	var editedID int64
	var beforeSHA string
	if err := database.Read.QueryRow(
		`SELECT id, sha256 FROM images WHERE canonical_path = ?`, edited,
	).Scan(&editedID, &beforeSHA); err != nil {
		t.Fatal(err)
	}

	twin := mustReadFile(t, keep)
	mustWriteFile(t, edited, twin, 0o644)
	future := time.Now().Add(2 * time.Second)
	mustChtimes(t, edited, future, future)

	result := env.sync(t, database)
	if result.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", result.Conflicts)
	}
	var afterSHA string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images WHERE id = ?`, editedID).Scan(&afterSHA); err != nil {
		t.Fatalf("the edited row did not survive the sync: %v", err)
	}
	if afterSHA != beforeSHA {
		t.Errorf("sha = %q, want the row left at %q", afterSHA, beforeSHA)
	}

	if second := env.sync(t, database); second.Conflicts != 1 {
		t.Errorf("second sync conflicts = %d, want 1", second.Conflicts)
	}
}

func TestSync_DetectsAnEditInsideTheObservedSecond(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "same-second.png")

	base := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	mustChtimes(t, path, base, base)
	env.sync(t, database)
	var origSHA string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&origSHA); err != nil {
		t.Fatal(err)
	}

	overwrite := mustReadFile(t, path)
	for i := 8; i < len(overwrite); i++ {
		overwrite[i] = byte((i * 11) ^ 0x3c)
	}
	mustWriteFile(t, path, overwrite, 0o644)
	sameSecond := base.Add(500 * time.Millisecond)
	mustChtimes(t, path, sameSecond, sameSecond)

	env.sync(t, database)

	var nextSHA string
	if err := database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&nextSHA); err != nil {
		t.Fatal(err)
	}
	if nextSHA == origSHA {
		t.Error("an edit inside the observed second was skipped by the unchanged-shortcut")
	}
}

func TestSync_InPlaceEditClearsAStaleDuration(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "wasvideo.png")
	env.sync(t, database)
	mustExec(t, database.Write,
		`UPDATE images SET file_type = 'mp4', duration_seconds = 12.5`)

	mustWriteFile(t, path, mustReadFile(t, createTestPNGFileSize(t, t.TempDir(), "src.png", 40, 25)), 0o644)
	env.sync(t, database)

	var fileType string
	var duration sql.NullFloat64
	if err := database.Read.QueryRow(
		`SELECT file_type, duration_seconds FROM images`,
	).Scan(&fileType, &duration); err != nil {
		t.Fatal(err)
	}
	if fileType != "png" {
		t.Fatalf("file_type = %q, want png", fileType)
	}
	if duration.Valid {
		t.Errorf("duration_seconds = %v, want NULL on a row that is no longer a video", duration.Float64)
	}
}

func TestSync_InPlaceEditFollowsTheNewType(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "swap.png")
	env.sync(t, database)

	var fileType string
	if err := database.Read.QueryRow(`SELECT file_type FROM images`).Scan(&fileType); err != nil {
		t.Fatal(err)
	}
	if fileType != "png" {
		t.Fatalf("seeded file_type = %q, want png", fileType)
	}

	jpegBytes := mustReadFile(t, createTestJPEG(t, t.TempDir(), "src.jpg", 20, 20))
	mustWriteFile(t, path, jpegBytes, 0o644)
	env.sync(t, database)

	var got, canonical string
	if err := database.Read.QueryRow(
		`SELECT file_type, canonical_path FROM images`,
	).Scan(&got, &canonical); err != nil {
		t.Fatal(err)
	}
	if got != "jpeg" {
		t.Errorf("file_type = %q, want jpeg", got)
	}
	if canonical != path {
		t.Errorf("canonical_path = %q, want %q", canonical, path)
	}
}

func TestSync_InPlaceCbzEditClearsPageCache(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := writeTestZip(t, galleryDir, "book.cbz", map[string][]byte{
		"001.png": solidPNG(t, 20, 20, [3]uint8{10, 20, 30}),
		"002.png": solidPNG(t, 20, 20, [3]uint8{40, 50, 60}),
	})

	env.sync(t, database)
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(mangaImageDir(env.thumbnailsPath, id), "page_0001.png")
	mustWriteFile(t, stale, []byte("stale page bytes"), 0o644)

	info := mustStat(t, path)
	writeTestZip(t, galleryDir, "book.cbz", map[string][]byte{
		"001.png": solidPNG(t, 30, 30, [3]uint8{200, 100, 0}),
		"002.png": solidPNG(t, 30, 30, [3]uint8{0, 100, 200}),
		"003.png": solidPNG(t, 30, 30, [3]uint8{100, 0, 100}),
	})
	future := info.ModTime().Add(2 * time.Second)
	mustChtimes(t, path, future, future)

	env.sync(t, database)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale raw page cache survived the in-place edit (err=%v)", err)
	}
}

func TestSync_InPlaceEditOfACBZRereadsComicInfo(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	book := func(title, series string) map[string][]byte {
		return map[string][]byte{
			"001.png":       solidPNG(t, 20, 20, [3]uint8{10, 20, 30}),
			"ComicInfo.xml": []byte("<ComicInfo><Title>" + title + "</Title><Series>" + series + "</Series></ComicInfo>"),
		}
	}
	path := writeTestZip(t, galleryDir, "book.cbz", book("First", "Old Series"))
	env.sync(t, database)

	info := mustStat(t, path)
	writeTestZip(t, galleryDir, "book.cbz", book("Second", "New Series"))
	future := info.ModTime().Add(2 * time.Second)
	mustChtimes(t, path, future, future)
	if r := env.sync(t, database); r.Edited != 1 {
		t.Fatalf("Edited = %d, want 1", r.Edited)
	}

	var title, metaSeries, series string
	if err := database.Read.QueryRow(
		`SELECT m.title, m.series, i.series FROM manga_metadata m JOIN images i ON i.id = m.image_id`,
	).Scan(&title, &metaSeries, &series); err != nil {
		t.Fatal(err)
	}
	if title != "Second" || metaSeries != "New Series" {
		t.Errorf("manga_metadata = %q / %q, want the rewritten archive's Second / New Series", title, metaSeries)
	}
	if series != "Old Series" {
		t.Errorf("images.series = %q, want Old Series (never overwritten)", series)
	}
}

func TestSync_ComicInfoSeriesInAnotherCaseJoinsTheExistingCollection(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	book := func(series string, shade uint8) map[string][]byte {
		return map[string][]byte{
			"001.png":       solidPNG(t, 20, 20, [3]uint8{shade, 20, 30}),
			"ComicInfo.xml": []byte("<ComicInfo><Series>" + series + "</Series></ComicInfo>"),
		}
	}
	writeTestZip(t, galleryDir, "first.cbz", book("Saga", 10))
	env.sync(t, database)
	writeTestZip(t, galleryDir, "second.cbz", book("saga", 90))
	env.sync(t, database)

	names, err := db.QueryStrings(database.Read, `SELECT DISTINCT name COLLATE BINARY FROM image_collections`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"Saga"}) {
		t.Errorf("collection spellings = %v, want [Saga]", names)
	}
	series, err := db.QueryStrings(database.Read, `SELECT DISTINCT series FROM images`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(series, []string{"Saga"}) {
		t.Errorf("images.series spellings = %v, want [Saga]", series)
	}
}

func TestSync_FileDeleted(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "test.png")

	env.sync(t, database)
	mustRemove(t, path)
	result := env.sync(t, database)
	if result.Removed != 1 {
		t.Errorf("Removed = %d, want 1", result.Removed)
	}

	var isMissing int
	if err := database.Read.QueryRow(`SELECT is_missing FROM images`).Scan(&isMissing); err != nil {
		t.Fatal(err)
	}
	if isMissing != 1 {
		t.Error("image not marked as missing")
	}
}

func TestSync_RefusesAnUnreadableRoot(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")
	env.sync(t, database)

	mustRemoveAll(t, galleryDir)
	if _, err := Sync(context.Background(), boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, Naming{}, func(int, int, string) {}, nil, nil); err == nil {
		t.Fatal("sync over a vanished root should fail")
	}

	missing := countRows(t, database, "images WHERE is_missing = 1")
	if missing != 0 {
		t.Errorf("%d row(s) flagged missing by a refused sync, want 0", missing)
	}
}

func TestSync_RefusesALinkedFolderThatStoppedResolving(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "local.png")
	elsewhere := filepath.Join(t.TempDir(), "share")
	mustMkdirAll(t, elsewhere, 0o755)
	createTestPNGFileSize(t, elsewhere, "shared.png", 12, 12)
	mustSymlink(t, elsewhere, filepath.Join(galleryDir, "linked"))
	if got := env.sync(t, database).Added; got != 2 {
		t.Fatalf("Added = %d, want 2", got)
	}

	mustRemoveAll(t, elsewhere)
	if _, err := Sync(context.Background(), boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, Naming{}, func(int, int, string) {}, nil, nil); err == nil {
		t.Fatal("sync behind a link that stopped resolving should fail")
	}
	if missing := countRows(t, database, "images WHERE is_missing = 1"); missing != 0 {
		t.Errorf("%d row(s) flagged missing by a refused sync, want 0", missing)
	}
}

func TestSync_ADeletedFolderStillMarksItsRowsMissing(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	sub := filepath.Join(galleryDir, "trip")
	mustMkdirAll(t, sub, 0o755)
	createTestPNGFile(t, sub, "a.png")
	env.sync(t, database)

	mustRemoveAll(t, sub)
	if got := env.sync(t, database).Removed; got != 1 {
		t.Errorf("Removed = %d, want 1", got)
	}
}

func TestSync_Duplicate(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "sub")
	mustMkdirAll(t, subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "original.png")
	content, _ := os.ReadFile(original)
	mustWriteFile(t, filepath.Join(subDir, "copy.png"), content, 0644)

	result := env.sync(t, database)
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1", result.Added)
	}
	if result.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", result.Duplicates)
	}
}

func TestSync_DuplicateAliasRecordsMtime(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "sub")
	mustMkdirAll(t, subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "original.png")
	content, _ := os.ReadFile(original)
	copyPath := filepath.Join(subDir, "copy.png")
	mustWriteFile(t, copyPath, content, 0644)

	env.sync(t, database)

	info := mustStat(t, copyPath)
	var mtime int64
	if err := database.Read.QueryRow(
		`SELECT mtime_unix FROM image_paths WHERE path = ?`, copyPath,
	).Scan(&mtime); err != nil {
		t.Fatal(err)
	}
	if mtime != info.ModTime().Unix() {
		t.Errorf("alias mtime_unix = %d, want %d (file mtime)", mtime, info.ModTime().Unix())
	}
}

func TestSync_ReactivatesAnImportPlaceholderWithItsSizeAndDimensions(t *testing.T) {
	for _, name := range []string{"placed.png", "elsewhere.png"} {
		t.Run(name, func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			body, id := insertImportPlaceholder(t, database, galleryDir, "placed.png")
			path := filepath.Join(galleryDir, name)
			mustWriteFile(t, path, body, 0o644)

			var stored *int64
			r, err := Sync(context.Background(), boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, env.naming,
				func(int, int, string) {}, nil, func(imageID int64, phash *int64) {
					if imageID == id {
						stored = phash
					}
				})
			if err != nil {
				t.Fatal(err)
			}
			if r.Reactivated != 1 || r.Added != 0 {
				t.Errorf("restored = %d, added = %d, want 1 and 0", r.Reactivated, r.Added)
			}
			if stored == nil {
				t.Error("the restored image's phash was not handed to the index")
			}
			checkPlaceholderRead(t, database, env, id, path, body)
		})
	}
}

func TestPlaceholderReadIsNotLoggedAsAnEdit(t *testing.T) {
	read := map[string]func(*testing.T, *db.DB, *syncEnv, string){
		"sync": func(t *testing.T, database *db.DB, env *syncEnv, _ string) { env.sync(t, database) },
		"ingest": func(t *testing.T, database *db.DB, env *syncEnv, path string) {
			mustIngest(t, database, env.galleryPath, env.thumbnailsPath, path, "")
		},
	}
	for name, readFile := range read {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			prev := logx.Get()
			logx.Set("info")
			t.Cleanup(func() { logx.Set(prev) })
			database, env, galleryDir := setupSyncTest(t)
			body, id := insertImportPlaceholder(t, database, galleryDir, "placed.png")
			path := filepath.Join(galleryDir, "placed.png")
			mustWriteFile(t, path, body, 0o644)

			readFile(t, database, env, path)
			got := buf.String()
			if strings.Contains(got, "in-place edit") || !strings.Contains(got, fmt.Sprintf("placeholder read: image id=%d", id)) {
				t.Errorf("log = %q, want the placeholder read named and no in-place edit", got)
			}
		})
	}
}

func TestSync_AMovedImageRecordsTheMtimeOfItsNewPath(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")
	env.sync(t, database)
	dstPath := filepath.Join(galleryDir, "moved.png")
	mustRename(t, srcPath, dstPath)

	if r := env.sync(t, database); r.Moved != 1 {
		t.Fatalf("Moved = %d, want 1", r.Moved)
	}

	if got, want := indexedAt(t, database, dstPath).MtimeNano, mustStat(t, dstPath).ModTime().UnixNano(); got != want {
		t.Errorf("mtime_nsec of the new path = %d, want %d (file mtime)", got, want)
	}
}

func insertImportPlaceholder(t *testing.T, database *db.DB, galleryDir, name string) ([]byte, int64) {
	t.Helper()
	src := createTestPNGFileSize(t, t.TempDir(), name, 12, 9)
	sha, _, err := hashFileDigests(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(galleryDir, name)
	var id int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing, source_type, origin)
		 VALUES (?, ?, '', 'png', 0, 1, 'none', 'ingest') RETURNING id`, sha, path,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`, id, path)
	return mustReadFile(t, src), id
}

func checkPlaceholderRead(t *testing.T, database *db.DB, env *syncEnv, id int64, path string, body []byte) {
	t.Helper()
	var size int64
	var width, height, phash sql.NullInt64
	var md5 sql.NullString
	if err := database.Read.QueryRow(
		`SELECT file_size, width, height, md5, phash FROM images WHERE id = ? AND is_missing = 0 AND canonical_path = ?`, id, path,
	).Scan(&size, &width, &height, &md5, &phash); err != nil {
		t.Fatalf("the restored row at %q: %v", path, err)
	}
	if size != int64(len(body)) || width.Int64 != 12 || height.Int64 != 9 || md5.String == "" || !phash.Valid {
		t.Errorf("restored row = size %d, %dx%d, md5 %q, phash %v; want size %d, 12x9, an md5 and a phash",
			size, width.Int64, height.Int64, md5.String, phash.Valid, len(body))
	}
	if _, err := os.Stat(ThumbnailPath(env.thumbnailsPath, id)); err != nil {
		t.Errorf("no thumbnail for the restored row: %v", err)
	}
	if UnlinkImageFile(NewBoundary(env.galleryPath, nil, nil), path, indexedAt(t, database, path), id) {
		t.Error("the restored file was reported changed since indexing")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the restored file survived the delete (%v)", err)
	}
}

func TestSync_SkipsLargeFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	bigPath := filepath.Join(galleryDir, "too_big.png")
	mustWriteFile(t, bigPath, noisePNG(t, 1024, 1024), 0o644)

	createTestPNGFileSize(t, galleryDir, "small.png", 10, 10)

	env.maxFileSizeMB = 1
	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Errorf("with cap=1 MiB expected 1 added (small), got %d", r1.Added)
	}
	env.maxFileSizeMB = 100
	r2 := env.sync(t, database)
	if r2.Added != 1 {
		t.Errorf("after raising cap expected 1 new add (big), got %d", r2.Added)
	}
}

func TestFolderPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		galleryPath string
		filePath    string
		want        string
	}{
		{"root", "/gallery", "/gallery/image.png", ""},
		{"nested", "/gallery", "/gallery/2024/jan/x.png", "2024/jan"},
		{"one level", "/gallery", "/gallery/sub/image.png", "sub"},
		{"trailing separator on the root", "/gallery/", "/gallery/sub/image.png", "sub"},
		{"outside the root", "/gallery", "/elsewhere/image.png", ""},
		{"unset root", "", "/gallery/sub/image.png", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FolderPath(tt.galleryPath, tt.filePath)
			if got != tt.want {
				t.Errorf("FolderPath(%q, %q) = %q, want %q", tt.galleryPath, tt.filePath, got, tt.want)
			}
		})
	}
}

func TestSync_FileMoved(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Fatalf("initial sync Added=%d, want 1", r1.Added)
	}

	subDir := filepath.Join(galleryDir, "sub")
	mustMkdirAll(t, subDir, 0755)
	dstPath := filepath.Join(subDir, "original.png")
	mustRename(t, srcPath, dstPath)

	r2 := env.sync(t, database)
	if r2.Moved != 1 {
		t.Errorf("Moved = %d, want 1", r2.Moved)
	}
}

func TestSync_AFileIngestedDuringTheWalkStaysVisible(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "walked.png")
	var landed string
	progress := func(_, _ int, message string) {
		if landed != "" || !strings.HasPrefix(message, "Phase 2") {
			return
		}
		landed = createTestPNGFileSize(t, galleryDir, "pushed.png", 12, 12)
		if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), landed, ""); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Sync(context.Background(), boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, env.naming, progress, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Removed != 0 {
		t.Errorf("Removed = %d, want 0", r.Removed)
	}
	if n := countRows(t, database, "images WHERE is_missing = 0 AND canonical_path = ?", landed); n != 1 {
		t.Error("the file landed during the walk was marked missing")
	}
}

func TestSync_ARepointWithTheOldCopyLeftMovesTheRowOnce(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	old := createTestPNGFile(t, galleryDir, "kept.png")
	env.sync(t, database)
	moved := filepath.Join(filepath.Dir(galleryDir), "new-disk")
	mustMkdirAll(t, moved, 0o755)
	mustWriteFile(t, filepath.Join(moved, "kept.png"), mustReadFile(t, old), 0o644)
	env.galleryPath = moved

	if r := env.sync(t, database); r.Moved != 1 || r.Removed != 0 {
		t.Fatalf("first sync after the repoint = %+v, want 1 moved and none missing", r)
	}
	if r := env.sync(t, database); r.Moved != 0 || r.Removed != 0 || r.Reactivated != 0 {
		t.Errorf("second sync = %+v, want nothing to do", r)
	}
	if n := countRows(t, database, "images WHERE is_missing = 0 AND canonical_path = ?", filepath.Join(moved, "kept.png")); n != 1 {
		t.Error("the image is not shown at the new root")
	}
}

func TestSync_AMissingImageFoundUnderANewPathCountsAsRestored(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")
	env.sync(t, database)
	data := mustReadFile(t, srcPath)
	mustRemove(t, srcPath)
	if r := env.sync(t, database); r.Removed != 1 {
		t.Fatalf("sync after the file went = %+v, want it missing", r)
	}

	mustWriteFile(t, filepath.Join(galleryDir, "elsewhere.png"), data, 0o644)
	r := env.sync(t, database)

	if r.Reactivated != 1 || r.Moved != 0 {
		t.Errorf("restored = %d, moved = %d, want 1 and 0", r.Reactivated, r.Moved)
	}
	if got, want := r.Summary(), "0 added, 0 missing, 0 moved, 1 restored"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%elsewhere.png' AND is_missing = 0"); n != 1 {
		t.Error("the image is not shown at its new path")
	}
}

func TestSync_AMoveWhoseWriteFailsIsNotCounted(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")
	env.sync(t, database)
	mustExec(t, database.Write, `CREATE TRIGGER refuse_repoint BEFORE UPDATE OF canonical_path ON images BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	mustRename(t, srcPath, filepath.Join(galleryDir, "moved.png"))

	r := env.sync(t, database)

	if r.Moved != 0 {
		t.Errorf("Moved = %d, want a refused repoint left uncounted", r.Moved)
	}
	if n := countRows(t, database, "images WHERE is_missing = 1"); n != 1 {
		t.Errorf("missing images = %d, want the row still on its gone path flagged", n)
	}
}

func TestSync_MoveTwinSameSHAKeepsBothPaths(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Fatalf("initial sync Added=%d, want 1", r1.Added)
	}

	data := mustReadFile(t, srcPath)
	mustRemove(t, srcPath)
	for _, name := range []string{"twin_a.png", "twin_b.png"} {
		mustWriteFile(t, filepath.Join(galleryDir, name), data, 0o644)
	}

	r2 := env.sync(t, database)
	if r2.Moved != 1 || r2.Duplicates != 1 {
		t.Errorf("Moved = %d, Duplicates = %d, want 1 and 1", r2.Moved, r2.Duplicates)
	}
	var canonical, aliases int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FILTER (WHERE is_canonical = 1), COUNT(*) FILTER (WHERE is_canonical = 0) FROM image_paths`,
	).Scan(&canonical, &aliases); err != nil {
		t.Fatal(err)
	}
	if canonical != 1 || aliases != 1 {
		t.Errorf("image_paths canonical = %d, aliases = %d, want 1 and 1", canonical, aliases)
	}
}

func TestFolderTree_Empty(t *testing.T) {
	database, _, _ := setupSyncTest(t)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 root node, got %d", len(nodes))
	}
	if nodes[0].Name != "(root)" {
		t.Errorf("root name = %q", nodes[0].Name)
	}
	if nodes[0].Count != 0 {
		t.Errorf("root count = %d, want 0", nodes[0].Count)
	}
}

func TestFolderTree_WithImages(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	createTestPNGFileSize(t, galleryDir, "root.png", 10, 10)

	subDir := filepath.Join(galleryDir, "sub")
	mustMkdirAll(t, subDir, 0755)
	createTestPNGFileSize(t, subDir, "sub.png", 11, 10)

	env.sync(t, database)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected nodes")
	}
	root := nodes[0]
	if root.Count < 2 {
		t.Errorf("root total count = %d, want >= 2", root.Count)
	}
	if len(root.Children) < 1 {
		t.Errorf("expected at least 1 child folder, got %d", len(root.Children))
	}
	found := false
	for _, child := range root.Children {
		if child.Name == "sub" {
			found = true
			if child.Count != 1 {
				t.Errorf("sub folder count = %d, want 1", child.Count)
			}
			if child.Depth != 1 {
				t.Errorf("sub folder depth = %d, want 1", child.Depth)
			}
		}
	}
	if !found {
		t.Error("sub folder not found in FolderTree")
	}
}

func TestFolderTree_RecursiveCount(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	parentDir := filepath.Join(galleryDir, "parent")
	subA := filepath.Join(parentDir, "a")
	subB := filepath.Join(parentDir, "b")
	mustMkdirAll(t, subA, 0755)
	mustMkdirAll(t, subB, 0755)
	createTestPNGFileSize(t, subA, "a.png", 10, 10)
	createTestPNGFileSize(t, subB, "b.png", 11, 10)

	env.sync(t, database)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	root := nodes[0]
	if root.Count != 2 {
		t.Errorf("root count = %d, want 2", root.Count)
	}
	var parent *FolderNode
	for i := range root.Children {
		if root.Children[i].Name == "parent" {
			parent = &root.Children[i]
		}
	}
	if parent == nil {
		t.Fatal("parent folder not found in tree")
	}
	if parent.Count != 2 {
		t.Errorf("parent count = %d, want 2 (recursive)", parent.Count)
	}
	for _, c := range parent.Children {
		if c.Count != 1 {
			t.Errorf("%s count = %d, want 1", c.Name, c.Count)
		}
	}
}

func TestSync_ContextCanceled(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	for i := 0; i < 32; i++ {
		createTestPNGFileSize(t, galleryDir, fmt.Sprintf("ctx_%02d.png", i), 10+i, 10)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Sync(ctx, boundHandle(database, env.boundary(), env.thumbnailsPath), env.maxFileSizeMB, Naming{}, func(int, int, string) {}, nil, nil)
	if err == nil {
		t.Fatal("Sync with a pre-cancelled ctx should surface the cancellation, got nil err")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestWatcher_IngestsFile(t *testing.T) {
	_, database, galleryDir := startWatcher(t)

	createTestPNGFile(t, galleryDir, "new.png")
	pollForRow(t, database, "1 = 1", 8*time.Second)
}

func TestWatcher_DecrementsTagsOnMissing(t *testing.T) {
	_, database, galleryDir := startWatcher(t)

	path := createTestPNGFile(t, galleryDir, "tagged.png")
	pollForRow(t, database, "canonical_path = '"+path+"'", 8*time.Second)
	imgID := imageIDByPath(t, database, path)

	sharedID := insertTag(t, database, "shared", "general", 2)
	soloID := insertTag(t, database, "solo", "general", 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0), (?, ?, 0)`, imgID, sharedID, imgID, soloID)

	mustRemove(t, path)
	pollForRow(t, database, fmt.Sprintf("id = %d AND is_missing = 1", imgID), 8*time.Second)

	if got := tagUsage(t, database, sharedID); got != 1 {
		t.Errorf("shared usage_count = %d, want 1 (was 2 before mark-missing)", got)
	}
	if got := tagUsage(t, database, soloID); got != 0 {
		t.Errorf("solo usage_count = %d, want 0", got)
	}
}

func TestWatcher_AliasPathRemovalDoesNotMarkMissing(t *testing.T) {
	_, database, galleryDir := startWatcher(t)

	canonicalPath := createTestPNGFile(t, galleryDir, "canonical.png")
	pollForRow(t, database, "canonical_path = '"+canonicalPath+"'", 8*time.Second)
	imgID := imageIDByPath(t, database, canonicalPath)

	tagID := insertTag(t, database, "alias_check", "general", 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, tagID)

	aliasPath := createTestPNGFile(t, galleryDir, "duplicate.png")
	pollForRow(t, database, fmt.Sprintf(
		"id = %d AND EXISTS (SELECT 1 FROM image_paths p WHERE p.image_id = images.id AND p.path = '%s' AND p.is_canonical = 0)", imgID, aliasPath), 8*time.Second)

	controlPath := createTestPNGFileSize(t, galleryDir, "control.png", 12, 12)
	pollForRow(t, database, "canonical_path = '"+controlPath+"'", 8*time.Second)
	controlID := imageIDByPath(t, database, controlPath)

	mustRemove(t, aliasPath)
	mustRemove(t, controlPath)
	pollForRow(t, database, fmt.Sprintf("id = %d AND is_missing = 1", controlID), 8*time.Second)

	if got := imageMissing(t, database, imgID); got != 0 {
		t.Errorf("image marked missing after alias-path removal; is_missing = %d, want 0", got)
	}
	if got := tagUsage(t, database, tagID); got != 1 {
		t.Errorf("tag usage_count = %d, want 1 (alias removal must not touch tag counts)", got)
	}
}

func TestSync_NamesOnlyWhatItIngests(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "first.png")
	env.sync(t, database)

	tmpl := mustParseNameTemplate(t, "{gallery}-{id}", ScopeUploadName)
	env.naming = Naming{Name: tmpl, Gallery: "default"}
	createTestPNGFileSize(t, galleryDir, "second.png", 14, 9)
	result := env.sync(t, database)

	if result.Added != 1 || result.Removed != 0 {
		t.Errorf("result = %+v, want 1 added and 0 removed", result)
	}
	var first, second string
	if err := database.Read.QueryRow(
		`SELECT canonical_path FROM images WHERE canonical_path LIKE '%first.png'`,
	).Scan(&first); err != nil {
		t.Fatalf("the row from the earlier sync was renamed or lost: %v", err)
	}
	if err := database.Read.QueryRow(
		`SELECT canonical_path FROM images WHERE canonical_path LIKE '%default-2.png'`,
	).Scan(&second); err != nil {
		t.Fatalf("the newly ingested row was not named: %v", err)
	}
	if _, statErr := os.Stat(second); statErr != nil {
		t.Errorf("file not at the named path: %v", statErr)
	}
	var missing int
	if err := database.Read.QueryRow(`SELECT COALESCE(SUM(is_missing), 0) FROM images`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("%d row(s) marked missing; the rename outran the sweep", missing)
	}
}

func ingestNamingFor(t *testing.T, folder, name string) Naming {
	t.Helper()
	return IngestNaming("default", folder, name)
}

func TestSync_FirstSyncAdoptsAnExistingTreeUntouched(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	mustMkdirAll(t, filepath.Join(galleryDir, "comics", "vol1"), 0o755)
	createTestPNGFileSize(t, galleryDir, "comics/vol1/page01.png", 10, 10)
	createTestPNGFileSize(t, galleryDir, "comics/vol1/page02.png", 11, 10)
	createTestPNGFileSize(t, galleryDir, "loose.png", 12, 10)

	env.naming = ingestNamingFor(t, "inbox/{year}", "{id}")
	result := env.sync(t, database)
	if result.Added != 3 || result.Removed != 0 {
		t.Fatalf("result = %+v, want 3 added and 0 missing", result)
	}

	for _, rel := range []string{"comics/vol1/page01.png", "comics/vol1/page02.png", "loose.png"} {
		if _, err := os.Stat(filepath.Join(galleryDir, rel)); err != nil {
			t.Errorf("%s left its place: %v", rel, err)
		}
	}
	filed := countRows(t, database, "images WHERE folder_path LIKE 'inbox%'")
	if filed != 0 {
		t.Errorf("%d row(s) were filed on the first sync", filed)
	}
}

func TestSync_FilesOnlyWhatALaterSyncIngests(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	mustMkdirAll(t, filepath.Join(galleryDir, "comics"), 0o755)
	createTestPNGFileSize(t, galleryDir, "comics/kept.png", 10, 10)
	env.sync(t, database)

	env.naming = ingestNamingFor(t, "inbox", "{id}")
	createTestPNGFileSize(t, galleryDir, "comics/fresh.png", 13, 14)
	result := env.sync(t, database)
	if result.Added != 1 || result.Removed != 0 {
		t.Fatalf("result = %+v, want 1 added and 0 missing", result)
	}

	if _, err := os.Stat(filepath.Join(galleryDir, "comics", "kept.png")); err != nil {
		t.Errorf("the already-known file moved: %v", err)
	}
	var canonical string
	if err := database.Read.QueryRow(
		`SELECT canonical_path FROM images WHERE folder_path = 'inbox'`,
	).Scan(&canonical); err != nil {
		t.Fatalf("the new file was not filed: %v", err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("filed row points nowhere: %v", err)
	}
	var missing int
	if err := database.Read.QueryRow(`SELECT COALESCE(SUM(is_missing), 0) FROM images`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("%d row(s) marked missing after the filing", missing)
	}
}

func TestSync_TemplatesThatRenderEmptyLeaveAFoundFileWhereItIs(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	mustMkdirAll(t, filepath.Join(galleryDir, "comics"), 0o755)
	createTestPNGFileSize(t, galleryDir, "comics/kept.png", 10, 10)
	env.sync(t, database)

	env.naming = ingestNamingFor(t, "{source}", "{source}")
	createTestPNGFileSize(t, galleryDir, "comics/fresh.png", 13, 14)
	if result := env.sync(t, database); result.Added != 1 {
		t.Fatalf("result = %+v, want 1 added", result)
	}
	if _, err := os.Stat(filepath.Join(galleryDir, "comics", "fresh.png")); err != nil {
		t.Errorf("an untagged file moved under templates that name nothing for it: %v", err)
	}
	if n := countRows(t, database, "images WHERE folder_path = 'comics' AND canonical_path LIKE '%fresh.png'"); n != 1 {
		t.Errorf("the row for fresh.png left comics/fresh.png")
	}
}

func TestSync_LeavesEverythingAloneWithNoNaming(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFileSize(t, galleryDir, "one.png", 10, 10)
	env.sync(t, database)
	createTestPNGFileSize(t, galleryDir, "two.png", 11, 11)
	env.sync(t, database)

	for _, rel := range []string{"one.png", "two.png"} {
		if _, err := os.Stat(filepath.Join(galleryDir, rel)); err != nil {
			t.Errorf("%s moved with no naming configured: %v", rel, err)
		}
	}
}

func TestSync_BlankFolderKeepsTheFolderItWasDroppedIn(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFileSize(t, galleryDir, "seed.png", 9, 9)
	env.sync(t, database)

	mustMkdirAll(t, filepath.Join(galleryDir, "deep"), 0o755)
	createTestPNGFileSize(t, galleryDir, "deep/inner.png", 15, 16)
	env.naming = ingestNamingFor(t, "", "{id}")
	env.sync(t, database)

	var canonical, folderPath string
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path FROM images WHERE folder_path = 'deep'`,
	).Scan(&canonical, &folderPath); err != nil {
		t.Fatalf("the file left its folder: %v", err)
	}
	if filepath.Base(canonical) == "inner.png" {
		t.Errorf("the name was not applied: %q", canonical)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("row points nowhere: %v", err)
	}
}

func TestSync_ReappearedFileIsReactivatedWithItsTagCounts(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	path := createTestPNGFile(t, galleryDir, "comesback.png")
	env.sync(t, database)

	imageID := imageIDByPath(t, database, path)
	tagID := insertTag(t, database, "comesback_tag", "general", 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`, imageID, tagID)

	mustRemove(t, path)
	env.sync(t, database)
	if got := imageMissing(t, database, imageID); got != 1 {
		t.Fatalf("is_missing after the file went = %d, want 1", got)
	}
	if got := tagUsage(t, database, tagID); got != 0 {
		t.Fatalf("usage_count after the file went = %d, want 0", got)
	}

	createTestPNGFile(t, galleryDir, "comesback.png")
	env.sync(t, database)

	if got := imageMissing(t, database, imageID); got != 0 {
		t.Errorf("is_missing after the file came back = %d, want 0", got)
	}
	if got := tagUsage(t, database, tagID); got != 1 {
		t.Errorf("usage_count after the file came back = %d, want 1", got)
	}
}

func TestSync_AMissingRowWhosePathGetsNewBytesComesBackAtOnce(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "back.png", 10, 10)
	env.sync(t, database)
	imageID := imageIDByPath(t, database, path)
	tagID := insertTag(t, database, "back_tag", "general", 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`, imageID, tagID)

	mustRemove(t, path)
	env.sync(t, database)
	if got := imageMissing(t, database, imageID); got != 1 {
		t.Fatalf("is_missing after the file went = %d, want 1", got)
	}

	createTestPNGFileSize(t, galleryDir, "back.png", 40, 25)
	if r := env.sync(t, database); r.Edited != 1 {
		t.Errorf("sync = %+v, want the file re-read", r)
	}
	if got := imageMissing(t, database, imageID); got != 0 {
		t.Errorf("is_missing after one sync = %d, want 0", got)
	}
	if got := tagUsage(t, database, tagID); got != 1 {
		t.Errorf("usage_count = %d, want 1", got)
	}
}

func TestSync_PromotesTheSurvivingAliasWhenTheCanonicalGoes(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	canonical := createTestPNGFile(t, galleryDir, "first.png")
	env.sync(t, database)
	bytes := mustReadFile(t, canonical)
	alias := filepath.Join(galleryDir, "second.png")
	mustWriteFile(t, alias, bytes, 0o644)
	if r := env.sync(t, database); r.Duplicates != 1 {
		t.Fatalf("Duplicates = %d, want the identical copy registered as one", r.Duplicates)
	}

	mustRemove(t, canonical)
	if r := env.sync(t, database); r.Moved != 1 {
		t.Fatalf("Moved = %d, want the alias promoted", r.Moved)
	}

	rows := countRows(t, database, "images")
	if rows != 1 {
		t.Errorf("images = %d, want the one row to have followed the surviving copy", rows)
	}
	var canonicalNow string
	var missing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, is_missing FROM images`).Scan(&canonicalNow, &missing); err != nil {
		t.Fatal(err)
	}
	if canonicalNow != alias {
		t.Errorf("canonical_path = %q, want the surviving %q", canonicalNow, alias)
	}
	if missing != 0 {
		t.Error("the row was flagged missing while a copy was still on disk")
	}
	paths := countRows(t, database, "image_paths")
	if paths != 1 {
		t.Errorf("image_paths rows = %d, want the vanished path dropped", paths)
	}
}

func TestSync_IngestsFilesUnderALinkedFolder(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, filepath.Join(elsewhere, "trip"), 0o755)
	createTestPNGFile(t, elsewhere, "a.png")
	createTestPNGFileSize(t, filepath.Join(elsewhere, "trip"), "b.png", 12, 12)
	mustSymlink(t, elsewhere, filepath.Join(galleryDir, "linked"))

	if r := env.sync(t, database); r.Added != 2 {
		t.Fatalf("added %d, want 2", r.Added)
	}

	folders, err := db.QueryStrings(database.Read, `SELECT folder_path FROM images ORDER BY folder_path`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"linked", "linked/trip"}
	if !slices.Equal(folders, want) {
		t.Errorf("folder_path = %v, want %v: a linked tree is filed under the link, not where the bytes live", folders, want)
	}
}

func TestSync_ASecondPassOverALinkedFolderIsIdle(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	createTestPNGFile(t, elsewhere, "a.png")
	mustSymlink(t, elsewhere, filepath.Join(galleryDir, "linked"))

	env.sync(t, database)
	r := env.sync(t, database)
	if r.Added != 0 || r.Removed != 0 || r.Moved != 0 {
		t.Errorf("second sync = %+v, want an idle run", r)
	}
}

func TestSync_AGalleryRootThatIsALinkKeepsItsLibrary(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	linkedRoot := filepath.Join(t.TempDir(), "gallery-link")
	mustSymlink(t, galleryDir, linkedRoot)
	env.galleryPath = linkedRoot

	path := createTestPNGFile(t, linkedRoot, "a.png")
	rec, _ := mustIngest(t, database, linkedRoot, env.thumbnailsPath, path, "")

	if r := env.sync(t, database); r.Removed != 0 {
		t.Errorf("missing %d, want 0", r.Removed)
	}
	if imageMissing(t, database, rec.ID) != 0 {
		t.Error("the library was flagged missing through a gallery root reached by a link")
	}
}

func TestSync_ACanonicalRespelledThroughALinkIsRepointedNotAliased(t *testing.T) {
	for _, selfAlias := range []bool{false, true} {
		t.Run(fmt.Sprintf("self-alias=%v", selfAlias), func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			linkedRoot := filepath.Join(t.TempDir(), "gallery-link")
			mustSymlink(t, galleryDir, linkedRoot)
			rec, _ := mustIngest(t, database, linkedRoot, env.thumbnailsPath, createTestPNGFile(t, linkedRoot, "a.png"), "")
			want := filepath.Join(galleryDir, "a.png")
			if selfAlias {
				mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, rec.ID, want)
			}

			first := env.sync(t, database)
			second := env.sync(t, database)

			if first.Removed != 0 || first.Duplicates != 0 {
				t.Errorf("first sync after the re-spelling = %+v, want no missing and no alias", first)
			}
			if second != (SyncResult{}) {
				t.Errorf("second sync = %+v, want an idle pass", second)
			}
			var canonical string
			if err := database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, rec.ID).Scan(&canonical); err != nil {
				t.Fatal(err)
			}
			if canonical != want || imageMissing(t, database, rec.ID) != 0 {
				t.Errorf("canonical = %q (missing %d), want %q present", canonical, imageMissing(t, database, rec.ID), want)
			}
			paths := countRows(t, database, "image_paths WHERE image_id = ?", rec.ID)
			if paths != 1 {
				t.Errorf("image_paths rows = %d, want the canonical alone", paths)
			}
		})
	}
}

func imageMissing(t *testing.T, database *db.DB, id int64) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tagUsage(t *testing.T, database *db.DB, id int64) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSync_LeavesANestedGallerysFolderOut(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFileSize(t, galleryDir, "top.png", 10, 10)
	inner := filepath.Join(galleryDir, ".H")
	mustMkdirAll(t, inner, 0o755)
	createTestPNGFileSize(t, inner, "in.png", 12, 12)
	if got := env.sync(t, database).Added; got != 2 {
		t.Fatalf("Added = %d, want 2", got)
	}

	env.fences = []Fence{{Path: inner, Owner: "H", Kind: FenceGallery}}
	r := env.sync(t, database)
	if r.Removed != 1 || r.Excluded != 1 {
		t.Fatalf("Removed = %d, Excluded = %d, want 1 and 1", r.Removed, r.Excluded)
	}
	if !strings.Contains(r.Summary(), "1 missing (1 excluded)") {
		t.Errorf("summary %q does not count the excluded row", r.Summary())
	}
	if n := countRows(t, database, "images WHERE is_missing = 1 AND canonical_path = ?", filepath.Join(inner, "in.png")); n != 1 {
		t.Error("the nested gallery's image is still visible here")
	}

	env.fences = nil
	env.sync(t, database)
	if n := countRows(t, database, "images WHERE is_missing = 1"); n != 0 {
		t.Errorf("%d row(s) still missing once the folder was handed back", n)
	}
}

func TestSync_MovesOntoACopyWhenTheCanonicalIsFencedOff(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	inner := filepath.Join(galleryDir, ".H")
	mustMkdirAll(t, inner, 0o755)
	createTestPNGFile(t, inner, "dup.png")
	copyPath := createTestPNGFile(t, galleryDir, "copy.png")
	env.sync(t, database)

	env.fences = []Fence{{Path: inner, Owner: "H", Kind: FenceGallery}}
	if r := env.sync(t, database); r.Removed != 0 {
		t.Fatalf("Removed = %d, want 0", r.Removed)
	}
	var canonical string
	var missing int
	if err := database.Read.QueryRow(`SELECT canonical_path, is_missing FROM images`).Scan(&canonical, &missing); err != nil {
		t.Fatal(err)
	}
	if canonical != copyPath || missing != 0 {
		t.Errorf("row at %q (missing=%d), want %q and visible", canonical, missing, copyPath)
	}
	if n := countRows(t, database, "image_paths WHERE path = ?", filepath.Join(inner, "dup.png")); n != 0 {
		t.Error("the fenced path is still recorded against the row")
	}
}

func TestSync_DropsACopyPathInAFencedFolder(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "a.png")
	wall := filepath.Join(galleryDir, "wall")
	mustMkdirAll(t, wall, 0o755)
	createTestPNGFile(t, wall, "a-copy.png")
	env.sync(t, database)
	if n := countRows(t, database, "image_paths WHERE is_canonical = 0"); n != 1 {
		t.Fatalf("%d copy path(s) recorded, want 1", n)
	}

	env.fences = []Fence{{Path: wall, Owner: "wall", Kind: FenceGallery}}
	env.sync(t, database)
	if n := countRows(t, database, "image_paths WHERE is_canonical = 0"); n != 0 {
		t.Errorf("%d copy path(s) left in the fenced folder, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(wall, "a-copy.png")); err != nil {
		t.Errorf("the fenced copy was touched: %v", err)
	}
}
