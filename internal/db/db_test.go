package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()

	dir := t.TempDir()
	db, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBootstrap(t *testing.T) {
	db := openTestDB(t)

	mustBootstrap(t, db)

	tables := []string{
		"tag_categories", "tags", "tag_notes",
		"images", "image_paths", "image_tags",
		"sd_metadata", "comfyui_metadata", "manga_metadata", "saved_searches",
	}
	for _, tbl := range tables {
		n := countRows(t, db, "sqlite_master WHERE type='table' AND name=?", tbl)
		if n == 0 {
			t.Errorf("table %q not found after Bootstrap", tbl)
		}
	}
}

func TestBootstrapBuiltinCategories(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	want := []struct {
		name  string
		color string
	}{
		{"general", "#3d90e3"},
		{"character", "#00aa00"},
		{"artist", "#cc0000"},
		{"copyright", "#aa00aa"},
		{"meta", "#ffaa00"},
		{"rating", "#996666"},
		{"medium", "#7d4fbf"},
		{"person", "#b85c9e"},
		{"year", "#4a8fa8"},
		{"species", "#ed5d1f"},
	}
	for _, w := range want {
		var color string
		var isBuiltin int
		err := db.Read.QueryRow(
			`SELECT color, is_builtin FROM tag_categories WHERE name = ?`, w.name,
		).Scan(&color, &isBuiltin)
		if err != nil {
			t.Errorf("category %q: %v", w.name, err)
			continue
		}
		if color != w.color {
			t.Errorf("category %q color = %q, want %q", w.name, color, w.color)
		}
		if isBuiltin != 1 {
			t.Errorf("category %q is_builtin = %d, want 1", w.name, isBuiltin)
		}
	}
}

func TestBootstrapPromotesExistingCustomBuiltins(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, `UPDATE tag_categories SET is_builtin = 0 WHERE name IN ('medium', 'person', 'year', 'species')`)
	var tagID int64
	if err := db.Write.QueryRow(
		`INSERT INTO tags (name, category_id)
		 VALUES ('fox', (SELECT id FROM tag_categories WHERE name = 'species'))
		 RETURNING id`,
	).Scan(&tagID); err != nil {
		t.Fatalf("seed species tag: %v", err)
	}
	mustBootstrap(t, db)
	for _, name := range []string{"medium", "person", "year", "species"} {
		var isBuiltin int
		if err := db.Read.QueryRow(
			`SELECT is_builtin FROM tag_categories WHERE name = ?`, name,
		).Scan(&isBuiltin); err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		if isBuiltin != 1 {
			t.Errorf("category %q is_builtin = %d after re-bootstrap, want 1", name, isBuiltin)
		}
	}
	var gotCat string
	if err := db.Read.QueryRow(
		`SELECT c.name FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE t.id = ?`, tagID,
	).Scan(&gotCat); err != nil {
		t.Fatalf("read seeded tag: %v", err)
	}
	if gotCat != "species" {
		t.Errorf("seeded tag category = %q after re-bootstrap, want species", gotCat)
	}
}

