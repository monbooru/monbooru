// Package library owns the open galleries: the set that publishes, switches
// and changes them, and each one's database, services, caches and
// background work.
package library

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type Gallery struct {
	gallery.Handle

	RelationsSvc *relations.Service
	Degraded     bool

	// Resolved once at open: the built-in row never changes.
	GeneralCategoryID int64

	// A lazy read stamps what it stores with the generation it read before
	// its query, so a value taken before an invalidation cannot outlive it.
	cacheGen          atomic.Uint64
	phashGen          atomic.Uint64
	folderTreeAll     atomic.Pointer[stamped[[]gallery.FolderNode]]
	sourceLabelCounts atomic.Pointer[stamped[[]gallery.SourceLabelCount]]

	// Copy-on-write maps: readers hold no lock, so a stored map is never
	// mutated.
	inboxCountUnder        atomic.Pointer[stamped[map[string]int]]
	phashMissingUnder      atomic.Pointer[stamped[map[string]int]]
	folderTreeUnder        atomic.Pointer[stamped[map[string][]gallery.FolderNode]]
	sourceLabelCountsUnder atomic.Pointer[stamped[map[string][]gallery.SourceLabelCount]]

	bkTree  *relations.BKTree
	tallies *counts.Tallies
	cache   *search.AdjacencyCache

	// bgMu guards the watcher and the reclaimer: a set change starts them
	// after its lock, so a shutdown's Close can come in between.
	bgMu          sync.Mutex
	watcher       *gallery.Watcher
	watcherCancel context.CancelFunc
	watcherDone   chan struct{}

	mangaReclaim *gallery.MangaCacheReclaimer

	exports atomic.Int32
	closed  atomic.Bool
}

// Sync drops the caches itself, so no caller has to remember InvalidateCaches.
// changed runs before a progress tick that follows rows changing.
func (cx *Gallery) Sync(ctx context.Context, maxFileSizeMB int, naming gallery.Naming, progress func(processed, total int, message string), changed func()) (gallery.SyncResult, error) {
	onChange := func() {
		cx.InvalidateCaches()
		changed()
	}
	cx.WaitIngests(ctx)
	result, err := gallery.Sync(ctx, cx.Handle, maxFileSizeMB, naming, progress, onChange, cx.phashSink())
	cx.InvalidateCaches()
	return result, err
}

// InvalidateCaches must follow every write that changes image membership.
func (cx *Gallery) InvalidateCaches() {
	if cx == nil {
		return
	}
	cx.cacheGen.Add(1)
	cx.phashGen.Add(1)
	cx.folderTreeAll.Store(nil)
	cx.sourceLabelCounts.Store(nil)
	cx.inboxCountUnder.Store(nil)
	cx.phashMissingUnder.Store(nil)
	cx.folderTreeUnder.Store(nil)
	cx.sourceLabelCountsUnder.Store(nil)
	if cx.tallies != nil {
		cx.tallies.Invalidate()
	}
	cx.cache.Drop()
}

type stamped[V any] struct {
	v   V
	gen uint64
}

func cachedValue[V any](gen *atomic.Uint64, slot *atomic.Pointer[stamped[V]], query func() (V, error)) (V, error) {
	g := gen.Load()
	if p := slot.Load(); p != nil && p.gen == g {
		return p.v, nil
	}
	v, err := query()
	if err != nil {
		var zero V
		return zero, err
	}
	slot.Store(&stamped[V]{v: v, gen: g})
	return v, nil
}

func (cx *Gallery) folderTree() ([]gallery.FolderNode, error) {
	return cachedValue(&cx.cacheGen, &cx.folderTreeAll, func() ([]gallery.FolderNode, error) { return gallery.FolderTree(cx.DB) })
}

func (cx *Gallery) SourceLabelCounts() ([]gallery.SourceLabelCount, error) {
	return cachedValue(&cx.cacheGen, &cx.sourceLabelCounts, func() ([]gallery.SourceLabelCount, error) {
		return gallery.SourceLabelCountsUnderQuery(cx.DB, 25, gallery.RankCeiling{})
	})
}

