package gallery

import (
	"context"
	"testing"
)

func TestIngestStoresBothDigests(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "a.png", 20, 20)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	wantSHA, wantMD5, err := hashFileDigests(path)
	if err != nil {
		t.Fatal(err)
	}
	var gotSHA, gotMD5 string
	if err := database.Read.QueryRow(
		`SELECT sha256, md5 FROM images WHERE id = ?`, rec.ID,
	).Scan(&gotSHA, &gotMD5); err != nil {
		t.Fatal(err)
	}
	if gotSHA != wantSHA || gotMD5 != wantMD5 {
		t.Errorf("stored = %q/%q, want %q/%q", gotSHA, gotMD5, wantSHA, wantMD5)
	}
}

func TestBackfillMD5s(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	a := createTestPNGFileSize(t, galleryDir, "a.png", 20, 20)
	b := createTestPNGFileSize(t, galleryDir, "b.png", 30, 30)
	recA, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, a, "")
	recB, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, b, "")

	mustExec(t, database.Write, `UPDATE images SET md5 = ''`)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, recB.ID)

	processed, updated, err := BackfillMD5s(context.Background(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || updated != 1 {
		t.Errorf("backfill = %d processed / %d updated, want 1/1 (the missing row is skipped)", processed, updated)
	}
	wantA, _ := Md5File(a)
	var gotA, gotB string
	if err := database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, recA.ID).Scan(&gotA); err != nil {
		t.Fatal(err)
	}
	if err := database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, recB.ID).Scan(&gotB); err != nil {
		t.Fatal(err)
	}
	if gotA != wantA {
		t.Errorf("visible row md5 = %q, want %q", gotA, wantA)
	}
	if gotB != "" {
		t.Errorf("missing row md5 = %q, want it left empty", gotB)
	}

	processed, updated, err = BackfillMD5s(context.Background(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 || updated != 0 {
		t.Errorf("second run = %d/%d, want 0/0", processed, updated)
	}
}

func TestBackfillMD5s_SkipsUnreadable(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "gone.png", 20, 20)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	mustExec(t, database.Write, `UPDATE images SET md5 = ''`)
	mustRemove(t, path)

	processed, updated, err := BackfillMD5s(context.Background(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || updated != 0 {
		t.Errorf("backfill = %d/%d, want 1 processed / 0 updated", processed, updated)
	}
	var got string
	if err := database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, rec.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("md5 = %q, want it left empty", got)
	}
}

func TestBackfillMD5s_HonoursCancellation(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "a.png", 20, 20)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `UPDATE images SET md5 = ''`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := BackfillMD5s(ctx, database, nil); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestInPlaceEditMovesBothDigests(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "a.png", 20, 20)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")

	edited := createTestPNGFileSize(t, t.TempDir(), "edited.png", 44, 44)
	bytes := mustReadFile(t, edited)
	mustWriteFile(t, path, bytes, 0o644)
	if _, _, err := Ingest(testHandle(database, galleryDir, env.thumbnailsPath), path, ""); err != nil {
		t.Fatal(err)
	}

	wantSHA, wantMD5, err := hashFileDigests(path)
	if err != nil {
		t.Fatal(err)
	}
	var gotSHA, gotMD5 string
	if err := database.Read.QueryRow(
		`SELECT sha256, md5 FROM images WHERE id = ?`, rec.ID,
	).Scan(&gotSHA, &gotMD5); err != nil {
		t.Fatal(err)
	}
	if gotSHA != wantSHA || gotMD5 != wantMD5 {
		t.Errorf("after edit = %q/%q, want %q/%q", gotSHA, gotMD5, wantSHA, wantMD5)
	}
}
