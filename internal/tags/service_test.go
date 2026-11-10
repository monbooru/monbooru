package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func edgesTouching(t *testing.T, database *db.DB, tagID int64) int {
	t.Helper()
	n := countRows(t, database, "tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?", tagID, tagID)
	return n
}

func mustTag(t *testing.T, svc *Service, name string, categoryID int64) *models.Tag {
	t.Helper()
	tag, err := svc.GetOrCreateTag(name, categoryID)
	if err != nil {
		t.Fatalf("GetOrCreateTag(%q): %v", name, err)
	}
	return tag
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

	svc := New(database, nil)
	return database, svc
}

func generalCategoryID(t *testing.T, svc *Service) int64 {
	t.Helper()
	return categoryIDOf(t, svc, "general")
}

func insertTestImage(t *testing.T, database *db.DB, sha string) int64 {
	t.Helper()
	var id int64
	err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 100) RETURNING id`,
		sha, "/gallery/"+sha+".png",
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertTestImage: %v", err)
	}
	return id
}

func TestAddTagToImage_UsageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "abc123")

	tag := mustTag(t, svc, "cute", catID)

	mustAddTag(t, svc, imgID, tag.ID, false, nil)

	got := mustGetTag(t, svc, tag.ID)
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1", got.UsageCount)
	}
}

func TestAddTagToMissingImage_DoesNotIncrementUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "missing-add")
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, imgID)

	tag := mustTag(t, svc, "phantom", catID)
	mustAddTag(t, svc, imgID, tag.ID, false, nil)
	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 0 {
		t.Errorf("UsageCount = %d, want 0 after add to missing image", got.UsageCount)
	}
}

func TestRemoveTagFromMissingImage_DoesNotDecrementUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	visible := insertTestImage(t, database, "remove-visible")
	missing := insertTestImage(t, database, "remove-missing")

	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, missing)

	tag := mustTag(t, svc, "dual", catID)
	mustAddTag(t, svc, visible, tag.ID, false, nil)
	mustAddTag(t, svc, missing, tag.ID, false, nil)
	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Fatalf("preflight UsageCount = %d, want 1 (visible-only)", got.UsageCount)
	}
	if _, err := svc.RemoveTagFromImage(missing, tag.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d after remove from missing, want 1", got.UsageCount)
	}
}

func TestAddTagTwice_NoDouble(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc124")

	tag, _ := svc.GetOrCreateTag("cute", catID)
	mustAddTag(t, svc, imgID, tag.ID, false, nil)
	mustAddTag(t, svc, imgID, tag.ID, false, nil)

	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1 after duplicate add", got.UsageCount)
	}
}

func TestAddTagsToImageFromTagger_ManualSource(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc130")

	tag, _ := svc.GetOrCreateTag("sourced", catID)
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{tag.ID}, false, "my_app"); err != nil {
		t.Fatalf("AddTagsToImageFromTagger: %v", err)
	}

	var isAuto int
	var taggerName *string
	err := database.Read.QueryRow(
		`SELECT is_auto, tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`,
		imgID, tag.ID,
	).Scan(&isAuto, &taggerName)
	if err != nil {
		t.Fatalf("scan image_tags: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0 for manual source-tagged add", isAuto)
	}
	if taggerName == nil || *taggerName != "my_app" {
		t.Errorf("tagger_name = %v, want %q", taggerName, "my_app")
	}
}

func TestAddTagToImage_AnonymousAddStoresANullTaggerName(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "anon_null")
	tag := mustTag(t, svc, "anon_tag", generalCategoryID(t, svc))
	if _, err := svc.AddTagToImageReportingDup(imgID, tag.ID, false, nil, ""); err != nil {
		t.Fatalf("AddTagToImageReportingDup: %v", err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND tagger_name IS NULL", imgID, tag.ID); n != 1 {
		t.Errorf("tagger_name = %q, want NULL for an anonymous add", imageTagTagger(t, database, imgID, tag.ID))
	}
}

func TestAddTagToImage_PromotesAutoToUser(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "auto_to_user")

	tag, _ := svc.GetOrCreateTag("auto_to_user_tag", catID)
	conf := 0.87
	if _, err := svc.AddTagToImageReportingDup(imgID, tag.ID, true, &conf, "wd-swin"); err != nil {
		t.Fatalf("seed auto row: %v", err)
	}

	res, err := svc.AddTagToImageReportingDup(imgID, tag.ID, false, nil, "")
	if err != nil {
		t.Fatalf("manual re-add: %v", err)
	}
	if res.Added {
		t.Error("Added should be false; the row already existed as auto")
	}
	if !res.Promoted {
		t.Error("Promoted should be true; auto row should flip to user")
	}

	var isAuto int
	var confidence sql.NullFloat64
	var taggerName sql.NullString
	if err := database.Read.QueryRow(
		`SELECT is_auto, confidence, tagger_name FROM image_tags
		 WHERE image_id = ? AND tag_id = ?`, imgID, tag.ID,
	).Scan(&isAuto, &confidence, &taggerName); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0", isAuto)
	}
	if confidence.Valid {
		t.Errorf("confidence = %v, want NULL after promotion", confidence.Float64)
	}
	if taggerName.Valid {
		t.Errorf("tagger_name = %q, want NULL after promotion", taggerName.String)
	}
}

func TestAddTagToImage_AutoReAddDoesNotDemoteUser(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "user_keep")

	tag, _ := svc.GetOrCreateTag("user_keep_tag", catID)
	mustAddTag(t, svc, imgID, tag.ID, false, nil)

	conf := 0.92
	res, err := svc.AddTagToImageReportingDup(imgID, tag.ID, true, &conf, "wd-swin")
	if err != nil {
		t.Fatalf("auto re-add: %v", err)
	}
	if res.Added || res.Promoted {
		t.Errorf("auto re-add of a user row should be a no-op, got %+v", res)
	}

	var isAuto int
	var confidence sql.NullFloat64
	var taggerName sql.NullString
	if err := database.Read.QueryRow(
		`SELECT is_auto, confidence, tagger_name FROM image_tags
		 WHERE image_id = ? AND tag_id = ?`, imgID, tag.ID,
	).Scan(&isAuto, &confidence, &taggerName); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0 (user-owned row must stick)", isAuto)
	}
	if confidence.Valid || taggerName.Valid {
		t.Errorf("user row should not gain confidence/tagger_name; got conf=%v tagger=%v", confidence, taggerName)
	}
}

func TestRemoveTag_DecrementUsageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc125")

	tag, _ := svc.GetOrCreateTag("cute", catID)
	mustAddTag(t, svc, imgID, tag.ID, false, nil)
	if _, err := svc.RemoveTagFromImage(imgID, tag.ID); err != nil {
		t.Fatal(err)
	}

	got := mustGetTag(t, svc, tag.ID)
	if got.UsageCount != 0 {
		t.Errorf("UsageCount = %d, want 0", got.UsageCount)
	}
}

func TestDeleteCategory_ReassignsToGeneral(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	custom := mustCreateCategory(t, svc, "custom_cat", "#aabbcc")

	tag := mustTag(t, svc, "mytag", custom.ID)

	if err := svc.DeleteCategoryMoveOrDelete(custom.ID, "move", 0); err != nil {
		t.Fatal(err)
	}

	got := mustGetTag(t, svc, tag.ID)
	if got.CategoryID != catID {
		t.Errorf("tag category = %d, want general (%d)", got.CategoryID, catID)
	}
}

func TestDeleteCategory_RejectsUnusableMoveTarget(t *testing.T) {
	_, svc := setupTestDB(t)
	custom := mustCreateCategory(t, svc, "custom_cat", "#aabbcc")
	tag := mustTag(t, svc, "mytag", custom.ID)

	for _, target := range []int64{custom.ID, 999999} {
		if err := svc.DeleteCategoryMoveOrDelete(custom.ID, "move", target); err != ErrInvalidMoveTarget {
			t.Errorf("target %d: got %v, want ErrInvalidMoveTarget", target, err)
		}
	}

	got := mustGetTag(t, svc, tag.ID)
	if got.CategoryID != custom.ID {
		t.Errorf("tag category = %d, want it left in %d", got.CategoryID, custom.ID)
	}
}

func TestDeleteCategory_NamesTheTagsAMoveWouldCollideWith(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	custom := mustCreateCategory(t, svc, "dupcat", "#aabbcc")
	for _, name := range []string{"scenery", "cat"} {
		mustTag(t, svc, name, general)
		mustTag(t, svc, name, custom.ID)
	}
	mustTag(t, svc, "unique_here", custom.ID)

	err := svc.DeleteCategoryMoveOrDelete(custom.ID, "move", general)
	var collision *ErrCategoryMoveCollision
	if !errors.As(err, &collision) {
		t.Fatalf("err = %v, want a named collision", err)
	}
	if got := strings.Join(collision.Names, ","); got != "cat,scenery" {
		t.Errorf("names = %v, want both colliding tags", collision.Names)
	}
	if strings.Contains(err.Error(), "constraint") {
		t.Errorf("message = %q, want it in the app's own words", err.Error())
	}
	n := countRows(t, svc.db, "tags WHERE category_id = ?", custom.ID)
	if n != 3 {
		t.Errorf("category holds %d tags, want the 3 it started with", n)
	}
}

func TestCategoryDeleteCounts_CountsAliasesAndTheAliasesPointingIn(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	custom := mustCreateCategory(t, svc, "countcat", "#aabbcc")
	aliasOnly := mustCreateCategory(t, svc, "aliasonly", "#aabbcc")
	canon := mustTag(t, svc, "count_canon", custom.ID)
	mustTag(t, svc, "count_plain", custom.ID)
	for _, a := range []struct {
		name string
		cat  int64
	}{{"count_alias", custom.ID}, {"count_elsewhere", general}, {"count_far", aliasOnly.ID}} {
		mustCreateAlias(t, svc, a.name, a.cat, canon.ID)
	}

	for _, c := range []struct {
		id   int64
		want CategoryCounts
	}{
		{custom.ID, CategoryCounts{Tags: 2, Aliases: 1, LinkedAliases: 2}},
		{aliasOnly.ID, CategoryCounts{Aliases: 1}},
	} {
		got, err := svc.CategoryDeleteCounts(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("category %d counts = %+v, want %+v", c.id, got, c.want)
		}
	}
	if _, err := svc.CategoryDeleteCounts(987654); !errors.Is(err, ErrCategoryNotFound) {
		t.Errorf("unknown category err = %v, want ErrCategoryNotFound", err)
	}
}

func TestDeleteCategory_RejectsTheRatingCategoryAsMoveTarget(t *testing.T) {
	_, svc := setupTestDB(t)
	custom := mustCreateCategory(t, svc, "safetycat", "#aabbcc")
	tag := mustTag(t, svc, "nsfw", custom.ID)

	if err := svc.DeleteCategoryMoveOrDelete(custom.ID, "move", svc.ratingCatID); err != ErrRatingCategoryClosed {
		t.Errorf("move into rating: got %v, want ErrRatingCategoryClosed", err)
	}
	got := mustGetTag(t, svc, tag.ID)
	if got.CategoryID != custom.ID {
		t.Errorf("tag category = %d, want it left in %d", got.CategoryID, custom.ID)
	}
	n := countRows(t, svc.db, "tags WHERE category_id = ?", svc.ratingCatID)
	if n != 4 {
		t.Errorf("rating category holds %d rows, want the four canonical ones", n)
	}
}

func TestDeleteBuiltinCategory_Rejected(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	err := svc.DeleteCategoryMoveOrDelete(catID, "move", 0)
	if err != ErrBuiltinCategory {
		t.Errorf("expected ErrBuiltinCategory, got %v", err)
	}
}

func TestCreateCategory_RejectsSystemReservedName(t *testing.T) {
	_, svc := setupTestDB(t)

	if _, err := svc.CreateCategory("system", "#aabbcc"); err != ErrReservedCategoryName {
		t.Errorf("CreateCategory(system) err = %v, want ErrReservedCategoryName", err)
	}
}

func TestCreateCategory_RejectsInvalidNames(t *testing.T) {
	_, svc := setupTestDB(t)

	bad := []string{
		"' OR 1=1",
		"<script>",
		"2024/01/02",
		"a b",
		"foo bar",
		"foo:bar",
		"foo bar/baz",
		"foo$bar",
		"-wip",
	}
	for _, name := range bad {
		if _, err := svc.CreateCategory(name, "#aabbcc"); err != ErrInvalidCategoryName {
			t.Errorf("CreateCategory(%q) err = %v, want ErrInvalidCategoryName", name, err)
		}
	}

	if _, err := svc.CreateCategory("mood-2", "#aabbcc"); err != nil {
		t.Errorf("CreateCategory(mood-2) err = %v, want nil", err)
	}
}

func TestCreateCategory_DuplicateNameFriendlyError(t *testing.T) {
	_, svc := setupTestDB(t)

	mustCreateCategory(t, svc, "mood", "#aabbcc")
	if _, err := svc.CreateCategory("mood", "#aabbcc"); err != ErrCategoryExists {
		t.Errorf("duplicate CreateCategory(mood) err = %v, want ErrCategoryExists", err)
	}
	if _, err := svc.CreateCategory("general", "#aabbcc"); err != ErrCategoryExists {
		t.Errorf("CreateCategory(general) err = %v, want ErrCategoryExists", err)
	}
}

func TestRenameCategory_DuplicateNameFriendlyError(t *testing.T) {
	_, svc := setupTestDB(t)

	a := mustCreateCategory(t, svc, "alpha", "#aabbcc")
	mustCreateCategory(t, svc, "bravo", "#aabbcc")
	if err := svc.RenameCategory(a.ID, "bravo"); err != ErrCategoryExists {
		t.Errorf("RenameCategory(alpha→bravo) err = %v, want ErrCategoryExists", err)
	}
}

func TestRenameCategory_RefusesALeadingHyphen(t *testing.T) {
	_, svc := setupTestDB(t)
	a := mustCreateCategory(t, svc, "wip", "#aabbcc")
	if err := svc.RenameCategory(a.ID, "-wip"); err != ErrInvalidCategoryName {
		t.Errorf("RenameCategory(wip→-wip) err = %v, want ErrInvalidCategoryName", err)
	}
}

func TestRenameCategory_BuiltinUsesRenameMessage(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.RenameCategory(generalCategoryID(t, svc), "renamed_general"); err != ErrBuiltinCategoryName {
		t.Fatalf("RenameCategory(builtin) err = %v, want ErrBuiltinCategoryName", err)
	}
}

func TestMergeTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc127")

	tagAlias, _ := svc.GetOrCreateTag("old_tag", catID)
	tagCanon, _ := svc.GetOrCreateTag("new_tag", catID)

	mustAddTag(t, svc, imgID, tagAlias.ID, false, nil)

	mustMerge(t, svc, tagAlias.ID, tagCanon.ID)

	_, imgTags := mustImageTags(t, svc, imgID)
	found := false
	for _, it := range imgTags {
		if it.TagID == tagCanon.ID {
			found = true
		}
		if it.TagID == tagAlias.ID {
			t.Error("alias tag still on image after merge")
		}
	}
	if !found {
		t.Error("canonical tag not on image after merge")
	}

	got, _ := svc.GetTag(tagAlias.ID)
	if !got.IsAlias {
		t.Error("aliasID not marked as alias")
	}
	if got.CanonicalTagID == nil || *got.CanonicalTagID != tagCanon.ID {
		t.Error("canonical_tag_id not set correctly")
	}
}

func TestSuggestTags_PrefixFirst(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	mustTag(t, svc, "abc_123", catID)
	mustTag(t, svc, "xyz_abc", catID)
	mustTag(t, svc, "abc_456", catID)
	imgA := insertTestImage(t, database, "abc_img_a")
	imgB := insertTestImage(t, database, "abc_img_b")
	xyzTag, _ := svc.GetOrCreateTag("xyz_abc", catID)
	for _, img := range []int64{imgA, imgB} {
		mustAddTag(t, svc, img, xyzTag.ID, false, nil)
	}

	results, err := svc.SuggestTags("abc", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 3 {
		t.Fatalf("expected 3 suggestions, got %d: %+v", len(results), results)
	}
	for i := 0; i < 2; i++ {
		if !strings.HasPrefix(results[i].Name, "abc") {
			t.Errorf("position %d: got %q, want a name starting with 'abc' (full order: %+v)", i, results[i].Name, results)
		}
	}
	if results[2].Name != "xyz_abc" {
		t.Errorf("position 2: got %q, want xyz_abc (substring match last)", results[2].Name)
	}
}

func TestSuggestTagsInCategory(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	character := categoryIDOf(t, svc, "character")

	parade, _ := svc.GetOrCreateTag("parade", character)
	parasol, _ := svc.GetOrCreateTag("parasol", character)
	_, _ = svc.GetOrCreateTag("parade_general", catID)
	_, _ = svc.GetOrCreateTag("comparable", character)
	canon, _ := svc.GetOrCreateTag("honored", character)
	mustCreateAlias(t, svc, "parka_alias", character, canon.ID)

	imgA := insertTestImage(t, database, "cat_suggest_a")
	imgB := insertTestImage(t, database, "cat_suggest_b")
	imgC := insertTestImage(t, database, "cat_suggest_c")
	for _, img := range []int64{imgA, imgB} {
		mustAddTag(t, svc, img, parasol.ID, false, nil)
	}
	mustAddTag(t, svc, imgC, parade.ID, false, nil)

	results, err := svc.SuggestTags("par", "character", 10)
	if err != nil {
		t.Fatal(err)
	}

	got := tagNames(results)
	want := []string{"parasol", "parade", "comparable"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SuggestTags = %v, want %v (prefix matches in the named category by usage, then substring ones, no alias)", got, want)
	}
	for _, r := range results {
		if r.CategoryName != "character" {
			t.Errorf("tag %q has category %q, want character", r.Name, r.CategoryName)
		}
	}

	one, err := svc.SuggestTags("par", "character", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Name != "parasol" {
		t.Errorf("limit=1 returned %v, want [parasol] (highest usage)", tagNames(one))
	}

	none, err := svc.SuggestTags("zzz", "character", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("no-match prefix returned %v, want empty", tagNames(none))
	}
}

func TestRelatedImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	img1 := insertTestImage(t, database, "rel1")
	img2 := insertTestImage(t, database, "rel2")
	img3 := insertTestImage(t, database, "rel3")

	tagA, _ := svc.GetOrCreateTag("rel_a", catID)
	tagB, _ := svc.GetOrCreateTag("rel_b", catID)
	tagC, _ := svc.GetOrCreateTag("rel_c", catID)

	mustAddTag(t, svc, img1, tagA.ID, false, nil)
	mustAddTag(t, svc, img1, tagB.ID, false, nil)
	mustAddTag(t, svc, img2, tagA.ID, false, nil)
	mustAddTag(t, svc, img2, tagB.ID, false, nil)
	mustAddTag(t, svc, img3, tagC.ID, false, nil)

	related, err := svc.RelatedImages(img1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != img2 {
		t.Fatalf("related = %+v, want only img2", related)
	}
}

func TestRelatedImages_CarriesFileTypeAndPageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	var src int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, page_count) VALUES (?, ?, 'cbz', 800, 5) RETURNING id`,
		"src-cbz", "/gallery/src.cbz",
	).Scan(&src); err != nil {
		t.Fatalf("insert src manga: %v", err)
	}
	var manga int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, page_count) VALUES (?, ?, 'cbz', 1000, 12) RETURNING id`,
		"manga-sha", "/gallery/m.cbz",
	).Scan(&manga); err != nil {
		t.Fatalf("insert manga: %v", err)
	}

	tagA, _ := svc.GetOrCreateTag("shared", catID)
	mustAddTag(t, svc, src, tagA.ID, false, nil)
	mustAddTag(t, svc, manga, tagA.ID, false, nil)

	related, err := svc.RelatedImages(src, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != manga {
		t.Fatalf("related = %+v, want one manga row", related)
	}
	if related[0].FileType != "cbz" {
		t.Errorf("FileType = %q, want cbz", related[0].FileType)
	}
	if related[0].PageCount == nil || *related[0].PageCount != 12 {
		t.Errorf("PageCount = %v, want 12", related[0].PageCount)
	}
}

func TestRelatedImages_TypePartition(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgSrc := insertTestImage(t, database, "img-src")
	imgPeer := insertTestImage(t, database, "img-peer")
	var manga int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, page_count) VALUES (?, ?, 'cbz', 100, 3) RETURNING id`,
		"manga-x", "/gallery/x.cbz",
	).Scan(&manga); err != nil {
		t.Fatal(err)
	}
	tag, _ := svc.GetOrCreateTag("link", catID)
	mustAddTag(t, svc, imgSrc, tag.ID, false, nil)
	mustAddTag(t, svc, imgPeer, tag.ID, false, nil)
	mustAddTag(t, svc, manga, tag.ID, false, nil)

	rel, err := svc.RelatedImages(imgSrc, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel) != 1 || rel[0].ID != imgPeer {
		t.Fatalf("img-source related = %+v, want only the peer image (manga must be filtered out)", rel)
	}

	relManga, err := svc.RelatedImages(manga, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(relManga) != 0 {
		t.Fatalf("manga-source related = %+v, want empty (no other manga)", relManga)
	}
}

