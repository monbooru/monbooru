package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
)

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func openTestGallery(t *testing.T) *Gallery {
	t.Helper()
	dir := t.TempDir()
	galleryDir := filepath.Join(dir, "gallery")
	if err := os.MkdirAll(galleryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cx, err := Open(config.Gallery{
		Name: "del", GalleryPath: galleryDir,
		DBPath: filepath.Join(dir, "data", "monbooru.db"), ThumbnailsPath: filepath.Join(dir, "data", "thumbnails"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cx.Close)
	return cx
}

func seedRow(t *testing.T, cx *Gallery, rel string, body []byte, missing bool) int64 {
	t.Helper()
	path := filepath.Join(cx.GalleryPath, rel)
	if body != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, path, body, 0o644)
	}
	folder := filepath.ToSlash(filepath.Dir(rel))
	if folder == "." {
		folder = ""
	}
	sha := "sha-" + rel
	if body != nil {
		sha = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	var id int64
	if err := cx.DB.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing) VALUES (?, ?, ?, 'png', ?, ?) RETURNING id`,
		sha, path, folder, len(body), missing,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func addAlias(t *testing.T, cx *Gallery, id int64, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, body, 0o644)
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustDelete(t *testing.T, cx *Gallery, ids []int64, opts DeleteOptions) DeleteResult {
	t.Helper()
	res, err := cx.DeleteImages(context.Background(), ids, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDeleteImages_UnlinksTheCopiesItOwns(t *testing.T) {
	cx := openTestGallery(t)
	body := []byte("original")
	id := seedRow(t, cx, "original.png", body, false)
	copyPath := filepath.Join(cx.GalleryPath, "copy.png")
	addAlias(t, cx, id, copyPath, body)

	res := mustDelete(t, cx, []int64{id}, DeleteOptions{})
	if len(res.Deleted) != 1 {
		t.Fatalf("deleted %d rows, want 1", len(res.Deleted))
	}
	for _, p := range []string{filepath.Join(cx.GalleryPath, "original.png"), copyPath} {
		if exists(p) {
			t.Errorf("%q survived the delete", p)
		}
	}
}

func TestDeleteImages_LeavesACopyInAFencedFolder(t *testing.T) {
	cx := openTestGallery(t)
	body := []byte("original")
	id := seedRow(t, cx, "original.png", body, false)
	inner := filepath.Join(cx.GalleryPath, ".H")
	theirs := filepath.Join(inner, "copy.png")
	addAlias(t, cx, id, theirs, body)
	cx.Bounds.Store(gallery.NewBoundary(cx.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))

	mustDelete(t, cx, []int64{id}, DeleteOptions{})
	if exists(filepath.Join(cx.GalleryPath, "original.png")) {
		t.Error("the image's own file survived the delete")
	}
	if !exists(theirs) {
		t.Error("the other gallery's copy went with the image")
	}
}

func TestDeleteImages_KeepsTagCountsExact(t *testing.T) {
	cx := openTestGallery(t)
	visible := seedRow(t, cx, "v.png", []byte("v"), false)
	missing := seedRow(t, cx, "m.png", nil, true)
	kept := seedRow(t, cx, "k.png", []byte("k"), false)
	tag, err := cx.TagSvc.GetOrCreateTag("shared", cx.GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{visible, missing, kept} {
		if err := cx.TagSvc.AddTagToImage(id, tag.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	mustDelete(t, cx, []int64{visible, missing}, DeleteOptions{})
	if n, err := tags.RecalcDBCount(cx.DB); err != nil || n != 0 {
		t.Errorf("the recount changed %d tags (%v), want none", n, err)
	}
	var usage int
	if err := cx.DB.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, tag.ID).Scan(&usage); err != nil || usage != 1 {
		t.Errorf("usage = %d (%v), want 1", usage, err)
	}
}

func TestDeleteImages_FilesGoneTakesOnlyRowsWhoseFileIsGone(t *testing.T) {
	cx := openTestGallery(t)
	gone := seedRow(t, cx, "gone.png", nil, true)
	copyPath := filepath.Join(cx.GalleryPath, "gone-copy.png")
	addAlias(t, cx, gone, copyPath, []byte("x"))
	present := seedRow(t, cx, "present.png", []byte("still here"), true)

	res := mustDelete(t, cx, []int64{gone, present}, DeleteOptions{FilesGone: true})
	if len(res.Deleted) != 1 || res.Deleted[0].ID != gone || res.Kept != 1 {
		t.Fatalf("deleted %+v kept %d, want only %d deleted and 1 kept", res.Deleted, res.Kept, gone)
	}
	if !exists(copyPath) {
		t.Error("prune unlinked an alias copy")
	}
	if !exists(filepath.Join(cx.GalleryPath, "present.png")) {
		t.Error("prune unlinked a file still on disk")
	}
}

func TestDeleteImages_FilesGoneRefusesAnUnreadableOrEmptyRoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unmount func(root string) error
		want    string
	}{
		{"root gone", os.RemoveAll, "gallery path is unreadable"},
		{"empty mount point", func(string) error { return nil }, "gallery folder is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cx := openTestGallery(t)
			id := seedRow(t, cx, "away.png", nil, true)
			if err := tc.unmount(cx.GalleryPath); err != nil {
				t.Fatal(err)
			}

			res, err := cx.DeleteImages(context.Background(), []int64{id}, DeleteOptions{FilesGone: true}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(res.Deleted) != 0 {
				t.Errorf("deleted %+v", res.Deleted)
			}
			if n := countRows(t, cx.DB, "images WHERE id = ?", id); n != 1 {
				t.Errorf("the flagged row was pruned (n=%d)", n)
			}
		})
	}
}

func TestDeleteImages_ContinuePastFailureSkipsTheImageThatFails(t *testing.T) {
	for _, carryOn := range []bool{false, true} {
		t.Run(fmt.Sprint(carryOn), func(t *testing.T) {
			cx := openTestGallery(t)
			a := seedRow(t, cx, "a.png", []byte("a"), false)
			stuck := seedRow(t, cx, "stuck.png", []byte("s"), false)
			b := seedRow(t, cx, "b.png", []byte("b"), false)
			if _, err := cx.DB.Write.Exec(fmt.Sprintf(
				`CREATE TRIGGER refuse_one BEFORE DELETE ON images WHEN OLD.id = %d BEGIN SELECT RAISE(ABORT, 'refused'); END`, stuck)); err != nil {
				t.Fatal(err)
			}

			res, err := cx.DeleteImages(context.Background(), []int64{a, stuck, b}, DeleteOptions{ContinuePastFailure: carryOn}, nil)
			if !carryOn {
				if err == nil || len(res.Deleted) != 0 {
					t.Fatalf("without the option: err %v, deleted %d; want the error and nothing deleted", err, len(res.Deleted))
				}
				return
			}
			if err != nil || len(res.Deleted) != 2 || res.Failed != 1 {
				t.Fatalf("err %v, deleted %d, failed %d; want 2 deleted and 1 failed", err, len(res.Deleted), res.Failed)
			}
			if !exists(filepath.Join(cx.GalleryPath, "stuck.png")) {
				t.Error("the refused image lost its file")
			}
		})
	}
}

func TestDeleteImages_RemovesAnEmptiedFolderOnlyWhenAsked(t *testing.T) {
	cx := openTestGallery(t)
	solo := seedRow(t, cx, "solo/a.png", []byte("a"), false)
	shared := seedRow(t, cx, "shared/a.png", []byte("b"), false)
	mustWriteFile(t, filepath.Join(cx.GalleryPath, "shared", "other.txt"), []byte("x"), 0o644)
	unasked := seedRow(t, cx, "kept/a.png", []byte("c"), false)

	res := mustDelete(t, cx, []int64{solo, shared}, DeleteOptions{RemoveEmptyFolder: true})
	removed := map[int64]bool{}
	for _, d := range res.Deleted {
		removed[d.ID] = d.FolderRemoved
	}
	if !removed[solo] || exists(filepath.Join(cx.GalleryPath, "solo")) {
		t.Error("the emptied folder stayed")
	}
	if removed[shared] || !exists(filepath.Join(cx.GalleryPath, "shared")) {
		t.Error("a folder with other content went")
	}
	mustDelete(t, cx, []int64{unasked}, DeleteOptions{})
	if !exists(filepath.Join(cx.GalleryPath, "kept")) {
		t.Error("a delete that did not ask removed its folder")
	}
}

func TestDeleteImages_PromotesAGroupOriginalTheChunkDeletes(t *testing.T) {
	cx := openTestGallery(t)
	orig := seedRow(t, cx, "o.png", []byte("o"), false)
	dup1 := seedRow(t, cx, "d1.png", []byte("d1"), false)
	dup2 := seedRow(t, cx, "d2.png", []byte("d2"), false)
	if err := cx.RelationsSvc.AddDuplicateOf(orig, dup1); err != nil {
		t.Fatal(err)
	}
	if err := cx.RelationsSvc.AddDuplicateOf(orig, dup2); err != nil {
		t.Fatal(err)
	}

	mustDelete(t, cx, []int64{orig}, DeleteOptions{})
	var original int64
	if err := cx.DB.Read.QueryRow(`SELECT original_image_id FROM dup_groups`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if original != dup1 && original != dup2 {
		t.Errorf("group original = %d, want a surviving member", original)
	}
}

func TestDeleteImages_StopsBetweenChunksOnCancel(t *testing.T) {
	cx := openTestGallery(t)
	ids := make([]int64, deleteChunk+100)
	for i := range ids {
		ids[i] = seedRow(t, cx, fmt.Sprintf("r%04d.png", i), nil, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res, err := cx.DeleteImages(ctx, ids, DeleteOptions{}, func(int, int) { cancel() })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled || len(res.Deleted) != deleteChunk {
		t.Fatalf("cancelled %v after %d deletes, want the first chunk of %d only", res.Cancelled, len(res.Deleted), deleteChunk)
	}
	if left := countRows(t, cx.DB, "images"); left != 100 {
		t.Errorf("rows left = %d, want 100", left)
	}
}

func TestDeleteImages_AnUnknownIDDeletesNothing(t *testing.T) {
	cx := openTestGallery(t)
	if res := mustDelete(t, cx, []int64{404}, DeleteOptions{}); len(res.Deleted) != 0 {
		t.Errorf("deleted %+v, want nothing", res.Deleted)
	}
}

func TestDeleteImages_KeepsAFileWhoseBytesChanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra int
	}{
		{"another length", 1},
		{"the same length", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cx := openTestGallery(t)
			a := filepath.Join(cx.GalleryPath, "a.png")
			b := filepath.Join(cx.GalleryPath, "b.png")
			bodyA, bodyB := pngBytes(t, 10), pngBytes(t, 11)
			size := max(len(bodyA), len(bodyB)) + 64
			bodyA, bodyB = padPNG(t, bodyA, size), padPNG(t, bodyB, size+tc.extra)
			for p, body := range map[string][]byte{a: bodyA, b: bodyB} {
				mustWriteFile(t, p, body, 0o644)
			}
			sync := func() gallery.SyncResult {
				t.Helper()
				r, err := cx.Sync(context.Background(), 0, gallery.Naming{}, func(int, int, string) {}, func() {})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			sync()
			var idA, idB int64
			if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE canonical_path = ?`, a).Scan(&idA); err != nil {
				t.Fatal(err)
			}
			if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE canonical_path = ?`, b).Scan(&idB); err != nil {
				t.Fatal(err)
			}

			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(b, later, later); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(b, a); err != nil {
				t.Fatal(err)
			}
			if r := sync(); r.Conflicts != 1 {
				t.Fatalf("test premise broken: sync = %+v, want the rename refused as a conflict", r)
			}

			res := mustDelete(t, cx, []int64{idA}, DeleteOptions{})
			if len(res.Deleted) != 1 || res.Changed != 1 {
				t.Errorf("deleted %d, changed %d, want 1 deleted and its file reported kept", len(res.Deleted), res.Changed)
			}
			if got, err := os.ReadFile(a); err != nil || !bytes.Equal(got, bodyB) {
				t.Fatalf("a.png lost the other image's bytes (%v)", err)
			}
			sync()
			if countRows(t, cx.DB, "images WHERE id = ? AND canonical_path = ? AND is_missing = 0", idB, a) != 1 {
				t.Error("the next sync did not restore the other image onto a.png")
			}
		})
	}
}

func TestDeleteCopies_RemovesATrueCopyAndDropsTheCaches(t *testing.T) {
	cx := openTestGallery(t)
	body := []byte("original")
	id := seedRow(t, cx, "original.png", body, false)
	copyPath := filepath.Join(cx.GalleryPath, "copy.png")
	addAlias(t, cx, id, copyPath, body)
	var pathID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM image_paths WHERE path = ?`, copyPath).Scan(&pathID); err != nil {
		t.Fatal(err)
	}
	q := search.Query{Cached: true, Text: "name:copy", Sort: "newest", Order: "desc"}
	cx.cache.Seed(q, []int64{id})

	res, err := cx.DeleteCopies(context.Background(), []int64{pathID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || len(res.Kept) != 0 {
		t.Errorf("result = %+v, want the one copy removed", res)
	}
	if exists(copyPath) {
		t.Error("the copy is still on disk")
	}
	if !exists(filepath.Join(cx.GalleryPath, "original.png")) {
		t.Error("the canonical file went with the copy")
	}
	if _, cached := search.PageOf(cx.Source(), q, id, 1); cached {
		t.Error("a cached name: search still lists the removed copy's image")
	}
}

func TestDeleteCopies_KeepsTheOnlyCopy(t *testing.T) {
	cx := openTestGallery(t)
	body := []byte("original")
	id := seedRow(t, cx, "original.png", body, false)
	copyPath := filepath.Join(cx.GalleryPath, "copy.png")
	addAlias(t, cx, id, copyPath, body)
	if err := os.Remove(filepath.Join(cx.GalleryPath, "original.png")); err != nil {
		t.Fatal(err)
	}
	var pathID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM image_paths WHERE path = ?`, copyPath).Scan(&pathID); err != nil {
		t.Fatal(err)
	}

	res, err := cx.DeleteCopies(context.Background(), []int64{pathID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || len(res.Kept) != 1 || !errors.Is(res.Kept[0].Reason, gallery.ErrCanonicalGone) {
		t.Errorf("result = %+v, want the copy kept as the only one left", res)
	}
	if !exists(copyPath) {
		t.Error("the only copy left was deleted")
	}
	if n := countRows(t, cx.DB, "image_paths WHERE id = ?", pathID); n != 1 {
		t.Errorf("the kept copy's row = %d, want 1", n)
	}
}

func TestDeleteImages_RolledBackChunkKeepsTreeEntries(t *testing.T) {
	cx := openTestGallery(t)
	id := seedRow(t, cx, "a.png", []byte("a"), false)
	const phash = 0x0f0f
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = ? WHERE id = ?`, phash, id)
	if err := cx.bkTree.EnsureBuilt(cx.DB); err != nil {
		t.Fatal(err)
	}
	mustExec(t, cx.DB.Write, `CREATE TRIGGER boom BEFORE DELETE ON images BEGIN SELECT RAISE(ABORT, 'boom'); END`)

	if _, err := cx.DeleteImages(context.Background(), []int64{id}, DeleteOptions{}, nil); err == nil {
		t.Fatal("DeleteImages succeeded through an aborting trigger")
	}
	if ids, _ := cx.bkTree.SearchWithinDistance(phash, 0); !slices.Contains(ids, id) {
		t.Errorf("the rolled-back delete left image %d out of the tree: %v", id, ids)
	}
}

