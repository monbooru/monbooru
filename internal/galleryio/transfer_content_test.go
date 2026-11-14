package galleryio

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/gallery"
)

func TestTransferOneImage_CopiesFullRecord(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := seedRichImage(t, src)

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}

	if LookupCategoryID(dst.DB, "mycat") == 0 {
		t.Error("custom category was not auto-created in the target")
	}

	manualCount := countRows(t, dst.DB, "image_tags WHERE image_id = ? AND is_auto = 0 AND tagger_name IS NULL", newID)
	if manualCount != 2 {
		t.Errorf("manual tags on target = %d, want 2 (special + explicit)", manualCount)
	}
	var danTagger string
	if err := dst.DB.Read.QueryRow(
		`SELECT it.tagger_name FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'fromdanbooru'`, newID).Scan(&danTagger); err != nil {
		t.Fatal(err)
	}
	if danTagger != "danbooru" {
		t.Errorf("transferred source tag tagger_name = %q, want danbooru", danTagger)
	}
	if countRows(t, dst.DB, "image_tags it JOIN tags t ON t.id = it.tag_id JOIN tag_categories tc ON tc.id = t.category_id WHERE it.image_id = ? AND tc.name = 'rating' AND t.name = 'explicit'", newID) == 0 {
		t.Error("rating tag did not ride along")
	}

	var inbox, fav int
	var note string
	if err := dst.DB.Read.QueryRow(`SELECT is_inbox, is_favorited, note FROM images WHERE id = ?`, newID).Scan(&inbox, &fav, &note); err != nil {
		t.Fatal(err)
	}
	if inbox != 1 {
		t.Errorf("is_inbox = %d, want 1 (lands in inbox for review)", inbox)
	}
	if fav != 1 {
		t.Errorf("is_favorited = %d, want 1 (copied)", fav)
	}
	if note != "mynote" {
		t.Errorf("note = %q, want mynote (copied)", note)
	}

	sources, _ := gallery.SourcesForImage(dst.DB, newID)
	if len(sources) != 1 || sources[0].Site != "danbooru" || sources[0].Commentary != "artist words" ||
		sources[0].CommentaryTranslated != "translated words" || sources[0].Original != "original words" {
		t.Errorf("sources = %+v, want one danbooru source with commentary, translation and original", sources)
	}
	anns, _ := gallery.AnnotationsForImage(dst.DB, newID)
	srcBox, manBox := 0, 0
	for _, a := range anns {
		if a.Manual {
			manBox++
		} else {
			srcBox++
		}
	}
	if srcBox != 1 || manBox != 1 {
		t.Errorf("transfer should carry one source box and one manual box, got source=%d manual=%d (%+v)", srcBox, manBox, anns)
	}

	if countRows(t, src.DB, "images WHERE id = ?", img.ID) == 0 {
		t.Error("source image should still exist when remove_after is off")
	}
}

