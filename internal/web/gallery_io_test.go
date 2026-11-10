package web

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
)

func mustExport(t *testing.T, srv *Server, name, format string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	write := map[string]func(*library.Gallery) error{
		"db":    func(cx *library.Gallery) error { return galleryio.ExportGalleryDB(cx.Handle, &buf) },
		"json":  func(cx *library.Gallery) error { return galleryio.ExportGalleryJSON(cx.Handle, &buf, nil) },
		"zip":   func(cx *library.Gallery) error { return galleryio.ExportGalleryArchive(cx.Handle, "db", &buf) },
		"light": func(cx *library.Gallery) error { return galleryio.ExportGalleryLight(cx.Handle, &buf) },
	}[format]
	if write == nil {
		t.Fatalf("no export format %q", format)
	}
	if err := srv.exportGallery(name, write); err != nil {
		t.Fatalf("export %s %s: %v", format, name, err)
	}
	return &buf
}

func postMultipart(t *testing.T, srv *Server, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func seedImportExportFixture(t *testing.T, srv *Server) {
	t.Helper()
	cx := srv.get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at) VALUES ('seed-sha', 'seed.png', 'png', 10, datetime('now'))`)

	alias, err := cx.TagSvc.GetOrCreateTag("cat", 1)
	if err != nil {
		t.Fatalf("create alias tag: %v", err)
	}
	canon, err := cx.TagSvc.GetOrCreateTag("feline", 1)
	if err != nil {
		t.Fatalf("create canon tag: %v", err)
	}
	if err := cx.TagSvc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var imgID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatalf("query seed image: %v", err)
	}
	mustAddTag(t, cx.TagSvc, imgID, canon.ID, false, nil)

	mustWriteFile(t, filepath.Join(cx.GalleryPath, "hello.txt"), []byte("hi"), 0o644)
}

func TestImportGalleryDB_RestoresRows(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "db")

	mustImport(t, srv, "stock", "db", bytes.NewReader(buf.Bytes()))

	cx := srv.get("stock")
	n := countRows(t, cx.DB, "images")
	if n != 1 {
		t.Errorf("images after import = %d, want 1", n)
	}
	aliasCount := countRows(t, cx.DB, "tags WHERE is_alias = 1")
	if aliasCount != 1 {
		t.Errorf("aliases after import = %d, want 1", aliasCount)
	}
}

func TestImportGalleryDB_NormalizesAliasChains(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	cx := srv.get("stock")

	insertTag := func(name string, isAlias int, canonical any) int64 {
		t.Helper()
		var id int64
		if err := cx.DB.Write.QueryRow(
			`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin)
			 VALUES (?, 1, ?, ?, 0, 'user') RETURNING id`,
			name, isAlias, canonical,
		).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	repoint := func(id, canonical int64) {
		t.Helper()
		mustExec(t, cx.DB.Write, `UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, canonical, id)
	}

	plainID := insertTag("norm_plain", 0, nil)
	midID := insertTag("norm_mid", 1, plainID)
	leafID := insertTag("norm_leaf", 1, midID)
	cycleA := insertTag("norm_cycle_a", 1, nil)
	cycleB := insertTag("norm_cycle_b", 1, cycleA)
	repoint(cycleA, cycleB)
	strayID := insertTag("norm_stray", 0, nil)
	repoint(strayID, plainID)

	buf := mustExport(t, srv, "stock", "db")
	mustImport(t, srv, "stock", "db", bytes.NewReader(buf.Bytes()))

	cx = srv.get("stock")
	shape := func(id int64) (int, int64) {
		t.Helper()
		var alias int
		var canonical int64
		if err := cx.DB.Read.QueryRow(
			`SELECT is_alias, COALESCE(canonical_tag_id, 0) FROM tags WHERE id = ?`, id,
		).Scan(&alias, &canonical); err != nil {
			t.Fatal(err)
		}
		return alias, canonical
	}
	if a, c := shape(leafID); a != 1 || c != plainID {
		t.Errorf("leaf = (alias=%d, canonical=%d), want re-pointed at plain %d", a, c, plainID)
	}
	if a, c := shape(midID); a != 1 || c != plainID {
		t.Errorf("mid = (alias=%d, canonical=%d), want %d unchanged", a, c, plainID)
	}

	if a, c := shape(cycleA); a != 0 || c != 0 {
		t.Errorf("cycle_a = (alias=%d, canonical=%d), want promoted to plain", a, c)
	}
	if a, c := shape(cycleB); a != 1 || c != cycleA {
		t.Errorf("cycle_b = (alias=%d, canonical=%d), want re-pointed at %d", a, c, cycleA)
	}
	if a, c := shape(strayID); a != 0 || c != 0 {
		t.Errorf("stray = (alias=%d, canonical=%d), want the pointer cleared", a, c)
	}
}

func TestImportGalleryJSON_RestoresRows(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx := srv.get("stock")
	imgCount := countRows(t, cx.DB, "images")
	aliasCount := countRows(t, cx.DB, "tags WHERE is_alias = 1")
	imageTagCount := countRows(t, cx.DB, "image_tags")
	if imgCount != 1 || aliasCount != 1 || imageTagCount != 1 {
		t.Errorf("after JSON import: images=%d aliases=%d image_tags=%d, want 1/1/1",
			imgCount, aliasCount, imageTagCount)
	}
}

