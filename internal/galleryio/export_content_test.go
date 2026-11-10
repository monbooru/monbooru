package galleryio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/tags"
)

func TestExportGalleryLight_RoundTripsImages(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")

	pngBytes := makePNGBytes(t, 8, 8, 7, 8, 9)
	img := writeImage(t, cx, "subdir/light.png", pngBytes)
	tag := mustTag(t, cx.TagSvc, "light_tag", 1)
	if err := cx.TagSvc.AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := ExportGalleryLight(cx, &buf); err != nil {
		t.Fatalf("ExportGalleryLight: %v", err)
	}

	zr := readZip(t, &buf)
	var sawManifest, sawImage bool
	var manifest LightManifest
	for _, f := range zr.File {
		switch f.Name {
		case "tags.json":
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			if err := json.Unmarshal(b, &manifest); err != nil {
				t.Fatalf("manifest unmarshal: %v", err)
			}
			sawManifest = true
		case "gallery/subdir/light.png":
			sawImage = true
		}
	}
	if !sawManifest {
		t.Fatal("light export missing tags.json")
	}
	if !sawImage {
		t.Fatal("light export missing gallery/subdir/light.png")
	}
	if len(manifest.Images) != 1 {
		t.Fatalf("manifest images = %d, want 1", len(manifest.Images))
	}
	if got, want := manifest.Images[0].Path, "subdir/light.png"; got != want {
		t.Errorf("manifest path = %q, want %q", got, want)
	}
	if !slices.Contains(manifest.Images[0].Tags, "light_tag") {
		t.Errorf("manifest tags = %v, want the seeded tag among them", manifest.Images[0].Tags)
	}
}

func TestExportGalleryLightManifest_StreamsTagsJSON(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")

	pngBytes := makePNGBytes(t, 8, 8, 5, 6, 7)
	img := writeImage(t, cx, "lone.png", pngBytes)
	tag := mustTag(t, cx.TagSvc, "solo", 1)
	if err := cx.TagSvc.AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := ExportGalleryLightManifest(cx, &buf); err != nil {
		t.Fatalf("ExportGalleryLightManifest: %v", err)
	}
	var manifest LightManifest
	if err := json.Unmarshal(buf.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v (body=%q)", err, buf.String())
	}
	if manifest.Version != LightManifestVersion {
		t.Errorf("manifest version = %d, want %d", manifest.Version, LightManifestVersion)
	}
	if len(manifest.Images) != 1 {
		t.Fatalf("manifest images = %d, want 1", len(manifest.Images))
	}
	if got, want := manifest.Images[0].Path, "lone.png"; got != want {
		t.Errorf("manifest path = %q, want %q", got, want)
	}
	if !slices.Contains(manifest.Images[0].Tags, "solo") {
		t.Errorf("manifest tags = %v, want the seeded tag among them", manifest.Images[0].Tags)
	}
}

func TestExportGalleryLightManifest_LeavesMissingRowsOut(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	writeImage(t, cx, "here.png", makePNGBytes(t, 8, 8, 5, 6, 7))
	gone := writeImage(t, cx, "gone.png", makePNGBytes(t, 8, 8, 1, 2, 3))
	if _, err := gallery.SetMissing(cx.DB, gone.ID, true); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := ExportGalleryLightManifest(cx, &buf); err != nil {
		t.Fatalf("ExportGalleryLightManifest: %v", err)
	}
	var manifest LightManifest
	if err := json.Unmarshal(buf.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Images) != 1 || manifest.Images[0].Path != "here.png" {
		t.Errorf("manifest images = %+v, want only the present here.png", manifest.Images)
	}
}

