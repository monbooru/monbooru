//go:build tagger

package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tagger"
)

type blockingBackend struct{ started chan struct{} }

func (b *blockingBackend) Run(ctx context.Context, _ tagger.RunRequest) (tagger.RunResponse, error) {
	close(b.started)
	<-ctx.Done()
	return tagger.RunResponse{}, ctx.Err()
}

func (b *blockingBackend) Status() tagger.CacheStatus       { return tagger.CacheStatus{} }
func (b *blockingBackend) ReleaseIdle(_ time.Duration) bool { return false }
func (b *blockingBackend) ReleaseAll()                      {}

type skipBackend struct{}

func (skipBackend) Run(_ context.Context, req tagger.RunRequest) (tagger.RunResponse, error) {
	resp := tagger.RunResponse{}
	for _, img := range req.Images {
		resp.Results = append(resp.Results, tagger.BackendImageResult{ID: img.ID, Err: "unreadable"})
	}
	return resp, nil
}

func (skipBackend) Status() tagger.CacheStatus       { return tagger.CacheStatus{} }
func (skipBackend) ReleaseIdle(_ time.Duration) bool { return false }
func (skipBackend) ReleaseAll()                      {}

func enableStubTagger(t *testing.T, env *testEnv, b tagger.Backend) {
	t.Helper()
	modelRoot := filepath.Join(t.TempDir(), "models")
	mustMkdirAll(t, filepath.Join(modelRoot, "wd14"), 0o755)
	mustWriteFile(t, filepath.Join(modelRoot, "wd14", "model.onnx"), []byte("dummy"), 0o644)
	mustWriteFile(t, filepath.Join(modelRoot, "wd14", "tags.csv"), []byte("tag_id,name,category\n0,sample,0\n"), 0o644)

	lib := filepath.Join(modelRoot, "libonnxruntime.so")
	mustWriteFile(t, lib, nil, 0o644)
	t.Setenv("ORT_LIB_PATH", lib)
	env.cfg.Paths.ModelPath = modelRoot
	env.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "wd14", Enabled: true, ConfidenceThreshold: 0.1}}
	tagger.SetBackend(b)
	t.Cleanup(func() { tagger.SetBackend(nil) })
}

func TestListTaggers_AppliesFollowsTheGalleryScope(t *testing.T) {
	env := newTestEnv(t)
	enableStubTagger(t, env, skipBackend{})
	elsewhere := env.cfg.Tagger.Taggers[0]
	elsewhere.Name, elsewhere.Galleries = "elsewhere", []string{"other"}
	dir := filepath.Join(env.cfg.Paths.ModelPath, "elsewhere")
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "model.onnx"), []byte("dummy"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "tags.csv"), []byte("tag_id,name,category\n0,sample,0\n"), 0o644)
	env.cfg.Tagger.Taggers = append(env.cfg.Tagger.Taggers, elsewhere)

	w := env.do("GET", "/api/v1/taggers")
	mustCode(t, w, http.StatusOK)
	var got []taggerResponse
	mustDecode(t, w, &got)
	applies := map[string]bool{}
	for _, row := range got {
		if !row.Available {
			t.Errorf("%s unavailable: %s", row.Name, row.Reason)
		}
		applies[row.Name] = row.Applies
	}
	if !applies["wd14"] || applies["elsewhere"] {
		t.Errorf("applies = %v, want wd14 only", applies)
	}
}

func TestCreateImage_AutotagReportsASkippedImage(t *testing.T) {
	env := newTestEnv(t)
	enableStubTagger(t, env, skipBackend{})

	id, _ := uploadForAutotag(t, env, "skip-me.png", 15)
	awaitIdle(t, env)

	st := env.handler.jobs.Get()
	if want := fmt.Sprintf("auto-tagger skipped image #%d", id); st == nil || st.Running || st.Summary != want || st.JobType != models.JobTypeAutotag {
		t.Errorf("job = %+v, want an autotag job completed with %q", st, want)
	}
}

