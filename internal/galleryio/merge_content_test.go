package galleryio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
)

func sha256OfBytes(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func seedMergeTarget(t *testing.T, h gallery.Handle) (int64, string, string) {
	t.Helper()
	img := writeImage(t, h, "seed.png", makePNGBytes(t, 8, 8, 1, 2, 3))
	return img.ID, img.SHA256, filepath.Join(h.GalleryPath, "seed.png")
}

func imageTagNames(t *testing.T, h gallery.Handle, imgID int64) []string {
	t.Helper()
	out, err := db.QueryStrings(h.DB.Read, `
		SELECT t.name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id = ? ORDER BY t.name`, imgID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tagCategories(t *testing.T, database *db.DB) map[string]string {
	t.Helper()
	rows, err := database.Read.Query(`
		SELECT t.name, tc.name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	for rows.Next() {
		var name, cat string
		if err := rows.Scan(&name, &cat); err != nil {
			t.Fatal(err)
		}
		got[name] = cat
	}
	return got
}

func TestMergeGallery_JSON_ApplyTagsOnExistingImage(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	imgID, sha, _ := seedMergeTarget(t, stock)

	exp := Export{
		Version:     ExportVersion,
		GalleryName: "anywhere",
		TagCategories: []tagCategoryRow{{
			ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1,
		}},
		Tags: []tagRow{
			{ID: 100, Name: "merged_tag", CategoryID: 1},
		},
		Images: []imageRow{
			{ID: 999, SHA256: sha, CanonicalPath: "/elsewhere/seed.png", FolderPath: "", FileType: "png", FileSize: 8, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest"},
		},
		ImageTags: []imageTagRow{
			{ImageID: 999, TagID: 100, IsAuto: 0, CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := MergeGallery(context.Background(), libGallery(stock), "json", bytes.NewReader(body), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	tags := imageTagNames(t, stock, imgID)
	if !slices.Contains(tags, "merged_tag") {
		t.Errorf("expected merged_tag on image %d after merge, got %v", imgID, tags)
	}
}

func TestStripScratchSchema_LeavesNoTriggerOrSecondaryIndex(t *testing.T) {
	database := openIOTestDB(t)
	const schema = "sqlite_master WHERE type = 'trigger' OR (type = 'index' AND sql IS NOT NULL)"
	if n := countRows(t, database, schema); n == 0 {
		t.Fatal("the bootstrapped schema has no trigger or index to strip")
	}
	if err := stripScratchSchema(database); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, schema); n != 0 {
		t.Errorf("%d triggers and indexes survived the strip", n)
	}
}

func TestMergeGallery_JSON_CarriesNoTagNotes(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	_, sha, _ := seedMergeTarget(t, stock)
	exp := Export{
		Version:       ExportVersion,
		TagCategories: []tagCategoryRow{{ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1}},
		Tags:          []tagRow{{ID: 100, Name: "noted_elsewhere", CategoryID: 1}},
		TagNotes:      []tagNoteRow{{TagID: 100, Body: "written in another gallery"}},
		Images: []imageRow{
			{ID: 999, SHA256: sha, CanonicalPath: "/elsewhere/seed.png", FileType: "png", FileSize: 8, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest"},
		},
		ImageTags: []imageTagRow{{ImageID: 999, TagID: 100, CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGallery(context.Background(), libGallery(stock), "json", bytes.NewReader(body), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if n := countRows(t, stock.DB, "tags WHERE name = 'noted_elsewhere'"); n != 1 {
		t.Fatalf("the merged tag landed %d times", n)
	}
	if n := countRows(t, stock.DB, "tag_notes"); n != 0 {
		t.Errorf("a merge carried %d tag notes", n)
	}
}

func TestMergeGallery_DB_ApplyTagsOnExistingImage(t *testing.T) {
	src, stock := newGalleryPair(t)
	pngBytes := makePNGBytes(t, 8, 8, 11, 22, 33)
	srcImg := writeImage(t, src, "src.png", pngBytes)
	gen := mustTag(t, src.TagSvc, "from_db_merge", 1)
	if err := src.TagSvc.AddTagToImage(srcImg.ID, gen.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	tgtImg := writeImage(t, stock, "src.png", pngBytes)

	var snap bytes.Buffer
	if err := ExportGalleryDB(src, &snap); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGallery(context.Background(), libGallery(stock), "db", &snap, testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	got := imageTagNames(t, stock, tgtImg.ID)
	if !slices.Contains(got, "from_db_merge") {
		t.Errorf("expected from_db_merge on image %d after .db merge, got %v", tgtImg.ID, got)
	}
}

func assertMergedIntoPose(t *testing.T, stock gallery.Handle) {
	t.Helper()
	var color string
	if err := stock.DB.Read.QueryRow(`SELECT color FROM tag_categories WHERE name = 'pose'`).Scan(&color); err != nil {
		t.Fatalf("the merge left no pose category: %v", err)
	}
	if color != "#aa5500" {
		t.Errorf("pose category colour = %q, want #aa5500", color)
	}
	if got := tagCategories(t, stock.DB)["standing"]; got != "pose" {
		t.Errorf("standing landed in %q, want pose", got)
	}
	if n := countRows(t, stock.DB, "tags WHERE name = 'pose:standing'"); n != 0 {
		t.Errorf("the merge made %d general pose:standing tags", n)
	}
}

func TestMergeGallery_DB_CreatesTheUploadsMissingCategory(t *testing.T) {
	src, stock := newGalleryPair(t)
	pngBytes := makePNGBytes(t, 8, 8, 11, 22, 33)
	srcImg := writeImage(t, src, "src.png", pngBytes)
	pose, err := src.TagSvc.CreateCategory("pose", "#aa5500")
	if err != nil {
		t.Fatal(err)
	}
	standing := mustTag(t, src.TagSvc, "standing", pose.ID)
	if err := src.TagSvc.AddTagToImage(srcImg.ID, standing.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	writeImage(t, stock, "src.png", pngBytes)

	var snap bytes.Buffer
	if err := ExportGalleryDB(src, &snap); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGallery(context.Background(), libGallery(stock), "db", &snap, testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	assertMergedIntoPose(t, stock)
}

func TestMergeGallery_JSON_CreatesTheUploadsMissingCategory(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	_, sha, _ := seedMergeTarget(t, stock)
	exp := Export{
		Version: ExportVersion,
		TagCategories: []tagCategoryRow{
			{ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1},
			{ID: 9, Name: "pose", Color: "#aa5500"},
		},
		Tags: []tagRow{{ID: 100, Name: "standing", CategoryID: 9}},
		Images: []imageRow{
			{ID: 999, SHA256: sha, CanonicalPath: "/elsewhere/seed.png", FileType: "png", FileSize: 8, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest"},
		},
		ImageTags: []imageTagRow{{ImageID: 999, TagID: 100, CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGallery(context.Background(), libGallery(stock), "json", bytes.NewReader(body), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	assertMergedIntoPose(t, stock)
}

func TestMergeGallery_Zip_IngestsNewImage(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")

	existingID, _, _ := seedMergeTarget(t, stock)

	imgBytes := makeUniquePNG(t, 77)
	manifest := LightManifest{
		Version: LightManifestVersion,
		Images: []LightManifestImage{{
			SHA256: sha256OfBytes(t, imgBytes),
			Path:   "merged_subdir/new.png",
			Tags:   []string{"new_tag", "general:other_tag"},
		}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	zipBytes := zipOf(t, [][2]string{{"tags.json", string(manifestJSON)}, {"gallery/merged_subdir/new.png", string(imgBytes)}})

	if _, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(zipBytes), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	cx := stock
	n := countRows(t, cx.DB, "images")
	if n != 2 {
		t.Errorf("expected 2 images after zip merge, got %d", n)
	}
	var newID int64
	if err := cx.DB.Read.QueryRow(
		`SELECT id FROM images WHERE id != ?`, existingID,
	).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	got := imageTagNames(t, stock, newID)
	if !slices.Contains(got, "new_tag") || !slices.Contains(got, "other_tag") {
		t.Errorf("merged image tags = %v, want the manifest's two among them", got)
	}
}

func TestMergeGallery_Zip_LeavesNoPartialFileForAnOversizeEntry(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	big := bytes.Repeat([]byte("x"), 1<<20+1)
	manifestJSON, err := json.Marshal(LightManifest{
		Version: LightManifestVersion,
		Images:  []LightManifestImage{{SHA256: sha256OfBytes(t, big), Path: "big.png", Tags: []string{"t"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	upload := zipOf(t, [][2]string{{"tags.json", string(manifestJSON)}, {"gallery/big.png", string(big)}})

	res, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(upload), 1, nil)

	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Skipped != 1 || res.Added != 0 {
		t.Errorf("result = %+v, want the entry skipped", res)
	}
	if _, err := os.Stat(filepath.Join(stock.GalleryPath, "big.png")); !os.IsNotExist(err) {
		t.Errorf("a partial file is left behind: %v", err)
	}
}

func TestMergeGallery_Zip_ADatabaseOverTheMediaCapMerges(t *testing.T) {
	dir := t.TempDir()
	for name, archive := range archivesWithADocumentPastOneMiB(t, dir) {
		t.Run(name, func(t *testing.T) {
			stock := newGallery(t, dir, "stock-"+name)

			res, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(archive), 1, nil)

			if err != nil {
				t.Fatalf("MergeGallery: %v", err)
			}
			if res.Added != 1 {
				t.Errorf("result = %+v, want the archive's image added", res)
			}
		})
	}
}

func TestMergeGallery_StopsWhenCancelled(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	upload := zipOf(t, [][2]string{{"pic.png", string(makeUniquePNG(t, 95))}, {"pic.png.txt", "blue_sky\n"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := MergeGallery(ctx, libGallery(stock), "zip", bytes.NewReader(upload), testMaxFileSizeMB, nil)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res != (MergeResult{}) {
		t.Errorf("result = %+v, want nothing done", res)
	}
	if n := countRows(t, stock.DB, "images"); n != 0 {
		t.Errorf("images = %d, want 0", n)
	}
}

func TestMergeGallery_Zip_ACompatArchiveMergedTwiceLeavesOneCopy(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	upload := zipOf(t, [][2]string{{"pic.png", string(makeUniquePNG(t, 93))}, {"pic.png.txt", "blue_sky\n"}})

	var results []MergeResult
	for range 3 {
		res, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(upload), testMaxFileSizeMB, nil)
		if err != nil {
			t.Fatalf("MergeGallery: %v", err)
		}
		results = append(results, res)
	}

	want := []MergeResult{{Added: 1}, {Unchanged: 1}, {Unchanged: 1}}
	if !slices.Equal(results, want) {
		t.Errorf("results = %+v, want %+v", results, want)
	}
	if n := countRows(t, stock.DB, "image_paths"); n != 1 {
		t.Errorf("image_paths = %d, want 1", n)
	}
	entries, err := os.ReadDir(stock.GalleryPath)
	if err != nil {
		t.Fatal(err)
	}
	var pngs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			pngs = append(pngs, e.Name())
		}
	}
	if len(pngs) != 1 {
		t.Errorf("files = %v, want one copy", pngs)
	}
}

func mergeHydrusSidecar(t *testing.T, h gallery.Handle, seed uint8, sidecar string) map[string]string {
	t.Helper()
	zipBytes := zipOf(t, [][2]string{{"img.png", string(makeUniquePNG(t, seed))}, {"img.png.txt", sidecar}})
	if _, err := MergeGallery(context.Background(), libGallery(h), "zip", bytes.NewReader(zipBytes), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	return tagCategories(t, h.DB)
}

func TestMergeGallery_Zip_HydrusNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seed    uint8
		sidecar string
		want    map[string]string
		absent  []string
	}{
		{
			name:    "built-in namespaces and rewrites",
			seed:    91,
			sidecar: "medium:digital\nperson:alice\nyear:2024\nspecies:fox\ncreator:bob\nseries:fate\nstudio:shaft\ngeneral_only_tag\n",
			want: map[string]string{
				"digital":          "medium",
				"alice":            "person",
				"2024":             "year",
				"fox":              "species",
				"bob":              "artist",
				"fate":             "copyright",
				"shaft":            "copyright",
				"general_only_tag": "general",
			},
			absent: []string{"creator:bob", "series:fate", "studio:shaft"},
		},
		{
			name:    "spaces fold to underscores",
			seed:    92,
			sidecar: "character:hatsune miku\nseries:neon genesis evangelion\ncreator:john doe\nred hair\n",
			want: map[string]string{
				"hatsune_miku":            "character",
				"neon_genesis_evangelion": "copyright",
				"john_doe":                "artist",
				"red_hair":                "general",
			},
			absent: []string{"hatsune miku", "neon genesis evangelion", "john doe", "red hair"},
		},
		{
			name:    "unknown namespace drops to its subtag",
			seed:    93,
			sidecar: "flavor:red fox\nmeme:doge\ncharacter:saya\n",
			want: map[string]string{
				"red_fox": "general",
				"doge":    "general",
				"saya":    "character",
			},
			absent: []string{"flavor:red_fox", "flavor:red fox", "meme:doge"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeHydrusSidecar(t, newGallery(t, t.TempDir(), "stock"), tc.seed, tc.sidecar)
			for n, wantCat := range tc.want {
				if gotCat, ok := got[n]; !ok {
					t.Errorf("tag %q missing; got rows %v", n, got)
				} else if gotCat != wantCat {
					t.Errorf("tag %q in category %q, want %q", n, gotCat, wantCat)
				}
			}
			for _, raw := range tc.absent {
				if _, ok := got[raw]; ok {
					t.Errorf("literal %q should not have survived the rewrite; got rows %v", raw, got)
				}
			}
		})
	}
}

func TestMergeGallery_Zip_BlombooruKeepsAColonInATagName(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	upload := zipOf(t, [][2]string{
		{"backup.json", `{"version":1,"type":"full","media":[{"filename":"pic.png","tags":["re:zero","meta:highres"],"archive_path":"media/pic.png"}]}`},
		{"tags.csv", "meta:highres,0\n"},
		{"media/pic.png", string(makeUniquePNG(t, 94))},
	})

	if _, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(upload), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	got := tagCategories(t, stock.DB)
	for _, name := range []string{"re:zero", "meta:highres"} {
		if got[name] != "general" {
			t.Errorf("tag %q: category %q, want general; rows %v", name, got[name], got)
		}
	}
}

func TestMergeGallery_Zip_RejectsTraversalEntry(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	manifest := LightManifest{
		Version: LightManifestVersion,
		Images: []LightManifestImage{{
			SHA256: "doesnotmatter",
			Path:   "../escape.png",
			Tags:   []string{"trying_to_escape"},
		}},
	}
	manifestJSON, _ := json.Marshal(manifest)

	zipBytes := zipOf(t, [][2]string{{"tags.json", string(manifestJSON)}})

	if _, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(zipBytes), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery should skip traversal entries silently, got: %v", err)
	}
	cx := stock
	n := countRows(t, cx.DB, "images")
	if n != 0 {
		t.Errorf("expected no images ingested from a traversal-only manifest, got %d", n)
	}
}

func TestMergeGallery_JSON_ReportsWhatItLanded(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	_, sha, _ := seedMergeTarget(t, stock)

	exp := Export{
		Version:     ExportVersion,
		GalleryName: "anywhere",
		TagCategories: []tagCategoryRow{{
			ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1,
		}},
		Tags: []tagRow{{ID: 100, Name: "landed_tag", CategoryID: 1}},
		Images: []imageRow{
			{ID: 999, SHA256: sha, CanonicalPath: "/elsewhere/seed.png", FileType: "png", FileSize: 8, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest"},
			{ID: 1000, SHA256: "sha-the-target-has-never-seen", CanonicalPath: "/elsewhere/other.png", FileType: "png", FileSize: 9, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest"},
		},
		ImageTags: []imageTagRow{{ImageID: 999, TagID: 100, CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}

	res, err := MergeGallery(context.Background(), libGallery(stock), "json", bytes.NewReader(body), testMaxFileSizeMB, nil)
	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Added != 0 || res.Tagged != 1 || res.Skipped != 1 {
		t.Errorf("result = %+v, want 0 added / 1 tagged / 1 skipped", res)
	}
	if got, want := res.Summary(), "0 images added, 1 tagged, 1 not in this gallery"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	res, err = MergeGallery(context.Background(), libGallery(stock), "json", bytes.NewReader(body), testMaxFileSizeMB, nil)
	if err != nil {
		t.Fatalf("MergeGallery again: %v", err)
	}
	if got, want := res.Summary(), "0 images added, 0 tagged, 1 unchanged, 1 not in this gallery"; got != want {
		t.Errorf("a re-run's summary = %q, want %q", got, want)
	}
}

func TestMergeGallery_Zip_LeavesAFileForAFencedFolderUnextracted(t *testing.T) {
	src, dst := newGalleryPair(t)
	mustMkdirAll(t, filepath.Join(src.GalleryPath, ".H"), 0o755)
	writeImage(t, src, ".H/in.png", makePNGBytes(t, 8, 8, 9, 8, 7))
	buf := mustExportArchive(t, src, "db")
	inner := filepath.Join(dst.GalleryPath, ".H")
	dst.Bounds = new(atomic.Pointer[gallery.Boundary])
	dst.Bounds.Store(gallery.NewBoundary(dst.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))

	res, err := MergeGallery(context.Background(), libGallery(dst), "zip", buf, testMaxFileSizeMB, nil)
	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Added != 0 || res.LeftOut != 1 {
		t.Errorf("Added = %d, LeftOut = %d, want 0 and 1", res.Added, res.LeftOut)
	}
	if !strings.Contains(res.Summary(), "1 in folders this gallery leaves out") {
		t.Errorf("summary %q does not report the file it left out", res.Summary())
	}
	if _, err := os.Stat(filepath.Join(inner, "in.png")); !os.IsNotExist(err) {
		t.Errorf("the file was extracted into the fenced folder (err=%v)", err)
	}
}

func TestMergeGallery_Zip_LeavesAnEntryBehindALinkOutUnextracted(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	other := t.TempDir()
	stock.Bounds = new(atomic.Pointer[gallery.Boundary])
	stock.Bounds.Store(gallery.NewBoundary(stock.GalleryPath, []gallery.Fence{{Path: other, Owner: "other", Kind: gallery.FenceGallery}}, nil))
	if err := os.Symlink(other, filepath.Join(stock.GalleryPath, "lnk")); err != nil {
		t.Fatal(err)
	}
	pic := makeUniquePNG(t, 94)
	manifestJSON, err := json.Marshal(LightManifest{
		Version: LightManifestVersion,
		Images:  []LightManifestImage{{SHA256: sha256OfBytes(t, pic), Path: "lnk/x.png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	upload := zipOf(t, [][2]string{{"tags.json", string(manifestJSON)}, {"gallery/lnk/x.png", string(pic)}})

	res, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(upload), testMaxFileSizeMB, nil)

	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Added != 0 || res.LeftOut != 1 {
		t.Errorf("result = %+v, want the entry left out", res)
	}
	if _, err := os.Stat(filepath.Join(other, "x.png")); !os.IsNotExist(err) {
		t.Errorf("the file landed in the other gallery's folder (err=%v)", err)
	}
}

func TestMergeGallery_DB_CarriesTheTagsOfMissingRows(t *testing.T) {
	src, dst := newGalleryPair(t)
	pngBytes := makePNGBytes(t, 8, 8, 44, 55, 66)
	srcImg := writeImage(t, src, "gone.png", pngBytes)
	tag := mustTag(t, src.TagSvc, "kept_through_missing", 1)
	if err := src.TagSvc.AddTagToImage(srcImg.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := src.DB.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, srcImg.ID); err != nil {
		t.Fatal(err)
	}
	dstImg := writeImage(t, dst, "here.png", pngBytes)

	var snap bytes.Buffer
	if err := ExportGalleryDB(src, &snap); err != nil {
		t.Fatal(err)
	}
	res, err := MergeGallery(context.Background(), libGallery(dst), "db", &snap, testMaxFileSizeMB, nil)
	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Tagged != 1 || res.Skipped != 0 {
		t.Errorf("result = %+v, want the missing row's tags applied", res)
	}
	if got := imageTagNames(t, dst, dstImg.ID); !slices.Contains(got, "kept_through_missing") {
		t.Errorf("tags on the target image = %v, want kept_through_missing among them", got)
	}
}

func TestMergeGallery_JSON_CarriesTheTagsOfMissingRows(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	imgID, sha, _ := seedMergeTarget(t, stock)
	exp := Export{
		Version:     ExportVersion,
		GalleryName: "anywhere",
		TagCategories: []tagCategoryRow{{
			ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1,
		}},
		Tags: []tagRow{{ID: 100, Name: "kept_through_missing", CategoryID: 1}},
		Images: []imageRow{
			{ID: 999, SHA256: sha, CanonicalPath: "/elsewhere/seed.png", FileType: "png", FileSize: 8, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest", IsMissing: 1},
		},
		ImageTags: []imageTagRow{{ImageID: 999, TagID: 100, CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	res, err := MergeGallery(context.Background(), libGallery(stock), "json", bytes.NewReader(body), testMaxFileSizeMB, nil)
	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Tagged != 1 {
		t.Errorf("result = %+v, want the missing row's tags applied", res)
	}
	if got := imageTagNames(t, stock, imgID); !slices.Contains(got, "kept_through_missing") {
		t.Errorf("tags on the target image = %v, want kept_through_missing among them", got)
	}
}

func TestMergeGallery_Zip_AMissingRowTakesNoFileAtItsPath(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	exp := Export{
		Version:     ExportVersion,
		GalleryName: "anywhere",
		TagCategories: []tagCategoryRow{{
			ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1,
		}},
		Tags: []tagRow{{ID: 100, Name: "the_lost_one", CategoryID: 1}},
		Images: []imageRow{
			{ID: 1, SHA256: "sha-of-a-file-long-gone", CanonicalPath: "/elsewhere/x.png", FileType: "png", FileSize: 9, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest", IsMissing: 1},
		},
		ImageTags: []imageTagRow{{ImageID: 1, TagID: 100, CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes := zipOf(t, [][2]string{{"monbooru.json", string(body)}, {"gallery/x.png", string(makeUniquePNG(t, 71))}})

	res, err := MergeGallery(context.Background(), libGallery(stock), "zip", bytes.NewReader(zipBytes), testMaxFileSizeMB, nil)
	if err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if res.Added != 0 || res.Skipped != 1 {
		t.Errorf("result = %+v, want nothing added and the missing row skipped", res)
	}
	if n := countRows(t, stock.DB, "images"); n != 0 {
		t.Errorf("%d image(s) ingested from the file at the missing row's path, want none", n)
	}
}

func TestMergeGallery_ZipFeedsTheLandedPhash(t *testing.T) {
	stock := newGallery(t, t.TempDir(), "stock")
	imgBytes := makeUniquePNG(t, 91)
	manifestJSON, err := json.Marshal(LightManifest{
		Version: LightManifestVersion,
		Images:  []LightManifestImage{{SHA256: sha256OfBytes(t, imgBytes), Path: "landed.png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	zipBytes := zipOf(t, [][2]string{{"tags.json", string(manifestJSON)}, {"gallery/landed.png", string(imgBytes)}})

	g := libGallery(stock)
	tree := builtTree(t, g)
	if _, err := MergeGallery(context.Background(), g, "zip", bytes.NewReader(zipBytes), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	var id int64
	if err := stock.DB.Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !treeHoldsImage(t, stock, tree, id) {
		t.Errorf("the built tree lacks image %d", id)
	}
}

func seedImpliedPair(t *testing.T, src, dst gallery.Handle) int64 {
	t.Helper()
	body := makePNGBytes(t, 8, 8, 70, 80, 90)
	srcImg := writeImage(t, src, "pair.png", body)
	dstImg := writeImage(t, dst, "pair.png", body)
	for _, h := range []gallery.Handle{src, dst} {
		a, b := mustTag(t, h.TagSvc, "a", 1), mustTag(t, h.TagSvc, "b", 1)
		if _, err := h.TagSvc.AddImplication(a.ID, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.TagSvc.AddTagToImage(srcImg.ID, mustTag(t, src.TagSvc, "a", 1).ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if impliedFlag(t, src, srcImg.ID, "b") != 1 {
		t.Fatal("the source's fan-out did not write b implied")
	}
	return dstImg.ID
}

func impliedFlag(t *testing.T, h gallery.Handle, imageID int64, name string) int {
	t.Helper()
	var implied int
	if err := h.DB.Read.QueryRow(`SELECT it.is_implied FROM image_tags it JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id = ? AND t.name = ?`, imageID, name).Scan(&implied); err != nil {
		t.Fatalf("%s on image %d: %v", name, imageID, err)
	}
	return implied
}

func TestMergeGallery_DB_LeavesImpliedTagsImplied(t *testing.T) {
	src, stock := newGalleryPair(t)
	tgtID := seedImpliedPair(t, src, stock)
	var snap bytes.Buffer
	if err := ExportGalleryDB(src, &snap); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGallery(context.Background(), libGallery(stock), "db", &snap, testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if impliedFlag(t, stock, tgtID, "a") != 0 || impliedFlag(t, stock, tgtID, "b") != 1 {
		t.Error("the merge did not land a direct and leave b to the target's implication")
	}
}

func TestMergeGallery_JSON_LeavesImpliedTagsImplied(t *testing.T) {
	src, stock := newGalleryPair(t)
	tgtID := seedImpliedPair(t, src, stock)
	var doc bytes.Buffer
	if err := ExportGalleryJSON(src, &doc, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGallery(context.Background(), libGallery(stock), "json", &doc, testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if impliedFlag(t, stock, tgtID, "a") != 0 || impliedFlag(t, stock, tgtID, "b") != 1 {
		t.Error("the merge did not land a direct and leave b to the target's implication")
	}
}

func TestMergeGallery_DB_ReadsAFileWithoutTheImpliedColumn(t *testing.T) {
	src, stock := newGalleryPair(t)
	tgtID := seedImpliedPair(t, src, stock)
	var snap bytes.Buffer
	if err := ExportGalleryDB(src, &snap); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(t.TempDir(), "old.db")
	mustWriteFile(t, oldPath, snap.Bytes(), 0o644)
	old, err := db.Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	dependents, err := db.QueryAll(old.Read, func(r *sql.Rows) (string, error) {
		var stmt string
		err := r.Scan(&stmt)
		return stmt, err
	}, `SELECT 'DROP ' || type || ' ' || name FROM sqlite_master WHERE type IN ('index', 'trigger') AND sql LIKE '%is_implied%'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range append(dependents, `ALTER TABLE image_tags DROP COLUMN is_implied`) {
		if _, err := old.Write.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := MergeGallery(context.Background(), libGallery(stock), "db", f, testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if got := imageTagNames(t, stock, tgtID); !slices.Contains(got, "a") || !slices.Contains(got, "b") {
		t.Errorf("tags = %v, want a and b", got)
	}
}

func TestMergeGallery_DB_KeepsAnAutoTagsConfidence(t *testing.T) {
	src, stock := newGalleryPair(t)
	body := makePNGBytes(t, 8, 8, 12, 34, 56)
	srcImg := writeImage(t, src, "auto.png", body)
	stockImg := writeImage(t, stock, "auto.png", body)
	conf := 0.875
	if _, err := src.TagSvc.AddTagToImageReportingDup(srcImg.ID, mustTag(t, src.TagSvc, "1girl", 1).ID, true, &conf, "wd-swinv2"); err != nil {
		t.Fatal(err)
	}
	var snap bytes.Buffer
	if err := ExportGalleryDB(src, &snap); err != nil {
		t.Fatal(err)
	}
	upload := snap.Bytes()

	for _, tc := range []struct {
		name    string
		target  gallery.Handle
		imageID int64
	}{
		{"into a gallery without the tag", stock, stockImg.ID},
		{"back into its own gallery", src, srcImg.ID},
	} {
		if _, err := MergeGallery(context.Background(), libGallery(tc.target), "db", bytes.NewReader(upload), testMaxFileSizeMB, nil); err != nil {
			t.Fatalf("%s: MergeGallery: %v", tc.name, err)
		}
		var isAuto int
		var gotConf sql.NullFloat64
		var tagger sql.NullString
		if err := tc.target.DB.Read.QueryRow(
			`SELECT it.is_auto, it.confidence, it.tagger_name FROM image_tags it JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND t.name = '1girl'`, tc.imageID).Scan(&isAuto, &gotConf, &tagger); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if isAuto != 1 || !gotConf.Valid || gotConf.Float64 != conf || tagger.String != "wd-swinv2" {
			t.Errorf("%s: is_auto %d, confidence %v, tagger %q; want an auto row from wd-swinv2 at %v", tc.name, isAuto, gotConf, tagger.String, conf)
		}
	}
}