func TestTransferOneImage_LeavesTagNotesBehind(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := seedRichImage(t, src)
	var tagID int64
	if err := src.DB.Read.QueryRow(`SELECT id FROM tags WHERE name = 'fromdanbooru'`).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	if err := src.TagSvc.SetTagNote(tagID, "only in the source gallery", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if n := countRows(t, dst.DB, "tags WHERE name = 'fromdanbooru'"); n != 1 {
		t.Fatalf("the tag landed %d times in the target", n)
	}
	if n := countRows(t, dst.DB, "tag_notes"); n != 0 {
		t.Errorf("a transfer carried %d tag notes", n)
	}
}

func TestTransferOneImage_CarriesAutoTagConfidence(t *testing.T) {
	src, dst := newGalleryPair(t)

	p := filepath.Join(src.GalleryPath, "conf.png")
	mustWriteFile(t, p, makePNGBytes(t, 8, 8, 9, 3, 1), 0o644)
	img, _, err := gallery.Ingest(src, p, "upload")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	tg, err := src.TagSvc.GetOrCreateTag("1girl", LookupCategoryID(src.DB, "general"))
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	conf := 0.875
	if _, err := src.TagSvc.AddTagToImageReportingDup(img.ID, tg.ID, true, &conf, "wd-swinv2"); err != nil {
		t.Fatalf("add auto tag: %v", err)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}
	var gotConf sql.NullFloat64
	var tagger sql.NullString
	if err := dst.DB.Read.QueryRow(
		`SELECT it.confidence, it.tagger_name FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = '1girl'`, newID).Scan(&gotConf, &tagger); err != nil {
		t.Fatalf("target auto tag: %v", err)
	}
	if !gotConf.Valid || gotConf.Float64 != conf {
		t.Errorf("transferred auto-tag confidence = %v, want %v", gotConf, conf)
	}
	if tagger.String != "wd-swinv2" {
		t.Errorf("transferred auto-tag tagger_name = %q, want wd-swinv2", tagger.String)
	}
}

func TestTransferOneImage_RemoveAfterAndShaDedup(t *testing.T) {
	src, dst := newGalleryPair(t)

	mustWriteFile(t, filepath.Join(dst.GalleryPath, "dup.png"), makePNGBytes(t, 8, 8, 5, 6, 7), 0o644)
	preImg, _, err := gallery.Ingest(dst, filepath.Join(dst.GalleryPath, "dup.png"), "ingest")
	if err != nil {
		t.Fatal(err)
	}

	img := seedRichImage(t, src)
	if img.SHA256 != preImg.SHA256 {
		t.Fatalf("fixtures should share a sha: %s vs %s", img.SHA256, preImg.SHA256)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	rowCount := countRows(t, dst.DB, "images WHERE sha256 = ?", img.SHA256)
	if rowCount != 1 {
		t.Errorf("target rows for sha = %d, want 1 (deduped)", rowCount)
	}
	var hasSpecial bool
	if err := dst.DB.Read.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'special')`, preImg.ID).Scan(&hasSpecial); err != nil {
		t.Fatal(err)
	}
	if !hasSpecial {
		t.Error("tags were not merged into the existing target row")
	}

	sources, _ := gallery.SourcesForImage(dst.DB, preImg.ID)
	if len(sources) != 1 || sources[0].Site != "danbooru" || sources[0].Commentary != "artist words" ||
		sources[0].CommentaryTranslated != "translated words" || sources[0].Original != "original words" {
		t.Errorf("sources = %+v, want one danbooru source with commentary, translation and original", sources)
	}
	anns, _ := gallery.AnnotationsForImage(dst.DB, preImg.ID)
	srcBox, manBox := 0, 0
	for _, a := range anns {
		if a.Manual {
			manBox++
		} else {
			srcBox++
		}
	}
	if srcBox != 1 || manBox != 1 {
		t.Errorf("dedup transfer should carry one source box and one manual box, got source=%d manual=%d", srcBox, manBox)
	}
	var fav int
	var note string
	if err := dst.DB.Read.QueryRow(`SELECT is_favorited, note FROM images WHERE id = ?`, preImg.ID).Scan(&fav, &note); err != nil {
		t.Fatal(err)
	}
	if fav != 1 {
		t.Errorf("is_favorited = %d, want 1 (raised on merge)", fav)
	}
	if note != "mynote" {
		t.Errorf("note = %q, want mynote (filled on merge)", note)
	}

	if countRows(t, src.DB, "images WHERE id = ?", img.ID) != 0 {
		t.Error("source image should be deleted when remove_after is on")
	}
}

func TestTransferOneImage_MergeDoesNotDuplicateManualAnnotations(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := seedRichImage(t, src)

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("first transfer: %v", err)
	}
	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("second transfer: %v", err)
	}

	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}
	anns, _ := gallery.AnnotationsForImage(dst.DB, newID)
	srcBox, manBox := 0, 0
	for _, a := range anns {
		if a.Manual {
			manBox++
		} else {
			srcBox++
		}
	}
	if srcBox != 1 || manBox != 1 {
		t.Errorf("re-transfer should stay idempotent, got source=%d manual=%d (%+v)", srcBox, manBox, anns)
	}
}

func TestTransferOneImage_RestoresMissingTarget(t *testing.T) {
	src, dst := newGalleryPair(t)

	dupPath := filepath.Join(dst.GalleryPath, "dup.png")
	preImg := writeImage(t, dst, "dup.png", makePNGBytes(t, 8, 8, 5, 6, 7))
	kept := mustTag(t, dst.TagSvc, "kept_tag", generalCategoryID(t, dst))
	if err := dst.TagSvc.AddTagToImage(preImg.ID, kept.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := gallery.SetMissing(dst.DB, preImg.ID, true); err != nil {
		t.Fatal(err)
	}

	img := seedRichImage(t, src)
	if img.SHA256 != preImg.SHA256 {
		t.Fatalf("fixtures should share a sha: %s vs %s", img.SHA256, preImg.SHA256)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var missing int
	var canon string
	if err := dst.DB.Read.QueryRow(`SELECT is_missing, canonical_path FROM images WHERE id = ?`, preImg.ID).Scan(&missing, &canon); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("is_missing = %d, want 0 (restored on transfer)", missing)
	}
	if _, err := os.Stat(canon); err != nil {
		t.Errorf("target file not restored at %s: %v", canon, err)
	}
	var usage int
	if err := dst.DB.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, kept.ID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 1 {
		t.Errorf("kept_tag usage_count = %d, want 1 once its image is visible again", usage)
	}
}

func TestTransferOneImage_ReadsTheFileItRestoresUnderAPlaceholder(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := seedRichImage(t, src)

	dupPath := filepath.Join(dst.GalleryPath, "dup.png")
	preID, err := insertMissingImageRow(dst.DB, img.SHA256, dupPath, dst.GalleryPath, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var missing int
	var size int64
	var width sql.NullInt64
	var phash sql.NullInt64
	if err := dst.DB.Read.QueryRow(`SELECT is_missing, file_size, width, phash FROM images WHERE id = ?`, preID).Scan(&missing, &size, &width, &phash); err != nil {
		t.Fatal(err)
	}
	if missing != 0 || size == 0 || width.Int64 != 8 || !phash.Valid {
		t.Errorf("row = missing %d, %d bytes, %d wide, phash %v; want the restored file read", missing, size, width.Int64, phash.Valid)
	}
}

func TestTransferOneImage_MoveRestoresATargetFileThatVanished(t *testing.T) {
	src, dst := newGalleryPair(t)

	dupPath := filepath.Join(dst.GalleryPath, "dup.png")
	preImg := writeImage(t, dst, "dup.png", makePNGBytes(t, 8, 8, 5, 6, 7))
	if err := os.Remove(dupPath); err != nil {
		t.Fatal(err)
	}
	img := seedRichImage(t, src)
	if img.SHA256 != preImg.SHA256 {
		t.Fatalf("fixtures should share a sha: %s vs %s", img.SHA256, preImg.SHA256)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	got, err := os.ReadFile(dupPath)
	if err != nil {
		t.Fatalf("the moved image survives nowhere: %v", err)
	}
	if want := makePNGBytes(t, 8, 8, 5, 6, 7); !bytes.Equal(got, want) {
		t.Error("the restored file differs from the moved one")
	}
	if _, err := os.Stat(img.CanonicalPath); !os.IsNotExist(err) {
		t.Errorf("the source file should be gone after a move: %v", err)
	}
}

func flaggedTarget(t *testing.T, dst gallery.Handle) (int64, string) {
	t.Helper()
	dupPath := filepath.Join(dst.GalleryPath, "dup.png")
	preImg := writeImage(t, dst, "dup.png", makePNGBytes(t, 8, 8, 5, 6, 7))
	if err := os.Remove(dupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := gallery.SetMissing(dst.DB, preImg.ID, true); err != nil {
		t.Fatal(err)
	}
	return preImg.ID, dupPath
}

func TestTransferOneImage_LeavesAnotherFileAtAFlaggedTargetsPathAlone(t *testing.T) {
	src, dst := newGalleryPair(t)
	dstID, dupPath := flaggedTarget(t, dst)
	foreign := makePNGBytes(t, 8, 8, 200, 1, 2)
	mustWriteFile(t, dupPath, foreign, 0o644)
	img := seedRichImage(t, src)

	_, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false)

	if err == nil || !strings.Contains(err.Error(), "now holds other bytes") {
		t.Errorf("transfer = %v, want it refused over the other file", err)
	}
	if got := mustReadFile(t, dupPath); !bytes.Equal(got, foreign) {
		t.Error("the file at the flagged target's path was overwritten")
	}
	var missing int
	if err := dst.DB.Read.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, dstID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 1 {
		t.Errorf("is_missing = %d, want the target row still flagged", missing)
	}
}

func TestTransferOneImage_FlaggedTargetWhoseFileCameBackIsCleared(t *testing.T) {
	src, dst := newGalleryPair(t)
	dstID, dupPath := flaggedTarget(t, dst)
	mustWriteFile(t, dupPath, makePNGBytes(t, 8, 8, 5, 6, 7), 0o644)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(dupPath, old, old); err != nil {
		t.Fatal(err)
	}
	img := seedRichImage(t, src)

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var missing int
	if err := dst.DB.Read.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, dstID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("is_missing = %d, want 0 with its file back", missing)
	}
	info, err := os.Stat(dupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("the file that came back was written again (mtime %v)", info.ModTime())
	}
}

func TestTransferOneImage_RefusesToRestoreFromASourceRewrittenSinceIndexing(t *testing.T) {
	src, dst := newGalleryPair(t)
	_, dupPath := flaggedTarget(t, dst)
	img := seedRichImage(t, src)
	rewritten := makePNGBytes(t, 9, 9, 1, 2, 3)
	mustWriteFile(t, img.CanonicalPath, rewritten, 0o644)

	_, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, true)

	if err == nil || !errors.Is(err, gallery.ErrChangedSinceIndexed) {
		t.Errorf("transfer = %v, want it refused as changed since indexing", err)
	}
	if _, err := os.Stat(dupPath); !os.IsNotExist(err) {
		t.Errorf("the rewritten bytes were restored under the target's sha (%v)", err)
	}
	if got := mustReadFile(t, img.CanonicalPath); !bytes.Equal(got, rewritten) {
		t.Error("the source file was not kept")
	}
	if countRows(t, src.DB, "images WHERE id = ?", img.ID) != 1 {
		t.Error("the source row went with a refused move")
	}
}

func sharedFolderPair(t *testing.T) (src, dst gallery.Handle, srcID, dstID int64, path string) {
	t.Helper()
	src, dst = newGalleryPair(t)
	dst.GalleryPath = src.GalleryPath
	path = filepath.Join(src.GalleryPath, "shared.png")
	mustWriteFile(t, path, makePNGBytes(t, 8, 8, 5, 6, 7), 0o644)
	for _, side := range []struct {
		h  gallery.Handle
		id *int64
	}{{src, &srcID}, {dst, &dstID}} {
		img, _, err := gallery.Ingest(side.h, path, "ingest")
		if err != nil {
			t.Fatal(err)
		}
		*side.id = img.ID
	}
	return src, dst, srcID, dstID, path
}

func TestTransferOneImage_MoveRefusesAFileBothGalleriesIndex(t *testing.T) {
	src, dst, srcID, _, path := sharedFolderPair(t)
	want := makePNGBytes(t, 8, 8, 5, 6, 7)

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), srcID, true); err == nil {
		t.Fatal("a move of a file both galleries index went ahead")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the shared file was lost (err=%v)", err)
	}
	if n := countRows(t, src.DB, "images WHERE id = ?", srcID); n != 1 {
		t.Error("the source row was removed")
	}
}

func TestTransferOneImage_CopyNeverEmptiesAFileBothGalleriesIndex(t *testing.T) {
	src, dst, srcID, dstID, path := sharedFolderPair(t)
	want := makePNGBytes(t, 8, 8, 5, 6, 7)
	if _, err := gallery.SetMissing(dst.DB, dstID, true); err != nil {
		t.Fatal(err)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), srcID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the shared file holds %d bytes, want its own %d (err=%v)", len(got), len(want), err)
	}
}

func TestTransferOneImage_IntoAGalleryOnTheSameFolderIndexesTheFileWhereItIs(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(map[bool]string{false: "copy", true: "move"}[move], func(t *testing.T) {
			src, dst := newGalleryPair(t)
			dst.GalleryPath = src.GalleryPath
			path := filepath.Join(src.GalleryPath, "shared.png")
			img := writeImage(t, src, "shared.png", makePNGBytes(t, 8, 8, 5, 6, 7))

			_, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, move)
			if move != (err != nil) {
				t.Fatalf("transfer err = %v, want a refusal only for the move", err)
			}
			entries, readErr := os.ReadDir(src.GalleryPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 {
				t.Errorf("the folder holds %d entries, want the one file and no suffixed copy", len(entries))
			}
			want := 1
			if move {
				want = 0
			}
			if n := countRows(t, dst.DB, "images WHERE canonical_path = ?", path); n != want {
				t.Errorf("the target holds %d row(s) on the shared file, want %d", n, want)
			}
			if n := countRows(t, src.DB, "images WHERE id = ?", img.ID); n != 1 {
				t.Error("the source row went")
			}
		})
	}
}

func TestTransferOneImage_MoveLandsInInbox(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := seedRichImage(t, src)

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var inbox int
	if err := dst.DB.Read.QueryRow(`SELECT is_inbox FROM images WHERE sha256 = ?`, img.SHA256).Scan(&inbox); err != nil {
		t.Fatalf("target row: %v", err)
	}
	if inbox != 1 {
		t.Errorf("is_inbox = %d, want 1 (a move lands in the target inbox for re-filing)", inbox)
	}

	if countRows(t, src.DB, "images WHERE id = ?", img.ID) != 0 {
		t.Error("source image should be deleted when remove_after is on")
	}
}

func TestTransferOneImage_RefusesAFolderTheTargetLeavesOut(t *testing.T) {
	src, dst := newGalleryPair(t)
	mustMkdirAll(t, filepath.Join(src.GalleryPath, ".H"), 0o755)
	img := writeImage(t, src, ".H/in.png", makePNGBytes(t, 8, 8, 3, 1, 4))
	inner := filepath.Join(dst.GalleryPath, ".H")
	dst.Bounds = new(atomic.Pointer[gallery.Boundary])
	dst.Bounds.Store(gallery.NewBoundary(dst.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))

	_, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false)
	if err == nil || !strings.Contains(err.Error(), "belongs to gallery H") {
		t.Fatalf("transfer = %v, want a refusal naming gallery H", err)
	}
	if _, err := os.Stat(filepath.Join(inner, "in.png")); !os.IsNotExist(err) {
		t.Errorf("the copy landed in the fenced folder (err=%v)", err)
	}
}

func TestTransferOneImage_RefusesASourceOutsideItsRoot(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := writeImage(t, src, "in.png", makePNGBytes(t, 8, 8, 2, 7, 1))
	outside := filepath.Join(t.TempDir(), "secret.png")
	mustWriteFile(t, outside, mustReadFile(t, img.CanonicalPath), 0o644)
	if _, err := src.DB.Write.Exec(`UPDATE images SET canonical_path = ? WHERE id = ?`, outside, img.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err == nil || err.Error() != "file missing from disk" {
		t.Fatalf("transfer = %v, want the missing-file refusal", err)
	}
	if n := countRows(t, dst.DB, "images"); n != 0 {
		t.Errorf("the target holds %d images, want none", n)
	}
}

func TestTransferOneImage_FeedsTheTargetsPhash(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := seedRichImage(t, src)
	target := libGallery(dst)
	tree := builtTree(t, target)

	if _, err := TransferOneImage(libGallery(src), target, img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}
	if !treeHoldsImage(t, dst, tree, newID) {
		t.Errorf("the target's built tree lacks image %d", newID)
	}
}

func TestTransferOneImage_RefreshesTheTargetsSourceCounts(t *testing.T) {
	src, dst := newGalleryPair(t)
	body := makeUniquePNG(t, 77)
	img := writeImage(t, src, "plain.png", body)
	if err := gallery.AddSourceMembership(src.DB, img.ID, "danbooru", "1", "https://danbooru.donmai.us/posts/1"); err != nil {
		t.Fatal(err)
	}
	writeImage(t, dst, "held.png", body)
	target := libGallery(dst)
	if before, err := target.SourceLabelCounts(); err != nil || len(before) != 0 {
		t.Fatalf("target source counts before = %v (%v), want none", before, err)
	}

	if _, err := TransferOneImage(libGallery(src), target, img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	after, err := target.SourceLabelCounts()
	if err != nil || len(after) != 1 || after[0].Source != "danbooru" {
		t.Errorf("target source counts after = %v (%v), want danbooru", after, err)
	}
}

func TestTransferOneImage_TheTargetReDerivesImpliedTags(t *testing.T) {
	src, dst := newGalleryPair(t)
	img := writeImage(t, src, "pair.png", makePNGBytes(t, 8, 8, 70, 80, 90))
	for _, h := range []gallery.Handle{src, dst} {
		a, b := mustTag(t, h.TagSvc, "a", 1), mustTag(t, h.TagSvc, "b", 1)
		if _, err := h.TagSvc.AddImplication(a.ID, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.TagSvc.AddTagToImage(img.ID, mustTag(t, src.TagSvc, "a", 1).ID, false, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := TransferOneImage(libGallery(src), libGallery(dst), img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}
	if impliedFlag(t, dst, newID, "a") != 0 || impliedFlag(t, dst, newID, "b") != 1 {
		t.Error("the transfer did not land a direct and leave b to the target's implication")
	}
}
