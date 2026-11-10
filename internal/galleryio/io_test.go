package galleryio

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"

	"github.com/monbooru/monbooru/internal/gallery"
)

func openIOTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestSanitizeImportedAliasChains_DanglingCanonical(t *testing.T) {
	cx := openIOTestDB(t)

	if _, err := cx.Write.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	var danglingID int64
	err := cx.Write.QueryRow(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin)
		 VALUES ('dangling_alias', 1, 1, 999999, 0, 'user') RETURNING id`,
	).Scan(&danglingID)
	if _, pragmaErr := cx.Write.Exec(`PRAGMA foreign_keys=ON`); pragmaErr != nil {
		t.Fatal(pragmaErr)
	}
	if err != nil {
		t.Fatal(err)
	}

	if err := sanitizeImportedAliasChains(cx); err != nil {
		t.Fatal(err)
	}

	var alias int
	var canonical int64
	if err := cx.Read.QueryRow(
		`SELECT is_alias, COALESCE(canonical_tag_id, 0) FROM tags WHERE id = ?`, danglingID,
	).Scan(&alias, &canonical); err != nil {
		t.Fatal(err)
	}
	if alias != 0 || canonical != 0 {
		t.Errorf("dangling alias = (alias=%d, canonical=%d), want promoted to plain", alias, canonical)
	}
}

func TestApplyImport_AnEscapingEntryLeavesTheGalleryAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries [][2]string
	}{
		{"native", [][2]string{{"monbooru.json", `{"version":1}`}, {"gallery/../escape.png", "x"}}},
		{"light", [][2]string{{"tags.json", `{"version":1,"images":[]}`}, {"gallery/../escape.png", "x"}}},

		{"hydrus", [][2]string{{"ok.png", "x"}, {"ok.png.txt", "tag\n"}, {"../escape.png", "x"}, {"../escape.png.txt", "tag\n"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := refusedArchiveLeavesTheGallery(t, zipOf(t, tc.entries), 0, "")
			if _, err := os.Stat(filepath.Join(dir, "escape.png")); err == nil {
				t.Error("the escaping entry was written")
			}
		})
	}
}

func TestApplyImport_AnOversizeEntryLeavesTheGalleryAsItWas(t *testing.T) {
	big := strings.Repeat("x", 1<<20+1)
	for _, tc := range []struct {
		name    string
		entries [][2]string
	}{
		{"native", [][2]string{{"monbooru.json", `{"version":1}`}, {"gallery/big.png", big}}},
		{"light", [][2]string{{"tags.json", `{"version":1,"images":[]}`}, {"gallery/big.png", big}}},
		{"hydrus", [][2]string{{"ok.png", "x"}, {"ok.png.txt", "tag\n"}, {"big.png", big}, {"big.png.txt", "tag\n"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, galleryPath := refusedArchiveLeavesTheGallery(t, zipOf(t, tc.entries), 1, "exceeds per-file limit")
			if _, err := os.Stat(filepath.Join(galleryPath, "big.png")); err == nil {
				t.Error("the oversize entry was written")
			}
		})
	}
}

func archivesWithADocumentPastOneMiB(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	src := newGallery(t, dir, "src")
	pic := makeUniquePNG(t, 41)
	writeImage(t, src, "ok.png", pic)
	if _, err := src.DB.Write.Exec(`INSERT INTO saved_searches (name, query) VALUES ('pad', ?)`, strings.Repeat("x", 1<<20+1)); err != nil {
		t.Fatal(err)
	}
	var manifest bytes.Buffer
	if err := ExportGalleryLightManifest(src, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.WriteString(strings.Repeat(" ", 1<<20+1))
	return map[string][]byte{
		"db":    mustExportArchive(t, src, "db").Bytes(),
		"json":  mustExportArchive(t, src, "json").Bytes(),
		"light": zipOf(t, [][2]string{{"tags.json", manifest.String()}, {"gallery/ok.png", string(pic)}}),
	}
}

func TestApplyImport_AnArchivesOwnDatabaseIsNotHeldToTheMediaCap(t *testing.T) {
	dir := t.TempDir()
	for name, archive := range archivesWithADocumentPastOneMiB(t, dir) {
		t.Run(name, func(t *testing.T) {
			cx := newGallery(t, dir, "stock-"+name)
			path := filepath.Join(dir, name+".zip")
			mustWriteFile(t, path, archive, 0o644)
			closeDB(t, cx.DB)

			b := gallery.NewBoundary(cx.GalleryPath, nil, nil)
			if _, err := ApplyImport("zip", path, cx.DBPath, cx.ThumbnailsPath, b, 1, noQuiesce); err != nil {
				t.Fatalf("ApplyImport: %v", err)
			}
			database := reopen(t, cx.DBPath)
			if n := countRows(t, database, "images WHERE is_missing = 0 AND canonical_path = ?", filepath.Join(cx.GalleryPath, "ok.png")); n != 1 {
				t.Error("the archive's image has no visible row")
			}
		})
	}
}

func zipOf(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCopyZipEntry_RejectsOversizedEntry(t *testing.T) {
	zipBytes := zipOf(t, [][2]string{{"payload.bin", string(make([]byte, 4096))}})
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	entry := zr.File[0]

	var sink bytes.Buffer
	if err := copyZipEntry(&sink, entry, 1024); err == nil {
		t.Errorf("expected oversize error for 4 KiB entry against 1 KiB cap")
	}

	sink.Reset()
	if err := copyZipEntry(&sink, entry, 0); err != nil {
		t.Errorf("maxBytes=0 should disable the cap, got %v", err)
	}
	if sink.Len() != 4096 {
		t.Errorf("uncapped copy wrote %d bytes, want 4096", sink.Len())
	}

	sink.Reset()
	if err := copyZipEntry(&sink, entry, 8192); err != nil {
		t.Errorf("entry inside the cap should copy clean, got %v", err)
	}
}

func TestExportGalleryJSONScopedToAnIDSet(t *testing.T) {
	cx := openIOTestDB(t)
	if err := loadExport(t, cx, Export{
		Version:       ExportVersion,
		TagCategories: []tagCategoryRow{{ID: 1, Name: "general", Color: "#888888", IsBuiltin: 1}},
		Tags: []tagRow{
			{ID: 1, Name: "kept", CategoryID: 1},
			{ID: 2, Name: "dropped", CategoryID: 1},
		},
		Images: []imageRow{
			{ID: 10, SHA256: "a", CanonicalPath: "a.png", FileType: "image/png"},
			{ID: 11, SHA256: "b", CanonicalPath: "b.png", FileType: "image/png"},
			{ID: 12, SHA256: "c", CanonicalPath: "c.png", FileType: "image/png"},
		},
		ImageTags: []imageTagRow{
			{ImageID: 10, TagID: 1},
			{ImageID: 12, TagID: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var scoped, whole bytes.Buffer
	handle := gallery.Handle{Name: "t", DB: cx}
	if err := ExportGalleryJSON(handle, &scoped, []int64{10, 11}); err != nil {
		t.Fatal(err)
	}
	if err := ExportGalleryJSON(handle, &whole, nil); err != nil {
		t.Fatal(err)
	}

	var got, all Export
	if err := json.Unmarshal(scoped.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(whole.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Images) != 3 {
		t.Fatalf("the unscoped export carries %d image(s), want 3", len(all.Images))
	}
	var ids []int64
	for _, img := range got.Images {
		ids = append(ids, img.ID)
	}
	if !slices.Equal(ids, []int64{10, 11}) {
		t.Errorf("scoped images = %v, want [10 11]", ids)
	}
	for _, it := range got.ImageTags {
		if it.ImageID == 12 {
			t.Error("a tag row for an image outside the scope rode along")
		}
	}
	if len(got.Tags) != len(all.Tags) {
		t.Errorf("scoped tags = %d, want the whole catalog's %d", len(got.Tags), len(all.Tags))
	}
}

func loadExport(t *testing.T, database *db.DB, exp Export) error {
	t.Helper()
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	return loadExportStream(database, bytes.NewReader(body))
}

func TestLoadExportStopsAtTheFirstBadRow(t *testing.T) {
	cx := openIOTestDB(t)

	err := loadExport(t, cx, Export{
		Version: ExportVersion,
		Tags: []tagRow{
			{ID: 7, Name: "first", CategoryID: 1},
			{ID: 7, Name: "collides", CategoryID: 1},
		},
		Images: []imageRow{
			{ID: 1, SHA256: "a", CanonicalPath: "a.png", FileType: "image/png"},
		},
	})
	if err == nil {
		t.Fatal("a colliding tag id should fail the import")
	}
	if !strings.Contains(err.Error(), "insert tag") || !strings.Contains(err.Error(), "collides") {
		t.Errorf("error = %v, want it to name the table and the row", err)
	}

	images := countRows(t, cx, "images")
	if images != 0 {
		t.Errorf("%d image(s) landed behind a failed import, want 0", images)
	}
}

func TestLoadExportReportsTheFirstBadRow(t *testing.T) {
	cx := openIOTestDB(t)

	err := loadExport(t, cx, Export{
		Version: ExportVersion,
		Tags: []tagRow{
			{ID: 7, Name: "first", CategoryID: 1},
			{ID: 7, Name: "collides", CategoryID: 1},
		},
		Images: []imageRow{
			{ID: 1, SHA256: "a", CanonicalPath: "a.png", FileType: "image/png"},
		},
		ImageTags: []imageTagRow{{ImageID: 1, TagID: 7}, {ImageID: 1, TagID: 7}},
	})

	if err == nil || !strings.Contains(err.Error(), "insert tag ") {
		t.Errorf("error = %v, want the colliding tag's", err)
	}
}

func keysMoved(t *testing.T, body []byte, first, last []string) []byte {
	t.Helper()
	var keys []string
	values := map[string]json.RawMessage{}
	if err := decodeSections(bytes.NewReader(body), func(key string, dec *json.Decoder) error {
		var v json.RawMessage
		keys = append(keys, key)
		values[key] = v
		if err := dec.Decode(&v); err != nil {
			return err
		}
		values[key] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var order []string
	order = append(order, first...)
	for _, k := range keys {
		if !slices.Contains(first, k) && !slices.Contains(last, k) {
			order = append(order, k)
		}
	}
	order = append(order, last...)
	var out bytes.Buffer
	out.WriteString("{")
	for i, k := range order {
		v, ok := values[k]
		if !ok {
			t.Fatalf("the document has no %q", k)
		}
		if i > 0 {
			out.WriteString(",")
		}
		name, _ := json.Marshal(k)
		out.Write(name)
		out.WriteString(":")
		out.Write(v)
	}
	out.WriteString("}")
	return out.Bytes()
}

func tableRows(t *testing.T, database *db.DB, table string) []string {
	t.Helper()
	rows, err := database.Read.Query(`SELECT * FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	slices.Sort(out)
	return out
}

