package relations

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func TestFindPairsBasic(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	c := insertImage(t, database, "c", 3000)
	setPhash(t, database, map[int64]int64{
		a: 0x00,
		b: 0x01,
		c: 0xF0,
	})
	tree := NewBKTree()
	added := mustFindPairs(t, context.Background(), database, tree, FindPairsOptions{Distance: 2}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only a-b within distance 2)", added)
	}
	n := countRows(t, database, "potential_relation_pairs")
	if n != 1 {
		t.Fatalf("queue size = %d, want 1", n)
	}
}

func TestFindPairsSkipsAlreadyRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	setPhash(t, database, map[int64]int64{a: 0, b: 1, c: 2})

	mustAddDuplicate(t, svc, a, b)
	if err := svc.AddNotRelated(a, c); err != nil {
		t.Fatal(err)
	}
	tree := NewBKTree()
	added := mustFindPairs(t, context.Background(), database, tree, FindPairsOptions{Distance: 5}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only b-c is fresh)", added)
	}
	assertQueuedPair(t, database, b, c)
}

func TestFindPairsCancelKeepsTheCountedPairs(t *testing.T) {
	database, _ := setupTestDB(t)

	for i := range 66 {
		id := insertImage(t, database, "c"+strconv.Itoa(i), 100)
		mustExec(t, database.Write, `UPDATE images SET phash = 0 WHERE id = ?`, id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	added, err := FindPairs(ctx, database, NewBKTree(), FindPairsOptions{Distance: 0}, func(processed, _ int, phase string) {
		if phase == "probing" && processed == 64 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if added == 0 {
		t.Fatal("added = 0, want the pairs found before the cancel")
	}
	if n := countRows(t, database, "potential_relation_pairs"); n != added {
		t.Fatalf("queue holds %d pairs, the job reported %d", n, added)
	}
}

func TestFindPairsReplaceWipesQueue(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	setPhash(t, database, map[int64]int64{a: 0, b: 1})
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, 'stale')`, a, b, 99)
	tree := NewBKTree()
	if _, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 2, Replace: true}, nil); err != nil {
		t.Fatal(err)
	}
	var dist int
	if err := database.Read.QueryRow(`SELECT distance FROM potential_relation_pairs`).Scan(&dist); err != nil {
		t.Fatal(err)
	}
	if dist != 1 {
		t.Fatalf("queue distance after replace = %d, want 1 (recomputed)", dist)
	}
}

func TestFindPairsLazyPhashCompute(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustExec(t, database.Write, `UPDATE images SET phash = NULL WHERE id IN (?, ?)`, a, b)
	tree := NewBKTree()
	added := mustFindPairs(t, context.Background(), database, tree, FindPairsOptions{Distance: 4, ThumbnailsPath: "/no/such/path"}, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 (no decodable thumbnails, lazy compute fails)", added)
	}
}

func markArchive(t *testing.T, database *db.DB, id int64) {
	t.Helper()
	mustExec(t, database.Write, `UPDATE images SET file_type = 'cbz' WHERE id = ?`, id)
}

func TestFindPairsSkipsArchives(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	markArchive(t, database, c)
	setPhash(t, database, map[int64]int64{a: 0, b: 0, c: 0})
	tree := NewBKTree()
	added := mustFindPairs(t, context.Background(), database, tree, FindPairsOptions{Distance: 2}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the a-b image pair; the archive is excluded)", added)
	}
	assertQueuedPair(t, database, a, b)
}

func TestIncrementalProbeSkipsArchives(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	markArchive(t, database, c)
	setPhash(t, database, map[int64]int64{a: 0, b: 0, c: 0})
	tree := NewBKTree()
	tree.Insert(a, 0)
	tree.Insert(b, 0)
	tree.Insert(c, 0)

	if err := incrementalProbe(database, tree, c, 0, 4); err != nil {
		t.Fatal(err)
	}
	n := countRows(t, database, "potential_relation_pairs")
	if n != 0 {
		t.Fatalf("archive seed queued %d pair(s), want 0", n)
	}

	if err := incrementalProbe(database, tree, a, 0, 4); err != nil {
		t.Fatal(err)
	}
	assertQueuedPair(t, database, a, b)
}

func TestFindPairsSkipsMissingImages(t *testing.T) {
	database, _ := setupTestDB(t)
	missingLow := insertImage(t, database, "m", 100)
	a := insertImage(t, database, "a", 200)
	b := insertImage(t, database, "b", 300)
	missingHigh := insertImage(t, database, "n", 400)
	setPhash(t, database, map[int64]int64{missingLow: 0, a: 0, b: 0, missingHigh: 0})
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id IN (?, ?)`, missingLow, missingHigh)

	added := mustFindPairs(t, context.Background(), database, NewBKTree(), FindPairsOptions{Distance: 2}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the a-b pair; missing images stay out)", added)
	}
	assertQueuedPair(t, database, a, b)
}

func TestIncrementalProbeSkipsMissingImages(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	gone := insertImage(t, database, "gone", 300)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)
	tree := NewBKTree()
	for _, id := range []int64{a, b, gone} {
		tree.Insert(id, 0)
	}

	if err := incrementalProbe(database, tree, gone, 0, 4); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, "potential_relation_pairs"); n != 0 {
		t.Fatalf("missing seed queued %d pair(s), want 0", n)
	}
	if err := incrementalProbe(database, tree, a, 0, 4); err != nil {
		t.Fatal(err)
	}
	assertQueuedPair(t, database, a, b)
}

