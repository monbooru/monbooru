package schedule

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tagger/taggertest"
	"github.com/monbooru/monbooru/internal/tags"
)

type fakeHost struct {
	sched     config.ScheduleConfig
	galleries []string
	tagger    config.TaggerConfig
}

func (h *fakeHost) View() (config.ScheduleConfig, []string) { return h.sched, h.galleries }
func (h *fakeHost) Config() *config.Config {
	return &config.Config{Schedule: h.sched, Tagger: h.tagger}
}
func (h *fakeHost) Naming(string) gallery.Naming { return gallery.Naming{} }
func (h *fakeHost) MemoryMark() func(string)     { return func(string) {} }

type fakeLookups struct{}

func (fakeLookups) Usable() bool   { return false }
func (fakeLookups) PTRReady() bool { return false }

func (fakeLookups) LookupPTR(context.Context, *library.Gallery) ([]string, error) {
	return nil, nil
}

func (fakeLookups) LookupOnline(context.Context, *library.Gallery) ([]string, error) {
	return nil, nil
}

func newTestScheduler(t *testing.T, sched config.ScheduleConfig) (*Scheduler, *fakeHost) {
	t.Helper()
	host := &fakeHost{sched: sched, galleries: []string{"default"}}
	return New(nil, nil, host, fakeLookups{}, filepath.Join(t.TempDir(), "state.toml"), nil), host
}

func TestNextFire(t *testing.T) {
	t.Parallel()
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 4, day, hour, minute, 0, 0, time.Local) }
	for _, tc := range []struct {
		name      string
		sched     config.ScheduleConfig
		now, want time.Time
	}{
		{"TodayStillAhead", config.ScheduleConfig{Time: "23:59", SyncGallery: true}, at(19, 8, 0), at(19, 23, 59)},
		{"TodayPassedRollsToTomorrow", config.ScheduleConfig{Time: "01:00", RemoveOrphans: true}, at(19, 10, 0), at(20, 1, 0)},
		{"NothingEnabledReturnsFalse", config.ScheduleConfig{Time: "01:00"}, at(19, 8, 0), time.Time{}},
		{"InvalidTimeReturnsFalse", config.ScheduleConfig{Time: "not-a-time", SyncGallery: true}, at(19, 8, 0), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestScheduler(t, tc.sched)
			got, ok := s.NextFire(tc.now)
			if wantOK := !tc.want.IsZero(); ok != wantOK || ok && !got.Equal(tc.want) {
				t.Errorf("NextFire(%s) = %s, %v; want %s", tc.now, got, ok, tc.want)
			}
		})
	}
}

func TestParseTime_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in          string
		wantHour    int
		wantMinute  int
		wantErr     bool
		description string
	}{
		{"00:00", 0, 0, false, "midnight"},
		{"23:59", 23, 59, false, "last minute of day"},
		{"01:30", 1, 30, false, "mid-morning"},
		{"24:00", 0, 0, true, "hour out of range"},
		{"12:60", 0, 0, true, "minute out of range"},
		{"noon", 0, 0, true, "not a time"},
		{"12", 0, 0, true, "missing colon"},
		{"", 0, 0, true, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			got, err := parseTime(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("%q: expected error", tc.in)
				}
				return
			}
			if err != nil {
				t.Errorf("%q: unexpected error %v", tc.in, err)
			}
			if got.hour != tc.wantHour || got.minute != tc.wantMinute {
				t.Errorf("%q: got {%d,%d}, want {%d,%d}", tc.in, got.hour, got.minute, tc.wantHour, tc.wantMinute)
			}
		})
	}
}

