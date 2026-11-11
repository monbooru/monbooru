// Package web is the HTTP transport (routes, handlers, templates) and the
// process's composition root: NewServer opens the set and starts every loop.
package web

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/registry"
	"github.com/monbooru/monbooru/internal/schedule"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/shared/webui"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
	webFS "github.com/monbooru/monbooru/web"
)

func groupOrdered[T, G any](items []T, skip func(T) bool, key func(T) string, newGroup func(T) *G, add func(*G, T)) []G {
	order := []string{}
	groups := map[string]*G{}
	for _, t := range items {
		if skip != nil && skip(t) {
			continue
		}
		k := key(t)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
			groups[k] = newGroup(t)
		}
		add(groups[k], t)
	}
	out := make([]G, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

type tagGroup struct {
	Name  string
	Color string
	Tags  []models.Tag
}

type Server struct {
	cfg        *config.Config
	configPath string
	cfgMu      sync.RWMutex // guards cfg reads/writes and config.Save calls
	jobs       *jobs.Manager
	pairs      *peer.Store
	sessions   *session.Store
	loginRL    *session.Backoff
	tmpl       *template.Template
	staticFS   fs.FS
	lightFS    fs.FS
	done       chan struct{}

	desktop bool
	// A field so a test can run the handler without an opener popping a window.
	folderOpener func(string) error
	logDir       string
	quit         chan struct{}
	quitOnce     sync.Once
	restart      atomic.Bool
	// A field so a test drives the menu and start-at-login entries under its
	// own name: the HKCU Run value cannot be redirected like HOME.
	desktopApp string

	set *library.Set

	sched *schedule.Scheduler

	ml *monloader.Link

	themeWarn *themeWarnings

	peers *plugins.Peers

	downloads downloadList
}

// Desktop.LogDir is not derived from the config: the command opens the
// log before the config loads, and data_path may point elsewhere.
type Desktop struct {
	Active bool
	LogDir string
}

func NewServer(cfg *config.Config, configPath string, jobManager *jobs.Manager, dk Desktop) (*Server, error) {
	sessions := session.New()
	if configPath != "" {
		metadata.SetNodeOverlay(filepath.Join(filepath.Dir(configPath), "comfyui_nodes.json"))
	}

	funcs := templateFuncs()
	maps.Copy(funcs, webui.Funcs(assetVersion))
	tmpl, err := template.New("").Funcs(funcs).ParseFS(webFS.FS, "templates/*.html", "templates/partials/*.html")
	if err == nil {
		tmpl, err = tmpl.ParseFS(webui.Templates, "*.html")
	}
	if err != nil {
		return nil, err
	}

	staticFS, err := fs.Sub(webFS.FS, "static")
	if err != nil {
		return nil, err
	}
	lightFS, err := fs.Sub(registry.FS, builtinLightDir)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:          cfg,
		configPath:   configPath,
		jobs:         jobManager,
		pairs:        peer.NewStore(),
		sessions:     sessions,
		loginRL:      session.NewBackoff(),
		tmpl:         tmpl,
		staticFS:     staticFS,
		lightFS:      lightFS,
		done:         make(chan struct{}),
		desktop:      dk.Active,
		folderOpener: desktop.OpenFolder,
		desktopApp:   "monbooru",
		logDir:       dk.LogDir,
		quit:         make(chan struct{}),
		themeWarn:    &themeWarnings{},
	}
	s.peers = plugins.NewPeers(s.pluginCallbackURL, s.done)
	s.ml = monloader.NewLink(s.monloaderLink, jobManager)

	applyRelationsConfig(cfg.Relations)
	gallery.MetaTagsEnabled.Store(cfg.Gallery.AutoMetaTags)

	if s.set, err = library.OpenSet(setHost{s}, jobManager, cfg.Galleries, cfg.DefaultGallery); err != nil {
		return nil, err
	}
	s.sched = schedule.New(s.set, jobManager, schedHost{s}, s.ml, filepath.Join(filepath.Dir(configPath), "state.toml"), s.done)

	if cfg.Server.ThemeColor != "" && !tags.IsValidCategoryColor(cfg.Server.ThemeColor) {
		logx.Warnf("server.theme_color %q is not a #rgb / #rrggbb colour; the bundled palette is used", cfg.Server.ThemeColor)
		s.cfg.Server.ThemeColor = ""
	}

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sessions.SweepExpired()
				s.loginRL.Sweep()
			case <-s.done:
				return
			}
		}
	}()

	go s.runMemoryReclaim()

	go s.sched.Run()

	go s.runPluginProbes()
	s.seedThemesDir()
	plugins.EnsureDir(s.pluginsDir())
	s.peers.StartManaged(s.effectivePlugins())

	return s, nil
}

// Long enough that a paused browse skips the rebuild, short enough that a
// finished one frees the memory.
const idleIndexReleaseAfter = 30 * time.Minute

// The short tick hands a job's peak working set back when the job ends,
// not at the next interval.
const (
	reclaimTick     = 30 * time.Second
	reclaimInterval = 5 * time.Minute
)

func reclaimDue(jobEnded bool, since time.Duration) bool { return jobEnded || since >= reclaimInterval }

func (s *Server) runMemoryReclaim() {
	ticker := time.NewTicker(reclaimTick)
	defer ticker.Stop()
	last := time.Now()
	seen := s.jobs.Finished()
	for {
		select {
		case <-ticker.C:
			if s.jobs.IsRunning() {
				continue
			}
			ended := s.jobs.Finished()
			if !reclaimDue(ended != seen, time.Since(last)) {
				continue
			}
			seen, last = ended, time.Now()
			ctxs := s.allContexts()
			for _, cx := range ctxs {
				dropped, err := cx.ReleaseIdle(idleIndexReleaseAfter)
				if dropped {
					logx.Debugf("memory reclaim %q: dropped idle indexes", cx.Name)
				}
				switch {
				case errors.Is(err, db.ErrPoolBusy):
					logx.Debugf("memory reclaim %q: %v", cx.Name, err)
				case err != nil:
					logx.Warnf("memory reclaim %q: %v", cx.Name, err)
				}
			}
			s.ml.Fetches.Prune()
			s.ml.ReconcileAll(s.allContexts())
			debug.FreeOSMemory()
			s.cfgMu.RLock()
			mins := s.cfg.Tagger.IdleReleaseAfterMinutes
			s.cfgMu.RUnlock()
			if mins > 0 {
				before := readVmRSS()
				if tagger.ReleaseIdle(time.Duration(mins) * time.Minute) {
					after := readVmRSS()
					if before > 0 && after > 0 && before > after {
						logx.Infof("memory reclaim: released idle auto-tagger session (-%s)", webui.HumanBytes(int64(before-after)))
					} else {
						logx.Infof("memory reclaim: released idle auto-tagger session")
					}
				}
			}
		case <-s.done:
			return
		}
	}
}