func writeTestThumb(t *testing.T, dir string, id int64) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8((x + y) * 255 / 126)
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	f, err := os.Create(filepath.Join(dir, strconv.FormatInt(id, 10)+".jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestFindPairsLazyComputeFindsPair(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	mustExec(t, database.Write, `UPDATE images SET phash = NULL WHERE id IN (?, ?)`, a, b)
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, a)
	writeTestThumb(t, thumbs, b)
	tree := NewBKTree()
	added := mustFindPairs(t, context.Background(), database, tree, FindPairsOptions{Distance: 4, ThumbnailsPath: thumbs}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (identical lazily-phashed thumbnails are a pair)", added)
	}
	assertQueuedPair(t, database, a, b)
}

func TestFindPairsSkipsIndirectChainPairs(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(svc *Service, parent, child int64) error
	}{
		{"derivative", func(svc *Service, parent, child int64) error { return svc.AddDerivativeEdge(parent, child) }},
		{"version", func(svc *Service, parent, child int64) error { return svc.AddVersionEdge(parent, child) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, svc := setupTestDB(t)
			a := insertImage(t, database, "a", 100)
			b := insertImage(t, database, "b", 200)
			c := insertImage(t, database, "c", 300)
			setPhash(t, database, map[int64]int64{a: 0, b: 1, c: 3})
			if err := tc.link(svc, a, b); err != nil {
				t.Fatalf("link a-b: %v", err)
			}
			if err := tc.link(svc, b, c); err != nil {
				t.Fatalf("link b-c: %v", err)
			}
			added := mustFindPairs(t, context.Background(), database, NewBKTree(), FindPairsOptions{Distance: 5}, nil)
			if added != 0 {
				t.Fatalf("added = %d, want 0 (every pair sits on one path)", added)
			}
		})
	}
}

func TestFindPairsKeepsSiblingPairs(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "parent", 100)
	sibA := insertImage(t, database, "sibA", 200)
	sibB := insertImage(t, database, "sibB", 300)
	setPhash(t, database, map[int64]int64{parent: 0, sibA: 1, sibB: 3})
	mustAddDerivativeEdge(t, svc, parent, sibA)
	mustAddDerivativeEdge(t, svc, parent, sibB)
	added := mustFindPairs(t, context.Background(), database, NewBKTree(), FindPairsOptions{Distance: 5}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (the sibling pair)", added)
	}
	var ai, bi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM potential_relation_pairs`).Scan(&ai, &bi); err != nil {
		t.Fatal(err)
	}
	if ai != sibA || bi != sibB {
		t.Fatalf("queue row = (%d, %d), want the sibling pair (%d, %d)", ai, bi, sibA, sibB)
	}
}

func setPhash(t *testing.T, database *db.DB, phashes map[int64]int64) {
	t.Helper()
	for id, phash := range phashes {
		mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, phash, id)
	}
}

func assertQueuedPair(t *testing.T, database *db.DB, a, b int64) {
	t.Helper()
	var ai, bi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM potential_relation_pairs`).Scan(&ai, &bi); err != nil {
		t.Fatal(err)
	}
	if ai != a || bi != b {
		t.Fatalf("queue row = (%d, %d), want (%d, %d)", ai, bi, a, b)
	}
}
