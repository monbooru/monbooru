// Package schedule runs the daily pass over a set's galleries: the clock
// trigger, the catch-up after a missed slot, and the actions each gallery
// is ticked for.
package schedule

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// Long enough that a cold start does not compete with the operator's
// first pages.
const graceDelay = 5 * time.Minute

// Past its slot by more than this, the clock was asleep or stepped: the
// slot goes to the catch-up rule instead of running late.
const wakeSlack = 2 * time.Minute

const catchUpRetry = time.Minute

type Host interface {
	// View is the schedule a pass reads, with the lookup phases off while
	// monloader is unpaired, and the configured gallery names.
	View() (config.ScheduleConfig, []string)
	Config() *config.Config
	Naming(name string) gallery.Naming
	// MemoryMark notes the memory in use; the func it returns logs the
	// peak over it for scope.
	MemoryMark() func(scope string)
}

// Lookups runs the two lookup phases, which speak monloader's protocol,
// under the job the pass started and returns the lines to record.
type Lookups interface {
	Usable() bool
	PTRReady() bool
	LookupPTR(ctx context.Context, cx *library.Gallery) ([]string, error)
	LookupOnline(ctx context.Context, cx *library.Gallery) ([]string, error)
}

type Scheduler struct {
	set       *library.Set
	jobs      *jobs.Manager
	host      Host
	lookups   Lookups
	statePath string
	done      <-chan struct{}
	// Buffered to 1 with non-blocking sends, so concurrent saves coalesce
	// into one wake-up.
	reload chan struct{}

	mu            sync.Mutex
	lastRun       time.Time
	lastDur       time.Duration
	lastInfo      string
	lookupInfo    []string
	galleryOffset int
	catchUpDay    time.Time
}

// statePath keeps the last run out of the config: a timestamp rewritten
// nightly would show in every diff of the operator's file.
func New(set *library.Set, jm *jobs.Manager, host Host, lookups Lookups, statePath string, done <-chan struct{}) *Scheduler {
	return &Scheduler{set: set, jobs: jm, host: host, lookups: lookups, statePath: statePath, done: done, reload: make(chan struct{}, 1)}
}

func (s *Scheduler) RequestReload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

func (s *Scheduler) markCatchUpDay(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catchUpDay = at
}

func (s *Scheduler) catchUpFiredToday(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.catchUpDay.IsZero() && !localDayStart(now).After(localDayStart(s.catchUpDay))
}

func (s *Scheduler) nextOffset(n int) int {
	if n <= 1 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.galleryOffset % n
	s.galleryOffset = (start + 1) % n
	return start
}

func (s *Scheduler) recordLookup(lines ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupInfo = append(s.lookupInfo, lines...)
}

func (s *Scheduler) clearLookup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupInfo = nil
}

// Run returns once done closes.
func (s *Scheduler) Run() {
	// Its own goroutine: the grace delay must not hold the clock trigger
	// unarmed, or a start shortly before schedule.time misses that day.
	go s.catchUp()
	for {
		next, ok := s.NextFire(time.Now())
		if !ok {
			select {
			case <-s.done:
				return
			case <-s.reload:
				continue
			case <-time.After(time.Hour):
				continue
			}
		}
		logx.Infof("scheduler: next run at %s (in %s)", next.Format(time.RFC3339), max(time.Until(next), 0).Round(time.Second))
	slot:
		for {
			wait, fire, missed := clockWake(next, time.Now())
			switch {
			case missed:
				logx.Infof("scheduler: the %s run went by while this machine was asleep", next.Format(time.RFC3339))
				go s.catchUp()
				break slot
			case fire:
				if s.clockFireOwed(time.Now()) {
					if sched, _ := s.host.View(); !s.RunNow() && sched.EffectiveMode() == config.ScheduleAtTimeCatchup {
						go s.catchUpAfter(catchUpRetry, catchUpRetry)
					}
				} else {
					logx.Infof("scheduler: today's pass already ran; skipping the clock trigger")
				}
				break slot
			}
			select {
			case <-s.done:
				return
			case <-s.reload:
				break slot
			case <-time.After(wait):
			}
		}
	}
}