func TestBootstrapBackfillsTagSources(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, `DROP TABLE image_tag_sources`)
	mustExec(t, db.Write, ` INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (1, 'a1', '/g/a1.png', 'png', 1); INSERT INTO tags (id, name, category_id) VALUES (101, 't1', 1), (102, 't2', 1), (103, 't3', 1), (104, 't4', 1); INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied, tagger_name) VALUES (1, 101, 0, 0, NULL), (1, 102, 0, 0, 'danbooru'), (1, 103, 1, 0, 'wd-swinv2'), (1, 104, 0, 1, NULL)`)
	mustBootstrap(t, db)
	want := map[int64]string{101: "user", 102: "danbooru", 103: "wd-swinv2"}
	rows, err := db.Read.Query(`SELECT tag_id, source FROM image_tag_sources WHERE image_id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for rows.Next() {
		var id int64
		var src string
		if err := rows.Scan(&id, &src); err != nil {
			t.Fatal(err)
		}
		if prev, dup := got[id]; dup {
			t.Errorf("tag %d has two ledger rows (%s, %s), want one", id, prev, src)
		}
		got[id] = src
	}
	_ = rows.Close()
	for id, src := range want {
		if got[id] != src {
			t.Errorf("tag %d source = %q, want %q", id, got[id], src)
		}
	}
	if _, implied := got[104]; implied {
		t.Error("implied row got a ledger entry; backfill must skip is_implied = 1")
	}
	mustBootstrap(t, db)
	n := countRows(t, db, "image_tag_sources")
	if n != 3 {
		t.Errorf("ledger rows after repeat bootstrap = %d, want 3", n)
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	db := openTestDB(t)

	mustBootstrap(t, db)
	mustBootstrap(t, db)
}

func TestBootstrap_ASecondRunChangesNoSchema(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	schemaVersion := func() int {
		var v int
		if err := db.Write.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := schemaVersion()

	mustBootstrap(t, db)

	if after := schemaVersion(); after != before {
		t.Errorf("schema_version %d -> %d: a second Bootstrap changed the schema", before, after)
	}
}

func TestBootstrap_DropsTheSourceMirrorIndexes(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, `CREATE INDEX idx_images_source ON images(source_type);
		CREATE INDEX idx_images_source_visible ON images(source) WHERE is_missing = 0;
		CREATE INDEX idx_images_source_nocase_visible ON images(source COLLATE NOCASE) WHERE is_missing = 0;
		PRAGMA user_version = 14`)

	mustBootstrap(t, db)
	if n := countRows(t, db, "sqlite_master WHERE type = 'index' AND name LIKE 'idx_images_source%' AND name NOT LIKE 'idx_images_source_type%'"); n != 0 {
		t.Errorf("%d index(es) on the source mirror left", n)
	}
}
func TestBootstrap_NullsInfiniteStoredFloats(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, `INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, file_size) VALUES
			(1, 'a', '/g/a.png', '', 'png', 1), (2, 'b', '/g/b.png', '', 'png', 1), (3, 'c', '/g/c.cbz', '', 'cbz', 1);
		INSERT INTO sd_metadata (image_id, cfg_scale) VALUES (1, 9e999), (2, 7);
		INSERT INTO comfyui_metadata (image_id, cfg_scale) VALUES (1, -9e999);
		INSERT INTO manga_metadata (image_id, community_rating) VALUES (3, 9e999);
		PRAGMA user_version = 15`)

	mustBootstrap(t, db)
	if n := countRows(t, db, "sd_metadata WHERE cfg_scale IS NULL"); n != 1 {
		t.Errorf("sd_metadata rows with no cfg_scale = %d, want the infinite one alone", n)
	}
	if n := countRows(t, db, "sd_metadata WHERE cfg_scale = 7"); n != 1 {
		t.Errorf("a finite cfg_scale was touched")
	}
	if n := countRows(t, db, "comfyui_metadata WHERE cfg_scale IS NOT NULL"); n != 0 {
		t.Errorf("an infinite comfyui cfg_scale survived")
	}
	if n := countRows(t, db, "manga_metadata WHERE community_rating IS NOT NULL"); n != 0 {
		t.Errorf("an infinite community_rating survived")
	}
}

func TestBootstrap_AnalyzesImagesInFull(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
		SELECT 'h' || i, '/g/' || i || '.png', '', 'png', 1, i % 100 = 0 FROM n;
		PRAGMA user_version = 15`)

	mustBootstrap(t, db)
	var stat string
	if err := db.Read.QueryRow(`SELECT stat FROM sqlite_stat1 WHERE idx = 'idx_images_missing'`).Scan(&stat); err != nil {
		t.Fatal(err)
	}
	var rows, perValue int
	if _, err := fmt.Sscanf(stat, "%d %d", &rows, &perValue); err != nil {
		t.Fatalf("stat %q: %v", stat, err)
	}
	if rows != 2000 || perValue < rows/2 {
		t.Errorf("idx_images_missing stat = %q, want 2000 rows at about 1000 per value", stat)
	}
}

