//go:build tagger

package tagger

import (
	"context"
	"fmt"
	"image/color"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/tags"
)

func setupStoreResultsDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

type callRecorder struct {
	mu       sync.Mutex
	failOn   int
	calls    int
	models   [][]string
	releases int

	pageDirs string
	dirs     []int
}

func (b *callRecorder) Run(_ context.Context, req RunRequest) (RunResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.models = append(b.models, req.Models)
	if b.pageDirs != "" {
		found, _ := filepath.Glob(filepath.Join(b.pageDirs, "autotag-*"))
		b.dirs = append(b.dirs, len(found))
	}
	if b.calls == b.failOn {
		return RunResponse{}, fmt.Errorf("worker gone")
	}
	resp := RunResponse{}
	for range req.Models {
		frames := make([]FrameScores, len(req.Frames))
		for i := range frames {
			frames[i].SparseScores = SparseScores{I: []int32{0}, S: []float32{0.9}}
		}
		resp.Results = append(resp.Results, frames)
	}
	return resp, nil
}

func (b *callRecorder) Status() CacheStatus              { return CacheStatus{} }
func (b *callRecorder) ReleaseIdle(_ time.Duration) bool { return false }
func (b *callRecorder) ReleaseAll()                      { b.mu.Lock(); b.releases++; b.mu.Unlock() }

func stubTaggers(t *testing.T, cfg *config.Config, sizes map[string]int) []TaggerStatus {
	t.Helper()
	if cfg.Paths.ModelPath == "" {
		cfg.Paths.ModelPath = t.TempDir()
	}
	var out []TaggerStatus
	for _, name := range slices.Sorted(maps.Keys(sizes)) {
		makeTaggerDir(t, cfg.Paths.ModelPath, name, []string{"model.onnx"})
		dir := filepath.Join(cfg.Paths.ModelPath, name)
		mustWriteFile(t, filepath.Join(dir, "tags.csv"), []byte("tag_id,name,category\n0,landscape,0\n"), 0o644)
		mustWriteFile(t, filepath.Join(dir, "tagger.json"), fmt.Appendf(nil, `{"version": 1, "profile": {"input_size": %d}}`, sizes[name]), 0o644)
		out = append(out, TaggerStatus{TaggerInstance: config.TaggerInstance{Name: name, Enabled: true, ConfidenceThreshold: 0.35, ModelFile: "model.onnx", TagsFile: "tags.csv"}})
	}
	return out
}

func runScope(t *testing.T, database *db.DB, ids []int64, rec *callRecorder, parallel int, sizes map[string]int, thumbs string) error {
	t.Helper()
	mgr := jobs.NewManager()
	if err := mgr.Start("autotag"); err != nil {
		t.Fatal(err)
	}
	prev := activeBackend()
	SetBackend(rec)
	t.Cleanup(func() { SetBackend(prev) })
	cfg := &config.Config{}
	cfg.Tagger.Parallel = parallel
	cfg.Tagger.IdleReleaseAfterMinutes = 15
	taggers := stubTaggers(t, cfg, sizes)
	_, err := RunWithTaggers(context.Background(),
		gallery.Handle{DB: database, TagSvc: tags.New(database, nil), ThumbnailsPath: thumbs},
		cfg, ids, taggers, mgr, "cpu")
	return err
}

func insertPNGImages(t *testing.T, database *db.DB, n int) []int64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pic.png")
	mustWriteFile(t, path, solidPNG(t, 16, 12, color.RGBA{40, 80, 120, 255}), 0o644)
	var ids []int64
	for i := range n {
		res, err := database.Write.Exec(
			`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
			 VALUES (?, ?, '', 'png', 0, 0)`, fmt.Sprintf("%064x", i), path)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	return ids
}

