package jobs

import (
	"slices"
	"sync"
	"testing"
)

func TestStart_ReturnsErrorIfRunning(t *testing.T) {
	m := NewManager()

	mustStart(t, m, "sync")
	if err := m.Start("autotag"); err != ErrJobRunning {
		t.Errorf("expected ErrJobRunning, got %v", err)
	}
}

func TestManager_RecordsTheJobsGalleries(t *testing.T) {
	m := NewManager()
	if err := m.Start("transfer", "main", "archive"); err != nil {
		t.Fatal(err)
	}
	if got := m.Get().Galleries; !slices.Equal(got, []string{"main", "archive"}) {
		t.Errorf("Galleries = %v, want [main archive]", got)
	}
	m.Complete("done")
	r, err := m.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	if err := r.Start("sync", "archive"); err != nil {
		t.Fatal(err)
	}
	if got := m.Get().Galleries; !slices.Equal(got, []string{"archive"}) {
		t.Errorf("scheduled Galleries = %v, want [archive]", got)
	}
}

func TestUpdate_SetsFields(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Update(5, 10, "processing…")

	state := m.Get()
	if state.Processed != 5 {
		t.Errorf("Processed = %d, want 5", state.Processed)
	}
	if state.Total != 10 {
		t.Errorf("Total = %d, want 10", state.Total)
	}
	if state.Message != "processing…" {
		t.Errorf("Message = %q", state.Message)
	}
}

func TestComplete_SetsFinishedAt(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Complete("done: 5 added")

	state := m.Get()
	if state.Running {
		t.Error("Running should be false after Complete")
	}
	if state.FinishedAt == nil {
		t.Error("FinishedAt should be set")
	}
	if state.Summary != "done: 5 added" {
		t.Errorf("Summary = %q", state.Summary)
	}
}

func TestConcurrentUpdate_NoRace(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "autotag")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Go(func() {
			m.Update(i, 100, "working")
		})
	}
	wg.Wait()
	if got := m.Get(); got.Total != 100 || got.Processed < 0 || got.Processed > 99 {
		t.Errorf("state after the burst = %d/%d, want a processed in [0,99] of 100", got.Processed, got.Total)
	}
	m.Complete("done")
	if got := m.Get(); got.Running || got.Summary != "done" {
		t.Errorf("state after Complete = running=%v summary=%q", got.Running, got.Summary)
	}
}

func TestFail_SetsError(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Fail("something went wrong")

	state := m.Get()
	if state.Running {
		t.Error("Running should be false after Fail")
	}
	if state.Error != "something went wrong" {
		t.Errorf("Error = %q", state.Error)
	}
}

func TestDismiss_ClearsState(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Complete("done")
	m.Dismiss()

	if m.Get() != nil {
		t.Error("state should be nil after Dismiss")
	}
}

func TestDismiss_NopWhenRunning(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Dismiss()

	state := m.Get()
	if state == nil || !state.Running {
		t.Error("Dismiss should not clear a running job")
	}
}

func TestSetWatcherMessage(t *testing.T) {
	m := NewManager()
	m.SetWatcherMessage("added file.png")

	state := m.Get()
	if state == nil {
		t.Fatal("state should not be nil")
	}
	if state.JobType != "watcher" {
		t.Errorf("JobType = %q, want watcher", state.JobType)
	}
	if state.Summary != "added file.png" {
		t.Errorf("Summary = %q", state.Summary)
	}
}

func TestCancel_FiresContext(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "autotag")
	ctx := m.Context()
	if ctx.Err() != nil {
		t.Fatal("context should be live right after Start")
	}
	m.Cancel()
	if ctx.Err() == nil {
		t.Error("context should be cancelled after Cancel")
	}
}

func TestSetWatcherMessage_NopWhenRunning(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.SetWatcherMessage("should be ignored")

	state := m.Get()
	if state.Summary == "should be ignored" {
		t.Error("watcher message should not override running job")
	}
}

func TestSetWatcherMessage_KeepsAFailedJobsError(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Fail("boom")
	m.SetWatcherMessage("added a.png")

	state := m.Get()
	if state == nil || state.Error != "boom" || state.Summary == "added a.png" {
		t.Errorf("state = %+v, want the failure kept over the watcher message", state)
	}
}

func TestSetWatcherMessage_BumpsCounterWhileRunning(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "autotag")
	m.SetWatcherMessage("added a.png")
	m.SetWatcherMessage("added b.png")

	state := m.Get()
	if !state.Running {
		t.Fatal("job should still be running")
	}
	if state.Changes != 2 {
		t.Errorf("Changes = %d, want 2", state.Changes)
	}
}

func TestNoteChange_CountsOnlyWhileAJobRuns(t *testing.T) {
	m := NewManager()
	m.NoteChange()
	mustStart(t, m, "sync")
	m.NoteChange()
	m.NoteChange()
	if got := m.Get().Changes; got != 2 {
		t.Errorf("Changes = %d, want 2", got)
	}
	m.Complete("done")
	m.NoteChange()
	if got := m.Get().Changes; got != 2 {
		t.Errorf("Changes after the job = %d, want 2", got)
	}
}