func TestImportGalleryJSON_RoundTripsProvenance(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	cx := srv.get("stock")
	var imgID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO image_sources (image_id, site, post_id, url, md5, commentary, parent_url, upgrade_kept, post_width, post_height, post_size, post_ext)
		  VALUES (?, 'danbooru', '42', 'https://x/1', 'abc123', 'artist says hi', 'https://x/parent', 1, 1920, 1080, 4096, 'png')`, []any{imgID}},
		{`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'pixiv', '', 'https://x/2')`, []any{imgID}},
		{`UPDATE images SET md5 = 'deadbeefdeadbeefdeadbeefdeadbeef' WHERE id = ?`, []any{imgID}},
		{`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body) VALUES (?, 'danbooru', '42', 1, 2, 3, 4, 'a note box')`, []any{imgID}},
		{`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual) VALUES (?, '', '', 5, 6, 7, 8, 'operator box', 1)`, []any{imgID}},
		{`INSERT INTO image_collections (image_id, name, position) VALUES (?, 'extras', 7)`, []any{imgID}},
		{`UPDATE images SET source = 'danbooru', url = 'https://x/1', note = 'operator note' WHERE id = ?`, []any{imgID}},
	} {
		mustExec(t, cx.DB.Write, stmt.sql, stmt.args...)
	}

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx = srv.get("stock")
	var site, postID, commentary, md5, note, primary string
	if err := cx.DB.Read.QueryRow(
		`SELECT site, post_id, commentary, md5 FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imgID,
	).Scan(&site, &postID, &commentary, &md5); err != nil {
		t.Fatalf("source row: %v", err)
	}
	if site != "danbooru" || postID != "42" || commentary != "artist says hi" || md5 != "abc123" {
		t.Errorf("primary source = %q/%q/%q/%q, want seeded values", site, postID, commentary, md5)
	}
	var imageMD5 string
	if err := cx.DB.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, imgID).Scan(&imageMD5); err != nil {
		t.Fatal(err)
	}
	if imageMD5 != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("images.md5 = %q, want the seeded digest", imageMD5)
	}
	var parentURL, postExt string
	var upgradeKept int
	var postW, postH, postSize int64
	if err := cx.DB.Read.QueryRow(
		`SELECT parent_url, upgrade_kept, post_width, post_height, post_size, post_ext
		 FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, imgID,
	).Scan(&parentURL, &upgradeKept, &postW, &postH, &postSize, &postExt); err != nil {
		t.Fatal(err)
	}
	if parentURL != "https://x/parent" || upgradeKept != 1 || postW != 1920 || postH != 1080 || postSize != 4096 || postExt != "png" {
		t.Errorf("origin facts = %q/%d/%d/%d/%d/%q, want the seeded values",
			parentURL, upgradeKept, postW, postH, postSize, postExt)
	}
	srcCount := countRows(t, cx.DB, "image_sources WHERE image_id = ?", imgID)
	annCount := countRows(t, cx.DB, "image_annotations WHERE image_id = ?", imgID)
	collCount := countRows(t, cx.DB, "image_collections WHERE image_id = ? AND name = 'extras'", imgID)
	if srcCount != 2 || annCount != 2 || collCount != 1 {
		t.Errorf("after import: sources=%d annotations=%d extra-collections=%d, want 2/2/1", srcCount, annCount, collCount)
	}
	manualCount := countRows(t, cx.DB, "image_annotations WHERE image_id = ? AND manual = 1", imgID)
	if manualCount != 1 {
		t.Errorf("manual annotation flag did not round-trip: manual=1 count = %d, want 1", manualCount)
	}
	if err := cx.DB.Read.QueryRow(`SELECT note, source FROM images WHERE id = ?`, imgID).Scan(&note, &primary); err != nil {
		t.Fatal(err)
	}
	if note != "operator note" {
		t.Errorf("note = %q, want %q", note, "operator note")
	}
	if primary != "danbooru" {
		t.Errorf("scalar source mirror = %q, want danbooru (oldest row)", primary)
	}
}

type gateReader struct {
	r      io.Reader
	once   sync.Once
	opened chan struct{}
	wait   chan struct{}
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.opened)
		<-g.wait
	})
	return g.r.Read(p)
}

func TestLeftOutNote_AgreesWithItsCount(t *testing.T) {
	for n, want := range map[int]string{
		0: "",
		1: " 1 file in the archive sits in a folder this gallery leaves out and was not extracted.",
		3: " 3 files in the archive sit in folders this gallery leaves out and were not extracted.",
	} {
		if got := leftOutNote(n); got != want {
			t.Errorf("leftOutNote(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestMergeGallery_ShowsAsAJob(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedMergeTarget(t, srv, "stock")
	body := mustMarshal(t, galleryio.LightManifest{Version: galleryio.LightManifestVersion})
	gate := &gateReader{r: bytes.NewReader(body), opened: make(chan struct{}), wait: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := srv.mergeGallery("stock", "json", gate)
		done <- err
	}()

	<-gate.opened
	running := srv.jobs.Get()
	close(gate.wait)
	if err := <-done; err != nil {
		t.Fatalf("mergeGallery: %v", err)
	}

	if running == nil || !running.Running || running.JobType != models.JobTypeMerge || !slices.Equal(running.Galleries, []string{"stock"}) {
		t.Errorf("job while merging = %+v, want a running merge naming stock", running)
	}
	if after := srv.jobs.Get(); after == nil || after.Running || !strings.Contains(after.Summary, "merged") {
		t.Errorf("job after the merge = %+v, want it complete with the summary", after)
	}
}

func TestMergeGallery_CancelStopsBeforeTheNextRecord(t *testing.T) {
	srv := newMultiGalleryServer(t)
	_, sha, _ := seedMergeTarget(t, srv, "stock")
	body := mustMarshal(t, galleryio.LightManifest{
		Version: galleryio.LightManifestVersion,
		Images:  []galleryio.LightManifestImage{{SHA256: sha, Tags: []string{"never_applied"}}},
	})
	gate := &gateReader{r: bytes.NewReader(body), opened: make(chan struct{}), wait: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := srv.mergeGallery("stock", "json", gate)
		done <- err
	}()

	<-gate.opened
	srv.jobs.Cancel()
	close(gate.wait)
	err := <-done

	if err == nil || !strings.Contains(err.Error(), "merge cancelled after 0 of 1") {
		t.Errorf("err = %v, want the cancelled merge's count", err)
	}
	if st := srv.jobs.Get(); st == nil || st.Running || st.Summary != "merge cancelled (0/1 records)" {
		t.Errorf("job = %+v, want the cancelled summary", st)
	}
	if n := countRows(t, srv.get("stock").DB, "tags WHERE name = 'never_applied'"); n != 0 {
		t.Error("a cancelled merge applied its record")
	}
}

func TestImportGallery_DoesNotHoldTheContextLock(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "json")
	gate := &gateReader{r: bytes.NewReader(buf.Bytes()), opened: make(chan struct{}), wait: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := srv.importGallery("stock", "json", gate); done <- err }()

	<-gate.opened
	locked := srv.set.TryRLock()
	if locked {
		srv.set.RUnlock()
	}
	close(gate.wait)
	if err := <-done; err != nil {
		t.Fatalf("import: %v", err)
	}
	if !locked {
		t.Error("the import held the set's lock across the upload copy")
	}
}

func TestGalleriesOpenedAtRuntimeReclaimTheirPageCache(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	added := filepath.Join(t.TempDir(), "added")
	mustMkdirAll(t, added, 0o755)
	if err := srv.addGallery("added", added); err != nil {
		t.Fatalf("AddGallery: %v", err)
	}
	if !srv.get("added").ReclaimerRunning() {
		t.Error("a gallery added at runtime runs no page-cache reclaimer")
	}

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))
	if !srv.get("stock").ReclaimerRunning() {
		t.Error("a gallery reopened by an import runs no page-cache reclaimer")
	}
}

func TestImportGalleryJSON_RoundTripsTagSourceLedger(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	cx := srv.get("stock")
	var imgID, tagID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, "feline").Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, cx.DB.Write, `INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source) VALUES (?, ?, 'danbooru')`, imgID, tagID)

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx = srv.get("stock")
	sources, err := db.QueryStrings(cx.DB.Read,
		`SELECT source FROM image_tag_sources WHERE image_id = ? AND tag_id = ? ORDER BY source`, imgID, tagID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0] != "danbooru" || sources[1] != "user" {
		t.Errorf("ledger after import = %v, want [danbooru user]", sources)
	}
}

func TestImportGalleryJSON_RoundTripsRelations(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	cx := srv.get("stock")

	var a int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = 'seed-sha'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	newImg := func(name string) int64 {
		id, _ := mustExec(t, cx.DB.Write,
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
			 VALUES (?, ?, 'png', 10, datetime('now'))`, name+"-sha", name+".png").LastInsertId()
		return id
	}

	b, c, d, e, f, g := newImg("b"), newImg("c"), newImg("d"), newImg("e"), newImg("f"), newImg("g")

	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO dup_groups (id, original_image_id) VALUES (1, ?)`, []any{a}},
		{`INSERT INTO dup_group_members (image_id, group_id) VALUES (?, 1), (?, 1)`, []any{a, b}},
		{`INSERT INTO alt_groups (id) VALUES (1)`, nil},
		{`INSERT INTO alt_group_members (image_id, group_id) VALUES (?, 1), (?, 1)`, []any{c, d}},
		{`INSERT INTO version_edges (child_image_id, parent_image_id) VALUES (?, ?)`, []any{e, f}},
		{`INSERT INTO derivative_edges (derivative_image_id, source_image_id) VALUES (?, ?), (?, ?)`, []any{g, a, g, f}},
		{`INSERT INTO not_related_pairs (a_image_id, b_image_id) VALUES (?, ?)`, []any{b, c}},
	} {
		mustExec(t, cx.DB.Write, s.sql, s.args...)
	}

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx = srv.get("stock")
	for table, want := range map[string]int{
		"dup_groups": 1, "dup_group_members": 2, "alt_groups": 1, "alt_group_members": 2,
		"version_edges": 1, "derivative_edges": 2, "not_related_pairs": 1,
	} {
		if got := countRows(t, cx.DB, table); got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}

	var child, parent int64
	if err := cx.DB.Read.QueryRow(`SELECT child_image_id, parent_image_id FROM version_edges`).Scan(&child, &parent); err != nil {
		t.Fatal(err)
	}
	if child != e || parent != f {
		t.Errorf("version edge = %d->%d, want %d->%d", child, parent, e, f)
	}
}

func TestImportGalleryJSON_SeedsCollectionsFromSeriesOnOldExports(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	cx := srv.get("stock")
	mustExec(t, cx.DB.Write, `UPDATE images SET series = 'oldbook', series_order = 3 WHERE sha256 = 'seed-sha'`)
	buf := mustExport(t, srv, "stock", "json")
	var exp galleryio.Export
	mustUnmarshal(t, buf.Bytes(), &exp)
	exp.Version = 2
	exp.ImageCollections = nil
	old := mustMarshal(t, exp)
	mustImport(t, srv, "stock", "json", bytes.NewReader(old))
	cx = srv.get("stock")
	var name string
	var pos int
	if err := cx.DB.Read.QueryRow(
		`SELECT name, position FROM image_collections WHERE name = 'oldbook'`,
	).Scan(&name, &pos); err != nil {
		t.Fatalf("derived membership: %v", err)
	}
	if pos != 3 {
		t.Errorf("derived position = %d, want 3", pos)
	}
}

func TestImportGalleryJSON_DerivesTagSourcesOnOldExports(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "json")
	var exp galleryio.Export
	mustUnmarshal(t, buf.Bytes(), &exp)
	exp.Version = 10
	exp.ImageTagSources = nil
	old := mustMarshal(t, exp)
	mustImport(t, srv, "stock", "json", bytes.NewReader(old))

	cx := srv.get("stock")
	n := countRows(t, cx.DB, "image_tag_sources WHERE source = 'user'")
	if n != 1 {
		t.Errorf("derived ledger rows = %d, want 1", n)
	}
}

func TestImportGalleryDB_FailedLoadLeavesTargetIntact(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	foreignPath := filepath.Join(t.TempDir(), "foreign.db")
	foreign, err := db.Open(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"tag_categories", "tags", "images", "image_tags"} {
		mustExec(t, foreign.Write, "CREATE TABLE "+tbl+" (x)")
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	upload, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := srv.importGallery("stock", "db", bytes.NewReader(upload)); err == nil {
		t.Fatal("import of a foreign database succeeded, want error")
	}

	cx := srv.get("stock")
	imgCount := countRows(t, cx.DB, "images")
	tagCount := countRows(t, cx.DB, "image_tags")
	if imgCount != 1 || tagCount != 1 {
		t.Errorf("after failed import: images=%d image_tags=%d, want the original 1/1", imgCount, tagCount)
	}
}

func TestImportGalleryJSON_FailedLoadLeavesTargetIntact(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "json")

	var exp galleryio.Export
	mustUnmarshal(t, buf.Bytes(), &exp)
	row := exp.ImageTags[0]
	row.TagID = 999999
	exp.ImageTags = append(exp.ImageTags, row)
	bad := mustMarshal(t, exp)

	if _, err := srv.importGallery("stock", "json", bytes.NewReader(bad)); err == nil {
		t.Fatal("import of a bad export succeeded, want error")
	}

	cx := srv.get("stock")
	imgCount := countRows(t, cx.DB, "images")
	tagCount := countRows(t, cx.DB, "image_tags")
	if imgCount != 1 || tagCount != 1 {
		t.Errorf("after failed import: images=%d image_tags=%d, want the original 1/1", imgCount, tagCount)
	}
}

func TestImportGalleryArchive_RestoresImagesAndDB(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "zip")

	cx := srv.get("stock")
	target := filepath.Join(cx.GalleryPath, "hello.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	mustImport(t, srv, "stock", "zip", bytes.NewReader(buf.Bytes()))
	if _, err := os.Stat(target); err != nil {
		t.Errorf("gallery file not restored: %v", err)
	}
}

func TestImportGalleryArchive_KeepsTheDatabaseOfAGalleryOnItsDataDir(t *testing.T) {
	srv, _ := newServerWithGalleryUnderData(t, "pics")
	for _, name := range []string{"default", "pics"} {
		cx := srv.get(name)
		mustWriteFile(t, filepath.Join(cx.GalleryPath, name+".png"), []byte(name), 0o644)
		mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES (?, ?, '', 'png', 5, datetime('now'))`, name+"-sha", filepath.Join(cx.GalleryPath, name+".png"))
	}

	own := mustExport(t, srv, "pics", "zip")
	zr, err := zip.NewReader(bytes.NewReader(own.Bytes()), int64(own.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "gallery/monbooru.db") {
			t.Errorf("the archive ships the live database file %s as a gallery file", f.Name)
		}
	}

	mustImport(t, srv, "pics", "zip", bytes.NewReader(mustExport(t, srv, "default", "zip").Bytes()))
	var n int
	if err := srv.get("pics").DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = 'default-sha'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("the imported database lost its row: count=%d err=%v", n, err)
	}
}

