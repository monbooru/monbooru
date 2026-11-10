package tags

import (
	"context"
	"testing"
)

type changeStep struct {
	name  string
	run   func() error
	fails bool
}

func TestService_SignalsAfterEveryCommit(t *testing.T) {
	database, plain := setupTestDB(t)
	calls := 0
	svc := New(database, func() { calls++ })
	general := generalCategoryID(t, plain)
	img := insertTestImage(t, database, "chg-a")
	img2 := insertTestImage(t, database, "chg-b")
	a := mustTag(t, plain, "chg_a", general)
	b := mustTag(t, plain, "chg_b", general)
	c := mustTag(t, plain, "chg_c", general)
	d := mustTag(t, plain, "chg_d", general)
	e := mustTag(t, plain, "chg_e", general)
	f := mustTag(t, plain, "chg_f", general)
	g := mustTag(t, plain, "chg_g", general)
	h := mustTag(t, plain, "chg_h", general)
	legacy := mustTag(t, plain, "chg_caf", general)
	mustTag(t, plain, "chg_café", general)
	conf := 0.9
	var cat, aliasTag int64

	steps := []changeStep{
		{name: "GetOrCreateTag", run: func() error { _, err := svc.GetOrCreateTag("chg_new", general); return err }},
		{name: "GetOrCreateTagFrom", run: func() error { _, err := svc.GetOrCreateTagFrom("chg_from", general, "user"); return err }},
		{name: "GetOrCreateTagsFrom", run: func() error {
			_, err := svc.GetOrCreateTagsFrom([]string{"chg_x", "chg_y"}, general, "user")
			return err
		}},
		{name: "AddTagToImage", run: func() error { return svc.AddTagToImage(img, a.ID, false, nil) }},
		{name: "AddTagToImageReportingDup", run: func() error {
			_, err := svc.AddTagToImageReportingDup(img, b.ID, false, nil, "")
			return err
		}},
		{name: "AddTagsToImageFromTagger", run: func() error { return svc.AddTagsToImageFromTagger(img, []int64{c.ID}, true, "wd") }},
		{name: "AddTagsToImageFromTaggerConf", run: func() error {
			_, err := svc.AddTagsToImageFromTaggerConf(img2, []int64{c.ID}, []*float64{&conf}, true, "wd")
			return err
		}},
		{name: "ApplyTaggerResults", run: func() error {
			return svc.ApplyTaggerResults(context.Background(), img2, []TaggerHit{{Name: "chg_tagged", CategoryID: general, Score: 0.9, Tagger: "wd"}}, []string{"wd"})
		}},
		{name: "AddTagsToOneImage", run: func() error { _, err := svc.AddTagsToOneImage(img2, []int64{a.ID}, ""); return err }},
		{name: "BatchAddTags", run: func() error {
			_, err := svc.BatchAddTags(context.Background(), []int64{img2}, []int64{e.ID}, nil)
			return err
		}},
		{name: "BatchRemoveTags", run: func() error {
			_, err := svc.BatchRemoveTags(context.Background(), []int64{img2}, []int64{e.ID}, nil)
			return err
		}},
		{name: "SyncSourceTags", run: func() error { _, err := svc.SyncSourceTags(img2, []int64{b.ID}, "danbooru", true); return err }},
		{name: "AddImplication", run: func() error { _, err := svc.AddImplication(a.ID, d.ID); return err }},
		{name: "AddImplication closing a cycle", fails: true, run: func() error { _, err := svc.AddImplication(d.ID, a.ID); return err }},
		{name: "AddImplicationFrom", run: func() error { _, err := svc.AddImplicationFrom(b.ID, d.ID, "ptr"); return err }},
		{name: "SyncImplicationStaleness", run: func() error {
			_, err := svc.SyncImplicationStaleness(b.ID, "ptr", map[int64]bool{})
			return err
		}},
		{name: "PropagateImplication", run: func() error {
			_, err := svc.PropagateImplication(context.Background(), a.ID, nil)
			return err
		}},
		{name: "SweepImplicationRemoval", run: func() error {
			_, err := svc.SweepImplicationRemoval(context.Background(), a.ID, []int64{d.ID}, nil)
			return err
		}},
		{name: "RemoveImplication", run: func() error { return svc.RemoveImplication(b.ID, d.ID) }},
		{name: "CreateAlias", run: func() error {
			tag, err := svc.CreateAlias("chg_alias", general, a.ID)
			if err == nil {
				aliasTag = tag.ID
			}
			return err
		}},
		{name: "MergeTags onto an alias", fails: true, run: func() error { return svc.MergeTags(g.ID, aliasTag) }},
		{name: "CreateAliasFrom", run: func() error { _, err := svc.CreateAliasFrom("chg_alias2", general, a.ID, "ptr"); return err }},
		{name: "SyncAliasStaleness", run: func() error {
			_, err := svc.SyncAliasStaleness(a.ID, "ptr", map[AliasKey]bool{})
			return err
		}},
		{name: "SetTagNote", run: func() error { return svc.SetTagNote(a.ID, "note", nil) }},
		{name: "RenameTag", run: func() error { return svc.RenameTag(e.ID, "chg_e2") }},
		{name: "RenameTagKeepAlias", run: func() error { return svc.RenameTagKeepAlias(e.ID, "chg_e3") }},
		{name: "CreateCategory", run: func() error {
			c, err := svc.CreateCategory("chgcat", "#123456")
			if err == nil {
				cat = c.ID
			}
			return err
		}},
		{name: "RenameCategory", run: func() error { return svc.RenameCategory(cat, "chgcat2") }},
		{name: "RenameCategory onto a taken name", fails: true, run: func() error { return svc.RenameCategory(cat, "general") }},
		{name: "UpdateCategoryColor", run: func() error { return svc.UpdateCategoryColor(cat, "#654321") }},
		{name: "ChangeTagCategory", run: func() error { return svc.ChangeTagCategory(f.ID, cat) }},
		{name: "ChangeTagCategoryMerge", run: func() error { _, err := svc.ChangeTagCategoryMerge(f.ID, general, false); return err }},
		{name: "DeleteCategoryMoveOrDelete", run: func() error { return svc.DeleteCategoryMoveOrDelete(cat, "delete_all", 0) }},
		{name: "DropSourceFromImageTags", run: func() error {
			_, _, err := svc.DropSourceFromImageTags(img2, "danbooru", nil)
			return err
		}},
		{name: "RemoveTagFromImage", run: func() error { _, err := svc.RemoveTagFromImage(img, b.ID); return err }},
		{name: "RemoveTagsFromOneImage", run: func() error { return svc.RemoveTagsFromOneImage(img2, []int64{a.ID}) }},
		{name: "RemoveAutoTagsFromImage", run: func() error { _, err := svc.RemoveAutoTagsFromImage(img, nil); return err }},
		{name: "RemoveSourceTagsFromImage", run: func() error {
			_, err := svc.RemoveSourceTagsFromImage(img, []string{"danbooru"}, "")
			return err
		}},
		{name: "RemoveStaleTagsFromImage", run: func() error { _, err := svc.RemoveStaleTagsFromImage(img); return err }},
		{name: "RemoveCategoryTagsFromImage", run: func() error {
			_, err := svc.RemoveCategoryTagsFromImage(img, "general")
			return err
		}},
		{name: "RemoveUserTagsFromImage", run: func() error { _, err := svc.RemoveUserTagsFromImage(img); return err }},
		{name: "RemoveAllTagsFromImage", run: func() error { return svc.RemoveAllTagsFromImage(img2) }},
		{name: "StripTags", run: func() error {
			_, err := svc.StripTags(context.Background(), []int64{img}, StripSpec{Mode: "stale"}, nil)
			return err
		}},
		{name: "MergeTags", run: func() error { return svc.MergeTags(g.ID, h.ID) }},
		{name: "ScanFoldedDuplicates", run: func() error { _, err := svc.ScanFoldedDuplicates(); return err }},
		{name: "MergeFolded", run: func() error {
			res, err := svc.MergeFolded(context.Background(), []int64{legacy.ID})
			if err == nil && res.Merged != 1 {
				t.Errorf("MergeFolded merged %d, want 1", res.Merged)
			}
			return err
		}},
		{name: "RecalcCount", run: func() error {
			mustExec(t, database.Write, `UPDATE tags SET usage_count = 7 WHERE id = ?`, c.ID)
			_, err := svc.RecalcCount()
			return err
		}},
		{name: "DeleteTag", run: func() error { return svc.DeleteTag(a.ID) }},
	}
	for _, step := range steps {
		before := calls
		err := step.run()
		switch {
		case step.fails:
			if err == nil {
				t.Errorf("%s: want a refusal", step.name)
			}
			if calls != before {
				t.Errorf("%s: a refused write signalled a change", step.name)
			}
		case err != nil:
			t.Errorf("%s: %v", step.name, err)
		case calls == before:
			t.Errorf("%s: committed without signalling a change", step.name)
		}
	}
}