func TestDeleteChildLookupsSeekAnIndex(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	for _, c := range []struct{ query, index string }{
		{`SELECT image_id FROM image_tag_sources WHERE tag_id = 1`, "idx_image_tag_sources_tag"},
		{`SELECT id FROM tags WHERE canonical_tag_id = 1`, "idx_tags_canonical"},
		{`SELECT id FROM dup_groups WHERE original_image_id = 1`, "idx_dup_groups_original"},
	} {
		details, err := QueryAll(db.Read, func(r *sql.Rows) (string, error) {
			var id, parent, notused int
			var detail string
			err := r.Scan(&id, &parent, &notused, &detail)
			return detail, err
		}, `EXPLAIN QUERY PLAN `+c.query)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for _, detail := range details {
			plan += detail + "\n"
		}
		if !strings.Contains(plan, c.index) {
			t.Errorf("%s plans as\n%s want %s", c.query, plan, c.index)
		}
	}
}

func TestBootstrap_RefusesALibraryBeforeTheWindowAndBootstrapsAFreshOne(t *testing.T) {
	old := openTestDB(t)
	mustBootstrap(t, old)
	mustExec(t, old.Write, fmt.Sprintf(`PRAGMA user_version = %d`, MinUpgradeUserVersion-1))
	if err := Bootstrap(old); !errors.Is(err, ErrLibraryTooOld) {
		t.Errorf("a library under the window: err = %v, want ErrLibraryTooOld", err)
	}
	fresh := openTestDB(t)
	mustBootstrap(t, fresh)
}

func TestBootstrap_AFreshLibraryInterruptedAfterTheSchemaReopens(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db.Write, `CREATE TABLE idx_images_inbox_visible (x)`)
	if err := Bootstrap(db); err == nil {
		t.Fatal("the interrupted boot reported success")
	}
	mustExec(t, db.Write, `DROP TABLE idx_images_inbox_visible`)
	mustBootstrap(t, db)
}