func (s *Server) activeGallery() string { return s.set.ActiveName() }

func (s *Server) active() *library.Gallery { return s.set.Active() }

// get answers a member an import or a refused rename closed and could not
// reopen as it does an unknown name.
func (s *Server) get(name string) *library.Gallery {
	if cx := s.set.Get(name); cx != nil && !cx.Closed() {
		return cx
	}
	return nil
}

type routeMode int

const (
	modeRead routeMode = iota
	// The handler takes the set's write lock itself; a read lock taken at
	// registration would deadlock it.
	modeWrite
	// No lock: the handler must not keep a gallery context across a
	// window a mutation could close it in. For routes that read no
	// gallery, wait on a peer, or stream after a locked start.
	modeFree
)

// registerRoutes must not reach rt.mux directly: every route picks a mode
// through add.
type routes struct {
	s     *Server
	mux   *http.ServeMux
	modes map[string]routeMode
}

func (s *Server) newRoutes() *routes {
	return &routes{s: s, mux: http.NewServeMux(), modes: map[string]routeMode{}}
}

func (rt *routes) add(pattern string, mode routeMode, h http.HandlerFunc) {
	rt.modes[pattern] = mode
	rt.mux.HandleFunc(pattern, h)
}

func (rt *routes) read(pattern string, h http.HandlerFunc) {
	s := rt.s
	rt.add(pattern, modeRead, func(w http.ResponseWriter, r *http.Request) {
		s.set.RLock()
		defer s.set.RUnlock()
		if pageGalleryStale(w, r, s.activeGallery()) {
			return
		}
		h(w, r)
	})
}

func (rt *routes) write(pattern string, h http.HandlerFunc) { rt.add(pattern, modeWrite, h) }

func (rt *routes) free(pattern string, h http.HandlerFunc) { rt.add(pattern, modeFree, h) }

func (s *Server) StartWatchers() { s.set.StartBackground() }

func (s *Server) background(name string) library.Background {
	watch, maxMB := s.watcherSettings()
	return library.Background{Watch: watch, MaxFileSizeMB: maxMB, Naming: s.ingestNaming(name)}
}

func (s *Server) Handler() http.Handler {
	rt := s.newRoutes()
	s.registerRoutes(rt)

	var h http.Handler = rt.mux
	h = s.sessions.Guard(pluginMountOrigin(h), isPluginMount)
	h = s.setupMiddleware(h)
	h = s.sessions.Gate(h, sessionCookie, s.authEnabled, isOpenPath)
	h = s.hostMiddleware(h)
	h = loggingMiddleware(h)

	return h
}