func TestRelatedImages_DropsPopularTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	img1 := insertTestImage(t, database, "rel_pop1")
	img2 := insertTestImage(t, database, "rel_pop2")

	tag, _ := svc.GetOrCreateTag("very_popular", catID)
	mustAddTag(t, svc, img1, tag.ID, false, nil)
	mustAddTag(t, svc, img2, tag.ID, false, nil)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = ? WHERE id = ?`, relatedMaxTagUsage+1, tag.ID)

	related, err := svc.RelatedImages(img1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 0 {
		t.Fatalf("related = %+v, want empty (shared tag is over the popularity cap)", related)
	}
}

func TestRelatedImagesLeavesOutExcludedTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	src := insertTestImage(t, database, "ex_src")
	safe := insertTestImage(t, database, "ex_safe")
	explicit := insertTestImage(t, database, "ex_explicit")
	tag, _ := svc.GetOrCreateTag("ex_shared", catID)
	for _, id := range []int64{src, safe, explicit} {
		mustAddTag(t, svc, id, tag.ID, false, nil)
	}
	mustAddTag(t, svc, explicit, ratingTagIDByName(t, database, "explicit"), false, nil)

	related, err := svc.RelatedImages(src, 10, svc.RatingTagIDsAbove("general"))
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != safe {
		t.Fatalf("related = %+v, want only the image without an excluded tag", related)
	}
	if all, err := svc.RelatedImages(src, 10, nil); err != nil || len(all) != 2 {
		t.Fatalf("related with nothing excluded = %+v (%v), want both", all, err)
	}
}

func TestListTags_All(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	mustTag(t, svc, "list_a", catID)
	mustTag(t, svc, "list_b", catID)

	tags, total := mustListTags(t, svc, TagFilter{Limit: 100, ShowZero: true})
	if total < 2 {
		t.Errorf("total = %d, want >= 2", total)
	}
	_ = tags
}

func TestListTags_WithPrefix(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	mustTag(t, svc, "prefix_abc", catID)
	mustTag(t, svc, "prefix_xyz", catID)
	mustTag(t, svc, "other_tag", catID)

	tags, total := mustListTags(t, svc, TagFilter{Prefix: "prefix", Limit: 100, ShowZero: true})
	if total < 2 {
		t.Errorf("prefix total = %d, want >= 2", total)
	}
	for _, tg := range tags {
		if len(tg.Name) < 6 || tg.Name[:6] != "prefix" {
			t.Errorf("unexpected tag in prefix filter: %q", tg.Name)
		}
	}
}

func TestListTags_WithWildcard(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	for _, n := range []string{"black_kimono", "white_kimono", "kimono", "kimono_dress", "50%_off", "under_score"} {
		mustTag(t, svc, n, catID)
	}

	cases := []struct {
		q    string
		want []string
	}{
		{"*kimono*", []string{"black_kimono", "kimono", "kimono_dress", "white_kimono"}},
		{"*kimono", []string{"black_kimono", "kimono", "white_kimono"}},
		{"kimono*", []string{"kimono", "kimono_dress"}},

		{"kimono", []string{"kimono", "kimono_dress"}},

		{"50%", []string{"50%_off"}},
		{"*%*", []string{"50%_off"}},
		{"under_", []string{"under_score"}},
	}
	for _, c := range cases {
		list, total := mustListTags(t, svc, TagFilter{Prefix: c.q, Limit: 100, ShowZero: true})
		got := []string{}
		for _, tg := range list {
			got = append(got, tg.Name)
		}
		if total != len(c.want) || !reflect.DeepEqual(got, c.want) {
			t.Errorf("q=%q got %v (total %d), want %v", c.q, got, total, c.want)
		}
	}
}

func TestListTags_WithCategoryFilter(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	custom, _ := svc.CreateCategory("custom_filter", "#000000")
	mustTag(t, svc, "cat_tag", custom.ID)
	mustTag(t, svc, "gen_tag", catID)

	tags, total := mustListTags(t, svc, TagFilter{CategoryID: &custom.ID, Limit: 100, ShowZero: true})
	if total != 1 {
		t.Errorf("category filter total = %d, want 1", total)
	}
	_ = tags
}

func TestListTags_SortByUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	img1 := insertTestImage(t, database, "usage_sort_1")
	img2 := insertTestImage(t, database, "usage_sort_2")
	img3 := insertTestImage(t, database, "usage_sort_3")
	tagA, _ := svc.GetOrCreateTag("sort_a", catID)
	tagB, _ := svc.GetOrCreateTag("sort_b", catID)
	tagC, _ := svc.GetOrCreateTag("sort_c", catID)
	for _, img := range []int64{img1, img2} {
		mustAddTag(t, svc, img, tagA.ID, false, nil)
	}
	mustAddTag(t, svc, img3, tagB.ID, false, nil)
	for _, img := range []int64{img1, img2, img3} {
		mustAddTag(t, svc, img, tagC.ID, false, nil)
	}

	tags, _ := mustListTags(t, svc, TagFilter{Sort: "usage", Prefix: "sort_", Limit: 100})
	if len(tags) < 3 {
		t.Fatalf("expected at least 3 tags with prefix sort_, got %d", len(tags))
	}
	wantOrder := []string{"sort_c", "sort_a", "sort_b"}
	for i, want := range wantOrder {
		if tags[i].Name != want {
			t.Errorf("position %d: got %q, want %q (full order: %+v)", i, tags[i].Name, want, tagNames(tags))
		}
	}
}

func tagNames(ts []models.Tag) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestRecalc_CountsOnlyNonMissing(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	liveImg := insertTestImage(t, database, "recalc_live")
	goneImg := insertTestImage(t, database, "recalc_gone")
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, goneImg)

	shared, _ := svc.GetOrCreateTag("recalc_shared", catID)
	onlyGone, _ := svc.GetOrCreateTag("recalc_only_gone", catID)
	mustAddTag(t, svc, liveImg, shared.ID, false, nil)
	mustAddTag(t, svc, goneImg, shared.ID, false, nil)
	mustAddTag(t, svc, goneImg, onlyGone.ID, false, nil)

	mustExec(t, database.Write, `UPDATE tags SET usage_count = 99 WHERE id IN (?, ?)`, shared.ID, onlyGone.ID)

	updated, err := svc.RecalcCount()
	if err != nil {
		t.Fatalf("RecalcCount: %v", err)
	}
	if updated < 2 {
		t.Errorf("updated = %d, want >= 2", updated)
	}

	got := mustGetTag(t, svc, shared.ID)
	if got.UsageCount != 1 {
		t.Errorf("shared UsageCount = %d, want 1 (only live image counts)", got.UsageCount)
	}
	gone := mustGetTag(t, svc, onlyGone.ID)
	if gone.UsageCount != 0 {
		t.Errorf("only_gone UsageCount = %d, want 0", gone.UsageCount)
	}
}

func TestListTags_OriginFilterMatchesStoredLabel(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	mustTag(t, svc, "from_ui", catID)
	if _, err := svc.GetOrCreateTagFrom("from_danbooru", catID, "danbooru"); err != nil {
		t.Fatal(err)
	}
	ptr := mustGetOrCreateTagFrom(t, svc, "from_ptr", catID, "ptr")

	names := func(f TagFilter) []string {
		f.Limit = 100
		f.ShowZero = true
		f.Sort = "name"
		list, _ := mustListTags(t, svc, f)
		return tagNames(list)
	}

	if got := names(TagFilter{Origin: "danbooru"}); !reflect.DeepEqual(got, []string{"from_danbooru"}) {
		t.Errorf("origin=danbooru => %v, want [from_danbooru]", got)
	}
	if got := names(TagFilter{Origin: "user"}); !reflect.DeepEqual(got, []string{"from_ui"}) {
		t.Errorf("origin=user => %v, want [from_ui]", got)
	}
	if got := names(TagFilter{Origin: "ptr"}); !reflect.DeepEqual(got, []string{"from_ptr"}) {
		t.Errorf("origin=ptr => %v, want [from_ptr]", got)
	}

	byLabel := func(typeFilter string) map[string]int {
		t.Helper()
		counts, err := svc.OriginCounts(typeFilter)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, oc := range counts {
			out[oc.Label] = oc.Count
		}
		return out
	}
	got := byLabel("")

	if got["danbooru"] != 1 || got["ptr"] != 1 || got["user"] != 1 {
		t.Errorf("OriginCounts = %v, want danbooru/ptr/user at 1 each", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("OriginCounts surfaced the empty label: %v", got)
	}

	if _, err := svc.CreateAliasFrom("counted_alias", catID, ptr.ID, "ptr"); err != nil {
		t.Fatal(err)
	}
	if all, tagsOnly := byLabel("")["ptr"], byLabel("tag")["ptr"]; all != 2 || tagsOnly != 1 {
		t.Errorf("ptr counts = %d all / %d tags, want 2 / 1", all, tagsOnly)
	}
	if aliasOnly := byLabel("alias")["ptr"]; aliasOnly != 1 {
		t.Errorf("ptr alias count = %d, want 1", aliasOnly)
	}
}

func TestListTags_TypeFilterAndLegacyAliasOrigin(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	canon := mustTag(t, svc, "type_canon", catID)
	if _, err := svc.CreateAliasFrom("type_alias", catID, canon.ID, "ptr"); err != nil {
		t.Fatal(err)
	}

	names := func(f TagFilter) []string {
		f.Limit = 100
		f.ShowZero = true
		f.Sort = "name"
		f.Prefix = "type_"
		list, _ := mustListTags(t, svc, f)
		return tagNames(list)
	}

	if got := names(TagFilter{Type: "alias"}); !reflect.DeepEqual(got, []string{"type_alias"}) {
		t.Errorf("type=alias => %v, want [type_alias]", got)
	}
	if got := names(TagFilter{Type: "tag"}); !reflect.DeepEqual(got, []string{"type_canon"}) {
		t.Errorf("type=tag => %v, want [type_canon]", got)
	}
	if got := names(TagFilter{Origin: "alias"}); !reflect.DeepEqual(got, []string{"type_alias"}) {
		t.Errorf("origin=alias => %v, want [type_alias]", got)
	}

	if got := names(TagFilter{Type: "alias", Origin: "ptr"}); !reflect.DeepEqual(got, []string{"type_alias"}) {
		t.Errorf("type=alias origin=ptr => %v, want [type_alias]", got)
	}
}

func TestListTags_CreatedAfterAndDateSorts(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	older := mustTag(t, svc, "dated_old", catID)
	mustExec(t, database.Write, `UPDATE tags SET created_at = '2020-01-01T00:00:00Z' WHERE id = ?`, older.ID)
	mustTag(t, svc, "dated_new", catID)

	list, _ := mustListTags(t, svc, TagFilter{Prefix: "dated_", CreatedAfter: "2025-01-01T00:00:00Z", ShowZero: true, Limit: 100})
	if len(list) != 1 || list[0].Name != "dated_new" {
		t.Errorf("CreatedAfter => %+v, want only dated_new", list)
	}

	list, _ = mustListTags(t, svc, TagFilter{Prefix: "dated_", Sort: "created", ShowZero: true, Limit: 100})
	if len(list) != 2 || list[0].Name != "dated_new" {
		t.Errorf("sort=created => %+v, want dated_new first", list)
	}

	imgID := insertTestImage(t, database, "dated_img")
	mustAddTag(t, svc, imgID, older.ID, false, nil)
	list, _ = mustListTags(t, svc, TagFilter{Prefix: "dated_", Sort: "last_used", ShowZero: true, Limit: 100})
	if len(list) != 2 || list[0].Name != "dated_old" {
		t.Errorf("sort=last_used => %+v, want dated_old (applied) first", list)
	}
}

func TestUpdateCategoryColor(t *testing.T) {
	_, svc := setupTestDB(t)

	cat := mustCreateCategory(t, svc, "color_test", "#aabbcc")

	if err := svc.UpdateCategoryColor(cat.ID, "#112233"); err != nil {
		t.Fatal(err)
	}

	cats := mustListCategories(t, svc)
	for _, c := range cats {
		if c.ID == cat.ID && c.Color != "#112233" {
			t.Errorf("color = %q, want #112233", c.Color)
		}
	}
}

func TestDefaultCategoryColorMatchesSeed(t *testing.T) {
	_, svc := setupTestDB(t)

	cats := mustListCategories(t, svc)
	seeded := 0
	for _, c := range cats {
		if !c.IsBuiltin {
			continue
		}
		seeded++
		if got := DefaultCategoryColor(c.Name); got != c.Color {
			t.Errorf("DefaultCategoryColor(%q) = %q, seeded %q", c.Name, got, c.Color)
		}
	}
	if len(builtinCategoryColors) != seeded {
		t.Errorf("builtinCategoryColors has %d entries, schema seeds %d built-ins", len(builtinCategoryColors), seeded)
	}

	custom := mustCreateCategory(t, svc, "no_default", "#aabbcc")
	if got := DefaultCategoryColor(custom.Name); got != "" {
		t.Errorf("DefaultCategoryColor(%q) = %q, want empty", custom.Name, got)
	}
}

func TestCategoryColorStoredNormalized(t *testing.T) {
	_, svc := setupTestDB(t)

	cat := mustCreateCategory(t, svc, "shorthand", "#ABC")
	if cat.Color != "#aabbcc" {
		t.Errorf("created colour = %q, want #aabbcc", cat.Color)
	}

	if err := svc.UpdateCategoryColor(cat.ID, "#CC0000"); err != nil {
		t.Fatal(err)
	}
	cats := mustListCategories(t, svc)
	for _, c := range cats {
		if c.ID == cat.ID && c.Color != "#cc0000" {
			t.Errorf("updated colour = %q, want #cc0000", c.Color)
		}
	}

	if err := svc.UpdateCategoryColor(cat.ID, "not a colour"); err != ErrInvalidCategoryColor {
		t.Errorf("UpdateCategoryColor(%q) = %v, want %v", "not a colour", err, ErrInvalidCategoryColor)
	}
}

func TestRemoveAllTagsFromMissingImage_DoesNotDecrementUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	visible := insertTestImage(t, database, "remove-all-visible")
	missing := insertTestImage(t, database, "remove-all-missing")
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, missing)

	shared := mustTag(t, svc, "remove_all_shared", catID)
	alone := mustTag(t, svc, "remove_all_alone", catID)
	mustAddTag(t, svc, visible, shared.ID, false, nil)
	mustAddTag(t, svc, missing, shared.ID, false, nil)
	mustAddTag(t, svc, missing, alone.ID, false, nil)

	if err := svc.RemoveAllTagsFromImage(missing); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ?", missing); n != 0 {
		t.Errorf("%d tag row(s) left on the missing image, want 0", n)
	}
	if got := usageOf(t, svc, shared.ID); got != 1 {
		t.Errorf("shared UsageCount = %d after remove-all on the missing image, want 1", got)
	}
	if got := usageOf(t, svc, alone.ID); got != 0 {
		t.Errorf("alone UsageCount = %d, want 0", got)
	}

	if err := svc.RemoveAllTagsFromImage(visible); err != nil {
		t.Fatal(err)
	}
	if got := usageOf(t, svc, shared.ID); got != 0 {
		t.Errorf("shared UsageCount = %d after remove-all on the visible image, want 0", got)
	}
}

func TestRemoveAllTagsFromImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "remove_all")

	tagA, _ := svc.GetOrCreateTag("rem_all_a", catID)
	tagB, _ := svc.GetOrCreateTag("rem_all_b", catID)
	mustAddTag(t, svc, imgID, tagA.ID, false, nil)
	mustAddTag(t, svc, imgID, tagB.ID, false, nil)

	if err := svc.RemoveAllTagsFromImage(imgID); err != nil {
		t.Fatal(err)
	}

	_, imgTags, _ := svc.GetImageTags(imgID)
	if len(imgTags) != 0 {
		t.Errorf("expected 0 tags after RemoveAllTagsFromImage, got %d", len(imgTags))
	}

	for _, id := range []int64{tagA.ID, tagB.ID} {
		got := mustGetTag(t, svc, id)
		if got.UsageCount != 0 {
			t.Errorf("tag %d UsageCount = %d, want 0", id, got.UsageCount)
		}
	}
}

func TestRemoveUserTagsFromImage_SweepsImpliedChain(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "implied_chain")

	child, _ := svc.GetOrCreateTag("chain_child", catID)
	mid, _ := svc.GetOrCreateTag("chain_mid", catID)
	root, _ := svc.GetOrCreateTag("chain_root", catID)
	for _, edge := range [][2]int64{{root.ID, mid.ID}, {root.ID, child.ID}, {mid.ID, child.ID}} {
		mustAddImply(t, svc, edge[0], edge[1])
	}
	mustAddTag(t, svc, imgID, root.ID, false, nil)
	if _, tagsOn, _ := svc.GetImageTags(imgID); len(tagsOn) != 3 {
		t.Fatalf("fan-out should have landed 3 rows, got %d", len(tagsOn))
	}

	if _, err := svc.RemoveUserTagsFromImage(imgID); err != nil {
		t.Fatal(err)
	}
	_, left, _ := svc.GetImageTags(imgID)
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, it := range left {
			names = append(names, it.TagName)
		}
		t.Errorf("implied rows left with no parent: %v", names)
	}
	for _, tg := range []int64{root.ID, mid.ID, child.ID} {
		got := mustGetTag(t, svc, tg)
		if got.UsageCount != 0 {
			t.Errorf("tag %d usage_count = %d, want 0", tg, got.UsageCount)
		}
	}
}

func TestRemoveSourceTagsFromImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "rem_source")

	manual, _ := svc.GetOrCreateTag("man", catID)
	danb, _ := svc.GetOrCreateTag("dan", catID)
	gelb, _ := svc.GetOrCreateTag("gel", catID)
	mustAddTag(t, svc, imgID, manual.ID, false, nil)
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{danb.ID}, false, "danbooru"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{gelb.ID}, false, "gelbooru"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.RemoveSourceTagsFromImage(imgID, []string{"danbooru"}, ""); err != nil {
		t.Fatal(err)
	}

	_, imgTags, _ := svc.GetImageTags(imgID)
	got := map[string]bool{}
	for _, it := range imgTags {
		got[it.TagName] = true
	}
	if got["dan"] {
		t.Error("danbooru's tag should be removed")
	}
	if !got["man"] || !got["gel"] {
		t.Errorf("manual and gelbooru tags must survive, got %+v", imgTags)
	}

	if _, err := svc.RemoveSourceTagsFromImage(imgID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, after, _ := svc.GetImageTags(imgID); len(after) != 2 {
		t.Errorf("empty sources should be a no-op, got %d tags", len(after))
	}
}

func TestGetOrCreateTag_ValidatesName(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	_, err := svc.GetOrCreateTag("", catID)
	if err == nil {
		t.Error("expected error for empty tag name")
	}
}

func TestMergeThenAddAliasName_LandsOnCanonical(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	alias, _ := svc.GetOrCreateTag("cat", catID)
	canon, _ := svc.GetOrCreateTag("feline", catID)
	mustMerge(t, svc, alias.ID, canon.ID)

	imgID := insertTestImage(t, database, "alias_redirect")
	tag := mustTag(t, svc, "cat", catID)
	if tag.ID != canon.ID {
		t.Fatalf("GetOrCreateTag(alias) = %d, want canonical %d", tag.ID, canon.ID)
	}
	mustAddTag(t, svc, imgID, tag.ID, false, nil)
	_, imgTags, _ := svc.GetImageTags(imgID)
	if len(imgTags) != 1 || imgTags[0].TagID != canon.ID {
		t.Errorf("image tags = %+v, want single canonical tag %d", imgTags, canon.ID)
	}
}

func TestMergeIntoAlias_Rejected(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("aaa", catID)
	b, _ := svc.GetOrCreateTag("bbb", catID)
	c, _ := svc.GetOrCreateTag("ccc", catID)
	mustMerge(t, svc, a.ID, b.ID)
	if err := svc.MergeTags(c.ID, a.ID); err == nil {
		t.Fatal("expected error when merging into an alias, got nil")
	}
}

func TestMergeTags_RefusesAnUnknownAlias(t *testing.T) {
	_, svc := setupTestDB(t)
	canon := mustTag(t, svc, "merge_known", generalCategoryID(t, svc))
	for _, c := range []struct{ alias, canonical int64 }{{987654, canon.ID}, {canon.ID, 987654}} {
		if err := svc.MergeTags(c.alias, c.canonical); !errors.Is(err, ErrTagNotFound) {
			t.Errorf("MergeTags(%d, %d) = %v, want ErrTagNotFound", c.alias, c.canonical, err)
		}
	}
}

func TestMergeTags_FlattensDependentAliases(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	x, _ := svc.GetOrCreateTag("xxx", catID)
	y, _ := svc.GetOrCreateTag("yyy", catID)
	z, _ := svc.GetOrCreateTag("zzz", catID)
	mustMerge(t, svc, z.ID, x.ID)
	mustMerge(t, svc, x.ID, y.ID)
	var canon sql.NullInt64
	if err := database.Read.QueryRow(`SELECT canonical_tag_id FROM tags WHERE id = ?`, z.ID).Scan(&canon); err != nil {
		t.Fatal(err)
	}
	if canon.Int64 != y.ID {
		t.Fatalf("z.canonical_tag_id = %d, want %d", canon.Int64, y.ID)
	}
	resolved, _ := svc.GetOrCreateTag("zzz", catID)
	if resolved.ID != y.ID {
		t.Fatalf("GetOrCreateTag(z) = %d, want %d", resolved.ID, y.ID)
	}
}

func TestCreateAlias_FlattensDependentAliases(t *testing.T) {

	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	x, _ := svc.GetOrCreateTag("xxx", catID)
	y, _ := svc.GetOrCreateTag("yyy", catID)
	mustCreateAlias(t, svc, "zzz", catID, x.ID)
	mustCreateAlias(t, svc, "xxx", catID, y.ID)
	resolved, _ := svc.GetOrCreateTag("zzz", catID)
	if resolved.ID != y.ID {
		t.Fatalf("GetOrCreateTag(z) = %d, want %d", resolved.ID, y.ID)
	}
}

func TestListTags_AliasFilterReturnsAliasesWithCanonicalJoin(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	alias, _ := svc.GetOrCreateTag("cat", catID)
	canon, _ := svc.GetOrCreateTag("feline", catID)
	mustMerge(t, svc, alias.ID, canon.ID)
	list, total := mustListTags(t, svc, TagFilter{Origin: "alias", Limit: 40})
	if total != 1 || len(list) != 1 {
		t.Fatalf("alias filter: total=%d len=%d, want 1/1", total, len(list))
	}
	got := list[0]
	if got.Name != "cat" || !got.IsAlias || got.CanonicalName != "feline" {
		t.Errorf("alias row = %+v, want cat → feline", got)
	}
}

func TestListTags_AllIncludesAliasesAndCanonicals(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	alias, _ := svc.GetOrCreateTag("cat", catID)
	canon, _ := svc.GetOrCreateTag("feline", catID)
	mustTag(t, svc, "dog", catID)
	mustMerge(t, svc, alias.ID, canon.ID)
	list, _ := mustListTags(t, svc, TagFilter{Limit: 40, ShowZero: true})
	aliasSeen := slices.ContainsFunc(list, func(t models.Tag) bool { return t.Name == "cat" && t.IsAlias })
	canonSeen := slices.ContainsFunc(list, func(t models.Tag) bool { return t.Name == "feline" && !t.IsAlias })
	if !aliasSeen || !canonSeen {
		t.Errorf("expected both the alias (cat) and canonical (feline) in default listing; got %+v", list)
	}
}

func TestMergeTags_CanonicalAlreadyOnImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_both")

	tagAlias, _ := svc.GetOrCreateTag("alias_tag_overlap", catID)
	tagCanon, _ := svc.GetOrCreateTag("canon_tag_overlap", catID)

	mustAddTag(t, svc, imgID, tagAlias.ID, false, nil)
	mustAddTag(t, svc, imgID, tagCanon.ID, false, nil)

	mustMerge(t, svc, tagAlias.ID, tagCanon.ID)

	_, imgTags, _ := svc.GetImageTags(imgID)
	for _, it := range imgTags {
		if it.TagID == tagAlias.ID {
			t.Error("alias tag still on image")
		}
	}
}

func TestGetOrCreateTag_CaseNormalized(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	tag := mustTag(t, svc, "valid_name", catID)
	if tag.Name != "valid_name" {
		t.Errorf("Name = %q", tag.Name)
	}
}

func TestFindTag_NormalisesAndFollowsAnAlias(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	canon := mustTag(t, svc, "blue_sky", catID)
	mustCreateAlias(t, svc, "azure_sky", catID, canon.ID)
	before := countRows(t, database, "tags")

	for _, name := range []string{"blue_sky", "Blue_Sky", "blue sky", "azure_sky", "Azure_Sky"} {
		got, err := svc.FindTag(name, catID)
		if err != nil {
			t.Errorf("FindTag(%q): %v", name, err)
			continue
		}
		if got.ID != canon.ID {
			t.Errorf("FindTag(%q) = #%d %s, want the canonical #%d", name, got.ID, got.Name, canon.ID)
		}
	}
	if _, err := svc.FindTag("no_such_tag", catID); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("FindTag(no_such_tag) err = %v, want ErrTagNotFound", err)
	}
	if after := countRows(t, database, "tags"); after != before {
		t.Errorf("tags rows %d -> %d, want no new tag", before, after)
	}
}

func TestGetOrCreateTag_DanglingCanonicalErrors(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	canon := mustTag(t, svc, "canon", catID)
	if _, err := svc.CreateAliasFrom("alias_name", catID, canon.ID, "user"); err != nil {
		t.Fatal(err)
	}

	conn, err := database.Write.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`PRAGMA foreign_keys = OFF`,
		`UPDATE tags SET canonical_tag_id = 999999 WHERE name = 'alias_name'`,
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.GetOrCreateTag("alias_name", catID); err == nil {
		t.Error("expected an error for a dangling canonical, got the alias row")
	}
}

func TestValidateTagName_FoldsSpace(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	tag, err := svc.GetOrCreateTag("has space", catID)
	if err != nil {
		t.Fatalf("GetOrCreateTag(has space): %v", err)
	}
	if tag.Name != "has_space" {
		t.Errorf("name = %q, want has_space", tag.Name)
	}
}

func TestValidateTagName_ValidSpecialChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	_, err := svc.GetOrCreateTag("tag(with)special", catID)
	if err != nil {
		t.Errorf("special chars should be valid: %v", err)
	}
}

func TestValidateTagName_AllowsEmoticonChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	for _, name := range []string{">_<", "<3", "=3", "=w=", "^_^", "^o^", "nani?", ":<", ":>", ";_;"} {
		if _, err := svc.GetOrCreateTag(name, catID); err != nil {
			t.Errorf("GetOrCreateTag(%q) error: %v", name, err)
		}
	}
}

func TestValidateTagName_AllowsUnicodeAndPunctuation(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	for _, name := range []string{
		"copyright:girls'_frontline_2:_exilium",
		"jill_&_her_friends",
		"fate/grand_order",
		"a;b,c%d[e]f{g}h|i`j\\k",
		"café",
		"東方project",
		"español",
		"кемоно",
	} {
		if _, err := svc.GetOrCreateTag(name, catID); err != nil {
			t.Errorf("GetOrCreateTag(%q) error: %v", name, err)
		}
	}
	for _, name := range []string{`quo"te`, "wild*card"} {
		if _, err := svc.GetOrCreateTag(name, catID); err == nil {
			t.Errorf("GetOrCreateTag(%q) should be rejected", name)
		}
	}
}

