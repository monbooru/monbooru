package relations

import (
	"context"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func insertSourcedPair(t *testing.T, database *db.DB, a, b int64, source string, distance int, score any) {
	t.Helper()
	lo, hi := canonicalPair(a, b)
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, source, score) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', ?, ?)`, lo, hi, distance, source, score)
}

func TestPruneQueue_DropsRowsTheSettingsNoLongerNominate(t *testing.T) {
	database, _ := setupTestDB(t)
	ids := make([]int64, 12)
	for i := range ids {
		ids[i] = insertImage(t, database, "pq_"+string(rune('a'+i)), int64(100+i))
	}

	insertSourcedPair(t, database, ids[0], ids[1], SourcePhash, 3, nil)
	insertSourcedPair(t, database, ids[2], ids[3], SourceTags, TagPairDistance(0.9), 0.9)
	insertSourcedPair(t, database, ids[4], ids[5], SourceBoth, 2, 0.95)

	insertSourcedPair(t, database, ids[6], ids[7], SourcePhash, 9, nil)
	insertSourcedPair(t, database, ids[8], ids[9], SourceTags, TagPairDistance(0.6), 0.6)

	insertSourcedPair(t, database, ids[10], ids[11], SourceReview, 0, nil)

	removed, err := PruneQueue(context.Background(), database, FindPairsOptions{
		Distance: 4, TagPairs: true, TagPairThreshold: 0.85,
	})
	if err != nil {
		t.Fatalf("PruneQueue: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, c := range []struct {
		name   string
		a, b   int64
		want   string
		wantOK bool
	}{
		{"in-budget phash", ids[0], ids[1], SourcePhash, true},
		{"in-budget tags", ids[2], ids[3], SourceTags, true},
		{"in-budget both", ids[4], ids[5], SourceBoth, true},
		{"over-distance phash", ids[6], ids[7], "", false},
		{"under-threshold tags", ids[8], ids[9], "", false},
		{"requeued", ids[10], ids[11], SourceReview, true},
	} {
		source, _, _, ok := queueRow(t, database, c.a, c.b)
		if ok != c.wantOK {
			t.Errorf("%s: present = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && source != c.want {
			t.Errorf("%s: source = %q, want %q", c.name, source, c.want)
		}
	}
}

func TestPruneQueue_DemotesRowsOneDetectorStillBacks(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "pq_dem_a", 100)
	b := insertImage(t, database, "pq_dem_b", 110)
	c := insertImage(t, database, "pq_dem_c", 120)
	d := insertImage(t, database, "pq_dem_d", 130)

	insertSourcedPair(t, database, a, b, SourceBoth, 2, 0.6)

	insertSourcedPair(t, database, c, d, SourceBoth, 9, 0.95)

	if _, err := PruneQueue(context.Background(), database, FindPairsOptions{
		Distance: 4, TagPairs: true, TagPairThreshold: 0.85,
	}); err != nil {
		t.Fatalf("PruneQueue: %v", err)
	}
	if source, _, distance, _ := queueRow(t, database, a, b); source != SourcePhash || distance != 2 {
		t.Errorf("tag-side failure: got (%q, %d), want (%q, 2)", source, distance, SourcePhash)
	}
	if source, _, distance, _ := queueRow(t, database, c, d); source != SourceTags || distance != TagPairDistance(0.95) {
		t.Errorf("pixel-side failure: got (%q, %d), want (%q, %d)", source, distance, SourceTags, TagPairDistance(0.95))
	}
}

func TestPruneQueue_TagPassOffClearsTheTagBand(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "pq_off_a", 100)
	b := insertImage(t, database, "pq_off_b", 110)
	c := insertImage(t, database, "pq_off_c", 120)
	d := insertImage(t, database, "pq_off_d", 130)
	insertSourcedPair(t, database, a, b, SourceTags, TagPairDistance(0.99), 0.99)
	insertSourcedPair(t, database, c, d, SourceBoth, 3, 0.99)

	removed, err := PruneQueue(context.Background(), database, FindPairsOptions{
		Distance: 4, TagPairs: false, TagPairThreshold: 0.85,
	})
	if err != nil {
		t.Fatalf("PruneQueue: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, _, _, ok := queueRow(t, database, a, b); ok {
		t.Error("tag-only pair survived the pass being turned off")
	}
	if source, _, _, _ := queueRow(t, database, c, d); source != SourcePhash {
		t.Errorf("both-detector pair: source = %q, want %q", source, SourcePhash)
	}
}