func TestImportGalleryArchive_ClearsMissingAfterExtract(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.get("stock")

	mustWriteFile(t, filepath.Join(cx.GalleryPath, "pic.png"), []byte("bytes"), 0o644)
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('pic-sha', ?, '', 'png', 5, datetime('now'))`, filepath.Join(cx.GalleryPath, "pic.png"))

	buf := mustExport(t, srv, "stock", "zip")

	if err := os.Remove(filepath.Join(cx.GalleryPath, "pic.png")); err != nil {
		t.Fatal(err)
	}

	mustImport(t, srv, "stock", "zip", bytes.NewReader(buf.Bytes()))
	cx = srv.get("stock")
	var missing int
	if err := cx.DB.Read.QueryRow(`SELECT is_missing FROM images WHERE sha256 = 'pic-sha'`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("is_missing = %d after archive import, want 0", missing)
	}
}

func TestImportGallery_RebasesPathsToTargetGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)

	cx := srv.get("stock")
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('sha1', '/foreign/gallery/2024/foo.png', '2024', 'png', 10, datetime('now'))`)
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES ((SELECT id FROM images WHERE sha256 = 'sha1'), '/foreign/gallery/2024/foo.png', 1)`)

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))
	cx = srv.get("stock")
	wantPrefix := cx.GalleryPath
	var got string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE sha256 = 'sha1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := wantPrefix + "/2024/foo.png"
	if got != want {
		t.Errorf("canonical_path = %q, want %q", got, want)
	}
	var gotAlias string
	if err := cx.DB.Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = (SELECT id FROM images WHERE sha256 = 'sha1')`,
	).Scan(&gotAlias); err != nil {
		t.Fatal(err)
	}
	if gotAlias != want {
		t.Errorf("image_paths.path = %q, want %q", gotAlias, want)
	}
}

