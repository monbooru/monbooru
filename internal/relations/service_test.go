package relations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

func onImagesDelete(t *testing.T, database *db.DB, svc *Service, ids []int64) {
	t.Helper()
	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return svc.OnImagesDeleteTx(tx, ids)
	}); err != nil {
		t.Fatalf("OnImagesDeleteTx: %v", err)
	}
}

func setupTestDB(t *testing.T) (*db.DB, *Service) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, New(database, tags.New(database, nil), nil)
}

func insertImage(t *testing.T, database *db.DB, sha string, fileSize int64) int64 {
	t.Helper()
	var id int64
	err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', ?) RETURNING id`,
		sha, "/gallery/"+sha+".png", fileSize,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertImage: %v", err)
	}
	return id
}

func dupGroupOf(t *testing.T, database *db.DB, imageID int64) (groupID sql.NullInt64, original sql.NullInt64) {
	t.Helper()
	err := database.Read.QueryRow(
		`SELECT m.group_id, g.original_image_id FROM dup_group_members m JOIN dup_groups g ON g.id = m.group_id WHERE m.image_id = ?`,
		imageID,
	).Scan(&groupID, &original)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	if err != nil {
		t.Fatalf("dupGroupOf: %v", err)
	}
	return
}

func altGroupOf(t *testing.T, database *db.DB, imageID int64) sql.NullInt64 {
	t.Helper()
	var gid sql.NullInt64
	err := database.Read.QueryRow(`SELECT group_id FROM alt_group_members WHERE image_id = ?`, imageID).Scan(&gid)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}
	}
	if err != nil {
		t.Fatalf("altGroupOf: %v", err)
	}
	return gid
}

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func dupGroupSize(t *testing.T, database *db.DB, groupID int64) int {
	return countRows(t, database, "dup_group_members WHERE group_id = ?", groupID)
}

func TestAddDuplicateSelf(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	if err := svc.AddDuplicate(a, a); !errors.Is(err, ErrSelfRelation) {
		t.Fatalf("AddDuplicate(self) = %v, want ErrSelfRelation", err)
	}
}

func TestAddDuplicateBothSingleton(t *testing.T) {
	database, svc := setupTestDB(t)
	small := insertImage(t, database, "small", 100)
	big := insertImage(t, database, "big", 5000)

	mustAddDuplicate(t, svc, small, big)
	gid, orig := dupGroupOf(t, database, small)
	if !gid.Valid {
		t.Fatal("small not in a dup group")
	}
	if !orig.Valid || orig.Int64 != small {
		t.Fatalf("original = %v, want %d (first arg)", orig.Int64, small)
	}
	if dupGroupSize(t, database, gid.Int64) != 2 {
		t.Fatalf("dup group size = %d, want 2", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateIdempotent(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, a, b)
	gid, _ := dupGroupOf(t, database, a)
	if dupGroupSize(t, database, gid.Int64) != 2 {
		t.Fatalf("group size after second add = %d, want 2", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateJoinExisting(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)

	mustAddDuplicate(t, svc, b, a)
	mustAddDuplicate(t, svc, c, b)
	gA, _ := dupGroupOf(t, database, a)
	gC, origC := dupGroupOf(t, database, c)
	if gA.Int64 != gC.Int64 {
		t.Fatalf("a and c not in same group: %d vs %d", gA.Int64, gC.Int64)
	}
	if origC.Int64 != b {
		t.Fatalf("original = %d, want %d (existing)", origC.Int64, b)
	}
	if dupGroupSize(t, database, gA.Int64) != 3 {
		t.Fatalf("group size = %d, want 3", dupGroupSize(t, database, gA.Int64))
	}
}

func TestAddDuplicateRightInGroupKeepsOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 5000)
	dup := insertImage(t, database, "dup", 100)
	newcomer := insertImage(t, database, "new", 200)
	mustAddDuplicate(t, svc, original, dup)
	mustAddDuplicate(t, svc, newcomer, original)
	gid, orig := dupGroupOf(t, database, newcomer)
	if !gid.Valid {
		t.Fatal("newcomer not in a group")
	}
	if orig.Int64 != original {
		t.Fatalf("original = %d, want %d (existing untouched)", orig.Int64, original)
	}
	if dupGroupSize(t, database, gid.Int64) != 3 {
		t.Fatalf("group size = %d, want 3", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateSameGroupNoOp(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 5000)
	dup := insertImage(t, database, "dup", 100)
	mustAddDuplicate(t, svc, original, dup)
	mustAddDuplicate(t, svc, dup, original)
	gid, orig := dupGroupOf(t, database, dup)
	if orig.Int64 != original {
		t.Fatalf("original flipped to %d, want %d", orig.Int64, original)
	}
	if dupGroupSize(t, database, gid.Int64) != 2 {
		t.Fatalf("group size = %d, want 2 (no duplicate insert)", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateMergesGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	d := insertImage(t, database, "d", 250)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, c, d)
	mustAddDuplicate(t, svc, b, c)
	gA, _ := dupGroupOf(t, database, a)
	gD, _ := dupGroupOf(t, database, d)
	if gA.Int64 != gD.Int64 {
		t.Fatalf("merge incomplete: a in %d, d in %d", gA.Int64, gD.Int64)
	}
	if dupGroupSize(t, database, gA.Int64) != 4 {
		t.Fatalf("merged group size = %d, want 4", dupGroupSize(t, database, gA.Int64))
	}
	groups := countRows(t, database, "dup_groups")
	if groups != 1 {
		t.Fatalf("dup_groups rows = %d, want 1", groups)
	}
}

func TestAddDuplicateConflictsWithVersion(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddVersionEdge(t, svc, a, b)
	if err := svc.AddDuplicate(a, b); !errors.Is(err, ErrRelationConflict) {
		t.Fatalf("AddDuplicate after version: got %v, want ErrRelationConflict", err)
	}
}

func TestAddRefusesPairsOnOneChainPath(t *testing.T) {
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
			if err := tc.link(svc, a, b); err != nil {
				t.Fatalf("link a-b: %v", err)
			}
			if err := tc.link(svc, b, c); err != nil {
				t.Fatalf("link b-c: %v", err)
			}
			if err := svc.AddNotRelated(a, c); !errors.Is(err, ErrIndirectRelation) {
				t.Fatalf("AddNotRelated(a, c): got %v, want ErrIndirectRelation", err)
			}
			if err := svc.AddDuplicate(a, c); !errors.Is(err, ErrIndirectRelation) {
				t.Fatalf("AddDuplicate(a, c): got %v, want ErrIndirectRelation", err)
			}
			n := countRows(t, database, "not_related_pairs")
			if n != 0 {
				t.Fatalf("not_related_pairs rows = %d, want 0 on a live path", n)
			}
		})
	}
}

func TestAddEdgeSweepsThePairsItAnswers(t *testing.T) {
	queue := func(t *testing.T, database *db.DB, a, b int64) {
		t.Helper()
		lo, hi := canonicalPair(a, b)
		mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, 3, ?)`, lo, hi, nowISO())
	}
	queued := func(t *testing.T, database *db.DB, a, b int64) bool {
		t.Helper()
		lo, hi := canonicalPair(a, b)
		n := countRows(t, database, "potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?", lo, hi)
		return n > 0
	}

	database, svc := setupTestDB(t)
	root := insertImage(t, database, "root", 100)
	mid := insertImage(t, database, "mid", 200)
	leaf := insertImage(t, database, "leaf", 300)
	sib := insertImage(t, database, "sib", 400)
	mustAddDerivativeEdge(t, svc, root, mid)
	mustAddDerivativeEdge(t, svc, mid, sib)

	queue(t, database, root, leaf)
	queue(t, database, leaf, sib)

	mustAddDerivativeEdge(t, svc, mid, leaf)
	if queued(t, database, root, leaf) {
		t.Fatal("root-leaf still queued after leaf joined the tree below root")
	}
	if !queued(t, database, leaf, sib) {
		t.Fatal("leaf-sib was swept; siblings share no path and stay queued")
	}
}

func TestMergeGroupsSweepsThePairsItAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		add   func(*testing.T, *Service, int64, int64)
		table string
		merge func(*Service, []int64) error
	}{
		{"duplicate", mustAddDuplicate, "dup_group_members",
			func(svc *Service, ids []int64) error { return svc.MergeDupGroups(ids, 0) }},
		{"alternate", mustAddAlternate, "alt_group_members", (*Service).MergeAltGroups},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, svc := setupTestDB(t)
			a := insertImage(t, database, "a", 100)
			b := insertImage(t, database, "b", 200)
			c := insertImage(t, database, "c", 300)
			d := insertImage(t, database, "d", 400)
			outside := insertImage(t, database, "e", 500)
			tc.add(t, svc, a, b)
			tc.add(t, svc, c, d)
			for _, pair := range [][2]int64{{a, c}, {b, d}, {a, outside}} {
				mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, 3, ?)`, pair[0], pair[1], nowISO())
			}
			groups, err := db.QueryIDs(database.Read, `SELECT DISTINCT group_id FROM `+tc.table)
			if err != nil {
				t.Fatal(err)
			}

			if err := tc.merge(svc, groups); err != nil {
				t.Fatal(err)
			}
			if n := countRows(t, database, "potential_relation_pairs WHERE b_image_id != ?", outside); n != 0 {
				t.Errorf("%d pair(s) the merge answers are still queued", n)
			}
			if n := countRows(t, database, "potential_relation_pairs WHERE b_image_id = ?", outside); n != 1 {
				t.Error("the pair with an image outside the groups was swept")
			}
		})
	}
}

