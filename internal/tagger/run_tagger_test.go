package tagger

import (
	"context"
	"database/sql"
	"fmt"
	"image/color"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	fake     *worker.Fake
	mu       sync.Mutex
	failOn   int
	calls    int
	frames   int
	models   [][]string
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
		last := b.fake.Calls[len(b.fake.Calls)-1]
		b.fake.Mu.Unlock()
		b.mu.Lock()
		b.models = append(b.models, last.Models)
		b.frames += last.Frames.Count
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
	CurrentPool().Set([]WorkerDef{{Name: "fake", URL: srv.URL, Token: "tok"}})
	CurrentPool().Refresh(context.Background())
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

	cfg.Tagger.CPUThreads = slots
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
		if rec.frames != 12 || rec.calls > 12 {
			t.Errorf("parallel %d: %d frames in %d calls, want the 12 pages in 12 calls at most", parallel, rec.frames, rec.calls)
		}
		if most := slices.Max(rec.dirs); most > parallel || most < 1 {
			t.Errorf("parallel %d: up to %d extraction folders on disk at a call", parallel, most)
		}
		if left, _ := filepath.Glob(filepath.Join(rec.pageDirs, "autotag-*")); len(left) != 0 {
			t.Errorf("parallel %d: %d extraction folders left after the run", parallel, len(left))
		}
	}
}