func TestNormalizeName_FoldsOnlyReservedChars(t *testing.T) {
	cases := map[string]string{
		"girls' frontline": "girls'_frontline",
		"fate/grand order": "fate/grand_order",
		"jill & friends":   "jill_&_friends",
		`say "cheese"`:     "say_cheese",
		"five*star":        "five_star",
		"Hatsune Miku":     "hatsune_miku",
		"  trim  me  ":     "trim_me",
		" café ":           "café",
		"ぼっち・ざ・ろっく!users入り": "ぼっち・ざ・ろっく!users入り",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateTagName_TrimsEndsAndCountsRunes(t *testing.T) {
	if got, err := ValidateTagName("  red   hair  "); err != nil || got != "red_hair" {
		t.Errorf("ValidateTagName(%q) = %q, %v; want red_hair", "  red   hair  ", got, err)
	}

	if _, err := ValidateTagName(strings.Repeat("あ", 200)); err != nil {
		t.Errorf("200-rune name rejected: %v", err)
	}
	if _, err := ValidateTagName(strings.Repeat("あ", 201)); err == nil {
		t.Error("201-rune name should be rejected")
	}
}

func TestListTags_StaleFilterAndCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	img1 := insertTestImage(t, database, "st1")
	img2 := insertTestImage(t, database, "st2")
	mk := func(name string) int64 { return mkTagID(t, svc, name, catID) }
	fully, mixed, clean := mk("fully_stale"), mk("mixed_stale"), mk("clean_tag")
	ins := func(img, tag int64, stale int) {
		t.Helper()
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, stale) VALUES (?, ?, 0, ?)`, img, tag, stale)
		mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tag)
	}
	ins(img1, fully, 1)
	ins(img2, fully, 1)
	ins(img1, mixed, 1)
	ins(img2, mixed, 0)
	ins(img1, clean, 0)

	gone := insertTestImage(t, database, "st3")
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)
	fullyGone := mk("fully_stale_with_missing")
	ins(img1, fullyGone, 1)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, stale) VALUES (?, ?, 0, 1)`, gone, fullyGone)

	if n, err := svc.StaleUsageCount(); err != nil || n != 3 {
		t.Errorf("StaleUsageCount = %d, %v; want 3", n, err)
	}

	if n, err := svc.FullyStaleCount(); err != nil || n != 2 {
		t.Errorf("FullyStaleCount = %d, %v; want 2", n, err)
	}

	byName := func(f TagFilter) map[string]models.Tag {
		list, _ := mustListTags(t, svc, f)
		m := map[string]models.Tag{}
		for _, tg := range list {
			m[tg.Name] = tg
		}
		return m
	}

	all := byName(TagFilter{})
	if all["fully_stale"].StaleUsage != 2 || all["mixed_stale"].StaleUsage != 1 || all["clean_tag"].StaleUsage != 0 {
		t.Errorf("StaleUsage: fully=%d mixed=%d clean=%d, want 2/1/0",
			all["fully_stale"].StaleUsage, all["mixed_stale"].StaleUsage, all["clean_tag"].StaleUsage)
	}

	has := byName(TagFilter{Stale: "has"})
	if _, ok := has["fully_stale"]; !ok {
		t.Error("stale=has: missing fully_stale")
	}
	if _, ok := has["mixed_stale"]; !ok {
		t.Error("stale=has: missing mixed_stale")
	}
	if _, ok := has["clean_tag"]; ok {
		t.Error("stale=has: clean_tag should be excluded")
	}

	full := byName(TagFilter{Stale: "full"})
	if _, ok := full["fully_stale"]; !ok {
		t.Error("stale=full: missing fully_stale")
	}
	if _, ok := full["mixed_stale"]; ok {
		t.Error("stale=full: mixed_stale should be excluded (has a fresh usage)")
	}
	if _, ok := full["fully_stale_with_missing"]; !ok {
		t.Error("stale=full: a stale usage on a missing image should not hide a fully-stale tag")
	}
	_ = clean
}