func countTagged(t *testing.T, database *db.DB) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE auto_tagged_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunWithTaggers_TagsEveryImageOnce(t *testing.T) {
	database := setupStoreResultsDB(t)
	ids := insertPNGImages(t, database, 5)
	rec := &callRecorder{}
	if err := runScope(t, database, ids, rec, 2, map[string]int{"stub": 8}, ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.calls != 5 || countTagged(t, database) != 5 {
		t.Errorf("calls %d, images tagged %d, want 5 and 5", rec.calls, countTagged(t, database))
	}
	if rec.releases != 0 {
		t.Errorf("released the backend %d times with an idle release of 15 minutes", rec.releases)
	}
}

func TestRunWithTaggers_SendsTaggersThatShareAnInputTogether(t *testing.T) {
	database := setupStoreResultsDB(t)
	ids := insertPNGImages(t, database, 1)
	rec := &callRecorder{}
	if err := runScope(t, database, ids, rec, 1, map[string]int{"a": 8, "b": 16, "c": 8}, ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fmt.Sprint(rec.models); got != "[[a c] [b]]" {
		t.Errorf("calls carried %s, want [[a c] [b]]", got)
	}
}

func TestRunWithTaggers_KeepsTaggedImagesWhenTheBackendFails(t *testing.T) {
	database := setupStoreResultsDB(t)
	ids := insertPNGImages(t, database, 4)
	err := runScope(t, database, ids, &callRecorder{failOn: 3}, 1, map[string]int{"stub": 8}, "")
	if err == nil {
		t.Fatal("run: want the backend failure to propagate")
	}
	if n := countTagged(t, database); n != 2 {
		t.Errorf("images tagged before the failure = %d, want 2", n)
	}
}

func TestRunWithTaggers_ExtractsOneArchivePerWorker(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	cbz := makeCBZ(t, dir, "m.cbz", [][]byte{pic, pic, pic, pic})

	for _, parallel := range []int{1, 3} {
		database := setupStoreResultsDB(t)
		var ids []int64
		for i := range 3 {
			res, err := database.Write.Exec(
				`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
				 VALUES (?, ?, '', 'cbz', 0, 0)`, fmt.Sprintf("%064x", i), cbz)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			ids = append(ids, id)
		}
		thumbs := t.TempDir()
		rec := &callRecorder{pageDirs: filepath.Dir(gallery.MangaCacheDir(thumbs))}
		if err := runScope(t, database, ids, rec, parallel, map[string]int{"stub": 8}, thumbs); err != nil {
			t.Fatalf("parallel %d: run: %v", parallel, err)
		}
		if rec.calls != 12 {
			t.Errorf("parallel %d: %d calls, want one per page, 12", parallel, rec.calls)
		}
		if most := slices.Max(rec.dirs); most > parallel || most < 1 {
			t.Errorf("parallel %d: up to %d extraction folders on disk at a call", parallel, most)
		}
		if left, _ := filepath.Glob(filepath.Join(rec.pageDirs, "autotag-*")); len(left) != 0 {
			t.Errorf("parallel %d: %d extraction folders left after the run", parallel, len(left))
		}
	}
}

func TestRunWithTaggers_AtZeroIdleReleasesTheBackendOnceAtTheEnd(t *testing.T) {
	database := setupStoreResultsDB(t)
	ids := insertPNGImages(t, database, 3)
	rec := &callRecorder{}
	mgr := jobs.NewManager()
	if err := mgr.Start("autotag"); err != nil {
		t.Fatal(err)
	}
	prev := activeBackend()
	SetBackend(rec)
	t.Cleanup(func() { SetBackend(prev) })
	cfg := &config.Config{}
	cfg.Tagger.Parallel = 2
	taggers := stubTaggers(t, cfg, map[string]int{"stub": 8})
	if _, err := RunWithTaggers(context.Background(), gallery.Handle{DB: database, TagSvc: tags.New(database, nil)}, cfg, ids, taggers, mgr, "cpu"); err != nil {
		t.Fatal(err)
	}
	if rec.releases != 1 {
		t.Errorf("released %d times, want 1", rec.releases)
	}
}