func TestCreateImage_AutotagLogsItsPeakMemory(t *testing.T) {
	env := newTestEnv(t)
	enableStubTagger(t, env, skipBackend{})
	scopes := make(chan string, 1)
	env.handler.WithMemoryMark(func() func(string) {
		return func(scope string) { scopes <- scope }
	})

	id, _ := uploadForAutotag(t, env, "peak.png", 15)
	select {
	case got := <-scopes:
		if want := fmt.Sprintf("image #%d", id); got != want {
			t.Errorf("peak logged for %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the finished job logged no peak")
	}
}

func TestCreateImage_AutotagCancelReportsCancelled(t *testing.T) {
	env := newTestEnv(t)
	b := &blockingBackend{started: make(chan struct{})}
	enableStubTagger(t, env, b)

	uploadForAutotag(t, env, "tag-me.png", 15)
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the auto-tagger never ran")
	}
	env.handler.jobs.Cancel()
	awaitIdle(t, env)
	st := env.handler.jobs.Get()
	if st == nil || st.Running || st.Error != "" || st.Summary != "auto-tagging cancelled" {
		t.Errorf("job = %+v, want it completed as cancelled", st)
	}
}

type countingBackend struct {
	runs atomic.Int32
	tags map[tagger.TagKey]tagger.Scored
}

func (b *countingBackend) Run(ctx context.Context, req tagger.RunRequest) (tagger.RunResponse, error) {
	b.runs.Add(1)
	if b.tags == nil {
		<-ctx.Done()
		return tagger.RunResponse{}, ctx.Err()
	}
	resp := tagger.RunResponse{}
	for _, img := range req.Images {
		resp.Results = append(resp.Results, tagger.BackendImageResult{ID: img.ID, Tags: b.tags})
	}
	return resp, nil
}

func (*countingBackend) Status() tagger.CacheStatus       { return tagger.CacheStatus{} }
func (*countingBackend) ReleaseIdle(_ time.Duration) bool { return false }
func (*countingBackend) ReleaseAll()                      {}

func sampleBackend(env *testEnv) *countingBackend {
	return &countingBackend{tags: map[tagger.TagKey]tagger.Scored{
		{Name: "sample", CatID: env.gallery.GeneralCategoryID}: {Score: 0.9, TaggerName: "wd14"},
	}}
}

func uploadForAutotag(t *testing.T, env *testEnv, name string, size int) (id int64, note string) {
	t.Helper()
	w := env.postUpload(t, name, pngFileBytes(t, size, size), map[string]string{"autotag": "true"})
	mustCode(t, w, http.StatusCreated)
	var created struct {
		Image struct {
			ID int64 `json:"id"`
		} `json:"image"`
		Autotag string `json:"autotag"`
	}
	mustDecode(t, w, &created)
	return created.Image.ID, created.Autotag
}

func awaitQueueDrained(t *testing.T, env *testEnv) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env.handler.autotagMu.Lock()
		draining := env.handler.autotagDraining
		env.handler.autotagMu.Unlock()
		if !draining {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the autotag queue never drained")
}

func TestCreateImage_AutotagBehindARunningJobIsQueuedAndRunsOnceTheSlotFrees(t *testing.T) {
	env := newTestEnv(t)
	b := sampleBackend(env)
	enableStubTagger(t, env, b)
	if err := env.handler.jobs.Start(models.JobTypeSync); err != nil {
		t.Fatal(err)
	}

	first, note1 := uploadForAutotag(t, env, "first.png", 15)
	second, note2 := uploadForAutotag(t, env, "second.png", 16)
	if note1 != "autotag queued" || note2 != "autotag queued" {
		t.Fatalf("notes = %q, %q, want both queued", note1, note2)
	}
	env.handler.jobs.Complete("synced")
	awaitQueueDrained(t, env)

	st := env.handler.jobs.Get()
	if st == nil || st.JobType != models.JobTypeAutotag || st.Summary != "auto-tagged 2 images" {
		t.Errorf("job = %+v, want one autotag job over both uploads", st)
	}
	if got := b.runs.Load(); got != 1 {
		t.Errorf("backend ran %d times, want 1", got)
	}
	for _, id := range []int64{first, second} {
		if n := autotagged(t, env, id); n != 1 {
			t.Errorf("image #%d carries %d auto 'sample' tags, want 1", id, n)
		}
	}
}