func TestAddAcceptsSiblingPairs(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "parent", 100)
	sibA := insertImage(t, database, "sibA", 200)
	sibB := insertImage(t, database, "sibB", 300)
	mustAddDerivativeEdge(t, svc, parent, sibA)
	mustAddDerivativeEdge(t, svc, parent, sibB)
	if err := svc.AddNotRelated(sibA, sibB); err != nil {
		t.Fatalf("AddNotRelated(siblings): %v", err)
	}
}

func TestAddAlternateConflictsWithDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddDuplicate(t, svc, a, b)
	if err := svc.AddAlternate(a, b); !errors.Is(err, ErrRelationConflict) {
		t.Fatalf("AddAlternate after duplicate: got %v, want ErrRelationConflict", err)
	}
}

func TestAddVersionEdgeRefusesSecondParent(t *testing.T) {
	database, svc := setupTestDB(t)
	parent1 := insertImage(t, database, "p1", 100)
	parent2 := insertImage(t, database, "p2", 100)
	child := insertImage(t, database, "c", 100)
	mustAddVersionEdge(t, svc, parent1, child)
	if err := svc.AddVersionEdge(parent2, child); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("second parent: got %v, want ErrVersionExists", err)
	}
}

func TestAddVersionEdgeRefusesSecondChild(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "p", 100)
	c1 := insertImage(t, database, "c1", 100)
	c2 := insertImage(t, database, "c2", 100)
	mustAddVersionEdge(t, svc, parent, c1)
	if err := svc.AddVersionEdge(parent, c2); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("second child: got %v, want ErrVersionExists", err)
	}
}

func TestAddVersionEdgeAllowsSuccessiveChain(t *testing.T) {
	database, svc := setupTestDB(t)
	x := insertImage(t, database, "x", 100)
	y := insertImage(t, database, "y", 100)
	z := insertImage(t, database, "z", 100)
	mustAddVersionEdge(t, svc, x, y)
	mustAddVersionEdge(t, svc, y, z)
}

func TestAddVersionEdgeRejectsChainPastDepthCap(t *testing.T) {
	database, svc := setupTestDB(t)
	ids := make([]int64, MaxVersionChainDepth+2)
	for i := range ids {
		ids[i] = insertImage(t, database, fmt.Sprintf("vd%02d", i), 100)
	}
	for i := 0; i < MaxVersionChainDepth; i++ {
		mustAddVersionEdge(t, svc, ids[i], ids[i+1])
	}
	if err := svc.AddVersionEdge(ids[MaxVersionChainDepth], ids[MaxVersionChainDepth+1]); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("edge past cap: got %v, want ErrChainTooDeep", err)
	}
}

func TestAddVersionEdgeRejectsJoiningChainsPastDepthCap(t *testing.T) {
	database, svc := setupTestDB(t)
	build := func(prefix string, n int) []int64 {
		ids := make([]int64, n)
		for i := range ids {
			ids[i] = insertImage(t, database, fmt.Sprintf("%s%02d", prefix, i), 100)
		}
		for i := 0; i < n-1; i++ {
			mustAddVersionEdge(t, svc, ids[i], ids[i+1])
		}
		return ids
	}
	a := build("ja", 9)
	b := build("jb", 9)
	if err := svc.AddVersionEdge(a[len(a)-1], b[0]); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("joining edge: got %v, want ErrChainTooDeep", err)
	}
}

func TestAddDerivativeEdgeRejectsDepthPastCap(t *testing.T) {
	database, svc := setupTestDB(t)
	ids := make([]int64, MaxVersionChainDepth+2)
	for i := range ids {
		ids[i] = insertImage(t, database, fmt.Sprintf("dd%02d", i), 100)
	}
	for i := 0; i < MaxVersionChainDepth; i++ {
		mustAddDerivativeEdge(t, svc, ids[i], ids[i+1])
	}
	if err := svc.AddDerivativeEdge(ids[MaxVersionChainDepth], ids[MaxVersionChainDepth+1]); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("edge past cap: got %v, want ErrChainTooDeep", err)
	}
}

