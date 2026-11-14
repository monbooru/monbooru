package gallery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type Watcher struct {
	handle      Handle
	fsw         *fsnotify.Watcher
	galleryName string
	galleryPath string
	db          *db.DB
	jobs        *jobs.Manager
	OnEvent     func(msg string)
	OnChange    func()
	OnPhash     PhashSink
	// Read at each ingest, so a Settings save reaches a running watcher.
	Settings func() (maxFileSizeMB int, naming Naming)
	// Read at every event, so a gallery added inside this one is left
	// alone from the moment it exists.
	boundary func() *Boundary

	mu sync.Mutex
	// Paths the watcher renamed itself, until their events have arrived:
	// without it every named file is hashed again by the ingest its own
	// rename triggers.
	selfMoved map[string]time.Time
	closing   bool
	stop      chan struct{}
	timers    map[string]*debounceTimer
	// Fired runs wait here for a slot rather than each in a goroutine of
	// its own.
	queue     []queuedRun
	draining  int
	ingesting int
	wg        sync.WaitGroup
}

// Each ingest hashes and decodes its file, so a folder moved in would
// otherwise start one per file at once. Vanish checks queue in the same
// line, behind the ingest that may claim their row.
var watcherSlots = make(chan struct{}, 2)

const debounceDelay = 500 * time.Millisecond

// Must outlast the debounced ingest a vanished file may pair with, so a move
// or an in-place replace claims the row before the file is judged gone.
const movedOutGrace = 2 * time.Second

type queuedRun struct {
	path   string
	run    func(string)
	ingest bool
}

type debounceTimer struct {
	t        *time.Timer
	deadline time.Time
	run      func(string)
	ingest   bool
}

func NewWatcher(h Handle, jobManager *jobs.Manager) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	galleryPath := h.GalleryPath

	w := &Watcher{
		handle:      h,
		fsw:         fsw,
		galleryName: h.Name,
		galleryPath: galleryPath,
		db:          h.DB,
		jobs:        jobManager,
		boundary:    h.Boundary,
		timers:      map[string]*debounceTimer{},
		selfMoved:   map[string]time.Time{},
		stop:        make(chan struct{}),
	}

	if addErr := fsw.Add(galleryPath); addErr != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("fsnotify watch gallery root: %w", addErr)
	}
	logx.Infof("watcher: watching %s", galleryPath)

	watchCount := 1
	limitHit := false
	if walkErr := WalkTree(w.boundary(), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == galleryPath {
			return nil
		}
		if limitHit {
			return filepath.SkipAll
		}
		if addErr := fsw.Add(path); addErr != nil {
			// Errno first, as glibc localises the messages; the strings
			// catch wrappers that do not unwrap to syscall.Errno.
			if errors.Is(addErr, syscall.ENOSPC) ||
				errors.Is(addErr, syscall.EMFILE) ||
				strings.Contains(addErr.Error(), "no space left") ||
				strings.Contains(addErr.Error(), "too many open files") {
				logx.Warnf("fsnotify: inotify limit hit at %d dirs. "+
					"Increase: echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.conf && sudo sysctl -p", watchCount)
				limitHit = true
				return filepath.SkipAll
			}
			logx.Warnf("fsnotify add %q: %v", path, addErr)
		} else {
			watchCount++
		}
		return nil
	}); walkErr != nil {
		// The watcher still works for the directories it did register.
		logx.Warnf("watcher: walk %q: %v", galleryPath, walkErr)
	}

	return w, nil
}