func TestLightManifest_AGeneralNameWithAColonComesBackGeneral(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	imgID, _, _ := seedMergeTarget(t, cx)
	tag := mustTag(t, cx.TagSvc, "meta:highres", 1)
	if err := cx.TagSvc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := ExportGalleryLightManifest(cx, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"general:meta:highres"`) {
		t.Fatalf("manifest = %s, want the general name qualified", buf.String())
	}
	if _, err := MergeGallery(context.Background(), libGallery(cx), "json", &buf, testMaxFileSizeMB, nil); err != nil {
		t.Fatal(err)
	}
	if countRows(t, cx.DB, "image_tags it JOIN tags t ON t.id = it.tag_id JOIN tag_categories c ON c.id = t.category_id WHERE it.image_id = ? AND c.name = 'meta' AND t.name = 'highres'", imgID) != 0 {
		t.Error("the merge filed the general meta:highres as meta:highres in meta")
	}
}

func TestMergeGalleryLightJSON_AppliesTagsBySha(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	imgID, sha, _ := seedMergeTarget(t, cx)

	manifest := LightManifest{
		Version: LightManifestVersion,
		Images: []LightManifestImage{
			{SHA256: sha, Path: "ignored.png", Tags: []string{"merged_via_json", "artist:painter"}},
			{SHA256: "deadbeefnomatch", Path: "wherever.png", Tags: []string{"phantom"}},
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := MergeGallery(context.Background(), libGallery(cx), "json", bytes.NewReader(body), testMaxFileSizeMB, nil); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	got := imageTagNames(t, cx, imgID)
	wantSeen := map[string]bool{"merged_via_json": false, "painter": false}
	for _, n := range got {
		if _, ok := wantSeen[n]; ok {
			wantSeen[n] = true
		}
		if n == "phantom" {
			t.Errorf("phantom tag should not have been applied (no sha match), got %v", got)
		}
	}
	for k, seen := range wantSeen {
		if !seen {
			t.Errorf("expected tag %q after light-json merge, got %v", k, got)
		}
	}
}

func TestApplyLightReplace_NoFiles_PreservesGalleryDir(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")

	keepBytes := makePNGBytes(t, 8, 8, 5, 6, 7)
	keepPath := filepath.Join(cx.GalleryPath, "untouched.png")
	mustWriteFile(t, keepPath, keepBytes, 0o644)

	dbPath := cx.DBPath
	thumbsPath := cx.ThumbnailsPath
	galleryPath := cx.GalleryPath
	if err := cx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	mf := LightManifest{Version: LightManifestVersion}
	if _, err := ApplyLightReplace(mf, nil, dbPath, thumbsPath, gallery.NewBoundary(galleryPath, nil, nil), ImportSourceNative, 0, noQuiesce); err != nil {
		t.Fatalf("ApplyLightReplace: %v", err)
	}

	if _, err := os.Stat(keepPath); err != nil {
		t.Errorf("manifest-only replace wiped gallery file: %v", err)
	}
}

func TestApplyLightReplace_ClearsTheMangaCache(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	page := filepath.Join(gallery.MangaCacheDir(cx.ThumbnailsPath), "1", "page_0001.jpg")
	mustMkdirAll(t, filepath.Dir(page), 0o755)
	mustWriteFile(t, page, []byte("old library's page"), 0o644)
	if err := cx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	mf := LightManifest{Version: LightManifestVersion}
	if _, err := ApplyLightReplace(mf, nil, cx.DBPath, cx.ThumbnailsPath, gallery.NewBoundary(cx.GalleryPath, nil, nil), ImportSourceNative, 0, noQuiesce); err != nil {
		t.Fatalf("ApplyLightReplace: %v", err)
	}

	if _, err := os.Stat(page); !os.IsNotExist(err) {
		t.Errorf("the old library's cached page survived the replace, under an id the new one reuses (%v)", err)
	}
}

func importStrays(t *testing.T, cx gallery.Handle) []string {
	t.Helper()
	strays, err := filepath.Glob(filepath.Join(filepath.Dir(cx.DBPath), "*import-*"))
	if err != nil {
		t.Fatal(err)
	}
	return strays
}

func TestApplyLightReplace_AFailureBeforeTheInstallKeepsTheLiveGallery(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	if _, err := cx.DB.Write.Exec(`INSERT INTO tags (name, category_id, origin) VALUES ('kept', 1, 'user')`); err != nil {
		t.Fatal(err)
	}
	oldThumb := gallery.ThumbnailPath(cx.ThumbnailsPath, 1)
	mustWriteFile(t, oldThumb, []byte("old thumbnail"), 0o644)
	if err := cx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	plantWALDir := func() error { return os.MkdirAll(filepath.Join(cx.DBPath+"-wal", "x"), 0o755) }

	mf := LightManifest{Version: LightManifestVersion}
	if _, err := ApplyLightReplace(mf, nil, cx.DBPath, cx.ThumbnailsPath, gallery.NewBoundary(cx.GalleryPath, nil, nil), ImportSourceNative, 0, plantWALDir); err == nil {
		t.Fatal("the install reported success over a -wal it could not remove")
	}
	if err := os.RemoveAll(cx.DBPath + "-wal"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, reopen(t, cx.DBPath), "tags WHERE name = 'kept'"); n != 1 {
		t.Errorf("the live database lost its rows: kept = %d", n)
	}
	if _, err := os.Stat(oldThumb); err != nil {
		t.Errorf("the live thumbnails went with the failed install: %v", err)
	}
	if strays := importStrays(t, cx); len(strays) != 0 {
		t.Errorf("the failed build left %v behind", strays)
	}
}

func TestApplyLightReplace_InstallsTheThumbnailsItBuilt(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	body := makePNGBytes(t, 8, 8, 5, 6, 7)
	mustWriteFile(t, filepath.Join(cx.GalleryPath, "kept.png"), body, 0o644)
	stale := gallery.ThumbnailPath(cx.ThumbnailsPath, 99)
	mustWriteFile(t, stale, []byte("old thumbnail"), 0o644)
	if err := cx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	mf := LightManifest{Version: LightManifestVersion, Images: []LightManifestImage{{SHA256: sha256OfBytes(t, body), Path: "kept.png"}}}
	if _, err := ApplyLightReplace(mf, nil, cx.DBPath, cx.ThumbnailsPath, gallery.NewBoundary(cx.GalleryPath, nil, nil), ImportSourceNative, 0, noQuiesce); err != nil {
		t.Fatalf("ApplyLightReplace: %v", err)
	}
	var id int64
	if err := reopen(t, cx.DBPath).Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%kept.png'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gallery.ThumbnailPath(cx.ThumbnailsPath, id)); err != nil {
		t.Errorf("the rebuilt image has no thumbnail: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the old library's thumbnail survived the replace (%v)", err)
	}
	if strays := importStrays(t, cx); len(strays) != 0 {
		t.Errorf("the replace left %v behind", strays)
	}
}

func TestApplyLightReplace_SkipsAMissingEntryWhoseTypeCannotBeTold(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	if err := cx.DB.Close(); err != nil {
		t.Fatal(err)
	}
	mf := LightManifest{Version: LightManifestVersion, Images: []LightManifestImage{
		{SHA256: strings.Repeat("a", 64), Path: "gone.png", Tags: []string{"kept"}},
		{SHA256: strings.Repeat("b", 64), Path: "gone.jfif", Tags: []string{"lost"}},
	}}
	if _, err := ApplyLightReplace(mf, nil, cx.DBPath, cx.ThumbnailsPath, gallery.NewBoundary(cx.GalleryPath, nil, nil), ImportSourceNative, 0, noQuiesce); err != nil {
		t.Fatalf("ApplyLightReplace: %v", err)
	}
	database, err := db.Open(cx.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if n := countRows(t, database, "images WHERE is_missing = 1 AND canonical_path LIKE '%gone.png'"); n != 1 {
		t.Errorf("missing rows for gone.png = %d, want 1", n)
	}
	if n := countRows(t, database, "images WHERE canonical_path LIKE '%gone.jfif'"); n != 0 {
		t.Errorf("rows for gone.jfif = %d, want the entry skipped", n)
	}
}

func TestExportGalleryDB_ProducesValidSQLite(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)

	buf := mustExportDB(t, cx)

	if !bytes.HasPrefix(buf.Bytes(), []byte("SQLite format 3")) {
		t.Errorf("exported DB missing SQLite magic prefix")
	}
	if buf.Len() < 1024 {
		t.Errorf("exported DB suspiciously small: %d bytes", buf.Len())
	}
}

func TestExportGalleryDB_RunsWhileAWriteIsOpen(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)
	tx, err := cx.DB.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE images SET note = 'uncommitted'`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- ExportGalleryDB(cx, &buf) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the export waited for the open write")
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("SQLite format 3")) {
		t.Error("the export is not a SQLite file")
	}
}