func TestAddDerivativeEdge_OffAWideTree(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)

	const width = 16384
	if _, err := database.Write.Exec(
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		 INSERT INTO images (sha256, canonical_path, file_type, file_size)
		 SELECT 'leaf' || i, '/gallery/leaf' || i || '.png', 'png', 100 FROM n`, width); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`INSERT INTO derivative_edges (source_image_id, derivative_image_id)
		 SELECT ?, id FROM images WHERE sha256 LIKE 'leaf%'`, src); err != nil {
		t.Fatal(err)
	}
	leafID := func(n int) int64 {
		var id int64
		if err := database.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, fmt.Sprintf("leaf%d", n)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	x := insertImage(t, database, "x", 100)
	if err := svc.AddDerivativeEdge(x, src); err != nil {
		t.Fatalf("AddDerivativeEdge onto the wide source: %v", err)
	}
	if err := svc.ReverseDerivativeEdge(src, leafID(1)); err != nil {
		t.Fatalf("ReverseDerivativeEdge off the wide source: %v", err)
	}
	y := insertImage(t, database, "y", 100)
	if err := svc.AddNotRelated(y, leafID(2)); err != nil {
		t.Fatal(err)
	}
	var cross *CrossConflictError
	if err := svc.AddDerivativeEdge(y, src); !errors.As(err, &cross) {
		t.Fatalf("AddDerivativeEdge over a not_related leaf = %v, want a CrossConflictError", err)
	}
}

func TestOverwriteVersionKeepsThirdImageEdges(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	e := insertImage(t, database, "e", 100)

	mustAddVersionEdge(t, svc, a, b)
	mustAddVersionEdge(t, svc, c, e)
	if err := svc.Overwrite("version", a, c); err != nil {
		t.Fatalf("Overwrite: %v", err)
	}
	count := countRows(t, database, "version_edges WHERE child_image_id = ? AND parent_image_id = ?", b, a)
	if count != 0 {
		t.Fatalf("a->b row count = %d, want 0 (was the conflict)", count)
	}
	count = countRows(t, database, "version_edges WHERE child_image_id = ? AND parent_image_id = ?", e, c)
	if count != 1 {
		t.Fatalf("c->e row count = %d, want 1 (unrelated third-image edge)", count)
	}
	if n := countRows(t, database, "version_edges WHERE child_image_id = ? AND parent_image_id = ?", c, a); n != 1 {
		t.Fatalf("a->c row count = %d, want 1 (the overwrite's edge)", n)
	}
}

func TestAddDuplicateOfTakesTheOriginalFromTheDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "o", 5000)
	dup := insertImage(t, database, "l", 3000)
	other := insertImage(t, database, "r", 1000)
	mustAddDuplicate(t, svc, dup, other)

	if err := svc.AddDuplicateOf(original, dup); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{original, dup, other} {
		if _, got := dupGroupOf(t, database, id); got.Int64 != original {
			t.Errorf("image %d: group original = %d, want %d", id, got.Int64, original)
		}
	}
}

func TestAddDuplicateOfKeepsAThirdOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	third := insertImage(t, database, "t", 5000)
	dup := insertImage(t, database, "r", 3000)
	original := insertImage(t, database, "l", 1000)
	mustAddDuplicate(t, svc, third, dup)

	if err := svc.AddDuplicateOf(original, dup); err != nil {
		t.Fatal(err)
	}
	if _, got := dupGroupOf(t, database, original); got.Int64 != third {
		t.Errorf("group original = %d, want the third image %d", got.Int64, third)
	}
}

func TestAddDuplicateRefusesAJoinThatRelatesAnotherPairTwice(t *testing.T) {
	database, svc := setupTestDB(t)
	x := insertImage(t, database, "x", 100)
	y := insertImage(t, database, "y", 100)
	z := insertImage(t, database, "z", 100)
	mustAddDuplicate(t, svc, x, y)
	if err := svc.AddNotRelated(x, z); err != nil {
		t.Fatal(err)
	}

	var cross *CrossConflictError
	if err := svc.AddDuplicate(y, z); !errors.As(err, &cross) {
		t.Fatalf("AddDuplicate = %v, want a CrossConflictError", err)
	}
	if g, _ := dupGroupOf(t, database, z); g.Valid {
		t.Error("z joined the group despite the refusal")
	}
}

func TestMergeDupGroupsRefusesAPairWithAnotherRelation(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	d := insertImage(t, database, "d", 100)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, c, d)
	mustAddAlternate(t, svc, a, c)
	g1, _ := dupGroupOf(t, database, a)
	g2, _ := dupGroupOf(t, database, c)

	var cross *CrossConflictError
	if err := svc.MergeDupGroups([]int64{g1.Int64, g2.Int64}, 0); !errors.As(err, &cross) {
		t.Fatalf("MergeDupGroups = %v, want a CrossConflictError", err)
	}
	if after, _ := dupGroupOf(t, database, c); after.Int64 != g2.Int64 {
		t.Error("the refused merge moved c's group")
	}
}

func TestAddVersionEdgeRefusesPuttingARelatedPairOnOnePath(t *testing.T) {
	database, svc := setupTestDB(t)
	p := insertImage(t, database, "p", 100)
	a := insertImage(t, database, "a", 100)
	c := insertImage(t, database, "c", 100)
	mustAddVersionEdge(t, svc, p, a)
	mustAddAlternate(t, svc, p, c)

	var cross *CrossConflictError
	if err := svc.AddVersionEdge(a, c); !errors.As(err, &cross) {
		t.Fatalf("AddVersionEdge = %v, want a CrossConflictError", err)
	}
	if n := countRows(t, database, "version_edges WHERE parent_image_id = ? AND child_image_id = ?", a, c); n != 0 {
		t.Error("the refused edge was stored")
	}
}

func TestOverwriteRefusedKeepsTheOldRelation(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)

	cur := a
	for i := range MaxVersionChainDepth {
		above := insertImage(t, database, fmt.Sprintf("above_%d", i), 100)
		mustAddVersionEdge(t, svc, above, cur)
		cur = above
	}
	mustAddAlternate(t, svc, a, b)

	if err := svc.Overwrite("version", a, b); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("Overwrite = %v, want ErrChainTooDeep", err)
	}
	if gA, gB := altGroupOf(t, database, a), altGroupOf(t, database, b); !gA.Valid || gA.Int64 != gB.Int64 {
		t.Error("the refused overwrite dissolved the alternate pair")
	}
}

func TestAddVersionEdgeRejectsCycle(t *testing.T) {
	database, svc := setupTestDB(t)
	x := insertImage(t, database, "x", 100)
	y := insertImage(t, database, "y", 100)
	z := insertImage(t, database, "z", 100)
	mustAddVersionEdge(t, svc, x, y)
	mustAddVersionEdge(t, svc, y, z)
	if err := svc.AddVersionEdge(z, x); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("z->x (cycle): got %v, want ErrVersionExists", err)
	}
}

func TestAddVersionEdgeIdempotentSameEdge(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "p", 100)
	child := insertImage(t, database, "c", 100)
	mustAddVersionEdge(t, svc, parent, child)
	if err := svc.AddVersionEdge(parent, child); err != nil {
		t.Errorf("idempotent re-add: got %v, want nil", err)
	}
	n := countRows(t, database, "version_edges")
	if n != 1 {
		t.Errorf("version_edges = %d, want 1", n)
	}
}

func TestAddDerivativeEdgeIdempotentSameEdge(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)
	d := insertImage(t, database, "d", 100)
	mustAddDerivativeEdge(t, svc, src, d)
	if err := svc.AddDerivativeEdge(src, d); err != nil {
		t.Errorf("idempotent re-add: got %v, want nil", err)
	}
	n := countRows(t, database, "derivative_edges")
	if n != 1 {
		t.Errorf("derivative_edges = %d, want 1", n)
	}
}

func TestAddDerivativeEdgeAllowsMultipleDerivatives(t *testing.T) {
	database, svc := setupTestDB(t)
	source := insertImage(t, database, "src", 100)
	d1 := insertImage(t, database, "d1", 100)
	d2 := insertImage(t, database, "d2", 100)
	mustAddDerivativeEdge(t, svc, source, d1)
	mustAddDerivativeEdge(t, svc, source, d2)
	n := countRows(t, database, "derivative_edges WHERE source_image_id = ?", source)
	if n != 2 {
		t.Fatalf("derivative count = %d, want 2", n)
	}
}

func TestAddDerivativeEdgeAllowsMultipleSources(t *testing.T) {
	database, svc := setupTestDB(t)
	s1 := insertImage(t, database, "s1", 100)
	s2 := insertImage(t, database, "s2", 100)
	s3 := insertImage(t, database, "s3", 100)
	d := insertImage(t, database, "d", 100)
	for _, s := range []int64{s1, s2, s3} {
		mustAddDerivativeEdge(t, svc, s, d)
	}
	got, err := db.QueryIDs(database.Read,
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ? ORDER BY source_image_id`, d)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int64{s1, s2, s3}) {
		t.Fatalf("sources = %v, want %v", got, []int64{s1, s2, s3})
	}
}

