//go:build tagger

package tagger

import (
	"context"
	"fmt"
	"image/color"
	"path/filepath"
	"strings"
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

func insertTaggerTestImage(t *testing.T, database *db.DB, sha string) int64 {
	t.Helper()
	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, is_missing)
		 VALUES (?, ?, '', 'png', 0, 0)`,
		sha, "/tmp/"+sha+".png",
	)
	if err != nil {
		t.Fatalf("insert image: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func catIDForName(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("category %q: %v", name, err)
	}
	return id
}

type chunkRecorder struct {
	mgr    *jobs.Manager
	tag    TagKey
	failOn int
	images []int
	msgs   []string

	pageDirs string
	dirs     []int
}

func (b *chunkRecorder) Run(_ context.Context, req RunRequest) (RunResponse, error) {
	b.images = append(b.images, len(req.Images))
	if st := b.mgr.Get(); st != nil {
		b.msgs = append(b.msgs, st.Message)
	}
	if b.pageDirs != "" {
		found, _ := filepath.Glob(filepath.Join(b.pageDirs, "autotag-*"))
		b.dirs = append(b.dirs, len(found))
	}
	if len(b.images) == b.failOn {
		return RunResponse{}, fmt.Errorf("worker gone")
	}
	resp := RunResponse{}
	for _, im := range req.Images {
		resp.Results = append(resp.Results, BackendImageResult{
			ID:   im.ID,
			Tags: map[TagKey]Scored{b.tag: {Score: 0.9, TaggerName: "stub"}},
		})
	}
	return resp, nil
}

func (b *chunkRecorder) Status() CacheStatus              { return CacheStatus{} }
func (b *chunkRecorder) ReleaseIdle(_ time.Duration) bool { return false }
func (b *chunkRecorder) ReleaseAll()                      {}

func runChunkedScope(t *testing.T, n, failOn int) (*db.DB, *chunkRecorder, error) {
	t.Helper()
	database := setupStoreResultsDB(t)
	ids := make([]int64, 0, n)
	for i := range n {
		ids = append(ids, insertTaggerTestImage(t, database, fmt.Sprintf("%064x", i)))
	}

	mgr := jobs.NewManager()
	if err := mgr.Start("autotag"); err != nil {
		t.Fatal(err)
	}
	mgr.Update(0, n, "starting (loading model may take a few seconds)…")

	rec := &chunkRecorder{
		mgr:    mgr,
		tag:    TagKey{Name: "landscape", CatID: catIDForName(t, database, "general")},
		failOn: failOn,
	}
	prev := activeBackend()
	SetBackend(rec)
	t.Cleanup(func() { SetBackend(prev) })

	cfg := &config.Config{}
	cfg.Tagger.Parallel = 1
	taggers := []TaggerStatus{{TaggerInstance: config.TaggerInstance{Name: "stub", Enabled: true}}}
	_, err := RunWithTaggers(context.Background(), gallery.Handle{DB: database, TagSvc: tags.New(database, nil)}, cfg, ids, taggers, mgr, "cpu")
	return database, rec, err
}

func TestRunWithTaggers_ChunksTheScope(t *testing.T) {
	_, rec, err := runChunkedScope(t, autotagChunkSize+1, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []int{autotagChunkSize, 1}
	if fmt.Sprint(rec.images) != fmt.Sprint(want) {
		t.Errorf("backend calls carried %v images, want %v", rec.images, want)
	}
	if len(rec.msgs) == 0 || !strings.HasPrefix(rec.msgs[0], "preparing ") {
		t.Errorf("status at the first call = %q, want a preparing line", rec.msgs)
	}
}

func TestRunWithTaggers_KeepsTaggedChunksWhenOneFails(t *testing.T) {
	database, _, err := runChunkedScope(t, autotagChunkSize+1, 2)
	if err == nil {
		t.Fatal("run: want the backend failure to propagate")
	}
	var tagged int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE auto_tagged_at IS NOT NULL`).Scan(&tagged); err != nil {
		t.Fatal(err)
	}
	if tagged != autotagChunkSize {
		t.Errorf("images tagged before the failure = %d, want %d", tagged, autotagChunkSize)
	}
}

func TestRunWithTaggers_BoundsAChunkByFrames(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	pages := make([][]byte, 150)
	for i := range pages {
		pages[i] = pic
	}
	cbz := makeCBZ(t, dir, "m.cbz", pages)

	for _, tc := range []struct {
		parallel   int
		images     []int
		dirsAtCall []int
	}{
		{1, []int{2, 1}, []int{2, 1}},

		{3, []int{3}, []int{3}},
	} {
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
		mgr := jobs.NewManager()
		if err := mgr.Start("autotag"); err != nil {
			t.Fatal(err)
		}
		thumbs := t.TempDir()
		rec := &chunkRecorder{
			mgr:      mgr,
			tag:      TagKey{Name: "landscape", CatID: catIDForName(t, database, "general")},
			pageDirs: filepath.Dir(gallery.MangaCacheDir(thumbs)),
		}
		prev := activeBackend()
		SetBackend(rec)
		cfg := &config.Config{}
		cfg.Tagger.Parallel = tc.parallel
		taggers := []TaggerStatus{{TaggerInstance: config.TaggerInstance{Name: "stub", Enabled: true}}}
		_, err := RunWithTaggers(context.Background(),
			gallery.Handle{DB: database, TagSvc: tags.New(database, nil), ThumbnailsPath: thumbs},
			cfg, ids, taggers, mgr, "cpu")
		SetBackend(prev)
		if err != nil {
			t.Fatalf("parallel %d: run: %v", tc.parallel, err)
		}
		if fmt.Sprint(rec.images) != fmt.Sprint(tc.images) {
			t.Errorf("parallel %d: backend calls carried %v images, want %v", tc.parallel, rec.images, tc.images)
		}
		if fmt.Sprint(rec.dirs) != fmt.Sprint(tc.dirsAtCall) {
			t.Errorf("parallel %d: extraction folders on disk at each call %v, want %v", tc.parallel, rec.dirs, tc.dirsAtCall)
		}
		if left, _ := filepath.Glob(filepath.Join(rec.pageDirs, "autotag-*")); len(left) != 0 {
			t.Errorf("parallel %d: %d extraction folders left after the run", tc.parallel, len(left))
		}
	}
}
