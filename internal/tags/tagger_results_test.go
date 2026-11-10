package tags

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func applyHits(t *testing.T, svc *Service, imageID int64, hits ...TaggerHit) {
	t.Helper()
	if err := svc.ApplyTaggerResults(context.Background(), imageID, hits, []string{"tagger-A"}); err != nil {
		t.Fatalf("ApplyTaggerResults: %v", err)
	}
}

func tagNamesOnImage(t *testing.T, svc *Service, imageID int64) []string {
	t.Helper()
	_, rows := mustImageTags(t, svc, imageID)
	var names []string
	for _, r := range rows {
		names = append(names, r.TagName)
	}
	slices.Sort(names)
	return names
}

func TestApplyTaggerResults_DropsOnlyTheRunsOwnRows(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	imageID := insertTestImage(t, database, "sr1")
	manual := mkTagID(t, svc, "manual_tag", general)
	stale := mkTagID(t, svc, "stale_auto_a", general)
	other := mkTagID(t, svc, "keep_auto_b", general)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 0, NULL)`, imageID, manual)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, 'tagger-A')`, imageID, stale)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, 'tagger-B')`, imageID, other)

	applyHits(t, svc, imageID, TaggerHit{Name: "fresh_auto_a", CategoryID: general, Score: 0.9, Tagger: "tagger-A"})

	if got, want := tagNamesOnImage(t, svc, imageID), []string{"fresh_auto_a", "keep_auto_b", "manual_tag"}; !slices.Equal(got, want) {
		t.Errorf("tags after the run = %v, want %v", got, want)
	}
	if n := countRows(t, database, "images WHERE id = ? AND auto_tagged_at IS NOT NULL", imageID); n != 1 {
		t.Error("auto_tagged_at is not stamped")
	}
	fresh := mustTag(t, svc, "fresh_auto_a", general).ID
	if got := ledgerSources(t, database, imageID, fresh); !slices.Equal(got, []string{"tagger-A"}) {
		t.Errorf("ledger for the new row = %v, want [tagger-A]", got)
	}
	if got := ledgerSources(t, database, imageID, stale); len(got) != 0 {
		t.Errorf("the dropped row keeps ledger entries %v", got)
	}
}

func TestApplyTaggerResults_StoresTheTaggerNameOrNull(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	imageID := insertTestImage(t, database, "tr_names")
	refreshed := mkTagID(t, svc, "refreshed_auto", general)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, 'tagger-A')`, imageID, refreshed)

	applyHits(t, svc, imageID,
		TaggerHit{Name: "named_auto", CategoryID: general, Score: 0.9, Tagger: "tagger-A"},
		TaggerHit{Name: "unnamed_auto", CategoryID: general, Score: 0.8},
		TaggerHit{Name: "refreshed_auto", CategoryID: general, Score: 0.7})

	if got := imageTagTagger(t, database, imageID, mustTag(t, svc, "named_auto", general).ID); got != "tagger-A" {
		t.Errorf("inserted row tagger_name = %q, want tagger-A", got)
	}
	for _, name := range []string{"unnamed_auto", "refreshed_auto"} {
		id := mustTag(t, svc, name, general).ID
		if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND tagger_name IS NULL", imageID, id); n != 1 {
			t.Errorf("%s tagger_name = %q, want NULL for a hit with no tagger", name, imageTagTagger(t, database, imageID, id))
		}
	}
}

func TestApplyTaggerResults_KeepsTheHighestRating(t *testing.T) {
	database, svc := setupTestDB(t)
	rating := svc.RatingCategoryID()
	imageID := insertTestImage(t, database, "sr2")
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, 'tagger-A')`,
		imageID, ratingTagIDByName(t, database, "general"))

	applyHits(t, svc, imageID,
		TaggerHit{Name: "explicit", CategoryID: rating, Score: 0.95, Tagger: "tagger-A"},
		TaggerHit{Name: "sensitive", CategoryID: rating, Score: 0.6, Tagger: "tagger-A"})

	if got := ratingNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"explicit"}) {
		t.Errorf("ratings = %v, want [explicit] only", got)
	}
}

func TestApplyTaggerResults_NeverCreatesARatingTag(t *testing.T) {
	database, svc := setupTestDB(t)
	rating := svc.RatingCategoryID()
	imageID := insertTestImage(t, database, "sr-level")

	applyHits(t, svc, imageID,
		TaggerHit{Name: "rating_safe", CategoryID: rating, Score: 0.9, Tagger: "tagger-A"},
		TaggerHit{Name: "general", CategoryID: rating, Score: 0.4, Tagger: "tagger-A"})

	if n := countRows(t, database, "tags WHERE name = 'rating_safe'"); n != 0 {
		t.Errorf("rating_safe rows = %d, want no new tag", n)
	}
	if got := ratingNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"general"}) {
		t.Errorf("ratings = %v, want [general]", got)
	}
}

func TestApplyTaggerResults_ParentAndChildBothOwned(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mustTag(t, svc, "cat_ears", general)
	child := mustTag(t, svc, "animal_ears", general)
	mustAddImply(t, svc, parent.ID, child.ID)
	hits := []TaggerHit{
		{Name: "cat_ears", CategoryID: general, Score: 0.9, Tagger: "tagger-A"},
		{Name: "animal_ears", CategoryID: general, Score: 0.8, Tagger: "tagger-A"},
	}

	for i := range 20 {
		imageID := insertTestImage(t, database, fmt.Sprintf("both_owned_%d", i))
		slices.Reverse(hits)
		applyHits(t, svc, imageID, hits...)
		var isImplied int
		var confidence *float64
		if err := database.Read.QueryRow(
			`SELECT is_implied, confidence FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, child.ID,
		).Scan(&isImplied, &confidence); err != nil {
			t.Fatal(err)
		}
		if isImplied != 0 || confidence == nil {
			t.Fatalf("run %d: child is_implied=%d confidence=%v, want the tagger's own row", i, isImplied, confidence)
		}
	}
}