func TestExportGalleryJSON_RoundTripsAliases(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)

	buf := mustExportJSON(t, cx)
	var exp Export
	if err := json.Unmarshal(buf.Bytes(), &exp); err != nil {
		t.Fatalf("unmarshal: %v\nraw:\n%s", err, buf.String())
	}
	if exp.Version != ExportVersion {
		t.Errorf("version = %d, want %d", exp.Version, ExportVersion)
	}
	if len(exp.Images) != 1 {
		t.Errorf("images = %d, want 1", len(exp.Images))
	}

	foundAlias := false
	for _, tag := range exp.Tags {
		if tag.Name == "cat" && tag.IsAlias == 1 && tag.CanonicalTagID.Valid {
			foundAlias = true
		}
	}
	if !foundAlias {
		t.Errorf("alias tag not preserved in JSON export, got:\n%s", buf.String())
	}
}

func TestExportGalleryJSON_IsOneSnapshot(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)
	var imgID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images`).Scan(&imgID); err != nil {
		t.Fatal(err)
	}

	w := &hookWriter{at: []byte(`"images":[`), hook: func() {
		late := mustTag(t, cx.TagSvc, "late_tag", generalCategoryID(t, cx))
		if _, err := cx.DB.Write.Exec(`INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, imgID, late.ID); err != nil {
			t.Fatal(err)
		}
	}}

	if err := ExportGalleryJSON(cx, w, nil); err != nil {
		t.Fatal(err)
	}

	if !w.fired {
		t.Fatal("the hook never ran")
	}
	if err := loadExportStream(openIOTestDB(t), &w.buf); err != nil {
		t.Errorf("the export does not load: %v", err)
	}
}