func TestImportGallery_RebasesWindowsPaths(t *testing.T) {
	srv := newMultiGalleryServer(t)

	cx := srv.get("stock")
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('sha-win', 'D:\pics\2024\foo.png', '2024', 'png', 10, datetime('now'))`)
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES ((SELECT id FROM images WHERE sha256 = 'sha-win'), 'D:\pics\2024\foo.png', 1)`)

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))
	cx = srv.get("stock")
	want := cx.GalleryPath + "/2024/foo.png"
	var got string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE sha256 = 'sha-win'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("canonical_path = %q, want %q", got, want)
	}
}

func TestImportGallery_PreservesAliasFolderOnRebase(t *testing.T) {
	srv := newMultiGalleryServer(t)

	cx := srv.get("stock")
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('sha-alias', '/source/gallery/portraits/cat.jpg', 'portraits', 'jpeg', 10, datetime('now'))`)
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES ((SELECT id FROM images WHERE sha256 = 'sha-alias'), '/source/gallery/portraits/cat.jpg', 1)`)
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES ((SELECT id FROM images WHERE sha256 = 'sha-alias'), '/source/gallery/photos/cat.jpg', 0)`)

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))
	cx = srv.get("stock")
	prefix := cx.GalleryPath

	var aliasPath string
	if err := cx.DB.Read.QueryRow(
		`SELECT path FROM image_paths
		 WHERE image_id = (SELECT id FROM images WHERE sha256 = 'sha-alias')
		   AND is_canonical = 0`,
	).Scan(&aliasPath); err != nil {
		t.Fatalf("read alias path: %v", err)
	}
	wantAlias := prefix + "/photos/cat.jpg"
	if aliasPath != wantAlias {
		t.Errorf("alias path = %q, want %q (alias folder must survive rebase)", aliasPath, wantAlias)
	}
}

func TestImportGallery_PreservesWindowsAliasFolderOnRebase(t *testing.T) {
	srv := newMultiGalleryServer(t)

	cx := srv.get("stock")
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('sha-win-alias', 'D:\pics\portraits\cat.jpg', 'portraits', 'jpeg', 10, datetime('now'))`)
	for _, p := range []struct {
		path      string
		canonical int
	}{
		{`D:\pics\portraits\cat.jpg`, 1},
		{`D:\pics\photos\cat.jpg`, 0},
		{`D:\pics\photos\kitty.jpg`, 0},
	} {
		mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES ((SELECT id FROM images WHERE sha256 = 'sha-win-alias'), ?, ?)`, p.path, p.canonical)
	}

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))
	cx = srv.get("stock")

	got, err := db.QueryStrings(cx.DB.Read,
		`SELECT path FROM image_paths
		 WHERE image_id = (SELECT id FROM images WHERE sha256 = 'sha-win-alias') AND is_canonical = 0
		 ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{cx.GalleryPath + "/photos/cat.jpg", cx.GalleryPath + "/photos/kitty.jpg"}
	if !slices.Equal(got, want) {
		t.Errorf("alias paths = %v, want %v", got, want)
	}
}

