package tags

import (
	"errors"
	"testing"
)

func TestRejectTag_StripsImagesDropsEdgesAndRefusesTheName(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	watermark := mustTag(t, svc, "watermark", catID)
	text := mustTag(t, svc, "text", catID)
	mustAddImply(t, svc, watermark.ID, text.ID)
	img := insertTestImage(t, database, "a1")
	mustAddTag(t, svc, img, watermark.ID, false, nil)
	if _, err := svc.CreateAlias("wm", catID, watermark.ID); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}

	if err := svc.RejectTag(watermark.ID); err != nil {
		t.Fatalf("RejectTag: %v", err)
	}

	got := mustGetTag(t, svc, watermark.ID)
	if !got.Rejected || got.UsageCount != 0 {
		t.Errorf("tag = rejected %v usage %d, want rejected at usage 0", got.Rejected, got.UsageCount)
	}
	var rows int
	_ = database.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE tag_id = ?`, watermark.ID).Scan(&rows)
	if rows != 0 {
		t.Errorf("image_tags rows = %d, want 0", rows)
	}
	if imps, _ := svc.ListImplications(watermark.ID); len(imps) != 0 {
		t.Errorf("implications after reject = %d, want 0", len(imps))
	}
	for _, name := range []string{"watermark", "wm"} {
		tag, err := svc.GetOrCreateTag(name, catID)
		if !errors.Is(err, ErrTagRejected) {
			t.Errorf("GetOrCreateTag(%q) err = %v, want ErrTagRejected", name, err)
		}
		if tag == nil || tag.ID != watermark.ID {
			t.Errorf("GetOrCreateTag(%q) tag = %v, want the rejected row", name, tag)
		}
	}
	if _, err := svc.AddImplication(text.ID, watermark.ID); !errors.Is(err, ErrTagRejected) {
		t.Errorf("AddImplication onto a rejected tag err = %v, want ErrTagRejected", err)
	}
	if _, err := svc.CreateAlias("water_mark", catID, watermark.ID); !errors.Is(err, ErrAliasToRejected) {
		t.Errorf("CreateAlias to a rejected tag err = %v, want ErrAliasToRejected", err)
	}
	if err := svc.MergeTags(text.ID, watermark.ID); !errors.Is(err, ErrAliasToRejected) {
		t.Errorf("MergeTags into a rejected tag err = %v, want ErrAliasToRejected", err)
	}
	if err := svc.MergeTags(watermark.ID, text.ID); !errors.Is(err, ErrTagRejected) {
		t.Errorf("MergeTags of a rejected tag err = %v, want ErrTagRejected", err)
	}
}

func TestRejectTag_RefusesAliasesAndRatings(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	target := mustTag(t, svc, "target", catID)
	alias, err := svc.CreateAlias("other", catID, target.ID)
	if err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	if err := svc.RejectTag(alias.ID); !errors.Is(err, ErrRejectAlias) {
		t.Errorf("RejectTag(alias) err = %v, want ErrRejectAlias", err)
	}
	if _, err := svc.RejectTagName("other", catID); !errors.Is(err, ErrRejectAlias) {
		t.Errorf("RejectTagName(alias) err = %v, want ErrRejectAlias", err)
	}
	ratingCat := categoryIDOf(t, svc, "rating")
	safe, err := svc.FindTag("general", ratingCat)
	if err != nil {
		t.Fatalf("FindTag(rating general): %v", err)
	}
	if err := svc.RejectTag(safe.ID); !errors.Is(err, ErrRejectRating) {
		t.Errorf("RejectTag(rating) err = %v, want ErrRejectRating", err)
	}
	if err := svc.RejectTag(99999); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("RejectTag(missing) err = %v, want ErrTagNotFound", err)
	}
}

func TestRejectTagName_RejectsANameTheGalleryNeverSaw(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	kept := mustTag(t, svc, "kept", catID)

	tag, err := svc.RejectTagName("lowres", catID)
	if err != nil {
		t.Fatalf("RejectTagName: %v", err)
	}
	if !tag.Rejected || tag.Name != "lowres" {
		t.Fatalf("tag = %+v, want a rejected lowres", tag)
	}
	again, err := svc.RejectTagName("lowres", catID)
	if err != nil || again.ID != tag.ID {
		t.Errorf("second RejectTagName = %v, %v, want the same row", again, err)
	}

	rejected, _, err := svc.ListTags(TagFilter{Type: "rejected", ShowZero: true})
	if err != nil || len(rejected) != 1 || rejected[0].ID != tag.ID || !rejected[0].Rejected {
		t.Errorf("type=rejected listing = %+v, %v, want only lowres", rejected, err)
	}
	plain, _, err := svc.ListTags(TagFilter{Type: "tag", ShowZero: true, Sort: "name"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plain {
		if p.ID == tag.ID {
			t.Errorf("type=tag listing holds the rejected row: %+v", plain)
		}
	}
	hidden, _, err := svc.ListTags(TagFilter{Type: "", ShowZero: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || hidden[0].ID != tag.ID {
		t.Errorf("hide-zero listing = %+v, want the rejected row to pass the gate like an alias", hidden)
	}
	if n, _ := svc.RejectedCount(); n != 1 {
		t.Errorf("RejectedCount = %d, want 1", n)
	}
	suggested, _ := svc.SuggestTags("", "general", 10)
	if len(suggested) != 1 || suggested[0].ID != kept.ID {
		t.Errorf("SuggestTags = %+v, want only kept", suggested)
	}
	if plainUnused, _, _ := svc.UnusedTags(); len(plainUnused) != 1 || plainUnused[0] != kept.ID {
		t.Errorf("UnusedTags = %v, want only kept", plainUnused)
	}
}

func TestUnrejectTag_MakesTheNameATagAgain(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	tag, err := svc.RejectTagName("lowres", catID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UnrejectTag(tag.ID); err != nil {
		t.Fatalf("UnrejectTag: %v", err)
	}
	got, err := svc.GetOrCreateTag("lowres", catID)
	if err != nil || got.ID != tag.ID || got.Rejected {
		t.Errorf("after un-reject GetOrCreateTag = %+v, %v, want the plain row", got, err)
	}
	if err := svc.UnrejectTag(99999); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("UnrejectTag(missing) err = %v, want ErrTagNotFound", err)
	}
}

func TestRejectedName_SilentPathsDropAndCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	rejected, err := svc.RejectTagName("watermark", catID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAlias("wm", catID, rejected.ID); !errors.Is(err, ErrAliasToRejected) {
		t.Fatalf("CreateAlias err = %v", err)
	}

	if _, err := svc.GetOrCreateTagFrom("watermark", catID, "danbooru"); !errors.Is(err, ErrTagRejected) {
		t.Fatalf("site arrival err = %v, want ErrTagRejected", err)
	}
	if _, err := svc.GetOrCreateTag("watermark", catID); !errors.Is(err, ErrTagRejected) {
		t.Fatalf("user typing err = %v, want ErrTagRejected", err)
	}
	ids, err := svc.GetOrCreateTagsFrom([]string{"watermark", "kept"}, catID, "monbooru")
	if err != nil || len(ids) != 1 {
		t.Fatalf("GetOrCreateTagsFrom = %v, %v, want kept alone", ids, err)
	}

	img := insertTestImage(t, database, "b2")
	applyHits(t, svc, img, TaggerHit{Name: "watermark", CategoryID: catID, Score: 0.9, Tagger: "tagger-A"},
		TaggerHit{Name: "kept", CategoryID: catID, Score: 0.9, Tagger: "tagger-A"})
	if names := tagNamesOnImage(t, svc, img); len(names) != 1 || names[0] != "kept" {
		t.Errorf("tagger left %v on the image, want kept alone", names)
	}

	got := mustGetTag(t, svc, rejected.ID)
	if got.RejectedDrops != 3 {
		t.Errorf("RejectedDrops = %d, want 3 (site, meta, tagger; the user's typing does not count)", got.RejectedDrops)
	}
	if err := svc.UnrejectTag(rejected.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGetTag(t, svc, rejected.ID); got.RejectedDrops != 0 || got.Rejected {
		t.Errorf("after un-reject = %+v", got)
	}
}