func TestShouldCatchUp(t *testing.T) {
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.Local)
	enabled := config.ScheduleConfig{SyncGallery: true, Time: "01:00"}
	yesterday := now.Add(-25 * time.Hour)
	thisMorning := now.Add(-2 * time.Hour)

	yesterdayJustLater := now.Add(-23*time.Hour - 55*time.Minute)

	cases := []struct {
		name string
		mode string
		last time.Time
		want bool
	}{
		{"at_time never catches up", config.ScheduleAtTime, time.Time{}, false},
		{"off never catches up", config.ScheduleOff, time.Time{}, false},
		{"catchup runs a missed day", config.ScheduleAtTimeCatchup, yesterday, true},
		{"catchup skips a day already run", config.ScheduleAtTimeCatchup, thisMorning, false},
		{"catchup runs a new day less than 24h after the last", config.ScheduleAtTimeCatchup, yesterdayJustLater, true},
		{"on_start runs a new day less than 24h after the last", config.ScheduleOnStart, yesterdayJustLater, true},
		{"catchup runs when nothing was ever recorded", config.ScheduleAtTimeCatchup, time.Time{}, true},
		{"on_start runs at most once a day", config.ScheduleOnStart, thisMorning, false},
		{"on_start runs when the day has turned", config.ScheduleOnStart, yesterday, true},

		{"an unknown mode is at_time", "sometimes", yesterday, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := enabled
			sched.Mode = tc.mode
			if got := shouldCatchUp(sched, []string{"default"}, tc.last, now); got != tc.want {
				t.Errorf("shouldCatchUp = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("catchup skips an evening slot that ran last night", func(t *testing.T) {
		sched := enabled
		sched.Mode, sched.Time = config.ScheduleAtTimeCatchup, "22:00"
		lastNight := time.Date(2026, 8, 23, 22, 0, 0, 0, time.Local)
		if shouldCatchUp(sched, []string{"default"}, lastNight, now) {
			t.Error("a morning boot owed the evening pass that already ran")
		}
		if !shouldCatchUp(sched, []string{"default"}, lastNight.AddDate(0, 0, -1), now) {
			t.Error("the evening pass missed last night was not owed")
		}
	})
}

func TestClockWakeSkipsASlotSleptThrough(t *testing.T) {
	next := time.Date(2026, 8, 24, 1, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name         string
		now          time.Time
		wait         time.Duration
		fire, missed bool
	}{
		{"hours ahead waits a minute", next.Add(-7 * time.Hour), time.Minute, false, false},
		{"seconds ahead waits the rest", next.Add(-20 * time.Second), 20 * time.Second, false, false},
		{"on time fires", next.Add(5 * time.Second), 0, true, false},
		{"seven hours late was slept through", next.Add(7 * time.Hour), 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wait, fire, missed := clockWake(next, tc.now)
			if wait != tc.wait || fire != tc.fire || missed != tc.missed {
				t.Errorf("clockWake = (%v, %v, %v), want (%v, %v, %v)", wait, fire, missed, tc.wait, tc.fire, tc.missed)
			}
		})
	}
}

func TestStatusReadsThePersistedLastRun(t *testing.T) {
	s, _ := newTestScheduler(t, config.ScheduleConfig{})
	at := time.Date(2026, 9, 20, 3, 17, 0, 0, time.UTC)
	s.saveRun(at)
	if got := s.Status().LastRun; !got.Equal(at) {
		t.Errorf("last run = %v, want the stored %v", got, at)
	}
}

func TestShouldCatchUpNeedsAnEnabledAction(t *testing.T) {
	sched := config.ScheduleConfig{Mode: config.ScheduleAtTimeCatchup}
	if shouldCatchUp(sched, []string{"default"}, time.Time{}, time.Now()) {
		t.Error("caught up with every action off")
	}
}

func TestAnActionTickedForNoGalleryArmsNothing(t *testing.T) {
	sched := config.ScheduleConfig{
		Time: "01:00", Mode: config.ScheduleAtTimeCatchup, SyncGallery: true,
		Galleries: map[string][]string{config.ActionSyncGallery: {}},
	}
	s, _ := newTestScheduler(t, sched)

	if shouldCatchUp(sched, []string{"default"}, time.Time{}, time.Now()) {
		t.Error("caught up with no gallery ticked")
	}
	st := s.Status()
	if !st.NextRun.IsZero() {
		t.Errorf("next run = %v, want none", st.NextRun)
	}
	if !strings.Contains(st.NextRunNote, "no gallery is ticked for the actions that are on") {
		t.Errorf("note = %q", st.NextRunNote)
	}
}