func (w *Watcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.shutdown()
			return nil

		case event, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}

			// A folder fenced off after the watch was taken still reports.
			if w.boundary().Excludes(event.Name) {
				continue
			}

			if w.jobSuppressesIngest() {
				// A folder made meanwhile is still watched; the timers for its
				// files check the job again when they fire.
				if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
					info, err := os.Stat(event.Name)
					switch {
					case err != nil && event.Has(fsnotify.Rename):
						w.unwatchUnder(event.Name)
					case err == nil && info.IsDir():
						w.registerTree(event.Name)
					}
				}
				continue
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				info, err := os.Stat(event.Name)
				if err != nil {
					// A Rename carries a move's source; if nothing claims the
					// row within the grace, the file left the library.
					if event.Has(fsnotify.Rename) {
						w.unwatchUnder(event.Name)
						w.schedule(event.Name, movedOutGrace, w.reconcileVanished, false)
					}
					continue
				}

				if info.IsDir() {
					w.registerTree(event.Name)
					continue
				}

				w.debounce(event.Name)
			}

			// Slow writers keep firing Write long after Create;
			// debouncing them holds the ingest until the bytes settle.
			if event.Has(fsnotify.Write) {
				w.debounce(event.Name)
			}

			// A delete gets the move's grace: a replace across
			// filesystems deletes and rewrites the path, and marking the
			// row missing in between races the replace's commit.
			if event.Has(fsnotify.Remove) {
				_ = w.fsw.Remove(event.Name)
				w.schedule(event.Name, movedOutGrace, w.reconcileVanished, false)
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			logx.Warnf("fsnotify error: %v", err)
		}
	}
}

func (w *Watcher) event(msg string) {
	if w.OnEvent == nil {
		return
	}
	prefix := "watcher: "
	if w.galleryName != "" {
		prefix = "watcher [" + w.galleryName + "]: "
	}
	w.OnEvent(prefix + msg)
}

func (w *Watcher) changed() {
	if w.OnChange != nil {
		w.OnChange()
	}
}

// Files can land in a new directory before its watch is added, and those
// emit no event, so what is already there is scheduled too.
func (w *Watcher) registerTree(dir string) {
	if !w.linkWorthFollowing(dir) {
		return
	}
	_ = walkTreeUnder(dir, w.boundary(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if addErr := w.fsw.Add(path); addErr != nil {
				logx.Warnf("fsnotify add new dir %q: %v", path, addErr)
			}
			return nil
		}
		w.debounce(path)
		return nil
	})
}

// fsnotify drops only a moved folder's own watch; its subfolders' watches
// keep their old names, which the new name's registration would get back.
func (w *Watcher) unwatchUnder(dir string) {
	if w.fsw.Remove(dir) != nil {
		return
	}
	for _, p := range w.fsw.WatchList() {
		if PathInside(dir, p) {
			_ = w.fsw.Remove(p)
		}
	}
}

// The walk's rule for links, applied here because registerTree starts at
// the link rather than at the gallery root.
func (w *Watcher) linkWorthFollowing(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return true
	}
	root := resolvePath(w.galleryPath)
	target, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return !PathInside(root, target) && !PathInside(target, root) && !w.boundary().leadsOut(target)
}

// SuppressesIngest names the jobs the watcher pauses for. They write
// image_paths and image_tags in their own transactions; an ingest beside them
// would race their UNIQUE constraints. prune-dirs: every directory it unlinks
// fires a Remove with no row behind it.
func SuppressesIngest(jobType string) bool {
	switch jobType {
	case models.JobTypeSync, models.JobTypeMove, models.JobTypeTransfer, models.JobTypeMerge,
		models.JobTypeDelete, models.JobTypeTag, models.JobTypePruneDirs:
		return true
	}
	return false
}

// A timer checks again on firing, as it may have been armed before the job
// started.
func (w *Watcher) jobSuppressesIngest() bool {
	if w.jobs == nil {
		return false
	}
	st := w.jobs.Get()
	if st == nil || !st.Running || !SuppressesIngest(st.JobType) {
		return false
	}
	return len(st.Galleries) == 0 || slices.Contains(st.Galleries, w.galleryName)
}

