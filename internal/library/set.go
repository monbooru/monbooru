package library

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// A read request holds the read lock for its whole handler and loads the
// published members unlocked: a nested RLock queues behind a pending writer,
// which waits on the outer lock. A change publishes a copy under the write lock.
type Set struct {
	sync.RWMutex
	state atomic.Pointer[setState]
	host  Host
	jobs  *jobs.Manager
	// Serialises Redraw so an older drawing cannot be stored last.
	boundsMu sync.Mutex
}

type setState struct {
	members map[string]*Gallery
	active  string
}

type Host interface {
	Layout() Layout
	Background(name string) Background
	DefaultGallery() string
	// Added, Removed, Repointed and Renamed save a change under the set's
	// lock before it goes live, undoing their edit if the save fails: a restart
	// on a disagreeing file opens a removed gallery empty or drops an added one.
	Added(g config.Gallery) error
	Removed(name string) error
	Repointed(name, galleryPath string) error
	Renamed(oldName, newName, dbPath, thumbnailsPath string) error
}

type Background struct {
	Watch         bool
	MaxFileSizeMB int
	Naming        gallery.Naming
}

// ErrJobRunning: a change holds the job lane, not just checks it. Batch
// routes claim the lane under the read lock, so a job can start between a
// check and the write lock.
var ErrJobRunning = errors.New("a job is running; try again when it finishes")

// OpenSet closes what it opened when one gallery fails to open.
func OpenSet(host Host, jm *jobs.Manager, galleries []config.Gallery, active string) (*Set, error) {
	st := &setState{members: make(map[string]*Gallery, len(galleries)), active: active}
	for _, g := range galleries {
		cx, err := Open(g)
		if err != nil {
			for _, open := range st.members {
				open.Close()
			}
			return nil, err
		}
		st.members[g.Name] = cx
	}
	s := &Set{host: host, jobs: jm}
	s.state.Store(st)
	s.Redraw()
	layout := host.Layout()
	for i, a := range galleries {
		for _, b := range galleries[i+1:] {
			if gallery.SameFolder(a.GalleryPath, b.GalleryPath) {
				logx.Warnf("galleries %q and %q share the folder %q: each indexes all of it", a.Name, b.Name, a.GalleryPath)
			}
		}
		if err := layout.inOwnFolder(a.GalleryPath); err != nil {
			logx.Warnf("gallery %q: %v", a.Name, err)
		}
	}
	return s, nil
}

func (s *Set) ActiveName() string { return s.state.Load().active }

func (s *Set) Active() *Gallery {
	st := s.state.Load()
	return st.members[st.active]
}

func (s *Set) Get(name string) *Gallery { return s.state.Load().members[name] }

func (s *Set) All() []*Gallery { return slices.Collect(maps.Values(s.state.Load().members)) }

func (s *Set) Names() []string { return slices.Sorted(maps.Keys(s.state.Load().members)) }

// publish makes a changed copy of the members current; the caller holds
// the write lock.
func (s *Set) publish(change func(members map[string]*Gallery, active *string)) {
	cur := s.state.Load()
	next := &setState{members: maps.Clone(cur.members), active: cur.active}
	change(next.members, &next.active)
	s.state.Store(next)
}

// start walks the whole tree: a change runs it after the lock, holding only
// the job lane.
func (s *Set) start(cx *Gallery) {
	cx.startBackground(s.background(cx.Name), s.jobs)
}

// A rename reopens the gallery, so the captured name does not go stale.
func (s *Set) background(name string) func() Background {
	return func() Background { return s.host.Background(name) }
}

// StartBackground watches every member, not just the active one, and
// warms each one's caches.
func (s *Set) StartBackground() {
	s.Lock()
	defer s.Unlock()
	for _, cx := range s.All() {
		s.start(cx)
		go cx.warmCaches()
	}
}

func (s *Set) Close() {
	s.Lock()
	defer s.Unlock()
	for _, cx := range s.All() {
		cx.Close()
	}
}