func (s *Server) registerRoutes(rt *routes) {
	// The thumbnail route needs no lock: it takes only the directory from
	// its context.
	rt.free("GET /static/", http.StripPrefix("/static/", webui.StaticHandler(s.staticFS, assetVersion)).ServeHTTP)
	rt.free("GET /static/shared/", http.StripPrefix("/static/shared/", webui.StaticHandler(webui.Static, assetVersion)).ServeHTTP)
	rt.free("GET /static/light/", http.StripPrefix("/static/light/", webui.StaticHandler(s.lightFS, assetVersion)).ServeHTTP)
	rt.free("GET /theme.css", s.serveThemeCSS)
	rt.free("GET /theme.logo", s.serveThemeLogo)
	rt.free("GET /theme.favicon", s.serveThemeFavicon)
	rt.free("GET /theme.mascot/{face}", s.serveThemeMascot)
	rt.free("GET /manifest.json", s.manifestHandler)
	rt.free("GET /thumbnails/{gallery}/{file}", s.serveThumbnail)
	// Found, not permanent: the target follows the active theme.
	rt.free("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, themeFaviconURL(s.activeTheme()), http.StatusFound)
	})

	rt.free("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// Not refused over its Origin: the browser blocks a cross-origin
		// read itself, and a monitor sending one still gets an answer.
		api.SetCORS(w, r, s.cfgSnapshot())
		// A second launch reads "app" to tell this instance from another
		// program on the port.
		contract.WriteJSON(w, http.StatusOK, contract.Health{App: contract.AppMonbooru, Status: "ok", Version: Version,
			API: peer.APIVersion, MinPeerAPI: peer.MinPeerAPI})
	})

	rt.free("GET /login", s.loginPage)
	rt.free("POST /login", s.loginPost)
	rt.free("POST /logout", s.logoutPost)

	rt.free("POST /upload", s.uploadPost)

	rt.read("GET /{$}", s.galleryHandler)
	rt.read("GET /", s.notFoundHandler)

	// Switches the active gallery when the image lives in another one.
	rt.write("GET /i/{sha}", s.imageByHashHandler)
	rt.read("GET /images/{id}", s.detailHandler)
	rt.read("GET /images/{id}/related", s.relatedImagesHandler)
	rt.read("GET /images/{id}/file", s.serveImageFile)
	rt.read("GET /images/{id}/view", s.serveImageView)
	rt.read("GET /images/{id}/page/{n}", s.serveMangaPage)
	rt.read("GET /images/{id}/page/{n}/thumb", s.serveMangaPageThumb)
	rt.read("POST /images/{id}/page/{n}/extract", s.extractMangaPage)
	rt.read("POST /images/{id}/generate-collection", s.generateMangaCollection)
	rt.read("GET /images/{id}/read", s.readerHandler)
	rt.read("GET /images/{id}/pages", s.pagesGridHandler)
	rt.read("POST /images/{id}/tags", s.addTagToImage)
	rt.read("DELETE /images/{id}/tags", s.removeAllTagsFromImageHandler)
	rt.read("DELETE /images/{id}/user-tags", s.removeUserTagsFromImageHandler)
	rt.read("DELETE /images/{id}/auto-tags", s.removeAutoTagsFromImageHandler)
	rt.read("DELETE /images/{id}/source-tags", s.removeSourceTagsFromImageHandler)
	rt.read("DELETE /images/{id}/stale-tags", s.removeStaleTagsFromImageHandler)
	rt.read("DELETE /images/{id}/category-tags", s.removeCategoryTagsFromImageHandler)
	rt.read("DELETE /images/{id}/source-contribution", s.dropSourceContributionHandler)
	rt.read("DELETE /images/{id}/tags/{tagID}", s.removeTagFromImage)
	rt.read("POST /images/{id}/favorite", s.toggleFavorite)
	rt.read("POST /images/{id}/inbox", s.toggleInbox)
	rt.read("DELETE /images/{id}", s.deleteImage)
	rt.read("POST /images/{id}/canonical-path", s.promoteCanonical)
	rt.read("POST /images/{id}/sources/set", s.setSource)
	rt.read("POST /images/{id}/sources/remove", s.removeSource)
	rt.read("POST /images/{id}/sources/primary", s.makeSourcePrimary)
	rt.read("POST /images/{id}/sources/keep", s.keepLocalFile)
	rt.read("POST /images/{id}/annotations/set", s.setAnnotation)
	rt.read("POST /images/{id}/annotations/remove", s.removeAnnotation)
	rt.read("POST /images/{id}/markup/preview", s.previewMarkup)
	rt.free("POST /images/{id}/sources/fetch", s.fetchSource)
	rt.free("POST /images/{id}/lookup", s.lookupImage)
	rt.free("POST /images/{id}/replace", s.replaceImage)
	rt.free("GET /images/{id}/ptr-contrib-panel", s.ptrContribPanel)
	rt.free("GET /images/{id}/ptr-contrib-dialog", s.ptrContribDialog)
	rt.free("POST /images/{id}/ptr-contrib", s.ptrContribSend)
	rt.read("POST /images/{id}/note", s.setNote)
	rt.read("POST /internal/batch-lookup/count", s.batchLookupCount)
	rt.read("POST /images/{id}/scheduled-lookup", s.scheduledLookupPost)
	rt.read("POST /images/{id}/scheduled-lookup/reset", s.scheduledLookupResetPost)
	rt.read("POST /images/{id}/commentary/set", s.setSourceCommentary)
	rt.read("POST /images/{id}/commentary/remove", s.removeSourceCommentary)
	rt.read("POST /images/{id}/translation/set", s.setSourceTranslation)
	rt.read("POST /images/{id}/translation/remove", s.removeSourceTranslation)
	rt.read("POST /images/{id}/original/set", s.setSourceOriginal)
	rt.read("POST /images/{id}/original/remove", s.removeSourceOriginal)
	rt.read("POST /images/{id}/collections/set", s.setCollection)
	rt.read("POST /images/{id}/collections/remove", s.removeCollection)
	rt.read("POST /images/{id}/place", s.placeImage)
	rt.read("POST /images/{id}/transfer", s.transferImage)
	rt.read("DELETE /images/{id}/aliases/{pathID}", s.deleteAlias)

	rt.read("GET /tags", s.tagsHandler)
	rt.read("GET /tags/{id}", s.tagDetailHandler)
	rt.read("GET /tags/{id}/usage", s.tagUsagePanelHandler)
	rt.read("POST /tags/batch-category", s.batchTagCategoryPost)
	rt.read("POST /tags/batch-alias", s.batchTagAliasPost)
	rt.read("POST /tags/merge-folded", s.batchMergeFoldedPost)
	rt.read("POST /tags/batch-imply", s.batchTagImplyPost)
	rt.read("POST /tags/batch-clean", s.batchTagCleanPost)
	rt.read("POST /tags/batch-distinct", s.batchTagDistinctPost)
	rt.read("POST /tags/new", s.createTagPost)
	rt.read("POST /tags/reject", s.rejectTagsPost)
	rt.read("POST /tags/reject-search", s.rejectTagsSearchPost)
	rt.read("POST /tags/aliases", s.createAliasPost)
	rt.read("POST /tags/{id}/rename", s.renameTagPost)
	rt.read("DELETE /tags/{id}", s.deleteTagHandler)
	rt.read("POST /tags/{id}/reject", s.rejectTagPost)
	rt.read("POST /tags/{id}/unreject", s.unrejectTagPost)
	rt.read("PATCH /tags/{id}/category", s.changeTagCategory)
	rt.read("GET /tags/{id}/implications", s.implicationsDialogHandler)
	rt.read("POST /tags/{id}/implications", s.addImplicationPost)
	rt.read("POST /tags/{id}/implied-by", s.addImpliedByPost)
	rt.read("POST /tags/{id}/aliases", s.addTagAliasPost)
	rt.read("POST /tags/{id}/note", s.setTagNote)
	rt.read("POST /tags/{id}/markup/preview", s.previewMarkup)
	// The /group suffix: a bare DELETE /tags/{id}/aliases would conflict
	// with DELETE /tags/categories/{id} in the mux.
	rt.read("DELETE /tags/{id}/implications/group", s.removeImplicationsDelete)
	rt.read("DELETE /tags/{id}/implied-by/group", s.removeImpliedByDelete)
	rt.read("DELETE /tags/{id}/aliases/group", s.removeTagAliasesDelete)
	rt.read("DELETE /tags/{id}/implications/{impliedID}", s.removeImplicationDelete)
	rt.read("DELETE /tags/{id}/distinct/{otherID}", s.unmarkTagDistinctDelete)
	rt.read("POST /tags/categories", s.createCategoryPost)
	rt.read("POST /tags/categories/{id}/rename", s.renameCategoryPost)
	rt.read("DELETE /tags/categories/{id}", s.deleteCategoryDelete)
	rt.read("GET /tags/categories/{id}/count", s.categoryCountHandler)
	rt.read("POST /tags/categories/{id}/remove-implications", s.removeCategoryImplicationsPost)
	rt.read("POST /tags/categories/order", s.reorderCategoriesPost)

	rt.read("GET /collections", s.collectionsHandler)
	rt.read("POST /collections/rename", s.renameCollectionPost)
	rt.read("POST /collections/dissolve", s.dissolveCollectionPost)
	rt.read("POST /collections/find-relations", s.collectionFindRelationsPost)
	rt.read("GET /collections/order", s.collectionOrderDialog)
	rt.read("POST /collections/order", s.reorderCollectionPost)
	rt.read("POST /collections/generate-cbz", s.generateCollectionCBZ)

	rt.read("GET /categories", s.categoriesHandler)

	rt.read("GET /settings", s.settingsHandler)
	rt.read("POST /settings/general", s.settingsGeneralPost)
	rt.read("POST /settings/general/ignore", s.settingsIgnorePost)
	rt.free("POST /settings/monloader", s.settingsMonloaderPost)
	rt.free("POST /settings/tagger", s.settingsTaggerPost)
	rt.free("POST /settings/auth/password", s.settingsPasswordPost)
	rt.free("POST /settings/auth/remove-password", s.settingsRemovePasswordPost)
	rt.free("POST /settings/auth/tokens", s.settingsTokenCreate)
	rt.free("DELETE /settings/auth/tokens/{id}", s.settingsTokenRevoke)
	rt.free("GET /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesGet)
	rt.free("POST /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesPost)
	rt.read("PATCH /settings/categories/{id}", s.updateCategoryPatch)
	rt.free("POST /settings/schedule", s.settingsSchedulePost)
	rt.free("POST /settings/schedule/run", s.settingsScheduleRunPost)
	rt.read("GET /setup", s.setupPage)
	// The wizard's submit repoints the default gallery.
	rt.write("POST /setup", s.localOnly(s.setupPost))
	rt.free("GET /internal/browse", s.localOnly(s.browseDirs))
	rt.free("POST /internal/open-folder", s.localOnly(s.openFolder))
	rt.free("POST /settings/desktop", s.localOnly(s.settingsDesktopPost))
	rt.free("POST /settings/quit", s.localOnly(s.settingsQuit))
	rt.free("POST /settings/restart", s.localOnly(s.settingsRestart))
	rt.free("GET /internal/desktop-power", s.localOnly(s.desktopPowerFragment))
	rt.read("POST /settings/maintenance/prune-missing", s.pruneMissingImagesPost)
	rt.read("POST /settings/maintenance/prune-orphaned-thumbnails", s.pruneOrphanedThumbnailsPost)
	rt.read("POST /settings/maintenance/empty-folders", s.emptyFoldersScanPost)
	rt.read("POST /settings/maintenance/empty-folders/remove", s.emptyFoldersRemovePost)
	rt.read("GET /settings/maintenance/apply-destination", s.applyDestinationGet)
	rt.read("POST /settings/maintenance/apply-destination", s.applyDestinationPost)
	rt.read("POST /settings/maintenance/recalc-tags", s.recalcTagsPost)
	rt.read("GET /settings/maintenance/unused-tags", s.unusedTagsGet)
	rt.read("POST /settings/maintenance/unused-tags", s.unusedTagsPost)
	rt.read("GET /settings/maintenance/reset-tags", s.resetTagsGet)
	rt.read("POST /settings/maintenance/reset-tags", s.resetTagsPost)
	rt.read("POST /settings/maintenance/tag-conflicts", s.tagCategoryConflictsPost)
	rt.read("POST /settings/maintenance/find-folded-duplicates", s.findFoldedDuplicatesPost)
	rt.read("POST /settings/maintenance/lookup-due", s.lookupDuePost)
	rt.read("POST /relations/file-duplicates/remove", s.removeDuplicatesPost)
	rt.read("GET /relations/duplicates/sha256", s.sha256WalkerPage)
	rt.read("POST /relations/duplicates/sha256/remove-one", s.sha256WalkerRemoveOnePost)
	rt.read("GET /relations/duplicates/marked", s.markedWalkerPage)
	rt.read("POST /relations/duplicates/marked/delete-one", s.markedWalkerDeleteOnePost)
	rt.read("POST /relations/duplicates/marked/delete-all", s.markedWalkerDeleteAllPost)
	rt.read("GET /relations", s.relationsPage)
	rt.read("GET /relations/browse", s.browseRelationsPage)
	rt.read("GET /relations/browse-groups", s.browseGroupsRedirect)
	rt.read("GET /relations/session", s.sessionPage)
	rt.read("POST /relations/session/decide", s.sessionDecidePost)
	rt.read("POST /relations/dup-group/{id}/copy-tags", s.copyTagsToOriginalPost)
	rt.read("GET /relations/dup-group/{id}/copy-tags/preview", s.copyTagsToOriginalPreview)
	rt.read("POST /settings/relations", s.settingsRelationsPost)
	rt.read("POST /settings/maintenance/re-extract-metadata", s.reExtractMetadataPost)
	rt.read("POST /settings/maintenance/rebuild-thumbnails", s.rebuildThumbnailsPost)
	rt.read("POST /settings/maintenance/compute-hashes", s.computeHashesPost)
	rt.read("POST /settings/maintenance/meta-tags", s.generateMetaTagsPost)
	rt.read("POST /settings/maintenance/index-workflows", s.indexWorkflowsPost)
	rt.read("POST /settings/maintenance/meta-tags/remove", s.removeMetaTagsPost)
	rt.read("POST /relations/find-pairs", s.findRelationPairsPost)
	rt.read("POST /relations/reset-skipped", s.resetSkippedPost)
	rt.read("POST /relations/phash/{id}/recompute", s.recomputePhashPost)
	rt.read("POST /relations/add", s.addRelationPost)
	rt.read("POST /relations/remove", s.removeRelationPost)
	rt.read("POST /relations/reverse", s.reverseRelationPost)
	rt.read("POST /relations/browse-groups/merge", s.mergeGroupsPost)
	rt.read("POST /relations/browse-groups/dissolve", s.dissolveGroupsPost)
	rt.read("GET /internal/images/{id}/md5", s.md5CellGet)
	rt.read("GET /internal/images/{id}/related-entries", s.relatedEntriesGet)
	rt.read("GET /internal/images/{id}/fetch-status", s.fetchStatusHandler)
	rt.read("GET /images/{id}/relations", s.imageRelationsPage)
	rt.read("POST /settings/maintenance/vacuum-db", s.vacuumDBPost)
	rt.read("POST /settings/maintenance/free-memory", s.freeMemoryPost)
	rt.free("POST /settings/tagger/{name}/enable", s.settingsTaggerEnablePost)
	rt.free("POST /settings/tagger/{name}/disable", s.settingsTaggerDisablePost)
	rt.free("POST /settings/tagger/{name}/delete", s.settingsTaggerDeletePost)
	rt.read("GET /settings/tagger/{name}/config", s.settingsTaggerConfigGet)
	rt.free("POST /settings/tagger/{name}/config", s.settingsTaggerConfigPost)
	rt.read("GET /settings/tagger/{name}/labels", s.settingsTaggerLabelsGet)
	rt.read("POST /settings/tagger/{name}/mapping", s.settingsTaggerMappingPost)
	rt.free("POST /settings/tagger/{name}/reset", s.settingsTaggerResetPost)

	rt.read("POST /search/saved", s.createSavedSearch)
	rt.read("DELETE /search/saved/{id}", s.deleteSavedSearch)

	rt.read("GET /internal/job/status", s.jobStatusHandler)
	rt.read("GET /internal/footer-counts", s.footerCountsHandler)
	rt.free("GET /internal/monloader-status", s.monloaderStatusHandler)
	rt.read("POST /internal/job/dismiss", s.jobDismissPost)
	rt.read("POST /internal/job/cancel", s.jobCancelPost)
	rt.read("POST /internal/sync", s.syncTrigger)
	rt.read("POST /internal/autotag", s.autotagTrigger)
	rt.read("POST /internal/batch-delete", s.batchDelete)
	rt.read("POST /internal/batch-place", s.batchPlace)
	rt.read("POST /internal/batch-transfer", s.batchTransfer)
	rt.read("POST /internal/batch-tag", s.batchTag)
	rt.read("POST /internal/batch-strip", s.batchStrip)
	rt.read("POST /internal/batch-inbox", s.batchInbox)
	rt.read("POST /internal/batch-favorite", s.batchFavorite)
	rt.read("POST /internal/batch-collection", s.batchCollection)
	rt.read("POST /internal/batch-lookup", s.batchLookup)
	rt.free("POST /internal/batch-download", s.batchDownload)
	rt.read("POST /internal/batch-download/count", s.batchDownloadCount)
	rt.read("POST /internal/slideshow", s.slideshowItems)
	rt.read("POST /internal/delete-search", s.deleteSearchPost)
	rt.read("POST /tags/delete-search", s.deleteTagsSearchPost)
	rt.read("POST /tags/ptr-lookup-search", s.ptrLookupSearchPost)
	rt.free("GET /tags/{id}/ptr-contrib-panel", s.tagPtrContribPanel)
	rt.free("GET /tags/{id}/rules", s.tagRulesPanel)
	rt.free("GET /tags/{id}/ptr-contrib-dialog", s.tagPtrContribDialog)
	rt.free("POST /tags/{id}/ptr-contrib", s.tagPtrContribSend)
	rt.read("GET /tags/{id}/ptr-lookup-dialog", s.tagPtrLookupDialog)
	rt.free("GET /tags/{id}/ptr-lookup-preview", s.tagPtrLookupPreview)
	rt.free("GET /tags/{id}/ptr-lookup-search", s.tagPtrLookupSearch)
	rt.read("POST /tags/{id}/ptr-lookup", s.ptrLookupTagPost)
	rt.read("GET /internal/tags/suggest", s.tagSuggest)
	rt.read("GET /internal/search/suggest", s.searchSuggest)
	rt.read("GET /internal/search/ids", s.searchIDs)
	rt.read("GET /internal/search/count", s.galleryCount)
	rt.read("GET /internal/folders/suggest", s.foldersSuggest)
	rt.read("GET /internal/folders/children", s.folderChildren)
	rt.read("GET /internal/collection/suggest", s.collectionSuggest)
	rt.read("GET /internal/source/suggest", s.sourceSuggest)
	rt.read("GET /internal/name/preview", s.namePreview)
	rt.read("GET /internal/sidebar", s.gallerySidebar)
	rt.read("GET /internal/sidebar-browse", s.sidebarBrowse)
	rt.read("POST /internal/rating-ceiling", s.ratingCeilingPost)
	rt.read("POST /internal/view-prefs", s.viewPrefsPost)
	rt.read("POST /images/{id}/autotag", s.autotagImage)
	rt.read("GET /images/{id}/tags", s.getImageTagsHandler)

	rt.write("POST /internal/gallery/switch", s.gallerySwitchHandler)
	rt.write("POST /settings/galleries", s.settingsGalleriesPost)
	rt.write("POST /settings/galleries/{name}/rename", s.settingsGalleryRenamePost)
	rt.write("POST /settings/galleries/{name}/delete", s.settingsGalleryDeletePost)
	rt.write("POST /settings/galleries/{name}/default", s.settingsGalleryDefaultPost)
	rt.free("GET /settings/galleries/{name}/export", s.settingsGalleryExport)
	rt.write("POST /settings/galleries/{name}/import", s.settingsGalleryImport)

	rt.free("POST /api/v1/pair/request", s.pairRequest)
	rt.free("GET /api/v1/pair/status", s.pairStatus)
	rt.free("POST /api/v1/pair/remove", s.pairTeardown)
	rt.free("GET /internal/plugins/pairing", s.pluginPairingFragment)
	rt.free("POST /settings/plugins/pair/{id}/approve", s.pluginPairApprove)
	rt.free("POST /settings/plugins/pair/{id}/deny", s.pluginPairDeny)
	rt.free("POST /settings/plugins/{name}/remove", s.pluginPairRemove)
	rt.free("POST /settings/plugins/{name}/pause", s.pluginPause)
	rt.free("POST /settings/plugins/{name}/start", s.pluginStart)
	rt.free("POST /settings/plugins/{name}/stop", s.pluginStop)
	rt.free("POST /settings/plugins/theme", s.settingsThemePost)
	rt.free("POST /internal/monloader/disconnect", s.monloaderLightDisconnect)
	rt.free("POST /internal/monloader/reconnect", s.monloaderLightReconnect)
	rt.free("POST /internal/plugin/relay", s.pluginRelay)
	rt.free("GET "+pluginMountPrefix+"{name}/", s.pluginMount)
	rt.free("POST "+pluginMountPrefix+"{name}/", s.pluginMount)

	// A mux of its own so the API subtree takes the read mode. The
	// methods are spelled out: a bare "/api/v1/" conflicts with "GET /".
	apiMux := http.NewServeMux()
	api.New(s.cfgSnapshot, s.jobs, s.apiResolver, Version).WithGalleryLock(func() func() {
		s.set.RLock()
		return s.set.RUnlock
	}).WithFetchStatus(s.ml.Fetches.Record).WithMemoryMark(autotagMemoryMark).WithDone(s.done).Mount(apiMux)
	// The exact pairing routes above answer ahead of this subtree, so
	// they skip notePeerAddress: none of them may move a peer.
	apiHandler := s.notePeerAddress(apiMux.ServeHTTP)
	// The uploads take the gallery lock themselves, once their body is in.
	rt.free("POST /api/v1/images", apiHandler)
	rt.free("POST /api/v1/images/{id}/file", apiHandler)
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		rt.read(method+" /api/v1/", apiHandler)
	}
}