func TestAddDerivativeEdgeAcrossADiamond(t *testing.T) {
	database, svc := setupTestDB(t)
	top := insertImage(t, database, "top", 100)
	left := insertImage(t, database, "left", 100)
	right := insertImage(t, database, "right", 100)
	bottom := insertImage(t, database, "bottom", 100)
	for _, e := range []struct{ src, der int64 }{
		{top, left}, {top, right}, {left, bottom}, {right, bottom},
	} {
		mustAddDerivativeEdge(t, svc, e.src, e.der)
	}
	if err := svc.AddDuplicate(top, bottom); !errors.Is(err, ErrIndirectRelation) {
		t.Fatalf("top/bottom: got %v, want ErrIndirectRelation", err)
	}
	if err := svc.DissolveDerivativeTree(bottom); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	if n := countRows(t, database, "derivative_edges"); n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

func TestAddDerivativeEdgeRejectsCycle(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	mustAddDerivativeEdge(t, svc, a, b)
	mustAddDerivativeEdge(t, svc, b, c)
	if err := svc.AddDerivativeEdge(c, a); !errors.Is(err, ErrDerivativeCycle) {
		t.Fatalf("c->a (cycle): got %v, want ErrDerivativeCycle", err)
	}
}

func TestAddDerivativeEdgeRejectsCycleThroughASecondSource(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	mid := insertImage(t, database, "mid", 100)
	for _, e := range []struct{ src, der int64 }{{a, mid}, {mid, b}} {
		mustAddDerivativeEdge(t, svc, e.src, e.der)
	}
	if err := svc.AddDerivativeEdge(b, a); !errors.Is(err, ErrDerivativeCycle) {
		t.Fatalf("b->a (cycle): got %v, want ErrDerivativeCycle", err)
	}
}

func TestAddNotRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddNotRelated(b, a); err != nil {
		t.Fatalf("AddNotRelated: %v", err)
	}
	var lo, hi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM not_related_pairs`).Scan(&lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo != a || hi != b {
		t.Fatalf("not_related_pairs canonical = (%d, %d), want (%d, %d)", lo, hi, a, b)
	}
	if err := svc.AddDuplicate(a, b); !errors.Is(err, ErrRelationConflict) {
		t.Fatalf("AddDuplicate after not_related: got %v, want ErrRelationConflict", err)
	}
}

func TestDuplicateDoesNotMergeAlternates(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	d := insertImage(t, database, "d", 100)
	mustAddAlternate(t, svc, a, c)
	mustAddAlternate(t, svc, b, d)
	mustAddDuplicate(t, svc, a, b)
	dA, _ := dupGroupOf(t, database, a)
	dB, _ := dupGroupOf(t, database, b)
	if !dA.Valid || dA.Int64 != dB.Int64 {
		t.Fatalf("a and b should share a dup group: a=%v b=%v", dA, dB)
	}
	gA, gB := altGroupOf(t, database, a), altGroupOf(t, database, b)
	gC, gD := altGroupOf(t, database, c), altGroupOf(t, database, d)
	if gA.Int64 != gC.Int64 || gB.Int64 != gD.Int64 {
		t.Fatalf("alt memberships should be untouched: a=%d c=%d b=%d d=%d", gA.Int64, gC.Int64, gB.Int64, gD.Int64)
	}
	if gA.Int64 == gB.Int64 {
		t.Fatal("a and b must not share an alt group (they are duplicates, not alternates)")
	}
}

func TestOverwriteAlternateWithDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	mustAddAlternate(t, svc, a, b)
	mustAddAlternate(t, svc, a, c)
	if err := svc.Overwrite("duplicate", a, b); err != nil {
		t.Fatalf("Overwrite: %v", err)
	}
	dA, _ := dupGroupOf(t, database, a)
	dB, _ := dupGroupOf(t, database, b)
	if !dA.Valid || dA.Int64 != dB.Int64 {
		t.Fatal("a and b should share a dup group")
	}
	if altGroupOf(t, database, b).Valid {
		t.Fatal("b must not be in an alt group after being overwritten to a duplicate of a")
	}
	if altGroupOf(t, database, a).Int64 != altGroupOf(t, database, c).Int64 {
		t.Fatal("c should remain an alternate of a")
	}
}

func TestOnImageDeletePromotesOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	mid := insertImage(t, database, "mid", 3000)
	small := insertImage(t, database, "small", 1000)
	mustAddDuplicate(t, svc, big, mid)
	mustAddDuplicate(t, svc, mid, small)

	onImagesDelete(t, database, svc, []int64{big})
	mustExec(t, database.Write, `DELETE FROM images WHERE id = ?`, big)
	gid, orig := dupGroupOf(t, database, mid)
	if !gid.Valid {
		t.Fatal("mid lost its group")
	}
	if orig.Int64 != mid {
		t.Fatalf("promoted original = %d, want %d (largest remaining)", orig.Int64, mid)
	}
}

func TestOnImageDeleteDissolvesSingletonGroup(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddDuplicate(t, svc, a, b)
	onImagesDelete(t, database, svc, []int64{a})
	mustExec(t, database.Write, `DELETE FROM images WHERE id = ?`, a)
	gid, _ := dupGroupOf(t, database, b)
	if gid.Valid {
		t.Fatalf("b should not be in any group, got %d", gid.Int64)
	}
	groups := countRows(t, database, "dup_groups")
	if groups != 0 {
		t.Fatalf("dup_groups remaining = %d, want 0", groups)
	}
}

func TestOnImagesDeletePromotesPastTheWholeChunk(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	mid := insertImage(t, database, "mid", 3000)
	small := insertImage(t, database, "small", 1000)
	tiny := insertImage(t, database, "tiny", 500)
	for _, pair := range [][2]int64{{big, mid}, {mid, small}, {small, tiny}} {
		mustAddDuplicate(t, svc, pair[0], pair[1])
	}
	leaving := []int64{big, mid}
	onImagesDelete(t, database, svc, leaving)
	mustExec(t, database.Write, `DELETE FROM images WHERE id IN (?, ?)`, big, mid)
	gid, orig := dupGroupOf(t, database, small)
	if !gid.Valid {
		t.Fatal("small lost its group")
	}
	if orig.Int64 != small {
		t.Fatalf("promoted original = %d, want %d (largest survivor)", orig.Int64, small)
	}
}

func TestOnImagesDeleteDissolvesWholeGroup(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	mustAddDuplicate(t, svc, a, b)
	mustAddAlternate(t, svc, a, c)
	ids := []int64{a, b, c}
	onImagesDelete(t, database, svc, ids)
	mustExec(t, database.Write, `DELETE FROM images WHERE id IN (?, ?, ?)`, a, b, c)
	for _, tbl := range []string{"dup_groups", "alt_groups"} {
		n := countRows(t, database, tbl)
		if n != 0 {
			t.Fatalf("%s remaining = %d, want 0", tbl, n)
		}
	}
}

func TestImageDeleteCascadesEdges(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "p", 100)
	child := insertImage(t, database, "c", 100)
	mustAddVersionEdge(t, svc, parent, child)

	mustExec(t, database.Write, `DELETE FROM images WHERE id = ?`, parent)
	n := countRows(t, database, "version_edges")
	if n != 0 {
		t.Fatalf("version edges remaining = %d, want 0", n)
	}
}

func TestImageDeleteCascadesNotRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddNotRelated(a, b); err != nil {
		t.Fatalf("AddNotRelated: %v", err)
	}
	mustExec(t, database.Write, `DELETE FROM images WHERE id = ?`, a)
	n := countRows(t, database, "not_related_pairs")
	if n != 0 {
		t.Fatalf("not_related_pairs remaining = %d, want 0", n)
	}
}

func TestImageDeleteBlockedWithoutHook(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddDuplicate(t, svc, a, b)

	_, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, a)
	if err == nil {
		t.Fatal("expected FK error deleting an original without OnImagesDeleteTx first")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") && !strings.Contains(err.Error(), "constraint failed") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestRemoveDupMemberIdempotent(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	if err := svc.RemoveDupMember(a); err != nil {
		t.Fatalf("RemoveDupMember(non-member): %v", err)
	}
}

func TestRemoveDupMemberDissolvesPair(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddDuplicate(t, svc, a, b)
	if err := svc.RemoveDupMember(a); err != nil {
		t.Fatalf("RemoveDupMember: %v", err)
	}
	if gid, _ := dupGroupOf(t, database, b); gid.Valid {
		t.Fatalf("group survived; b still in group %d", gid.Int64)
	}
	groups := countRows(t, database, "dup_groups")
	if groups != 0 {
		t.Fatalf("dup_groups remaining = %d, want 0", groups)
	}
}

func TestRemoveDupMemberPromotesOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	mid := insertImage(t, database, "mid", 3000)
	small := insertImage(t, database, "small", 1000)
	mustAddDuplicate(t, svc, big, mid)
	mustAddDuplicate(t, svc, big, small)

	if err := svc.RemoveDupMember(big); err != nil {
		t.Fatalf("RemoveDupMember: %v", err)
	}
	_, orig := dupGroupOf(t, database, mid)
	if orig.Int64 != mid {
		t.Fatalf("new original = %d, want %d", orig.Int64, mid)
	}
}

func TestDissolveDupGroup(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, b, c)
	gid, _ := dupGroupOf(t, database, a)
	if err := svc.DissolveDupGroup(gid.Int64); err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	for _, id := range []int64{a, b, c} {
		if g, _ := dupGroupOf(t, database, id); g.Valid {
			t.Fatalf("image %d still in group %d", id, g.Int64)
		}
	}
}

func TestPromoteToOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	small := insertImage(t, database, "small", 100)
	mustAddDuplicate(t, svc, big, small)
	gid, orig := dupGroupOf(t, database, big)
	if orig.Int64 != big {
		t.Fatalf("starting original = %d, want %d", orig.Int64, big)
	}
	if err := svc.PromoteToOriginal(gid.Int64, small); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	_, orig2 := dupGroupOf(t, database, small)
	if orig2.Int64 != small {
		t.Fatalf("promoted original = %d, want %d", orig2.Int64, small)
	}
}

func TestPromoteToOriginalNonMember(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	mustAddDuplicate(t, svc, a, b)
	gid, _ := dupGroupOf(t, database, a)
	if err := svc.PromoteToOriginal(gid.Int64, c); !errors.Is(err, ErrNotInGroup) {
		t.Fatalf("Promote non-member: got %v, want ErrNotInGroup", err)
	}
}

func TestRemoveAltMember(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	mustAddAlternate(t, svc, a, b)
	mustAddAlternate(t, svc, b, c)
	if err := svc.RemoveAltMember(b); err != nil {
		t.Fatal(err)
	}
	if altGroupOf(t, database, b).Valid {
		t.Fatal("b still in alt group")
	}

	if !altGroupOf(t, database, a).Valid || !altGroupOf(t, database, c).Valid {
		t.Fatal("a or c lost alt membership unexpectedly")
	}
}

func TestRemoveEdge(t *testing.T) {
	for _, c := range []struct {
		name, table string
		add, remove func(svc *Service, a, b int64) error
	}{
		{"version", "version_edges",
			func(svc *Service, a, b int64) error { return svc.AddVersionEdge(a, b) },
			func(svc *Service, a, b int64) error { return svc.RemoveVersionEdge(a, b) }},
		{"derivative", "derivative_edges",
			func(svc *Service, a, b int64) error { return svc.AddDerivativeEdge(a, b) },
			func(svc *Service, a, b int64) error { return svc.RemoveDerivativeEdge(a, b) }},
	} {
		for _, swapped := range []bool{false, true} {
			name := c.name
			if swapped {
				name += "/swapped sides"
			}
			t.Run(name, func(t *testing.T) {
				database, svc := setupTestDB(t)
				a := insertImage(t, database, "a", 100)
				b := insertImage(t, database, "b", 100)
				if err := c.add(svc, a, b); err != nil {
					t.Fatal(err)
				}
				left, right := a, b
				if swapped {
					left, right = b, a
				}
				if err := c.remove(svc, left, right); err != nil {
					t.Fatal(err)
				}
				n := countRows(t, database, c.table)
				if n != 0 {
					t.Fatalf("%s remaining = %d, want 0", c.table, n)
				}

				if err := c.remove(svc, left, right); err != nil {
					t.Fatalf("idempotent: %v", err)
				}
			})
		}
	}
}

func TestReverseVersionEdgeMidChainRaisesTypedError(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	mustAddVersionEdge(t, svc, a, b)
	mustAddVersionEdge(t, svc, b, c)

	err := svc.ReverseVersionEdge(a, b)
	if !errors.Is(err, ErrVersionExists) {
		t.Fatalf("ReverseVersionEdge mid-chain expected ErrVersionExists, got %v", err)
	}

	n := countRows(t, database, "version_edges")
	if n != 2 {
		t.Fatalf("version_edges = %d after failed reverse, want 2", n)
	}
}

func TestReverseVersionEdgeFlipsATwoMemberChain(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	mustAddVersionEdge(t, svc, a, b)
	if err := svc.ReverseVersionEdge(a, b); err != nil {
		t.Fatalf("ReverseVersionEdge: %v", err)
	}
	var parent, child int64
	if err := database.Read.QueryRow(`SELECT parent_image_id, child_image_id FROM version_edges`).Scan(&parent, &child); err != nil {
		t.Fatal(err)
	}
	if parent != b || child != a {
		t.Fatalf("edge = %d -> %d, want %d -> %d", parent, child, b, a)
	}
	if n := countRows(t, database, "version_edges"); n != 1 {
		t.Fatalf("version_edges = %d, want 1", n)
	}
}

func TestReverseDerivativeEdgeMidTreeAddsASecondSource(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	for _, e := range []struct{ src, der int64 }{{a, b}, {c, a}} {
		mustAddDerivativeEdge(t, svc, e.src, e.der)
	}
	if err := svc.ReverseDerivativeEdge(a, b); err != nil {
		t.Fatalf("ReverseDerivativeEdge: %v", err)
	}
	got, err := db.QueryIDs(database.Read,
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ? ORDER BY source_image_id`, a)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{b, c}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sources of a = %v, want %v", got, want)
	}
}

