package relations

import (
	"context"
	"testing"
)

func TestBackfillPhashes(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	c := insertImage(t, database, "c", 3000)

	mustExec(t, database.Write, `UPDATE images SET phash = NULL WHERE id IN (?, ?, ?)`, a, b, c)
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, a)
	writeTestThumb(t, thumbs, b)
	writeTestThumb(t, thumbs, c)

	processed, updated, err := BackfillPhashes(context.Background(), database, nil, thumbs, nil)
	if err != nil {
		t.Fatalf("BackfillPhashes: %v", err)
	}
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
	if updated != 3 {
		t.Fatalf("updated = %d, want 3 (all thumbnails decodable)", updated)
	}

	nullCount := countRows(t, database, "images WHERE phash IS NULL")
	if nullCount != 0 {
		t.Fatalf("rows still NULL = %d, want 0 (every seeded row backfilled)", nullCount)
	}
}

func TestBackfillPhashesSkipsMissingAndUndecodable(t *testing.T) {
	database, _ := setupTestDB(t)
	good := insertImage(t, database, "good", 1000)
	noThumb := insertImage(t, database, "nothumb", 2000)
	missing := insertImage(t, database, "missing", 3000)
	mustExec(t, database.Write, `UPDATE images SET phash = NULL`)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, missing)
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, good)

	processed, updated, err := BackfillPhashes(context.Background(), database, nil, thumbs, nil)
	if err != nil {
		t.Fatalf("BackfillPhashes: %v", err)
	}
	if processed != 2 {
		t.Fatalf("processed = %d, want 2 (good + noThumb; missing excluded)", processed)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1 (only good has a decodable thumbnail)", updated)
	}

	var phashGood, phashNoThumb, phashMissing interface{}
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, good).Scan(&phashGood); err != nil {
		t.Fatal(err)
	}
	if phashGood == nil {
		t.Fatal("good row still NULL, want a computed phash")
	}
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, noThumb).Scan(&phashNoThumb); err != nil {
		t.Fatal(err)
	}
	if phashNoThumb != nil {
		t.Fatal("noThumb row got a phash, want NULL (no thumbnail to decode)")
	}
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, missing).Scan(&phashMissing); err != nil {
		t.Fatal(err)
	}
	if phashMissing != nil {
		t.Fatal("missing (is_missing=1) row got a phash, want NULL (never a candidate)")
	}
}

func TestBackfillPhashesReportsProgress(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "a", 1000)
	b := insertImage(t, database, "b", 2000)
	mustExec(t, database.Write, `UPDATE images SET phash = NULL WHERE id IN (?, ?)`, a, b)
	thumbs := t.TempDir()
	writeTestThumb(t, thumbs, a)
	writeTestThumb(t, thumbs, b)

	var lastProcessed, lastTotal, calls int
	progress := func(processed, total int, _ string) {
		calls++
		lastProcessed, lastTotal = processed, total
	}
	processed, updated, err := BackfillPhashes(context.Background(), database, nil, thumbs, progress)
	if err != nil {
		t.Fatalf("BackfillPhashes: %v", err)
	}
	if calls == 0 {
		t.Fatal("progress callback never fired")
	}
	if lastTotal != 2 {
		t.Fatalf("final total = %d, want 2", lastTotal)
	}
	if lastProcessed != processed || lastProcessed != 2 {
		t.Fatalf("final processed = %d, want %d (== returned processed, == 2)", lastProcessed, processed)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2", updated)
	}
}