func (s *Server) allContexts() []*library.Gallery {
	return slices.DeleteFunc(s.set.All(), (*library.Gallery).Closed)
}

func (s *Server) apiResolver(name string) (*library.Gallery, bool) {
	if name == "" {
		cx := s.active()
		return cx, cx != nil
	}
	cx := s.get(name)
	return cx, cx != nil
}

func isNoisyPath(path string) bool {
	switch path {
	case "/internal/job/status", "/internal/monloader-status", "/health":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/thumbnails/")
}

// Stamped by the outermost middleware so the footer's load time covers
// the whole chain.
type requestStartKey struct{}

func requestStartFromContext(ctx context.Context) time.Time {
	if t, ok := ctx.Value(requestStartKey{}).(time.Time); ok {
		return t
	}
	return time.Time{}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), requestStartKey{}, time.Now()))
		if isNoisyPath(r.URL.Path) {
			logx.Debugf("%s %s", r.Method, r.URL.Path)
		} else {
			logx.Infof("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// Version is set at build time via -ldflags.
var Version = "dev"

// RepoURL is set at build time via -ldflags.
var RepoURL = "https://github.com/monbooru/monbooru"

// DocURL is set at build time via -ldflags.
var DocURL = "https://monbooru.github.io/mondocs/index.html"

// Variant names the provider build, set via -ldflags; empty on the CPU build.
var Variant = ""

// Package names the artifact, set via -ldflags; "source" is a plain go build.
var Package = "source"

func BuildLabel() string {
	parts := make([]string, 0, 2)
	if Package != "" && Package != "source" {
		parts = append(parts, Package)
	}
	if Variant != "" {
		parts = append(parts, Variant)
	}
	return strings.Join(parts, ", ")
}

type ratingLevel struct {
	Value string
	Label string
}

var ratingFooterLevels = func() []ratingLevel {
	levels := make([]ratingLevel, len(contract.RatingLevels))
	for i, name := range contract.RatingLevels {
		levels[i] = ratingLevel{Value: name, Label: name}
	}
	levels[0].Label = "sfw"
	return levels
}()

type baseData struct {
	Title       string
	ActiveNav   string
	CSRFToken   string
	AuthEnabled bool
	Degraded    bool
	Version     string
	RepoURL     string
	DocURL      string
	Build       string
	Theme       bool
	// Server-rendered so a navigation does not flash the column before
	// main.js runs.
	SidebarCollapsed    bool
	BooruName           string
	BooruLogo           string
	BooruFavicon        string
	Mascot              map[string]string
	ActiveGallery       string
	Galleries           []config.Gallery
	VisibleCount        int
	InboxCount          int
	TagCount            int
	CollectionsCount    int
	InboxNavActive      bool
	HiddenByCeiling     int
	RatingLevels        []ratingLevel
	ActiveRating        string
	RequestStart        time.Time
	MonloaderPaired     bool
	MonloaderUsable     bool
	MonloaderConn       string
	MonloaderLight      webui.PeerLight
	MonloaderPTR        bool
	MonloaderPTRSyncing bool
	// The render gate, where MonloaderPTR is the live one: a paused or
	// unreachable link reports no PTR, so it counts as present.
	MonloaderPTRPresent bool
	PairNotice          webui.PairNotice
}

// main.js writes it; the name and the "collapsed" value must match.
const sidebarCookieName = "monbooru_sidebar"

func sidebarCollapsed(r *http.Request) bool {
	return session.FromCookie(r, sidebarCookieName) == "collapsed"
}

// main.js writes it: each key encoded once and comma-joined. A value with
// no comma of its own was encoded twice and reads once more.
const folderCookieName = "monbooru_folders"

func folderCookieKeys(r *http.Request) map[string]bool {
	raw := session.FromCookie(r, folderCookieName)
	if lower := strings.ToLower(raw); !strings.Contains(raw, ",") && (strings.Contains(lower, "%2c") || strings.Contains(lower, "%25")) {
		var err error
		if raw, err = url.PathUnescape(raw); err != nil {
			return nil
		}
	}
	keys := map[string]bool{}
	for _, k := range strings.Split(raw, ",") {
		key, err := url.PathUnescape(k)
		if err != nil {
			return nil
		}
		if key != "" {
			keys[key] = true
		}
	}
	return keys
}

func (s *Server) base(r *http.Request, nav, title string) baseData {
	sessID := session.FromContext(r.Context())
	cx := s.active()
	degraded := false
	visible, inbox, tagCount, collectionsCount := 0, 0, 0, 0
	if cx != nil {
		degraded = cx.Degraded
		visible, _ = cx.VisibleCount()
		// Ceiling-aware: every surface showing it promises the count a
		// click will match.
		inbox, _ = cx.InboxCountUnder(resolveCeiling(r, cx))
		tagCount, _ = cx.TagCount()
		collectionsCount, _ = cx.CollectionsCount()
	}
	inboxNavActive := inboxFilterActive(search.Parse(r.URL.Query().Get("q")))
	galleries := s.galleries()
	active := cmp.Or(readRatingCookie(r), "explicit")
	ml := s.ml.Status()
	if s.ml.Paused() {
		ml = monloader.Status{Conn: peer.ConnPaused}
	}
	paired := s.pairedWith(monloader.App)
	monloaderUsable := s.ml.Usable()
	theme := s.activeTheme()
	return baseData{
		Title:               title,
		ActiveNav:           nav,
		CSRFToken:           s.sessions.CSRFToken(sessID),
		AuthEnabled:         s.authEnabled(),
		Degraded:            degraded,
		Version:             Version,
		RepoURL:             RepoURL,
		DocURL:              DocURL,
		Build:               BuildLabel(),
		Theme:               theme.Path != "",
		SidebarCollapsed:    sidebarCollapsed(r),
		BooruName:           s.booruName(),
		BooruLogo:           themeLogoURL(theme),
		BooruFavicon:        themeFaviconURL(theme),
		Mascot:              themeMascots(theme),
		MonloaderPaired:     paired,
		MonloaderUsable:     monloaderUsable,
		MonloaderConn:       ml.Conn,
		MonloaderLight:      s.monloaderLight(ml.Conn, ml.Version, ml.APIMismatch, ml.Panels()),
		MonloaderPTR:        ml.PTR,
		MonloaderPTRSyncing: ml.PTRSyncing,
		MonloaderPTRPresent: paired && (ml.PTR || ml.PTRSyncing || !monloaderUsable),
		ActiveGallery:       s.activeGallery(),
		Galleries:           galleries,
		VisibleCount:        visible,
		InboxCount:          inbox,
		TagCount:            tagCount,
		CollectionsCount:    collectionsCount,
		InboxNavActive:      inboxNavActive,
		RatingLevels:        ratingFooterLevels,
		ActiveRating:        active,
		RequestStart:        requestStartFromContext(r.Context()),
		PairNotice:          s.pairNotice(),
	}
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Buffered so a failed execution can still send a clean 500.
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		logx.Errorf("template %q: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if _, err := buf.WriteTo(w); err != nil {
		logx.Warnf("template %q write: %v", name, err)
	}
}

var thumbnailNameRe = regexp.MustCompile(`^\d+(?:_hover\.webp|\.jpg)$`)

// The gallery is in the URL so a switch cannot show another gallery's
// cached preview.
func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	file := filepath.Base(r.PathValue("file"))
	if !thumbnailNameRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	cx := s.get(r.PathValue("gallery"))
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(cx.ThumbnailsPath, file)
	// 204, not 404: the img onerror still fires, and the console logs no
	// error per card.
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// SQLite reuses the highest id once its row is deleted, so a
	// thumbnail URL can come back with new bytes.
	setGalleryScopedCache(w, r.PathValue("gallery"), file, fullPath)
	http.ServeFile(w, r, fullPath)
}

func (s *Server) booruName() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return cmp.Or(s.cfg.Server.BooruName, "Monbooru")
}