func (cx *Gallery) VisibleCount() (int, bool) { return cx.tallies.VisibleCount() }
func (cx *Gallery) InboxCount() (int, bool)   { return cx.tallies.InboxCount() }
func (cx *Gallery) TagCount() (int, bool)     { return cx.tallies.TagCount() }

func (cx *Gallery) CollectionsCount() (int, bool) { return cx.tallies.CollectionsCount() }

func okErr(n int, ok bool) (int, error) {
	if !ok {
		return 0, errCountUnavailable
	}
	return n, nil
}

var errCountUnavailable = errors.New("count unavailable")

func lookupByCeiling[V any](slot *atomic.Pointer[stamped[map[string]V]], gen uint64, level string) (V, bool) {
	if m := slot.Load(); m != nil && m.gen == gen {
		if v, ok := m.v[level]; ok {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func storeByCeiling[V any](slot *atomic.Pointer[stamped[map[string]V]], gen uint64, level string, value V) {
	for {
		current := slot.Load()
		if current != nil && current.gen > gen {
			return
		}
		newMap := make(map[string]V, 4)
		if current != nil && current.gen == gen {
			for k, v := range current.v {
				newMap[k] = v
			}
		}
		newMap[level] = value
		if slot.CompareAndSwap(current, &stamped[map[string]V]{v: newMap, gen: gen}) {
			return
		}
	}
}

func ceilingCached[V any](c *Ceiling, blind func() (V, error), gen *atomic.Uint64, slot *atomic.Pointer[stamped[map[string]V]], query func() (V, error)) (V, error) {
	if c == nil || !c.IsActive() {
		return blind()
	}
	g := gen.Load()
	if v, ok := lookupByCeiling(slot, g, c.level); ok {
		return v, nil
	}
	v, err := query()
	if err != nil {
		var zero V
		return zero, err
	}
	storeByCeiling(slot, g, c.level, v)
	return v, nil
}

func (cx *Gallery) InboxCountUnder(c *Ceiling) (int, error) {
	return ceilingCached(c, func() (int, error) { return okErr(cx.InboxCount()) }, &cx.cacheGen, &cx.inboxCountUnder,
		func() (int, error) { return gallery.InboxCountUnder(cx.DB, c.RankFilter()) })
}

func (cx *Gallery) PhashMissingUnder(c *Ceiling) (int, error) {
	return ceilingCached(c,
		func() (int, error) { return okErr(cx.tallies.PhashMissing()) },
		&cx.phashGen, &cx.phashMissingUnder,
		func() (int, error) { return gallery.PhashMissingUnder(cx.DB, c.RankFilter()) })
}

// invalidatePhashMissing is for phash writes: they move no image in or out
// of the folder tree or the tag counts.
func (cx *Gallery) invalidatePhashMissing() {
	if cx == nil {
		return
	}
	cx.phashGen.Add(1)
	cx.tallies.InvalidatePhashMissing()
	cx.phashMissingUnder.Store(nil)
	cx.cache.Drop()
}

func (cx *Gallery) FolderTreeUnder(c *Ceiling) ([]gallery.FolderNode, error) {
	return ceilingCached(c, cx.folderTree, &cx.cacheGen, &cx.folderTreeUnder,
		func() ([]gallery.FolderNode, error) { return gallery.FolderTreeUnder(cx.DB, c.RankFilter()) })
}

func (cx *Gallery) SourceLabelCountsUnder(c *Ceiling) ([]gallery.SourceLabelCount, error) {
	return ceilingCached(c, cx.SourceLabelCounts, &cx.cacheGen, &cx.sourceLabelCountsUnder,
		func() ([]gallery.SourceLabelCount, error) {
			return gallery.SourceLabelCountsUnderQuery(cx.DB, 25, c.RankFilter())
		})
}

func (cx *Gallery) warmCaches() {
	if cx == nil || cx.DB == nil {
		return
	}
	cx.SourceLabelCounts() //nolint:errcheck
	cx.VisibleCount()
	cx.InboxCount()
	cx.TagCount()
	cx.CollectionsCount()
}

var processStart = time.Now()

func Open(g config.Gallery) (*Gallery, error) {
	if dbDir := filepath.Dir(g.DBPath); dbDir != "" && dbDir != "." {
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return nil, fmt.Errorf("gallery %q: create db dir: %w", g.Name, err)
		}
	}
	database, err := db.Open(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("gallery %q: open db: %w", g.Name, err)
	}
	if err := db.Bootstrap(database); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: bootstrap db: %w", g.Name, err)
	}
	if err := respellRelativeRows(database, g.GalleryPath); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: respell relative paths: %w", g.Name, err)
	}
	if err := os.MkdirAll(g.ThumbnailsPath, 0o755); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: create thumbnails dir: %w", g.Name, err)
	}
	gallery.SweepStaleTemps(filepath.Dir(g.DBPath), processStart)
	degraded := false
	if _, err := os.ReadDir(g.GalleryPath); err != nil {
		logx.Warnf("gallery %q: path %q unreadable: %v - degraded mode", g.Name, g.GalleryPath, err)
		degraded = true
	}
	cx, err := New(gallery.Handle{
		Name:           g.Name,
		GalleryPath:    g.GalleryPath,
		ThumbnailsPath: g.ThumbnailsPath,
		DBPath:         g.DBPath,
		DB:             database,
		Bounds:         new(atomic.Pointer[gallery.Boundary]),
	})
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	cx.Degraded = degraded
	return cx, nil
}