func TestOpenTakesAPathWithURICharacters(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, dir := range []string{"plain", "#photos", "q?mark", "sp ace", "pct%41", "a&b=c", filepath.Join(root, "abs#q?%41")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "monbooru.db")
		db, err := Open(path)
		if err != nil {
			t.Errorf("%s: Open: %v", dir, err)
			continue
		}
		var timeout int
		if err := db.Write.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
			t.Errorf("%s: busy_timeout = %d, %v; want 5000", dir, timeout, err)
		}
		_ = db.Close()
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: the database is not at its path: %v", dir, err)
		}
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)

	_, err := db.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id) VALUES (9999, 9999)`,
	)
	if err == nil {
		t.Error("expected foreign key error, got nil")
	}
}

func TestConcurrentReads(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			var n int
			if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
				t.Errorf("concurrent read failed: %v", err)
			}
		})
	}
	wg.Wait()
}

func TestBootstrapMangaColumns(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	for _, col := range []string{"page_count", "series"} {
		n := countRows(t, db, "pragma_table_info('images') WHERE name = ?", col)
		if n != 1 {
			t.Errorf("images.%s missing after Bootstrap", col)
		}
	}
	idxN := countRows(t, db, "sqlite_master WHERE type='index' AND name='idx_images_series'")
	if idxN != 1 {
		t.Errorf("idx_images_series not registered after Bootstrap")
	}
}

func TestShrinkMemory(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	for i := 0; i < 4; i++ {
		countRows(t, db, "tag_categories")
	}
	if err := db.ShrinkMemory(context.Background()); err != nil {
		t.Errorf("ShrinkMemory: %v", err)
	}
	var n int
	if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
		t.Errorf("read after shrink: %v", err)
	}
}

func TestShrinkMemoryAndOptimizeDoNotDeadlock(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	for round := 0; round < 20; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		errs := make(chan error, 2)
		go func() { errs <- db.Optimize(ctx) }()
		go func() { errs <- db.ShrinkMemory(ctx) }()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		cancel()
	}
}

func TestShrinkMemorySkipsAPoolAReaderHolds(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	held, err := db.Read.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	start := time.Now()
	if err := db.ShrinkMemory(context.Background()); !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("ShrinkMemory with a reader held = %v, want ErrPoolBusy", err)
	}
	if waited := time.Since(start); waited > 3*eachConnWait {
		t.Errorf("the pass waited %v on the held reader", waited)
	}
	var n int
	if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
		t.Errorf("read after the skipped pass: %v", err)
	}
}

func TestOptimizeReachesPooledReaders(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	const q = `SELECT COUNT(DISTINCT fp.old_id) FROM folded_tag_pairs fp
		JOIN tags o ON o.id = fp.old_id AND o.is_alias = 0
		JOIN tags n ON n.id = fp.new_id AND n.is_alias = 0 AND n.category_id = o.category_id`
	plan := func() string {
		rows, err := db.Read.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var steps []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, detail)
		}
		return strings.Join(steps, "; ")
	}

	if got := plan(); !strings.Contains(got, "idx_tags_category") {
		t.Fatalf("stale plan = %q, want the per-category scan", got)
	}
	tx, err := db.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if _, err := tx.Exec(`INSERT INTO tags(name, category_id) VALUES (?, 1)`, fmt.Sprintf("t%04d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Optimize(context.Background()); err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if got := plan(); strings.Contains(got, "idx_tags_category") {
		t.Errorf("reader plan = %q, still the per-category scan after Optimize", got)
	}
}

func TestBasenameSQL(t *testing.T) {
	db := openTestDB(t)
	tests := []struct {
		name string
		path string
		want string
	}{
		{"posix", "/gallery/2024/jan/x.png", "x.png"},
		{"windows", `C:\gallery\2024\jan\x.png`, "x.png"},
		{"mixed", `C:/gallery\x.png`, "x.png"},
		{"bare name", "x.png", "x.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if err := db.Read.QueryRow(`SELECT basename(?)`, tt.path).Scan(&got); err != nil {
				t.Fatalf("basename(%q): %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("basename(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
	var null sql.NullString
	if err := db.Read.QueryRow(`SELECT basename(NULL)`).Scan(&null); err != nil {
		t.Fatalf("basename(NULL): %v", err)
	}
	if null.Valid {
		t.Errorf("basename(NULL) = %q, want NULL", null.String)
	}
}
func TestBootstrapUpgradeMatrix(t *testing.T) {
	for v := 0; v <= bootstrapSchemaVersion; v++ {
		t.Run(fmt.Sprintf("from_v%d", v), func(t *testing.T) {
			db := openTestDB(t)
			mustBootstrap(t, db)
			if _, err := db.Write.Exec(`
				INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, file_size, is_missing) VALUES
					(1, 'a', '/srv/g/2024/jan/a.png', '2024/jan', 'png', 1, 0),
					(2, 'b', '/srv/g/root.png',       '',         'png', 1, 0);
				INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, source, score)
					VALUES (1, 2, 0, 'tags', 0.8)`,
			); err != nil {
				t.Fatalf("seed: %v", err)
			}
			mustExec(t, db.Write, fmt.Sprintf(`PRAGMA user_version = %d`, v))
			marker := func() int {
				t.Helper()
				var m int
				if err := db.Read.QueryRow(`PRAGMA user_version`).Scan(&m); err != nil {
					t.Fatal(err)
				}
				return m
			}

			if v < MinUpgradeUserVersion {
				before := schemaShape(t, db)
				if err := Bootstrap(db); !errors.Is(err, ErrLibraryTooOld) || !strings.Contains(err.Error(), UpgradeVia) {
					t.Fatalf("from v%d: err = %v, want the refusal naming %s", v, err, UpgradeVia)
				}
				if got := marker(); got != v {
					t.Errorf("a refused library's user_version moved to %d", got)
				}
				if !slices.Equal(schemaShape(t, db), before) {
					t.Error("a refused library's schema was written to")
				}
				if got := countRows(t, db, "potential_relation_pairs"); got != 1 {
					t.Errorf("a refused library was written to: %d pairs left, want 1", got)
				}
				return
			}
			mustBootstrap(t, db)
			if got := marker(); got != bootstrapSchemaVersion {
				t.Errorf("user_version = %d, want %d", got, bootstrapSchemaVersion)
			}
			shape := schemaShape(t, db)
			for i := 0; i < 2; i++ {
				mustBootstrap(t, db)
			}
			if !slices.Equal(schemaShape(t, db), shape) {
				t.Error("a re-boot changed the schema")
			}
			if images, pairs := countRows(t, db, "images"), countRows(t, db, "potential_relation_pairs"); images != 2 || pairs != 1 {
				t.Errorf("from v%d: %d images and %d pairs, want 2 and 1", v, images, pairs)
			}
		})
	}
}

func schemaShape(t *testing.T, d *DB) []string {
	t.Helper()
	out, err := QueryStrings(d.Read, `
		SELECT m.type || ' ' || m.name || ' ' || COALESCE(CASE WHEN m.type IN ('index', 'trigger') THEN m.sql END, '')
		  FROM sqlite_master m WHERE m.name NOT LIKE 'sqlite_%'
		UNION ALL
		SELECT 'column ' || m.name || '.' || p.name || ' ' || p.type || ' ' || p."notnull" || ' ' || COALESCE(p.dflt_value, '') || ' ' || p.pk || ' ' || p.hidden
		  FROM sqlite_master m, pragma_table_xinfo(m.name) p
		 WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.sql NOT LIKE 'CREATE VIRTUAL%'`)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestBootstrap_ALibraryAtTheFloorConvergesOnAFreshOne(t *testing.T) {
	dump, err := os.ReadFile(filepath.Join("testdata", "v1.19.0-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	floor := openTestDB(t)
	mustExec(t, floor.Write, string(dump))
	if marker := countRows(t, floor, "pragma_user_version WHERE user_version = ?", MinUpgradeUserVersion); marker != 1 {
		t.Fatalf("the floor dump is not at marker %d", MinUpgradeUserVersion)
	}
	mustBootstrap(t, floor)
	fresh := openTestDB(t)
	mustBootstrap(t, fresh)

	upgraded, want := schemaShape(t, floor), schemaShape(t, fresh)
	for _, row := range upgraded {
		if !slices.Contains(want, row) {
			t.Errorf("only the upgraded library has %s", row)
		}
	}
	for _, row := range want {
		if !slices.Contains(upgraded, row) {
			t.Errorf("only a fresh library has %s", row)
		}
	}
}

func TestBootstrapWidensDerivativeEdgesKey(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, ` INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (1, 'a', '/srv/g/a.png', 'png', 1), (2, 'b', '/srv/g/b.png', 'png', 1), (3, 'c', '/srv/g/c.png', 'png', 1); DROP TABLE derivative_edges; CREATE TABLE derivative_edges ( derivative_image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE, source_image_id     INTEGER NOT NULL    REFERENCES images(id) ON DELETE CASCADE, created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')) ); INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (3, 1, 'seeded')`)

	mustBootstrap(t, db)

	var src, ts string
	if err := db.Read.QueryRow(
		`SELECT source_image_id, created_at FROM derivative_edges WHERE derivative_image_id = 3`,
	).Scan(&src, &ts); err != nil {
		t.Fatalf("seeded edge lost: %v", err)
	}
	if src != "1" || ts != "seeded" {
		t.Fatalf("edge = (%s, %s), want (1, seeded)", src, ts)
	}
	mustExec(t, db.Write, `INSERT INTO derivative_edges (derivative_image_id, source_image_id) VALUES (3, 2)`)
	if _, err := db.Write.Exec(
		`INSERT INTO derivative_edges (derivative_image_id, source_image_id) VALUES (3, 2)`,
	); err == nil {
		t.Fatal("duplicate edge accepted, want a primary-key conflict")
	}
	idx := countRows(t, db, "sqlite_master WHERE type = 'index' AND name = 'idx_derivative_edges_source'")
	if idx != 1 {
		t.Fatal("idx_derivative_edges_source did not survive the rebuild")
	}

	mustBootstrap(t, db)
	edges := countRows(t, db, "derivative_edges")
	if edges != 2 {
		t.Fatalf("edges after re-Bootstrap = %d, want 2", edges)
	}
}