// Timers run on a clock that stops in suspend, so the wall clock is
// re-read at least every minute.
func clockWake(next, now time.Time) (wait time.Duration, fire, missed bool) {
	if now.Before(next) {
		return min(next.Sub(now), time.Minute), false, false
	}
	if now.Sub(next) > wakeSlack {
		return 0, false, true
	}
	return 0, true, false
}

func (s *Scheduler) catchUp() { s.catchUpAfter(graceDelay, catchUpRetry) }

func (s *Scheduler) catchUpAfter(grace, retry time.Duration) {
	if !s.catchUpOwed() {
		return
	}
	for wait := grace; ; wait = retry {
		select {
		case <-s.done:
			return
		case <-time.After(wait):
		}
		// Re-read: the schedule may have changed during the wait.
		if !s.catchUpOwed() {
			return
		}
		if s.jobs.IsRunning() {
			continue
		}
		logx.Infof("scheduler: running the pass this machine was not awake for")
		s.markCatchUpDay(time.Now())
		if s.RunNow() {
			return
		}
	}
}

func (s *Scheduler) catchUpOwed() bool {
	sched, galleries := s.host.View()
	return shouldCatchUp(sched, galleries, s.LastRun(), time.Now())
}

// The clock skips only a day the startup catch-up covered, or a boot
// before schedule.time would run twice; a manual run does not spend the
// night's pass.
func (s *Scheduler) clockFireOwed(now time.Time) bool {
	sched, _ := s.host.View()
	if sched.EffectiveMode() != config.ScheduleAtTimeCatchup || !s.catchUpFiredToday(now) {
		return true
	}
	// A catch-up the job lane refused recorded no run, so the clock still
	// owes the day.
	return dayPassOwed(s.LastRun(), now)
}

// on_start has no clock trigger, so this check is its only once-per-day
// bound. at_time_catchup owes the most recent slot, so a morning boot
// after an evening slot that ran owes nothing.
func shouldCatchUp(sched config.ScheduleConfig, galleries []string, last, now time.Time) bool {
	switch sched.EffectiveMode() {
	case config.ScheduleAtTimeCatchup, config.ScheduleOnStart:
	default:
		return false
	}
	if !hasWork(sched, galleries) {
		return false
	}
	if t, err := parseTime(sched.Time); err == nil && sched.EffectiveMode() == config.ScheduleAtTimeCatchup {
		return last.Before(mostRecentSlot(t, now))
	}
	return dayPassOwed(last, now)
}

func mostRecentSlot(t clockTime, now time.Time) time.Time {
	local := now.In(time.Local)
	year, month, day := local.Date()
	slot := time.Date(year, month, day, t.hour, t.minute, 0, 0, time.Local)
	if slot.After(local) {
		slot = time.Date(year, month, day-1, t.hour, t.minute, 0, 0, time.Local)
	}
	return slot
}

// The local calendar day, not 24 hours: a boot slightly earlier than
// yesterday's run would otherwise skip a day.
func dayPassOwed(last, now time.Time) bool {
	return last.IsZero() || localDayStart(now).After(localDayStart(last))
}

func localDayStart(t time.Time) time.Time {
	local := t.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
}

func firesAtTime(sched config.ScheduleConfig, galleries []string) bool {
	if !hasWork(sched, galleries) {
		return false
	}
	mode := sched.EffectiveMode()
	return mode == config.ScheduleAtTime || mode == config.ScheduleAtTimeCatchup
}

func (s *Scheduler) NextFire(now time.Time) (time.Time, bool) {
	sched, galleries := s.host.View()
	if !firesAtTime(sched, galleries) {
		return time.Time{}, false
	}
	t, err := parseTime(sched.Time)
	if err != nil {
		return time.Time{}, false
	}
	year, month, day := now.Date()
	fire := time.Date(year, month, day, t.hour, t.minute, 0, 0, now.Location())
	// A day the catch-up covered is skipped too, so its time is not the
	// next fire.
	if !fire.After(now) || !s.clockFireOwed(now) {
		// day+1, not Add(24h), which slips the local time by an hour
		// across DST.
		fire = time.Date(year, month, day+1, t.hour, t.minute, 0, 0, now.Location())
	}
	return fire, true
}