func (s *Server) modelPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Paths.ModelPath
}

func (s *Server) authEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.EnablePassword
}

func (s *Server) passwordHash() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.PasswordHash
}

func (s *Server) sessionLifetimeDays() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.SessionLifetimeDays
}

const thumbSizeCookieName = "monbooru_thumb_size"

func thumbSize(r *http.Request) string {
	switch v := session.FromCookie(r, thumbSizeCookieName); v {
	case "s", "l":
		return v
	}
	return "m"
}

const pageSizeCookieName = "monbooru_page_size"

var PageSizeOptions = []int{20, 40, 60, 100, 250, 500}

// The cookie is client-writable, and the query budgets cover only the
// offered sizes.
func pageSizeOverride(r *http.Request) int {
	n, err := strconv.Atoi(session.FromCookie(r, pageSizeCookieName))
	if err != nil || !slices.Contains(PageSizeOptions, n) {
		return 0
	}
	return n
}

func (s *Server) pageSize(r *http.Request) int {
	if n := pageSizeOverride(r); n > 0 {
		return n
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.UI.PageSize
}

func (s *Server) thumbnailFit() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.UI.ThumbnailFit
}

func (s *Server) maxFileSizeMB() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Gallery.MaxFileSizeMB
}

// One read lock for both, so a save cannot land between them.
func (s *Server) watcherSettings() (bool, int) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB
}