func TestBootstrapDissolvesDegenerateRelationGroups(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, ` INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (1, 'a', '/srv/g/a.png', 'png', 1), (2, 'b', '/srv/g/b.png', 'png', 1), (3, 'c', '/srv/g/c.png', 'png', 1), (4, 'd', '/srv/g/d.png', 'png', 1), (5, 'e', '/srv/g/e.png', 'png', 1), (6, 'f', '/srv/g/f.png', 'png', 1); INSERT INTO alt_groups (id) VALUES (10), (11), (12); INSERT INTO alt_group_members (image_id, group_id) VALUES (1, 10), (2, 10), (3, 12); INSERT INTO dup_groups (id, original_image_id) VALUES (20, 4), (21, 5); INSERT INTO dup_group_members (image_id, group_id) VALUES (4, 20), (5, 21), (6, 21)`)

	mustBootstrap(t, db)

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{`SELECT id FROM alt_groups ORDER BY id`, []int64{10}},
		{`SELECT image_id FROM alt_group_members ORDER BY image_id`, []int64{1, 2}},
		{`SELECT id FROM dup_groups ORDER BY id`, []int64{21}},
		{`SELECT image_id FROM dup_group_members ORDER BY image_id`, []int64{5, 6}},
	} {
		got, err := QueryIDs(db.Read, tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.query, got, tc.want)
		}
	}

	images := countRows(t, db, "images")
	if images != 6 {
		t.Errorf("images = %d, want 6 (the sweep must not reach past the group rows)", images)
	}
}

