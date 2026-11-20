package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tagger/taggertest"
)

func enableStubTagger(t *testing.T, env *testEnv, others ...string) *taggertest.Worker {
	t.Helper()
	env.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "wd14", Enabled: true, ConfidenceThreshold: 0.1}}
	held := map[string][]string{"wd14": {"sample"}}
	for _, o := range others {
		held[o] = []string{"sample"}
	}
	return taggertest.Use(t, held)
}

func TestListTaggers_AppliesFollowsTheGalleryScope(t *testing.T) {
	env := newTestEnv(t)
	enableStubTagger(t, env, "elsewhere")
	elsewhere := env.cfg.Tagger.Taggers[0]
	elsewhere.Name, elsewhere.Galleries = "elsewhere", []string{"other"}
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
	enableStubTagger(t, env).Refuse("unreadable")

	id, _ := uploadForAutotag(t, env, "skip-me.png", 15)
	awaitIdle(t, env)

	st := env.handler.jobs.Get()
	if want := fmt.Sprintf("auto-tagger skipped image #%d", id); st == nil || st.Running || st.Summary != want || st.JobType != models.JobTypeAutotag {
		t.Errorf("job = %+v, want an autotag job completed with %q", st, want)
	}
}

func TestCreateImage_AutotagLogsItsPeakMemory(t *testing.T) {
	env := newTestEnv(t)
	enableStubTagger(t, env).Refuse("unreadable")
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
	started := enableStubTagger(t, env).Block()

	uploadForAutotag(t, env, "tag-me.png", 15)
	select {
	case <-started:
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
	b := enableStubTagger(t, env)
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

	if got := b.Calls(); got != 2 {
		t.Errorf("backend ran %d times, want 2", got)
	}
	for _, id := range []int64{first, second} {
		if n := autotagged(t, env, id); n != 1 {
			t.Errorf("image #%d carries %d auto 'sample' tags, want 1", id, n)
		}
	}
}

func TestCreateImage_AutotagCancelDropsTheQueuedUploads(t *testing.T) {
	env := newTestEnv(t)
	b := enableStubTagger(t, env)
	started := b.Block()

	if _, note := uploadForAutotag(t, env, "first.png", 15); note != "autotag job started" {
		t.Fatalf("first note = %q, want started", note)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the auto-tagger never ran")
	}
	if _, note := uploadForAutotag(t, env, "second.png", 16); note != "autotag queued" {
		t.Fatalf("second note = %q, want queued", note)
	}
	env.handler.jobs.Cancel()
	awaitQueueDrained(t, env)

	if got := b.Calls(); got != 1 {
		t.Errorf("backend ran %d times, want the queued upload dropped", got)
	}
	if st := env.handler.jobs.Get(); st == nil || st.Running || st.Summary != "auto-tagging cancelled" {
		t.Errorf("job = %+v, want it completed as cancelled", st)
	}
}

func TestCreateImage_AutotagQueueStartsNothingOnceDoneCloses(t *testing.T) {
	env := newTestEnv(t)
	b := enableStubTagger(t, env)
	b.Block()
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

	if got := b.Calls(); got != 0 {
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
	enableStubTagger(t, env)
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
	b := enableStubTagger(t, env)
	first := env.createTestImage(t, "first.png", 15, 15)
	second := env.createTestImage(t, "second.png", 16, 16)

	body := fmt.Appendf(nil, `{"ids":[%d,%d,%d],"tagger_name":"wd14"}`, first, second, first)
	w := doJSON(env.mux, "POST", "/api/v1/images/autotag", body)
	mustCode(t, w, http.StatusAccepted)
	wantIn(t, w.Body.String(), `"images":2`)
	awaitQueueDrained(t, env)

	if got := b.Calls(); got != 2 {
		t.Errorf("backend ran %d times, want 2", got)
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
	enableStubTagger(t, env).Refuse("unreadable")
	id := env.createTestImage(t, "named.png", 15, 15)

	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/autotag", id), []byte(`{"tagger_name":"nope"}`))
	mustCode(t, w, http.StatusBadRequest)
	if env.handler.jobs.Get() != nil {
		t.Error("a refused call started a job")
	}
}
