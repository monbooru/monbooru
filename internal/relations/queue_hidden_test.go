package relations

import (
	"context"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func insertQueuePairRow(t *testing.T, database *db.DB, a, b int64) {
	t.Helper()
	lo, hi := canonicalPair(a, b)
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, 3, '2026-01-01T00:00:00Z')`, lo, hi)
}

func hiddenOf(t *testing.T, database *db.DB, a, b int64) int {
	t.Helper()
	lo, hi := canonicalPair(a, b)
	var hidden int
	if err := database.Read.QueryRow(
		`SELECT collection_hidden FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`,
		lo, hi,
	).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	return hidden
}

func addToCollection(t *testing.T, database *db.DB, name string, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name) VALUES (?, ?)`, id, name)
	}
}

func TestQueuePairHidden_StampedOnInsert(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "qh_a", 100)
	b := insertImage(t, database, "qh_b", 110)
	c := insertImage(t, database, "qh_c", 120)
	addToCollection(t, database, "private", a, b)
	addToCollection(t, database, "open", a, c)
	mustExec(t, database.Write, `INSERT INTO collection_find_relations (name) VALUES ('open')`)
	insertQueuePairRow(t, database, a, b)
	insertQueuePairRow(t, database, a, c)
	if hiddenOf(t, database, a, b) != 1 {
		t.Fatal("a pair sharing an unopted collection was not stamped hidden")
	}
	if hiddenOf(t, database, a, c) != 0 {
		t.Fatal("a pair sharing an opted-in collection was stamped hidden")
	}
}

func TestQueuePairHidden_TracksMembershipChanges(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "qm_a", 100)
	b := insertImage(t, database, "qm_b", 110)
	insertQueuePairRow(t, database, a, b)
	if hiddenOf(t, database, a, b) != 0 {
		t.Fatal("a collectionless pair started hidden")
	}
	addToCollection(t, database, "doujin", a, b)
	if hiddenOf(t, database, a, b) != 1 {
		t.Fatal("filing both images into a collection did not hide the pair")
	}
	mustExec(t, database.Write, `DELETE FROM image_collections WHERE image_id = ? AND name = 'doujin'`, b)
	if hiddenOf(t, database, a, b) != 0 {
		t.Fatal("dropping one membership did not unhide the pair")
	}
}

func TestQueuePairHidden_TracksOptInFlips(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "qo_a", 100)
	b := insertImage(t, database, "qo_b", 110)
	addToCollection(t, database, "doujin", a, b)
	insertQueuePairRow(t, database, a, b)
	if hiddenOf(t, database, a, b) != 1 {
		t.Fatal("the shared-collection pair started visible")
	}
	mustExec(t, database.Write, `INSERT INTO collection_find_relations (name) VALUES ('doujin')`)
	if hiddenOf(t, database, a, b) != 0 {
		t.Fatal("opting the collection in did not unhide the pair")
	}
	mustExec(t, database.Write, `DELETE FROM collection_find_relations WHERE name = 'doujin'`)
	if hiddenOf(t, database, a, b) != 1 {
		t.Fatal("opting the collection back out did not re-hide the pair")
	}
}

func TestQueuePairHidden_TracksRelabel(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "qr_a", 100)
	b := insertImage(t, database, "qr_b", 110)
	addToCollection(t, database, "open", a, b)
	mustExec(t, database.Write, `INSERT INTO collection_find_relations (name) VALUES ('open')`)
	insertQueuePairRow(t, database, a, b)
	if hiddenOf(t, database, a, b) != 0 {
		t.Fatal("the opted-in pair started hidden")
	}
	mustExec(t, database.Write, `UPDATE image_collections SET name = 'private' WHERE name = 'open'`)
	if hiddenOf(t, database, a, b) != 1 {
		t.Fatal("relabelling into an unopted collection did not hide the pair")
	}
}

func rankOf(t *testing.T, database *db.DB, a, b int64) int {
	t.Helper()
	lo, hi := canonicalPair(a, b)
	var rank int
	if err := database.Read.QueryRow(
		`SELECT max_rating_rank FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`,
		lo, hi,
	).Scan(&rank); err != nil {
		t.Fatal(err)
	}
	return rank
}

func attachRatingTag(t *testing.T, database *db.DB, imageID int64, level string) {
	t.Helper()
	var tagID int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE t.name = ? AND tc.name = 'rating'`, level,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	attachTagRow(t, database, imageID, tagID)
}

func TestQueuePairRank_StampedOnInsert(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "rk_a", 100)
	b := insertImage(t, database, "rk_b", 110)
	attachRatingTag(t, database, a, "explicit")
	insertQueuePairRow(t, database, a, b)
	if got := rankOf(t, database, a, b); got != 3 {
		t.Fatalf("max_rating_rank = %d, want the explicit member's 3", got)
	}
}

func TestQueuePairRank_TracksRatingChanges(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "rc_a", 100)
	b := insertImage(t, database, "rc_b", 110)
	insertQueuePairRow(t, database, a, b)
	if got := rankOf(t, database, a, b); got != -1 {
		t.Fatalf("max_rating_rank = %d, want the unrated -1", got)
	}

	attachRatingTag(t, database, b, "questionable")
	if got := rankOf(t, database, a, b); got != 2 {
		t.Fatalf("max_rating_rank = %d after rating add, want 2", got)
	}
	mustExec(t, database.Write, `DELETE FROM image_tags WHERE image_id = ?`, b)
	if got := rankOf(t, database, a, b); got != -1 {
		t.Fatalf("max_rating_rank = %d after rating removal, want -1", got)
	}
}

func TestFindTagPairs_QueueRowsCarryTheHiddenVerdict(t *testing.T) {
	database, _ := setupTestDB(t)
	f := setupTagPairFixture(t, database)
	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if hiddenOf(t, database, f.seed, f.near) != 0 {
		t.Fatal("an admitted tag pair was stamped hidden")
	}
}
