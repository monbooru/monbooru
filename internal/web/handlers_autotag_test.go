//go:build tagger

package web

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tagger"
)

type fakeBackend struct {
	tags      map[tagger.TagKey]tagger.Scored
	runs      int
	providers []string
}

func (b *fakeBackend) Run(_ context.Context, req tagger.RunRequest) (tagger.RunResponse, error) {
	b.runs++
	b.providers = append(b.providers, req.Provider)
	resp := tagger.RunResponse{}
	for _, img := range req.Images {
		resp.Results = append(resp.Results, tagger.BackendImageResult{ID: img.ID, Tags: b.tags})
	}
	return resp, nil
}

func (b *fakeBackend) Status() tagger.CacheStatus       { return tagger.CacheStatus{} }
func (b *fakeBackend) ReleaseIdle(_ time.Duration) bool { return false }
func (b *fakeBackend) ReleaseAll()                      {}

func enableDummyTagger(t *testing.T, srv *Server, labels ...string) *fakeBackend {
	t.Helper()
	stageDummyTagger(t, srv)
	srv.cfgMu.Lock()
	srv.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "wd14", Enabled: true, ConfidenceThreshold: 0.1}}
	srv.cfgMu.Unlock()

	generalID := srv.active().GeneralCategoryID
	scored := map[tagger.TagKey]tagger.Scored{}
	for _, l := range labels {
		scored[tagger.TagKey{Name: l, CatID: generalID}] = tagger.Scored{Score: 0.9, TaggerName: "wd14"}
	}
	b := &fakeBackend{tags: scored}
	tagger.SetBackend(b)
	return b
}

type blockingBackend struct {
	fakeBackend
	mu      sync.Mutex
	started chan struct{}
}

func (b *blockingBackend) Run(ctx context.Context, _ tagger.RunRequest) (tagger.RunResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.started)
	<-ctx.Done()
	return tagger.RunResponse{}, ctx.Err()
}

func (b *blockingBackend) ReleaseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
}

func TestServerClose_CancelsTheRunningJob(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv)
	b := &blockingBackend{started: make(chan struct{})}
	tagger.SetBackend(b)
	t.Cleanup(func() { tagger.SetBackend(&fakeBackend{}) })
	id := seedImage(t, srv, "slow.png", 32, 32)

	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/autotag", url.Values{})
	mustCode(t, w, http.StatusAccepted)
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tagger never ran")
	}

	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited on the running job")
	}
	awaitJobsDrain(t, srv)
}

func autoTagsOf(t *testing.T, srv *Server, id int64) []string {
	t.Helper()
	out, err := db.QueryStrings(srv.db().Read,
		`SELECT t.name FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND it.is_auto = 1 ORDER BY t.name`, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDetailAutotagDialog_HasAnErrorSlotAndClosesOnlyOnAStart(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv)
	id := seedImage(t, srv, "dialog.png", 32, 32)

	w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	start := strings.Index(body, `<dialog id="autotag-dialog">`)
	if start < 0 {
		t.Fatal("no auto-tag dialog rendered")
	}
	dialog := body[start : start+strings.Index(body[start:], "</dialog>")]
	wantIn(t, dialog, `hx-target="#autotag-error"`)
	wantIn(t, dialog, `<div id="autotag-error"></div>`)
	wantIn(t, dialog, `if(event.detail.xhr.status === 202)`)
	wantNotIn(t, dialog, `hx-swap="none"`)
}

func TestAutotagImage_WritesTheInferredTags(t *testing.T) {
	srv := newTestServer(t)
	b := enableDummyTagger(t, srv, "blue_eyes", "smile")
	id := seedImage(t, srv, "subject.png", 32, 32)
	other := seedImage(t, srv, "bystander.png", 24, 24)

	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/autotag", url.Values{})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if got := strings.Join(autoTagsOf(t, srv, id), ","); got != "blue_eyes,smile" {
		t.Errorf("auto tags = %q, want blue_eyes,smile", got)
	}
	if got := autoTagsOf(t, srv, other); len(got) != 0 {
		t.Errorf("the run reached an image outside its scope: %v", got)
	}
	if b.runs != 1 {
		t.Errorf("backend ran %d times, want 1", b.runs)
	}
}

func TestAutotagImage_RunsOnTheConfiguredProvider(t *testing.T) {
	srv := newTestServer(t)
	b := enableDummyTagger(t, srv, "smile")
	srv.cfgMu.Lock()
	srv.cfg.Tagger.ExecutionProvider = "cuda"
	srv.cfgMu.Unlock()
	id := seedImage(t, srv, "warm.png", 32, 32)

	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/autotag", url.Values{})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	if got := strings.Join(b.providers, ","); got != "cuda" {
		t.Errorf("the image ran on %q, want the configured cuda", got)
	}
}

type skipBackend struct{ fakeBackend }

func (skipBackend) Run(_ context.Context, req tagger.RunRequest) (tagger.RunResponse, error) {
	resp := tagger.RunResponse{}
	for _, img := range req.Images {
		resp.Results = append(resp.Results, tagger.BackendImageResult{ID: img.ID, Err: "unreadable"})
	}
	return resp, nil
}

func TestAutotagImage_ReportsASkippedImage(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv)
	tagger.SetBackend(&skipBackend{})
	id := seedImage(t, srv, "skipped.png", 32, 32)

	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/autotag", url.Values{})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	st := srv.jobs.Get()
	if want := "auto-tagger skipped image #" + strconv.FormatInt(id, 10); st == nil || st.Summary != want {
		t.Errorf("job = %+v, want it completed with %q", st, want)
	}
}