type hookWriter struct {
	buf   bytes.Buffer
	at    []byte
	hook  func()
	fired bool
}

func (w *hookWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if !w.fired && bytes.HasSuffix(w.buf.Bytes(), w.at) {
		w.fired = true
		w.hook()
	}
	return n, err
}

func TestExportGalleryJSON_RoundTripsTagNotes(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)
	tag := mustTag(t, cx.TagSvc, "noted", generalCategoryID(t, cx))
	want := tags.TagNote{Body: "a [b]note[/b]", Links: []string{"https://a.example/1", "-https://b.example/2"}}
	if err := cx.TagSvc.SetTagNote(tag.ID, want.Body, want.Links); err != nil {
		t.Fatal(err)
	}

	restored := openIOTestDB(t)
	if err := loadExportStream(restored, mustExportJSON(t, cx)); err != nil {
		t.Fatal(err)
	}
	got, err := tags.New(restored, nil).TagNote(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("restored note = %+v, want %+v", got, want)
	}
}

func TestExportGalleryJSON_RoundTripsDistinctMarks(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)
	a := mustTag(t, cx.TagSvc, "kirin", generalCategoryID(t, cx))
	species, ok, err := tags.CategoryIDByName(cx.DB, "species")
	if err != nil || !ok {
		t.Fatalf("species category: %v", err)
	}
	b := mustTag(t, cx.TagSvc, "kirin", species)
	if _, _, err := cx.TagSvc.MarkDistinct([]int64{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}

	restored := openIOTestDB(t)
	if err := loadExportStream(restored, mustExportJSON(t, cx)); err != nil {
		t.Fatal(err)
	}
	got, err := tags.New(restored, nil).DistinctPartners([]int64{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []tags.DistinctPartner{{ID: b.ID, Category: "species"}}; !reflect.DeepEqual(got[a.ID], want) {
		t.Errorf("restored partners = %+v, want %+v", got[a.ID], want)
	}
}

func TestExportArchive_ContainsGalleryFiles(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)

	buf := mustExportArchive(t, cx, "db")
	zr := readZip(t, buf)
	seenDB := false
	seenFile := false
	for _, f := range zr.File {
		switch f.Name {
		case "monbooru.db":
			seenDB = true
		case "gallery/hello.txt":
			seenFile = true
		}
	}
	if !seenDB || !seenFile {
		t.Errorf("archive missing entries: db=%v file=%v (%d entries)", seenDB, seenFile, len(zr.File))
	}
}

func TestExportArchive_SkipsWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permissions")
	}
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)
	lost := filepath.Join(cx.GalleryPath, "lost+found")
	mustMkdirAll(t, lost, 0o755)
	mustWriteFile(t, filepath.Join(lost, "inside.png"), []byte("x"), 0o644)
	locked := filepath.Join(cx.GalleryPath, "locked.png")
	mustWriteFile(t, locked, []byte("y"), 0o644)
	for _, p := range []string{lost, locked} {
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}

	for _, format := range []string{"db", "light"} {
		var buf bytes.Buffer
		export := func() error { return ExportGalleryArchive(cx, "db", &buf) }
		if format == "light" {
			export = func() error { return ExportGalleryLight(cx, &buf) }
		}
		if err := export(); err != nil {
			t.Fatalf("%s: export: %v", format, err)
		}
		zr := readZip(t, &buf)
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		if !slices.Contains(names, "gallery/hello.txt") || slices.ContainsFunc(names, func(n string) bool {
			return strings.Contains(n, "lost+found") || strings.Contains(n, "locked.png")
		}) {
			t.Errorf("%s: entries = %v, want the readable file and nothing it could not read", format, names)
		}
	}
}

