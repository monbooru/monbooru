package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestListTaggers_AConfiguredTaggerWithNoModelDoesNotApply(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Paths.ModelPath = t.TempDir()
	env.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "gone", Enabled: true}}

	w := env.do("GET", "/api/v1/taggers")
	mustCode(t, w, http.StatusOK)
	var got []taggerResponse
	mustDecode(t, w, &got)
	if len(got) != 1 || got[0].Name != "gone" || !got[0].Enabled || got[0].Available || got[0].Applies || got[0].Reason == "" {
		t.Errorf("taggers = %+v, want one enabled, unavailable row with a reason", got)
	}
}

func TestListTaggers_EmptyIsAnEmptyArray(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Paths.ModelPath = t.TempDir()

	w := env.do("GET", "/api/v1/taggers")
	mustCode(t, w, http.StatusOK)
	if body := w.Body.String(); body != "[]\n" && body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestListTaggers_UnknownGalleryIs400(t *testing.T) {
	wantCode(t, newTestEnv(t).do("GET", "/api/v1/taggers?gallery=nope"), http.StatusBadRequest)
}

func TestAutotagImage_NoTaggerIs503(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Paths.ModelPath = t.TempDir()
	id := env.createTestImage(t, "untaggable.png", 10, 10)

	for _, w := range []*httptest.ResponseRecorder{
		doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/autotag", id), nil),
		doJSON(env.mux, "POST", "/api/v1/images/autotag", fmt.Appendf(nil, `{"ids":[%d]}`, id)),
	} {
		mustCode(t, w, http.StatusServiceUnavailable)
		wantIn(t, w.Body.String(), `"code":"tagger_unavailable"`)
	}
	if env.handler.jobs.Get() != nil {
		t.Error("a refused call started a job")
	}
}

func TestAutotagImages_RefusesBadIDsBeforeQueueing(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "real.png", 10, 10)

	for body, want := range map[string]int{
		`{"ids":[]}`: http.StatusBadRequest,
		`{"ids":[` + fmt.Sprint(id) + `,999999]}`: http.StatusNotFound,
		`not json`: http.StatusBadRequest,
	} {
		wantCode(t, doJSON(env.mux, "POST", "/api/v1/images/autotag", []byte(body)), want)
	}
	wantCode(t, doJSON(env.mux, "POST", "/api/v1/images/999999/autotag", nil), http.StatusNotFound)
	wantCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/autotag", id), []byte(`{"tagger_name":`)), http.StatusBadRequest)
}
