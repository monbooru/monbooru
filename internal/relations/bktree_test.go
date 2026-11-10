package relations

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

func TestBKTreeInsertAndExactSearch(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0x1234567890ABCDEF)
	tree.Insert(2, 0x1234567890ABCDEF)
	tree.Insert(3, 0x0)

	got, _ := tree.SearchWithinDistance(0x1234567890ABCDEF, 0)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("exact match: got %v, want [1 2]", got)
	}
}

func TestBKTreeMatchesLinearScan(t *testing.T) {
	tree := NewBKTree()
	rng := rand.New(rand.NewSource(42))
	const n = 200
	hashes := make([]int64, n)
	for i := 0; i < n; i++ {
		h := int64(rng.Uint64())
		hashes[i] = h
		tree.Insert(int64(i+1), h)
	}
	queries := []int64{0, hashes[0], hashes[42], hashes[199], int64(rng.Uint64())}
	for _, q := range queries {
		for _, d := range []int{0, 2, 4, 8, 16, 32, 64} {
			want := linearSearch(hashes, q, d)
			got, _ := tree.SearchWithinDistance(q, d)
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			sort.Ints(want)
			if len(got) != len(want) {
				t.Fatalf("q=%016x d=%d: tree=%v linear=%v", uint64(q), d, got, want)
			}
			for i := range got {
				if int(got[i]) != want[i]+1 {
					t.Fatalf("q=%016x d=%d at %d: tree=%v linear=%v", uint64(q), d, i, got, want)
				}
			}
		}
	}
}

func TestBKTreeRemove(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0xAA)
	tree.Insert(2, 0xBB)
	tree.Insert(3, 0xCC)
	tree.Remove(2)
	if got, _ := tree.SearchWithinDistance(0xBB, 0); len(got) != 0 {
		t.Fatalf("after Remove(2), got %v, want []", got)
	}
	if tree.Size() != 2 {
		t.Fatalf("size = %d, want 2", tree.Size())
	}

	tree.Remove(2)
	tree.Remove(999)
}

func TestBKTreeInsertReplacesPreviousPhash(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0xAA)
	tree.Insert(1, 0xBB)
	if got, _ := tree.SearchWithinDistance(0xAA, 0); len(got) != 0 {
		t.Fatalf("after re-insert, old phash still matches: %v", got)
	}
	if got, _ := tree.SearchWithinDistance(0xBB, 0); len(got) != 1 || got[0] != 1 {
		t.Fatalf("new phash search: got %v, want [1]", got)
	}
}

func TestBKTreeReset(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0xAA)
	tree.Insert(2, 0xBB)
	tree.Reset()
	if tree.Size() != 0 {
		t.Fatalf("size after Reset = %d, want 0", tree.Size())
	}
	if got, _ := tree.SearchWithinDistance(0xAA, 64); len(got) != 0 {
		t.Fatalf("Reset leaked entries: %v", got)
	}
	if tree.Built() {
		t.Fatal("Built() = true after Reset")
	}
}

func TestBKTreeReleaseIdle(t *testing.T) {
	database, _ := setupTestDB(t)
	id := insertImage(t, database, "a", 100)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, int64(0xAA), id)
	tree := NewBKTree()
	if err := tree.BuildFromDB(database); err != nil {
		t.Fatal(err)
	}
	if tree.ReleaseIdle(time.Hour) {
		t.Fatal("released a tree searched a moment ago")
	}
	if !tree.ReleaseIdle(0) {
		t.Fatal("idle tree was not released")
	}
	if tree.Built() || tree.Size() != 0 {
		t.Fatalf("after release: built=%v size=%d, want false/0", tree.Built(), tree.Size())
	}
	if _, built := tree.SearchWithinDistance(0xAA, 0); built {
		t.Fatal("search reported a built tree after release")
	}
	if tree.ReleaseIdle(0) {
		t.Fatal("released a tree that holds nothing")
	}
	if err := tree.EnsureBuilt(database); err != nil {
		t.Fatal(err)
	}
	got, built := tree.SearchWithinDistance(0xAA, 0)
	if !built || len(got) != 1 || got[0] != id {
		t.Fatalf("after rebuild: got %v built=%v, want [%d] true", got, built, id)
	}
}