// Redraw fences every member off from the folders it must leave out.
func (s *Set) Redraw() {
	s.boundsMu.Lock()
	defer s.boundsMu.Unlock()
	layout := s.host.Layout()
	drawn := s.draw(layout, layout.Ignore)
	for _, cx := range s.All() {
		if b, ok := drawn[cx.Name]; ok {
			cx.Bounds.Store(b)
		}
	}
}

// Draw takes ignore apart from the configuration so a new list can be
// checked before it is stored.
func (s *Set) Draw(ignore []string) map[string]*gallery.Boundary {
	return s.draw(s.host.Layout(), ignore)
}

func (s *Set) draw(layout Layout, ignore []string) map[string]*gallery.Boundary {
	own := []gallery.Fence{
		{Path: layout.DataPath, Kind: gallery.FenceData},
		{Path: layout.ModelPath, Kind: gallery.FenceModels},
		{Path: layout.ThemesDir, Kind: gallery.FenceThemes},
		{Path: layout.PluginsDir, Kind: gallery.FencePlugins},
	}
	// A gallery mounted under data_path is not fenced off by it, but what
	// monbooru writes there still has to stay out of that gallery.
	for _, g := range layout.Galleries {
		own = append(own,
			gallery.Fence{Path: g.DBPath, Kind: gallery.FenceData},
			gallery.Fence{Path: g.DBPath + "-wal", Kind: gallery.FenceData},
			gallery.Fence{Path: g.DBPath + "-shm", Kind: gallery.FenceData},
			gallery.Fence{Path: g.ThumbnailsPath, Kind: gallery.FenceData},
			gallery.Fence{Path: gallery.MangaCacheDir(g.ThumbnailsPath), Kind: gallery.FenceData})
	}
	drawn := map[string]*gallery.Boundary{}
	for _, cx := range s.All() {
		fences := slices.Clone(own)
		for _, g := range layout.Galleries {
			if g.Name != cx.Name {
				fences = append(fences, gallery.Fence{Path: g.GalleryPath, Owner: g.Name, Kind: gallery.FenceGallery})
			}
		}
		drawn[cx.Name] = gallery.NewBoundary(cx.GalleryPath, fences, ignore)
	}
	return drawn
}

// restartEnclosing restarts the watchers that saw nothing of dir while it
// was fenced off. The caller holds the job lane rather than the lock: a
// restart walks the whole tree.
func (s *Set) restartEnclosing(dir string) {
	for _, cx := range s.All() {
		if gallery.Encloses(cx.GalleryPath, dir) {
			cx.restartWatcher(s.background(cx.Name), s.jobs)
		}
	}
}

func (s *Set) Switch(name string) error {
	r, err := s.jobs.Reserve()
	if err != nil {
		return ErrJobRunning
	}
	defer r.Release()
	reopened, err := s.switchMember(name)
	if reopened != nil {
		s.start(reopened)
		go reopened.warmCaches()
	}
	return err
}

// switchMember returns the member it reopened: one an import or a refused
// rename closed and could not reopen.
func (s *Set) switchMember(name string) (*Gallery, error) {
	s.Lock()
	defer s.Unlock()
	cx := s.Get(name)
	if cx == nil {
		return nil, fmt.Errorf("unknown gallery %q", name)
	}
	var reopened *Gallery
	if cx.Closed() {
		var err error
		reopened, err = Open(config.Gallery{Name: name, GalleryPath: cx.GalleryPath, DBPath: cx.DBPath, ThumbnailsPath: cx.ThumbnailsPath})
		if err != nil {
			return nil, fmt.Errorf("gallery %q is closed and could not be reopened: %w", name, err)
		}
		s.publish(func(members map[string]*Gallery, _ *string) { members[name] = reopened })
		s.Redraw()
		logx.Infof("gallery: reopened %q", name)
	}
	old := s.ActiveName()
	if name == old {
		return reopened, nil
	}
	prev := s.Active()
	s.publish(func(_ map[string]*Gallery, active *string) { *active = name })
	// Only the active gallery's lists are worth their memory: a switch
	// back refills them.
	if prev != nil {
		prev.cache.Drop()
	}
	logx.Infof("gallery: switched from %q to %q", old, name)
	return reopened, nil
}