func (s *Server) themeColor() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Server.ThemeColor
}

func (s *Server) defaultGallery() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.DefaultGallery
}

func (s *Server) executionProvider() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Tagger.ExecutionProvider
}

func (s *Server) envPinned(key string) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.EnvPinned(key)
}

// The password forms write both keys, so either one pinned locks them.
func (s *Server) passwordPinned() string {
	return cmp.Or(s.envPinned("auth.password_hash"), s.envPinned("auth.enable_password"))
}

// The slices settings writers edit in place are cloned; what their elements
// hold is replaced wholesale, never written through, so one level is enough.
func cloneConfig(cfg *config.Config) *config.Config {
	c := *cfg
	c.Galleries = slices.Clone(c.Galleries)
	c.Plugins = slices.Clone(c.Plugins)
	c.Auth.Tokens = slices.Clone(c.Auth.Tokens)
	c.Tagger.Taggers = slices.Clone(c.Tagger.Taggers)
	return &c
}

func (s *Server) cfgSnapshot() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return cloneConfig(s.cfg)
}

func (s *Server) galleries() []config.Gallery {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return slices.Clone(s.cfg.Galleries)
}

var assetVersion = webui.AssetVersion(webFS.FS, webui.Static, registry.FS)

func themeLogoURL(e webui.Theme) string {
	if e.Logo != "" {
		return "/theme.logo"
	}
	return cmp.Or(builtinArt[e.Name], "/static/") + "logo.png?v=" + assetVersion
}