// New wires a gallery around h's open, bootstrapped database, with its
// own tag service.
func New(h gallery.Handle) (*Gallery, error) {
	generalID, ok, err := tags.CategoryIDByName(h.DB, "general")
	if err == nil && !ok {
		err = sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("gallery %q: resolve general category: %w", h.Name, err)
	}
	cx := &Gallery{
		Handle:            h,
		GeneralCategoryID: generalID,
		bkTree:            relations.NewBKTree(),
		tallies:           counts.New(h.DB),
		cache:             search.NewAdjacencyCache(),
	}
	cx.TagSvc = tags.New(h.DB, cx.InvalidateCaches)
	cx.RelationsSvc = relations.New(h.DB, cx.TagSvc, cx.InvalidateCaches)
	return cx, nil
}

func (cx *Gallery) Source() search.Source {
	return search.Source{DB: cx.DB, Tree: cx.bkTree, Tallies: cx.tallies, Cache: cx.cache}
}

func (cx *Gallery) phashSink() gallery.PhashSink { return relations.PhashSink(cx.DB, cx.bkTree) }

func (cx *Gallery) mangaCacheDir() string {
	if cx == nil {
		return ""
	}
	return gallery.MangaCacheDir(cx.ThumbnailsPath)
}

// Close leaves cx.DB set: an unjoined warmCaches may still read it, and a
// closed pool errors where a nil one would panic.
func (cx *Gallery) Close() {
	cx.closed.Store(true)
	cx.bgMu.Lock()
	cx.stopWatcher()
	cx.stopMangaReclaim()
	cx.bgMu.Unlock()
	cx.cache.Drop()
	if cx.DB != nil {
		_ = cx.DB.Close()
	}
}

// Closed reports a member an import or a refused rename closed and could
// not reopen: it stays in the set, so the import can be retried or a
// switch can reopen it.
func (cx *Gallery) Closed() bool { return cx.closed.Load() }

// An export streams without the gallery lock, so whatever closes the
// gallery must check exporting first.
func (cx *Gallery) BeginExport()    { cx.exports.Add(1) }
func (cx *Gallery) EndExport()      { cx.exports.Add(-1) }
func (cx *Gallery) exporting() bool { return cx.exports.Load() > 0 }

// ShrinkMemory skips an exporting gallery: the export holds a read
// connection the shrink would wait on for the whole stream.
func (cx *Gallery) ShrinkMemory() error {
	if cx.exporting() {
		return nil
	}
	return cx.DB.ShrinkMemory(context.Background())
}

// Optimize skips an exporting gallery for the reason ShrinkMemory does.
func (cx *Gallery) Optimize() error {
	if cx.exporting() {
		return nil
	}
	return cx.DB.Optimize(context.Background())
}

func (cx *Gallery) ReleaseIdle(idle time.Duration) (dropped bool, err error) {
	dropped = cx.tallies.ReleaseIdleCountedTags(idle)
	if cx.bkTree.ReleaseIdle(idle) {
		dropped = true
	}
	err = cmp.Or(cx.Optimize(), cx.ShrinkMemory())
	cx.cache.Sweep()
	return dropped, err
}