type clockTime struct{ hour, minute int }

func parseTime(v string) (clockTime, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return clockTime{}, err
	}
	return clockTime{hour: t.Hour(), minute: t.Minute()}, nil
}

func hasWork(sc config.ScheduleConfig, galleries []string) bool {
	for _, action := range config.ScheduleActions {
		for _, g := range galleries {
			if sc.RunsOn(action, g) {
				return true
			}
		}
	}
	return false
}

// RunNow runs a pass unless a job holds the lane. The lookup phases run
// last: an external limit can cut them short without costing the rest,
// and their writes must not interleave with autotag's.
func (s *Scheduler) RunNow() bool {
	r, err := s.jobs.Reserve()
	if err != nil {
		logx.Warnf("scheduler: skipping run (a job is already running)")
		return false
	}
	defer r.Release()

	started := time.Now()
	s.clearLookup()
	var failures []string
	cancelled := false
	defer func() {
		info := "OK"
		switch {
		case len(failures) > 0:
			info = strings.Join(failures, "; ")
		case cancelled:
			info = "cancelled"
		}
		// A pass the shutdown cut short stays owed, so the next boot's catch-up redoes it.
		s.recordRun(started, time.Since(started), info, !cancelled || !s.stopping())
	}()

	sched, _ := s.host.View()

	// Sorted, then rotated run to run: a budget too small for the first
	// gallery would otherwise starve the rest.
	names := s.set.Names()
	names = rotate(names, s.nextOffset(len(names)))

	// Without this check a cancel would stop only the current phase.
	abort := func() bool {
		if r.Cancelled() {
			logx.Infof("scheduler: run cancelled mid-flight; remaining phases skipped")
			cancelled = true
			return true
		}
		return false
	}
	phase := func(on bool, name string, run func() error) bool {
		if !on {
			return false
		}
		if err := run(); err != nil {
			failures = append(failures, name+": "+err.Error())
		}
		return abort()
	}

	for _, name := range names {
		if abort() {
			return true
		}
		cx := s.set.Get(name)
		runs := func(action string) bool { return sched.RunsOn(action, name) }
		if cx == nil || cx.Closed() || !slices.ContainsFunc(config.ScheduleActions, runs) {
			continue
		}
		logx.Infof("scheduler: running actions on gallery %q", name)

		if phase(runs(config.ActionSyncGallery) && !cx.Degraded, "sync "+name, func() error { return s.sync(r, cx) }) {
			return true
		}
		if phase(runs(config.ActionRemoveOrphans), "remove-orphans "+name, func() error { return s.removeOrphans(r, cx) }) {
			return true
		}
		if phase(runs(config.ActionRunAutoTaggers) && tagger.IsAvailable(s.host.Config()), "autotag "+name, func() error { return s.autotag(r, cx) }) {
			return true
		}
		if phase(runs(config.ActionFindRelationPairs), "find-pairs "+name, func() error { return s.findRelationPairs(r, cx) }) {
			return true
		}
		if phase(runs(config.ActionLookupPTR), "ptr-lookup "+name, func() error {
			switch {
			case !s.lookups.Usable():
				s.recordLookup("[" + name + "] Lookup skipped: monloader unreachable.")
			case !s.lookups.PTRReady():
				s.recordLookup("[" + name + "] PTR lookup skipped: monloader's index is not ready.")
			default:
				return s.lookup(r, cx, "ptr lookup", s.lookups.LookupPTR)
			}
			return nil
		}) {
			return true
		}
		if phase(runs(config.ActionLookupBooru), "online-lookup "+name, func() error {
			if !s.lookups.Usable() {
				s.recordLookup("[" + name + "] Online lookup skipped: monloader unreachable.")
				return nil
			}
			return s.lookup(r, cx, "online lookup", s.lookups.LookupOnline)
		}) {
			return true
		}
	}
	return true
}

