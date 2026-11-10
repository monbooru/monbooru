package api

import (
	"net/http"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func currentJobOf(t *testing.T, env *testEnv) currentJobResponse {
	t.Helper()
	w := env.do("GET", "/api/v1/jobs/current")
	mustCode(t, w, http.StatusOK)
	var got currentJobResponse
	mustDecode(t, w, &got)
	return got
}

func TestCurrentJob_FollowsAJobFromStartToFinish(t *testing.T) {
	env := newTestEnv(t)
	if got := currentJobOf(t, env); got.Running || got.Finished != 0 || got.Queued != 0 || got.Job != nil {
		t.Fatalf("idle = %+v, want nothing running and no job", got)
	}

	if err := env.handler.jobs.Start(models.JobTypeTag, "default"); err != nil {
		t.Fatal(err)
	}
	env.handler.jobs.Update(3, 10, "applying implication…")
	got := currentJobOf(t, env)
	if !got.Running || got.Job == nil || got.Job.Type != models.JobTypeTag || got.Job.Processed != 3 || got.Job.Total != 10 ||
		got.Job.Message != "applying implication…" || got.Job.FinishedAt != nil || len(got.Job.Galleries) != 1 {
		t.Fatalf("running = %+v (job %+v), want the tag job at 3/10", got, got.Job)
	}

	env.handler.jobs.Complete("Implication applied to 10 images.")
	got = currentJobOf(t, env)
	if got.Running || got.Finished != 1 || got.Job == nil || got.Job.Summary != "Implication applied to 10 images." || got.Job.FinishedAt == nil {
		t.Errorf("finished = %+v (job %+v), want the completed job counted", got, got.Job)
	}
}

func TestCurrentJob_CountsTheAutotagQueue(t *testing.T) {
	env := newTestEnv(t)
	env.handler.autotagMu.Lock()
	env.handler.autotagQueue = []queuedAutotag{{"default", "", 1}, {"default", "", 2}}
	env.handler.autotagMu.Unlock()

	if got := currentJobOf(t, env); got.Queued != 2 {
		t.Errorf("queued = %d, want 2", got.Queued)
	}
}