// Versioned: browsers keep favicons outside the page cache and Firefox
// does not revalidate them, so a fixed URL keeps the first icon seen.
func themeFaviconURL(e webui.Theme) string {
	if e.Favicon == "" {
		return cmp.Or(builtinArt[e.Name], "/static/") + "favicon.png"
	}
	return "/theme.favicon?v=" + themeFaviconVersion(e)
}

func themeFaviconVersion(e webui.Theme) string {
	if e.Builtin {
		return cmp.Or(Version, "builtin")
	}
	info, err := os.Stat(e.Favicon)
	if err != nil {
		return e.Name
	}
	return url.QueryEscape(e.Name) + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

func (s *Server) monloaderLink() monloader.LinkConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	c := monloader.LinkConfig{APIURL: s.cfg.Monloader.APIURL, Token: s.cfg.Monloader.APIToken, Paused: s.cfg.Monloader.Paused}
	if t := s.cfg.Auth.Tokens.Paired(monloader.App); t != nil {
		c.PeerURL, c.Paired = t.PeerURL, true
	}
	return c
}

func (s *Server) monloaderWebBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return strings.TrimRight(s.cfg.Server.MonloaderURL, "/")
}

func (s *Server) resolveMangaImage(idStr string) (string, bool) {
	cx := s.active()
	if cx == nil {
		return "", false
	}
	var canonPath, fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT canonical_path, file_type FROM images WHERE id = ?`, idStr,
	).Scan(&canonPath, &fileType); err != nil {
		return "", false
	}
	if fileType != "cbz" {
		return "", false
	}
	if !gallery.NamedInside(cx.GalleryPath, canonPath) {
		return "", false
	}
	return canonPath, true
}

func (s *Server) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPage, "", true)
}

func (s *Server) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb, "-thumb", false)
}

func (s *Server) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
	cacheSuffix string, negotiate bool,
) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	canonPath, ok := s.resolveMangaImage(idStr)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, err := ensure(s.thumbnailsPath(), canonPath, id, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fileType := models.ExtFileType(page); negotiate && gallery.IsFFmpegStill(fileType) {
		w.Header().Add("Vary", "Accept")
		if !acceptsMediaType(r.Header.Get("Accept"), gallery.MIMEForFileType(fileType)) {
			if view, err := gallery.EnsureMangaPageView(s.thumbnailsPath(), canonPath, id, n); err != nil {
				logx.Warnf("view copy for page %d of image %s: %v", n, idStr, err)
			} else {
				page, cacheSuffix = view, "-view"
			}
		}
	}
	// The archive's mtime, not the page's: every hit touches the page to keep
	// it cached, which would make each ETag new.
	setGalleryScopedCache(w, s.activeGallery(), fmt.Sprintf("%s-%d%s", idStr, n, cacheSuffix), canonPath)
	http.ServeFile(w, r, page)
}

func (s *Server) serveImageFile(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, false)
}

// The viewers' URL; downloads and <video> stay on /file. A still past the
// display ceiling, or an AVIF or JPEG XL the browser does not accept, is
// served as a JPEG rendition.
func (s *Server) serveImageView(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, true)
}

func (s *Server) serveImageBytes(w http.ResponseWriter, r *http.Request, scaled bool) {
	idStr := r.PathValue("id")
	cx := s.active()
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	var id int64
	var canonPath, fileType string
	var width, height sql.NullInt64
	if err := cx.DB.Read.QueryRow(
		`SELECT id, canonical_path, file_type, width, height FROM images WHERE id = ?`, idStr,
	).Scan(&id, &canonPath, &fileType, &width, &height); err != nil {
		http.NotFound(w, r)
		return
	}
	if !gallery.NamedInside(cx.GalleryPath, canonPath) {
		http.NotFound(w, r)
		return
	}
	if scaled && !gallery.IsVideoType(fileType) && fileType != "cbz" {
		past := gallery.NeedsViewRendition(int(width.Int64), int(height.Int64))
		// Not every browser decodes AVIF or JPEG XL: one that does names
		// the type in Accept, and any other gets a full-size JPEG.
		negotiated := gallery.IsFFmpegStill(fileType)
		if negotiated {
			w.Header().Add("Vary", "Accept")
		}
		if past || (negotiated && !acceptsMediaType(r.Header.Get("Accept"), gallery.MIMEForFileType(fileType))) {
			maxDim := 0
			if past {
				maxDim = gallery.ViewMaxDim
			}
			rendition, err := gallery.EnsureViewRendition(canonPath, cx.ThumbnailsPath, id, fileType, maxDim)
			if err != nil {
				logx.Warnf("view rendition for image %s: %v", idStr, err)
			} else {
				setGalleryScopedCache(w, s.activeGallery(), idStr+"-view", rendition)
				w.Header().Set("Content-Type", "image/jpeg")
				http.ServeFile(w, r, rendition)
				return
			}
		}
	}
	setGalleryScopedCache(w, s.activeGallery(), idStr, canonPath)
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

// A wildcard does not count: every browser sends image/*, whether or not
// it decodes JPEG XL.
func acceptsMediaType(accept, mediaType string) bool {
	for _, part := range strings.Split(accept, ",") {
		t, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || t != mediaType {
			continue
		}
		q, err := strconv.ParseFloat(cmp.Or(params["q"], "1"), 64)
		return err == nil && q > 0
	}
	return false
}

// The gallery is in the tag because /images/{id} URLs repeat across galleries,
// and the mtime is in nanoseconds so two rewrites in a second differ.
func setGalleryScopedCache(w http.ResponseWriter, gallery, id, path string) {
	w.Header().Set("Cache-Control", "private, no-cache")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%s-%d"`, gallery, id, info.ModTime().UnixNano()))
}

func (s *Server) RestartRequested() bool { return s.restart.Load() }

// QuitRequested closes on a Quit or a Restart; RestartRequested tells
// them apart.
func (s *Server) QuitRequested() <-chan struct{} { return s.quit }

func (s *Server) Close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	// Otherwise ReleaseAll waits out a running auto-tag chunk.
	s.jobs.Cancel()
	const jobWait = 5 * time.Second
	if !s.jobs.WaitIdle(jobWait) {
		logx.Warnf("shutdown: a job was still running %s after its cancel", jobWait)
	}
	s.peers.StopAll()
	tagger.ReleaseAll()
	s.set.Close()
}

func (s *Server) withConfig(fn func(*config.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	before := cloneConfig(s.cfg)
	if err := fn(s.cfg); err != nil {
		*s.cfg = *before
		return err
	}
	if err := config.Save(s.cfg, s.configPath); err != nil {
		*s.cfg = *before
		logx.Errorf("config save: %v", err)
		return err
	}
	return nil
}