// Add refuses a folder it cannot read, which a configured gallery still
// opens on, degraded.
func (s *Set) Add(name, galleryPath string) error {
	if err := config.ValidateGalleryName(name); err != nil {
		return err
	}
	if galleryPath == "" {
		return fmt.Errorf("gallery path must not be empty")
	}
	galleryPath = filepath.Clean(galleryPath)
	if !filepath.IsAbs(galleryPath) {
		return fmt.Errorf("the gallery folder must be an absolute path")
	}
	if _, err := os.ReadDir(galleryPath); err != nil {
		return fmt.Errorf("gallery path %q is not readable: %w", galleryPath, err)
	}
	r, err := s.jobs.Reserve()
	if err != nil {
		return ErrJobRunning
	}
	defer r.Release()
	cx, err := s.addMember(name, galleryPath)
	if err != nil {
		return err
	}
	s.start(cx)
	return nil
}

func (s *Set) addMember(name, galleryPath string) (*Gallery, error) {
	s.Lock()
	defer s.Unlock()
	if s.Get(name) != nil {
		return nil, fmt.Errorf("gallery %q already exists", name)
	}
	layout := s.host.Layout()
	if other := layout.galleryOnFolder(galleryPath, ""); other != "" {
		return nil, fmt.Errorf("%s is already the folder of gallery %s", galleryPath, other)
	}
	if err := layout.inOwnFolder(galleryPath, name); err != nil {
		return nil, err
	}
	if dir, what := layout.dataFolderHolds(name); what != "" {
		return nil, fmt.Errorf("a gallery named %s would keep its data in %s, which holds %s", name, dir, what)
	}
	dbPath, thumbnailsPath := layout.DerivePaths(name)
	if _, err := os.Stat(dbPath); err == nil {
		return nil, fmt.Errorf("data for gallery %q already exists at %q", name, filepath.Dir(dbPath))
	}
	g := config.Gallery{Name: name, GalleryPath: galleryPath, DBPath: dbPath, ThumbnailsPath: thumbnailsPath}
	discard := func() {
		removeOwnData(dbPath, thumbnailsPath)
		_ = os.Remove(filepath.Dir(dbPath))
	}
	cx, err := Open(g)
	if err != nil {
		discard()
		return nil, err
	}
	if err := s.host.Added(g); err != nil {
		cx.Close()
		discard()
		return nil, fmt.Errorf("persist new gallery: %w", err)
	}
	s.publish(func(members map[string]*Gallery, _ *string) { members[name] = cx })
	s.Redraw()
	return cx, nil
}

// Remove deletes only what monbooru wrote for the gallery, plus its folder
// when removeFolder is set; kept lists what that folder still held.
func (s *Set) Remove(name string, removeFolder bool) (kept []string, err error) {
	r, err := s.jobs.Reserve()
	if err != nil {
		return nil, ErrJobRunning
	}
	defer r.Release()
	cx, bound, err := s.removeMember(name, removeFolder)
	if err != nil {
		return nil, err
	}
	galleryPath := cx.GalleryPath
	dataDir := filepath.Dir(cx.DBPath)
	removeOwnData(cx.DBPath, cx.ThumbnailsPath)
	if removeFolder {
		kept = removeGalleryFolder(galleryPath, bound)
	}
	if err := os.Remove(dataDir); err != nil && !os.IsNotExist(err) {
		logx.Infof("gallery data dir %q kept: %v", dataDir, err)
	}
	s.restartEnclosing(galleryPath)
	return kept, nil
}

