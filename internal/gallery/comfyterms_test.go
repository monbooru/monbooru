package gallery

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

const testWorkflow = `{"1": {"class_type": "KSampler", "inputs": {"sampler_name": "dpmpp_2m"}}}`

func comfyTerms(t *testing.T, database *db.DB, imageID int64) []string {
	t.Helper()
	terms, err := db.QueryStrings(database.Read,
		`SELECT term FROM comfyui_terms WHERE image_id = ? ORDER BY term`, imageID)
	if err != nil {
		t.Fatalf("read terms: %v", err)
	}
	return terms
}

func replaceComfy(t *testing.T, database *db.DB, imageID int64, comfy *models.ComfyUIMetadata) {
	t.Helper()
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := replaceGenerationMetadata(context.Background(), tx, imageID, nil, comfy); err != nil {
		t.Fatalf("replace metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedComfyImage(t *testing.T, database *db.DB) int64 {
	t.Helper()
	res := mustExec(t, database.Write,
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size) VALUES ('a', '/g/a.png', '', 'png', 1)`)
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReplaceGenerationMetadata_WritesTerms(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)

	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: testWorkflow})
	want := []string{"node=KSampler", "sampler_name=dpmpp_2m"}
	if got := comfyTerms(t, database, id); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("terms = %q, want %q", got, want)
	}
	var version int
	if err := database.Read.QueryRow(
		`SELECT terms_version FROM comfyui_metadata WHERE image_id = ?`, id).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != metadata.ComfyTermsVersion {
		t.Errorf("terms_version = %d, want %d", version, metadata.ComfyTermsVersion)
	}
}

func TestReplaceGenerationMetadata_ReplacesAndClearsTerms(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: testWorkflow})

	replaceComfy(t, database, id, &models.ComfyUIMetadata{
		RawWorkflow: `{"1": {"class_type": "SamplerCustom", "inputs": {"steps": 30}}}`,
	})
	want := []string{"node=SamplerCustom", "steps=30"}
	if got := comfyTerms(t, database, id); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("after replace terms = %q, want %q", got, want)
	}

	replaceComfy(t, database, id, nil)
	if got := comfyTerms(t, database, id); len(got) != 0 {
		t.Errorf("after strip terms = %q, want none", got)
	}
}

func TestComfyTermsCascadeOnImageDelete(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: testWorkflow})

	mustExec(t, database.Write, `DELETE FROM images WHERE id = ?`, id)
	if n := countRows(t, database, "comfyui_terms"); n != 0 {
		t.Errorf("terms after delete = %d, want 0", n)
	}
}

func TestWriteComfyTerms_EmptyWorkflowStampsRow(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: ""})

	var version int
	var terms int
	if err := database.Read.QueryRow(`
		SELECT cm.terms_version, (SELECT COUNT(*) FROM comfyui_terms WHERE image_id = cm.image_id)
		FROM comfyui_metadata cm WHERE cm.image_id = ?`, id).Scan(&version, &terms); err != nil {
		t.Fatal(err)
	}
	if version != metadata.ComfyTermsVersion || terms != 0 {
		t.Errorf("terms_version = %d, terms = %d; want %d / 0", version, terms, metadata.ComfyTermsVersion)
	}
}

func TestBackfillComfyTerms(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: testWorkflow})
	mustExec(t, database.Write, `DELETE FROM comfyui_terms`)
	mustExec(t, database.Write, `UPDATE comfyui_metadata SET terms_version = 0`)

	pending, err := ComfyTermsPending(database)
	if err != nil || pending != 1 {
		t.Fatalf("pending = %d, %v; want 1", pending, err)
	}
	indexed, err := BackfillComfyTerms(context.Background(), database, nil)
	if err != nil || indexed != 1 {
		t.Fatalf("indexed = %d, %v; want 1", indexed, err)
	}
	if got := comfyTerms(t, database, id); len(got) != 2 {
		t.Errorf("terms = %q, want 2", got)
	}

	indexed, err = BackfillComfyTerms(context.Background(), database, nil)
	if err != nil || indexed != 0 {
		t.Errorf("second run indexed = %d, %v; want 0", indexed, err)
	}
}

func TestBackfillComfyTerms_CommitsInChunksOf50(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	for i := range 120 {
		res := mustExec(t, database.Write,
			`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size) VALUES (?, ?, '', 'png', 1)`,
			fmt.Sprintf("s%d", i), fmt.Sprintf("/g/%d.png", i))
		id, _ := res.LastInsertId()
		mustExec(t, database.Write, `INSERT INTO comfyui_metadata (image_id, raw_workflow) VALUES (?, ?)`, id, testWorkflow)
	}

	var steps []int
	if _, err := BackfillComfyTerms(context.Background(), database, func(processed, _ int, _ string) {
		steps = append(steps, processed)
	}); err != nil {
		t.Fatal(err)
	}

	if want := []int{0, 50, 100, 120, 120}; !slices.Equal(steps, want) {
		t.Errorf("progress = %v, want %v", steps, want)
	}
}

func TestBackfillComfyTerms_CancelResumes(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: testWorkflow})
	mustExec(t, database.Write, `UPDATE comfyui_metadata SET terms_version = 0`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BackfillComfyTerms(ctx, database, nil); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if pending, _ := ComfyTermsPending(database); pending != 1 {
		t.Fatalf("pending after cancel = %d, want 1", pending)
	}
	if _, err := BackfillComfyTerms(context.Background(), database, nil); err != nil {
		t.Fatal(err)
	}
	if pending, _ := ComfyTermsPending(database); pending != 0 {
		t.Errorf("pending after resume = %d, want 0", pending)
	}
}

func TestBackfillComfyTerms_RederivesTheRecipeColumns(t *testing.T) {
	database, _, _ := setupSyncTest(t)
	id := seedComfyImage(t, database)
	raw := `{"1": {"class_type": "KSampler", "inputs": {"seed": 1, "steps": 20, "cfg": 7, "sampler_name": "euler", "scheduler": "normal"}},
		"2": {"class_type": "KSampler", "inputs": {"seed": 2, "steps": 10, "cfg": 5, "sampler_name": "euler", "scheduler": "normal"}}}`
	replaceComfy(t, database, id, &models.ComfyUIMetadata{RawWorkflow: raw, Sampler: "euler/normal/normal", GenerationHash: "stale"})
	mustExec(t, database.Write, `UPDATE comfyui_metadata SET terms_version = 1`)

	if _, err := BackfillComfyTerms(context.Background(), database, nil); err != nil {
		t.Fatal(err)
	}
	var sampler, hash string
	if err := database.Read.QueryRow(`SELECT sampler, generation_hash FROM comfyui_metadata WHERE image_id = ?`, id).Scan(&sampler, &hash); err != nil {
		t.Fatal(err)
	}
	if want := metadata.ReparseComfyWorkflow(raw); sampler != "euler/normal" || hash != want.GenerationHash {
		t.Errorf("sampler %q hash %q, want euler/normal and %q", sampler, hash, want.GenerationHash)
	}
}