func TestBKTreeBuildFromDB(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, int64(0xAA), a)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, int64(0xBB), b)

	tree := NewBKTree()
	if err := tree.BuildFromDB(database); err != nil {
		t.Fatalf("BuildFromDB: %v", err)
	}
	if tree.Size() != 2 {
		t.Fatalf("Size = %d, want 2 (c has NULL phash)", tree.Size())
	}
	if !tree.Built() {
		t.Fatal("Built() = false after BuildFromDB")
	}
	got, _ := tree.SearchWithinDistance(0xAA, 0)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("search for a: got %v, want [%d]", got, a)
	}
	_ = c
}

func TestPhashClearedDropsTheTreeEntry(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, 0x55, a)
	tree := NewBKTree()
	if err := tree.BuildFromDB(database); err != nil {
		t.Fatal(err)
	}

	PhashSink(database, tree).Stored(a, nil)
	if got, _ := tree.SearchWithinDistance(0x55, 0); len(got) != 0 {
		t.Errorf("the cleared hash still matches %v", got)
	}
}

func TestPhashStoredQueuesAPairWhenNoTreeIsBuiltYet(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id IN (?, ?)`, 0x55, a, b)
	tree := NewBKTree()

	PhashStored(database, tree, b, 0x55)
	if tree.Built() {
		t.Fatal("a single store built the tree")
	}
	var ai, bi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM potential_relation_pairs`).Scan(&ai, &bi); err != nil {
		t.Fatalf("no pair queued: %v", err)
	}
	if ai != a || bi != b {
		t.Errorf("queue row = (%d, %d), want (%d, %d)", ai, bi, a, b)
	}

	IncrementalProbeEnabled.Store(false)
	t.Cleanup(func() { IncrementalProbeEnabled.Store(true) })
	idle := NewBKTree()
	PhashStored(database, idle, a, 0x55)
	if idle.Built() {
		t.Error("with the probe off, a store built the tree")
	}
}

func TestPhashStored_BuildsTheTreeAfterManyUnbuiltProbes(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, 0x55, a)
	tree := NewBKTree()
	for range unbuiltProbesBeforeBuild {
		PhashStored(database, tree, a, 0x55)
	}
	if tree.Built() {
		t.Fatalf("%d stores built the tree", unbuiltProbesBeforeBuild)
	}
	PhashStored(database, tree, a, 0x55)
	if !tree.Built() {
		t.Fatalf("store %d left the tree unbuilt", unbuiltProbesBeforeBuild+1)
	}
	tree.Reset()
	PhashStored(database, tree, a, 0x55)
	if tree.Built() {
		t.Error("the first store after a release built the tree")
	}
}

func TestPhashStoredDuringABuildEntersTheFinishedTree(t *testing.T) {
	t.Cleanup(func() { IncrementalProbeEnabled.Store(true) })
	for _, probe := range []bool{true, false} {
		IncrementalProbeEnabled.Store(probe)
		database, _ := setupTestDB(t)
		a := insertImage(t, database, "a", 100)
		mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = ?`, 0x55, a)
		tree := NewBKTree()

		tree.mu.Lock()
		done := make(chan struct{})
		go func() {
			PhashStored(database, tree, a, 0x55)
			close(done)
		}()

		time.Sleep(50 * time.Millisecond)
		tree.built.Store(true)
		tree.mu.Unlock()
		<-done

		if got, _ := tree.SearchWithinDistance(0x55, 0); len(got) != 1 || got[0] != a {
			t.Errorf("probe=%v: tree holds %v after the build, want [%d]", probe, got, a)
		}
	}
}

func linearSearch(hashes []int64, query int64, d int) []int {
	var out []int
	for i, h := range hashes {
		if hammingDistance(h, query) <= d {
			out = append(out, i)
		}
	}
	return out
}