func TestExportArchive_ContainsFilesBehindALink(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	seedExportFixture(t, cx)
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	mustWriteFile(t, filepath.Join(elsewhere, "linked.txt"), []byte("bye"), 0o644)
	if err := os.Symlink(elsewhere, filepath.Join(cx.GalleryPath, "outside")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	buf := mustExportArchive(t, cx, "db")
	zr := readZip(t, buf)
	for _, f := range zr.File {
		if f.Name == "gallery/outside/linked.txt" {
			return
		}
	}
	t.Errorf("archive is missing gallery/outside/linked.txt (%d entries)", len(zr.File))
}

func TestSafeArchiveDest(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		rel     string
		wantErr bool
	}{
		{"a.png", false},
		{"sub/a.png", false},
		{"deeper/nested/photo.webp", false},
		{"foo..bar.png", false},
		{"a/b/c.png", false},
		{"/etc/passwd", true},
		{"../escape.txt", true},
		{"a/../../escape.png", true},
		{"a/../b/../../escape.png", true},
	}
	for _, tc := range cases {
		dst, err := SafeArchiveDest(root, tc.rel)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("SafeArchiveDest(root, %q) err=%v want err=%v (dst=%q)",
				tc.rel, err, tc.wantErr, dst)
			continue
		}
		if !tc.wantErr {
			rootAbs, _ := filepath.Abs(root)
			if !strings.HasPrefix(dst, rootAbs) {
				t.Errorf("SafeArchiveDest(root, %q) returned %q, expected under %q",
					tc.rel, dst, rootAbs)
			}
		}
	}
}

func TestExportGalleryArchive_LeavesANestedGallerysFilesOut(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	inner := filepath.Join(cx.GalleryPath, ".H")
	mustMkdirAll(t, inner, 0o755)
	mustWriteFile(t, filepath.Join(cx.GalleryPath, "top.png"), makePNGBytes(t, 8, 8, 1, 2, 3), 0o644)
	mustWriteFile(t, filepath.Join(inner, "in.png"), makePNGBytes(t, 8, 8, 4, 5, 6), 0o644)
	cx.Bounds = new(atomic.Pointer[gallery.Boundary])
	cx.Bounds.Store(gallery.NewBoundary(cx.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))

	buf := mustExportArchive(t, cx, "db")
	zr := readZip(t, buf)
	var names []string
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "gallery/") {
			names = append(names, f.Name)
		}
	}
	if want := []string{"gallery/top.png"}; !slices.Equal(names, want) {
		t.Errorf("archive carries %v, want %v", names, want)
	}
}