func TestConflictsCountAndFilter(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	other := mustCreateCategory(t, svc, "mood", "#445566")
	for _, c := range []int64{general, other.ID} {
		mustTag(t, svc, "split_name", c)
	}
	mustTag(t, svc, "single_name", general)

	canonical := mustTag(t, svc, "alias_target", general)
	mustCreateAlias(t, svc, "single_name", other.ID, canonical.ID)

	n, err := svc.ConflictsCount()
	if err != nil {
		t.Fatal(err)
	}
	list, total := mustListTags(t, svc, TagFilter{ConflictsOnly: true, ShowZero: true})
	if total != 2 {
		t.Errorf("conflicts listing total = %d; want 2 (one row per category)", total)
	}
	if n != total {
		t.Errorf("ConflictsCount = %d but its own listing shows %d", n, total)
	}
	for _, tg := range list {
		if tg.Name != "split_name" {
			t.Errorf("conflicts listing carries %q; want only split_name", tg.Name)
		}
	}
}

func TestRemoveStaleTagsFromImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	img := insertTestImage(t, database, "rst")
	mk := func(name string) int64 { return mkTagID(t, svc, name, catID) }
	staleTag, freshTag := mk("stale_one"), mk("fresh_one")
	ins := func(tag int64, stale int) {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, stale) VALUES (?, ?, 0, ?)`, img, tag, stale)
		mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tag)
	}
	ins(staleTag, 1)
	ins(freshTag, 0)

	if _, err := svc.RemoveStaleTagsFromImage(img); err != nil {
		t.Fatal(err)
	}
	if imageHasTag(t, database, img, staleTag) {
		t.Error("stale tag should be removed")
	}
	if !imageHasTag(t, database, img, freshTag) {
		t.Error("fresh tag should remain")
	}
	if uc := mustGetTag(t, svc, staleTag).UsageCount; uc != 0 {
		t.Errorf("stale tag usage_count = %d, want 0", uc)
	}
}

func TestMergeFolded_ReportsARefusal(t *testing.T) {
	database, svc := setupTestDB(t)
	plain, _ := svc.GetOrCreateTag("folded_target", generalCategoryID(t, svc))
	ratingGeneral := ratingTagIDByName(t, database, "general")

	var wider int64
	if err := database.Write.QueryRow(`INSERT INTO tags (name, category_id, origin) VALUES ('general%', (SELECT category_id FROM tags WHERE id = ?), 'user') RETURNING id`, ratingGeneral).Scan(&wider); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO folded_tag_pairs (old_id, new_id, category_id, ambiguous) VALUES (?, ?, (SELECT category_id FROM tags WHERE id = ?), 0)`, ratingGeneral, wider, ratingGeneral)

	res, err := svc.MergeFolded(context.Background(), []int64{ratingGeneral})
	if err != nil {
		t.Fatalf("MergeFolded: %v", err)
	}
	if res.Merged != 0 || res.Skipped != 1 {
		t.Errorf("result = %+v, want 0 merged / 1 skipped", res)
	}
	if len(res.Refused) != 1 || !errors.Is(res.Refused[0], ErrRatingTagImmutable) {
		t.Errorf("refused = %v, want one ErrRatingTagImmutable", res.Refused)
	}
	res, err = svc.MergeFolded(context.Background(), []int64{plain.ID})
	if err != nil {
		t.Fatalf("MergeFolded on a stale pair: %v", err)
	}
	if res.Skipped != 1 || len(res.Refused) != 0 {
		t.Errorf("stale pair result = %+v, want 1 skipped and no reason", res)
	}
}

func TestFoldedDuplicates_ScanAndMerge(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	img := insertTestImage(t, database, "fd1")
	mk := func(name string) *models.Tag {
		tg := mustTag(t, svc, name, catID)
		return tg
	}
	oldTag := mk("fate_grand_order")
	newTag := mk("fate/grand_order")
	mk("blue_eyes")
	mk("girls_frontline")
	mk("girls__frontline")
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, oldTag.ID)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = 1 WHERE id = ?`, oldTag.ID)

	n, err := svc.ScanFoldedDuplicates()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("scan found %d folded originals, want 1", n)
	}
	if c, _ := svc.FoldedDuplicatesCount(); c != 1 {
		t.Errorf("FoldedDuplicatesCount = %d, want 1", c)
	}

	list, _ := mustListTags(t, svc, TagFilter{FoldedOnly: true})
	if len(list) != 1 || list[0].Name != "fate_grand_order" || list[0].FoldedInto != "fate/grand_order" {
		t.Errorf("folded listing = %+v, want fate_grand_order -> fate/grand_order", list)
	}

	res, err := svc.MergeFolded(context.Background(), []int64{oldTag.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged != 1 || res.Skipped != 0 || res.Cancelled || len(res.Refused) != 0 {
		t.Errorf("merge = %+v; want 1 merged, 0 skipped, no refusal, not cancelled", res)
	}
	if !imageHasTag(t, database, img, newTag.ID) {
		t.Error("image should carry the corrected tag after merge")
	}
	if !mustGetTag(t, svc, oldTag.ID).IsAlias {
		t.Error("old tag should be an alias after merge")
	}
	if c, _ := svc.FoldedDuplicatesCount(); c != 0 {
		t.Errorf("FoldedDuplicatesCount after merge = %d, want 0", c)
	}
}

func TestDeleteCategory_DeletesEmpty(t *testing.T) {
	_, svc := setupTestDB(t)

	cat := mustCreateCategory(t, svc, "empty_cat", "#123456")

	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "move", 0); err != nil {
		t.Fatalf("expected no error deleting empty category, got: %v", err)
	}

	cats, _ := svc.ListCategories()
	for _, c := range cats {
		if c.ID == cat.ID {
			t.Error("category still present after delete")
		}
	}
}

func TestRemoveTagFromImage_NotOnImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "rem_not_on")

	tag := mustTag(t, svc, "not_on_image", catID)
	if n, err := svc.RemoveTagFromImage(imgID, tag.ID); err != nil || n != 0 {
		t.Errorf("removing absent tag must be a no-op, got %d removed, err %v", n, err)
	}
	got := mustGetTag(t, svc, tag.ID)
	if got == nil {
		t.Fatal("tag should still exist (auto-prune only fires when a row is deleted)")
	}
	if got.UsageCount != 0 {
		t.Errorf("UsageCount = %d, want 0 after no-op remove", got.UsageCount)
	}
}

func TestListCategories(t *testing.T) {
	_, svc := setupTestDB(t)
	cats := mustListCategories(t, svc)
	if len(cats) == 0 {
		t.Error("expected built-in categories to be seeded")
	}
	hasGeneral := false
	for _, c := range cats {
		if c.Name == "general" {
			hasGeneral = true
		}
	}
	if !hasGeneral {
		t.Error("general category not found in ListCategories")
	}
}

func TestListTags_DefaultLimit(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	mustTag(t, svc, "default_lim_test", catID)

	tags, total, err := svc.ListTags(TagFilter{Limit: 0, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Error("expected at least 1 tag")
	}
	_ = tags
}

func TestListTags_WithPage(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	mustTag(t, svc, "page_tag_a", catID)
	mustTag(t, svc, "page_tag_b", catID)
	mustTag(t, svc, "page_tag_c", catID)

	p0, total := mustListTags(t, svc, TagFilter{Prefix: "page_tag_", Sort: "name", Limit: 1, PageIndex: 0, ShowZero: true})
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(p0) != 1 {
		t.Fatalf("page 0 len = %d, want 1", len(p0))
	}
	p1, _ := mustListTags(t, svc, TagFilter{Prefix: "page_tag_", Sort: "name", Limit: 1, PageIndex: 1, ShowZero: true})
	if len(p1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(p1))
	}
	if p0[0].Name == p1[0].Name {
		t.Errorf("page 0 and page 1 returned the same tag %q; pagination is broken", p0[0].Name)
	}
}

func TestGetTag_WithCanonical(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "canon_branch")

	tagAlias, _ := svc.GetOrCreateTag("alias_for_get", catID)
	tagCanon, _ := svc.GetOrCreateTag("canon_for_get", catID)
	mustAddTag(t, svc, imgID, tagAlias.ID, false, nil)
	mustMerge(t, svc, tagAlias.ID, tagCanon.ID)

	got := mustGetTag(t, svc, tagAlias.ID)
	if got.CanonicalTagID == nil {
		t.Error("expected CanonicalTagID to be set after merge")
	}
}

func TestValidateTagName_TooLong(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	long := strings.Repeat("a", 201)
	if _, err := svc.GetOrCreateTag(long, catID); err == nil {
		t.Error("expected error for tag name > 200 chars")
	}
	if _, err := svc.GetOrCreateTag(strings.Repeat("b", 200), catID); err != nil {
		t.Errorf("200-char name should be accepted, got %v", err)
	}
}

func TestValidateTagName_PunctuationOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	_, err := svc.GetOrCreateTag("---", catID)
	if err == nil {
		t.Error("expected error for punctuation-only tag name")
	}
}

func TestValidateTagName_AllowsColon(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	for _, name := range []string{":3", "nier:automata"} {
		if _, err := svc.GetOrCreateTag(name, catID); err != nil {
			t.Errorf("GetOrCreateTag(%q) error: %v", name, err)
		}
	}

	if _, err := svc.GetOrCreateTag("::-:", catID); err == nil {
		t.Error("expected all-punctuation name to be rejected even with colon allowed")
	}
}

func TestAdjacentTags(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	var ids []int64
	for _, name := range []string{"t_alpha", "t_beta", "t_gamma"} {
		tag := mustTag(t, svc, name, catID)
		ids = append(ids, tag.ID)
	}

	filter := TagFilter{Sort: "name", Order: "asc", ShowZero: true, Prefix: "t_"}
	prev, next, err := svc.AdjacentTags(filter, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != ids[0] || next == nil || *next != ids[2] {
		t.Errorf("beta neighbours = %v %v, want %d %d", prev, next, ids[0], ids[2])
	}

	prev, next, err = svc.AdjacentTags(filter, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil || next == nil || *next != ids[1] {
		t.Errorf("alpha neighbours = %v %v, want nil %d", prev, next, ids[1])
	}

	prev, next, err = svc.AdjacentTags(filter, ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != ids[1] || next != nil {
		t.Errorf("gamma neighbours = %v %v, want %d nil", prev, next, ids[1])
	}

	filter.Prefix = "z"
	prev, next, err = svc.AdjacentTags(filter, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil || next != nil {
		t.Errorf("out-of-set neighbours = %v %v, want nil nil", prev, next)
	}
}

func TestListTags_SortByName(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	mustTag(t, svc, "name_zzz", catID)
	mustTag(t, svc, "name_aaa", catID)

	tags, _ := mustListTags(t, svc, TagFilter{Sort: "name", Prefix: "name_", Limit: 100, ShowZero: true})
	if len(tags) < 2 {
		t.Fatalf("expected >= 2 tags, got %d", len(tags))
	}
	if tags[0].Name > tags[1].Name {
		t.Errorf("tags not sorted by name: %s > %s", tags[0].Name, tags[1].Name)
	}
}

func TestGetTag_NotFound(t *testing.T) {
	_, svc := setupTestDB(t)
	_, err := svc.GetTag(999999)
	if err == nil {
		t.Error("expected error for non-existent tag ID")
	}
}

func TestGetTags_ReadsEveryChunkAndLeavesOutAMissingID(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	var ids []int64
	for i := range 501 {
		tag, err := svc.GetOrCreateTag(fmt.Sprintf("bulk_%03d", i), catID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tag.ID)
	}
	got, err := svc.GetTags(append(ids, 999999))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("got %d tags, want %d", len(got), len(ids))
	}
	if last := got[ids[500]]; last == nil || last.Name != "bulk_500" || last.CategoryName != "general" {
		t.Errorf("tag past the first chunk = %+v", last)
	}
}

func TestSuggestTags_Empty(t *testing.T) {
	_, svc := setupTestDB(t)
	results, err := svc.SuggestTags("nonexistent_prefix_xyz", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 suggestions, got %d", len(results))
	}
}

func TestAddTagToImage_IsAuto(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "auto_conf")

	tag, _ := svc.GetOrCreateTag("auto_with_conf", catID)
	conf := 0.95
	mustAddTag(t, svc, imgID, tag.ID, true, &conf)

	_, imgTags := mustImageTags(t, svc, imgID)
	if len(imgTags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(imgTags))
	}
	if !imgTags[0].IsAuto {
		t.Error("expected IsAuto=true")
	}
	if imgTags[0].Confidence == nil || *imgTags[0].Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", imgTags[0].Confidence)
	}
}

func TestDeleteCategory_NotFound(t *testing.T) {
	_, svc := setupTestDB(t)
	err := svc.DeleteCategoryMoveOrDelete(999999, "move", 0)
	if err != ErrCategoryNotFound {
		t.Errorf("expected ErrCategoryNotFound, got %v", err)
	}
}

func TestCreateCategory_Duplicate(t *testing.T) {
	_, svc := setupTestDB(t)
	mustCreateCategory(t, svc, "dup_cat", "#000000")
	_, err := svc.CreateCategory("dup_cat", "#111111")
	if err == nil {
		t.Error("expected error for duplicate category name")
	}
}

func TestChangeTagCategory_RejectsDuplicateInTarget(t *testing.T) {
	_, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)
	characterID := categoryIDOf(t, svc, "character")

	a, _ := svc.GetOrCreateTag("cat", generalID)
	if _, err := svc.GetOrCreateTag("cat", characterID); err != nil {
		t.Fatalf("seed character:cat: %v", err)
	}

	err := svc.ChangeTagCategory(a.ID, characterID)
	if err == nil {
		t.Fatal("expected error when moving into a category that already has the same name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error message = %q, want 'already exists'", err.Error())
	}

	got, _ := svc.GetTag(a.ID)
	if got.CategoryID != generalID {
		t.Errorf("CategoryID = %d, want %d (tag should be unchanged on rejection)", got.CategoryID, generalID)
	}
}

func TestRenameAndMoveTag_RollsBackTheRenameOnACollision(t *testing.T) {
	_, svc := setupTestDB(t)
	general, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "character")
	orig := mustTag(t, svc, "orig", general)
	mustTag(t, svc, "clash", character)

	var coll *ErrCategoryCollision
	if err := svc.RenameAndMoveTag(orig.ID, "Clash", character); !errors.As(err, &coll) {
		t.Fatalf("err = %v, want a category collision", err)
	}
	if got := mustGetTag(t, svc, orig.ID); got.Name != "orig" || got.CategoryID != general {
		t.Errorf("tag = %s in %d, want orig left in %d", got.Name, got.CategoryID, general)
	}

	if err := svc.RenameAndMoveTag(orig.ID, "Moved Name", character); err != nil {
		t.Fatal(err)
	}
	if got := mustGetTag(t, svc, orig.ID); got.Name != "moved_name" || got.CategoryID != character {
		t.Errorf("tag = %s in %d, want moved_name in %d", got.Name, got.CategoryID, character)
	}
}

func TestRenameAndMoveTag_ChecksTheNewNameInTheTargetCategoryOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	general, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "character")
	foo := mustTag(t, svc, "foo", general)
	mustTag(t, svc, "zed", general)
	mustTag(t, svc, "foo", character)

	if err := svc.RenameAndMoveTag(foo.ID, "zed", character); err != nil {
		t.Fatalf("rename onto a name only the old category holds: %v", err)
	}
	if got := mustGetTag(t, svc, foo.ID); got.Name != "zed" || got.CategoryID != character {
		t.Errorf("tag = %s in %d, want zed in %d", got.Name, got.CategoryID, character)
	}

	bar := mustTag(t, svc, "bar", general)
	if err := svc.RenameAndMoveTagKeepAlias(bar.ID, "zed", character); !errors.As(err, new(*ErrCategoryCollision)) {
		t.Fatalf("err = %v, want a category collision", err)
	}
	if got := mustGetTag(t, svc, bar.ID); got.Name != "bar" || got.CategoryID != general {
		t.Errorf("tag = %s in %d, want bar left in %d", got.Name, got.CategoryID, general)
	}
}

func TestRenameAndMoveTagKeepAlias_LeavesTheOldNameInTheOldCategory(t *testing.T) {
	_, svc := setupTestDB(t)
	general, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "character")
	moved := mustTag(t, svc, "mr_smith", general)
	if err := svc.RenameAndMoveTagKeepAlias(moved.ID, "mr_smith", character); err != nil {
		t.Fatal(err)
	}
	if got := mustGetTag(t, svc, moved.ID); got.CategoryID != character {
		t.Errorf("CategoryID = %d, want %d", got.CategoryID, character)
	}
	alias, err := svc.FindTag("mr_smith", general)
	if err != nil || alias.ID != moved.ID {
		t.Fatalf("general:mr_smith = %+v, %v, want it to resolve to %d", alias, err, moved.ID)
	}

	renamed := mustTag(t, svc, "ms_smith", general)
	if err := svc.RenameAndMoveTagKeepAlias(renamed.ID, "smith_sister", character); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.FindTag("ms_smith", general); err != nil || got.ID != renamed.ID {
		t.Errorf("general:ms_smith = %+v, %v, want it to resolve to %d", got, err, renamed.ID)
	}

	aliasRow := mustCreateAlias(t, svc, "alias_row", general, moved.ID)
	if err := svc.RenameAndMoveTagKeepAlias(aliasRow.ID, "alias_row", character); !errors.Is(err, errKeepAliasOfAlias) {
		t.Errorf("err = %v, want errKeepAliasOfAlias", err)
	}
}

func TestChangeTagCategoryKeepAlias_MovesAnAliasPlainly(t *testing.T) {
	_, svc := setupTestDB(t)
	general, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "character")
	canon := mustTag(t, svc, "canon_tag", general)
	alias := mustCreateAlias(t, svc, "other_spelling", general, canon.ID)
	if err := svc.ChangeTagCategoryKeepAlias(alias.ID, character); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, svc.db, "tags WHERE name = 'other_spelling'"); n != 1 {
		t.Errorf("other_spelling rows = %d, want the one moved alias", n)
	}
	if err := svc.ChangeTagCategoryKeepAlias(canon.ID, general); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, svc.db, "tags WHERE name = 'canon_tag'"); n != 1 {
		t.Errorf("a same-category move kept an alias (%d rows)", n)
	}
}

func TestChangeTagCategory_MovingBackDropsTheTagsOwnAliasOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	general, character := generalCategoryID(t, svc), categoryIDOf(t, svc, "character")
	artist := categoryIDOf(t, svc, "artist")
	tag := mustTag(t, svc, "bar", character)
	if err := svc.ChangeTagCategoryKeepAlias(tag.ID, artist); err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangeTagCategory(tag.ID, character); err != nil {
		t.Fatalf("move back onto its own alias: %v", err)
	}
	if n := countRows(t, svc.db, "tags WHERE name = 'bar'"); n != 1 {
		t.Errorf("bar rows = %d, want the tag alone", n)
	}

	if err := svc.ChangeTagCategoryKeepAlias(tag.ID, artist); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangeTagCategoryKeepAlias(tag.ID, character); err != nil {
		t.Fatalf("keep-alias move back: %v", err)
	}
	if got, err := svc.FindTag("bar", artist); err != nil || got.ID != tag.ID {
		t.Errorf("artist:bar = %+v, %v, want it to resolve to %d", got, err, tag.ID)
	}

	other := mustTag(t, svc, "other", general)
	mustCreateAlias(t, svc, "bar", general, other.ID)
	if err := svc.ChangeTagCategory(tag.ID, general); !errors.As(err, new(*ErrCategoryCollision)) {
		t.Errorf("err = %v, want a collision with another tag's alias", err)
	}
}

func TestChangeTagCategory_SameCategoryNoop(t *testing.T) {
	_, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("cute", generalID)
	if err := svc.ChangeTagCategory(a.ID, generalID); err != nil {
		t.Errorf("expected no error moving to same category, got %v", err)
	}
}

func ratingTagIDByName(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

func TestGetOrCreateTag_RejectsNonCanonicalRating(t *testing.T) {
	_, svc := setupTestDB(t)
	if svc.RatingCategoryID() == 0 {
		t.Fatal("rating category not seeded")
	}
	if _, err := svc.GetOrCreateTag("ambiguous", svc.RatingCategoryID()); err != ErrNonCanonicalRating {
		t.Errorf("err = %v, want ErrNonCanonicalRating", err)
	}
	tag, err := svc.GetOrCreateTag("explicit", svc.RatingCategoryID())
	if err != nil {
		t.Fatalf("canonical name should be allowed: %v", err)
	}
	if tag.CategoryID != svc.RatingCategoryID() {
		t.Errorf("tag.CategoryID = %d, want %d", tag.CategoryID, svc.RatingCategoryID())
	}
}

func TestGetOrCreateTagsFrom_RejectsNonCanonicalRating(t *testing.T) {
	database, svc := setupTestDB(t)
	ratingID := svc.RatingCategoryID()
	if _, err := svc.GetOrCreateTagsFrom([]string{"explicit", "ambiguous"}, ratingID, "user"); err != ErrNonCanonicalRating {
		t.Errorf("err = %v, want ErrNonCanonicalRating", err)
	}
	if n := countRows(t, database, "tags WHERE name = 'ambiguous'"); n != 0 {
		t.Errorf("a refused batch left %d row(s) behind", n)
	}
	if ids, err := svc.GetOrCreateTagsFrom([]string{"explicit", "general"}, ratingID, "user"); err != nil || len(ids) != 2 {
		t.Errorf("canonical names = %v, %v; want two ids", ids, err)
	}
}

func TestRenameTag_RejectedOnRating(t *testing.T) {
	database, svc := setupTestDB(t)
	id := ratingTagIDByName(t, database, "explicit")
	if err := svc.RenameTag(id, "very_explicit"); err != ErrRatingTagImmutable {
		t.Errorf("err = %v, want ErrRatingTagImmutable", err)
	}
}

func TestDeleteTag_RatingStripsUsageButKeepsRow(t *testing.T) {
	database, svc := setupTestDB(t)
	ratingID := ratingTagIDByName(t, database, "general")
	imageID := insertTestImage(t, database, "rated.png")
	mustAddTag(t, svc, imageID, ratingID, false, nil)
	if err := svc.DeleteTag(ratingID); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	if _, err := svc.GetTag(ratingID); err != nil {
		t.Errorf("rating tag row should still exist, got err: %v", err)
	}
	_, tags := mustImageTags(t, svc, imageID)
	for _, tg := range tags {
		if tg.TagID == ratingID {
			t.Errorf("rating row still on image %d", imageID)
		}
	}
	tag, _ := svc.GetTag(ratingID)
	if tag.UsageCount != 0 {
		t.Errorf("usage_count = %d, want 0", tag.UsageCount)
	}
}

func TestMergeTags_RejectedOnRatingSource(t *testing.T) {
	database, svc := setupTestDB(t)
	gen := ratingTagIDByName(t, database, "general")
	exp := ratingTagIDByName(t, database, "explicit")
	if err := svc.MergeTags(gen, exp); err != ErrRatingTagImmutable {
		t.Errorf("err = %v, want ErrRatingTagImmutable", err)
	}
	other, _ := svc.GetOrCreateTag("ordinary", generalCategoryID(t, svc))
	if err := svc.MergeTags(gen, other.ID); err != ErrRatingTagImmutable {
		t.Errorf("rating source into a plain tag: err = %v, want ErrRatingTagImmutable", err)
	}
}

func ratingNamesOnImage(t *testing.T, svc *Service, imageID int64) []string {
	t.Helper()
	_, rows := mustImageTags(t, svc, imageID)
	var names []string
	for _, r := range rows {
		if r.Category == "rating" {
			names = append(names, r.TagName)
		}
	}
	return names
}

func TestMergeTags_IntoRatingKeepsHighestPerImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	exp := ratingTagIDByName(t, database, "explicit")
	gen := ratingTagIDByName(t, database, "general")

	unrated := insertTestImage(t, database, "merge_rating_unrated")
	preRated := insertTestImage(t, database, "merge_rating_prerated")
	nsfw, _ := svc.GetOrCreateTag("nsfw", catID)
	for _, id := range []int64{unrated, preRated} {
		mustAddTag(t, svc, id, nsfw.ID, false, nil)
	}
	mustAddTag(t, svc, preRated, gen, false, nil)

	mustMerge(t, svc, nsfw.ID, exp)

	for _, id := range []int64{unrated, preRated} {
		if got := ratingNamesOnImage(t, svc, id); !reflect.DeepEqual(got, []string{"explicit"}) {
			t.Errorf("ratings on image %d = %v, want [explicit]", id, got)
		}
	}
	alias := mustGetTag(t, svc, nsfw.ID)
	if !alias.IsAlias || alias.UsageCount != 0 {
		t.Errorf("source row: is_alias = %v, usage = %d, want true / 0", alias.IsAlias, alias.UsageCount)
	}
	expTag, _ := svc.GetTag(exp)
	if expTag.UsageCount != 2 {
		t.Errorf("explicit usage_count = %d, want 2", expTag.UsageCount)
	}
	genTag, _ := svc.GetTag(gen)
	if genTag.UsageCount != 0 {
		t.Errorf("general usage_count = %d, want 0 (pruned)", genTag.UsageCount)
	}
	resolved, err := svc.GetOrCreateTag("nsfw", catID)
	if err != nil {
		t.Fatalf("GetOrCreateTag after merge: %v", err)
	}
	if resolved.ID != exp {
		t.Errorf("resolved id = %d, want %d", resolved.ID, exp)
	}
}

func TestMergeTags_IntoRatingNeverLowersAnImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	gen := ratingTagIDByName(t, database, "general")
	exp := ratingTagIDByName(t, database, "explicit")

	imageID := insertTestImage(t, database, "merge_rating_no_downgrade")
	safe, _ := svc.GetOrCreateTag("safe", catID)
	mustAddTag(t, svc, imageID, safe.ID, false, nil)
	mustAddTag(t, svc, imageID, exp, false, nil)

	mustMerge(t, svc, safe.ID, gen)

	if got := ratingNamesOnImage(t, svc, imageID); !reflect.DeepEqual(got, []string{"explicit"}) {
		t.Errorf("ratings = %v, want [explicit]", got)
	}
	var rank int
	if err := database.Read.QueryRow(`SELECT rating_rank FROM images WHERE id = ?`, imageID).Scan(&rank); err != nil {
		t.Fatalf("read rating_rank: %v", err)
	}
	if rank != contract.RatingRank("explicit") {
		t.Errorf("rating_rank = %d, want %d", rank, contract.RatingRank("explicit"))
	}
}

func TestCreateAlias_IntoRatingCanonical(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	exp := ratingTagIDByName(t, database, "explicit")

	alias := mustCreateAlias(t, svc, "nsfw", catID, exp)
	if !alias.IsAlias || alias.CategoryID != catID {
		t.Errorf("alias row: is_alias = %v, category = %d, want true / %d", alias.IsAlias, alias.CategoryID, catID)
	}
	if _, err := svc.CreateAlias("nsfw2", svc.RatingCategoryID(), exp); err != ErrRatingCategoryClosed {
		t.Errorf("alias inside the rating category: err = %v, want ErrRatingCategoryClosed", err)
	}
}

func TestChangeTagCategory_RejectedOnRating(t *testing.T) {
	database, svc := setupTestDB(t)
	expID := ratingTagIDByName(t, database, "explicit")
	if err := svc.ChangeTagCategory(expID, generalCategoryID(t, svc)); err != ErrRatingTagImmutable {
		t.Errorf("moving rating tag out: err = %v, want ErrRatingTagImmutable", err)
	}
	other, _ := svc.GetOrCreateTag("not_a_rating", generalCategoryID(t, svc))
	if err := svc.ChangeTagCategory(other.ID, svc.RatingCategoryID()); err != ErrRatingCategoryClosed {
		t.Errorf("moving in to rating: err = %v, want ErrRatingCategoryClosed", err)
	}
}

func insertRatingCategoryRow(t *testing.T, database *db.DB, svc *Service, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, origin) VALUES (?, ?, 'import') RETURNING id`,
		name, svc.RatingCategoryID(),
	).Scan(&id); err != nil {
		t.Fatalf("seed rating-category row %q: %v", name, err)
	}
	return id
}

