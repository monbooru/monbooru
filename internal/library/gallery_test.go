package library

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestIndexWorkflows_DropsCachedSearchesWhenDone(t *testing.T) {
	cx := openTestGallery(t)
	id, _ := mustExec(t, cx.DB.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES ('s', ?, 'png', 1)`, filepath.Join(cx.GalleryPath, "a.png")).LastInsertId()
	mustExec(t, cx.DB.Write, `INSERT INTO comfyui_metadata (image_id, raw_workflow) VALUES (?, ?)`, id, `{"1": {"class_type": "KSampler", "inputs": {}}}`)
	q := search.Query{Cached: true, Text: "comfyui:KSampler", Sort: "newest", Order: "desc"}
	cx.cache.Seed(q, []int64{id})

	cx.indexWorkflows()

	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, cached := search.PageOf(cx.Source(), q, id, 1); !cached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the workflow index left the gallery's cached searches in place")
		}
	}
}

func TestCeilingCached_StaleMapIsIgnored(t *testing.T) {
	cx := openTestGallery(t)
	ceiling := NewCeiling("general", cx)
	before := cx.cacheGen.Load()
	seedRow(t, cx, "a.png", nil, false)
	cx.InvalidateCaches()

	storeByCeiling(&cx.inboxCountUnder, before, "general", 0)

	if n, err := cx.InboxCountUnder(ceiling); err != nil || n != 1 {
		t.Fatalf("InboxCountUnder = (%d, %v), want (1, nil)", n, err)
	}
}

func TestInboxCountUnder_MatchesTheCeilingSearch(t *testing.T) {
	cx := openTestGallery(t)
	rate := func(id int64, level string) {
		t.Helper()
		if _, err := cx.DB.Write.Exec(`INSERT INTO image_tags (image_id, tag_id)
			SELECT ?, t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
			WHERE tc.name = 'rating' AND t.name = ?`, id, level); err != nil {
			t.Fatal(err)
		}
	}
	seedRow(t, cx, "unrated.png", nil, false)
	for _, level := range contract.RatingLevels {
		rate(seedRow(t, cx, level+".png", nil, false), level)
	}
	rate(seedRow(t, cx, "gone.png", nil, true), "general")
	read := seedRow(t, cx, "read.png", nil, false)
	rate(read, "general")
	mustExec(t, cx.DB.Write, `UPDATE images SET is_inbox = 0 WHERE id = ?`, read)

	for level, want := range map[string]int{"general": 2, "sensitive": 3, "questionable": 4} {
		ceiling := NewCeiling(level, cx)
		n, err := cx.InboxCountUnder(ceiling)
		if err != nil {
			t.Fatal(err)
		}
		res, err := search.Execute(context.Background(), cx.Source(), search.Query{Expr: ceiling.Apply(search.Parse("inbox:true")), Page: 1, Limit: 40})
		if err != nil {
			t.Fatal(err)
		}
		if n != want || res.Total != want {
			t.Errorf("under %s: InboxCountUnder = %d, inbox:true search total = %d, want %d", level, n, res.Total, want)
		}
	}
}

func TestShrinkMemory_SkipsAGalleryThatIsExporting(t *testing.T) {
	cx := openTestGallery(t)
	cx.BeginExport()
	defer cx.EndExport()

	tx, err := cx.DB.Read.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	done := make(chan error, 1)
	go func() { done <- cx.ShrinkMemory() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the shrink waited on the export's connection")
	}
}