func TestImportGalleryJSON_ReadsSectionsInAnyOrder(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	img := seedRichImage(t, cx)
	if _, err := cx.DB.Write.Exec(`INSERT INTO image_collections (image_id, name, position) VALUES (?, 'set', 1)`, img.ID); err != nil {
		t.Fatal(err)
	}
	body := mustExportJSON(t, cx).Bytes()
	inOrder := openIOTestDB(t)
	if err := loadExportStream(inOrder, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	var tagCount, rank int
	if err := inOrder.Read.QueryRow(`SELECT tag_count, rating_rank FROM images WHERE id = ?`, img.ID).Scan(&tagCount, &rank); err != nil {
		t.Fatal(err)
	}
	if tagCount == 0 || rank != 3 {
		t.Fatalf("in order: tag_count %d, rating_rank %d, want some tags and explicit", tagCount, rank)
	}

	shuffled := openIOTestDB(t)
	moved := keysMoved(t, body,
		[]string{"image_tags", "image_collections", "image_tag_sources", "image_paths", "image_sources"},
		[]string{"images", "tags", "tag_categories", "version"})
	if err := loadExportStream(shuffled, bytes.NewReader(moved)); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"images", "image_tags", "image_tag_sources", "image_collections", "collection_counts",
		"image_paths", "image_sources", "tags", "tag_categories"} {
		if got, want := tableRows(t, shuffled, table), tableRows(t, inOrder, table); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", table, got, want)
		}
	}
}