func TestRatingLock_ImportedNonCanonicalRowIsRepairable(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)

	junk := insertRatingCategoryRow(t, database, svc, "safe")
	if err := svc.MergeTags(junk, ratingTagIDByName(t, database, "general")); err != nil {
		t.Errorf("merge an imported rating row into its canonical: %v", err)
	}

	moveMe := insertRatingCategoryRow(t, database, svc, "sfw")
	if err := svc.ChangeTagCategory(moveMe, general); err != nil {
		t.Errorf("move an imported rating row out: %v", err)
	}

	renameMe := insertRatingCategoryRow(t, database, svc, "nsfw")
	if err := svc.RenameTag(renameMe, "nsfw_import"); err != nil {
		t.Errorf("rename an imported rating row: %v", err)
	}

	dropMe := insertRatingCategoryRow(t, database, svc, "rating_junk")
	if err := svc.DeleteTag(dropMe); err != nil {
		t.Fatalf("delete an imported rating row: %v", err)
	}
	if _, err := svc.GetTag(dropMe); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("imported rating row survived delete: err = %v, want ErrTagNotFound", err)
	}

	for _, name := range contract.RatingLevels {
		id := ratingTagIDByName(t, database, name)
		if err := svc.RenameTag(id, name+"_x"); err != ErrRatingTagImmutable {
			t.Errorf("rename %s: err = %v, want ErrRatingTagImmutable", name, err)
		}
		if err := svc.ChangeTagCategory(id, general); err != ErrRatingTagImmutable {
			t.Errorf("move %s out: err = %v, want ErrRatingTagImmutable", name, err)
		}
		if err := svc.DeleteTag(id); err != nil {
			t.Errorf("delete %s: %v", name, err)
		}
		if _, err := svc.GetTag(id); err != nil {
			t.Errorf("canonical %s row should survive delete: %v", name, err)
		}
	}
}

func TestRemoveUserTags_KeepsSourceTagsAndTheirImpliedRows(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "remove_user_keeps_source_implied")

	parent, _ := svc.GetOrCreateTag("miko", catID)
	implied, _ := svc.GetOrCreateTag("shrine_maiden", catID)
	userTag, _ := svc.GetOrCreateTag("my_tag", catID)

	mustAddImply(t, svc, parent.ID, implied.ID)
	if _, err := svc.SyncSourceTags(imgID, []int64{parent.ID}, "danbooru", false); err != nil {
		t.Fatalf("SyncSourceTags: %v", err)
	}
	mustAddTag(t, svc, imgID, userTag.ID, false, nil)

	if _, err := svc.RemoveUserTagsFromImage(imgID); err != nil {
		t.Fatalf("RemoveUserTagsFromImage: %v", err)
	}

	if imageHasTag(t, database, imgID, userTag.ID) {
		t.Error("the operator's own tag should have been removed")
	}
	if !imageHasTag(t, database, imgID, parent.ID) {
		t.Error("danbooru's tag was wrongly removed as a user tag")
	}
	if !imageHasTag(t, database, imgID, implied.ID) {
		t.Error("the source-implied row was wrongly removed as a user tag")
	}
}

func TestMergeTags_RepointsImplicationsAndKeepsImpliedRowsCleanable(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_implication_repoint")

	a, _ := svc.GetOrCreateTag("merge_a", catID)
	b, _ := svc.GetOrCreateTag("merge_b", catID)
	c, _ := svc.GetOrCreateTag("merge_c", catID)

	mustAddImply(t, svc, a.ID, c.ID)
	mustAddTag(t, svc, imgID, a.ID, false, nil)

	mustMerge(t, svc, a.ID, b.ID)

	var parent, implied int64
	if err := database.Read.QueryRow(
		`SELECT parent_tag_id, implied_tag_id FROM tag_implications WHERE implied_tag_id = ?`, c.ID,
	).Scan(&parent, &implied); err != nil {
		t.Fatalf("expected one implication after merge: %v", err)
	}
	if parent != b.ID {
		t.Errorf("implication parent = %d, want %d (canonical)", parent, b.ID)
	}

	aliasEdges := edgesTouching(t, database, a.ID)
	if aliasEdges != 0 {
		t.Errorf("alias still referenced by %d tag_implications row(s)", aliasEdges)
	}

	if _, err := svc.RemoveTagFromImage(imgID, b.ID); err != nil {
		t.Fatalf("RemoveTagFromImage: %v", err)
	}

	_, imgTags := mustImageTags(t, svc, imgID)
	if len(imgTags) != 0 {
		t.Errorf("image still carries %d tag(s) after removing the only user tag: %+v", len(imgTags), imgTags)
	}
}

func TestListTags_PrefixEscapesLikeWildcards(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	mustTag(t, svc, "a_b", catID)
	mustTag(t, svc, "axb", catID)
	tags, _ := mustListTags(t, svc, TagFilter{Prefix: "a_b", Limit: 50, ShowZero: true})
	if len(tags) != 1 || tags[0].Name != "a_b" {
		names := make([]string, len(tags))
		for i, tg := range tags {
			names[i] = tg.Name
		}
		t.Errorf("prefix a_b matched %v, want only [a_b]", names)
	}
}

