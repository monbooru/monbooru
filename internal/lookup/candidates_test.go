package lookup

import (
	"slices"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestDueTakesNeverTriedFirstAndSkipsTheIneligible(t *testing.T) {
	database := setupTestDB(t)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for id, fileType := range map[int64]string{1: "png", 2: "png", 3: "cbz", 4: "png", 5: "png"} {
		insertImage(t, database, id, fileType)
	}
	mustExec(t, database, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (2, 'danbooru', '1', 'https://example.test/1')`)
	mustExec(t, database, `UPDATE images SET scheduled_lookup = 0 WHERE id = 5`)
	if err := Record(database, 1, contract.LookupBooru, ResultMiss, 0, now); err != nil {
		t.Fatal(err)
	}

	ids := func(at time.Time) []int64 {
		t.Helper()
		out, err := DueIDs(database, contract.LookupBooru, at)
		if err != nil {
			t.Fatal(err)
		}
		n, err := DueCount(database, contract.LookupBooru, at)
		if err != nil || n != len(out) {
			t.Errorf("count = %d (%v), want %d", n, err, len(out))
		}
		return out
	}
	if got := ids(now); !slices.Equal(got, []int64{4}) {
		t.Errorf("due now = %v, want [4]", got)
	}
	if got := ids(now.AddDate(1, 0, 0)); !slices.Equal(got, []int64{4, 1}) {
		t.Errorf("due in a year = %v, want the never-tried image before the retry", got)
	}
}

func TestDueIDsKeepTheOrderAndDueDropsWhatWasRecordedSince(t *testing.T) {
	database := setupTestDB(t)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for id := int64(1); id <= 4; id++ {
		insertImage(t, database, id, "png")
	}
	if err := Record(database, 1, contract.LookupBooru, ResultMiss, 0, now.AddDate(0, 0, -2)); err != nil {
		t.Fatal(err)
	}
	if err := Record(database, 2, contract.LookupBooru, ResultMiss, 0, now.AddDate(0, 0, -9)); err != nil {
		t.Fatal(err)
	}
	later := now.AddDate(1, 0, 0)
	ids, err := DueIDs(database, contract.LookupBooru, later)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []int64{3, 4, 2, 1}) {
		t.Fatalf("snapshot = %v, want the never-tried rows, then the oldest due", ids)
	}

	if err := Record(database, 3, contract.LookupBooru, ResultMiss, 0, later); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, `UPDATE images SET scheduled_lookup = 0 WHERE id = 1`)
	due, err := Due(database, contract.LookupBooru, later, ids)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, c := range due {
		got = append(got, c.ImageID)
	}
	if !slices.Equal(got, []int64{4, 2}) {
		t.Errorf("re-checked = %v, want the recorded and the opted-out rows gone", got)
	}
	if due, err := Due(database, contract.LookupBooru, later, nil); err != nil || len(due) != 0 {
		t.Errorf("empty slice = %v (%v), want nothing", due, err)
	}
}

func TestStandingOf(t *testing.T) {
	database := setupTestDB(t)
	insertImage(t, database, 1, "png")
	insertImage(t, database, 2, "cbz")
	insertImage(t, database, 3, "png")
	mustExec(t, database, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (3, 'danbooru', '1', 'https://example.test/1')`)
	mustExec(t, database, `UPDATE images SET scheduled_lookup_ptr = 0 WHERE id = 1`)
	for id, want := range map[int64]Standing{
		1: {PTR: false, Booru: true, Candidate: true},
		2: {PTR: true, Booru: true, Candidate: false},
		3: {PTR: true, Booru: true, Candidate: false},
	} {
		got, err := StandingOf(database, id)
		if err != nil || got != want {
			t.Errorf("image %d: standing = %+v (%v), want %+v", id, got, err, want)
		}
	}
	if _, err := StandingOf(database, 404); err == nil {
		t.Error("an unknown image read as a standing")
	}
}