func TestClockFireSkipsADayTheCatchUpAlreadyRan(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		mode    string
		last    time.Time
		catchUp time.Time
		want    bool
	}{
		{"never run", config.ScheduleAtTimeCatchup, time.Time{}, time.Time{}, true},
		{"catch-up covered today", config.ScheduleAtTimeCatchup, now, now, false},
		{"catch-up was yesterday", config.ScheduleAtTimeCatchup, now.AddDate(0, 0, -1), now.AddDate(0, 0, -1), true},
		{"manual run only", config.ScheduleAtTimeCatchup, now, time.Time{}, true},
		{"catch-up fired but recorded nothing", config.ScheduleAtTimeCatchup, now.AddDate(0, 0, -1), now, true},
		{"no catch-up in this mode", config.ScheduleAtTime, now, now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestScheduler(t, config.ScheduleConfig{Time: "01:00", Mode: tc.mode, SyncGallery: true})
			if !tc.last.IsZero() {
				s.saveRun(tc.last)
			}
			if !tc.catchUp.IsZero() {
				s.markCatchUpDay(tc.catchUp)
			}
			if got := s.clockFireOwed(now); got != tc.want {
				t.Errorf("clockFireOwed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextFireSkipsTheDayTheCatchUpTook(t *testing.T) {
	s, _ := newTestScheduler(t, config.ScheduleConfig{Time: "23:00", Mode: config.ScheduleAtTimeCatchup, SyncGallery: true})
	now := time.Date(2026, 8, 30, 9, 0, 0, 0, time.Local)

	s.saveRun(now)
	fire, ok := s.NextFire(now)
	if !ok || fire.Day() != now.Day() {
		t.Fatalf("after a manual run the next fire is still today: got %v ok=%v", fire, ok)
	}

	s.markCatchUpDay(now)
	fire, ok = s.NextFire(now)
	if !ok || fire.Day() == now.Day() {
		t.Errorf("next fire = %v, want tomorrow once the catch-up took today", fire)
	}
}

func TestModeGatesTheClockTrigger(t *testing.T) {
	s, host := newTestScheduler(t, config.ScheduleConfig{})
	for mode, wantFire := range map[string]bool{
		config.ScheduleAtTime:        true,
		config.ScheduleAtTimeCatchup: true,
		config.ScheduleOnStart:       false,
		config.ScheduleOff:           false,
	} {
		host.sched = config.ScheduleConfig{Time: "01:00", Mode: mode, SyncGallery: true}
		if _, ok := s.NextFire(time.Now()); ok != wantFire {
			t.Errorf("mode %q: fires = %v, want %v", mode, ok, wantFire)
		}
	}
}

func TestStatusNamesWhatStoppedTheClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		sync bool
		want string
	}{
		{"never", config.ScheduleOff, true, "set to never"},
		{"startup only", config.ScheduleOnStart, true, "runs at startup only"},
		{"nothing ticked", config.ScheduleAtTime, false, "every action is off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestScheduler(t, config.ScheduleConfig{Time: "01:00", Mode: tc.mode, SyncGallery: tc.sync})
			st := s.Status()
			if !st.NextRun.IsZero() {
				t.Fatalf("next run = %v, want none", st.NextRun)
			}
			if !strings.Contains(st.NextRunNote, tc.want) {
				t.Errorf("note = %q, want %q", st.NextRunNote, tc.want)
			}
		})
	}

	s, _ := newTestScheduler(t, config.ScheduleConfig{Time: "01:00", Mode: config.ScheduleAtTime, SyncGallery: true})
	if st := s.Status(); st.NextRun.IsZero() || st.NextRunNote != "" {
		t.Errorf("armed schedule reported next=%v note=%q", st.NextRun, st.NextRunNote)
	}
}