func TestApplyTaggerResults_FansOutAnEmittedParent(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	parent := mustTag(t, svc, "fox_ears", general)
	child := mustTag(t, svc, "animal_ears", general)
	mustAddImply(t, svc, parent.ID, child.ID)
	imageID := insertTestImage(t, database, "fan_out")

	applyHits(t, svc, imageID, TaggerHit{Name: "fox_ears", CategoryID: general, Score: 0.9, Tagger: "tagger-A"})

	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imageID, child.ID); n != 1 {
		t.Errorf("implied rows for the child = %d, want 1", n)
	}
	if got := mustGetTag(t, svc, child.ID).UsageCount; got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
}

func impliedChildRow(t *testing.T, svc *Service, parent, child string) (general, parentID, childID int64) {
	t.Helper()
	general = generalCategoryID(t, svc)
	parentID, childID = mkTagID(t, svc, parent, general), mkTagID(t, svc, child, general)
	mustAddImply(t, svc, parentID, childID)
	return general, parentID, childID
}

func childState(t *testing.T, svc *Service, imageID, childID int64) (isImplied int, tagger *string) {
	t.Helper()
	if err := svc.db.Read.QueryRow(
		`SELECT is_implied, tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, childID,
	).Scan(&isImplied, &tagger); err != nil {
		t.Fatalf("child row: %v", err)
	}
	return isImplied, tagger
}

func TestApplyTaggerResults_KeepsAChildItsParentStillImplies(t *testing.T) {
	database, svc := setupTestDB(t)
	general, _, child := impliedChildRow(t, svc, "cat_ears", "animal_ears")
	imageID := insertTestImage(t, database, "keep_child")

	applyHits(t, svc, imageID,
		TaggerHit{Name: "cat_ears", CategoryID: general, Score: 0.9, Tagger: "tagger-A"},
		TaggerHit{Name: "animal_ears", CategoryID: general, Score: 0.8, Tagger: "tagger-A"})
	applyHits(t, svc, imageID, TaggerHit{Name: "cat_ears", CategoryID: general, Score: 0.9, Tagger: "tagger-A"})

	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", imageID, child); n != 1 {
		t.Fatalf("child rows = %d, want 1", n)
	}
	if isImplied, tagger := childState(t, svc, imageID, child); isImplied != 1 || tagger != nil {
		t.Errorf("child is_implied=%d tagger=%v, want an implied row with no tagger", isImplied, tagger)
	}
	if got := mustGetTag(t, svc, child).UsageCount; got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
	if got := ledgerSources(t, database, imageID, child); len(got) != 0 {
		t.Errorf("implied child keeps ledger entries %v", got)
	}
}

func TestApplyTaggerResults_DropsTheChildrenOfADroppedParent(t *testing.T) {
	database, svc := setupTestDB(t)
	general, _, child := impliedChildRow(t, svc, "cat_ears", "animal_ears")
	imageID := insertTestImage(t, database, "drop_child")

	applyHits(t, svc, imageID, TaggerHit{Name: "cat_ears", CategoryID: general, Score: 0.9, Tagger: "tagger-A"})
	applyHits(t, svc, imageID, TaggerHit{Name: "smile", CategoryID: general, Score: 0.9, Tagger: "tagger-A"})

	if got, want := tagNamesOnImage(t, svc, imageID), []string{"smile"}; !slices.Equal(got, want) {
		t.Errorf("tags after the run = %v, want %v", got, want)
	}
	if got := mustGetTag(t, svc, child).UsageCount; got != 0 {
		t.Errorf("child usage = %d, want 0", got)
	}
}

func TestApplyTaggerResults_ReEmittingAnImpliedRowOwnsIt(t *testing.T) {
	database, svc := setupTestDB(t)
	general, _, child := impliedChildRow(t, svc, "cat_ears", "animal_ears")
	imageID := insertTestImage(t, database, "own_child")
	parentHit := TaggerHit{Name: "cat_ears", CategoryID: general, Score: 0.9, Tagger: "tagger-A"}

	applyHits(t, svc, imageID, parentHit)
	applyHits(t, svc, imageID, parentHit, TaggerHit{Name: "animal_ears", CategoryID: general, Score: 0.8, Tagger: "tagger-A"})
	if isImplied, tagger := childState(t, svc, imageID, child); isImplied != 0 || tagger == nil || *tagger != "tagger-A" {
		t.Fatalf("re-emitted child is_implied=%d tagger=%v, want the tagger's own row", isImplied, tagger)
	}

	applyHits(t, svc, imageID, parentHit)
	if isImplied, tagger := childState(t, svc, imageID, child); isImplied != 1 || tagger != nil {
		t.Errorf("child is_implied=%d tagger=%v after the tagger stops emitting it, want implied", isImplied, tagger)
	}
	if got := mustGetTag(t, svc, child).UsageCount; got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
}

func TestApplyTaggerResults_ReEmittingARowAManualParentImpliesOwnsIt(t *testing.T) {
	database, svc := setupTestDB(t)
	general, parent, child := impliedChildRow(t, svc, "cat_ears", "animal_ears")
	imageID := insertTestImage(t, database, "own_manual_child")
	mustAddTag(t, svc, imageID, parent, false, nil)

	applyHits(t, svc, imageID, TaggerHit{Name: "animal_ears", CategoryID: general, Score: 0.8, Tagger: "tagger-A"})
	if isImplied, tagger := childState(t, svc, imageID, child); isImplied != 0 || tagger == nil || *tagger != "tagger-A" {
		t.Fatalf("re-emitted child is_implied=%d tagger=%v, want the tagger's own row", isImplied, tagger)
	}

	if _, err := svc.RemoveTagFromImage(imageID, parent); err != nil {
		t.Fatal(err)
	}
	if got := tagNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"animal_ears"}) {
		t.Errorf("tags after removing the parent = %v, want [animal_ears]", got)
	}
}

func TestApplyTaggerResults_SwapsARatingWithoutLosingIt(t *testing.T) {
	database, svc := setupTestDB(t)
	rating := svc.RatingCategoryID()
	imageID := insertTestImage(t, database, "rating_swap")

	applyHits(t, svc, imageID, TaggerHit{Name: "explicit", CategoryID: rating, Score: 0.9, Tagger: "tagger-A"})
	applyHits(t, svc, imageID, TaggerHit{Name: "sensitive", CategoryID: rating, Score: 0.9, Tagger: "tagger-A"})

	if got := ratingNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"sensitive"}) {
		t.Errorf("ratings = %v, want [sensitive]", got)
	}
}

func TestApplyTaggerResults_LeavesAManualRatingAlone(t *testing.T) {
	database, svc := setupTestDB(t)
	rating := svc.RatingCategoryID()
	imageID := insertTestImage(t, database, "manual_rating")
	mustAddTag(t, svc, imageID, ratingTagIDByName(t, database, "sensitive"), false, nil)

	applyHits(t, svc, imageID, TaggerHit{Name: "explicit", CategoryID: rating, Score: 0.95, Tagger: "tagger-A"})

	if got := ratingNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"sensitive"}) {
		t.Errorf("ratings = %v, want [sensitive]", got)
	}
}

func aliasOntoRating(t *testing.T, svc *Service, alias string, ratingID int64) int64 {
	t.Helper()
	meta := categoryIDOf(t, svc, "meta")
	mustCreateAlias(t, svc, alias, meta, ratingID)
	return meta
}

func TestApplyTaggerResults_AnAliasOntoARatingKeepsTheManualRating(t *testing.T) {
	database, svc := setupTestDB(t)
	meta := aliasOntoRating(t, svc, "nsfw", ratingTagIDByName(t, database, "explicit"))
	imageID := insertTestImage(t, database, "alias_manual_rating")
	mustAddTag(t, svc, imageID, ratingTagIDByName(t, database, "sensitive"), false, nil)

	applyHits(t, svc, imageID, TaggerHit{Name: "nsfw", CategoryID: meta, Score: 0.9, Tagger: "tagger-A"})

	if got := ratingNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"sensitive"}) {
		t.Errorf("ratings = %v, want [sensitive]", got)
	}
}

func TestApplyTaggerResults_AnAliasOntoARatingKeepsOnlyTheHighest(t *testing.T) {
	database, svc := setupTestDB(t)
	meta := aliasOntoRating(t, svc, "nsfw", ratingTagIDByName(t, database, "explicit"))
	imageID := insertTestImage(t, database, "alias_rating_prune")
	other := []TaggerHit{{Name: "general", CategoryID: svc.RatingCategoryID(), Score: 0.6, Tagger: "tagger-B"}}
	if err := svc.ApplyTaggerResults(context.Background(), imageID, other, []string{"tagger-B"}); err != nil {
		t.Fatal(err)
	}

	applyHits(t, svc, imageID, TaggerHit{Name: "nsfw", CategoryID: meta, Score: 0.9, Tagger: "tagger-A"})

	if got := ratingNamesOnImage(t, svc, imageID); !slices.Equal(got, []string{"explicit"}) {
		t.Errorf("ratings = %v, want [explicit]", got)
	}
}
