package galleryio

import (
	"bytes"
	"database/sql"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tags"
)

func newGallery(t *testing.T, dir, name string) gallery.Handle {
	t.Helper()
	root := filepath.Join(dir, name)
	thumbs := filepath.Join(dir, "data", name, "thumbnails")
	for _, d := range []string{root, thumbs} {
		mustMkdirAll(t, d, 0o755)
	}
	dbPath := filepath.Join(dir, "data", name, "monbooru.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap %s: %v", name, err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return gallery.Handle{
		Name:           name,
		GalleryPath:    root,
		ThumbnailsPath: thumbs,
		DBPath:         dbPath,
		DB:             database,
		TagSvc:         tags.New(database, nil),
	}
}

func newGalleryPair(t *testing.T) (src, dst gallery.Handle) {
	t.Helper()
	dir := t.TempDir()
	return newGallery(t, dir, "stock"), newGallery(t, dir, "spare")
}

func mustTag(t *testing.T, svc *tags.Service, name string, categoryID int64) *models.Tag {
	t.Helper()
	tag, err := svc.GetOrCreateTag(name, categoryID)
	if err != nil {
		t.Fatalf("GetOrCreateTag(%q): %v", name, err)
	}
	return tag
}

func writeImage(t *testing.T, h gallery.Handle, rel string, body []byte) *models.Image {
	t.Helper()
	path := filepath.Join(h.GalleryPath, rel)
	mustMkdirAll(t, filepath.Dir(path), 0o755)
	mustWriteFile(t, path, body, 0o644)
	img, _, err := gallery.Ingest(h, path, models.OriginIngest)
	if err != nil {
		t.Fatalf("ingest %s: %v", rel, err)
	}
	return img
}

func makePNGBytes(t *testing.T, w, h int, r, g, b uint8) []byte {
	t.Helper()
	plain := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			plain.Pix[(y*w+x)*4+0] = r
			plain.Pix[(y*w+x)*4+1] = g
			plain.Pix[(y*w+x)*4+2] = b
			plain.Pix[(y*w+x)*4+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, plain); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeUniquePNG(t *testing.T, marker uint8) []byte {
	t.Helper()
	return makePNGBytes(t, 12, 12, marker, 200, 50)
}

const testMaxFileSizeMB = 2048

func ingestAt(h gallery.Handle, path string) (*models.Image, bool, error) {
	return gallery.Ingest(h, path, models.OriginIngest)
}

func ratingTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

func seedRichImage(t *testing.T, cx gallery.Handle) *models.Image {
	t.Helper()
	p := filepath.Join(cx.GalleryPath, "sub", "seed.png")
	mustMkdirAll(t, filepath.Dir(p), 0o755)
	mustWriteFile(t, p, makePNGBytes(t, 8, 8, 5, 6, 7), 0o644)
	img, _, err := gallery.Ingest(cx, p, "upload")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	cat, err := cx.TagSvc.CreateCategory("mycat", "#123456")
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	tg, err := cx.TagSvc.GetOrCreateTag("special", cat.ID)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := cx.TagSvc.AddTagsToImageFromTagger(img.ID, []int64{tg.ID, ratingTagID(t, cx.DB, "explicit")}, false, ""); err != nil {
		t.Fatalf("add tags: %v", err)
	}
	srcTag, err := cx.TagSvc.GetOrCreateTag("fromdanbooru", LookupCategoryID(cx.DB, "general"))
	if err != nil {
		t.Fatalf("create source tag: %v", err)
	}
	if err := cx.TagSvc.AddTagsToImageFromTagger(img.ID, []int64{srcTag.ID}, false, "danbooru"); err != nil {
		t.Fatalf("add source tag: %v", err)
	}
	if err := gallery.AddSourceMembership(cx.DB, img.ID, "danbooru", "123", "https://example.test/posts/123"); err != nil {
		t.Fatalf("add source: %v", err)
	}
	if err := gallery.SetSourceCommentary(cx.DB, img.ID, "danbooru", "123", "artist words"); err != nil {
		t.Fatalf("set commentary: %v", err)
	}
	if err := gallery.SetSourceCommentaryTranslated(cx.DB, img.ID, "danbooru", "123", "translated words"); err != nil {
		t.Fatalf("set translated commentary: %v", err)
	}
	if err := gallery.SetSourceOriginal(cx.DB, img.ID, "danbooru", "123", "original words"); err != nil {
		t.Fatalf("set original: %v", err)
	}
	if err := gallery.ReplaceSourceAnnotations(cx.DB, img.ID, "danbooru", "123",
		[]models.Annotation{{Site: "danbooru", PostID: "123", X: 1, Y: 2, W: 3, H: 4, Body: "a box"}}); err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if err := gallery.AddManualAnnotation(cx.DB, img.ID, 5, 6, 7, 8, "my box"); err != nil {
		t.Fatalf("manual annotation: %v", err)
	}
	if _, err := cx.DB.Write.Exec(`UPDATE images SET note = 'mynote', is_favorited = 1, is_inbox = 0 WHERE id = ?`, img.ID); err != nil {
		t.Fatalf("column setup: %v", err)
	}
	return img
}

func seedExportFixture(t *testing.T, cx gallery.Handle) {
	t.Helper()
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
		 VALUES ('seed-sha', 'seed.png', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	general := generalCategoryID(t, cx)
	alias := mustTag(t, cx.TagSvc, "cat", general)
	canon := mustTag(t, cx.TagSvc, "feline", general)
	if err := cx.TagSvc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var imgID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatalf("query seed image: %v", err)
	}
	if err := cx.TagSvc.AddTagToImage(imgID, canon.ID, false, nil); err != nil {
		t.Fatalf("tag image: %v", err)
	}

	mustWriteFile(t, filepath.Join(cx.GalleryPath, "hello.txt"), []byte("hi"), 0o644)
}

func generalCategoryID(t *testing.T, h gallery.Handle) int64 {
	t.Helper()
	id := LookupCategoryID(h.DB, "general")
	if id == 0 {
		t.Fatal("no general category")
	}
	return id
}

func mustExportDB(t *testing.T, cx gallery.Handle) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := ExportGalleryDB(cx, &buf); err != nil {
		t.Fatalf("export db: %v", err)
	}
	return &buf
}

func mustExportJSON(t *testing.T, cx gallery.Handle) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := ExportGalleryJSON(cx, &buf, nil); err != nil {
		t.Fatalf("export json: %v", err)
	}
	return &buf
}

func mustExportArchive(t *testing.T, cx gallery.Handle, format string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := ExportGalleryArchive(cx, format, &buf); err != nil {
		t.Fatalf("export archive %s: %v", format, err)
	}
	return &buf
}

func noQuiesce() error { return nil }

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func libGallery(h gallery.Handle) *library.Gallery {
	g, err := library.New(h)
	if err != nil {
		panic(err)
	}
	return g
}

func builtTree(t *testing.T, g *library.Gallery) *relations.BKTree {
	t.Helper()
	tree := g.Source().Tree
	if err := tree.EnsureBuilt(g.DB); err != nil {
		t.Fatal(err)
	}
	return tree
}

func treeHoldsImage(t *testing.T, h gallery.Handle, tree *relations.BKTree, id int64) bool {
	t.Helper()
	var phash sql.NullInt64
	if err := h.DB.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&phash); err != nil || !phash.Valid {
		t.Fatalf("phash of image %d: %v (set: %t)", id, err, phash.Valid)
	}
	ids, _ := tree.SearchWithinDistance(phash.Int64, 0)
	return slices.Contains(ids, id)
}
