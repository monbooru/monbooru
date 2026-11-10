package web

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
)

func seedImage(t *testing.T, srv *Server, name string, w, h int) int64 {
	t.Helper()
	cx := srv.active()
	if cx == nil {
		t.Fatal("no active gallery")
	}
	path := filepath.Join(cx.GalleryPath, name)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, path, "")
	return rec.ID
}

func TestScheduledRemoveOrphans_RemovesStrayFiles(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", RemoveOrphans: true}
	srv.cfgMu.Unlock()

	id := seedImage(t, srv, "keeper.png", 10, 10)

	cx := srv.active()

	orphanJpg := filepath.Join(cx.ThumbnailsPath, "999999.jpg")
	orphanHover := filepath.Join(cx.ThumbnailsPath, "999999_hover.webp")
	for _, p := range []string{orphanJpg, orphanHover} {
		mustWriteFile(t, p, []byte("orphan"), 0o644)
	}

	bystander := filepath.Join(cx.ThumbnailsPath, "note.txt")
	mustWriteFile(t, bystander, []byte("keep me"), 0o644)

	srv.sched.RunNow()

	if _, err := os.Stat(orphanJpg); !os.IsNotExist(err) {
		t.Error("orphan .jpg should have been removed")
	}
	if _, err := os.Stat(orphanHover); !os.IsNotExist(err) {
		t.Error("orphan _hover.webp should have been removed")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Error("bystander file (not matching the pattern) should be kept")
	}
	thumb := filepath.Join(cx.ThumbnailsPath, gallery.ThumbnailPath(cx.ThumbnailsPath, id)[len(cx.ThumbnailsPath)+1:])
	if _, err := os.Stat(thumb); err != nil {
		t.Errorf("real thumbnail should survive sweep: %v", err)
	}
}

func TestScheduledRemoveOrphans_ReportsViaJobManager(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", RemoveOrphans: true}
	srv.cfgMu.Unlock()
	cx := srv.active()

	orphanJpg := filepath.Join(cx.ThumbnailsPath, "424242.jpg")
	mustWriteFile(t, orphanJpg, []byte("orphan"), 0o644)

	srv.sched.RunNow()

	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("expected a job state after the orphan sweep")
	}
	if state.Running {
		t.Error("job should be marked done, not running")
	}
	if state.JobType != "prune-thumbs" {
		t.Errorf("JobType = %q, want prune-thumbs", state.JobType)
	}
	wantIn(t, state.Summary, "removed 1 orphaned thumbnail")
}

func TestScheduledSync_IngestsNewFiles(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", SyncGallery: true}
	srv.cfgMu.Unlock()
	cx := srv.active()

	path := filepath.Join(cx.GalleryPath, "dropped.png")
	mustWriteFile(t, path, makePNGBytes(t, 10, 10, 0, 0, 0), 0o644)

	srv.sched.RunNow()

	count := countRows(t, cx.DB, "images WHERE canonical_path = ?", path)
	if count != 1 {
		t.Errorf("scheduled sync did not ingest dropped file, count = %d", count)
	}
}

func TestScheduleStatus_RecordsLastRun(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", RemoveOrphans: true}
	srv.cfgMu.Unlock()

	if got := srv.sched.Status(); !got.LastRun.IsZero() {
		t.Fatalf("fresh server: LastRun should be zero, got %v", got.LastRun)
	}
	srv.sched.RunNow()
	got := srv.sched.Status()
	if got.LastRun.IsZero() {
		t.Error("after a run: LastRun should be populated")
	}
	if got.LastInfo != "OK" {
		t.Errorf("LastInfo = %q, want OK", got.LastInfo)
	}
}

func TestScheduledRun_SkipsWhenJobRunning(t *testing.T) {
	srv := newTestServer(t)

	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time:          "01:00",
		SyncGallery:   true,
		RemoveOrphans: true,
	}
	srv.cfgMu.Unlock()

	mustStartJob(t, srv.jobs, "sync")
	defer srv.jobs.Complete("test")

	cx := srv.active()
	orphan := filepath.Join(cx.ThumbnailsPath, "77777.jpg")
	mustWriteFile(t, orphan, []byte("x"), 0o644)

	srv.sched.RunNow()

	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan should still exist when scheduler declines: %v", err)
	}
}

func TestScheduledRun_CancelAbortsRemainingPhases(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time:          "01:00",
		SyncGallery:   true,
		RemoveOrphans: true,
	}
	srv.cfgMu.Unlock()

	cx := srv.active()
	orphan := filepath.Join(cx.ThumbnailsPath, "55555.jpg")
	mustMkdirAll(t, cx.ThumbnailsPath, 0o755)
	mustWriteFile(t, orphan, []byte("orphan"), 0o644)

	r, err := srv.jobs.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	srv.jobs.Cancel()
	defer r.Release()

	if !r.Cancelled() {
		t.Errorf("Cancel should have reached the reservation; phases must abort")
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan should still exist (scheduler never ran past cancel): %v", err)
	}
}

