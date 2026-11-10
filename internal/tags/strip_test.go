package tags

import (
	"context"
	"fmt"
	"testing"
)

func usageOf(t *testing.T, svc *Service, id int64) int {
	t.Helper()
	return mustGetTag(t, svc, id).UsageCount
}

func TestStripTags_KeepsUsageInStepAcrossChunks(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	user := mkTagID(t, svc, "strip_user", general)
	kept := mkTagID(t, svc, "strip_kept", general)
	var ids []int64
	for i := range 600 {
		id := insertTestImage(t, database, fmt.Sprintf("strip_%d", i))
		mustAddTag(t, svc, id, user, false, nil)
		if err := svc.AddTagsToImageFromTagger(id, []int64{kept}, true, "wd"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var calls []int
	res, err := svc.StripTags(context.Background(), ids[:550], StripSpec{Mode: "user"}, func(done, _ int) { calls = append(calls, done) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 550 || res.Removed != 550 || res.Cancelled {
		t.Fatalf("result %+v, want 550 processed and removed", res)
	}
	if fmt.Sprint(calls) != "[500 550]" {
		t.Errorf("progress calls %v, want one per chunk", calls)
	}
	if got := usageOf(t, svc, user); got != 50 {
		t.Errorf("stripped tag usage = %d, want 50", got)
	}
	if got := usageOf(t, svc, kept); got != 600 {
		t.Errorf("auto tag usage = %d, want 600 (user mode leaves it)", got)
	}
}

func TestStripTags_LeavesAMissingImageOutOfTheCount(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	tag := mkTagID(t, svc, "strip_missing", general)
	var ids []int64
	for _, sha := range []string{"sm_a", "sm_b", "sm_c"} {
		id := insertTestImage(t, database, sha)
		mustAddTag(t, svc, id, tag, false, nil)
		ids = append(ids, id)
	}
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, ids[1])
	mustExec(t, database.Write, `UPDATE tags SET usage_count = 2 WHERE id = ?`, tag)

	if _, err := svc.StripTags(context.Background(), ids[:2], StripSpec{Mode: "all"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, svc, tag); got != 1 {
		t.Errorf("usage = %d, want 1: only the visible stripped image counted", got)
	}
}

func TestStripTags_SweepsTheImpliedRowsLeftWithoutAParent(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mkTagID(t, svc, "strip_parent", general)
	child := mkTagID(t, svc, "strip_child", general)
	mustAddImply(t, svc, parent, child)
	id := insertTestImage(t, database, "strip_implied")
	mustAddTag(t, svc, id, parent, false, nil)

	res, err := svc.StripTags(context.Background(), []int64{id}, StripSpec{Mode: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 2 {
		t.Errorf("removed %d rows, want the user row and its orphaned implied row", res.Removed)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ?", id); n != 0 {
		t.Errorf("%d rows left on the image, want 0", n)
	}
	if got := usageOf(t, svc, child); got != 0 {
		t.Errorf("implied tag usage = %d, want 0", got)
	}
}

func TestStripTags_NarrowsBySourceAndTagger(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	id := insertTestImage(t, database, "strip_narrow")
	danb := mkTagID(t, svc, "strip_danbooru", general)
	gelb := mkTagID(t, svc, "strip_gelbooru", general)
	wd := mkTagID(t, svc, "strip_wd", general)
	joy := mkTagID(t, svc, "strip_joy", general)
	mustSyncSourceTags(t, svc, id, []int64{danb}, "danbooru", false)
	mustSyncSourceTags(t, svc, id, []int64{gelb}, "gelbooru", false)
	for tagger, tag := range map[string]int64{"wd": wd, "joy": joy} {
		if err := svc.AddTagsToImageFromTagger(id, []int64{tag}, true, tagger); err != nil {
			t.Fatal(err)
		}
	}
	for _, spec := range []StripSpec{{Mode: "source", Name: "danbooru"}, {Mode: "auto", Name: "wd"}} {
		if _, err := svc.StripTags(context.Background(), []int64{id}, spec, nil); err != nil {
			t.Fatal(err)
		}
	}
	for tag, want := range map[int64]int{danb: 0, gelb: 1, wd: 0, joy: 1} {
		if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", id, tag); n != want {
			t.Errorf("tag %d rows = %d, want %d", tag, n, want)
		}
	}
}

func TestStripTags_RefusesAnUnknownModeAndStopsWhenCancelled(t *testing.T) {
	database, svc := setupTestDB(t)
	id := insertTestImage(t, database, "strip_cancel")
	if _, err := svc.StripTags(context.Background(), []int64{id}, StripSpec{Mode: "bogus"}, nil); err == nil {
		t.Error("an unknown mode was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := svc.StripTags(ctx, []int64{id}, StripSpec{Mode: "all"}, nil)
	if err != nil || !res.Cancelled || res.Processed != 0 {
		t.Errorf("cancelled strip = %+v (%v), want cancelled before any chunk", res, err)
	}
}

func autoAndManualParents(t *testing.T, svc *Service, imageID int64) (child int64) {
	t.Helper()
	general := generalCategoryID(t, svc)
	autoParent := mkTagID(t, svc, "cat_ears", general)
	manualParent := mkTagID(t, svc, "fox_ears", general)
	child = mkTagID(t, svc, "animal_ears", general)
	mustAddImply(t, svc, autoParent, child)
	mustAddImply(t, svc, manualParent, child)
	if err := svc.AddTagsToImageFromTagger(imageID, []int64{autoParent}, true, "wd"); err != nil {
		t.Fatal(err)
	}
	mustAddTag(t, svc, imageID, manualParent, false, nil)
	return child
}

func TestStripTags_AutoKeepsTheImpliedRowsAManualParentJustifies(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "strip_auto_implied")
	child := autoAndManualParents(t, svc, imageID)

	res, err := svc.StripTags(context.Background(), []int64{imageID}, StripSpec{Mode: "auto"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Errorf("removed = %d, want 1 (the tagger's own row)", res.Removed)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imageID, child); n != 1 {
		t.Errorf("implied child rows = %d, want 1", n)
	}
	if got := usageOf(t, svc, child); got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
}

func TestRemoveAutoTagsFromImage_KeepsTheImpliedRowsAManualParentJustifies(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "remove_auto_implied")
	child := autoAndManualParents(t, svc, imageID)

	removed, err := svc.RemoveAutoTagsFromImage(imageID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (the tagger's own row)", removed)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imageID, child); n != 1 {
		t.Errorf("implied child rows = %d, want 1", n)
	}
	if got := usageOf(t, svc, child); got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
}

func TestStripTags_UserDemotesAnOwnedRowAParentImplies(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID, _, child := ownedChildUnderParent(t, database, svc, "demote_strip", true)
	if _, err := svc.StripTags(context.Background(), []int64{imageID}, StripSpec{Mode: "user"}, nil); err != nil {
		t.Fatal(err)
	}
	assertDemoted(t, database, svc, imageID, child)
}
