package tags

import (
	"fmt"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func imageHasTag(t *testing.T, database *db.DB, imgID, tagID int64) bool {
	t.Helper()
	n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", imgID, tagID)
	return n > 0
}

func imageTagTagger(t *testing.T, database *db.DB, imgID, tagID int64) string {
	t.Helper()
	var tn *string
	if err := database.Read.QueryRow(`SELECT tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, tagID).Scan(&tn); err != nil {
		t.Fatal(err)
	}
	if tn == nil {
		return ""
	}
	return *tn
}

func imageTagStale(t *testing.T, database *db.DB, imgID, tagID int64) bool {
	t.Helper()
	var stale int
	if err := database.Read.QueryRow(`SELECT stale FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, tagID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	return stale == 1
}

func TestSyncSourceTags_ClonesSliceKeepingOtherTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "sync01")
	mk := func(name string) int64 { tg, _ := svc.GetOrCreateTag(name, catID); return tg.ID }
	alpha, beta, gamma, manual := mk("alpha"), mk("beta"), mk("gamma"), mk("manual_keep")

	mustAddTag(t, svc, imgID, manual, false, nil)
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{gamma}, false, "gelbooru"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncSourceTags(imgID, []int64{alpha, beta}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if got := imageTagTagger(t, database, imgID, alpha); got != "danbooru" {
		t.Errorf("alpha tagger = %q, want danbooru", got)
	}

	r := mustSyncSourceTags(t, svc, imgID, []int64{alpha, gamma}, "danbooru", true)
	if r.Retired != 1 {
		t.Errorf("Retired = %d, want 1", r.Retired)
	}
	if !imageHasTag(t, database, imgID, beta) {
		t.Error("beta must survive the sync that dropped it")
	}
	if !imageTagStale(t, database, imgID, beta) {
		t.Error("beta should be flagged stale in danbooru's slice")
	}
	if !imageHasTag(t, database, imgID, alpha) || imageTagStale(t, database, imgID, alpha) {
		t.Error("alpha should remain current in danbooru's set")
	}
	if !imageHasTag(t, database, imgID, manual) {
		t.Error("manual tag must survive a source sync")
	}
	if got := imageTagTagger(t, database, imgID, gamma); got != "gelbooru" {
		t.Errorf("gamma tagger = %q, want gelbooru (not re-owned by danbooru)", got)
	}

	if _, err := svc.SyncSourceTags(imgID, []int64{alpha, beta}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if imageTagStale(t, database, imgID, beta) {
		t.Error("beta should shed its stale flag once re-listed")
	}
}

func TestSyncSourceTags_WithheldCategoryIsNotRetired(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	artist, _, err := CategoryIDByName(database, "artist")
	if err != nil {
		t.Fatal(err)
	}
	imgID := insertTestImage(t, database, "sync_withheld")
	hat, _ := svc.GetOrCreateTag("hat", general)
	scarf, _ := svc.GetOrCreateTag("scarf", general)
	painter, _ := svc.GetOrCreateTag("painter", artist)
	sculptor, _ := svc.GetOrCreateTag("sculptor", artist)
	mustSyncSourceTags(t, svc, imgID, []int64{hat.ID, scarf.ID, painter.ID, sculptor.ID}, "danbooru", true)

	r, err := svc.SyncSourceTagsWithheld(imgID, []int64{hat.ID, painter.ID}, "danbooru", true, Withheld{Categories: []string{"artist"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Retired != 1 {
		t.Errorf("Retired = %d, want only the listed category's", r.Retired)
	}
	if !imageTagStale(t, database, imgID, scarf.ID) {
		t.Error("a tag of a sent category should still go stale")
	}
	if imageTagStale(t, database, imgID, sculptor.ID) {
		t.Error("a tag of a withheld category went stale")
	}
}

func TestSyncSourceTags_WithheldTagIsNotRetired(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "sync_withheld_tag")
	hat, _ := svc.GetOrCreateTag("hat", general)
	scarf, _ := svc.GetOrCreateTag("scarf", general)
	gone, _ := svc.GetOrCreateTag("gone", general)
	mustSyncSourceTags(t, svc, imgID, []int64{hat.ID, scarf.ID, gone.ID}, "danbooru", true)

	r, err := svc.SyncSourceTagsWithheld(imgID, []int64{hat.ID}, "danbooru", true, Withheld{TagIDs: []int64{scarf.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Retired != 1 {
		t.Errorf("Retired = %d, want only the tag left unlisted", r.Retired)
	}
	if !imageTagStale(t, database, imgID, gone.ID) || imageTagStale(t, database, imgID, scarf.ID) {
		t.Errorf("stale = gone:%v scarf:%v, want true and false", imageTagStale(t, database, imgID, gone.ID), imageTagStale(t, database, imgID, scarf.ID))
	}
}

func TestSyncSourceTags_ProtectsExistingRating(t *testing.T) {
	database, svc := setupTestDB(t)
	genID := generalCategoryID(t, svc)
	ratingCat := svc.RatingCategoryID()
	imgID := insertTestImage(t, database, "sync02")
	plain, _ := svc.GetOrCreateTag("scenery", genID)
	general, _ := svc.GetOrCreateTag("general", ratingCat)
	explicit, _ := svc.GetOrCreateTag("explicit", ratingCat)

	r := mustSyncSourceTags(t, svc, imgID, []int64{plain.ID, general.ID}, "danbooru", true)
	if !r.RatingFilled {
		t.Error("expected RatingFilled on an unrated image")
	}
	if !imageHasTag(t, database, imgID, general.ID) {
		t.Error("general rating should be filled")
	}

	if _, err := svc.SyncSourceTags(imgID, []int64{plain.ID, explicit.ID}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if imageHasTag(t, database, imgID, explicit.ID) {
		t.Error("explicit must not overwrite the existing rating")
	}
	if !imageHasTag(t, database, imgID, general.ID) {
		t.Error("existing general rating must be preserved")
	}
}

func TestSyncSourceTags_ConflictingPayloadLandsTheHighestRating(t *testing.T) {
	database, svc := setupTestDB(t)
	genID := generalCategoryID(t, svc)
	ratingCat := svc.RatingCategoryID()
	plain, _ := svc.GetOrCreateTag("scenery", genID)
	general, _ := svc.GetOrCreateTag("general", ratingCat)
	sensitive, _ := svc.GetOrCreateTag("sensitive", ratingCat)
	explicit, _ := svc.GetOrCreateTag("explicit", ratingCat)

	for i, payload := range [][]int64{
		{plain.ID, explicit.ID, general.ID, sensitive.ID},
		{plain.ID, general.ID, sensitive.ID, explicit.ID},
	} {
		imgID := insertTestImage(t, database, fmt.Sprintf("sync03-%d", i))
		if _, err := svc.SyncSourceTags(imgID, payload, "ptr", true); err != nil {
			t.Fatal(err)
		}
		if !imageHasTag(t, database, imgID, explicit.ID) {
			t.Errorf("payload %d: the highest rating should land", i)
		}
		if imageHasTag(t, database, imgID, general.ID) || imageHasTag(t, database, imgID, sensitive.ID) {
			t.Errorf("payload %d: lower ratings in the same payload must not stack", i)
		}
	}
}

func TestSyncSourceTags_NeverReownsAutoOrImpliedRows(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "sync03")
	mk := func(name string) int64 { tg, _ := svc.GetOrCreateTag(name, catID); return tg.ID }
	autoTag, parent, implied := mk("long_hair"), mk("twin_braids"), mk("braid")

	conf := 0.93
	mustAddTag(t, svc, imgID, autoTag, true, &conf)
	mustAddImply(t, svc, parent, implied)
	mustAddTag(t, svc, imgID, parent, false, nil)

	rowState := func(tagID int64) (isAuto, isImplied int, confidence *float64) {
		t.Helper()
		if err := database.Read.QueryRow(
			`SELECT is_auto, is_implied, confidence FROM image_tags WHERE image_id = ? AND tag_id = ?`,
			imgID, tagID).Scan(&isAuto, &isImplied, &confidence); err != nil {
			t.Fatal(err)
		}
		return
	}

	if _, err := svc.SyncSourceTags(imgID, []int64{autoTag, implied}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if isAuto, _, confidence := rowState(autoTag); isAuto != 1 || confidence == nil {
		t.Errorf("auto row after sync: is_auto=%d confidence=%v, want the tagger's attribution kept", isAuto, confidence)
	}
	if _, isImplied, _ := rowState(implied); isImplied != 1 {
		t.Errorf("implied row after sync: is_implied=%d, want 1", isImplied)
	}

	if _, err := svc.SyncSourceTags(imgID, nil, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if !imageHasTag(t, database, imgID, autoTag) || imageTagStale(t, database, imgID, autoTag) {
		t.Error("auto-tagger row flagged by a source refetch")
	}
	if !imageHasTag(t, database, imgID, implied) || imageTagStale(t, database, imgID, implied) {
		t.Error("implied row flagged by a source refetch")
	}
}

func TestSyncSourceTags_ParentListedFirstLeavesTheChildTheSources(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "sync_parent_first")
	mk := func(name string) int64 { tg, _ := svc.GetOrCreateTag(name, catID); return tg.ID }
	parent, child := mk("thighhighs"), mk("legwear")
	mustAddImply(t, svc, parent, child)

	if _, err := svc.SyncSourceTags(imgID, []int64{parent, child}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	var isImplied int
	if err := database.Read.QueryRow(
		`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, child).Scan(&isImplied); err != nil {
		t.Fatal(err)
	}
	if tagger := imageTagTagger(t, database, imgID, child); isImplied != 0 || tagger != "danbooru" {
		t.Errorf("listed child: is_implied=%d tagger=%q, want 0 / danbooru", isImplied, tagger)
	}

	if _, err := svc.SyncSourceTags(imgID, []int64{child}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RemoveStaleTagsFromImage(imgID); err != nil {
		t.Fatal(err)
	}
	if imageHasTag(t, database, imgID, parent) {
		t.Error("the retired parent is still on the image")
	}
	if !imageHasTag(t, database, imgID, child) {
		t.Error("the child the source still lists went with its parent")
	}
}