// shutdown waits out fired ingests so the caller can close the DB once
// Run returns.
func (w *Watcher) shutdown() {
	w.mu.Lock()
	w.closing = true
	close(w.stop)
	w.queue = nil
	for path, e := range w.timers {
		e.t.Stop()
		delete(w.timers, path)
	}
	w.mu.Unlock()
	w.wg.Wait()
	_ = w.fsw.Close()
}

func (w *Watcher) debounce(path string) {
	w.schedule(path, debounceDelay, w.ingestFile, true)
}

// Outlasts both timers a self-rename can arm: the destination's ingest
// and the source's vanish check.
const selfMovedGrace = movedOutGrace + debounceDelay

// Cancelling matters as much as the mark: the rename's events usually arm
// a timer before this runs.
func (w *Watcher) claimSelfMove(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(selfMovedGrace)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if e, ok := w.timers[p]; ok {
			e.t.Stop()
			delete(w.timers, p)
		}
		w.selfMoved[p] = deadline
	}
}

func (w *Watcher) schedule(path string, delay time.Duration, run func(string), ingest bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closing {
		return
	}
	now := time.Now()
	for p, deadline := range w.selfMoved {
		if now.After(deadline) {
			delete(w.selfMoved, p)
		}
	}
	if _, mine := w.selfMoved[path]; mine {
		return
	}
	if e, ok := w.timers[path]; ok {
		e.deadline = time.Now().Add(delay)
		e.run, e.ingest = run, ingest
		e.t.Reset(delay)
		return
	}

	e := &debounceTimer{deadline: time.Now().Add(delay), run: run, ingest: ingest}
	e.t = time.AfterFunc(delay, func() { w.onDebounceFired(path, e) })
	w.timers[path] = e
}

// A Reset that raced this callback moved the deadline on, and that later
// fire takes over. wg.Add runs under the lock so shutdown cannot miss a
// drainer started after the closing check.
func (w *Watcher) onDebounceFired(path string, e *debounceTimer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing || w.timers[path] != e || time.Now().Before(e.deadline) {
		return
	}
	delete(w.timers, path)
	w.queue = append(w.queue, queuedRun{path: path, run: e.run, ingest: e.ingest})
	if w.draining < cap(watcherSlots) {
		w.draining++
		w.wg.Add(1)
		go w.drain()
	}
}

// drain exits under the lock on an empty queue, so a run queued after
// that check starts a drainer of its own.
func (w *Watcher) drain() {
	defer w.wg.Done()
	for {
		if !w.acquireSlot() {
			w.mu.Lock()
			w.draining--
			w.mu.Unlock()
			return
		}
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.draining--
			w.mu.Unlock()
			<-watcherSlots
			return
		}
		next := w.queue[0]
		w.queue = w.queue[1:]
		if next.ingest {
			w.ingesting++
		}
		w.mu.Unlock()
		next.run(next.path)
		if next.ingest {
			w.mu.Lock()
			w.ingesting--
			w.mu.Unlock()
		}
		<-watcherSlots
	}
}

// acquireSlot is false once the watcher is closing, whether it was
// waiting or had just got a slot.
func (w *Watcher) acquireSlot() bool {
	select {
	case watcherSlots <- struct{}{}:
	case <-w.stop:
		return false
	}
	select {
	case <-w.stop:
		<-watcherSlots
		return false
	default:
		return true
	}
}

// A file or folder moved inside the tree is repointed by the ingests of
// its new paths, which a long walk or queue can hold past the grace.
func (w *Watcher) reconcileVanished(path string) {
	if w.jobSuppressesIngest() {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	if w.ingestsDue() {
		w.schedule(path, movedOutGrace, w.reconcileVanished, false)
		return
	}
	w.markFileMissing(path)
	w.markFolderMissing(path)
}

func (w *Watcher) WaitIngests(ctx context.Context) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		n := w.ingesting
		w.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (w *Watcher) ingestsDue() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ingesting > 0 || slices.ContainsFunc(w.queue, func(q queuedRun) bool { return q.ingest }) {
		return true
	}
	for _, e := range w.timers {
		if e.ingest {
			return true
		}
	}
	return false
}

