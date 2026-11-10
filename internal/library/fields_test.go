package library

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestFieldWrites_DropTheCachedSearches(t *testing.T) {
	cx := openTestGallery(t)
	id := seedRow(t, cx, "a.png", []byte("a"), false)
	q := search.Query{Cached: true, Text: "fav:true", Sort: "newest", Order: "desc"}
	for name, write := range map[string]func() error{
		"ToggleFlag":         func() error { _, err := cx.ToggleFlag(id, gallery.FlagFavorited); return err },
		"ToggleFlags":        func() error { return cx.ToggleFlags([]int64{id}, gallery.FlagInbox) },
		"SetFlag":            func() error { return cx.SetFlag([]int64{id}, gallery.FlagFavorited, true) },
		"SetScheduledLookup": func() error { return cx.SetScheduledLookup([]int64{id}, []string{contract.LookupPTR}, false) },
		"SetDimensions":      func() error { return cx.SetDimensions(context.Background(), id, 4, 3) },
	} {
		cx.cache.Seed(q, []int64{id})
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, cached := search.PageOf(cx.Source(), q, id, 1); cached {
			t.Errorf("%s left the gallery's cached searches in place", name)
		}
	}
}

func TestStoreMD5_DropsTheCachesOnlyWhenItStored(t *testing.T) {
	cx := openTestGallery(t)
	stored := seedRow(t, cx, "a.png", []byte("a"), false)
	unreadable := seedRow(t, cx, "gone.png", nil, false)
	q := search.Query{Cached: true, Text: "-md5:", Sort: "newest", Order: "desc"}
	for id, wantDropped := range map[int64]bool{unreadable: false, stored: true} {
		cx.cache.Seed(q, []int64{stored, unreadable})
		_, err := cx.StoreMD5(context.Background(), id)
		if (err == nil) != wantDropped {
			t.Fatalf("image %d: StoreMD5 err = %v", id, err)
		}
		if _, cached := search.PageOf(cx.Source(), q, id, 1); cached == wantDropped {
			t.Errorf("image %d: cached searches kept = %v, want %v", id, cached, !wantDropped)
		}
	}
}

func TestEdit_DropsTheCachesWhateverTheWriteReturned(t *testing.T) {
	cx := openTestGallery(t)
	id := seedRow(t, cx, "a.png", []byte("a"), false)
	q := search.Query{Cached: true, Sort: "newest", Order: "desc"}
	failed := errors.New("failed half-way")
	for _, want := range []error{nil, failed} {
		cx.cache.Seed(q, []int64{id})
		if err := cx.Edit(func(*db.DB) error { return want }); err != want {
			t.Fatalf("Edit returned %v, want %v", err, want)
		}
		if _, cached := search.PageOf(cx.Source(), q, id, 1); cached {
			t.Errorf("an edit returning %v left the cached searches in place", want)
		}
	}
}

func TestPlaceImage_RefreshesTheFolderTree(t *testing.T) {
	cx := openTestGallery(t)
	id := seedRow(t, cx, "a.png", []byte("a"), false)
	if _, err := cx.folderTree(); err != nil {
		t.Fatal(err)
	}
	folder := "sub"
	if _, err := cx.PlaceImage(id, &folder, nil); err != nil {
		t.Fatal(err)
	}
	tree, err := cx.folderTree()
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || !slices.ContainsFunc(tree[0].Children, func(n gallery.FolderNode) bool { return n.Path == folder }) {
		t.Errorf("the folder tree %+v lacks the folder the image moved to", tree)
	}
}

func TestInvalidatePhashMissing_KeepsTheFolderTree(t *testing.T) {
	cx := openTestGallery(t)
	seedRow(t, cx, "a.png", []byte("a"), false)
	if _, err := cx.folderTree(); err != nil {
		t.Fatal(err)
	}
	before := cx.folderTreeAll.Load()
	cx.invalidatePhashMissing()
	if _, err := cx.folderTree(); err != nil {
		t.Fatal(err)
	}
	if cx.folderTreeAll.Load() != before {
		t.Error("the phash drop rebuilt the folder tree")
	}
}

func TestWarmCaches_LeavesTheFolderTreeUnread(t *testing.T) {
	cx := openTestGallery(t)
	seedRow(t, cx, "a.png", []byte("a"), false)
	cx.warmCaches()
	if cx.folderTreeAll.Load() != nil {
		t.Error("warming the caches read the folder tree")
	}
}

func TestRecomputePhash_FeedsTheTreeAndTheMissingCount(t *testing.T) {
	cx := openTestGallery(t)
	res, err := land(t, cx, "a.png", pngBytes(t, 70), LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Image.ID
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = NULL WHERE id = ?`, id)
	cx.invalidatePhashMissing()
	if n, err := cx.PhashMissingUnder(nil); err != nil || n != 1 {
		t.Fatalf("test premise broken: %d missing (%v), want 1", n, err)
	}
	if err := cx.bkTree.EnsureBuilt(cx.DB); err != nil {
		t.Fatal(err)
	}
	h, err := cx.RecomputePhash(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := cx.PhashMissingUnder(nil); err != nil || n != 0 {
		t.Errorf("missing after the recompute = %d (%v), want 0", n, err)
	}
	if ids, _ := cx.bkTree.SearchWithinDistance(h, 0); !slices.Contains(ids, id) {
		t.Errorf("the built tree lacks image %d: %v", id, ids)
	}
}

func TestRecomputePhash_DropsCachedSearches(t *testing.T) {
	cx := openTestGallery(t)
	res, err := land(t, cx, "a.png", pngBytes(t, 75), LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Image.ID
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = NULL WHERE id = ?`, id)
	q := search.Query{Cached: true, Text: "-phash:", Sort: "newest", Order: "desc"}
	cx.cache.Seed(q, []int64{id})

	if _, err := cx.RecomputePhash(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, cached := search.PageOf(cx.Source(), q, id, 1); cached {
		t.Error("the recompute left the gallery's cached searches in place")
	}
}

func TestBackfillPhashes_DropsTheMissingCountAndTheBuiltTree(t *testing.T) {
	cx := openTestGallery(t)
	res, err := land(t, cx, "a.png", pngBytes(t, 80), LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = NULL WHERE id = ?`, res.Image.ID)
	cx.invalidatePhashMissing()
	if n, err := cx.PhashMissingUnder(nil); err != nil || n != 1 {
		t.Fatalf("test premise broken: %d missing (%v), want 1", n, err)
	}
	if err := cx.bkTree.EnsureBuilt(cx.DB); err != nil {
		t.Fatal(err)
	}
	if _, updated, err := cx.BackfillPhashes(context.Background(), nil); err != nil || updated != 1 {
		t.Fatalf("backfill updated %d (%v), want 1", updated, err)
	}
	if n, err := cx.PhashMissingUnder(nil); err != nil || n != 0 {
		t.Errorf("missing after the backfill = %d (%v), want 0", n, err)
	}
	if cx.bkTree.Built() {
		t.Error("the tree built before the backfill was kept")
	}
}