func TestCreateImage_AutotagCancelDropsTheQueuedUploads(t *testing.T) {
	env := newTestEnv(t)
	b := &countingBackend{}
	enableStubTagger(t, env, b)

	if _, note := uploadForAutotag(t, env, "first.png", 15); note != "autotag job started" {
		t.Fatalf("first note = %q, want started", note)
	}
	if _, note := uploadForAutotag(t, env, "second.png", 16); note != "autotag queued" {
		t.Fatalf("second note = %q, want queued", note)
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	env.handler.jobs.Cancel()
	awaitQueueDrained(t, env)

	if got := b.runs.Load(); got != 1 {
		t.Errorf("backend ran %d times, want the queued upload dropped", got)
	}
	if st := env.handler.jobs.Get(); st == nil || st.Running || st.Summary != "auto-tagging cancelled" {
		t.Errorf("job = %+v, want it completed as cancelled", st)
	}
}

func TestCreateImage_AutotagQueueStartsNothingOnceDoneCloses(t *testing.T) {
	env := newTestEnv(t)
	b := &countingBackend{}
	enableStubTagger(t, env, b)
	done := make(chan struct{})
	env.handler.WithDone(done)
	if err := env.handler.jobs.Start(models.JobTypeSync); err != nil {
		t.Fatal(err)
	}

	if _, note := uploadForAutotag(t, env, "queued.png", 15); note != "autotag queued" {
		t.Fatalf("note = %q, want queued", note)
	}
	close(done)
	awaitQueueDrained(t, env)
	env.handler.jobs.Complete("synced")

	if got := b.runs.Load(); got != 0 {
		t.Errorf("backend ran %d times after done closed, want 0", got)
	}
	if env.handler.jobs.IsRunning() {
		t.Error("a job holds the slot after done closed")
	}
}

func autotagged(t *testing.T, env *testEnv, id int64) int {
	t.Helper()
	return countRows(t, env.database, `image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.name = 'sample' AND it.is_auto = 1`, id)
}

func TestAutotagImage_TagsAnExistingImage(t *testing.T) {
	env := newTestEnv(t)
	b := sampleBackend(env)
	enableStubTagger(t, env, b)
	id := env.createTestImage(t, "later.png", 15, 15)

	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/autotag", id), nil)
	mustCode(t, w, http.StatusAccepted)
	wantIn(t, w.Body.String(), `"autotag":"autotag job started"`)
	awaitQueueDrained(t, env)
	if n := autotagged(t, env, id); n != 1 {
		t.Errorf("image carries %d auto 'sample' tags, want 1", n)
	}
}

func TestAutotagImages_RunsTheIDsAsOneJob(t *testing.T) {
	env := newTestEnv(t)
	b := sampleBackend(env)
	enableStubTagger(t, env, b)
	first := env.createTestImage(t, "first.png", 15, 15)
	second := env.createTestImage(t, "second.png", 16, 16)

	body := fmt.Appendf(nil, `{"ids":[%d,%d,%d],"tagger_name":"wd14"}`, first, second, first)
	w := doJSON(env.mux, "POST", "/api/v1/images/autotag", body)
	mustCode(t, w, http.StatusAccepted)
	wantIn(t, w.Body.String(), `"images":2`)
	awaitQueueDrained(t, env)

	if got := b.runs.Load(); got != 1 {
		t.Errorf("backend ran %d times, want 1", got)
	}
	if st := env.handler.jobs.Get(); st == nil || st.Summary != "auto-tagged 2 images" {
		t.Errorf("job = %+v, want one autotag job over both images", st)
	}
	for _, id := range []int64{first, second} {
		if n := autotagged(t, env, id); n != 1 {
			t.Errorf("image #%d carries %d auto 'sample' tags, want 1", id, n)
		}
	}
}

func TestAutotagImages_RefusesWhatNoTaggerCanRun(t *testing.T) {
	env := newTestEnv(t)
	enableStubTagger(t, env, skipBackend{})
	id := env.createTestImage(t, "named.png", 15, 15)

	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/autotag", id), []byte(`{"tagger_name":"nope"}`))
	mustCode(t, w, http.StatusBadRequest)
	if env.handler.jobs.Get() != nil {
		t.Error("a refused call started a job")
	}
}