func (w *Watcher) ingestFile(path string) {
	// Asked again, as the timer may have been armed before the folder was
	// fenced off.
	if w.jobSuppressesIngest() || w.boundary().Excludes(path) {
		return
	}
	if _, err := DetectFileType(path); err != nil {
		return
	}

	var maxMB int
	var naming Naming
	if w.Settings != nil {
		maxMB, naming = w.Settings()
	}
	if info, statErr := os.Stat(path); statErr == nil {
		if maxMB > 0 && info.Size() > int64(maxMB)*1024*1024 {
			logx.Warnf("watcher: skipping %q (size %d exceeds %d MB)",
				path, info.Size(), maxMB)
			return
		}
		if indexedUnchanged(w.handle.DB, path, info) {
			return
		}
	}

	img, out, err := ingestPath(w.handle, path, "")
	switch {
	case err != nil:
		logx.Warnf("watcher ingest %q: %v", path, err)
		if errors.Is(err, ErrRewritten) {
			w.event("conflict " + filepath.Base(path))
		}
		return
	case out == ingestDuplicate:
		logx.Infof("watcher: duplicate %q", path)
		return
	}
	verb := "added"
	switch out {
	case ingestCreated:
		if !naming.Empty() && img != nil {
			path = w.renameIngested(naming, img.ID, path)
		}
	case ingestMoved:
		verb = "moved"
	case ingestRestored:
		verb = "restored"
	case ingestEdited:
		verb = "re-read"
	}
	// A failed thumbnail after an edit leaves the phash NULL, and the
	// index must drop the old bytes' entry too.
	if out == ingestEdited {
		w.OnPhash.Stored(img.ID, img.Phash)
	} else {
		w.OnPhash.Landed(img)
	}
	logx.Infof("watcher: %s %q", verb, path)
	w.event(verb + " " + filepath.Base(path))
	w.changed()
}

// Sync's trust rule, so the event a landing or a rename raises does not
// hash the file again.
func indexedUnchanged(database *db.DB, path string, info os.FileInfo) bool {
	var k syncKnownEntry
	var missing int
	err := database.Read.QueryRow(
		`SELECT i.file_size, ip.mtime_unix, ip.mtime_nsec, i.is_missing FROM image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.path = ?`, path,
	).Scan(&k.size, &k.mtime, &k.mtimeNano, &missing)
	return err == nil && missing == 0 && k.unchanged(info.Size(), info.ModTime().Unix(), info.ModTime().UnixNano())
}

func (w *Watcher) renameIngested(naming Naming, id int64, path string) string {
	w.claimSelfMove(path)
	// Not cancellable, like the ingest before it: a rename cut short at
	// shutdown would strand the file half-filed.
	newPath, err := naming.ApplyExisting(context.Background(), w.db, w.boundary(), id)
	if err != nil {
		logx.Warnf("watcher: name %q: %v", path, err)
		return path
	}
	if newPath == "" {
		return path
	}
	w.claimSelfMove(newPath)
	return newPath
}