func (s *Set) removeMember(name string, removeFolder bool) (*Gallery, *gallery.Boundary, error) {
	s.Lock()
	defer s.Unlock()
	cx := s.Get(name)
	if cx == nil {
		return nil, nil, fmt.Errorf("unknown gallery %q", name)
	}
	if name == s.ActiveName() {
		return nil, nil, fmt.Errorf("cannot remove the active gallery; switch to another first")
	}
	if name == s.host.DefaultGallery() {
		return nil, nil, fmt.Errorf("cannot remove the default gallery; set another as default first")
	}
	if len(s.Names()) <= 1 {
		return nil, nil, fmt.Errorf("cannot remove the last gallery")
	}
	if err := cx.exportBusy(); err != nil {
		return nil, nil, err
	}
	if other := cx.Boundary().SharedWith(); removeFolder && other != "" {
		return nil, nil, fmt.Errorf("gallery %q indexes the same folder; remove this gallery without its folder", other)
	}
	if err := s.host.Removed(name); err != nil {
		return nil, nil, fmt.Errorf("persist gallery removal: %w", err)
	}
	bound := cx.Boundary()
	cx.Close()
	s.publish(func(members map[string]*Gallery, _ *string) { delete(members, name) })
	s.Redraw()
	return cx, bound, nil
}

// removeOwnData deletes only what monbooru wrote: a gallery folder can sit
// in the data directory.
func removeOwnData(dbPath, thumbnailsPath string) {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", thumbnailsPath, gallery.MangaCacheDir(thumbnailsPath)} {
		if err := os.RemoveAll(p); err != nil {
			logx.Warnf("remove gallery data %q: %v", p, err)
		}
	}
}

// Not through a symlink: the link could resolve to any directory.
func removeGalleryFolder(galleryPath string, bound *gallery.Boundary) []string {
	info, err := os.Lstat(galleryPath)
	if err != nil {
		logx.Warnf("remove gallery folder %q: stat: %v", galleryPath, err)
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		logx.Warnf("remove gallery folder %q: refusing to follow symlink", galleryPath)
		return nil
	}
	kept, err := gallery.RemoveOwned(bound)
	if err != nil {
		logx.Warnf("remove gallery folder %q: %v", galleryPath, err)
		return nil
	}
	if len(kept) == 0 {
		if err := os.Remove(galleryPath); err != nil {
			logx.Warnf("remove gallery folder %q: %v", galleryPath, err)
		}
		return nil
	}
	// Other galleries' folders go first: what the ignore list kept can be
	// a sidecar per image.
	fenced := map[string]bool{}
	for _, e := range bound.Fenced() {
		fenced[e.Rel] = true
	}
	var first, rest []string
	for _, k := range kept {
		if fenced[k] {
			first = append(first, k)
		} else {
			rest = append(rest, k)
		}
	}
	kept = append(first, rest...)
	logx.Infof("remove gallery folder %q: kept %s", galleryPath, KeptList(kept))
	return kept
}

func KeptList(kept []string) string {
	const shown = 5
	if len(kept) <= shown {
		return strings.Join(kept, ", ")
	}
	return fmt.Sprintf("%s (and %d more)", strings.Join(kept[:shown], ", "), len(kept)-shown)
}

// Repoint reopens rather than edits in place: the gallery caches the path
// and the watcher holds it open. It reports whether anything changed.
func (s *Set) Repoint(name, galleryPath string) (bool, error) {
	galleryPath = filepath.Clean(galleryPath)
	if galleryPath == "" || !filepath.IsAbs(galleryPath) {
		return false, fmt.Errorf("the gallery folder must be an absolute path")
	}
	if _, err := os.ReadDir(galleryPath); err != nil {
		return false, fmt.Errorf("gallery path %q is not readable: %w", galleryPath, err)
	}
	r, err := s.jobs.Reserve()
	if err != nil {
		return false, ErrJobRunning
	}
	defer r.Release()
	newCx, oldPath, err := s.repointMember(name, galleryPath)
	if err != nil || newCx == nil {
		return false, err
	}
	s.start(newCx)
	s.restartEnclosing(oldPath)
	return true, nil
}

