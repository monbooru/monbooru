package tags

import (
	"context"
	"fmt"
	"testing"
)

func seedBatchImages(t *testing.T, svc *Service, n int, prefix string) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = insertTestImage(t, svc.db, fmt.Sprintf("%s_%d", prefix, i))
	}
	return ids
}

func TestBatchAddTags_TakesSmallerChunksWhenATagImplies(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	plain := mkTagID(t, svc, "batch_plain", general)
	parent := mkTagID(t, svc, "batch_parent", general)
	child := mkTagID(t, svc, "batch_child", general)
	mustAddImply(t, svc, parent, child)
	ids := seedBatchImages(t, svc, 600, "batch")

	for _, tc := range []struct {
		tag  int64
		want string
	}{{plain, "[500 600]"}, {parent, "[100 200 300 400 500 600]"}} {
		var calls []int
		res, err := svc.BatchAddTags(context.Background(), ids, []int64{tc.tag}, func(done, _ int) { calls = append(calls, done) })
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != tc.want {
			t.Errorf("tag %d: progress %v, want %s", tc.tag, calls, tc.want)
		}
		if res.Processed != 600 || res.Changed != 600 {
			t.Errorf("tag %d: result %+v, want 600 processed and added", tc.tag, res)
		}
	}
	for tag, want := range map[int64]int{plain: 600, parent: 600, child: 600} {
		if got := mustGetTag(t, svc, tag).UsageCount; got != want {
			t.Errorf("tag %d usage = %d, want %d", tag, got, want)
		}
	}
}

func TestBatchRemoveTags_CountsRowsAParentKeeps(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mkTagID(t, svc, "rm_parent", general)
	child := mkTagID(t, svc, "rm_child", general)
	other := mkTagID(t, svc, "rm_other", general)
	mustAddImply(t, svc, parent, child)
	ids := seedBatchImages(t, svc, 3, "rm")
	for _, id := range ids {
		mustAddTag(t, svc, id, parent, false, nil)
		mustAddTag(t, svc, id, other, false, nil)
	}
	res, err := svc.BatchRemoveTags(context.Background(), ids, []int64{child, other}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed != 3 || res.Kept != 3 {
		t.Errorf("result %+v, want 3 removed and 3 kept", res)
	}
	if got := mustGetTag(t, svc, other).UsageCount; got != 0 {
		t.Errorf("removed tag usage = %d, want 0", got)
	}
}

func TestBatchRemoveTags_CountsTheImpliedRowsThatLeaveWithIt(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mkTagID(t, svc, "rmc_parent", general)
	child := mkTagID(t, svc, "rmc_child", general)
	grandchild := mkTagID(t, svc, "rmc_grandchild", general)
	mustAddImply(t, svc, parent, child)
	mustAddImply(t, svc, child, grandchild)
	ids := seedBatchImages(t, svc, 3, "rmc")
	for _, id := range ids {
		mustAddTag(t, svc, id, parent, false, nil)
	}
	res, err := svc.BatchRemoveTags(context.Background(), ids, []int64{parent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed != 9 {
		t.Errorf("changed = %d, want 9 (the parent and the two implied rows on each image)", res.Changed)
	}
}

func TestBatchAddTags_CountsTheImagesItReRates(t *testing.T) {
	database, svc := setupTestDB(t)
	ids := seedBatchImages(t, svc, 2, "rerate")
	mustAddTag(t, svc, ids[0], ratingTagIDByName(t, database, "general"), false, nil)
	res, err := svc.BatchAddTags(context.Background(), ids, []int64{ratingTagIDByName(t, database, "explicit")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replaced != 1 {
		t.Errorf("replaced = %d, want the one image that had a rating", res.Replaced)
	}
}

func TestBatchRemoveTags_DemotesAndCountsAnOwnedRowAParentImplies(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID, _, child := ownedChildUnderParent(t, database, svc, "demote_batch", false)
	res, err := svc.BatchRemoveTags(context.Background(), []int64{imageID}, []int64{child}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed != 0 || res.Kept != 1 {
		t.Errorf("BatchRemoveTags = %d changed, %d kept; want 0, 1", res.Changed, res.Kept)
	}
	assertDemoted(t, database, svc, imageID, child)
}