func TestStateRoundTrip(t *testing.T) {
	s, _ := newTestScheduler(t, config.ScheduleConfig{})
	if !s.LastRun().IsZero() {
		t.Error("a missing state file should read as never run")
	}
	at := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	s.saveRun(at)
	if got := s.LastRun(); !got.Equal(at) {
		t.Errorf("last run = %v, want %v", got, at)
	}

	if err := os.WriteFile(s.statePath, []byte("not toml at all ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.LastRun().IsZero() {
		t.Error("an unparseable state file should read as never run")
	}
}

func TestGalleryRotationAdvancesAndWraps(t *testing.T) {
	s, _ := newTestScheduler(t, config.ScheduleConfig{})
	names := []string{"a", "b", "c"}
	var starts []string
	for range 4 {
		starts = append(starts, rotate(names, s.nextOffset(len(names)))[0])
	}
	if want := []string{"a", "b", "c", "a"}; !slices.Equal(starts, want) {
		t.Errorf("run starts = %v, want %v", starts, want)
	}
	if got := s.nextOffset(1); got != 0 {
		t.Errorf("single-gallery offset = %d, want 0", got)
	}
}

type emptySetHost struct{}

func (emptySetHost) Layout() library.Layout                       { return library.Layout{} }
func (emptySetHost) Background(string) library.Background         { return library.Background{} }
func (emptySetHost) DefaultGallery() string                       { return "" }
func (emptySetHost) Added(config.Gallery) error                   { return nil }
func (emptySetHost) Removed(string) error                         { return nil }
func (emptySetHost) Repointed(string, string) error               { return nil }
func (emptySetHost) Renamed(string, string, string, string) error { return nil }

func TestACatchUpTheLaneRefusedRunsOnceTheLaneFrees(t *testing.T) {
	jm := jobs.NewManager()
	set, err := library.OpenSet(emptySetHost{}, jm, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{sched: config.ScheduleConfig{Mode: config.ScheduleOnStart, SyncGallery: true}, galleries: []string{"default"}}
	s := New(set, jm, host, fakeLookups{}, filepath.Join(t.TempDir(), "state.toml"), nil)
	held, err := jm.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.catchUpAfter(0, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if !s.LastRun().IsZero() {
		t.Fatal("the catch-up ran while a job held the lane")
	}
	held.Release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the catch-up never returned after the lane freed")
	}
	if s.LastRun().IsZero() {
		t.Error("the catch-up gave up on the refused pass instead of running it once the lane freed")
	}
}

func TestAutotagBacklogIsEachTaggersOwn(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Bootstrap(d); err != nil {
		t.Fatal(err)
	}
	insert := func(name string, missing int, readBy ...string) int64 {
		t.Helper()
		res, err := d.Write.Exec(`INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
			VALUES (?, ?, 1, ?, ?, datetime('now'))`, "/g/"+name, models.ExtFileType(name), "sha-"+name, missing)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		for _, tg := range readBy {
			if _, err := d.Write.Exec(`INSERT INTO image_tagger_runs (image_id, tagger_name, tagged_at) VALUES (?, ?, '2026-01-01T00:00:00Z')`, id, tg); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	fresh := insert("fresh.png", 0)
	insert("both.png", 0, "wd", "joy")
	wdOnly := insert("wd-only.png", 0, "wd")
	insert("gone.png", 1)
	video := insert("clip.mp4", 0)
	still := insert("photo.avif", 0, "joy")

	check := func(label string, ffmpeg bool, limit int, want ...autotagGroup) {
		t.Helper()
		got, _, err := autotagBacklog(d, ffmpeg, []string{"wd", "joy"}, limit, 0)
		if err != nil {
			t.Fatal(err)
		}
		same := len(got) == len(want)
		for i := 0; same && i < len(got); i++ {
			same = slices.Equal(got[i].taggers, want[i].taggers) && slices.Equal(got[i].ids, want[i].ids)
		}
		if !same {
			t.Errorf("%s: backlog = %+v, want %+v", label, got, want)
		}
	}
	check("with ffmpeg", true, 0,
		autotagGroup{[]string{"wd", "joy"}, []int64{fresh, video}},
		autotagGroup{[]string{"joy"}, []int64{wdOnly}},
		autotagGroup{[]string{"wd"}, []int64{still}})
	check("without ffmpeg a video or an ffmpeg still would be skipped unread every night", false, 0,
		autotagGroup{[]string{"wd", "joy"}, []int64{fresh}},
		autotagGroup{[]string{"joy"}, []int64{wdOnly}})
	check("the limit counts images, lowest ids first", true, 2,
		autotagGroup{[]string{"wd", "joy"}, []int64{fresh}},
		autotagGroup{[]string{"joy"}, []int64{wdOnly}})
}

func TestACappedAutotagTakesTheBacklogFromWhereTheLastRunStopped(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Bootstrap(d); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var good bytes.Buffer
	if err := png.Encode(&good, image.NewRGBA(image.Rect(0, 0, 16, 12))); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		data := good.Bytes()
		if i < 3 {
			data = []byte("truncated download")
		}
		path := filepath.Join(dir, fmt.Sprintf("%d.png", i))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Write.Exec(`INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
			VALUES (?, 'png', 1, ?, 0, datetime('now'))`, path, fmt.Sprintf("sha-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	taggertest.Use(t, map[string][]string{"stub": {"landscape"}})
	mgr := jobs.NewManager()
	s, host := newTestScheduler(t, config.ScheduleConfig{Autotag: &config.ScheduleAutotag{MaxImages: 3}})
	s.jobs = mgr
	host.tagger.Taggers = []config.TaggerInstance{{Name: "stub", Enabled: true, ConfidenceThreshold: 0.35}}
	cx := &library.Gallery{Handle: gallery.Handle{Name: "default", DB: d, TagSvc: tags.New(d, nil)}}
	at := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	s.saveRun(at)

	for night, want := range []struct {
		stamped []int64
		line    string
	}{
		{nil, "[default] Auto-tag: 0 images, 3 skipped, "},
		{[]int64{4, 5, 6}, "[default] Auto-tag: 3 images (fake 3), "},
		{[]int64{4, 5, 6, 7, 8}, "[default] Auto-tag: 2 images (fake 2), 1 skipped, "},
	} {
		s.clearLookup()
		r, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.autotag(r, cx); err != nil {
			t.Fatalf("night %d: %v", night+1, err)
		}
		r.Release()
		stamped, err := db.QueryIDs(d.Read, `SELECT image_id FROM image_tagger_runs ORDER BY image_id`)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(stamped, want.stamped) {
			t.Errorf("night %d: images read = %v, want %v", night+1, stamped, want.stamped)
		}
		if lines := s.Status().AutotagInfo; len(lines) != 1 || !strings.HasPrefix(lines[0], want.line) {
			t.Errorf("night %d: status = %q, want %q and the time", night+1, lines, want.line)
		}
	}
	if st := s.readState(); !st.LastRun.Equal(at) || st.AutotagAfter["default"] != 1 {
		t.Errorf("state = %+v, want the last run kept and the place after image 1", st)
	}
	host.sched.Autotag = nil
	r, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.autotag(r, cx); err != nil {
		t.Fatal(err)
	}
	if st := s.readState(); len(st.AutotagAfter) != 0 {
		t.Errorf("state after a run nothing cut = %+v, want no place kept", st)
	}
}

func TestAScheduleNamingNoWorkerLeavesOutATaggerOnlyAnOnDemandWorkerHolds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.Bootstrap(d); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pic.png")
	var pic bytes.Buffer
	if err := png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 16, 12))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pic.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.Exec(`INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		VALUES (?, 'png', 1, 'sha', 0, datetime('now'))`, path); err != nil {
		t.Fatal(err)
	}

	defs := []tagger.WorkerDef{{Name: "nas"}, {Name: "rented", OnDemand: true}}
	taggertest.UsePool(t, map[string][]string{"wd": {"landscape"}}, defs[:1]...)
	taggertest.UsePool(t, map[string][]string{"px": {"sky"}}, defs[1:]...)
	p := tagger.NewPool("")
	p.Set(defs)
	p.Refresh(context.Background())
	if err := p.Wake(context.Background(), []string{"rented"}, nil); err != nil {
		t.Fatal(err)
	}
	tagger.SetPool(p)

	mgr := jobs.NewManager()
	s, host := newTestScheduler(t, config.ScheduleConfig{})
	s.jobs = mgr
	host.tagger.Taggers = []config.TaggerInstance{{Name: "wd", Enabled: true, ConfidenceThreshold: 0.35}, {Name: "px", Enabled: true, ConfidenceThreshold: 0.35}}
	cx := &library.Gallery{Handle: gallery.Handle{Name: "default", DB: d, TagSvc: tags.New(d, nil)}}
	for _, step := range []struct {
		workers []string
		want    string
	}{
		{nil, "wd"},
		{nil, "wd"},
		{[]string{"rented"}, "px,wd"},
	} {
		host.sched.Autotag = &config.ScheduleAutotag{Workers: step.workers}
		r, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.autotag(r, cx); err != nil {
			t.Fatalf("workers %v: %v", step.workers, err)
		}
		r.Release()
		var got string
		if err := d.Read.QueryRow(`SELECT COALESCE(group_concat(tagger_name), '') FROM (SELECT tagger_name FROM image_tagger_runs ORDER BY 1)`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != step.want {
			t.Errorf("workers %v: read by %q, want %q", step.workers, got, step.want)
		}
	}
}

func TestWorkersUpKeepsTheNamedWorkersThatCanTakeAJob(t *testing.T) {
	t.Parallel()
	up := []tagger.JobWorker{{Name: "local"}, {Name: "beta"}}
	if got := workersUp([]string{"alpha", "beta"}, up); !slices.Equal(got, []string{"beta"}) {
		t.Errorf("workersUp = %v, want [beta]", got)
	}
	if got := workersUp([]string{"alpha"}, up); got != nil {
		t.Errorf("workersUp = %v, want none", got)
	}
	if got := offline([]string{"alpha"}); got != "alpha is offline" {
		t.Errorf("offline = %q", got)
	}
	if got := offline([]string{"alpha", "gamma"}); got != "none of alpha, gamma is up" {
		t.Errorf("offline = %q", got)
	}
}
