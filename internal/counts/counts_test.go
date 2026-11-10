package counts

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
)

func openCountsTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func mustExec(t *testing.T, database *db.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := database.Write.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func mkImage(t *testing.T, database *db.DB, sha, path string) int64 {
	t.Helper()
	id, _ := mustExec(t, database, `INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 1)`, sha, path).LastInsertId()
	return id
}

func mkTag(t *testing.T, database *db.DB, name, category string, usage int) int64 {
	t.Helper()
	id, _ := mustExec(t, database, `INSERT INTO tags (name, category_id, usage_count) VALUES (?, (SELECT id FROM tag_categories WHERE name = ?), ?)`, name, category, usage).LastInsertId()
	return id
}

func TestUntaggedVisibleCounts_CacheAndInvalidate(t *testing.T) {
	database := openCountsTestDB(t)
	c := New(database)
	tagged := mkImage(t, database, "sha-tagged", "/a")
	auto := mkImage(t, database, "sha-auto", "/b")
	mkImage(t, database, "sha-untagged", "/c")
	tagID := mkTag(t, database, "blue", "general", 0)
	mustExec(t, database, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, tagged, tagID)
	mustExec(t, database, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 1)`, auto, tagID)

	untagged, ok := c.UntaggedVisibleCount()
	if !ok || untagged != 1 {
		t.Fatalf("UntaggedVisibleCount = (%d, %v), want (1, true)", untagged, ok)
	}
	autoUntagged, ok := c.AutoUntaggedVisibleCount()
	if !ok || autoUntagged != 2 {
		t.Fatalf("AutoUntaggedVisibleCount = (%d, %v), want (2, true)", autoUntagged, ok)
	}

	mkImage(t, database, "sha-new", "/d")
	if got, _ := c.UntaggedVisibleCount(); got != 1 {
		t.Errorf("post-insert pre-invalidate UntaggedVisibleCount = %d, want cached 1", got)
	}

	c.Invalidate()
	if got, _ := c.UntaggedVisibleCount(); got != 2 {
		t.Errorf("post-invalidate UntaggedVisibleCount = %d, want 2 (new row is untagged)", got)
	}
	if got, _ := c.AutoUntaggedVisibleCount(); got != 3 {
		t.Errorf("post-invalidate AutoUntaggedVisibleCount = %d, want 3", got)
	}
}

func TestCountedTagTotals_CacheAndInvalidate(t *testing.T) {
	database := openCountsTestDB(t)
	c := New(database)
	image := mkImage(t, database, "sha-a", "/a")
	attach := func(name, category string, usage int) {
		t.Helper()
		mustExec(t, database, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, image, mkTag(t, database, name, category, usage))
	}
	attach("blue_eyes", "general", 3)
	attach("highres", "meta", 3)
	attach("1girl", "general", 50)

	totals, err := c.CountedTagTotals(context.Background(), 10)
	if err != nil {
		t.Fatalf("CountedTagTotals: %v", err)
	}
	if got := totals.Total(image); got != 1 {
		t.Fatalf("total = %d, want 1 (the meta tag and the over-cap tag are excluded)", got)
	}
	if got := totals.Total(image + 1); got != 0 {
		t.Fatalf("total for an unknown image = %d, want 0", got)
	}

	attach("plaid_scarf", "general", 3)
	totals, _ = c.CountedTagTotals(context.Background(), 10)
	if got := totals.Total(image); got != 1 {
		t.Errorf("post-attach pre-invalidate total = %d, want cached 1", got)
	}

	c.Invalidate()
	totals, _ = c.CountedTagTotals(context.Background(), 10)
	if got := totals.Total(image); got != 2 {
		t.Errorf("post-invalidate total = %d, want 2", got)
	}

	totals, _ = c.CountedTagTotals(context.Background(), 100)
	if got := totals.Total(image); got != 3 {
		t.Errorf("total under a wider cap = %d, want 3", got)
	}

	if c.ReleaseIdleCountedTags(time.Hour) {
		t.Error("released tallies read a moment ago")
	}
	if !c.ReleaseIdleCountedTags(0) {
		t.Error("idle tallies were not released")
	}
	if c.ReleaseIdleCountedTags(0) {
		t.Error("released tallies that were already dropped")
	}
	totals, err = c.CountedTagTotals(context.Background(), 100)
	if err != nil || totals.Total(image) != 3 {
		t.Errorf("post-release read = %d (err %v), want 3", totals.Total(image), err)
	}
}

func TestPageTallies_CacheAndInvalidate(t *testing.T) {
	database := openCountsTestDB(t)
	c := New(database)
	mustExec(t, database, `INSERT INTO images (sha256, canonical_path, file_type, file_size, is_inbox) VALUES ('a', '/g/a.png', 'png', 1, 1)`)

	seededTags, ok := c.TagCount()
	if !ok {
		t.Fatal("TagCount failed")
	}
	mkTag(t, database, "solo", "general", 0)
	c.Invalidate()

	for _, tc := range []struct {
		name string
		read func() (int, bool)
		want int
	}{
		{"inbox", func() (int, bool) { return c.InboxCount() }, 1},
		{"tags", func() (int, bool) { return c.TagCount() }, seededTags + 1},
		{"phash missing", func() (int, bool) { return c.PhashMissing() }, 1},
	} {
		if n, ok := tc.read(); !ok || n != tc.want {
			t.Errorf("%s = %d, %v; want %d", tc.name, n, ok, tc.want)
		}
	}

	counted, err := c.CountedTagTotals(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, `UPDATE images SET phash = 7, is_inbox = 0 WHERE sha256 = 'a'`)
	if n, _ := c.PhashMissing(); n != 1 {
		t.Errorf("phash missing = %d before invalidation, want the cached 1", n)
	}
	c.InvalidatePhashMissing()
	if n, _ := c.PhashMissing(); n != 0 {
		t.Errorf("phash missing = %d after its own invalidation, want 0", n)
	}
	if n, _ := c.InboxCount(); n != 1 {
		t.Errorf("inbox = %d, want the phash drop to leave the cached 1", n)
	}
	if again, err := c.CountedTagTotals(context.Background(), 100); err != nil || again != counted {
		t.Errorf("the phash drop rebuilt the counted-tags index (%v)", err)
	}

	c.Invalidate()
	if n, _ := c.InboxCount(); n != 0 {
		t.Errorf("inbox = %d after Invalidate, want 0", n)
	}
}

func TestUntaggedVisibleCount_SkipsTheDerivedTags(t *testing.T) {
	database := openCountsTestDB(t)
	c := New(database)
	mustExec(t, database, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (1, 'a', '/a', 'png', 1)`)
	mustExec(t, database, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (2, 'b', '/b', 'png', 1)`)
	mustExec(t, database, `INSERT INTO image_tags (image_id, tag_id, tagger_name) VALUES (1, ?, 'monbooru')`, mkTag(t, database, "highres", "meta", 0))
	mustExec(t, database, `INSERT INTO image_tags (image_id, tag_id) VALUES (2, ?)`, mkTag(t, database, "blue_eyes", "general", 0))

	if got, ok := c.UntaggedVisibleCount(); !ok || got != 1 {
		t.Errorf("untagged = %d (ok=%v), want 1: only the hand-tagged row counts as tagged", got, ok)
	}
}

func TestTallies_StaleStampedCountIsIgnored(t *testing.T) {
	database := openCountsTestDB(t)
	c := New(database)
	before := c.gen.Load()
	mustExec(t, database, `INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES ('s', '/a', 'png', 1)`)
	c.Invalidate()

	c.visibleCount.Store(&stampedCount{n: 0, gen: before})

	if n, ok := c.VisibleCount(); !ok || n != 1 {
		t.Fatalf("VisibleCount = (%d, %v), want (1, true)", n, ok)
	}
}