func TestReserve_BlocksUserStart(t *testing.T) {
	m := NewManager()
	r, err := m.Reserve()
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	defer r.Release()

	if err := m.Start("sync"); err != ErrJobRunning {
		t.Errorf("user Start during a reservation = %v, want ErrJobRunning", err)
	}
	if !m.IsRunning() {
		t.Error("IsRunning should be true while the lane is reserved")
	}
	if err := r.Start("sync"); err != nil {
		t.Errorf("the holder's Start = %v, want nil", err)
	}
	m.Complete("done")
}

func TestReserve_RefusesWhileJobRunning(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	if _, err := m.Reserve(); err != ErrJobRunning {
		t.Errorf("Reserve with active job = %v, want ErrJobRunning", err)
	}
	m.Complete("done")
}

func TestReserve_DoubleAcquireRefuses(t *testing.T) {
	m := NewManager()
	r, err := m.Reserve()
	if err != nil {
		t.Fatalf("first Reserve failed: %v", err)
	}
	defer r.Release()
	if _, err := m.Reserve(); err != ErrJobRunning {
		t.Errorf("second Reserve = %v, want ErrJobRunning", err)
	}
}

func TestReservation_CancelDoesNotFreeTheLane(t *testing.T) {
	m := NewManager()
	r, err := m.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := r.Start("sync"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Cancel()
	m.Complete("cancelled")

	if !r.Cancelled() {
		t.Error("the holder was not told of the cancel")
	}
	if _, err := m.Reserve(); err != ErrJobRunning {
		t.Errorf("Reserve after a cancel = %v, want ErrJobRunning while the holder runs", err)
	}
	if err := m.Start("sync"); err != ErrJobRunning {
		t.Errorf("Start after a cancel = %v, want ErrJobRunning while the holder runs", err)
	}
	r.Release()
	if err := m.Start("sync"); err != nil {
		t.Errorf("Start after the holder released = %v, want nil", err)
	}
}

func TestReservation_OnlyItsHolderReleasesIt(t *testing.T) {
	m := NewManager()
	r1, err := m.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	r1.Release()
	r2, err := m.Reserve()
	if err != nil {
		t.Fatalf("Reserve after a release = %v", err)
	}
	defer r2.Release()

	r1.Release()

	if err := m.Start("sync"); err != ErrJobRunning {
		t.Errorf("Start after a stale Release = %v, want ErrJobRunning: r2 still holds the lane", err)
	}
	if err := r1.Start("sync"); err != ErrJobRunning {
		t.Errorf("a released handle's Start = %v, want ErrJobRunning", err)
	}
	if r2.Cancelled() {
		t.Error("r2 reads cancelled with no Cancel")
	}
}

func TestMarkViewed_NopBeforeComplete(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.MarkViewed()
	m.Complete("done")
	m.MarkViewed()
	if m.Get() == nil {
		t.Fatal("state should still be visible right after MarkViewed")
	}
}

func TestMarkViewed_ShortensDismiss(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "sync")
	m.Complete("done")

	m.MarkViewed()
	m.MarkViewed()

	if m.Get() == nil {
		t.Error("state cleared too early by MarkViewed")
	}
	m.Dismiss()
	if m.Get() != nil {
		t.Error("Dismiss should clear the state regardless of MarkViewed")
	}
}

func TestMarkViewed_KeepsFullWindowOnFailure(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "tag")
	m.Fail("merged 0 folded tag(s), skipped 1")

	m.MarkViewed()
	if m.viewed {
		t.Error("a failed job armed the shortened dismiss")
	}
	if m.Get() == nil {
		t.Fatal("state cleared by MarkViewed")
	}
}

func TestRelease_ReleasesUserStart(t *testing.T) {
	m := NewManager()
	r, err := m.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	r.Release()
	if err := m.Start("sync"); err != nil {
		t.Errorf("Start after Release = %v, want nil", err)
	}
}

func TestFinished_CountsPastTheAutoDismiss(t *testing.T) {
	m := NewManager()
	if got := m.Finished(); got != 0 {
		t.Fatalf("Finished on a fresh manager = %d, want 0", got)
	}
	mustStart(t, m, "sync")
	m.Complete("done")
	m.Dismiss()
	if got := m.Finished(); got != 1 {
		t.Errorf("Finished after a completed job = %d, want 1", got)
	}
	mustStart(t, m, "sync")
	m.Fail("boom")
	if got := m.Finished(); got != 2 {
		t.Errorf("Finished after a failed job = %d, want 2", got)
	}
}

func mustStart(t *testing.T, m *Manager, jobType string) {
	t.Helper()
	if err := m.Start(jobType); err != nil {
		t.Fatalf("Start(%q): %v", jobType, err)
	}
}