// repointMember returns no gallery when the path is unchanged.
func (s *Set) repointMember(name, galleryPath string) (*Gallery, string, error) {
	s.Lock()
	defer s.Unlock()
	cx := s.Get(name)
	if cx == nil {
		return nil, "", fmt.Errorf("unknown gallery %q", name)
	}
	if cx.GalleryPath == galleryPath {
		return nil, "", nil
	}
	layout := s.host.Layout()
	if other := layout.galleryOnFolder(galleryPath, name); other != "" {
		return nil, "", fmt.Errorf("%s is already the folder of gallery %s", galleryPath, other)
	}
	if err := layout.inOwnFolder(galleryPath); err != nil {
		return nil, "", err
	}
	if err := cx.exportBusy(); err != nil {
		return nil, "", err
	}
	// Opened before the old one closes, so a failed open leaves the
	// gallery as it was.
	newCx, err := Open(config.Gallery{Name: name, GalleryPath: galleryPath, DBPath: cx.DBPath, ThumbnailsPath: cx.ThumbnailsPath})
	if err != nil {
		return nil, "", err
	}
	if err := s.host.Repointed(name, galleryPath); err != nil {
		newCx.Close()
		return nil, "", fmt.Errorf("persist gallery path: %w", err)
	}
	oldPath := cx.GalleryPath
	cx.Close()
	s.publish(func(members map[string]*Gallery, _ *string) { members[name] = newCx })
	s.Redraw()
	return newCx, oldPath, nil
}

// Replace holds the job lane rather than the lock: a held lock would freeze
// every request for minutes. stage runs in the open gallery's data folder and
// apply calls quiesce before its first write, so a refusal before then leaves
// the gallery as it was; once quiesced it reopens even after a failed apply.
func (s *Set) Replace(name string, stage func(dataDir string) error, apply func(dbPath, thumbnailsPath string, bound *gallery.Boundary, quiesce func() error) error) (*Gallery, error) {
	r, err := s.jobs.Reserve()
	if err != nil {
		return nil, ErrJobRunning
	}
	defer r.Release()
	cx, g, bound, err := s.replaceTarget(name)
	if err != nil {
		return nil, err
	}
	if err := stage(filepath.Dir(g.DBPath)); err != nil {
		return nil, err
	}
	quiesced := false
	applyErr := apply(g.DBPath, g.ThumbnailsPath, bound, func() error {
		if err := s.quiesce(cx); err != nil {
			return err
		}
		quiesced = true
		return nil
	})
	if !quiesced {
		return cx, applyErr
	}
	newCx, openErr := Open(g)
	if openErr != nil {
		if applyErr != nil {
			return nil, fmt.Errorf("import failed: %w (reopen also failed: %v)", applyErr, openErr)
		}
		return nil, fmt.Errorf("reopen gallery: %w", openErr)
	}
	s.Lock()
	s.publish(func(members map[string]*Gallery, _ *string) { members[name] = newCx })
	s.Redraw()
	s.Unlock()
	s.start(newCx)
	go newCx.warmCaches()
	return newCx, applyErr
}

func (s *Set) replaceTarget(name string) (*Gallery, config.Gallery, *gallery.Boundary, error) {
	s.Lock()
	defer s.Unlock()
	cx := s.Get(name)
	if cx == nil {
		return nil, config.Gallery{}, nil, fmt.Errorf("unknown gallery %q", name)
	}
	if name == s.ActiveName() {
		return nil, config.Gallery{}, nil, fmt.Errorf("cannot import over the active gallery; switch to another first")
	}
	if name == s.host.DefaultGallery() {
		return nil, config.Gallery{}, nil, fmt.Errorf("cannot import over the default gallery; set another as default first")
	}
	g := config.Gallery{Name: name, GalleryPath: cx.GalleryPath, DBPath: cx.DBPath, ThumbnailsPath: cx.ThumbnailsPath}
	return cx, g, cx.Boundary(), nil
}

func (s *Set) quiesce(cx *Gallery) error {
	s.Lock()
	defer s.Unlock()
	if err := cx.exportBusy(); err != nil {
		return err
	}
	cx.Close()
	return nil
}