func TestImportGallery_QueuesRebuildThumbsJob(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "db")
	mustImport(t, srv, "stock", "db", bytes.NewReader(buf.Bytes()))

	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("no job state after import; expected rebuild-thumbs")
	}
	if state.JobType != "rebuild-thumbs" {
		t.Errorf("job type = %q, want rebuild-thumbs", state.JobType)
	}
}

func TestImportGallery_ActivatesImportedGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	if srv.activeGallery() != "default" {
		t.Fatalf("pre-import active gallery = %q, want default", srv.activeGallery())
	}

	buf := mustExport(t, srv, "stock", "db")
	mustImport(t, srv, "stock", "db", bytes.NewReader(buf.Bytes()))
	if srv.activeGallery() != "stock" {
		t.Errorf("post-import active gallery = %q, want stock", srv.activeGallery())
	}
}

func TestImportGallery_FlagsMissingFilesAfterRebase(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.get("stock")

	pngBytes := makePNGBytes(t, 8, 8, 1, 2, 3)
	presentPath := filepath.Join(cx.GalleryPath, "present.png")
	mustWriteFile(t, presentPath, pngBytes, 0o644)
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('present', '/foreign/gallery/present.png', '', 'png', 10, datetime('now'))`)
	mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at) VALUES ('gone', '/foreign/gallery/gone.png', '', 'png', 10, datetime('now'))`)
	for _, sha := range []string{"present", "gone"} {
		mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES ((SELECT id FROM images WHERE sha256 = ?), (SELECT canonical_path FROM images WHERE sha256 = ?), 1)`, sha, sha)
	}

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx2 := srv.get("stock")
	rows := map[string]int{}
	r, err := cx2.DB.Read.Query(`SELECT sha256, is_missing FROM images`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		var sha string
		var miss int
		if err := r.Scan(&sha, &miss); err != nil {
			t.Fatal(err)
		}
		rows[sha] = miss
	}
	if rows["present"] != 0 {
		t.Errorf("present row is_missing = %d, want 0", rows["present"])
	}
	if rows["gone"] != 1 {
		t.Errorf("gone row is_missing = %d, want 1", rows["gone"])
	}
}

func TestImportGallery_DropsTheOldLibrarysMatchIDs(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	q := search.Query{Cached: true, Text: "type:image", Sort: "newest", Order: "desc"}
	srv.get("stock").Source().Cache.Seed(q, []int64{900_001, 900_002})

	buf := mustExport(t, srv, "stock", "db")
	mustImport(t, srv, "stock", "db", bytes.NewReader(buf.Bytes()))
	if _, ok := search.PageOf(srv.get("stock").Source(), q, 900_001, 1); ok {
		t.Error("the pre-import match ids are still served")
	}
}

func TestImportGallery_RejectsActive(t *testing.T) {
	srv := newMultiGalleryServer(t)
	var buf bytes.Buffer

	_, err := srv.importGallery("default", "db", &buf)
	if err == nil || !strings.Contains(err.Error(), "active gallery") {
		t.Errorf("expected active-gallery error, got %v", err)
	}
}

func TestExportHandler_ServesDBDownload(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	w := do(srv, "GET", "/settings/galleries/stock/export?format=db")

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Header().Get("Content-Disposition"), `filename="stock.db"`)
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("SQLite format 3")) {
		t.Errorf("body missing SQLite magic prefix")
	}
}

func TestExportHandler_CutsOffAFailedExport(t *testing.T) {
	srv := newTestServer(t)

	mustExec(t, srv.get("default").DB.Write, `DROP TABLE image_lookups`)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/settings/galleries/default/export?format=json&with_images=true")
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("an export that failed partway ended as if whole")
	}
}

func TestImportHandler_RoundTripsStockGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.sessions.CSRFToken("anon"),
		"confirm_name": "stock",
	}, "file", "stock.db", mustExport(t, srv, "stock", "db").Bytes())
	w := postMultipart(t, srv, "/settings/galleries/stock/import", body, ct)

	mustCode(t, w, http.StatusOK)

	resp := w.Body.String()
	if !strings.Contains(resp, "flash-ok") || !strings.Contains(resp, "imported and now active") {
		t.Errorf("expected flash-ok import message, got %q", resp)
	}
}

func TestImportHandler_MergeKeepsTheActiveGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedMergeTarget(t, srv, "stock")

	body, ct := buildMultipart(t, map[string]string{
		"_csrf": srv.sessions.CSRFToken("anon"),
		"mode":  "merge",
	}, "file", "light.json", mustMarshal(t, galleryio.LightManifest{Version: galleryio.LightManifestVersion}))
	w := postMultipart(t, srv, "/settings/galleries/stock/import", body, ct)

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "merged")
	if got := srv.activeGallery(); got != "default" {
		t.Errorf("active gallery after a merge = %q, want default", got)
	}
}

func TestImportHandler_RejectsBadConfirmName(t *testing.T) {
	srv := newMultiGalleryServer(t)
	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.sessions.CSRFToken("anon"),
		"confirm_name": "wrong",
	}, "file", "stock.db", []byte("whatever"))
	w := postMultipart(t, srv, "/settings/galleries/stock/import", body, ct)

	wantIn(t, w.Body.String(), "confirm")
}

func TestSettingsRendersImportExportActions(t *testing.T) {
	srv := newMultiGalleryServer(t)

	w := do(srv, "GET", "/settings")

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	checks := []string{
		`<th>Actions</th>`,
		`btn-gallery-export`,
		`btn-gallery-import`,
		`id="gallery-export-dialog"`,
		`id="gallery-import-dialog"`,
	}
	for _, c := range checks {
		wantIn(t, body, c)
	}
}

func buildMultipart(t *testing.T, fields map[string]string, fileField, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mustWriteField(t, mw, k, v)
	}
	fw, err := mw.CreateFormFile(fileField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestAddGalleryWithAnImportFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		held     []string
		want     string
		added    bool
	}{
		{"an archive into a folder that holds files", "stock.zip", []string{"precious.png", "holiday/beach.jpg", ".keep"}, "is not empty", false},
		{"an archive into an empty folder", "stock.zip", nil, "added and imported", true},
		{"a file of another type", "stock.txt", nil, "must be .db, .json, or .zip", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMultiGalleryServer(t)
			seedImportExportFixture(t, srv)
			archive := mustExport(t, srv, "stock", "zip")
			dir := filepath.Join(t.TempDir(), "fresh")
			mustMkdirAll(t, dir, 0o755)
			for _, rel := range tc.held {
				p := filepath.Join(dir, rel)
				mustMkdirAll(t, filepath.Dir(p), 0o755)
				mustWriteFile(t, p, []byte(rel), 0o644)
			}

			body, ct := buildMultipart(t, map[string]string{
				"_csrf":        srv.sessions.CSRFToken("anon"),
				"name":         "fresh",
				"gallery_path": dir,
			}, "file", tc.filename, archive.Bytes())
			w := postMultipart(t, srv, "/settings/galleries", body, ct)

			wantIn(t, w.Body.String(), tc.want)
			for _, rel := range tc.held {
				if got, err := os.ReadFile(filepath.Join(dir, rel)); err != nil || string(got) != rel {
					t.Errorf("%s did not survive the refused add (%v)", rel, err)
				}
			}
			if added := srv.get("fresh") != nil; added != tc.added {
				t.Errorf("gallery added = %v, want %v", added, tc.added)
			}
			dataDir := filepath.Join(filepath.Dir(filepath.Dir(srv.get("stock").DBPath)), "fresh")
			if _, err := os.Stat(dataDir); (err == nil) != tc.added {
				t.Errorf("data folder present = %v, want %v", err == nil, tc.added)
			}
			if _, err := os.Stat(filepath.Join(dir, "hello.txt")); tc.added && err != nil {
				t.Errorf("the archive's file was not extracted: %v", err)
			}
		})
	}
}

func TestImportJSONRecalculatesTagCountsForMissingFiles(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	body := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(body.Bytes()))

	cx := srv.get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	visible := countRows(t, cx.DB, "images WHERE is_missing = 0")
	if visible != 0 {
		t.Fatalf("test premise broken: %d visible images in a gallery whose files are absent", visible)
	}
	nonZero := countRows(t, cx.DB, "tags WHERE usage_count != 0")
	if nonZero != 0 {
		t.Errorf("%d tag(s) still carry a usage_count over a gallery with no visible images", nonZero)
	}
}

func TestImportGallery_WindowsJSONExport(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.get("stock")

	pngBytes := makePNGBytes(t, 8, 8, 1, 2, 3)
	mustMkdirAll(t, filepath.Join(cx.GalleryPath, "2024", "jan"), 0o755)
	for _, rel := range []string{"2024/jan/a.png", "b.png"} {
		mustWriteFile(t, filepath.Join(cx.GalleryPath, filepath.FromSlash(rel)), pngBytes, 0o644)
	}
	mustExec(t, cx.DB.Write, ` INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, file_size, is_missing, ingested_at) VALUES (1, 'w1', 'D:\pics\2024\jan\a.png', '\2024\jan', 'png', 1, 1, datetime('now')), (2, 'w2', 'D:\pics\b.png',            '',            'png', 1, 1, datetime('now')); INSERT INTO image_paths (id, image_id, path, is_canonical) VALUES (1, 1, 'D:\pics\2024\jan\a.png', 1), (2, 2, 'D:\pics\b.png',            1)`)

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx = srv.get("stock")
	cases := []struct{ sha, wantPath, wantFolder string }{
		{"w1", cx.GalleryPath + "/2024/jan/a.png", "2024/jan"},
		{"w2", cx.GalleryPath + "/b.png", ""},
	}
	for _, c := range cases {
		var gotPath, gotFolder string
		var missing int
		if err := cx.DB.Read.QueryRow(
			`SELECT canonical_path, folder_path, is_missing FROM images WHERE sha256 = ?`, c.sha,
		).Scan(&gotPath, &gotFolder, &missing); err != nil {
			t.Fatalf("read %s: %v", c.sha, err)
		}
		if gotPath != c.wantPath {
			t.Errorf("%s canonical_path = %q, want %q", c.sha, gotPath, c.wantPath)
		}
		if gotFolder != c.wantFolder {
			t.Errorf("%s folder_path = %q, want %q", c.sha, gotFolder, c.wantFolder)
		}
		if missing != 0 {
			t.Errorf("%s is_missing = %d over a file that is on disk", c.sha, missing)
		}
	}
}

func TestImportGallery_WindowsDBFile(t *testing.T) {
	srv := newMultiGalleryServer(t)

	srcPath := filepath.Join(t.TempDir(), "windows.db")
	src, err := db.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(src); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Write.Exec(`
		INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, file_size) VALUES
			(1, 'w1', 'D:\pics\2024\jan\a.png', '\2024\jan', 'png', 1),
			(2, 'w2', 'D:\pics\b.png',            '',            'png', 1);
		INSERT INTO image_paths (id, image_id, path, is_canonical) VALUES
			(1, 1, 'D:\pics\2024\jan\a.png', 1),
			(2, 2, 'D:\pics\b.png',            1)`); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	mustImport(t, srv, "stock", "db", bytes.NewReader(raw))

	cx := srv.get("stock")
	cases := []struct{ sha, wantPath, wantFolder string }{
		{"w1", cx.GalleryPath + "/2024/jan/a.png", "2024/jan"},
		{"w2", cx.GalleryPath + "/b.png", ""},
	}
	for _, c := range cases {
		var gotPath, gotFolder string
		if err := cx.DB.Read.QueryRow(
			`SELECT canonical_path, folder_path FROM images WHERE sha256 = ?`, c.sha,
		).Scan(&gotPath, &gotFolder); err != nil {
			t.Fatalf("read %s: %v", c.sha, err)
		}
		if gotPath != c.wantPath {
			t.Errorf("%s canonical_path = %q, want %q", c.sha, gotPath, c.wantPath)
		}
		if gotFolder != c.wantFolder {
			t.Errorf("%s folder_path = %q, want %q", c.sha, gotFolder, c.wantFolder)
		}
	}
}

func TestImportGalleryJSON_RoundTripsPerImageState(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	cx := srv.get("stock")
	var imgID, tagID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT tag_id FROM image_tags WHERE image_id = ?`, imgID).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE images SET phash = 1234567890123, last_read_page = 4, upload_batch = 99,
		         scheduled_lookup = 0, scheduled_lookup_ptr = 0 WHERE id = ?`, []any{imgID}},
		{`UPDATE image_tags SET stale = 1 WHERE image_id = ?`, []any{imgID}},
		{`INSERT INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec)
		  VALUES (?, 'seed.png', 1, 1700000000, 123456789)`, []any{imgID}},
		{`UPDATE tags SET stale = 1 WHERE name = 'cat'`, nil},
		{`INSERT INTO collection_find_relations (name) VALUES ('extras')`, nil},
		{`INSERT INTO image_lookups (image_id, backend, attempts, last_result, next_due_at, ptr_cursor)
		  VALUES (?, 'booru', 3, 'miss', '2026-09-01T00:00:00Z', 77)`, []any{imgID}},
	} {
		mustExec(t, cx.DB.Write, stmt.sql, stmt.args...)
	}

	buf := mustExport(t, srv, "stock", "json")
	mustImport(t, srv, "stock", "json", bytes.NewReader(buf.Bytes()))

	cx = srv.get("stock")
	var phash, lastRead, uploadBatch, scheduled, scheduledPTR int64
	if err := cx.DB.Read.QueryRow(
		`SELECT phash, last_read_page, upload_batch, scheduled_lookup, scheduled_lookup_ptr
		 FROM images WHERE id = ?`, imgID,
	).Scan(&phash, &lastRead, &uploadBatch, &scheduled, &scheduledPTR); err != nil {
		t.Fatalf("image row: %v", err)
	}
	if phash != 1234567890123 || lastRead != 4 || uploadBatch != 99 || scheduled != 0 || scheduledPTR != 0 {
		t.Errorf("image state = phash %d, page %d, batch %d, lookup %d/%d; want the seeded values",
			phash, lastRead, uploadBatch, scheduled, scheduledPTR)
	}
	var tagStale, usageStale int
	if err := cx.DB.Read.QueryRow(`SELECT stale FROM image_tags WHERE image_id = ?`, imgID).Scan(&usageStale); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT stale FROM tags WHERE name = 'cat'`).Scan(&tagStale); err != nil {
		t.Fatal(err)
	}
	if usageStale != 1 || tagStale != 1 {
		t.Errorf("stale flags = usage %d, tag %d; want 1/1", usageStale, tagStale)
	}
	var mtime, mnsec int64
	if err := cx.DB.Read.QueryRow(
		`SELECT mtime_unix, mtime_nsec FROM image_paths WHERE image_id = ?`, imgID,
	).Scan(&mtime, &mnsec); err != nil {
		t.Fatal(err)
	}
	if mtime != 1700000000 || mnsec != 123456789 {
		t.Errorf("path mtime = %d.%d, want 1700000000.123456789", mtime, mnsec)
	}
	var backend, lastResult, nextDue string
	var attempts, cursor int64
	if err := cx.DB.Read.QueryRow(
		`SELECT backend, attempts, last_result, next_due_at, ptr_cursor FROM image_lookups WHERE image_id = ?`, imgID,
	).Scan(&backend, &attempts, &lastResult, &nextDue, &cursor); err != nil {
		t.Fatalf("lookup ledger: %v", err)
	}
	if backend != "booru" || attempts != 3 || lastResult != "miss" || nextDue != "2026-09-01T00:00:00Z" || cursor != 77 {
		t.Errorf("lookup row = %q/%d/%q/%q/%d, want the seeded values", backend, attempts, lastResult, nextDue, cursor)
	}
	findRel := countRows(t, cx.DB, "collection_find_relations WHERE name = 'extras'")
	if findRel != 1 {
		t.Errorf("collection_find_relations opt-in count = %d, want 1", findRel)
	}
}