func TestScheduledRun_ExecutesEnabledPhases(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time:           "01:00",
		SyncGallery:    true,
		RemoveOrphans:  true,
		RunAutoTaggers: true,
	}
	srv.cfgMu.Unlock()

	cx := srv.active()

	syncPath := filepath.Join(cx.GalleryPath, "phase_sync.png")
	mustWriteFile(t, syncPath, makePNGBytes(t, 10, 10, 0, 0, 0), 0o644)

	orphan := filepath.Join(cx.ThumbnailsPath, "424242.jpg")
	mustMkdirAll(t, cx.ThumbnailsPath, 0o755)
	mustWriteFile(t, orphan, []byte("zz"), 0o644)

	srv.sched.RunNow()

	count := countRows(t, cx.DB, "images WHERE canonical_path = ?", syncPath)
	if count != 1 {
		t.Error("scheduler's sync phase didn't ingest phase_sync.png")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("scheduler's orphan sweep didn't remove the orphan thumbnail")
	}
}

func TestScheduledRunTakesEachActionToItsGalleries(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "photos", filepath.Join(t.TempDir(), "photos"))
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time: "01:00", SyncGallery: true, RemoveOrphans: true,
		Galleries: map[string][]string{
			config.ActionSyncGallery:   {"default"},
			config.ActionRemoveOrphans: {"photos"},
		},
	}
	srv.cfgMu.Unlock()

	dropped, orphans := map[string]string{}, map[string]string{}
	for _, name := range []string{"default", "photos"} {
		cx := srv.get(name)
		dropped[name] = filepath.Join(cx.GalleryPath, "dropped.png")
		mustWriteFile(t, dropped[name], makePNGBytes(t, 10, 10, 0, 0, 0), 0o644)
		orphans[name] = filepath.Join(cx.ThumbnailsPath, "424242.jpg")
		mustMkdirAll(t, cx.ThumbnailsPath, 0o755)
		mustWriteFile(t, orphans[name], []byte("orphan"), 0o644)
	}

	srv.sched.RunNow()

	for name, synced := range map[string]bool{"default": true, "photos": false} {
		n := countRows(t, srv.get(name).DB, "images WHERE canonical_path = ?", dropped[name])
		if (n == 1) != synced {
			t.Errorf("%s: %d row(s) for the dropped file, want synced = %v", name, n, synced)
		}
	}
	for name, swept := range map[string]bool{"default": false, "photos": true} {
		if _, err := os.Stat(orphans[name]); os.IsNotExist(err) != swept {
			t.Errorf("%s: orphan removed = %v, want %v", name, os.IsNotExist(err), swept)
		}
	}
}

func TestScheduledRun_ReleasesTheLaneAndRecordsTheRun(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", RemoveOrphans: true}
	srv.cfgMu.Unlock()

	srv.sched.RunNow()

	if srv.jobs.IsRunning() {
		t.Error("the run kept the job lane")
	}
	if err := srv.jobs.Start("sync"); err != nil {
		t.Errorf("a job after the run was refused: %v", err)
	} else {
		srv.jobs.Complete("done")
	}
	if st := srv.sched.Status(); st.LastInfo != "OK" || st.LastRun.IsZero() {
		t.Errorf("recorded run = %q at %v, want OK", st.LastInfo, st.LastRun)
	}
}

func TestScheduledRun_AFailedPhaseLeavesTheRestToRun(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "photos", filepath.Join(t.TempDir(), "photos"))
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", RemoveOrphans: true}
	srv.cfgMu.Unlock()

	broken := srv.get("default").ThumbnailsPath
	if err := os.RemoveAll(broken); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, broken, []byte("not a folder"), 0o644)
	photos := srv.get("photos")
	orphan := filepath.Join(photos.ThumbnailsPath, "424242.jpg")
	mustMkdirAll(t, photos.ThumbnailsPath, 0o755)
	mustWriteFile(t, orphan, []byte("orphan"), 0o644)

	srv.sched.RunNow()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the gallery after the failed one was not swept")
	}
	if info := srv.sched.Status().LastInfo; !strings.Contains(info, "remove-orphans default: read thumbnails dir") {
		t.Errorf("recorded run = %q, want the failed phase named", info)
	}
}

func TestScheduledRun_FindPairsQueuesANearDuplicate(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", FindRelationPairs: true}
	srv.cfgMu.Unlock()
	cx := srv.active()
	a := seedImage(t, srv, "pair_a.png", 10, 10)
	b := seedImage(t, srv, "pair_b.png", 12, 12)
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = 0 WHERE id = ?`, a)
	mustExec(t, cx.DB.Write, `UPDATE images SET phash = 1 WHERE id = ?`, b)
	cx.Source().Tree.Reset()

	srv.sched.RunNow()

	if n := countRows(t, cx.DB, "potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?", min(a, b), max(a, b)); n != 1 {
		t.Errorf("queued pairs for the two images = %d, want 1", n)
	}
	if info := srv.sched.Status().LastInfo; info != "OK" {
		t.Errorf("recorded run = %q, want OK", info)
	}
}

func TestScheduleStatusShowsThePersistedLastRun(t *testing.T) {
	srv := newTestServer(t)
	at := time.Date(2026, 9, 20, 3, 17, 0, 0, time.UTC)
	mustWriteFile(t, filepath.Join(filepath.Dir(srv.configPath), "state.toml"), []byte("last_scheduled_run = "+at.Format(time.RFC3339)+"\n"), 0o644)
	var buf strings.Builder
	if err := srv.tmpl.ExecuteTemplate(&buf, "partials/schedule_status.html", map[string]any{"Status": srv.sched.Status()}); err != nil {
		t.Fatal(err)
	}
	wantIn(t, buf.String(), "Last run: "+at.Local().Format("2006-01-02 15:04:05")+".")
}