func (s *Scheduler) startPhase(r *jobs.Reservation, jobType, phase, gallery string) (context.Context, error) {
	if err := r.Start(jobType, gallery); err != nil {
		logx.Warnf("scheduler %s %q: %v", phase, gallery, err)
		return nil, err
	}
	return s.jobs.Context(), nil
}

func rotate(names []string, offset int) []string {
	if offset <= 0 || offset >= len(names) {
		return names
	}
	return append(append([]string(nil), names[offset:]...), names[:offset]...)
}

func (s *Scheduler) lookup(r *jobs.Reservation, cx *library.Gallery, phase string, run func(context.Context, *library.Gallery) ([]string, error)) error {
	ctx, err := s.startPhase(r, models.JobTypeLookup, phase, cx.Name)
	if err != nil {
		return err
	}
	lines, err := run(ctx, cx)
	s.recordLookup(lines...)
	return err
}

func (s *Scheduler) findRelationPairs(r *jobs.Reservation, cx *library.Gallery) error {
	ctx, err := s.startPhase(r, models.JobTypeRelations, "find-pairs", cx.Name)
	if err != nil {
		return err
	}
	rc := s.host.Config().Relations
	opts := relations.FindPairsOptions{
		Distance:         int(relations.IncrementalProbeDistance.Load()),
		Replace:          false,
		TagPairs:         rc.TagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(rc.TagPairThreshold),
	}
	added, err := cx.FindPairs(ctx, opts, s.jobs.Update)
	return s.jobs.Settle(ctx, err,
		fmt.Sprintf("[%s] find-pairs cancelled (%d added)", cx.Name, added),
		fmt.Sprintf("[%s] find-pairs added %d %s.", cx.Name, added, models.Plural(added, "candidate", "candidates")))
}

func (s *Scheduler) sync(r *jobs.Reservation, cx *library.Gallery) error {
	ctx, err := s.startPhase(r, models.JobTypeSync, "sync", cx.Name)
	if err != nil {
		return err
	}
	result, err := cx.Sync(ctx, s.host.Config().Gallery.MaxFileSizeMB, s.host.Naming(cx.Name), s.jobs.Update, s.jobs.NoteChange)
	if err := s.jobs.Settle(ctx, err,
		fmt.Sprintf("[%s] sync cancelled (%s)", cx.Name, result.Summary()),
		fmt.Sprintf("[%s] %s", cx.Name, result.Summary())); err != nil {
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return err
	}
	return nil
}

func (s *Scheduler) removeOrphans(r *jobs.Reservation, cx *library.Gallery) error {
	ctx, err := s.startPhase(r, models.JobTypePruneThumbs, "orphans", cx.Name)
	if err != nil {
		return err
	}
	removed, processed, total, err := gallery.PruneOrphanThumbnails(ctx, cx.DB, cx.ThumbnailsPath, func(done, total int) {
		s.jobs.Update(done, total, fmt.Sprintf("[%s] pruning…", cx.Name))
	})
	s.jobs.Finish(err, ctx.Err() != nil,
		fmt.Sprintf("[%s] orphan sweep cancelled (%d/%d scanned, %d removed)", cx.Name, processed, total, removed),
		fmt.Sprintf("[%s] removed %d orphaned %s", cx.Name, removed, models.Plural(removed, "thumbnail", "thumbnails")))
	if err != nil {
		logx.Warnf("scheduler orphans %q: %v", cx.Name, err)
		return err
	}
	logx.Infof("scheduler: [%s] removed %d orphaned %s", cx.Name, removed, models.Plural(removed, "thumbnail", "thumbnails"))
	return nil
}