func TestImportGalleryJSON_PreV10KeepsScheduledLookupOn(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	buf := mustExport(t, srv, "stock", "json")
	var doc map[string]any
	mustUnmarshal(t, buf.Bytes(), &doc)
	doc["version"] = 9
	for _, img := range doc["images"].([]any) {
		row := img.(map[string]any)
		delete(row, "scheduled_lookup")
		delete(row, "scheduled_lookup_ptr")
	}
	older := mustMarshal(t, doc)
	mustImport(t, srv, "stock", "json", bytes.NewReader(older))

	cx := srv.get("stock")
	off := countRows(t, cx.DB, "images WHERE scheduled_lookup = 0 OR scheduled_lookup_ptr = 0")
	if off != 0 {
		t.Errorf("%d image(s) came back opted out of the lookup ladder, want 0", off)
	}
}

func TestImportGallery_ARefusedUploadLeavesTheGalleryOpen(t *testing.T) {
	srv := newMultiGalleryServer(t)
	before := srv.get("stock")
	if _, err := srv.importGallery("stock", "db", strings.NewReader("not a database")); err == nil {
		t.Fatal("import of a file that is no database succeeded, want error")
	}
	if srv.get("stock") != before || before.Closed() {
		t.Error("a refused upload closed and reopened the gallery")
	}
}