func seedVerdictRows(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db.Write, ` INSERT INTO images (id, sha256, md5, canonical_path, file_type, file_size) VALUES (1, 'a', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '/g/1.png', 'png', 1), (2, 'b', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', '/g/2.png', 'png', 1), (3, 'c', '',                                 '/g/3.png', 'png', 1), (4, 'd', 'dddddddddddddddddddddddddddddddd', '/g/4.png', 'png', 1); INSERT INTO image_sources (image_id, site, post_id, url, md5) VALUES (1, 'danbooru', '1', 'https://d/1', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'), (2, 'danbooru', '2', 'https://d/2', 'CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC'), (3, 'danbooru', '3', 'https://d/3', 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'), (4, 'danbooru', '4', 'https://d/4', '')`)
}

func verdictOf(t *testing.T, db *DB, imageID int64) string {
	t.Helper()
	var v string
	if err := db.Read.QueryRow(
		`SELECT md5_match FROM image_sources WHERE image_id = ?`, imageID).Scan(&v); err != nil {
		t.Fatalf("read verdict for image %d: %v", imageID, err)
	}
	return v
}
func TestLocalDigestArrivingSettlesTheOrigins(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	seedVerdictRows(t, db)
	if got := verdictOf(t, db, 3); got != "" {
		t.Fatalf("image 3 verdict = %q before its file was hashed, want empty", got)
	}

	mustExec(t, db.Write, `UPDATE images SET md5 = 'ffffffffffffffffffffffffffffffff' WHERE id = 3`)
	if got := verdictOf(t, db, 3); got != "differ" {
		t.Errorf("image 3 verdict = %q after hashing, want differ", got)
	}

	mustExec(t, db.Write, `UPDATE images SET md5 = 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee' WHERE id = 3`)
	if got := verdictOf(t, db, 3); got != "match" {
		t.Errorf("image 3 verdict = %q after the local file became the source's, want match", got)
	}
}

func TestSourceClaimWritesTheVerdict(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	seedVerdictRows(t, db)

	if got := verdictOf(t, db, 1); got != "match" {
		t.Errorf("equal digests = %q, want match", got)
	}
	if got := verdictOf(t, db, 2); got != "differ" {
		t.Errorf("differing digests = %q, want differ (uppercase claim)", got)
	}

	mustExec(t, db.Write, `UPDATE image_sources SET md5 = 'dddddddddddddddddddddddddddddddd' WHERE image_id = 4`)
	if got := verdictOf(t, db, 4); got != "match" {
		t.Errorf("late claim = %q, want match", got)
	}
}