func TestAutotagTrigger_TagsTheSelectionAndNothingElse(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv, "landscape")
	first := seedImage(t, srv, "one.png", 32, 32)
	second := seedImage(t, srv, "two.png", 33, 33)
	untouched := seedImage(t, srv, "three.png", 34, 34)

	w := postHTMXForm(t, srv, "/internal/autotag", url.Values{
		"scope": {"selection"},
		"ids":   {strconv.FormatInt(first, 10), strconv.FormatInt(second, 10)},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	for _, id := range []int64{first, second} {
		if got := autoTagsOf(t, srv, id); len(got) != 1 || got[0] != "landscape" {
			t.Errorf("image %d auto tags = %v, want [landscape]", id, got)
		}
	}
	if got := autoTagsOf(t, srv, untouched); len(got) != 0 {
		t.Errorf("an unselected image was tagged: %v", got)
	}
}

func TestAutotagImage_FansOutImplications(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv, "cat")
	svc := srv.tagSvc()
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, svc, "cat", generalID)
	implied := mustTag(t, svc, "animal", generalID)
	mustAddImply(t, svc, parent.ID, implied.ID)
	id := seedImage(t, srv, "cat.png", 32, 32)

	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/autotag", url.Values{})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	implCount := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", id, implied.ID)
	if implCount != 1 {
		t.Errorf("implied rows = %d, want the tagger's parent to have fanned out", implCount)
	}
}

func TestAutotagTrigger_AnUnknownScopeIsRefused(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv, "landscape")
	id := seedImage(t, srv, "one.png", 32, 32)

	w := postHTMXForm(t, srv, "/internal/autotag", url.Values{"scope": {"bogus"}, "ids": {strconv.FormatInt(id, 10)}})

	wantCode(t, w, http.StatusBadRequest)
	awaitJobsDrain(t, srv)
	if got := autoTagsOf(t, srv, id); len(got) != 0 {
		t.Errorf("an unknown scope tagged the image: %v", got)
	}
}

func TestAutotagTrigger_RefusalsAnswerTheDialogWithAFlash(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv, "landscape")
	id := strconv.FormatInt(seedImage(t, srv, "one.png", 32, 32), 10)

	for _, c := range []struct {
		name string
		form url.Values
		busy bool
		code int
		msg  string
	}{
		{"unknown tagger", url.Values{"scope": {"selection"}, "ids": {id}, "tagger_name": {"nope"}}, false, http.StatusBadRequest, `tagger &#34;nope&#34; is not enabled`},
		{"no images", url.Values{"scope": {"selection"}}, false, http.StatusBadRequest, "No images to tag."},
		{"job running", url.Values{"scope": {"selection"}, "ids": {id}}, true, http.StatusConflict, "A job is already running."},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.busy {
				mustStartJob(t, srv.jobs, "sync")
				defer srv.jobs.Complete("")
			}
			w := postForm(t, srv, "/internal/autotag", c.form)
			wantCode(t, w, c.code)
			wantIn(t, w.Body.String(), `<div class="flash flash-err">`)
			wantIn(t, w.Body.String(), c.msg)
		})
	}
}

func TestDetailAutotagControlFollowsTheTaggers(t *testing.T) {
	for _, c := range []struct {
		name   string
		tagger *config.TaggerInstance
		noLib  bool
		want   string
	}{
		{"no tagger set up", nil, false, ""},
		{"enabled", &config.TaggerInstance{Name: "wd14", Enabled: true}, false, `title="Auto-tag this image (t)"`},
		{"disabled", &config.TaggerInstance{Name: "wd14"}, false, `disabled title="no enabled tagger"`},
		{"enabled for another gallery", &config.TaggerInstance{Name: "wd14", Enabled: true, Galleries: []string{"other"}}, false, `disabled title="no enabled tagger applies to gallery &#34;default&#34;"`},
		{"runtime missing", &config.TaggerInstance{Name: "wd14", Enabled: true}, true, `disabled title="missing absent.so at `},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t)
			id := seedImage(t, srv, "one.png", 32, 32)
			srv.cfgMu.Lock()
			srv.cfg.Paths.ModelPath = filepath.Join(t.TempDir(), "models")
			srv.cfgMu.Unlock()
			if c.tagger != nil {
				stageDummyTagger(t, srv)
				srv.cfgMu.Lock()
				srv.cfg.Tagger.Taggers = []config.TaggerInstance{*c.tagger}
				srv.cfgMu.Unlock()
			}
			if c.noLib {
				t.Setenv("ORT_LIB_PATH", filepath.Join(t.TempDir(), "absent.so"))
			}

			body := mustGetBody(t, srv, "/images/"+strconv.FormatInt(id, 10))
			if c.want == "" {
				if strings.Contains(body, `class="btn-autotag"`) {
					t.Error("the Auto-tag button shows with no tagger set up")
				}
				return
			}
			i := strings.Index(body, `class="btn-autotag"`)
			if i < 0 {
				t.Fatal("no Auto-tag button")
			}
			if ctl := body[i:min(len(body), i+300)]; !strings.Contains(ctl, c.want) {
				t.Errorf("Auto-tag button = %q, want %q", ctl, c.want)
			}
		})
	}
}