func TestReverseDerivativeEdgeRefusesALoop(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	mid := insertImage(t, database, "mid", 100)
	for _, e := range []struct{ src, der int64 }{{a, b}, {a, mid}, {mid, b}} {
		mustAddDerivativeEdge(t, svc, e.src, e.der)
	}
	if err := svc.ReverseDerivativeEdge(a, b); !errors.Is(err, ErrDerivativeCycle) {
		t.Fatalf("ReverseDerivativeEdge: got %v, want ErrDerivativeCycle", err)
	}
	if n := countRows(t, database, "derivative_edges"); n != 3 {
		t.Fatalf("derivative_edges = %d after failed reverse, want 3", n)
	}
}

func TestReverseDerivativeEdgeRefusesACrossConflict(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	e := insertImage(t, database, "e", 100)
	f := insertImage(t, database, "f", 100)
	for _, edge := range []struct{ src, der int64 }{{a, b}, {e, b}, {a, f}} {
		mustAddDerivativeEdge(t, svc, edge.src, edge.der)
	}
	if err := svc.AddNotRelated(e, f); err != nil {
		t.Fatal(err)
	}

	var cross *CrossConflictError
	if err := svc.ReverseDerivativeEdge(a, b); !errors.As(err, &cross) {
		t.Fatalf("ReverseDerivativeEdge: got %v, want a CrossConflictError", err)
	}
	if n := countRows(t, database, "derivative_edges WHERE source_image_id = ? AND derivative_image_id = ?", a, b); n != 1 {
		t.Fatalf("the refused reverse left a -> b in %d rows, want 1", n)
	}
}