func TestDeleteCategoryDeleteAll_SweepsImpliedClosure(t *testing.T) {
	database, svc := setupTestDB(t)
	genID := generalCategoryID(t, svc)
	cat := mustCreateCategory(t, svc, "doomed", "#aabbcc")
	imgID := insertTestImage(t, database, "delall_closure")

	parent, _ := svc.GetOrCreateTag("doomed_parent", cat.ID)
	child, _ := svc.GetOrCreateTag("surviving_child", genID)
	mustAddImply(t, svc, parent.ID, child.ID)
	mustAddTag(t, svc, imgID, parent.ID, false, nil)
	implied := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imgID, child.ID)
	if implied != 1 {
		t.Fatalf("setup: implied child row = %d, want 1", implied)
	}

	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "delete_all", 0); err != nil {
		t.Fatalf("DeleteCategoryMoveOrDelete: %v", err)
	}

	rows := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", imgID, child.ID)
	if rows != 0 {
		t.Errorf("orphaned implied child row remains: %d", rows)
	}
	if usage := mustGetTag(t, svc, child.ID).UsageCount; usage != 0 {
		t.Errorf("child usage_count = %d, want 0", usage)
	}
}

func TestMergeTags_FansCanonicalImplicationsOntoMigratedRows(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_implication_fanout")

	canonical, _ := svc.GetOrCreateTag("canon", catID)
	implied, _ := svc.GetOrCreateTag("imp_child", catID)
	alias, _ := svc.GetOrCreateTag("alias_src", catID)

	mustAddImply(t, svc, canonical.ID, implied.ID)
	mustAddTag(t, svc, imgID, alias.ID, false, nil)

	mustMerge(t, svc, alias.ID, canonical.ID)

	_, imgTags := mustImageTags(t, svc, imgID)
	carried := map[int64]bool{}
	for _, t := range imgTags {
		carried[t.TagID] = true
	}
	if !carried[canonical.ID] {
		t.Errorf("canonical tag missing from image after merge: %+v", imgTags)
	}
	if !carried[implied.ID] {
		t.Errorf("canonical's implied child missing from image after merge: %+v", imgTags)
	}
}

func TestMergeTags_RepointsInboundImplications(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	parent, _ := svc.GetOrCreateTag("merge_in_parent", catID)
	a, _ := svc.GetOrCreateTag("merge_in_a", catID)
	b, _ := svc.GetOrCreateTag("merge_in_b", catID)

	mustAddImply(t, svc, parent.ID, a.ID)
	mustMerge(t, svc, a.ID, b.ID)

	var implied int64
	if err := database.Read.QueryRow(
		`SELECT implied_tag_id FROM tag_implications WHERE parent_tag_id = ?`, parent.ID,
	).Scan(&implied); err != nil {
		t.Fatalf("expected one implication after merge: %v", err)
	}
	if implied != b.ID {
		t.Errorf("implied = %d, want %d (canonical)", implied, b.ID)
	}
	aliasEdges := edgesTouching(t, database, a.ID)
	if aliasEdges != 0 {
		t.Errorf("alias still referenced by %d tag_implications row(s)", aliasEdges)
	}
}

func TestMergeTags_PromotesCanonicalImpliedRow(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_promote_implied")

	a, _ := svc.GetOrCreateTag("merge_promote_a", catID)
	c, _ := svc.GetOrCreateTag("merge_promote_c", catID)

	mustAddImply(t, svc, a.ID, c.ID)
	mustAddTag(t, svc, imgID, a.ID, false, nil)
	mustMerge(t, svc, a.ID, c.ID)

	var isImplied, isAuto int
	if err := database.Read.QueryRow(
		`SELECT is_implied, is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`,
		imgID, c.ID,
	).Scan(&isImplied, &isAuto); err != nil {
		t.Fatalf("expected canonical on image: %v", err)
	}
	if isImplied != 0 {
		t.Errorf("canonical row is_implied = %d, want 0 (user-owned)", isImplied)
	}
	if isAuto != 0 {
		t.Errorf("canonical row is_auto = %d, want 0 (matches alias side)", isAuto)
	}
}

func TestMergeTags_MovedRowsKeepTheirDateAndStaleFlag(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	moved := insertTestImage(t, database, "merge_keep_moved")
	promoted := insertTestImage(t, database, "merge_keep_promoted")

	a, _ := svc.GetOrCreateTag("merge_keep_a", catID)
	c, _ := svc.GetOrCreateTag("merge_keep_c", catID)
	p, _ := svc.GetOrCreateTag("merge_keep_p", catID)
	child, _ := svc.GetOrCreateTag("merge_keep_child", catID)
	parent, _ := svc.GetOrCreateTag("merge_keep_parent", catID)

	mustAddImply(t, svc, p.ID, c.ID)
	mustAddImply(t, svc, a.ID, child.ID)
	mustAddImply(t, svc, parent.ID, a.ID)
	mustAddTag(t, svc, moved, a.ID, false, nil)
	mustAddTag(t, svc, promoted, p.ID, false, nil)
	mustAddTag(t, svc, promoted, a.ID, false, nil)

	const old = "2020-01-02T03:04:05Z"
	for _, q := range []string{
		`UPDATE image_tags SET tagger_name = 'src', stale = 1, created_at = ?1 WHERE tag_id = ?2`,
		`UPDATE tag_implications SET stale = 1, created_at = ?1 WHERE ?2 IN (parent_tag_id, implied_tag_id)`,
	} {
		if _, err := database.Write.Exec(q, old, a.ID); err != nil {
			t.Fatal(err)
		}
	}

	mustMerge(t, svc, a.ID, c.ID)

	row := func(imageID int64) (created string, stale int) {
		t.Helper()
		if err := database.Read.QueryRow(
			`SELECT created_at, stale FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, c.ID,
		).Scan(&created, &stale); err != nil {
			t.Fatal(err)
		}
		return created, stale
	}
	if created, stale := row(moved); created != old || stale != 1 {
		t.Errorf("moved row = %s stale=%d, want %s stale=1", created, stale, old)
	}
	if _, stale := row(promoted); stale != 1 {
		t.Errorf("promoted row stale = %d, want 1", stale)
	}
	for _, edge := range [][2]int64{{c.ID, child.ID}, {parent.ID, c.ID}} {
		var created string
		var stale int
		if err := database.Read.QueryRow(
			`SELECT created_at, stale FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`, edge[0], edge[1],
		).Scan(&created, &stale); err != nil {
			t.Fatal(err)
		}
		if created != old || stale != 1 {
			t.Errorf("edge %v = %s stale=%d, want %s stale=1", edge, created, stale, old)
		}
	}
}

func TestMergeTags_RefusesARepointThatClosesACycle(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	trio := func(prefix string) (alias, mid, canonical int64) {
		return mustTag(t, svc, prefix+"_alias", catID).ID, mustTag(t, svc, prefix+"_mid", catID).ID, mustTag(t, svc, prefix+"_canon", catID).ID
	}
	unchanged := func(label string, alias int64) {
		t.Helper()
		if got := mustGetTag(t, svc, alias); got.IsAlias || edgesTouching(t, database, alias) != 1 {
			t.Errorf("%s: the refused merge changed the alias (is_alias=%v, edges=%d)", label, got.IsAlias, edgesTouching(t, database, alias))
		}
	}

	alias, mid, canonical := trio("cycle_out")
	mustAddImply(t, svc, alias, mid)
	mustAddImply(t, svc, mid, canonical)
	if err := svc.MergeTags(alias, canonical); !errors.Is(err, ErrImplicationCycle) {
		t.Errorf("outbound: MergeTags err = %v, want ErrImplicationCycle", err)
	}
	unchanged("outbound", alias)

	alias, mid, canonical = trio("cycle_in")
	mustAddImply(t, svc, canonical, mid)
	mustAddImply(t, svc, mid, alias)
	if err := svc.MergeTags(alias, canonical); !errors.Is(err, ErrImplicationCycle) {
		t.Errorf("inbound: MergeTags err = %v, want ErrImplicationCycle", err)
	}
	unchanged("inbound", alias)

	alias, mid, canonical = trio("cycle_upgrade")
	mustAddImply(t, svc, alias, mid)
	mustAddImply(t, svc, mid, canonical)
	if _, err := svc.CreateAlias("cycle_upgrade_alias", catID, canonical); !errors.Is(err, ErrImplicationCycle) {
		t.Errorf("upgrade: CreateAlias err = %v, want ErrImplicationCycle", err)
	}
	unchanged("upgrade", alias)
}

func TestMergeTags_FansGainedImplicationsOntoTheCanonicalsImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	catImage := insertTestImage(t, database, "merge_gain_cat")
	kittyImage := insertTestImage(t, database, "merge_gain_kitty")

	cat := mustTag(t, svc, "merge_gain_cat", catID)
	kitty := mustTag(t, svc, "merge_gain_kitty", catID)
	animal := mustTag(t, svc, "merge_gain_animal", catID)
	feline := mustTag(t, svc, "merge_gain_feline", catID)
	mustAddImply(t, svc, kitty.ID, animal.ID)
	mustAddImply(t, svc, cat.ID, feline.ID)
	mustAddTag(t, svc, catImage, cat.ID, true, nil)
	mustAddTag(t, svc, kittyImage, kitty.ID, true, nil)

	mustMerge(t, svc, kitty.ID, cat.ID)

	for _, want := range []struct {
		image, tag int64
	}{{catImage, animal.ID}, {kittyImage, feline.ID}, {kittyImage, animal.ID}} {
		var isAuto int
		if err := database.Read.QueryRow(
			`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1`, want.image, want.tag,
		).Scan(&isAuto); err != nil {
			t.Errorf("image %d tag %d: %v", want.image, want.tag, err)
		} else if isAuto != 1 {
			t.Errorf("image %d tag %d: is_auto = %d, want 1", want.image, want.tag, isAuto)
		}
	}
	if got := mustGetTag(t, svc, animal.ID).UsageCount; got != 2 {
		t.Errorf("animal usage = %d, want 2", got)
	}
}

func TestCreateAlias_UpgradeInPlace_FansRepointedImplications(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "alias_upgrade_fan")

	canon := mustTag(t, svc, "alias_upgrade_canon", catID)
	unused := mustTag(t, svc, "alias_upgrade_unused", catID)
	child := mustTag(t, svc, "alias_upgrade_child", catID)
	mustAddImply(t, svc, unused.ID, child.ID)
	mustAddTag(t, svc, imgID, canon.ID, false, nil)

	mustCreateAlias(t, svc, "alias_upgrade_unused", catID, canon.ID)
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imgID, child.ID); n != 1 {
		t.Errorf("child rows on the canonical's image = %d, want 1", n)
	}
}

func TestDeleteTag_SweepsImpliedClosureOnCarrierImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "delete_implied_sweep")

	parent, _ := svc.GetOrCreateTag("delete_parent", catID)
	implied, _ := svc.GetOrCreateTag("delete_implied", catID)

	mustAddImply(t, svc, parent.ID, implied.ID)
	mustAddTag(t, svc, imgID, parent.ID, false, nil)

	if err := svc.DeleteTag(parent.ID); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	_, imgTags := mustImageTags(t, svc, imgID)
	if len(imgTags) != 0 {
		t.Errorf("image still carries %d tag(s) after deleting the only parent: %+v", len(imgTags), imgTags)
	}

	got := mustGetTag(t, svc, implied.ID)
	if got.UsageCount != 0 {
		t.Errorf("implied tag usage_count = %d, want 0", got.UsageCount)
	}
}

func TestDeleteTag_RatingStripSweepsItsImpliedClosure(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "rating_strip_sweep")
	exp := ratingTagIDByName(t, database, "explicit")
	nsfw := mustTag(t, svc, "rating_strip_nsfw", catID)
	mustAddImply(t, svc, exp, nsfw.ID)
	mustAddTag(t, svc, imgID, exp, false, nil)

	if err := svc.DeleteTag(exp); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ?", imgID); n != 0 {
		t.Errorf("image keeps %d row(s) after the rating strip, want 0", n)
	}
	if got := mustGetTag(t, svc, nsfw.ID).UsageCount; got != 0 {
		t.Errorf("implied tag usage_count = %d, want 0", got)
	}
}

func TestDeleteTag_SweepsOnlyTheDeletedTagsImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	carrier := insertTestImage(t, database, "delete_scope_carrier")
	bystander := insertTestImage(t, database, "delete_scope_bystander")
	parent := mustTag(t, svc, "delete_scope_parent", catID)
	child := mustTag(t, svc, "delete_scope_child", catID)
	mustAddImply(t, svc, parent.ID, child.ID)
	mustAddTag(t, svc, carrier, parent.ID, false, nil)
	mustAddTag(t, svc, bystander, parent.ID, false, nil)

	if _, err := database.Write.Exec(`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, bystander, parent.ID); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteTag(parent.ID); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", carrier, child.ID); n != 0 {
		t.Errorf("the carrier keeps its implied child (%d row)", n)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", bystander, child.ID); n != 1 {
		t.Errorf("the bystander's orphan rows = %d, want 1", n)
	}
}

func TestDeleteTag_SweepsAChildWhoseLastParentIsSweptAfterIt(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "delete_implied_chain")

	child, _ := svc.GetOrCreateTag("delete_chain_child", catID)
	mid, _ := svc.GetOrCreateTag("delete_chain_mid", catID)
	root, _ := svc.GetOrCreateTag("delete_chain_root", catID)
	for _, edge := range [][2]int64{{root.ID, mid.ID}, {root.ID, child.ID}, {mid.ID, child.ID}} {
		mustAddImply(t, svc, edge[0], edge[1])
	}
	mustAddTag(t, svc, imgID, root.ID, false, nil)

	if err := svc.DeleteTag(root.ID); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	_, imgTags := mustImageTags(t, svc, imgID)
	for _, it := range imgTags {
		t.Errorf("implied row left with no parent: %s", it.TagName)
	}
	for _, id := range []int64{mid.ID, child.ID} {
		if got := mustGetTag(t, svc, id); got.UsageCount != 0 {
			t.Errorf("tag %s usage_count = %d, want 0", got.Name, got.UsageCount)
		}
	}
}

func TestDeleteTag_AliasChainAndCycle(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	insertAlias := func(name string, canonical any) int64 {
		t.Helper()
		var id int64
		err := database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin)
			 VALUES (?, ?, 1, ?, 0, 'user') RETURNING id`,
			name, catID, canonical,
		).Scan(&id)
		if err != nil {
			t.Fatalf("insert alias %s: %v", name, err)
		}
		return id
	}
	tagCount := func(name string) int {
		t.Helper()
		n := countRows(t, database, "tags WHERE name = ?", name)
		return n
	}

	canon, _ := svc.GetOrCreateTag("chain_canon", catID)
	aliasA := mustCreateAlias(t, svc, "chain_a", catID, canon.ID)
	bID := insertAlias("chain_b", aliasA.ID)
	insertAlias("chain_d", bID)

	if err := svc.DeleteTag(aliasA.ID); err != nil {
		t.Fatalf("DeleteTag on the chained alias: %v", err)
	}
	for _, name := range []string{"chain_a", "chain_b", "chain_d"} {
		if n := tagCount(name); n != 0 {
			t.Errorf("%s still present after delete", name)
		}
	}
	if n := tagCount("chain_canon"); n != 1 {
		t.Errorf("chain_canon count = %d, want 1", n)
	}

	cycleA := insertAlias("cycle_a", nil)
	cycleB := insertAlias("cycle_b", cycleA)
	mustExec(t, database.Write, `UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, cycleB, cycleA)

	if err := svc.DeleteTag(cycleA); err != nil {
		t.Fatalf("DeleteTag on the alias cycle: %v", err)
	}
	for _, name := range []string{"cycle_a", "cycle_b"} {
		if n := tagCount(name); n != 0 {
			t.Errorf("%s still present after delete", name)
		}
	}
}

func TestCreateAlias_UpgradeInPlace_RepointsImplications(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	a, _ := svc.GetOrCreateTag("alias_up_a", catID)
	b, _ := svc.GetOrCreateTag("alias_up_b", catID)
	c, _ := svc.GetOrCreateTag("alias_up_c", catID)

	mustAddImply(t, svc, a.ID, c.ID)

	mustCreateAlias(t, svc, "alias_up_a", catID, b.ID)

	var parent, implied int64
	if err := database.Read.QueryRow(
		`SELECT parent_tag_id, implied_tag_id FROM tag_implications WHERE implied_tag_id = ?`, c.ID,
	).Scan(&parent, &implied); err != nil {
		t.Fatalf("expected one implication after alias: %v", err)
	}
	if parent != b.ID {
		t.Errorf("implication parent = %d, want %d (canonical)", parent, b.ID)
	}
	aliasEdges := edgesTouching(t, database, a.ID)
	if aliasEdges != 0 {
		t.Errorf("alias still referenced by %d tag_implications row(s)", aliasEdges)
	}
}