func TestKeepRulingLapsesOnANewClaim(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	seedVerdictRows(t, db)
	mustExec(t, db.Write, `UPDATE image_sources SET upgrade_kept = 1 WHERE image_id = 2`)

	kept := func() bool {
		var k bool
		if err := db.Read.QueryRow(
			`SELECT upgrade_kept FROM image_sources WHERE image_id = 2`).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}

	mustExec(t, db.Write, `UPDATE image_sources SET md5 = 'cccccccccccccccccccccccccccccccc' WHERE image_id = 2`)
	if !kept() {
		t.Error("a re-claim of the same digest cleared the keep ruling")
	}
	mustExec(t, db.Write, `UPDATE image_sources SET md5 = '11111111111111111111111111111111' WHERE image_id = 2`)
	if kept() {
		t.Error("a new claimed digest left the keep ruling in place")
	}
}

func TestRecordedMismatchSurvivesADisagreeingClaim(t *testing.T) {
	db := openTestDB(t)
	mustBootstrap(t, db)
	mustExec(t, db.Write, ` INSERT INTO images (id, sha256, md5, canonical_path, file_type, file_size) VALUES (1, 'a', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '/g/1.png', 'png', 1); INSERT INTO image_sources (image_id, site, post_id, url, md5) VALUES (1, 'danbooru', '1', 'https://d/1', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`)

	mustExec(t, db.Write, `UPDATE image_sources SET md5_match = 'differ' WHERE image_id = 1`)

	mustExec(t, db.Write, `UPDATE images SET md5 = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 1`)
	if got := verdictOf(t, db, 1); got != "differ" {
		t.Errorf("verdict after a no-op digest rewrite = %q, want differ", got)
	}

	mustExec(t, db.Write, `UPDATE images SET md5 = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE id = 1`)
	if got := verdictOf(t, db, 1); got != "differ" {
		t.Errorf("verdict after a real file change = %q, want differ", got)
	}
	mustExec(t, db.Write, `UPDATE images SET md5 = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 1`)
	if got := verdictOf(t, db, 1); got != "match" {
		t.Errorf("verdict once the local file is the claimed one again = %q, want match", got)
	}
}

func countRows(t *testing.T, database *DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustBootstrap(t *testing.T, database *DB) {
	t.Helper()
	if err := Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
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

func TestVacuum(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "vacuum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	if err := database.Vacuum(context.Background()); err != nil {
		t.Errorf("Vacuum: %v", err)
	}
}

func TestBootstrapOrdersCategoriesTheWayTheImagePageDid(t *testing.T) {
	categoryNames := func(db *DB) []string {
		t.Helper()
		names, err := QueryStrings(db.Read, `SELECT name FROM tag_categories ORDER BY position`)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	shipped := []string{"rating", "year", "meta", "medium", "person", "artist", "copyright", "character", "species"}

	fresh := openTestDB(t)
	mustBootstrap(t, fresh)
	if got, want := categoryNames(fresh), append(slices.Clone(shipped), "general"); !slices.Equal(got, want) {
		t.Errorf("fresh library order = %v, want %v", got, want)
	}

	upgraded := openTestDB(t)
	mustBootstrap(t, upgraded)
	mustExec(t, upgraded.Write, `ALTER TABLE tag_categories DROP COLUMN position`)
	mustExec(t, upgraded.Write, `INSERT INTO tag_categories (name, color) VALUES ('zeta', '#123456'), ('alpha', '#654321')`)
	mustBootstrap(t, upgraded)
	if got, want := categoryNames(upgraded), append(slices.Clone(shipped), "alpha", "zeta", "general"); !slices.Equal(got, want) {
		t.Errorf("upgraded library order = %v, want %v", got, want)
	}

	mustExec(t, upgraded.Write, `UPDATE tag_categories SET position = -1 WHERE name = 'general'`)
	mustBootstrap(t, upgraded)
	if got := categoryNames(upgraded)[0]; got != "general" {
		t.Errorf("a later boot reset the order: first = %q, want general", got)
	}
}
