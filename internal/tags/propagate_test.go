package tags

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func TestPropagateImplication_FansOutAcrossChunksWithoutARecount(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mkTagID(t, svc, "prop_parent", general)
	child := mkTagID(t, svc, "prop_child", general)
	for i := range 600 {
		mustAddTag(t, svc, insertTestImage(t, database, fmt.Sprintf("prop_%d", i)), parent, false, nil)
	}
	mustAddImply(t, svc, parent, child)

	var calls []int
	p, err := svc.PropagateImplication(context.Background(), parent, func(done, _ int) { calls = append(calls, done) })
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Walked) != 600 || p.Total != 600 || p.Cancelled {
		t.Fatalf("walk reached %d of %d (cancelled %t), want all 600", len(p.Walked), p.Total, p.Cancelled)
	}
	if fmt.Sprint(calls) != "[0 500 600]" {
		t.Errorf("progress calls %v, want a start and one per chunk", calls)
	}
	if n := countRows(t, database, "image_tags WHERE tag_id = ? AND is_implied = 1", child); n != 600 {
		t.Errorf("implied rows = %d, want 600", n)
	}
	if got := mustGetTag(t, svc, child).UsageCount; got != 600 {
		t.Errorf("child usage = %d, want 600", got)
	}
}

func TestSweepImplicationRemoval_KeepsRowsAnotherParentJustifies(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	p1 := mkTagID(t, svc, "sweep_p1", general)
	p2 := mkTagID(t, svc, "sweep_p2", general)
	child := mkTagID(t, svc, "sweep_child", general)
	mustAddImply(t, svc, p1, child)
	mustAddImply(t, svc, p2, child)
	alone := insertTestImage(t, database, "sweep_alone")
	both := insertTestImage(t, database, "sweep_both")
	mustAddTag(t, svc, alone, p1, false, nil)
	mustAddTag(t, svc, both, p1, false, nil)
	mustAddTag(t, svc, both, p2, false, nil)
	if err := svc.RemoveImplication(p1, child); err != nil {
		t.Fatal(err)
	}

	closure, err := svc.ImplicationClosure(child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SweepImplicationRemoval(context.Background(), p1, closure, nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", alone, child); n != 0 {
		t.Error("the row only the removed edge justified stayed")
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", both, child); n != 1 {
		t.Error("the row another parent justifies went")
	}
	if got := mustGetTag(t, svc, child).UsageCount; got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
}

func TestImplicationClosure_StartsWithTheTag(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	a := mkTagID(t, svc, "clo_a", general)
	b := mkTagID(t, svc, "clo_b", general)
	c := mkTagID(t, svc, "clo_c", general)
	mustAddImply(t, svc, a, b)
	mustAddImply(t, svc, b, c)
	got, err := svc.ImplicationClosure(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != a || !slices.Contains(got, b) || !slices.Contains(got, c) {
		t.Errorf("closure = %v, want %d first, then %d and %d", got, a, b, c)
	}
}

func TestPropagateImplication_StopsWhenCancelled(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mkTagID(t, svc, "cancel_parent", general)
	mustAddTag(t, svc, insertTestImage(t, database, "cancel_img"), parent, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := svc.PropagateImplication(ctx, parent, nil)
	if err == nil && (!p.Cancelled || len(p.Walked) != 0) {
		t.Errorf("walk %+v, want it stopped before the first chunk", p)
	}
}