func TestCreateAlias_MissingOnlyCarrierRoutesThroughMerge(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	stranded, _ := svc.GetOrCreateTag("stranded", catID)
	canonical, _ := svc.GetOrCreateTag("stranded_canon", catID)
	imgID := insertTestImage(t, database, "missing-carrier")
	mustAddTag(t, svc, imgID, stranded.ID, false, nil)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, imgID)
	if _, err := svc.RecalcCount(); err != nil {
		t.Fatal(err)
	}
	if usage := mustGetTag(t, svc, stranded.ID).UsageCount; usage != 0 {
		t.Fatalf("usage_count = %d, want 0 so the branch under test is the one reached", usage)
	}

	if _, err := svc.CreateAlias("stranded", catID, canonical.ID); err != ErrAliasNameInUse {
		t.Fatalf("CreateAlias over a missing-only carrier: err = %v, want ErrAliasNameInUse", err)
	}
	var isAlias int
	if err := database.Read.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, stranded.ID).Scan(&isAlias); err != nil {
		t.Fatal(err)
	}
	if isAlias != 0 {
		t.Error("tag was upgraded to an alias with its image_tags rows left behind")
	}
}

func TestRatingTagIDsAbove(t *testing.T) {
	_, svc := setupTestDB(t)
	if got := svc.RatingTagIDsAbove("explicit"); len(got) != 0 {
		t.Errorf("explicit ceiling: got %d ids, want 0", len(got))
	}
	if got := svc.RatingTagIDsAbove("general"); len(got) != 3 {
		t.Errorf("general ceiling: got %d ids, want 3 (sensitive/questionable/explicit)", len(got))
	}
	if got := svc.RatingTagIDsAbove("sensitive"); len(got) != 2 {
		t.Errorf("sensitive ceiling: got %d ids, want 2 (questionable/explicit)", len(got))
	}
	if got := svc.RatingTagIDsAbove(""); len(got) != 0 {
		t.Errorf("empty ceiling: got %d ids, want 0", len(got))
	}
}

func TestAddRating_PrunesLowerRanksOnAdd(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "ratings.png")

	imageRatingNames := func() []string { return ratingNamesOnImage(t, svc, imageID) }

	for _, level := range []string{"general", "sensitive", "questionable", "explicit"} {
		id := ratingTagIDByName(t, database, level)
		mustAddTag(t, svc, imageID, id, true, nil)
		got := imageRatingNames()
		if len(got) != 1 || got[0] != level {
			t.Fatalf("after adding %s: image carries %v, want only [%s]", level, got, level)
		}
	}

	genID := ratingTagIDByName(t, database, "general")
	mustAddTag(t, svc, imageID, genID, true, nil)
	got := imageRatingNames()
	if len(got) != 1 || got[0] != "explicit" {
		t.Fatalf("lower-rank add should be pruned; image carries %v, want only [explicit]", got)
	}
}

func TestAddRating_UsageCountsTrackPrune(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "ratings_usage.png")

	genID := ratingTagIDByName(t, database, "general")
	expID := ratingTagIDByName(t, database, "explicit")

	mustAddTag(t, svc, imageID, genID, true, nil)
	gen, _ := svc.GetTag(genID)
	if gen.UsageCount != 1 {
		t.Fatalf("general usage_count = %d after seed, want 1", gen.UsageCount)
	}

	mustAddTag(t, svc, imageID, expID, true, nil)
	gen, _ = svc.GetTag(genID)
	if gen.UsageCount != 0 {
		t.Errorf("general usage_count = %d after prune, want 0", gen.UsageCount)
	}
	exp, _ := svc.GetTag(expID)
	if exp.UsageCount != 1 {
		t.Errorf("explicit usage_count = %d, want 1", exp.UsageCount)
	}
}

func TestAddRating_ManualOverwritesPriorLevel(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "ratings_manual.png")

	imageRatingNames := func() []string { return ratingNamesOnImage(t, svc, imageID) }

	expID := ratingTagIDByName(t, database, "explicit")
	conf := 0.9
	mustAddTag(t, svc, imageID, expID, true, &conf)

	genID := ratingTagIDByName(t, database, "general")
	mustAddTag(t, svc, imageID, genID, false, nil)
	got := imageRatingNames()
	if len(got) != 1 || got[0] != "general" {
		t.Fatalf("manual general after auto explicit: image carries %v, want only [general]", got)
	}
	if exp, _ := svc.GetTag(expID); exp.UsageCount != 0 {
		t.Errorf("explicit usage_count = %d after manual overwrite, want 0", exp.UsageCount)
	}
	if gen, _ := svc.GetTag(genID); gen.UsageCount != 1 {
		t.Errorf("general usage_count = %d after manual overwrite, want 1", gen.UsageCount)
	}

	senID := ratingTagIDByName(t, database, "sensitive")
	mustAddTag(t, svc, imageID, senID, false, nil)
	got = imageRatingNames()
	if len(got) != 1 || got[0] != "sensitive" {
		t.Fatalf("manual sensitive after manual general: image carries %v, want only [sensitive]", got)
	}
}

func TestAddImplication_RejectsCycle(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	a, _ := svc.GetOrCreateTag("cyc_a", catID)
	b, _ := svc.GetOrCreateTag("cyc_b", catID)
	c, _ := svc.GetOrCreateTag("cyc_c", catID)

	mustAddImply(t, svc, a.ID, b.ID)
	mustAddImply(t, svc, b.ID, c.ID)
	if _, err := svc.AddImplication(c.ID, a.ID); err != ErrImplicationCycle {
		t.Errorf("c→a after a→b→c expected ErrImplicationCycle, got %v", err)
	}
}

func TestAddImplication_RejectsSelf(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("self_loop", catID)

	if _, err := svc.AddImplication(a.ID, a.ID); err == nil {
		t.Errorf("AddImplication(a, a) should fail; got nil")
	}
}

func TestAddImplication_HonoursDepthBound(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	const chainLen = 6
	tags := make([]int64, chainLen)
	for i := 0; i < chainLen; i++ {
		tag, err := svc.GetOrCreateTag(fmt.Sprintf("depth_%d", i), catID)
		if err != nil {
			t.Fatalf("create tag %d: %v", i, err)
		}
		tags[i] = tag.ID
	}
	for i := 0; i < chainLen-1; i++ {
		mustAddImply(t, svc, tags[i], tags[i+1])
	}
	if _, err := svc.AddImplication(tags[chainLen-1], tags[0]); err != ErrImplicationCycle {
		t.Errorf("close chain expected ErrImplicationCycle, got %v", err)
	}
}

func TestAddImplication_RatingImpliedPrunesLower(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imageID := insertTestImage(t, database, "rating_imply.png")

	parent, _ := svc.GetOrCreateTag("hardcore_parent", catID)
	expRatingID := ratingTagIDByName(t, database, "explicit")

	mustAddTag(t, svc, imageID, ratingTagIDByName(t, database, "general"), false, nil)

	mustAddImply(t, svc, parent.ID, expRatingID)
	mustAddTag(t, svc, imageID, parent.ID, false, nil)

	rows, err := database.Read.Query(
		`SELECT t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ?`,
		imageID, svc.RatingCategoryID(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ratings []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		ratings = append(ratings, n)
	}
	if len(ratings) != 1 || ratings[0] != "explicit" {
		t.Errorf("post-fanout image carries ratings %v, want only [explicit]", ratings)
	}
}

func TestAddRating_ManualReportsDisplacedNames(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "displaced.png")

	expID := ratingTagIDByName(t, database, "explicit")
	conf := 0.9
	mustAddTag(t, svc, imageID, expID, true, &conf)

	genID := ratingTagIDByName(t, database, "general")
	res, err := svc.AddTagToImageReportingDup(imageID, genID, false, nil, "")
	if err != nil {
		t.Fatalf("AddTagToImageReportingDup: %v", err)
	}
	if !res.Added {
		t.Errorf("expected Added=true on a fresh manual rating, got %+v", res)
	}
	if got, want := res.DisplacedRatings, []string{"explicit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DisplacedRatings = %v, want %v", got, want)
	}

	res2, err := svc.AddTagToImageReportingDup(imageID, genID, false, nil, "")
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if len(res2.DisplacedRatings) != 0 {
		t.Errorf("DisplacedRatings on no-op re-add = %v, want empty", res2.DisplacedRatings)
	}
}

func TestAddNonRating_DoesNotTriggerPrune(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "non_rating.png")

	for _, name := range []string{"sensitive", "questionable"} {
		id := ratingTagIDByName(t, database, name)
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`, imageID, id)
		mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, id)
	}

	catID := generalCategoryID(t, svc)
	tag := mustTag(t, svc, "scenery", catID)
	mustAddTag(t, svc, imageID, tag.ID, false, nil)
	ratingCount := countRows(t, database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.category_id = ?", imageID, svc.RatingCategoryID())
	if ratingCount != 2 {
		t.Errorf("rating rows after non-rating add = %d, want 2 (legacy multi-rating left untouched)", ratingCount)
	}
}

func TestGetOrCreateTagFrom_StampsOriginOnInsertOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	created := mustGetOrCreateTagFrom(t, svc, "provenance_tag", catID, "danbooru")
	if created.Origin != "danbooru" {
		t.Errorf("fresh tag origin = %q, want danbooru", created.Origin)
	}

	again := mustGetOrCreateTagFrom(t, svc, "provenance_tag", catID, "ptr")
	full := mustGetTag(t, svc, again.ID)
	if full.Origin != "danbooru" {
		t.Errorf("origin after second get = %q, want danbooru", full.Origin)
	}

	plain := mustTag(t, svc, "ui_tag", catID)
	full = mustGetTag(t, svc, plain.ID)
	if full.Origin != "user" {
		t.Errorf("GetOrCreateTag origin = %q, want user", full.Origin)
	}
}

func TestCreateAliasFrom_StampsFreshInsertKeepsUpgrade(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	canon := mustTag(t, svc, "alias_canon", catID)

	fresh, err := svc.CreateAliasFrom("swept_alias", catID, canon.ID, "ptr")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Origin != "ptr" {
		t.Errorf("fresh alias origin = %q, want ptr", fresh.Origin)
	}

	mustTag(t, svc, "upgraded_alias", catID)
	upgraded, err := svc.CreateAliasFrom("upgraded_alias", catID, canon.ID, "ptr")
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Origin != "user" {
		t.Errorf("upgraded alias origin = %q, want user (creator preserved)", upgraded.Origin)
	}
}

func TestAddImplicationFrom_StampsEdgeOrigin(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	parent := mustTag(t, svc, "imp_parent", catID)
	implied := mustTag(t, svc, "imp_child", catID)
	if _, err := svc.AddImplicationFrom(parent.ID, implied.ID, "ptr"); err != nil {
		t.Fatal(err)
	}
	imps, err := svc.ListImplications(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imps) != 1 || imps[0].Origin != "ptr" {
		t.Errorf("implication origin = %+v, want one edge with origin ptr", imps)
	}
}

func TestRenameTagKeepAlias(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "renamekeep1")
	tag := mustGetOrCreateTagFrom(t, svc, "old_spelling", catID, "danbooru")
	mustAddTag(t, svc, imgID, tag.ID, false, nil)

	if err := svc.RenameTagKeepAlias(tag.ID, "new_spelling"); err != nil {
		t.Fatal(err)
	}
	renamed := mustGetTag(t, svc, tag.ID)
	if renamed.Name != "new_spelling" || renamed.Origin != "danbooru" {
		t.Errorf("renamed tag = %+v, want new_spelling with its origin kept", renamed)
	}
	resolved := mustTag(t, svc, "old_spelling", catID)
	if resolved.ID != tag.ID {
		t.Errorf("old spelling resolves to %d, want the renamed row %d", resolved.ID, tag.ID)
	}
	var aliasOrigin string
	var isAlias int
	if err := database.Read.QueryRow(
		`SELECT is_alias, origin FROM tags WHERE name = 'old_spelling'`,
	).Scan(&isAlias, &aliasOrigin); err != nil {
		t.Fatal(err)
	}
	if isAlias != 1 || aliasOrigin != "user" {
		t.Errorf("leftover row is_alias=%d origin=%q, want a user-origin alias", isAlias, aliasOrigin)
	}

	var aliasID int64
	if err := database.Read.QueryRow(`SELECT id FROM tags WHERE name = 'old_spelling'`).Scan(&aliasID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenameTagKeepAlias(aliasID, "third_spelling"); err == nil {
		t.Error("RenameTagKeepAlias on an alias row should refuse")
	}
}

func TestAddTagToImage_SetsLastUsedAt(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "lastused1")
	catID := generalCategoryID(t, svc)
	tag := mustTag(t, svc, "recently_used", catID)
	full := mustGetTag(t, svc, tag.ID)
	if !full.LastUsedAt.IsZero() {
		t.Errorf("never-applied tag LastUsedAt = %v, want zero", full.LastUsedAt)
	}
	mustAddTag(t, svc, imageID, tag.ID, false, nil)
	full = mustGetTag(t, svc, tag.ID)
	if full.LastUsedAt.IsZero() {
		t.Error("LastUsedAt still zero after applying the tag to an image")
	}
}

func TestRemoveTagFromImage_RefusesAnImpliedRow(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "implied_row_stands")

	parent, _ := svc.GetOrCreateTag("seated", catID)
	implied, _ := svc.GetOrCreateTag("sitting_on_object", catID)
	mustAddImply(t, svc, parent.ID, implied.ID)
	mustAddTag(t, svc, imgID, parent.ID, false, nil)

	if _, err := svc.RemoveTagFromImage(imgID, implied.ID); !errors.Is(err, ErrTagImplied) {
		t.Fatalf("RemoveTagFromImage on an implied row = %v, want ErrTagImplied", err)
	}
	if err := svc.RemoveTagsFromOneImage(imgID, []int64{implied.ID}); !errors.Is(err, ErrTagImplied) {
		t.Errorf("RemoveTagsFromOneImage on an implied row = %v, want ErrTagImplied", err)
	}
	n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", imgID, implied.ID)
	if n != 1 {
		t.Errorf("implied row survived = %d, want 1", n)
	}

	mustAddTag(t, svc, imgID, implied.ID, false, nil)
	if _, err := svc.RemoveTagFromImage(imgID, implied.ID); err != nil {
		t.Errorf("removing a user-owned row = %v, want nil", err)
	}

	mustAddTag(t, svc, imgID, parent.ID, false, nil)
	if _, err := svc.RemoveTagFromImage(imgID, parent.ID); err != nil {
		t.Fatalf("removing the parent = %v, want nil", err)
	}
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, imgID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rows left after removing the parent = %d, want 0", n)
	}
}

func TestRemoveTagFromImage_AllowsAnOrphanedImpliedRow(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "orphaned_implied_row")

	orphan, _ := svc.GetOrCreateTag("nobody_implies_me", catID)
	mustAddTag(t, svc, imgID, orphan.ID, false, nil)
	mustExec(t, database.Write, `UPDATE image_tags SET is_implied = 1 WHERE image_id = ? AND tag_id = ?`, imgID, orphan.ID)

	if _, err := svc.RemoveTagFromImage(imgID, orphan.ID); err != nil {
		t.Errorf("removing an orphaned implied row = %v, want nil", err)
	}
}

func TestRemoveTagFromImage_AllowsAChildOfAnOrphan(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "child_of_orphan")

	root := mustTag(t, svc, "orphan_chain_root", catID)
	orphan := mustTag(t, svc, "orphan_chain_orphan", catID)
	child := mustTag(t, svc, "orphan_chain_child", catID)
	mustAddImply(t, svc, root.ID, orphan.ID)
	mustAddImply(t, svc, orphan.ID, child.ID)
	mustAddTag(t, svc, imgID, root.ID, false, nil)

	mustExec(t, database.Write, `DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, root.ID)

	if _, err := svc.RemoveTagFromImage(imgID, child.ID); err != nil {
		t.Errorf("removing the child of an orphan = %v, want nil", err)
	}
	if err := svc.RemoveTagsFromOneImage(imgID, []int64{orphan.ID}); err != nil {
		t.Errorf("removing the orphan = %v, want nil", err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ?", imgID); n != 0 {
		t.Errorf("rows left = %d, want 0", n)
	}
}