func (cx *Gallery) exportBusy() error {
	if cx.exporting() {
		return fmt.Errorf("gallery %q is being exported; try again when the export finishes", cx.Name)
	}
	return nil
}

// WatcherRunning and ReclaimerRunning exist for other packages' tests.
func (cx *Gallery) WatcherRunning() bool   { return cx != nil && cx.watcherCancel != nil }
func (cx *Gallery) ReclaimerRunning() bool { return cx != nil && cx.mangaReclaim != nil }

// startBackground reads watch once; settings is read again at each ingest.
func (cx *Gallery) startBackground(settings func() Background, jm *jobs.Manager) {
	cx.bgMu.Lock()
	defer cx.bgMu.Unlock()
	if cx.Closed() {
		return
	}
	cx.startWatcher(settings, jm)
	cx.startMangaReclaim()
	cx.indexWorkflows()
}

// Outside the job slot: that slot is single and scoped to the active
// gallery, and every opened gallery runs this.
func (cx *Gallery) indexWorkflows() {
	if cx.DB == nil {
		return
	}
	pending, err := gallery.ComfyTermsPending(cx.DB)
	if err != nil || pending == 0 {
		return
	}
	go func() {
		indexed, err := gallery.BackfillComfyTerms(context.Background(), cx.DB, nil)
		if indexed > 0 {
			cx.InvalidateCaches()
		}
		if err != nil {
			logx.Warnf("gallery %q: workflow index: %v", cx.Name, err)
			return
		}
		logx.Infof("gallery %q: indexed %d %s", cx.Name, indexed, models.Plural(indexed, "workflow", "workflows"))
	}()
}

func (cx *Gallery) startWatcher(settings func() Background, jm *jobs.Manager) {
	if !settings().Watch || cx.Degraded || cx.watcherCancel != nil {
		return
	}
	w, err := gallery.NewWatcher(cx.Handle, jm)
	if err != nil {
		logx.Warnf("gallery %q: watcher start: %v", cx.Name, err)
		return
	}
	w.Settings = func() (int, gallery.Naming) {
		b := settings()
		return b.MaxFileSizeMB, b.Naming
	}
	w.OnEvent = jm.SetWatcherMessage
	w.OnChange = cx.InvalidateCaches
	w.OnPhash = cx.phashSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cx.watcher = w
	cx.watcherCancel = cancel
	cx.watcherDone = done
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			logx.Warnf("gallery %q: watcher stopped: %v", cx.Name, err)
		}
	}()
	logx.Infof("gallery %q: watcher started", cx.Name)
}

// restartWatcher is for a folder handed back; the event filter already
// follows a folder fenced off.
func (cx *Gallery) restartWatcher(settings func() Background, jm *jobs.Manager) {
	cx.bgMu.Lock()
	defer cx.bgMu.Unlock()
	if cx.Closed() {
		return
	}
	cx.stopWatcher()
	cx.startWatcher(settings, jm)
}

func (cx *Gallery) stopWatcher() {
	if cx.watcherCancel == nil {
		return
	}
	cx.watcherCancel()
	<-cx.watcherDone
	cx.watcher = nil
	cx.watcherCancel = nil
	cx.watcherDone = nil
}

// WaitIngests is for a job that suppresses the watcher's ingests: one already
// past its check could otherwise rename a file under the job.
func (cx *Gallery) WaitIngests(ctx context.Context) {
	cx.bgMu.Lock()
	w := cx.watcher
	cx.bgMu.Unlock()
	if w != nil {
		_ = w.WaitIngests(ctx)
	}
}

func (cx *Gallery) startMangaReclaim() {
	if cx.mangaReclaim != nil {
		return
	}
	r := gallery.NewMangaCacheReclaimer(cx.mangaCacheDir())
	r.Start(context.Background())
	cx.mangaReclaim = r
}

func (cx *Gallery) stopMangaReclaim() {
	if cx.mangaReclaim == nil {
		return
	}
	cx.mangaReclaim.Stop()
	cx.mangaReclaim = nil
}
