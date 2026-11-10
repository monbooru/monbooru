package lookup

import (
	"slices"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func mustExec(t *testing.T, database *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Write.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func insertImage(t *testing.T, database *db.DB, id int64, fileType string) {
	t.Helper()
	if _, err := database.Write.Exec(
		`INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, ?, 1)`,
		id, "sha"+time.Now().Format("150405.000000000")+string(rune('a'+id)), "/g/img", fileType,
	); err != nil {
		t.Fatalf("insert image %d: %v", id, err)
	}
}

func rowFor(t *testing.T, database *db.DB, imageID int64, backend string) Row {
	t.Helper()
	rows, err := ForImage(database, imageID)
	if err != nil {
		t.Fatalf("ForImage: %v", err)
	}
	return rows[backend]
}

func TestRecorderConcludesOnlyWhatIsInFlight(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	now := time.Now()

	if err := RecordInFlight(database, 1, contract.LookupBooru, ResultHit, now); err != nil {
		t.Fatalf("RecordInFlight: %v", err)
	}
	if got := rowFor(t, database, 1, contract.LookupBooru); got.Backend != "" {
		t.Errorf("row = %+v, want none written for an image with nothing in flight", got)
	}

	if err := Enqueued(database, 1, []string{contract.LookupPTR, contract.LookupBooru}, 77, now); err != nil {
		t.Fatalf("Enqueued: %v", err)
	}
	if got := rowFor(t, database, 1, contract.LookupPTR); got.QueuedAt.IsZero() || !got.LastAt.IsZero() {
		t.Errorf("row = %+v, want in flight with no concluded outcome yet", got)
	}

	if err := RecordInFlight(database, 1, contract.LookupPTR, ResultHit, now); err != nil {
		t.Fatalf("RecordInFlight: %v", err)
	}
	ptr := rowFor(t, database, 1, contract.LookupPTR)
	if ptr.LastResult != ResultHit || ptr.Attempts != 0 || !ptr.QueuedAt.IsZero() || !ptr.NextDueAt.IsZero() {
		t.Errorf("ptr row = %+v, want a concluded hit with nothing scheduled", ptr)
	}
	if booru := rowFor(t, database, 1, contract.LookupBooru); booru.LastResult != "" {
		t.Errorf("booru row = %+v, want the named backend's row alone concluded", booru)
	}
}

func TestRecorderAdvancesOnlyOnEvidence(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	now := time.Now()

	if err := Record(database, 1, contract.LookupBooru, ResultMiss, 0, now); err != nil {
		t.Fatalf("Record miss: %v", err)
	}
	miss := rowFor(t, database, 1, contract.LookupBooru)
	if miss.Attempts != 1 || miss.LastResult != ResultMiss || miss.NextDueAt.IsZero() {
		t.Fatalf("row = %+v, want one miss with a next try scheduled", miss)
	}

	if err := Record(database, 1, contract.LookupBooru, ResultError, 0, now.Add(time.Hour)); err != nil {
		t.Fatalf("Record error: %v", err)
	}
	after := rowFor(t, database, 1, contract.LookupBooru)
	if after.Attempts != 1 || after.LastResult != ResultMiss || !after.LastAt.Equal(miss.LastAt) {
		t.Errorf("row = %+v, want the miss history untouched by the error", after)
	}
	if after.NextDueAt.After(now.Add(2 * time.Hour)) {
		t.Errorf("next due %s, want the image due now rather than a ladder rung out", after.NextDueAt)
	}
}

func TestBooruLadderExhausts(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	now := time.Now()
	for i := range BooruMaxAttempts {
		if err := Record(database, 1, contract.LookupBooru, ResultMiss, 0, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	row := rowFor(t, database, 1, contract.LookupBooru)
	if row.Attempts != BooruMaxAttempts || !row.NextDueAt.IsZero() || !row.Exhausted() {
		t.Errorf("row = %+v, want an exhausted ladder after %d misses", row, BooruMaxAttempts)
	}

	if err := Reset(database, 1, contract.LookupBooru, now); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	back := rowFor(t, database, 1, contract.LookupBooru)
	if back.Attempts != 0 || back.NextDueAt.IsZero() || back.LastResult != ResultMiss || back.LastAt.IsZero() {
		t.Errorf("row = %+v, want a zeroed ladder with the history kept", back)
	}
}

func TestCandidateClause(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	insertImage(t, database, 2, "cbz")
	insertImage(t, database, 3, "png")
	insertImage(t, database, 4, "png")
	insertImage(t, database, 5, "png")
	insertImage(t, database, 6, "png")

	for _, q := range []string{
		`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (3, 'danbooru', '1', 'https://x/1')`,
		`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (4, 'ptr', '', '')`,
		`UPDATE images SET scheduled_lookup = 0 WHERE id = 5`,
		`UPDATE images SET is_missing = 1 WHERE id = 6`,
	} {
		mustExec(t, database, q)
	}

	candidates := func(backend string) []int64 {
		t.Helper()
		got, err := db.QueryIDs(database.Read, `SELECT i.id FROM images i WHERE `+CandidateClause(backend)+` ORDER BY i.id`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return got
	}

	if got := candidates(contract.LookupBooru); !slices.Equal(got, []int64{1, 4}) {
		t.Errorf("booru candidates = %v, want the unsourced image and the ptr-only one", got)
	}
	if got := candidates(contract.LookupPTR); !slices.Equal(got, []int64{1, 4, 5}) {
		t.Errorf("ptr candidates = %v, want the booru-only opt-out still included", got)
	}
	mustExec(t, database, `UPDATE images SET scheduled_lookup_ptr = 0 WHERE id = 5`)
	if got := candidates(contract.LookupPTR); !slices.Equal(got, []int64{1, 4}) {
		t.Errorf("ptr candidates = %v, want 5 gone once both halves are off", got)
	}
}

func TestDueClauseSelectsWhatItShould(t *testing.T) {
	database := setupTestDB(t)
	for id := int64(1); id <= 4; id++ {
		insertImage(t, database, id, "png")
	}
	now := time.Now()

	if err := Record(database, 2, contract.LookupBooru, ResultMiss, 0, now); err != nil {
		t.Fatal(err)
	}
	if err := Record(database, 3, contract.LookupBooru, ResultMiss, 0, now.Add(-3*week)); err != nil {
		t.Fatal(err)
	}
	if err := Enqueued(database, 4, []string{contract.LookupBooru}, 9, now); err != nil {
		t.Fatal(err)
	}

	clause, args := DueClause(contract.LookupBooru, now)
	got, err := db.QueryIDs(database.Read, `SELECT i.id FROM images i WHERE `+clause+` ORDER BY i.id`, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("due = %v, want the never-tried image and the overdue retry", got)
	}
}

func TestWaitingRespectsTheGrace(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	insertImage(t, database, 2, "png")
	now := time.Now()
	if err := Enqueued(database, 1, []string{contract.LookupBooru}, 5, now); err != nil {
		t.Fatal(err)
	}
	if err := Enqueued(database, 2, []string{contract.LookupBooru}, 6, now.Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	waiting, err := Waiting(database, now.Add(-6*time.Hour))
	if err != nil {
		t.Fatalf("Waiting: %v", err)
	}
	if len(waiting) != 1 || waiting[0].ImageID != 2 || waiting[0].JobID != 6 {
		t.Errorf("waiting = %+v, want only the attempt past the grace window", waiting)
	}
}

func TestDeleteForImageDropsHistory(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	if err := Record(database, 1, contract.LookupBooru, ResultMiss, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := DeleteForImage(database.Write, 1); err != nil {
		t.Fatalf("DeleteForImage: %v", err)
	}
	rows, err := ForImage(database, 1)
	if err != nil {
		t.Fatalf("ForImage: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none once the bytes changed", rows)
	}
}