// Without this the image hides until a sync, and Prune missing images
// would drop its row and tags while a copy sits in the tree.
func (w *Watcher) promoteSurvivingCopy(id int64, gonePath string) bool {
	copies, err := AliasPathsFor(w.db.Read, []int64{id})
	if err != nil {
		logx.Warnf("watcher promote copy %d: list paths: %v", id, err)
		return false
	}
	for _, c := range copies[id] {
		p := c.Path
		if leftOut(w.boundary(), p) {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := ChangedSinceIndexed(p, c.Indexed); err != nil {
			logx.Warnf("watcher promote copy %d: %q %v - left alone", id, p, err)
			continue
		}
		if err := db.InWriteTx(w.db.Write, func(tx *sql.Tx) error {
			return repointCanonical(tx, id, p, FolderPath(w.galleryPath, p), gonePath, c.Mtime, c.MtimeNano)
		}); err != nil {
			logx.Warnf("watcher promote copy %d: %v", id, err)
			return false
		}
		logx.Infof("watcher: %q went, kept id=%d at %q", gonePath, id, p)
		w.changed()
		return true
	}
	return false
}

// A folder that leaves the tree reports only itself, so the files it held
// are judged here, all at once, as a sync would judge them.
func (w *Watcher) markFolderMissing(dir string) {
	if dir == w.galleryPath || !NamedInside(w.galleryPath, dir) {
		return
	}
	type entry struct {
		pathID, imageID int64
		path            string
		canonical       bool
	}
	entries, err := db.QueryAll(w.db.Read, db.ScanRow(func(e *entry) []any {
		return []any{&e.pathID, &e.imageID, &e.path, &e.canonical}
	}), `SELECT ip.id, ip.image_id, ip.path, ip.is_canonical
		FROM image_paths ip JOIN images i ON i.id = ip.image_id
		WHERE ip.path >= ? AND ip.path < ? AND (ip.is_canonical = 0 OR i.is_missing = 0)`,
		dir+string(filepath.Separator), dir+string(filepath.Separator+1))
	if err != nil {
		logx.Warnf("watcher: list %q: %v", dir, err)
		return
	}
	var stalePaths []int64
	var goneCanonicals []entry
	for _, e := range entries {
		if _, err := os.Stat(e.path); err == nil {
			continue
		}
		if e.canonical {
			goneCanonicals = append(goneCanonicals, e)
		} else {
			stalePaths = append(stalePaths, e.pathID)
		}
	}
	if err := db.Chunked(stalePaths, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		_, err := w.db.Write.Exec(`DELETE FROM image_paths WHERE id IN (`+in+`)`, args...)
		return err
	}); err != nil {
		logx.Warnf("watcher: drop paths under %q: %v", dir, err)
		return
	}
	var gone []int64
	for _, e := range goneCanonicals {
		if !w.promoteSurvivingCopy(e.imageID, e.path) {
			gone = append(gone, e.imageID)
		}
	}
	if len(gone) == 0 && len(stalePaths) == 0 {
		return
	}
	var marked int
	if err := db.InWriteTx(w.db.Write, func(tx *sql.Tx) error {
		var err error
		marked, err = setMissingTx(tx, gone, true)
		return err
	}); err != nil {
		logx.Warnf("watcher: mark missing under %q: %v", dir, err)
		return
	}
	logx.Infof("watcher: %q left, marked %d %s missing", dir, marked, models.Plural(marked, "image", "images"))
	if marked > 0 {
		w.event(fmt.Sprintf("removed %d %s in %s", marked, models.Plural(marked, "image", "images"), filepath.Base(dir)))
	}
	w.changed()
}

func (w *Watcher) markFileMissing(path string) {
	if !NamedInside(w.galleryPath, path) {
		return
	}

	var imgID int64
	err := w.db.Read.QueryRow(
		`SELECT id FROM images WHERE canonical_path = ? AND is_missing = 0`, path,
	).Scan(&imgID)
	if err != nil {
		err2 := w.db.Read.QueryRow(
			`SELECT ip.image_id FROM image_paths ip
			 JOIN images i ON i.id = ip.image_id
			 WHERE ip.path = ? AND ip.is_canonical = 1 AND i.is_missing = 0`, path,
		).Scan(&imgID)
		if err2 != nil {
			return
		}
	}

	if w.promoteSurvivingCopy(imgID, path) {
		return
	}

	if _, err := SetMissing(w.db, imgID, true); err != nil {
		logx.Warnf("watcher mark missing %q: %v", path, err)
		return
	}

	logx.Infof("watcher: marked missing %q (id=%d)", path, imgID)
	w.event("removed " + filepath.Base(path))
	w.changed()
}