func TestImportGalleryJSON_PreV10WithTheVersionLast(t *testing.T) {
	body, err := json.Marshal(Export{
		Version:       9,
		TagCategories: []tagCategoryRow{{ID: 1, Name: "general", Color: "#888888", IsBuiltin: 1}},
		Tags:          []tagRow{{ID: 1, Name: "kept", CategoryID: 1}},
		Images:        []imageRow{{ID: 1, SHA256: "a", CanonicalPath: "a.png", FileType: "image/png"}},
		ImageTags:     []imageTagRow{{ImageID: 1, TagID: 1, TaggerName: sql.NullString{String: "wd", Valid: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cx := openIOTestDB(t)

	if err := loadExportStream(cx, bytes.NewReader(keysMoved(t, body, nil, []string{"version"}))); err != nil {
		t.Fatal(err)
	}

	var scheduled, scheduledPTR int
	if err := cx.Read.QueryRow(`SELECT scheduled_lookup, scheduled_lookup_ptr FROM images WHERE id = 1`).Scan(&scheduled, &scheduledPTR); err != nil {
		t.Fatal(err)
	}
	if scheduled != 1 || scheduledPTR != 1 {
		t.Errorf("opt-ins = %d/%d, want 1/1", scheduled, scheduledPTR)
	}
	if n := countRows(t, cx, "image_tag_sources WHERE image_id = 1 AND tag_id = 1 AND source = 'wd'"); n != 1 {
		t.Errorf("derived ledger rows = %d, want 1", n)
	}
}

func TestImportGalleryJSON_PreV3SeriesInAnotherCaseJoinsOneCollection(t *testing.T) {
	cx := openIOTestDB(t)

	if err := loadExport(t, cx, Export{
		Version: 2,
		Images: []imageRow{
			{ID: 1, SHA256: "a", CanonicalPath: "a.png", FileType: "image/png", Series: "Saga", SeriesOrder: sql.NullInt64{Int64: 1, Valid: true}},
			{ID: 2, SHA256: "b", CanonicalPath: "b.png", FileType: "image/png", Series: "saga", SeriesOrder: sql.NullInt64{Int64: 2, Valid: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	names, err := db.QueryStrings(cx.Read, `SELECT DISTINCT name COLLATE BINARY FROM image_collections`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"Saga"}) {
		t.Errorf("collection spellings = %v, want [Saga]", names)
	}
	series, err := db.QueryStrings(cx.Read, `SELECT series FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(series, []string{"Saga", "Saga"}) {
		t.Errorf("images.series = %v, want both Saga", series)
	}
	if n := countRows(t, cx, "image_collections WHERE image_id = 2 AND position = 2"); n != 1 {
		t.Errorf("the variant's membership kept its order %d times, want 1", n)
	}
}

func TestLoadExportCanonicalisesNotRelatedPairs(t *testing.T) {
	cx := openIOTestDB(t)

	if err := loadExport(t, cx, Export{
		Version: ExportVersion,
		Images: []imageRow{
			{ID: 1, SHA256: "a", CanonicalPath: "a.png", FileType: "image/png"},
			{ID: 2, SHA256: "b", CanonicalPath: "b.png", FileType: "image/png"},
		},
		NotRelatedPairs: []notRelatedPairRow{
			{AImageID: 2, BImageID: 1, CreatedAt: "2026-01-01T00:00:00Z"},
			{AImageID: 1, BImageID: 2, CreatedAt: "2026-01-02T00:00:00Z"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := cx.Read.Query(`SELECT a_image_id, b_image_id FROM not_related_pairs`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got [][2]int64
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]int64{a, b})
	}
	if len(got) != 1 || got[0] != [2]int64{1, 2} {
		t.Fatalf("not_related_pairs = %v, want one row (1,2)", got)
	}
}

func TestApplyImport_AReplaceLeavesANestedGallerysFilesAlone(t *testing.T) {
	dir := t.TempDir()
	cx := newGallery(t, dir, "stock")
	inner := filepath.Join(cx.GalleryPath, ".H")
	mustMkdirAll(t, inner, 0o755)
	top := filepath.Join(cx.GalleryPath, "top.png")
	nested := filepath.Join(inner, "in.png")
	mustWriteFile(t, top, makePNGBytes(t, 8, 8, 1, 2, 3), 0o644)
	mustWriteFile(t, nested, makePNGBytes(t, 8, 8, 4, 5, 6), 0o644)
	for _, p := range []string{top, nested} {
		if _, _, err := ingestAt(cx, p); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(dir, "backup.zip")
	mustWriteFile(t, archive, mustExportArchive(t, cx, "db").Bytes(), 0o644)
	mustWriteFile(t, nested, []byte("the nested gallery's own bytes"), 0o644)
	closeDB(t, cx.DB)

	b := gallery.NewBoundary(cx.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil)
	leftOut, err := ApplyImport("zip", archive, cx.DBPath, cx.ThumbnailsPath, b, 0, noQuiesce)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if leftOut != 1 {
		t.Errorf("left out %d archive entry, want 1", leftOut)
	}
	if got, err := os.ReadFile(nested); err != nil || string(got) != "the nested gallery's own bytes" {
		t.Errorf("the nested gallery's file was touched (err=%v)", err)
	}
	if _, err := os.Stat(top); err != nil {
		t.Errorf("the gallery's own file was not restored: %v", err)
	}
	database := reopen(t, cx.DBPath)
	if n := countRows(t, database, "images WHERE is_missing = 1 AND canonical_path = ?", nested); n != 1 {
		t.Errorf("the nested gallery's row is still visible here")
	}
}

func TestApplyImport_AReplaceRefusesAFolderAnotherGalleryShares(t *testing.T) {
	dir := t.TempDir()
	cx := newGallery(t, dir, "stock")
	own := filepath.Join(cx.GalleryPath, "own.png")
	mustWriteFile(t, own, makePNGBytes(t, 8, 8, 1, 2, 3), 0o644)
	if _, _, err := ingestAt(cx, own); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "backup.zip")
	mustWriteFile(t, archive, mustExportArchive(t, cx, "db").Bytes(), 0o644)
	twins := filepath.Join(cx.GalleryPath, "twin.png")
	mustWriteFile(t, twins, []byte("the other gallery's file"), 0o644)
	closeDB(t, cx.DB)

	b := gallery.NewBoundary(cx.GalleryPath, []gallery.Fence{{Path: cx.GalleryPath, Owner: "twin", Kind: gallery.FenceGallery}}, nil)
	if _, err := ApplyImport("zip", archive, cx.DBPath, cx.ThumbnailsPath, b, 0, noQuiesce); err == nil || !strings.Contains(err.Error(), "indexes the same folder") {
		t.Fatalf("ApplyImport err = %v, want a refusal naming the shared folder", err)
	}
	if _, err := os.Stat(twins); err != nil {
		t.Errorf("the other gallery's file went: %v", err)
	}
}

func TestApplyImport_ACompatibilityReplaceRebuildsTheGallery(t *testing.T) {
	a, b := string(makeUniquePNG(t, 21)), string(makeUniquePNG(t, 22))
	for _, tc := range []struct {
		name    string
		entries [][2]string
		files   []string
	}{
		{"hydrus", [][2]string{
			{"a.png", a}, {"a.png.txt", "creator:bob\nred hair\n"},
			{"sub/b.png", b}, {"sub/b.png.txt", "sky\n"},
		}, []string{"a.png", "sub/b.png"}},
		{"blombooru", [][2]string{
			{"backup.json", `{"version":1,"type":"full","media":[` +
				`{"filename":"a.png","tags":["bob","red_hair"],"archive_path":"media/a.png"},` +
				`{"filename":"b.png","tags":["sky"],"archive_path":"media/b.png"}]}`},
			{"tags.csv", "bob,1\nred_hair,0\nsky,0\n"},
			{"media/a.png", a}, {"media/b.png", b},
		}, []string{"a.png", "b.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cx := newGallery(t, dir, "stock")
			old := writeImage(t, cx, "old.png", makePNGBytes(t, 8, 8, 1, 2, 3))
			archive := filepath.Join(dir, tc.name+".zip")
			mustWriteFile(t, archive, zipOf(t, tc.entries), 0o644)
			closeDB(t, cx.DB)

			leftOut, err := ApplyImport("zip", archive, cx.DBPath, cx.ThumbnailsPath, gallery.NewBoundary(cx.GalleryPath, nil, nil), 0, noQuiesce)
			if err != nil {
				t.Fatalf("ApplyImport: %v", err)
			}
			if leftOut != 0 {
				t.Errorf("left out %d archive entries, want 0", leftOut)
			}
			database := reopen(t, cx.DBPath)
			if n := countRows(t, database, "images WHERE canonical_path = ?", old.CanonicalPath); n != 0 {
				t.Error("the replaced gallery's row survived")
			}
			if _, err := os.Stat(old.CanonicalPath); !os.IsNotExist(err) {
				t.Errorf("the replaced gallery's file survived: %v", err)
			}
			for _, rel := range tc.files {
				path := filepath.Join(cx.GalleryPath, rel)
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s is not on disk: %v", rel, err)
				}
				if n := countRows(t, database, "images WHERE is_missing = 0 AND canonical_path = ?", path); n != 1 {
					t.Errorf("%s has no visible row", rel)
				}
			}
			got := tagCategories(t, database)
			if got["bob"] != "artist" || got["red_hair"] != "general" {
				t.Errorf("tag categories = %v, want bob artist and red_hair general", got)
			}
		})
	}
}

func refusedArchiveLeavesTheGallery(t *testing.T, archive []byte, maxMB int, wantErr string) (dir, galleryPath string) {
	t.Helper()
	dir = t.TempDir()
	cx := newGallery(t, dir, "stock")
	kept := filepath.Join(cx.GalleryPath, "kept.png")
	writeImage(t, cx, "kept.png", makePNGBytes(t, 8, 8, 1, 2, 3))
	path := filepath.Join(dir, "refused.zip")
	mustWriteFile(t, path, archive, 0o644)
	closeDB(t, cx.DB)

	b := gallery.NewBoundary(cx.GalleryPath, nil, nil)
	if _, err := ApplyImport("zip", path, cx.DBPath, cx.ThumbnailsPath, b, maxMB, noQuiesce); err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %v, want %q", err, wantErr)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the gallery's file is gone: %v", err)
	}
	if n := countRows(t, reopen(t, cx.DBPath), "images WHERE canonical_path = ?", kept); n != 1 {
		t.Errorf("the gallery's row is gone")
	}
	return dir, cx.GalleryPath
}

func reopen(t *testing.T, path string) *db.DB {
	t.Helper()
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func closeDB(t *testing.T, database *db.DB) {
	t.Helper()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

var replaceInners = [][2]string{
	{"native", "monbooru.json"},
	{"light", "tags.json"},
}

func innerFor(name string) [2]string {
	if name == "tags.json" {
		return [2]string{name, `{"version":1,"images":[]}`}
	}
	return [2]string{name, `{"version":1}`}
}

func TestApplyImport_ACorruptEntryLeavesTheGalleryAsItWas(t *testing.T) {
	for _, inner := range replaceInners {
		t.Run(inner[0], func(t *testing.T) {
			payload := "the stored bytes of a picture"
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for _, e := range [][2]string{innerFor(inner[1]), {"gallery/bad.png", payload}} {
				w, err := zw.CreateHeader(&zip.FileHeader{Name: e[0], Method: zip.Store})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte(e[1])); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			archive := buf.Bytes()
			archive[bytes.Index(archive, []byte(payload))] ^= 0xff

			refusedArchiveLeavesTheGallery(t, archive, 0, "checksum")
		})
	}
}

func TestApplyImport_AFileAndAFolderOfOneNameLeaveTheGalleryAsItWas(t *testing.T) {
	for _, inner := range replaceInners {
		t.Run(inner[0], func(t *testing.T) {
			refusedArchiveLeavesTheGallery(t, zipOf(t, [][2]string{
				innerFor(inner[1]), {"gallery/x", "a file"}, {"gallery/x/y.png", "a file inside it"},
			}), 0, "both a file and a folder")
		})
	}
}

func TestApplyImport_TwoEntriesNamingOneFileLeaveTheGalleryAsItWas(t *testing.T) {
	for _, inner := range replaceInners {
		for _, second := range []string{"gallery/x.png", "gallery/sub/../x.png"} {
			t.Run(inner[0]+" "+second, func(t *testing.T) {
				refusedArchiveLeavesTheGallery(t, zipOf(t, [][2]string{
					innerFor(inner[1]), {"gallery/x.png", "one picture"}, {second, "another picture"},
				}), 0, "names the same file")
			})
		}
	}
}

func TestApplyImport_EntriesDifferingInCaseOnAFoldingFilesystemLeaveTheGalleryAsItWas(t *testing.T) {
	saved := foldsCase
	foldsCase = func(string) bool { return true }
	t.Cleanup(func() { foldsCase = saved })
	for _, inner := range replaceInners {
		for _, tc := range []struct {
			name    string
			entries [][2]string
			wantErr string
		}{
			{"one file", [][2]string{{"gallery/X.png", "one picture"}, {"gallery/x.png", "another picture"}}, "names the same file"},
			{"a file and a folder", [][2]string{{"gallery/X", "a file"}, {"gallery/x/y.png", "a file inside it"}}, "both a file and a folder"},
		} {
			t.Run(inner[0]+" "+tc.name, func(t *testing.T) {
				refusedArchiveLeavesTheGallery(t, zipOf(t, append([][2]string{innerFor(inner[1])}, tc.entries...)), 0, tc.wantErr)
			})
		}
	}
}

func TestPlanExtraction_KeepsEntriesDifferingInCaseWhereTheFilesystemTellsThemApart(t *testing.T) {
	root := t.TempDir()
	if foldsCase(root) {
		t.Skip("this filesystem folds case")
	}
	archive := zipOf(t, [][2]string{{"X.png", "one picture"}, {"x.png", "another picture"}})
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	files := []translatedFile{{rel: "X.png", file: zr.File[0]}, {rel: "x.png", file: zr.File[1]}}
	if _, err := planExtraction(gallery.NewBoundary(root, nil, nil), files, 0); err != nil {
		t.Fatalf("refused two names a case-sensitive folder holds apart: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("the probe left %d file(s) behind", len(entries))
	}
}

func TestApplyImport_AnEntryNamingTheRootLeavesTheGalleryAsItWas(t *testing.T) {
	for _, inner := range replaceInners {
		for _, name := range []string{"gallery/.", "gallery/x/.."} {
			t.Run(inner[0]+" "+name, func(t *testing.T) {
				refusedArchiveLeavesTheGallery(t, zipOf(t, [][2]string{innerFor(inner[1]), {name, "x"}}), 0, "names the gallery root")
			})
		}
	}
}

func TestApplyImport_AFailureAfterTheWipeStillReconciles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a folder with no permissions")
	}
	for _, tc := range []struct {
		name   string
		export func(gallery.Handle, io.Writer) error
	}{
		{"native", func(cx gallery.Handle, w io.Writer) error { return ExportGalleryArchive(cx, "json", w) }},
		{"light", ExportGalleryLight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := newGallery(t, dir, "src")
			mustMkdirAll(t, filepath.Join(src.GalleryPath, "locked"), 0o755)
			for name, marker := range map[string]uint8{"locked/a.png": 1, "ok.png": 2} {
				p := filepath.Join(src.GalleryPath, name)
				mustWriteFile(t, p, makeUniquePNG(t, marker), 0o644)
				if _, _, err := ingestAt(src, p); err != nil {
					t.Fatal(err)
				}
			}
			var buf bytes.Buffer
			if err := tc.export(src, &buf); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(dir, "backup.zip")
			mustWriteFile(t, archive, buf.Bytes(), 0o644)

			cx := newGallery(t, dir, "stock")
			closeDB(t, cx.DB)

			locked := filepath.Join(cx.GalleryPath, "locked")
			mustMkdirAll(t, locked, 0o755)
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

			b := gallery.NewBoundary(cx.GalleryPath, nil, nil)
			if _, err := ApplyImport("zip", archive, cx.DBPath, cx.ThumbnailsPath, b, 0, noQuiesce); err == nil {
				t.Fatal("an extraction that failed reported success")
			}
			database := reopen(t, cx.DBPath)
			if n := countRows(t, database, "images WHERE is_missing = 0 AND canonical_path = ?", filepath.Join(cx.GalleryPath, "ok.png")); n != 1 {
				t.Error("the extracted file has no visible row")
			}
			if n := countRows(t, database, "images WHERE is_missing = 1 AND canonical_path = ?", filepath.Join(locked, "a.png")); n != 1 {
				t.Error("the file that could not be extracted is not flagged missing")
			}
		})
	}
}

func TestLoadThenInstall_AFailedRenameKeepsTheLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "monbooru.db")
	live, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(live); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Write.Exec(`INSERT INTO tags (name, category_id, origin) VALUES ('kept', 1, 'user')`); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	newPath := filepath.Join(dir, "import-1.db")

	err = loadThenInstall(newPath, dbPath, filepath.Join(dir, "thumbnails"), "", noQuiesce, func(*db.DB) error {
		return os.Remove(newPath)
	})
	if err == nil {
		t.Fatal("a rename of a file that is gone reported success")
	}
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if n := countRows(t, database, "tags WHERE name = 'kept'"); n != 1 {
		t.Errorf("the live database lost its rows: %d", n)
	}
}

func TestRebaseImagePaths_RebasesMoreRowsThanOneChunk(t *testing.T) {
	const n = 2500
	database := openIOTestDB(t)
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "f"), 0o755)
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		canonical := fmt.Sprintf("/old/f/img-%d.png", i)
		var id int64
		if err := tx.QueryRow(`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
			VALUES (?, ?, 'f', 'png', 1, 1) RETURNING id`, fmt.Sprintf("%064d", i), canonical).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1), (?, ?, 0)`,
			id, canonical, id, fmt.Sprintf("/old/other/alias-%d.png", i)); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			mustWriteFile(t, filepath.Join(root, "f", fmt.Sprintf("img-%d.png", i)), nil, 0o644)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := rebaseImagePaths(database, gallery.NewBoundary(root, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, database, "images WHERE canonical_path = ? || '/f/img-' || (id - 1) || '.png'", root); got != n {
		t.Errorf("%d of %d canonical paths rebased", got, n)
	}
	if got := countRows(t, database, "image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.is_canonical = 1 AND ip.path = i.canonical_path"); got != n {
		t.Errorf("%d of %d canonical path rows rebased", got, n)
	}
	if got := countRows(t, database, "image_paths WHERE is_canonical = 0 AND path = ? || '/other/alias-' || (image_id - 1) || '.png'", root); got != n {
		t.Errorf("%d of %d alias rows kept their own folder", got, n)
	}
	if got := countRows(t, database, "images WHERE is_missing = 1"); got != n/2 {
		t.Errorf("%d rows flagged missing, want %d", got, n/2)
	}
}

func TestRebaseImagePaths_SlashesAWindowsFolderAndKeepsAPosixBackslash(t *testing.T) {
	database := openIOTestDB(t)
	root := t.TempDir()
	if _, err := database.Write.Exec(`
		INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, file_size, is_missing) VALUES
			(1, 'a', 'C:\g\2024\jan\a.png',    '2024\jan',  'png', 1, 0),
			(2, 'b', 'C:\g\root.png',          '',          'png', 1, 0),
			(3, 'c', '/srv/g/art\weird/c.png', 'art\weird', 'png', 1, 0);
		INSERT INTO image_paths (image_id, path, is_canonical) VALUES
			(1, 'C:\g\2024\jan\a.png', 1),
			(1, 'C:\g\alias\a.png', 0),
			(3, '/srv/g/art\weird/c.png', 1)`,
	); err != nil {
		t.Fatal(err)
	}

	if err := rebaseImagePaths(database, gallery.NewBoundary(root, nil, nil)); err != nil {
		t.Fatal(err)
	}
	want := map[int64][2]string{
		1: {"2024/jan", filepath.Join(root, "2024", "jan", "a.png")},
		2: {"", filepath.Join(root, "root.png")},
		3: {`art\weird`, filepath.Join(root, `art\weird`, "c.png")},
	}
	for id, w := range want {
		var folder, canonical string
		if err := database.Read.QueryRow(`SELECT folder_path, canonical_path FROM images WHERE id = ?`, id).Scan(&folder, &canonical); err != nil {
			t.Fatal(err)
		}
		if folder != w[0] || canonical != w[1] {
			t.Errorf("image %d = (%q, %q), want (%q, %q)", id, folder, canonical, w[0], w[1])
		}
	}
	var alias string
	if err := database.Read.QueryRow(`SELECT path FROM image_paths WHERE image_id = 1 AND is_canonical = 0`).Scan(&alias); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "alias", "a.png"); alias != want {
		t.Errorf("alias path = %q, want %q", alias, want)
	}
}

func TestLoadExportKeepsTheCategoryOrderAndOrdersAnOlderDocument(t *testing.T) {
	cats := []tagCategoryRow{
		{ID: 1, Name: "general", Color: "#888888", IsBuiltin: 1, Position: 0},
		{ID: 2, Name: "artist", Color: "#888888", IsBuiltin: 1, Position: 2},
		{ID: 3, Name: "mine", Color: "#888888", Position: 1},
	}
	order := func(cx *db.DB) []string {
		t.Helper()
		names, err := db.QueryStrings(cx.Read, `SELECT name FROM tag_categories ORDER BY position`)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	current := openIOTestDB(t)
	if err := loadExport(t, current, Export{Version: ExportVersion, TagCategories: cats}); err != nil {
		t.Fatal(err)
	}
	if got, want := order(current), []string{"general", "mine", "artist"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want the document's %v", got, want)
	}

	older := openIOTestDB(t)
	if err := loadExport(t, older, Export{Version: 13, TagCategories: cats}); err != nil {
		t.Fatal(err)
	}
	if got, want := order(older), []string{"artist", "mine", "general"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want the shipped %v for a document without positions", got, want)
	}
}

func TestLoadExportRoundTripsEveryTableByteForByte(t *testing.T) {
	str := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	i64 := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	f64 := func(f float64) sql.NullFloat64 { return sql.NullFloat64{Float64: f, Valid: true} }
	const at = "2024-01-02T03:04:05Z"
	doc := Export{
		Version: ExportVersion,
		TagCategories: []tagCategoryRow{
			{ID: 1, Name: "general", Color: "#888888", IsBuiltin: 1, Position: 0},
			{ID: 5, Name: "mycat", Color: "#123456", Position: 1},
		},
		Tags: []tagRow{
			{ID: 1, Name: "kept", CategoryID: 1, UsageCount: 1, CreatedAt: at, Origin: "user", LastUsedAt: str(at)},
			{ID: 2, Name: "alias", CategoryID: 1, IsAlias: 1, CanonicalTagID: i64(1), CreatedAt: at, Stale: 1},
			{ID: 3, Name: "other", CategoryID: 5, UsageCount: 1, CreatedAt: at, Origin: "danbooru"},
		},
		TagImplications:  []tagImplicationRow{{ParentTagID: 3, ImpliedTagID: 1, CreatedAt: at, Origin: "user", Stale: 1}},
		TagNotes:         []tagNoteRow{{TagID: 1, Body: "a note", Links: "https://a.example/1"}},
		TagDistinctPairs: []tagDistinctPairRow{{ATagID: 1, BTagID: 3, CreatedAt: at}},
		Images: []imageRow{
			{ID: 1, SHA256: "sha-a", MD5: "md5-a", CanonicalPath: "/g/sub/a.png", FolderPath: "sub", FileType: "png",
				Width: i64(10), Height: i64(20), FileSize: 30, IsMissing: 1, IsFavorited: 1, IsInbox: 1,
				AutoTaggedAt: str(at), SourceType: "a1111", Origin: "upload", Source: "src", URL: "https://u.example",
				PageCount: i64(3), DurationSeconds: f64(1.5), Series: "set", SeriesOrder: i64(2), Note: "n",
				OriginalSource: "orig", Phash: i64(-77), LastReadPage: i64(1), UploadBatch: i64(9), IngestedAt: at,
				ScheduledLookup: 1, ScheduledLookupPTR: 1},
			{ID: 2, SHA256: "sha-b", CanonicalPath: "/g/b.mp4", FileType: "mp4", FileSize: 40,
				SourceType: "none", Origin: "ingest", IngestedAt: at},
		},
		ImageCollections: []imageCollectionRow{{ImageID: 1, Name: "set", Position: i64(2)}, {ImageID: 2, Name: "set"}},
		FindRelations:    []findRelationsRow{{Name: "set"}},
		ImageSources: []imageSourceRow{{ImageID: 1, Site: "danbooru", PostID: "123", URL: "https://d.example/123", MD5: "md5-a",
			Commentary: "c", Translated: "tr", Original: "o", Similarity: 87.5, MD5Match: "match", ParentURL: "https://d.example/1",
			UpgradeKept: 1, PostWidth: 11, PostHeight: 12, PostSize: 13, PostExt: "png", FetchedAt: at}},
		ImageAnnotations: []imageAnnotationRow{{ImageID: 1, Site: "danbooru", PostID: "123", X: 1, Y: 2, W: 3, H: 4, Body: "box", Manual: 1, FetchedAt: at}},
		ImagePaths: []imagePathRow{
			{ID: 1, ImageID: 1, Path: "/g/sub/a.png", IsCanonical: 1, MtimeUnix: 1700000000, MtimeNsec: 5},
			{ID: 2, ImageID: 2, Path: "/g/b.mp4", IsCanonical: 1},
		},
		ImageTags: []imageTagRow{
			{ImageID: 1, TagID: 1, IsAuto: 1, Confidence: f64(0.75), TaggerName: str("wd"), CreatedAt: at, Stale: 1},
			{ImageID: 2, TagID: 3, CreatedAt: at},
		},
		ImageTagSources: []imageTagSourceRow{{ImageID: 1, TagID: 1, Source: "wd", CreatedAt: at}, {ImageID: 2, TagID: 3, Source: "user", CreatedAt: at}},
		SDMetadata: []sdMetadataRow{
			{ImageID: 1, Prompt: str("p"), NegativePrompt: str("np"), Model: str("m"), Seed: i64(42), Sampler: str("s"),
				Steps: i64(20), CFGScale: f64(7.5), RawParams: str("raw"), GenerationHash: str("h")},
			{ImageID: 2},
		},
		ComfyUIMetadata: []comfyMetadataRow{
			{ImageID: 1, Prompt: str("p"), ModelCheckpoint: str("ck"), Seed: i64(43), Sampler: str("s"), Steps: i64(21),
				CFGScale: f64(6.5), RawWorkflow: str(`{"a":1}`), GenerationHash: str("h")},
			{ImageID: 2},
		},
		MangaMetadata: []mangaMetadataRow{
			{ImageID: 1, Title: str("t"), Series: str("se"), Number: str("1"), Volume: str("2"), Count: i64(3), Summary: str("su"),
				Notes: str("no"), Year: i64(2020), Month: i64(4), Day: i64(5), Writer: str("w"), Penciller: str("pe"), Inker: str("i"),
				Colorist: str("co"), Letterer: str("l"), CoverArtist: str("ca"), Editor: str("e"), Publisher: str("pu"),
				Imprint: str("im"), Genre: str("g"), Web: str("we"), LanguageISO: str("ja"), Format: str("f"), Manga: str("Yes"),
				AgeRating: str("a"), CommunityRating: f64(4.5), XMLPageCount: i64(6), RawXML: str("<x/>")},
			{ImageID: 2},
		},
		DupGroups:       []dupGroupRow{{ID: 1, OriginalImageID: 1, CreatedAt: at}},
		DupGroupMembers: []dupGroupMemberRow{{ImageID: 1, GroupID: 1, CreatedAt: at}, {ImageID: 2, GroupID: 1, CreatedAt: at}},
		AltGroups:       []altGroupRow{{ID: 1, CreatedAt: at}},
		AltGroupMembers: []altGroupMemberRow{{ImageID: 1, GroupID: 1, CreatedAt: at}},
		VersionEdges:    []versionEdgeRow{{ChildImageID: 2, ParentImageID: 1, CreatedAt: at}},
		DerivativeEdges: []derivativeEdgeRow{{DerivativeImageID: 2, SourceImageID: 1, CreatedAt: at}},
		NotRelatedPairs: []notRelatedPairRow{{AImageID: 1, BImageID: 2, CreatedAt: at}},
		SavedSearches:   []savedSearchRow{{ID: 1, Name: "s", Query: "kept", Sort: "newest", Order: "desc", Seed: "x", CreatedAt: at}},
		ImageLookups: []imageLookupRow{
			{ImageID: 1, Backend: "saucenao", Attempts: 2, QueuedAt: str(at), JobID: i64(4), LastAt: str(at), LastResult: "hit", NextDueAt: str(at), PTRCursor: i64(8)},
			{ImageID: 2, Backend: "ptr"},
		},
	}
	first := openIOTestDB(t)
	if err := loadExport(t, first, doc); err != nil {
		t.Fatal(err)
	}
	want := mustExportJSON(t, gallery.Handle{Name: "g", GalleryPath: "/g", DB: first}).Bytes()

	second := openIOTestDB(t)
	if err := loadExportStream(second, bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	got := mustExportJSON(t, gallery.Handle{Name: "g", GalleryPath: "/g", DB: second}).Bytes()

	if !bytes.Equal(got, want) {
		t.Errorf("re-export differs:\n got %s\nwant %s", got, want)
	}
	var exported Export
	if err := json.Unmarshal(want, &exported); err != nil {
		t.Fatal(err)
	}
	exported.GalleryName, exported.GalleryPath = "", ""
	if !reflect.DeepEqual(exported, doc) {
		t.Errorf("export = %+v\nwant the loaded document %+v", exported, doc)
	}
	var sections int
	if err := decodeSections(bytes.NewReader(want), func(key string, dec *json.Decoder) error {
		var rows []json.RawMessage
		if _, ok := tableByKey[key]; !ok {
			return skipValue(dec)
		}
		if err := dec.Decode(&rows); err != nil {
			return err
		}
		if len(rows) == 0 {
			t.Errorf("section %s exported no rows", key)
		}
		sections++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sections != len(exportTables) {
		t.Errorf("document holds %d table sections, want %d", sections, len(exportTables))
	}
}
