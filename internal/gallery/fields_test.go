package gallery

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func seedRows(t *testing.T, database *db.DB, n int) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = insertImage(t, database, false)
	}
	return ids
}

func countWhere(t *testing.T, database *db.DB, where string, args ...any) int {
	t.Helper()
	return countRows(t, database, "images WHERE "+where, args...)
}

func TestToggleFlag_ReportsTheNewValue(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	for _, want := range []bool{true, false} {
		got, err := ToggleFlag(database, id, FlagFavorited)
		if err != nil || got != want {
			t.Fatalf("toggle = %v (%v), want %v", got, err, want)
		}
	}
	if _, err := ToggleFlag(database, 404, FlagFavorited); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown id err = %v, want sql.ErrNoRows", err)
	}
}

func TestToggleFlagsAndSetFlag_CoverIDsOverOneChunk(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids := seedRows(t, database, 600)
	if err := ToggleFlags(database, ids, FlagFavorited); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "is_favorited = 1"); n != 600 {
		t.Fatalf("favorited after the toggle = %d, want 600", n)
	}
	if err := SetFlag(database, ids, FlagInbox, false); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "is_inbox = 0"); n != 600 {
		t.Errorf("archived after SetFlag = %d, want 600", n)
	}
}

func TestSetNote_RefusesAnUnknownID(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := SetNote(database, id, "kept"); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "note = 'kept'"); n != 1 {
		t.Errorf("rows with the note = %d, want 1", n)
	}
	if err := SetNote(database, 404, "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown id err = %v, want sql.ErrNoRows", err)
	}
}

func TestSetReadPage_ClearsOnPageZero(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := SetReadPage(database, id, 7); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "last_read_page = 7"); n != 1 {
		t.Fatalf("rows at page 7 = %d, want 1", n)
	}
	if err := SetReadPage(database, id, 0); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "last_read_page IS NULL"); n != 1 {
		t.Errorf("rows with no position = %d, want 1", n)
	}
}

func TestSetScheduledLookup_ResetsOnlyTheNamedBackendsLadder(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids := seedRows(t, database, 600)
	for _, id := range ids {
		for _, b := range []string{contract.LookupBooru, contract.LookupPTR} {
			mustExec(t, database.Write, `INSERT INTO image_lookups (image_id, backend, attempts) VALUES (?, ?, 5)`, id, b)
		}
	}
	both := []string{contract.LookupBooru, contract.LookupPTR}
	if err := SetScheduledLookup(database, ids, both, false); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "scheduled_lookup = 0 AND scheduled_lookup_ptr = 0"); n != 600 {
		t.Fatalf("opted out of both = %d, want 600", n)
	}
	if err := SetScheduledLookup(database, ids, []string{contract.LookupPTR}, true); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "scheduled_lookup = 0 AND scheduled_lookup_ptr = 1"); n != 600 {
		t.Errorf("opted back into the PTR only = %d, want 600", n)
	}
	if n := countRows(t, database, "image_lookups WHERE backend = ? AND attempts = 0", contract.LookupPTR); n != 600 {
		t.Errorf("PTR ladders reset = %d, want 600", n)
	}
	if n := countRows(t, database, "image_lookups WHERE backend = ? AND attempts = 5", contract.LookupBooru); n != 600 {
		t.Errorf("booru ladders left alone = %d, want 600", n)
	}
}

func TestSetScheduledLookup_OptOutKeepsARunningLadderFromReadingExhausted(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := lookup.Record(database, id, contract.LookupBooru, lookup.ResultMiss, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := SetScheduledLookup(database, []int64{id}, []string{contract.LookupBooru}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := lookup.ForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if booru := rows[contract.LookupBooru]; booru.Exhausted() || booru.NextDueAt.IsZero() {
		t.Errorf("booru row = %+v, want its due date kept and the ladder not exhausted", booru)
	}
}

func TestStampUploadBatch_CoversIDsOverOneChunk(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids := seedRows(t, database, 600)
	if err := StampUploadBatch(database, ids, 42); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "upload_batch = 42"); n != 600 {
		t.Errorf("stamped rows = %d, want 600", n)
	}
}

func TestSetDimensions(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := SetDimensions(context.Background(), database, id, 640, 480); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "width = 640 AND height = 480"); n != 1 {
		t.Errorf("rows at 640x480 = %d, want 1", n)
	}
}

func TestApplyReExtract_LeavesTheDurationAloneWhenNil(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	d := 12.5
	if err := ApplyReExtract(context.Background(), database, id, "a1111", &d, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := ApplyReExtract(context.Background(), database, id, "none", nil, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, database, "source_type = 'none' AND duration_seconds = 12.5"); n != 1 {
		t.Errorf("rows re-extracted with the duration kept = %d, want 1", n)
	}
}

func TestRebuildComfyTerms_RederivesTermsAlreadyIndexed(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := seedComfyImage(t, database)
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: testWorkflow})
	mustExec(t, database.Write, `DELETE FROM comfyui_terms WHERE image_id = ?`, id)
	if n, err := BackfillComfyTerms(context.Background(), database, nil); err != nil || n != 0 {
		t.Fatalf("test premise broken: the backfill re-indexed %d (%v)", n, err)
	}
	if _, err := RebuildComfyTerms(context.Background(), database, nil); err != nil {
		t.Fatal(err)
	}
	if len(comfyTerms(t, database, id)) == 0 {
		t.Error("the rebuild left the workflow without terms")
	}
}

func TestRenameCollectionFindRelations(t *testing.T) {
	database := newCollectionsTestDB(t)
	flagged := func(name string) bool {
		return countRows(t, database, "collection_find_relations WHERE name = ?", name) == 1
	}
	if err := SetCollectionFindRelations(database, "Old", true); err != nil {
		t.Fatal(err)
	}
	if err := RenameCollectionFindRelations(database, "Old", "New", true); err != nil {
		t.Fatal(err)
	}
	if !flagged("New") || flagged("Old") {
		t.Fatalf("after a rename: new %t, old %t; want only the new label flagged", flagged("New"), flagged("Old"))
	}
	if err := SetCollectionFindRelations(database, "Other", true); err != nil {
		t.Fatal(err)
	}
	if err := RenameCollectionFindRelations(database, "New", "Other", true); err != nil {
		t.Fatal(err)
	}
	if !flagged("Other") || flagged("New") {
		t.Errorf("after a merge: target %t, source %t; want the target kept and the source gone", flagged("Other"), flagged("New"))
	}
	if err := RenameCollectionFindRelations(database, "Other", "OTHER", false); err != nil {
		t.Fatal(err)
	}
	if !flagged("OTHER") {
		t.Error("a case-only rename lost the flag")
	}
}

func TestFileTypes_LeavesOutIDsWithNoRow(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids := seedRows(t, database, 2)
	mustExec(t, database.Write, `UPDATE images SET file_type = 'cbz' WHERE id = ?`, ids[1])
	got, err := FileTypes(database, append(ids, 404))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int64]string{ids[0]: "image", ids[1]: "cbz"}; !maps.Equal(got, want) {
		t.Errorf("types = %v, want %v", got, want)
	}
}