func TestPrepareCount(t *testing.T) {
	t.Parallel()
	cores := runtime.GOMAXPROCS(0)
	for _, tc := range []struct {
		name             string
		prepare, threads int
		slots            int
		remote           bool
		want             int
	}{
		{"LocalKeepsTheSlots", 0, 0, 4, false, 4},
		{"LocalWithinCPUThreads", 0, 2, 4, false, 2},
		{"PairedDoublesUpToTheCores", 0, 0, 1, true, max(1, min(2, cores))},
		{"PairedWithinCPUThreads", 0, 1, 4, true, 1},
		{"TheSettingWins", 12, 2, 4, true, 12},
		{"TheSettingNeverUnderTheSlots", 2, 0, 4, false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := prepareCount(config.TaggerConfig{PrepareParallel: tc.prepare, CPUThreads: tc.threads}, tc.slots, tc.remote)
			if got != tc.want {
				t.Errorf("prepareCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunWithTaggers_SendsAnArchivesReadyPagesTogetherInOrder(t *testing.T) {
	dir := t.TempDir()
	var pages [][]byte
	for i := range 20 {
		pages = append(pages, solidPNG(t, 8, 8, color.RGBA{uint8(10 * i), 0, 0, 255}))
	}
	cbz := makeCBZ(t, dir, "m.cbz", pages)
	database := setupStoreResultsDB(t)
	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
		 VALUES (?, ?, '', 'cbz', 0, 0)`, fmt.Sprintf("%064x", 1), cbz)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	rec := &callRecorder{}
	var reds []uint8
	scoreMu := sync.Mutex{}
	taggers := recordWorker(t, rec, 1, map[string]int{"stub": 8})
	rec.fake.Mu.Lock()
	rec.fake.Score = func(_ string, frame []byte) worker.FrameScores {
		scoreMu.Lock()
		reds = append(reds, frame[0])
		scoreMu.Unlock()
		time.Sleep(2 * time.Millisecond)
		return worker.FrameScores{I: []int32{0}, S: []float32{0.9}}
	}
	rec.fake.Mu.Unlock()
	mgr := jobs.NewManager()
	if err := mgr.Start("autotag"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Paths.DataPath = t.TempDir()
	cfg.Tagger.PrepareParallel = 4
	if _, err := RunWithTaggers(context.Background(),
		gallery.Handle{DB: database, TagSvc: tags.New(database, nil), ThumbnailsPath: t.TempDir()},
		cfg, []int64{id}, taggers, mgr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.frames != 20 || rec.calls >= 20 {
		t.Errorf("%d frames in %d calls, want the 20 pages in fewer calls", rec.frames, rec.calls)
	}
	for _, m := range rec.fake.Calls {
		if m.Frames.Count > maxCallFrames {
			t.Errorf("a call carried %d frames, over the %d a call takes", m.Frames.Count, maxCallFrames)
		}
	}
	for i, r := range reds {
		if r != uint8(10*i) {
			t.Fatalf("frame %d reached the worker as page %d: the pages left their order (%v)", i, r/10, reds)
		}
	}
}

func runOn(t *testing.T, database *db.DB, ids []int64, only []string, names ...string) (Result, error) {
	t.Helper()
	mgr := jobs.NewManager()
	if err := mgr.Start("autotag"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Paths.DataPath = t.TempDir()
	var taggers []TaggerStatus
	for _, name := range names {
		taggers = append(taggers, TaggerStatus{TaggerInstance: config.TaggerInstance{Name: name, Enabled: true, ConfidenceThreshold: 0.35}})
	}
	return RunOn(context.Background(), gallery.Handle{DB: database, TagSvc: tags.New(database, nil)}, cfg, ids, taggers, only, mgr)
}

func autoTags(t *testing.T, database *db.DB) []string {
	t.Helper()
	rows, err := database.Read.Query(`SELECT it.tagger_name || ':' || t.name FROM image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.is_auto = 1 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestAJobReadsAnOnDemandWorkersLabelsBeforeItPlans(t *testing.T) {
	database := setupStoreResultsDB(t)
	ids := insertPNGImages(t, database, 1)
	rented := newFake(t, fakeModel{name: "wd", labels: labelsOf("cat", "dog")})
	p := usePool(t, "", WorkerDef{Name: "rented", URL: serve(t, rented), Token: "tok", OnDemand: true})
	if err := p.Wake(context.Background(), []string{"rented"}, nil); err != nil {
		t.Fatal(err)
	}

	etag := fmt.Sprintf("%q", t.Name()+"/replaced")
	rented.Mu.Lock()
	rented.Models[0].Digest, rented.Models[0].LabelsETag = "d-replaced", etag
	rented.Labels["wd"] = worker.Labels{Model: "wd", ETag: etag, CategoryScheme: "wd14_numeric", Labels: labelsOf("dog", "cat")}
	rented.Mu.Unlock()
	if _, err := runOn(t, database, ids, []string{"rented"}, "wd"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := autoTags(t, database); !slices.Equal(got, []string{"wd:dog"}) {
		t.Errorf("stored %v for a score of the worker's first label, want wd:dog", got)
	}
}

func TestATaggerWhoseEveryFrameIsRefusedKeepsItsTagsAndIsNotStamped(t *testing.T) {
	database := setupStoreResultsDB(t)
	ids := insertPNGImages(t, database, 1)
	f := useFakeWorker(t, fakeModel{name: "wd", labels: labelsOf("landscape")}, fakeModel{name: "px", labels: labelsOf("sky")})
	if _, err := runOn(t, database, ids, nil, "wd", "px"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := database.Write.Exec(`DELETE FROM image_tagger_runs`); err != nil {
		t.Fatal(err)
	}
	f.Mu.Lock()
	f.Score = func(model string, _ []byte) worker.FrameScores {
		if model == "px" {
			return worker.FrameScores{Err: "CUDA out of memory"}
		}
		return worker.FrameScores{I: []int32{0}, S: []float32{0.9}}
	}
	f.Mu.Unlock()
	if res, err := runOn(t, database, ids, nil, "wd", "px"); err != nil || res.Total() != 1 {
		t.Fatalf("run with px refusing: %+v, %v", res, err)
	}
	if got := autoTags(t, database); !slices.Equal(got, []string{"px:sky", "wd:landscape"}) {
		t.Errorf("tags = %v, want px's earlier tag kept", got)
	}
	var stamped string
	if err := database.Read.QueryRow(`SELECT group_concat(tagger_name) FROM image_tagger_runs`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != "wd" {
		t.Errorf("stamped as read by %q, want wd alone", stamped)
	}
}

func TestAFailedCallDoesNotCountTheImagesItCancelsAsSkipped(t *testing.T) {
	database := setupStoreResultsDB(t)

	ids := insertPNGImages(t, database, 8)
	f := newFake(t, fakeModel{name: "wd", labels: labelsOf("landscape")})
	readers := database.Read.Stats().MaxOpenConnections
	held := make(chan *sql.Conn, readers)
	holding := make(chan struct{})
	var calls atomic.Int32
	usePool(t, "", WorkerDef{Name: "fake", URL: serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != worker.PathTag {
			f.ServeHTTP(w, r)
			return
		}
		if calls.Add(1) != 1 {
			<-holding
			f.ServeHTTP(w, r)
			return
		}

		for range readers {
			c, err := database.Read.Conn(context.Background())
			if err != nil {
				t.Error(err)
				break
			}
			held <- c
		}
		close(holding)
		waited := database.Read.Stats().WaitCount
		for deadline := time.Now().Add(5 * time.Second); database.Read.Stats().WaitCount == waited && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		http.Error(w, `{"error":"worker gone"}`, http.StatusInternalServerError)
	})), Token: "tok"})
	res, err := runOn(t, database, ids, nil, "wd")
	close(held)
	for c := range held {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("run: want the worker's failure")
	}
	if res.Skipped != 0 {
		t.Errorf("skipped = %d, want the cancelled images left out of the count", res.Skipped)
	}
}
