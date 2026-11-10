package gallery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
)

func TestUnlinkAliasFiles_LeavesAReplacedCopy(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")

	sameBytes := filepath.Join(galleryDir, "copy.png")
	replaced := filepath.Join(galleryDir, "swapped.png")
	body := mustReadFile(t, srcPath)
	for _, p := range []string{sameBytes, replaced} {
		mustWriteFile(t, p, body, 0o644)
		mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, img.ID, p)
	}
	mustWriteFile(t, replaced, append(body, "trailing"...), 0o644)

	copies, err := AliasPathsFor(database.Read, []int64{img.ID})
	if err != nil {
		t.Fatalf("alias paths: %v", err)
	}
	if len(copies[img.ID]) != 2 {
		t.Fatalf("alias copies = %d, want 2", len(copies[img.ID]))
	}
	UnlinkAliasFiles(NewBoundary(galleryDir, nil, nil), img.ID, copies[img.ID])

	if _, err := os.Stat(sameBytes); !os.IsNotExist(err) {
		t.Error("the copy still holding the row's bytes survived the delete")
	}
	if _, err := os.Stat(replaced); err != nil {
		t.Errorf("the replaced file was unlinked with the image it no longer belongs to: %v", err)
	}
}

func TestUnlinkImageFile_LeavesACanonicalWrittenOverSince(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	b := NewBoundary(galleryDir, nil, nil)
	kept := createTestPNGFile(t, galleryDir, "kept.png")
	rewritten, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, kept, "")
	mustWriteFile(t, kept, append(mustReadFile(t, kept), "trailing"...), 0o644)
	gone := createTestPNGFileSize(t, galleryDir, "gone.png", 12, 12)
	same, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, gone, "")

	if !UnlinkImageFile(b, kept, indexedAt(t, database, kept), rewritten.ID) {
		t.Error("the rewritten canonical was not reported kept")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the rewritten canonical was unlinked: %v", err)
	}
	if UnlinkImageFile(b, gone, indexedAt(t, database, gone), same.ID) {
		t.Error("the unchanged canonical was reported kept")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("the unchanged canonical survived the delete (%v)", err)
	}
}

func TestUnlinkImageFile_ComparesMoreThanTheLength(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	b := NewBoundary(galleryDir, nil, nil)
	ingest := func(name string, w int) (string, int64, Indexed) {
		t.Helper()
		p := createTestPNGFileSize(t, galleryDir, name, w, 10)
		img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, p, "")
		return p, img.ID, indexedAt(t, database, p)
	}
	swapped, swappedID, swappedRec := ingest("swapped.png", 10)
	other := createTestPNGFileSize(t, t.TempDir(), "other.png", 10, 10)
	body := mustReadFile(t, other)
	body[len(body)-20] ^= 0xff
	mustWriteFile(t, swapped, body, 0o644)

	later := time.Now().Add(time.Hour)
	mustChtimes(t, swapped, later, later)
	touched, touchedID, touchedRec := ingest("touched.png", 11)
	mustChtimes(t, touched, later, later)

	if !UnlinkImageFile(b, swapped, swappedRec, swappedID) {
		t.Error("a file of the same length written over since was not reported kept")
	}
	if _, err := os.Stat(swapped); err != nil {
		t.Errorf("a file of the same length written over since was unlinked: %v", err)
	}
	if UnlinkImageFile(b, touched, touchedRec, touchedID) {
		t.Error("a file only touched since was reported kept")
	}
	if _, err := os.Stat(touched); !os.IsNotExist(err) {
		t.Errorf("a file only touched since survived the delete (%v)", err)
	}
}

func indexedAt(t *testing.T, database *db.DB, path string) Indexed {
	t.Helper()
	var rec Indexed
	if err := database.Read.QueryRow(
		`SELECT i.file_size, i.sha256, ip.mtime_unix, ip.mtime_nsec
		   FROM image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.path = ?`, path,
	).Scan(&rec.Size, &rec.SHA256, &rec.Mtime, &rec.MtimeNano); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestRemoveCopy(t *testing.T) {
	body := []byte("the image's bytes")
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, root, canonical string) (path string, fences []Fence)
		want   CopyOutcome
		reason error
		onDisk bool
	}{
		{"a true copy", func(t *testing.T, root, _ string) (string, []Fence) {
			p := filepath.Join(root, "copy.png")
			mustWriteFile(t, p, body, 0o644)
			return p, nil
		}, CopyRemoved, nil, false},
		{"a copy already gone, the canonical too", func(t *testing.T, root, canonical string) (string, []Fence) {
			mustRemove(t, canonical)
			return filepath.Join(root, "gone.png"), nil
		}, CopyRemoved, nil, false},
		{"a copy outside the root", func(t *testing.T, _, _ string) (string, []Fence) {
			p := filepath.Join(t.TempDir(), "elsewhere.png")
			mustWriteFile(t, p, body, 0o644)
			return p, nil
		}, CopyForgotten, nil, true},
		{"a copy in a fenced folder", func(t *testing.T, root, _ string) (string, []Fence) {
			inner := filepath.Join(root, ".H")
			p := filepath.Join(inner, "copy.png")
			mustMkdirAll(t, inner, 0o755)
			mustWriteFile(t, p, body, 0o644)
			return p, []Fence{{Path: inner, Owner: "H", Kind: FenceGallery}}
		}, CopyForgotten, nil, true},
		{"the canonical file reached through a linked folder", func(t *testing.T, root, canonical string) (string, []Fence) {
			if err := os.Symlink(root, filepath.Join(root, "again")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return filepath.Join(root, "again", filepath.Base(canonical)), nil
		}, CopyForgotten, nil, true},
		{"a copy written over since", func(t *testing.T, root, _ string) (string, []Fence) {
			p := filepath.Join(root, "swapped.png")
			mustWriteFile(t, p, append(body, "trailing"...), 0o644)
			return p, nil
		}, CopyKept, ErrChangedSinceIndexed, true},
		{"a copy of the same length written over since", func(t *testing.T, root, _ string) (string, []Fence) {
			p := filepath.Join(root, "swapped.png")
			mustWriteFile(t, p, []byte("other image's bytes"[:len(body)]), 0o644)
			return p, nil
		}, CopyKept, ErrChangedSinceIndexed, true},
		{"the only copy left", func(t *testing.T, root, canonical string) (string, []Fence) {
			p := filepath.Join(root, "copy.png")
			mustWriteFile(t, p, body, 0o644)
			mustRemove(t, canonical)
			return p, nil
		}, CopyKept, ErrCanonicalGone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			canonical := filepath.Join(root, "original.png")
			mustWriteFile(t, canonical, body, 0o644)
			path, fences := tc.setup(t, root, canonical)

			sum := sha256.Sum256(body)
			rec := Indexed{Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
			got, err := RemoveCopy(NewBoundary(root, fences, nil), path, canonical, rec)
			if got != tc.want {
				t.Errorf("outcome = %d (%v), want %d", got, err, tc.want)
			}
			if tc.reason != nil && !errors.Is(err, tc.reason) {
				t.Errorf("reason = %v, want %v", err, tc.reason)
			}
			if _, statErr := os.Lstat(path); (statErr == nil) != tc.onDisk {
				t.Errorf("file on disk = %v, want %v", statErr == nil, tc.onDisk)
			}
		})
	}
}