func TestRemoveNotRelatedMatchesANonCanonicalRow(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustExec(t, database.Write, `INSERT INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`, b, a, "2026-01-01T00:00:00Z")

	if err := svc.RemoveNotRelated(a, b); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, "not_related_pairs"); n != 0 {
		t.Fatalf("not_related_pairs remaining = %d, want 0", n)
	}
}

func TestRemoveNotRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddNotRelated(a, b); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveNotRelated(b, a); err != nil {
		t.Fatal(err)
	}
	n := countRows(t, database, "not_related_pairs")
	if n != 0 {
		t.Fatalf("not_related_pairs remaining = %d, want 0", n)
	}

	mustAddDuplicate(t, svc, a, b)
}

func TestCopyTagsFromDuplicatesToOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 10000)
	dup := insertImage(t, database, "dup", 100)
	tagID := insertTagRow(t, database, "cat")
	if err := tags.New(database, nil).AddTagToImage(dup, tagID, false, nil); err != nil {
		t.Fatal(err)
	}

	var explicitID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tags WHERE name = 'explicit' AND category_id = (SELECT id FROM tag_categories WHERE name = 'rating')`,
	).Scan(&explicitID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, dup, explicitID)

	mustAddDuplicate(t, svc, original, dup)
	gid, _ := dupGroupOf(t, database, original)
	added, err := svc.CopyTagsFromDuplicatesToOriginal(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (rating excluded)", added)
	}
	hasGeneral := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", original, tagID)
	if hasGeneral != 1 {
		t.Fatal("original did not pick up the general tag")
	}
	hasRating := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", original, explicitID)
	if hasRating != 0 {
		t.Fatal("original picked up rating tag; rating exclusion broken")
	}

	var usage int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, tagID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 2 {
		t.Fatalf("usage_count = %d, want 2 (dup + original)", usage)
	}

	added2, err := svc.CopyTagsFromDuplicatesToOriginal(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if added2 != 0 {
		t.Fatalf("second run added = %d, want 0", added2)
	}
}

func TestTagsToCopy_LeavesOutImpliedAndRatingRows(t *testing.T) {
	database, svc := setupTestDB(t)
	tagSvc := tags.New(database, nil)
	var generalID, explicitID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	if err := database.Read.QueryRow(
		`SELECT id FROM tags WHERE name = 'explicit' AND category_id = (SELECT id FROM tag_categories WHERE name = 'rating')`,
	).Scan(&explicitID); err != nil {
		t.Fatal(err)
	}
	cat, _ := tagSvc.GetOrCreateTag("cat", generalID)
	stray, _ := tagSvc.GetOrCreateTag("stray_implied", generalID)

	orig1 := insertImage(t, database, "orig1", 10000)
	dup1 := insertImage(t, database, "dup1", 100)
	mustAddDuplicate(t, svc, orig1, dup1)
	gid1, original1 := dupGroupOf(t, database, orig1)
	member1 := dup1
	if original1.Int64 == dup1 {
		member1 = orig1
	}
	if err := tagSvc.AddTagToImage(member1, cat.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?, ?, 1)`, member1, stray.ID)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, member1, explicitID)

	orig2 := insertImage(t, database, "orig2", 10000)
	dup2 := insertImage(t, database, "dup2", 100)
	mustAddDuplicate(t, svc, orig2, dup2)
	gid2, original2 := dupGroupOf(t, database, orig2)
	member2 := dup2
	if original2.Int64 == dup2 {
		member2 = orig2
	}
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?, ?, 1)`, member2, stray.ID)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, member2, explicitID)

	ids, err := svc.TagsToCopy(gid1.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != cat.ID {
		t.Errorf("TagsToCopy = %v, want only the user tag %d", ids, cat.ID)
	}
	if ids, err := svc.TagsToCopy(gid2.Int64); err != nil || len(ids) != 0 {
		t.Errorf("TagsToCopy of the implied-and-rating group = %v (%v), want none", ids, err)
	}
	withTags, err := svc.GroupsWithTagsToCopy([]int64{gid1.Int64, gid2.Int64})
	if err != nil {
		t.Fatal(err)
	}
	if !withTags[gid1.Int64] || withTags[gid2.Int64] {
		t.Errorf("GroupsWithTagsToCopy = %v, want only group %d", withTags, gid1.Int64)
	}
}

func TestTagsToCopy_LeavesOutTagsOnlyTheMetaDerivationClaims(t *testing.T) {
	database, svc := setupTestDB(t)
	tagSvc := tags.New(database, nil)
	var metaID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'meta'`).Scan(&metaID); err != nil {
		t.Fatal(err)
	}
	derived, _ := tagSvc.GetOrCreateTag("animated_png", metaID)
	typed, _ := tagSvc.GetOrCreateTag("highres", metaID)
	original := insertImage(t, database, "orig", 10000)
	dup := insertImage(t, database, "dup", 100)
	mustAddDuplicate(t, svc, original, dup)
	gid, orig := dupGroupOf(t, database, original)
	member := dup
	if orig.Int64 == dup {
		member = original
	}
	for _, id := range []int64{derived.ID, typed.ID} {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, tagger_name) VALUES (?, ?, 'monbooru')`, member, id)
		mustExec(t, database.Write, `INSERT INTO image_tag_sources (image_id, tag_id, source) VALUES (?, ?, 'monbooru')`, member, id)
	}
	mustExec(t, database.Write, `INSERT INTO image_tag_sources (image_id, tag_id, source) VALUES (?, ?, 'user')`, member, typed.ID)

	ids, err := svc.TagsToCopy(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != typed.ID {
		t.Errorf("TagsToCopy = %v, want only the tag the operator also claimed (%d)", ids, typed.ID)
	}
	added, err := svc.CopyTagsFromDuplicatesToOriginal(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", orig.Int64, derived.ID) != 0 {
		t.Errorf("added = %d; the derived-only tag must stay off the original", added)
	}
}

func TestCopyTagsFromDuplicatesToOriginal_RecordsTheOperatorsClaimAndFansOut(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 10000)
	dup := insertImage(t, database, "dup", 100)
	tagSvc := tags.New(database, nil)
	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	cat, _ := tagSvc.GetOrCreateTag("cat", generalID)
	animal, _ := tagSvc.GetOrCreateTag("animal", generalID)
	if _, err := tagSvc.AddImplication(cat.ID, animal.ID); err != nil {
		t.Fatal(err)
	}
	if err := tagSvc.AddTagToImage(dup, cat.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	mustAddDuplicate(t, svc, original, dup)
	gid, _ := dupGroupOf(t, database, original)
	added, err := svc.CopyTagsFromDuplicatesToOriginal(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1: the implied row is not copied as the operator's", added)
	}
	row := func(tagID int64) (isImplied int, sources string) {
		t.Helper()
		if err := database.Read.QueryRow(
			`SELECT it.is_implied, COALESCE((SELECT group_concat(source) FROM image_tag_sources s
			                                  WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id), '')
			 FROM image_tags it WHERE it.image_id = ? AND it.tag_id = ?`, original, tagID,
		).Scan(&isImplied, &sources); err != nil {
			t.Fatalf("tag %d on the original: %v", tagID, err)
		}
		return isImplied, sources
	}
	if isImplied, sources := row(cat.ID); isImplied != 0 || sources != "user" {
		t.Errorf("copied row: is_implied=%d sources=%q, want 0 / user", isImplied, sources)
	}
	if isImplied, sources := row(animal.ID); isImplied != 1 || sources != "" {
		t.Errorf("implied row: is_implied=%d sources=%q, want 1 / none", isImplied, sources)
	}
}

func TestAddDuplicateSweepsCoGroupedQueueRows(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	for _, pair := range [][2]int64{{a, b}, {a, c}, {b, c}} {
		seedQueuePair(t, database, pair[0], pair[1])
	}
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, b, c)
	rest := countRows(t, database, "potential_relation_pairs")
	if rest != 0 {
		t.Fatalf("queue rows remaining = %d, want 0 (all three pairs co-grouped)", rest)
	}
}

func TestAddEdgeDropsQueuePair(t *testing.T) {
	database, svc := setupTestDB(t)
	seed := func(a, b int64) { seedQueuePair(t, database, a, b) }
	remaining := func(a, b int64) int { return queuedPairCount(t, database, a, b) }

	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	seed(a, b)
	mustAddDerivativeEdge(t, svc, a, b)
	if n := remaining(a, b); n != 0 {
		t.Fatalf("derivative queue rows remaining = %d, want 0", n)
	}

	c := insertImage(t, database, "c", 300)
	d := insertImage(t, database, "d", 400)
	seed(c, d)
	mustAddVersionEdge(t, svc, c, d)
	if n := remaining(c, d); n != 0 {
		t.Fatalf("version queue rows remaining = %d, want 0", n)
	}
}

func TestReAddEdgeDropsStaleQueuePair(t *testing.T) {
	database, svc := setupTestDB(t)
	seed := func(a, b int64) { seedQueuePair(t, database, a, b) }
	remaining := func(a, b int64) int { return queuedPairCount(t, database, a, b) }

	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddDerivativeEdge(t, svc, a, b)
	seed(a, b)
	mustAddDerivativeEdge(t, svc, a, b)
	if n := remaining(a, b); n != 0 {
		t.Fatalf("derivative queue rows remaining = %d, want 0", n)
	}

	c := insertImage(t, database, "c", 300)
	d := insertImage(t, database, "d", 400)
	mustAddVersionEdge(t, svc, c, d)
	seed(c, d)
	mustAddVersionEdge(t, svc, c, d)
	if n := remaining(c, d); n != 0 {
		t.Fatalf("version queue rows remaining = %d, want 0", n)
	}
}

func TestAddNotRelatedDropsQueuePair(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	seedQueuePair(t, database, a, b)

	if err := svc.AddNotRelated(a, b); err != nil {
		t.Fatalf("AddNotRelated: %v", err)
	}
	n := countRows(t, database, "potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?", a, b)
	if n != 0 {
		t.Fatalf("queue rows remaining = %d, want 0", n)
	}
}

func TestNextOriginalIfRemovedPicksBiggest(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 5000)
	c := insertImage(t, database, "c", 3000)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, b, c)
	gid, _ := dupGroupOf(t, database, a)

	next, err := svc.NextOriginalIfRemoved(gid.Int64, a)
	if err != nil {
		t.Fatal(err)
	}
	if next != b {
		t.Fatalf("next original = %d, want %d (largest remaining)", next, b)
	}

	if err := svc.RemoveDupMember(c); err != nil {
		t.Fatalf("RemoveDupMember c: %v", err)
	}
	next, err = svc.NextOriginalIfRemoved(gid.Int64, a)
	if err != nil {
		t.Fatal(err)
	}
	if next != 0 {
		t.Fatalf("next original on 2-member group = %d, want 0", next)
	}
}

func TestAddDuplicateAutoMergesDistinctGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 500)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 600)
	d := insertImage(t, database, "d", 250)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, c, d)
	gAB, origAB := dupGroupOf(t, database, a)
	gCD, _ := dupGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: two distinct dup groups expected")
	}

	mustAddDuplicate(t, svc, b, d)
	gPost, origPost := dupGroupOf(t, database, a)
	gPostD, _ := dupGroupOf(t, database, d)
	if !gPost.Valid || !gPostD.Valid || gPost.Int64 != gPostD.Int64 {
		t.Fatalf("after auto-merge: a in %v, d in %v; expected same group", gPost, gPostD)
	}
	if dupGroupSize(t, database, gPost.Int64) != 4 {
		t.Errorf("merged dup group size = %d, want 4", dupGroupSize(t, database, gPost.Int64))
	}
	if origPost.Int64 != origAB.Int64 {
		t.Errorf("original_image_id after merge = %d, want %d (survivor kept)", origPost.Int64, origAB.Int64)
	}
	groups := countRows(t, database, "dup_groups")
	if groups != 1 {
		t.Errorf("dup_groups rows after merge = %d, want 1", groups)
	}
}

func TestAddAlternateAutoMergesDistinctGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	d := insertImage(t, database, "d", 250)
	mustAddAlternate(t, svc, a, b)
	mustAddAlternate(t, svc, c, d)
	gAB := altGroupOf(t, database, a)
	gCD := altGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: two distinct alt groups expected")
	}
	mustAddAlternate(t, svc, b, d)
	gA := altGroupOf(t, database, a)
	gD := altGroupOf(t, database, d)
	if !gA.Valid || !gD.Valid || gA.Int64 != gD.Int64 {
		t.Fatalf("after auto-merge: a alt = %v, d alt = %v; expected same group", gA, gD)
	}
	rowCount := countRows(t, database, "alt_groups")
	if rowCount != 1 {
		t.Errorf("alt_groups rows after merge = %d, want 1", rowCount)
	}
	memberCount := countRows(t, database, "alt_group_members WHERE group_id = ?", gA.Int64)
	if memberCount != 4 {
		t.Errorf("merged alt group size = %d, want 4", memberCount)
	}
}

func TestMergeAltGroupsCombinesTwoGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	d := insertImage(t, database, "d", 250)
	mustAddAlternate(t, svc, a, b)
	mustAddAlternate(t, svc, c, d)
	gAB := altGroupOf(t, database, a)
	gCD := altGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: expected two distinct alt groups, got %v / %v", gAB, gCD)
	}
	if err := svc.MergeAltGroups([]int64{gAB.Int64, gCD.Int64}); err != nil {
		t.Fatalf("MergeAltGroups: %v", err)
	}
	gA := altGroupOf(t, database, a)
	gD := altGroupOf(t, database, d)
	if !gA.Valid || !gD.Valid || gA.Int64 != gD.Int64 {
		t.Fatalf("after merge: a alt = %v, d alt = %v; expected the same group", gA, gD)
	}
	rowCount := countRows(t, database, "alt_groups")
	if rowCount != 1 {
		t.Errorf("alt_groups rows = %d, want 1", rowCount)
	}
	memberCount := countRows(t, database, "alt_group_members WHERE group_id = ?", gA.Int64)
	if memberCount != 4 {
		t.Errorf("merged alt group size = %d, want 4", memberCount)
	}
}

func TestMergeAltGroupsSingleGroupNoOp(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	mustAddAlternate(t, svc, a, b)
	gid := altGroupOf(t, database, a)
	if !gid.Valid {
		t.Fatal("alt group missing")
	}
	if err := svc.MergeAltGroups([]int64{gid.Int64}); err != nil {
		t.Fatalf("MergeAltGroups single: %v", err)
	}
	post := altGroupOf(t, database, a)
	if !post.Valid || post.Int64 != gid.Int64 {
		t.Errorf("single-group merge changed membership: pre=%v post=%v", gid, post)
	}
}

func TestMergeDupGroupsKeepsSurvivorOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 500)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 600)
	d := insertImage(t, database, "d", 250)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, c, d)
	gAB, origAB := dupGroupOf(t, database, a)
	gCD, origCD := dupGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: two distinct dup groups expected")
	}
	if origAB.Int64 != a {
		t.Fatalf("origAB = %d, want %d", origAB.Int64, a)
	}
	if origCD.Int64 != c {
		t.Fatalf("origCD = %d, want %d", origCD.Int64, c)
	}
	if err := svc.MergeDupGroups([]int64{gAB.Int64, gCD.Int64}, 0); err != nil {
		t.Fatalf("MergeDupGroups: %v", err)
	}
	gPost, origPost := dupGroupOf(t, database, a)
	gPostD, _ := dupGroupOf(t, database, d)
	if !gPost.Valid || !gPostD.Valid || gPost.Int64 != gPostD.Int64 {
		t.Fatalf("post-merge: a in %v, d in %v; expected the same group", gPost, gPostD)
	}
	if dupGroupSize(t, database, gPost.Int64) != 4 {
		t.Errorf("merged dup group size = %d, want 4", dupGroupSize(t, database, gPost.Int64))
	}
	if origPost.Int64 != a {
		t.Errorf("post-merge original = %d, want %d (survivor kept)", origPost.Int64, a)
	}
}

func TestMergeDupGroupsKeepOriginalFromNonSurvivor(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 500)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 600)
	d := insertImage(t, database, "d", 250)
	mustAddDuplicate(t, svc, a, b)
	mustAddDuplicate(t, svc, c, d)
	gAB, _ := dupGroupOf(t, database, a)
	gCD, _ := dupGroupOf(t, database, c)
	if err := svc.MergeDupGroups([]int64{gAB.Int64, gCD.Int64}, gCD.Int64); err != nil {
		t.Fatalf("MergeDupGroups: %v", err)
	}
	_, origPost := dupGroupOf(t, database, a)
	if origPost.Int64 != c {
		t.Errorf("post-merge original = %d, want %d (inherited from %d)", origPost.Int64, c, gCD.Int64)
	}
}

func TestDissolveVersionChain(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	mustAddVersionEdge(t, svc, a, b)
	mustAddVersionEdge(t, svc, b, c)

	if err := svc.DissolveVersionChain(b); err != nil {
		t.Fatalf("DissolveVersionChain: %v", err)
	}
	n := countRows(t, database, "version_edges")
	if n != 0 {
		t.Fatalf("version_edges remaining = %d, want 0", n)
	}
}

func TestDissolveVersionChainNoEdges(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.DissolveVersionChain(42); err != nil {
		t.Fatalf("DissolveVersionChain on missing chain: %v", err)
	}
}

func TestDissolveDerivativeTree(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)
	d1 := insertImage(t, database, "d1", 110)
	d2 := insertImage(t, database, "d2", 120)
	d1a := insertImage(t, database, "d1a", 130)
	mustAddDerivativeEdge(t, svc, src, d1)
	mustAddDerivativeEdge(t, svc, src, d2)
	mustAddDerivativeEdge(t, svc, d1, d1a)

	if err := svc.DissolveDerivativeTree(d1a); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	n := countRows(t, database, "derivative_edges")
	if n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

func TestDissolveDerivativeTreeAcrossSeveralRoots(t *testing.T) {
	database, svc := setupTestDB(t)
	s1 := insertImage(t, database, "s1", 100)
	s2 := insertImage(t, database, "s2", 110)
	shared := insertImage(t, database, "shared", 120)
	leaf := insertImage(t, database, "leaf", 130)
	for _, e := range []struct{ src, der int64 }{{s1, shared}, {s2, shared}, {shared, leaf}} {
		mustAddDerivativeEdge(t, svc, e.src, e.der)
	}
	if err := svc.DissolveDerivativeTree(s1); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	if n := countRows(t, database, "derivative_edges"); n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

func TestDissolveDerivativeTreeNoEdges(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.DissolveDerivativeTree(42); err != nil {
		t.Fatalf("DissolveDerivativeTree on missing tree: %v", err)
	}
}

func TestDissolveDerivativeTreeWideFanout(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)

	const fanout = 300
	for i := 0; i < fanout; i++ {
		d := insertImage(t, database, fmt.Sprintf("d%03d", i), int64(200+i))
		mustAddDerivativeEdge(t, svc, src, d)
	}
	if err := svc.DissolveDerivativeTree(src); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	n := countRows(t, database, "derivative_edges")
	if n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

func TestDissolveDerivativeTreePastTheVariableCap(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)

	const fanout = 16384
	mustExec(t, database.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size)
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		SELECT 'cap' || n, '/cap/' || n || '.png', 'png', 1 FROM seq`, fanout)
	mustExec(t, database.Write, `INSERT INTO derivative_edges (source_image_id, derivative_image_id)
		SELECT ?, id FROM images WHERE sha256 LIKE 'cap%'`, src)

	if err := svc.DissolveDerivativeTree(src); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	if n := countRows(t, database, "derivative_edges"); n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