func TestRemoveTagFromImage_CycleWithNoOwnedRootIsRemovable(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	single := insertTestImage(t, database, "implied_cycle_single")
	batch := insertTestImage(t, database, "implied_cycle_batch")

	a := mustTag(t, svc, "implied_cycle_a", catID)
	b := mustTag(t, svc, "implied_cycle_b", catID)

	mustExec(t, database.Write, `INSERT INTO tag_implications (parent_tag_id, implied_tag_id) VALUES (?1, ?2), (?2, ?1)`, a.ID, b.ID)
	for _, id := range []int64{single, batch} {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?1, ?2, 1), (?1, ?3, 1)`, id, a.ID, b.ID)
	}

	if _, err := svc.RemoveTagFromImage(single, a.ID); err != nil {
		t.Errorf("RemoveTagFromImage = %v, want nil", err)
	}
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	removed, _, implied, err := batchRemoveTagsTx(tx, []int64{batch}, []int64{a.ID})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if removed != 2 || implied != 0 {
		t.Errorf("BatchRemoveTagsTx = %d removed, %d implied; want 2, 0", removed, implied)
	}
	if n := countRows(t, database, "image_tags WHERE image_id IN (?, ?)", single, batch); n != 0 {
		t.Errorf("rows left = %d, want 0", n)
	}
}

func TestRemoveTagsFromOneImage_TakesAParentWithItsImpliedChild(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	single := insertTestImage(t, database, "parent_child_single")
	batch := insertTestImage(t, database, "parent_child_batch")

	parent := mustTag(t, svc, "parent_child_parent", catID)
	child := mustTag(t, svc, "parent_child_child", catID)
	mustAddImply(t, svc, parent.ID, child.ID)
	mustAddTag(t, svc, single, parent.ID, false, nil)
	mustAddTag(t, svc, batch, parent.ID, false, nil)

	if err := svc.RemoveTagsFromOneImage(single, []int64{child.ID, parent.ID}); err != nil {
		t.Errorf("RemoveTagsFromOneImage = %v, want nil", err)
	}
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	removed, _, kept, err := batchRemoveTagsTx(tx, []int64{batch}, []int64{child.ID, parent.ID})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if removed != 2 || kept != 0 {
		t.Errorf("BatchRemoveTagsTx = %d removed, %d kept; want 2, 0", removed, kept)
	}
	if n := countRows(t, database, "image_tags WHERE image_id IN (?, ?)", single, batch); n != 0 {
		t.Errorf("rows left = %d, want 0", n)
	}

	other := mustTag(t, svc, "parent_child_other", catID)
	mustAddImply(t, svc, other.ID, child.ID)
	mustAddTag(t, svc, single, parent.ID, false, nil)
	mustAddTag(t, svc, single, other.ID, false, nil)
	if err := svc.RemoveTagsFromOneImage(single, []int64{child.ID, parent.ID}); !errors.Is(err, ErrTagImplied) {
		t.Errorf("RemoveTagsFromOneImage with the child still held = %v, want ErrTagImplied", err)
	}
	if n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", single, parent.ID); n != 1 {
		t.Errorf("the refused call removed the parent (%d rows left)", n)
	}
}

func mkTagID(t *testing.T, svc *Service, name string, categoryID int64) int64 {
	t.Helper()
	tg := mustTag(t, svc, name, categoryID)
	return tg.ID
}

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustAddTag(t *testing.T, svc *Service, imageID, tagID int64, isAuto bool, confidence *float64) {
	t.Helper()
	if err := svc.AddTagToImage(imageID, tagID, isAuto, confidence); err != nil {
		t.Fatalf("AddTagToImage(%d, %d): %v", imageID, tagID, err)
	}
}

func mustAddImply(t *testing.T, svc *Service, parentID, impliedID int64) bool {
	t.Helper()
	created, err := svc.AddImplication(parentID, impliedID)
	if err != nil {
		t.Fatalf("AddImplication(%d, %d): %v", parentID, impliedID, err)
	}
	return created
}

func mustMerge(t *testing.T, svc *Service, aliasID, canonicalID int64) {
	t.Helper()
	if err := svc.MergeTags(aliasID, canonicalID); err != nil {
		t.Fatalf("MergeTags(%d -> %d): %v", aliasID, canonicalID, err)
	}
}

func mustSyncSourceTags(t *testing.T, svc *Service, imageID int64, tagIDs []int64, site string, reconcile bool) SyncResult {
	t.Helper()
	res, err := svc.SyncSourceTags(imageID, tagIDs, site, reconcile)
	if err != nil {
		t.Fatalf("SyncSourceTags(%d, %q): %v", imageID, site, err)
	}
	return res
}

func mustListTags(t *testing.T, svc *Service, filter TagFilter) ([]models.Tag, int) {
	t.Helper()
	list, total, err := svc.ListTags(filter)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	return list, total
}

func mustGetTag(t *testing.T, svc *Service, id int64) *models.Tag {
	t.Helper()
	tag, err := svc.GetTag(id)
	if err != nil {
		t.Fatalf("GetTag(%d): %v", id, err)
	}
	return tag
}

func mustCreateAlias(t *testing.T, svc *Service, name string, categoryID, canonicalID int64) *models.Tag {
	t.Helper()
	tag, err := svc.CreateAlias(name, categoryID, canonicalID)
	if err != nil {
		t.Fatalf("CreateAlias(%q): %v", name, err)
	}
	return tag
}

func mustCreateCategory(t *testing.T, svc *Service, name, color string) *models.TagCategory {
	t.Helper()
	cat, err := svc.CreateCategory(name, color)
	if err != nil {
		t.Fatalf("CreateCategory(%q): %v", name, err)
	}
	return cat
}

func mustImageTags(t *testing.T, svc *Service, imageID int64) (string, []models.ImageTag) {
	t.Helper()
	raw, list, err := svc.GetImageTags(imageID)
	if err != nil {
		t.Fatalf("GetImageTags(%d): %v", imageID, err)
	}
	return raw, list
}

func mustListCategories(t *testing.T, svc *Service) []models.TagCategory {
	t.Helper()
	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	return cats
}

func mustGetOrCreateTagFrom(t *testing.T, svc *Service, name string, categoryID int64, origin string) *models.Tag {
	t.Helper()
	tag, err := svc.GetOrCreateTagFrom(name, categoryID, origin)
	if err != nil {
		t.Fatalf("GetOrCreateTagFrom(%q, %q): %v", name, origin, err)
	}
	return tag
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func TestUsageBreakdown_LeavesOutImagesMarkedMissing(t *testing.T) {
	database, svc := setupTestDB(t)
	tag := mustTag(t, svc, "counted", generalCategoryID(t, svc))
	live := insertTestImage(t, database, "live")
	gone := insertTestImage(t, database, "gone")
	for _, id := range []int64{live, gone} {
		if err := svc.AddTagsToImageFromTagger(id, []int64{tag.ID}, false, "danbooru"); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)

	applied, months, err := svc.UsageBreakdown(tag.ID)

	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Count != 1 {
		t.Errorf("applied by = %+v, want the one visible image", applied)
	}
	if len(months) != 1 || months[0].Count != 1 {
		t.Errorf("usage over time = %+v, want the one visible image", months)
	}
}

func TestUsageBreakdown_CountsImpliedRowsOnTheirOwnLine(t *testing.T) {
	database, svc := setupTestDB(t)
	gen := generalCategoryID(t, svc)
	parent := mustTag(t, svc, "parent_tag", gen)
	child := mustTag(t, svc, "child_tag", gen)
	mustAddImply(t, svc, parent.ID, child.ID)
	fanned := insertTestImage(t, database, "fanned")
	if err := svc.AddTagsToImageFromTagger(fanned, []int64{parent.ID}, false, "danbooru"); err != nil {
		t.Fatal(err)
	}
	mustAddTag(t, svc, insertTestImage(t, database, "typed"), child.ID, false, nil)

	applied, _, err := svc.UsageBreakdown(child.ID)

	if err != nil {
		t.Fatal(err)
	}
	want := []AppliedByCount{{Implied: true, Count: 1}, {Count: 1}}
	slices.SortFunc(applied, func(a, b AppliedByCount) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	slices.SortFunc(want, func(a, b AppliedByCount) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	if !slices.Equal(applied, want) {
		t.Errorf("applied by = %+v, want the fan-out apart from the manual add: %+v", applied, want)
	}
}

func TestAutoTaggerLabels_KeepsOnlyLabelsWithAutoRows(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "auto_labels")
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, 'wd14')`, imgID, mkTagID(t, svc, "al_a", general))
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, 'joytag')`, imgID, mkTagID(t, svc, "al_b", general))
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 0, 'danbooru')`, imgID, mkTagID(t, svc, "al_c", general))

	for _, tc := range []struct {
		name   string
		labels []string
		want   []string
	}{
		{"duplicates", []string{"wd14", "joytag", "wd14", "joytag"}, []string{"joytag", "wd14"}},
		{"empty label", []string{"", "wd14", ""}, []string{"wd14"}},
		{"only empty", []string{""}, nil},
		{"no auto rows", []string{"danbooru", "nobody"}, nil},
		{"none", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := svc.AutoTaggerLabels(tc.labels)
			if err != nil {
				t.Fatalf("AutoTaggerLabels: %v", err)
			}
			if set == nil {
				t.Fatal("AutoTaggerLabels returned a nil set")
			}
			var got []string
			for l := range set {
				got = append(got, l)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("AutoTaggerLabels(%q) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func ownedChildUnderParent(t *testing.T, database *db.DB, svc *Service, sha string, parentAuto bool) (imageID, parent, child int64) {
	t.Helper()
	general := generalCategoryID(t, svc)
	parent, child = mkTagID(t, svc, "red_hair", general), mkTagID(t, svc, "cat", general)
	mustAddImply(t, svc, parent, child)
	imageID = insertTestImage(t, database, sha)
	mustAddTag(t, svc, imageID, child, false, nil)
	if parentAuto {
		conf := 0.9
		if _, err := svc.AddTagToImageReportingDup(imageID, parent, true, &conf, "tagger-A"); err != nil {
			t.Fatal(err)
		}
	} else {
		mustAddTag(t, svc, imageID, parent, false, nil)
	}
	return imageID, parent, child
}

func assertDemoted(t *testing.T, database *db.DB, svc *Service, imageID, child int64) {
	t.Helper()
	var isImplied int
	var tagger sql.NullString
	if err := database.Read.QueryRow(`SELECT is_implied, tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`,
		imageID, child).Scan(&isImplied, &tagger); err != nil {
		t.Fatalf("the owned row a parent implies is gone: %v", err)
	}
	if isImplied != 1 || tagger.Valid {
		t.Errorf("child is_implied=%d tagger=%v, want an implied row", isImplied, tagger)
	}
	if got := ledgerSources(t, database, imageID, child); len(got) != 0 {
		t.Errorf("demoted child keeps ledger entries %v", got)
	}
	if got := mustGetTag(t, svc, child).UsageCount; got != 1 {
		t.Errorf("child usage = %d, want 1", got)
	}
}

func TestRemoveTagFromImage_DemotesAnOwnedRowAParentImplies(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID, _, child := ownedChildUnderParent(t, database, svc, "demote_single", false)
	removed, err := svc.RemoveTagFromImage(imageID, child)
	if err != nil || removed != 0 {
		t.Fatalf("RemoveTagFromImage = %d, %v; want 0 removed", removed, err)
	}
	assertDemoted(t, database, svc, imageID, child)
}

func TestRemoveUserTagsFromImage_DemotesAnOwnedRowAParentImplies(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID, _, child := ownedChildUnderParent(t, database, svc, "demote_user", true)
	if _, err := svc.RemoveUserTagsFromImage(imageID); err != nil {
		t.Fatal(err)
	}
	assertDemoted(t, database, svc, imageID, child)
}

func scannedFoldedPair(t *testing.T, svc *Service) (old, wide int64) {
	t.Helper()
	general := generalCategoryID(t, svc)
	old, wide = mkTagID(t, svc, "hello_world", general), mkTagID(t, svc, "hello\u2605world", general)
	if n, err := svc.ScanFoldedDuplicates(); err != nil || n != 1 {
		t.Fatalf("ScanFoldedDuplicates = %d, %v; want one pair", n, err)
	}
	return old, wide
}

func TestMergeFolded_SkipsAnOldTagAliasedElsewhere(t *testing.T) {
	_, svc := setupTestDB(t)
	old, _ := scannedFoldedPair(t, svc)
	third := mkTagID(t, svc, "hello_planet", generalCategoryID(t, svc))
	mustMerge(t, svc, old, third)

	res, err := svc.MergeFolded(context.Background(), []int64{old})
	if err != nil {
		t.Fatalf("MergeFolded: %v", err)
	}
	if res.Merged != 0 || res.Skipped != 1 {
		t.Errorf("result = %+v, want 0 merged / 1 skipped", res)
	}
	if got := mustGetTag(t, svc, old); !got.IsAlias || got.CanonicalTagID == nil || *got.CanonicalTagID != third {
		t.Errorf("old tag alias=%v canonical=%v, want an alias of %d", got.IsAlias, got.CanonicalTagID, third)
	}
}

func TestFoldedDuplicates_CountAndListOnlyLivePairs(t *testing.T) {
	cases := map[string]func(t *testing.T, svc *Service, old, wide int64){
		"old aliased elsewhere": func(t *testing.T, svc *Service, old, _ int64) {
			mustMerge(t, svc, old, mkTagID(t, svc, "hello_planet", generalCategoryID(t, svc)))
		},
		"wide aliased elsewhere": func(t *testing.T, svc *Service, _, wide int64) {
			mustMerge(t, svc, wide, mkTagID(t, svc, "hello_planet", generalCategoryID(t, svc)))
		},
		"wide moved to another category": func(t *testing.T, svc *Service, _, wide int64) {
			if err := svc.ChangeTagCategory(wide, mustCreateCategory(t, svc, "elsewhere", "#123456").ID); err != nil {
				t.Fatal(err)
			}
		},
		"old renamed": func(t *testing.T, svc *Service, old, _ int64) {
			if err := svc.RenameTag(old, "goodbye_world"); err != nil {
				t.Fatal(err)
			}
		},
		"wide renamed": func(t *testing.T, svc *Service, _, wide int64) {
			if err := svc.RenameTag(wide, "goodbye_world"); err != nil {
				t.Fatal(err)
			}
		},
		"old rejected": func(t *testing.T, svc *Service, old, _ int64) {
			if err := svc.RejectTag(old); err != nil {
				t.Fatal(err)
			}
		},
		"wide rejected": func(t *testing.T, svc *Service, _, wide int64) {
			if err := svc.RejectTag(wide); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, undo := range cases {
		t.Run(name, func(t *testing.T) {
			_, svc := setupTestDB(t)
			old, wide := scannedFoldedPair(t, svc)
			undo(t, svc, old, wide)
			if n, err := svc.FoldedDuplicatesCount(); err != nil || n != 0 {
				t.Errorf("FoldedDuplicatesCount = %d, %v; want 0", n, err)
			}
			for _, typ := range []string{"", "tag"} {
				if list, total := mustListTags(t, svc, TagFilter{FoldedOnly: true, ShowZero: true, Type: typ}); total != 0 || len(list) != 0 {
					t.Errorf("folded listing (type %q) = %d %+v, want none", typ, total, list)
				}
			}
		})
	}
}

func TestFoldedDuplicates_PlanDrivesFromThePairsUnderStaleStatistics(t *testing.T) {
	database, svc := setupTestDB(t)
	gen := generalCategoryID(t, svc)

	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if _, err := tx.Exec(`INSERT INTO tags(name, category_id) VALUES (?, ?)`, fmt.Sprintf("t%04d", i), gen); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Read.Query(`EXPLAIN QUERY PLAN SELECT COUNT(DISTINCT fp.old_id) FROM ` + liveFoldedPairs)
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
	plan := strings.Join(steps, "; ")
	if !strings.Contains(plan, "SCAN fp") || strings.Contains(plan, "idx_tags_category") {
		t.Errorf("plan = %q, want the pairs driving the join", plan)
	}
}

func TestMergeTags_DropsTheAliasFoldedPairs(t *testing.T) {
	for _, side := range []string{"old", "wide"} {
		t.Run(side, func(t *testing.T) {
			database, svc := setupTestDB(t)
			old, wide := scannedFoldedPair(t, svc)
			alias := map[string]int64{"old": old, "wide": wide}[side]
			mustMerge(t, svc, alias, mkTagID(t, svc, "hello_planet", generalCategoryID(t, svc)))
			if n := countRows(t, database, `folded_tag_pairs`); n != 0 {
				t.Errorf("folded_tag_pairs keeps %d row(s) of the merged tag, want 0", n)
			}
		})
	}
}

func TestMergeFolded_SkipsAPairARenameUndid(t *testing.T) {
	_, svc := setupTestDB(t)
	old, wide := scannedFoldedPair(t, svc)
	if err := svc.RenameTag(wide, "goodbye_world"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.MergeFolded(context.Background(), []int64{old})
	if err != nil {
		t.Fatalf("MergeFolded: %v", err)
	}
	if res.Merged != 0 || res.Skipped != 1 {
		t.Errorf("result = %+v, want 0 merged / 1 skipped", res)
	}
	if mustGetTag(t, svc, old).IsAlias {
		t.Error("the old tag was merged into a tag that no longer folds onto it")
	}
}

func TestCategoryReads_AFailedReadIsNotANotFound(t *testing.T) {
	database, svc := setupTestDB(t)
	cat := mustCreateCategory(t, svc, "custom", "#123456")
	tag := mkTagID(t, svc, "moved", generalCategoryID(t, svc))
	mustExec(t, database.Write, `ALTER TABLE tag_categories RENAME TO tag_categories_gone`)
	if err := svc.ChangeTagCategory(tag, cat.ID); err == nil || errors.Is(err, ErrCategoryNotFound) {
		t.Errorf("ChangeTagCategory with the category read failing = %v, want the read error", err)
	}
	mustExec(t, database.Write, `ALTER TABLE tag_categories_gone RENAME TO tag_categories`)

	if err := database.Read.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenameCategory(cat.ID, "renamed"); err == nil || errors.Is(err, ErrCategoryNotFound) {
		t.Errorf("RenameCategory with the read failing = %v, want the read error", err)
	}
}

func TestDeleteTag_RefusesOnAFailedRatingRead(t *testing.T) {
	database, svc := setupTestDB(t)
	explicit := ratingTagIDByName(t, database, "explicit")
	if err := database.Read.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteTag(explicit); err == nil {
		t.Error("DeleteTag succeeded with the rating read failing")
	}
	var n int
	if err := database.Write.QueryRow(`SELECT COUNT(*) FROM tags WHERE id = ?`, explicit).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the canonical rating row was deleted")
	}
}
