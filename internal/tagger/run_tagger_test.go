package tagger

import (
	"context"
	"fmt"
	"image/color"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/shared/worker"
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
	fake   *worker.Fake
	mu     sync.Mutex
	failOn int
	calls  int
	models [][]string

	pageDirs string
	dirs     []int
}

func (b *callRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == worker.PathTag {
		b.mu.Lock()
		b.calls++
		if b.pageDirs != "" {
			found, _ := filepath.Glob(filepath.Join(b.pageDirs, "autotag-*"))
			b.dirs = append(b.dirs, len(found))
		}
		fail := b.calls == b.failOn
		b.mu.Unlock()
		if fail {
			http.Error(w, `{"error":"worker gone"}`, http.StatusInternalServerError)
			return
		}
	}
	b.fake.ServeHTTP(w, r)
	if r.URL.Path == worker.PathTag {
		b.fake.Mu.Lock()
		last := b.fake.Calls[len(b.fake.Calls)-1].Models
		b.fake.Mu.Unlock()
		b.mu.Lock()
		b.models = append(b.models, last)
		b.mu.Unlock()
	}
}

func recordWorker(t *testing.T, rec *callRecorder, slots int, sizes map[string]int) []TaggerStatus {
	t.Helper()
	var ms []fakeModel
	for _, name := range slices.Sorted(maps.Keys(sizes)) {
		ms = append(ms, fakeModel{name: name, size: sizes[name], labels: labelsOf("landscape")})
	}
	rec.fake = useFakeWorker(t, ms...)
	rec.fake.Mu.Lock()
	rec.fake.Info.Slots = slots
	rec.fake.Mu.Unlock()
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	currentPool().Set([]WorkerDef{{Name: "fake", URL: srv.URL, Token: "tok"}})
	currentPool().Refresh(context.Background())
	var out []TaggerStatus
	for _, name := range slices.Sorted(maps.Keys(sizes)) {
		out = append(out, TaggerStatus{TaggerInstance: config.TaggerInstance{Name: name, Enabled: true, ConfidenceThreshold: 0.35}})
	}
	return out
}

func runScope(t *testing.T, database *db.DB, ids []int64, rec *callRecorder, slots int, sizes map[string]int, thumbs string) error {
	t.Helper()
	mgr := jobs.NewManager()
	if err := mgr.Start("autotag"); err != nil {
		t.Fatal(err)
	}
	taggers := recordWorker(t, rec, slots, sizes)
	cfg := &config.Config{}
	cfg.Paths.DataPath = t.TempDir()
	_, err := RunWithTaggers(context.Background(),
		gallery.Handle{DB: database, TagSvc: tags.New(database, nil), ThumbnailsPath: thumbs},
		cfg, ids, taggers, mgr)
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
