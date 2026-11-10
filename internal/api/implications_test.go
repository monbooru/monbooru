package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/models"
)

func newTag(t *testing.T, env *testEnv, name string) int64 {
	t.Helper()
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": name}, http.StatusCreated)
	return int64(resp["id"].(float64))
}

func getImplications(t *testing.T, env *testEnv, parentID int64, wantStatus int) []map[string]any {
	t.Helper()
	w := env.do("GET", fmt.Sprintf("/api/v1/tags/%d/implications", parentID))
	mustCode(t, w, wantStatus)
	var out []map[string]any
	if w.Body.Len() > 0 {
		_ = json.NewDecoder(w.Body).Decode(&out)
	}
	return out
}

func TestAddAndListImplications(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	child := newTag(t, env, "animal")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusCreated)

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusOK)

	imps := getImplications(t, env, parent, http.StatusOK)
	if len(imps) != 1 {
		t.Fatalf("expected 1 implication, got %d", len(imps))
	}
	if int64(imps[0]["implied_id"].(float64)) != child {
		t.Errorf("implied_id = %v, want %d", imps[0]["implied_id"], child)
	}
	if imps[0]["implied_name"] != "animal" {
		t.Errorf("implied_name = %v, want animal", imps[0]["implied_name"])
	}
}

func TestAddImplication_MissingImplied(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{}, http.StatusBadRequest)
}

func TestAddImplication_Self(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": parent}, http.StatusBadRequest)
}

func TestAddImplication_Cycle(t *testing.T) {
	env := newTestEnv(t)
	a := newTag(t, env, "cat")
	b := newTag(t, env, "animal")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", a),
		map[string]any{"implied_id": b}, http.StatusCreated)

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", b),
		map[string]any{"implied_id": a}, http.StatusConflict)
}

func TestListImplications_ParentNotFound(t *testing.T) {
	env := newTestEnv(t)
	getImplications(t, env, 99999, http.StatusNotFound)
}

func TestRemoveImplication(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	child := newTag(t, env, "animal")
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusCreated)

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/%d", parent, child), nil, http.StatusNoContent)

	if imps := getImplications(t, env, parent, http.StatusOK); len(imps) != 0 {
		t.Errorf("expected 0 implications after delete, got %d", len(imps))
	}
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/%d", parent, child), nil, http.StatusNotFound)
}

func imageTagNames(t *testing.T, env *testEnv, id int64) map[string]bool {
	t.Helper()
	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/tags", id))
	mustCode(t, w, http.StatusOK)
	var list []struct {
		Name string `json:"name"`
	}
	mustDecode(t, w, &list)
	out := map[string]bool{}
	for _, tag := range list {
		out[tag.Name] = true
	}
	return out
}

func awaitIdle(t *testing.T, env *testEnv) {
	t.Helper()
	if !env.handler.jobs.WaitIdle(5 * time.Second) {
		t.Fatal("the job never finished")
	}
}

func TestImplication_PropagateAppliesAndSweepsExistingImages(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tabby.png", 10, 10)
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), []byte(`{"tags":["cat"]}`)), http.StatusOK)
	parent, child := newTag(t, env, "cat"), newTag(t, env, "animal")

	resp := apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications?propagate=true", parent),
		map[string]any{"implied_id": child}, http.StatusAccepted)
	if resp["created"] != true {
		t.Errorf("declare = %v, want created", resp)
	}
	awaitIdle(t, env)
	if !imageTagNames(t, env, id)["animal"] {
		t.Fatal("the image already tagged cat did not gain animal")
	}
	if st := env.handler.jobs.Get(); st == nil || st.Summary != "Implication applied to 1 image." {
		t.Errorf("job = %+v, want the fan-out summary", st)
	}

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/%d?propagate=true", parent, child), nil, http.StatusAccepted)
	awaitIdle(t, env)
	if got := imageTagNames(t, env, id); got["animal"] || !got["cat"] {
		t.Errorf("tags after the sweep = %v, want cat without animal", got)
	}
}

func TestImplication_WithoutPropagateLeavesExistingImages(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tabby.png", 10, 10)
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), []byte(`{"tags":["cat"]}`)), http.StatusOK)
	parent, child := newTag(t, env, "cat"), newTag(t, env, "animal")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusCreated)
	if imageTagNames(t, env, id)["animal"] {
		t.Error("a declare without propagate touched an existing image")
	}
	if env.handler.jobs.Get() != nil {
		t.Error("a declare without propagate started a job")
	}
}

func TestImplication_PropagateBehindARunningJobWritesNothing(t *testing.T) {
	env := newTestEnv(t)
	parent, child := newTag(t, env, "cat"), newTag(t, env, "animal")
	if err := env.handler.jobs.Start(models.JobTypeSync); err != nil {
		t.Fatal(err)
	}

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications?propagate=true", parent),
		map[string]any{"implied_id": child}, http.StatusConflict)
	if imps := getImplications(t, env, parent, http.StatusOK); len(imps) != 0 {
		t.Errorf("implications = %v, want none written", imps)
	}
	env.handler.jobs.Complete("synced")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusCreated)
	if err := env.handler.jobs.Start(models.JobTypeSync); err != nil {
		t.Fatal(err)
	}
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/%d?propagate=true", parent, child), nil, http.StatusConflict)
	if imps := getImplications(t, env, parent, http.StatusOK); len(imps) != 1 {
		t.Errorf("implications = %v, want the edge kept", imps)
	}
}

func TestImplication_RefusedPropagateLeavesNoJob(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications?propagate=true", parent),
		map[string]any{"implied_id": parent}, http.StatusBadRequest)
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/99999?propagate=true", parent), nil, http.StatusNotFound)
	if st := env.handler.jobs.Get(); st != nil || env.handler.jobs.IsRunning() {
		t.Errorf("job = %+v, want the slot free and nothing shown", st)
	}
}

func TestImplication_RefusedPropagateKeepsThePreviousJobState(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	m := env.handler.jobs
	if err := m.Start(models.JobTypeTag); err != nil {
		t.Fatal(err)
	}
	m.Fail("boom")
	finished := m.Finished()

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications?propagate=true", parent),
		map[string]any{"implied_id": parent}, http.StatusBadRequest)
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/99999?propagate=true", parent), nil, http.StatusNotFound)
	if st := m.Get(); st == nil || st.Error != "boom" {
		t.Errorf("job = %+v, want the earlier failure still shown", st)
	}
	if got := m.Finished(); got != finished {
		t.Errorf("Finished() = %d, want %d: a refused edge ended a job", got, finished)
	}
	if m.IsRunning() {
		t.Error("a refused edge left a job running")
	}
}