func padPNG(t *testing.T, body []byte, size int) []byte {
	t.Helper()
	const iend = 12
	fill := size - len(body) - 12 - len("pad\x00")
	if fill < 0 {
		t.Fatalf("cannot pad %d bytes to %d", len(body), size)
	}
	data := append([]byte("tEXtpad\x00"), bytes.Repeat([]byte("x"), fill)...)
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(data)-4))
	chunk.Write(data)
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(data))
	return slices.Concat(body[:len(body)-iend], chunk.Bytes(), body[len(body)-iend:])
}

func TestSyncReportsChangesOnlyWhenRowsChanged(t *testing.T) {
	cx := openTestGallery(t)
	for i := range 3 {
		mustWriteFile(t, filepath.Join(cx.GalleryPath, fmt.Sprintf("c%d.png", i)), pngBytes(t, uint8(20+i)), 0o644)
	}
	changes := 0
	sync := func() {
		t.Helper()
		if _, err := cx.Sync(context.Background(), 0, gallery.Naming{}, func(int, int, string) {}, func() { changes++ }); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	if changes == 0 {
		t.Fatal("a sync that added three rows reported no change")
	}
	changes = 0
	sync()
	if changes != 0 {
		t.Errorf("a resync that changed nothing reported %d changes", changes)
	}
}
