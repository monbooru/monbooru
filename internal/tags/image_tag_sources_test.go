package tags

import (
	"reflect"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

func ledgerSources(t *testing.T, database *db.DB, imageID, tagID int64) []string {
	t.Helper()
	out, err := db.QueryStrings(database.Read,
		`SELECT source FROM image_tag_sources WHERE image_id = ? AND tag_id = ? ORDER BY created_at, source`,
		imageID, tagID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTagSources_AddRecordsTheVia(t *testing.T) {
	for _, tc := range []struct {
		name, sha, via, want string
	}{
		{"UIAddRecordsUser", "src-ui", "", "user"},
		{"ViaLabelRecorded", "src-via", "scraper", "scraper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, svc := setupTestDB(t)
			imgID := insertTestImage(t, database, tc.sha)
			tag := mustTag(t, svc, "cute", generalCategoryID(t, svc))
			if _, err := svc.AddTagsToOneImage(imgID, []int64{tag.ID}, tc.via); err != nil {
				t.Fatal(err)
			}
			if got := ledgerSources(t, database, imgID, tag.ID); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Errorf("sources = %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestTagSources_ReconfirmAddsSecondRow(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-reconfirm")
	tag := mustTag(t, svc, "cute", generalCategoryID(t, svc))
	mustAddTag(t, svc, imgID, tag.ID, false, nil)
	if _, err := svc.SyncSourceTags(imgID, []int64{tag.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}
	got := ledgerSources(t, database, imgID, tag.ID)
	want := map[string]bool{"user": true, "danbooru": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("sources = %v, want user + danbooru", got)
	}
	var tname *string
	if err := database.Read.QueryRow(
		`SELECT tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, tag.ID,
	).Scan(&tname); err != nil {
		t.Fatal(err)
	}
	if tname != nil {
		t.Errorf("tagger_name = %v, want NULL (first source wins)", *tname)
	}
}

func TestTagSources_UIReAddOfExistingRecordsNothing(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-readd")
	tag := mustTag(t, svc, "red_eyes", generalCategoryID(t, svc))
	if _, err := svc.SyncSourceTags(imgID, []int64{tag.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTagsToOneImage(imgID, []int64{tag.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := ledgerSources(t, database, imgID, tag.ID); !reflect.DeepEqual(got, []string{"danbooru"}) {
		t.Errorf("sources = %v, want [danbooru] (no phantom user)", got)
	}
}

func TestTagSources_UIReAddOfADerivedTagRecordsTheUser(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-rederived")
	tag := mustTag(t, svc, "highres", generalCategoryID(t, svc))
	if _, err := svc.SyncSourceTags(imgID, []int64{tag.ID}, models.TagSourceMonbooru, false); err != nil {
		t.Fatal(err)
	}
	res, err := svc.AddTagsToOneImage(imgID, []int64{tag.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].Promoted {
		t.Errorf("re-add = %+v, want it reported as promoted", res)
	}
	if got := ledgerSources(t, database, imgID, tag.ID); !reflect.DeepEqual(got, []string{models.TagSourceMonbooru, "user"}) {
		t.Errorf("sources = %v, want [monbooru user]", got)
	}
}

func TestTagSources_ImportLabelRecorded(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-import")
	tag := mustTag(t, svc, "cute", generalCategoryID(t, svc))
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{tag.ID}, false, "import"); err != nil {
		t.Fatal(err)
	}
	if got := ledgerSources(t, database, imgID, tag.ID); !reflect.DeepEqual(got, []string{"import"}) {
		t.Errorf("sources = %v, want [import]", got)
	}
}

func TestTagSources_ImpliedFanoutRecordsNothing(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "src-implied")
	parent := mustTag(t, svc, "miko", catID)
	implied := mustTag(t, svc, "shrine_maiden", catID)
	mustAddImply(t, svc, parent.ID, implied.ID)
	mustAddTag(t, svc, imgID, parent.ID, false, nil)
	if got := ledgerSources(t, database, imgID, implied.ID); got != nil {
		t.Errorf("implied tag sources = %v, want none", got)
	}
	if got := ledgerSources(t, database, imgID, parent.ID); !reflect.DeepEqual(got, []string{"user"}) {
		t.Errorf("parent sources = %v, want [user]", got)
	}
}

func TestTagSources_RemovalClearsLedger(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-remove")
	tag := mustTag(t, svc, "cute", generalCategoryID(t, svc))
	if _, err := svc.SyncSourceTags(imgID, []int64{tag.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RemoveTagFromImage(imgID, tag.ID); err != nil {
		t.Fatal(err)
	}
	if got := ledgerSources(t, database, imgID, tag.ID); got != nil {
		t.Errorf("sources after removal = %v, want none", got)
	}
	if _, err := svc.AddTagsToOneImage(imgID, []int64{tag.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := ledgerSources(t, database, imgID, tag.ID); !reflect.DeepEqual(got, []string{"user"}) {
		t.Errorf("sources after re-add = %v, want [user]", got)
	}
}

func TestTagSources_RemoveAllClearsLedger(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-remove-all")
	tag := mustTag(t, svc, "cute", generalCategoryID(t, svc))
	if _, err := svc.SyncSourceTags(imgID, []int64{tag.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveAllTagsFromImage(imgID); err != nil {
		t.Fatal(err)
	}
	n := countRows(t, database, "image_tag_sources WHERE image_id = ?", imgID)
	if n != 0 {
		t.Errorf("ledger rows after remove-all = %d, want 0", n)
	}
}

func TestTagSources_MergeMovesLedger(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "src-merge")
	alias := mustTag(t, svc, "blond_hair", catID)
	canonical := mustTag(t, svc, "blonde_hair", catID)
	if _, err := svc.SyncSourceTags(imgID, []int64{alias.ID}, "gelbooru", false); err != nil {
		t.Fatal(err)
	}
	mustMerge(t, svc, alias.ID, canonical.ID)
	if got := ledgerSources(t, database, imgID, canonical.ID); !reflect.DeepEqual(got, []string{"gelbooru"}) {
		t.Errorf("canonical sources = %v, want [gelbooru]", got)
	}
	if got := ledgerSources(t, database, imgID, alias.ID); got != nil {
		t.Errorf("alias sources = %v, want none", got)
	}
}

func TestUsedBy_LabelsColumnAndFilter(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgA := insertTestImage(t, database, "usedby-a")
	imgB := insertTestImage(t, database, "usedby-b")
	shared := mustGetOrCreateTagFrom(t, svc, "shared", catID, "danbooru")
	gelOnly := mustGetOrCreateTagFrom(t, svc, "gel_only", catID, "gelbooru")
	if _, err := svc.SyncSourceTags(imgA, []int64{shared.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncSourceTags(imgB, []int64{shared.ID, gelOnly.ID}, "gelbooru", false); err != nil {
		t.Fatal(err)
	}

	labels, err := svc.UsedByLabels()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(labels, []string{"danbooru", "gelbooru"}) {
		t.Errorf("UsedByLabels = %v, want [danbooru gelbooru]", labels)
	}

	sources, err := svc.UsedByForTags([]int64{shared.ID, gelOnly.ID}, labels)
	if err != nil {
		t.Fatal(err)
	}
	if got := sources[shared.ID]; !reflect.DeepEqual(got, []string{"danbooru", "gelbooru"}) {
		t.Errorf("used by shared = %v, want [danbooru gelbooru]", got)
	}
	if got := sources[gelOnly.ID]; !reflect.DeepEqual(got, []string{"gelbooru"}) {
		t.Errorf("used by gel_only = %v, want [gelbooru]", got)
	}

	names := func(f TagFilter) []string {
		f.Limit = 40
		list, _ := mustListTags(t, svc, f)
		var out []string
		for _, tag := range list {
			out = append(out, tag.Name)
		}
		return out
	}

	if got := names(TagFilter{UsedBy: "gelbooru", Sort: "name"}); !reflect.DeepEqual(got, []string{"gel_only", "shared"}) {
		t.Errorf("used_by=gelbooru = %v, want [gel_only shared]", got)
	}
	if got := names(TagFilter{Origin: "gelbooru", Sort: "name"}); !reflect.DeepEqual(got, []string{"gel_only"}) {
		t.Errorf("origin=gelbooru = %v, want [gel_only]", got)
	}
	if got := names(TagFilter{UsedBy: "nobody"}); got != nil {
		t.Errorf("used_by=nobody = %v, want none", got)
	}
}

func TestTagSourcesForImage(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "src-read")
	tag := mustTag(t, svc, "cute", generalCategoryID(t, svc))
	mustAddTag(t, svc, imgID, tag.ID, false, nil)
	if _, err := svc.SyncSourceTags(imgID, []int64{tag.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}
	got, err := svc.TagSourcesForImage(imgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[tag.ID]) != 2 {
		t.Errorf("TagSourcesForImage[%d] = %v, want 2 sources", tag.ID, got[tag.ID])
	}
}

func TestDropSourceFromImageTags_KeepsWhatOthersStillVouchFor(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "drop-src")
	cat := generalCategoryID(t, svc)

	shared := mustTag(t, svc, "shared", cat)
	theirs := mustTag(t, svc, "theirs", cat)

	if _, err := svc.AddTagsToOneImage(imgID, []int64{shared.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTagsToOneImage(imgID, []int64{shared.ID, theirs.ID}, "danbooru"); err != nil {
		t.Fatal(err)
	}

	covered, removed, err := svc.DropSourceFromImageTags(imgID, "danbooru", nil)
	if err != nil {
		t.Fatal(err)
	}
	if covered != 2 || removed != 1 {
		t.Errorf("covered/removed = %d/%d, want 2/1", covered, removed)
	}
	if got := ledgerSources(t, database, imgID, theirs.ID); len(got) != 0 {
		t.Errorf("theirs still has sources %v", got)
	}
	if onImage(t, database, imgID, theirs.ID) {
		t.Error("theirs should have left the image with its last source")
	}
	if got := ledgerSources(t, database, imgID, shared.ID); !reflect.DeepEqual(got, []string{"user"}) {
		t.Errorf("shared sources = %v, want [user]", got)
	}
	if !onImage(t, database, imgID, shared.ID) {
		t.Error("shared lost its row even though the user still vouches for it")
	}

	covered, removed, err = svc.DropSourceFromImageTags(imgID, "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if covered != 1 || removed != 1 {
		t.Errorf("second withdrawal = %d/%d, want 1/1", covered, removed)
	}
	if onImage(t, database, imgID, shared.ID) {
		t.Error("shared should be gone once no source vouches for it")
	}
}

func TestDropSourceFromImageTags_NarrowsToOneTag(t *testing.T) {
	database, svc := setupTestDB(t)
	imgID := insertTestImage(t, database, "drop-one")
	cat := generalCategoryID(t, svc)

	a := mustTag(t, svc, "a_tag", cat)
	b := mustTag(t, svc, "b_tag", cat)
	if _, err := svc.AddTagsToOneImage(imgID, []int64{a.ID, b.ID}, "danbooru"); err != nil {
		t.Fatal(err)
	}

	covered, removed, err := svc.DropSourceFromImageTags(imgID, "danbooru", []int64{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if covered != 1 || removed != 1 {
		t.Errorf("covered/removed = %d/%d, want 1/1", covered, removed)
	}
	if onImage(t, database, imgID, a.ID) {
		t.Error("a_tag should be gone")
	}
	if !onImage(t, database, imgID, b.ID) {
		t.Error("b_tag was not named and should be untouched")
	}
}

func onImage(t *testing.T, database *db.DB, imageID, tagID int64) bool {
	t.Helper()
	n := countRows(t, database, "image_tags WHERE image_id = ? AND tag_id = ?", imageID, tagID)
	return n > 0
}