// A stamped image was tagged before, so tags the operator stripped since
// stay stripped. Without ffmpeg the tagger skips the types only it frames
// and leaves them unstamped, so they are left out rather than retried nightly.
func autotagCandidates(d *db.DB, ffmpeg bool) ([]int64, error) {
	q := `SELECT id FROM images WHERE is_missing = 0 AND auto_tagged_at IS NULL`
	if !ffmpeg {
		var skip []string
		for _, ft := range models.FileTypes {
			if ft.Video || ft.FFmpegStill {
				skip = append(skip, "'"+ft.Name+"'")
			}
		}
		q += ` AND file_type NOT IN (` + strings.Join(skip, ", ") + `)`
	}
	return db.QueryIDs(d.Read, q)
}

func (s *Scheduler) autotag(r *jobs.Reservation, cx *library.Gallery) error {
	ids, err := autotagCandidates(cx.DB, gallery.FFmpegAvailable())
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	cfg := s.host.Config()
	enabled := tagger.EnabledTaggersForGallery(cfg, cx.Name)
	if len(enabled) == 0 {
		return nil
	}
	ctx, err := s.startPhase(r, models.JobTypeAutotag, "autotag", cx.Name)
	if err != nil {
		return err
	}
	peak := s.host.MemoryMark()
	skipped, err := tagger.RunWithTaggers(ctx, cx.Handle, cfg, ids, enabled, s.jobs, cfg.Tagger.ExecutionProvider)
	finished, err := tagger.Settle(ctx, s.jobs, "["+cx.Name+"] ", "", len(ids), skipped, err)
	if finished {
		peak(fmt.Sprintf("scheduled %s %d %s", cx.Name, len(ids), models.Plural(len(ids), "image", "images")))
	}
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
	}
	return err
}

func (s *Scheduler) recordRun(started time.Time, dur time.Duration, info string, persist bool) {
	s.mu.Lock()
	s.lastRun, s.lastDur, s.lastInfo = started, dur, info
	s.mu.Unlock()
	// Persisted: the catch-up after a restart needs to know whether last
	// night ran.
	if persist {
		s.saveRun(started)
	}
}

func (s *Scheduler) stopping() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

type Status struct {
	LastRun     time.Time
	LastDur     time.Duration
	LastInfo    string
	NextRun     time.Time
	NextRunNote string
	LookupInfo  []string
}

func (s *Scheduler) Status() Status {
	s.mu.Lock()
	st := Status{
		LastRun: s.lastRun, LastDur: s.lastDur, LastInfo: s.lastInfo,
		LookupInfo: append([]string(nil), s.lookupInfo...),
	}
	s.mu.Unlock()
	// This process has not run a pass yet; the stored run is the last one.
	if st.LastRun.IsZero() {
		if last := s.LastRun(); !last.IsZero() {
			st.LastRun = last.Local()
		}
	}
	if next, ok := s.NextFire(time.Now()); ok {
		st.NextRun = next
		return st
	}
	sched, galleries := s.host.View()
	switch mode := sched.EffectiveMode(); {
	case mode == config.ScheduleOff:
		st.NextRunNote = "No next run scheduled (set to never)."
	case mode == config.ScheduleOnStart:
		st.NextRunNote = "No next run scheduled (it runs at startup only)."
	case !slices.ContainsFunc(config.ScheduleActions, sched.Enabled):
		st.NextRunNote = "No next run scheduled (every action is off)."
	case !hasWork(sched, galleries):
		st.NextRunNote = "No next run scheduled (no gallery is ticked for the actions that are on)."
	default:
		st.NextRunNote = "No next run scheduled (the time of day is unreadable)."
	}
	return st
}

type state struct {
	LastRun time.Time `toml:"last_scheduled_run"`
}

// LastRun is the last pass the state file records.
func (s *Scheduler) LastRun() time.Time {
	var st state
	if _, err := toml.DecodeFile(s.statePath, &st); err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("schedule state: %v", err)
		}
		return time.Time{}
	}
	return st.LastRun
}

func (s *Scheduler) saveRun(at time.Time) {
	err := fsx.WriteAtomic(s.statePath, ".state.toml.*", func(f *os.File) error {
		return toml.NewEncoder(f).Encode(state{LastRun: at.UTC()})
	})
	if err != nil {
		logx.Warnf("schedule state: %v", err)
	}
}