func seedQueuePair(t *testing.T, database *db.DB, a, b int64) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, 0, ?)`, a, b, "now")
}

func queuedPairCount(t *testing.T, database *db.DB, a, b int64) int {
	t.Helper()
	n := countRows(t, database, "potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?", a, b)
	return n
}

func mustAddDuplicate(t *testing.T, svc *Service, a, b int64) {
	t.Helper()
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate(%d, %d): %v", a, b, err)
	}
}

func mustAddAlternate(t *testing.T, svc *Service, a, b int64) {
	t.Helper()
	if err := svc.AddAlternate(a, b); err != nil {
		t.Fatalf("AddAlternate(%d, %d): %v", a, b, err)
	}
}

func mustAddVersionEdge(t *testing.T, svc *Service, parent, child int64) {
	t.Helper()
	if err := svc.AddVersionEdge(parent, child); err != nil {
		t.Fatalf("AddVersionEdge(%d -> %d): %v", parent, child, err)
	}
}

func mustAddDerivativeEdge(t *testing.T, svc *Service, source, derivative int64) {
	t.Helper()
	if err := svc.AddDerivativeEdge(source, derivative); err != nil {
		t.Fatalf("AddDerivativeEdge(%d -> %d): %v", source, derivative, err)
	}
}

func mustFindTagPairs(t *testing.T, ctx context.Context, database *db.DB, threshold float64, progress FindPairsProgress) int {
	t.Helper()
	n, err := findTagPairs(ctx, database, visibleImages(t, database), threshold, progress)
	if err != nil {
		t.Fatalf("findTagPairs: %v", err)
	}
	return n
}

func mustFindPairs(t *testing.T, ctx context.Context, database *db.DB, tree *BKTree, opts FindPairsOptions, progress FindPairsProgress) int {
	t.Helper()
	n, err := FindPairs(ctx, database, tree, opts, progress)
	if err != nil {
		t.Fatalf("FindPairs: %v", err)
	}
	return n
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func visibleImages(t *testing.T, database *db.DB) int {
	t.Helper()
	return countRows(t, database, "images WHERE is_missing = 0")
}
