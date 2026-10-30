package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) generateMangaCollection(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	name, cx, writeDir, naming, ok := s.collectionJobPrologue(w, r)
	if !ok {
		return
	}
	s.goGenerationJob(func(ctx context.Context) (string, error) {
		done, created, err := s.runMangaCollection(ctx, cx, img, name, writeDir, naming)
		if created == 0 {
			return fmt.Sprintf("%s already in collection %q; nothing new was created.", pagesAre(done), name), err
		}
		return fmt.Sprintf("Generated %d page%s into collection %q.", created, plural(created), name), err
	})
	w.WriteHeader(http.StatusAccepted)
}

// JobTypeTag on purpose: the watcher skips ingests during a tag job, so
// the files this job writes are not ingested twice. The snapshot holds:
// switchGallery refuses while a job runs.
func (s *Server) collectionJobPrologue(w http.ResponseWriter, r *http.Request) (name string, cx *library.Gallery, writeDir string, naming gallery.Naming, ok bool) {
	if !parseFormOK(w, r) {
		return "", nil, "", naming, false
	}
	name = strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		flashStatus(w, http.StatusBadRequest, "Collection label required.")
		return "", nil, "", naming, false
	}
	if utf8.RuneCountInString(name) > contract.MaxCollection {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return "", nil, "", naming, false
	}
	if active := s.active(); active == nil || active.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Generation unavailable: gallery path is unreadable.")
		return "", nil, "", naming, false
	}
	if !s.startJob(w, models.JobTypeTag) {
		return "", nil, "", naming, false
	}
	cx = s.active()
	writeDir, naming = s.receivedNaming(cx.Name)
	return name, cx, writeDir, naming, true
}

func (s *Server) goGenerationJob(run func(ctx context.Context) (string, error)) {
	s.goJobWork(models.JobTypeTag, func(ctx context.Context) {
		summary, err := run(ctx)
		_ = s.jobs.Settle(ctx, err, "generation cancelled", summary)
	})
}

// Returns the pages walked and those that landed as new rows; a page the
// gallery already holds folds onto its row.
func (s *Server) runMangaCollection(ctx context.Context, cx *library.Gallery, img *models.Image, name, writeDir string, naming gallery.Naming) (int, int, error) {
	destDir, err := cx.Boundary().ResolveSubdir(writeDir)
	if err != nil {
		return 0, 0, err
	}
	stem := strings.TrimSuffix(filepath.Base(img.CanonicalPath), filepath.Ext(img.CanonicalPath))
	sub := stem
	if h := shortHash(img.SHA256); h != "" {
		sub = stem + "-" + h
	}
	destDir = filepath.Join(destDir, sub)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, 0, err
	}
	// os.Remove only takes an empty folder: the one a run leaves when
	// every page folded onto an existing row.
	defer func() { _ = os.Remove(destDir) }()
	total := *img.PageCount
	s.jobs.Update(0, total, "generating…")
	done, created := 0, 0
	filedDir := ""
	for n := 1; n <= total; n++ {
		if ctx.Err() != nil {
			return done, created, ctx.Err()
		}
		pageID, filed, err := s.extractMangaPageToGallery(ctx, cx, img, n, destDir, "p", gallery.Naming{})
		if err != nil {
			return done, created, fmt.Errorf("page %d/%d: %w", n, total, err)
		}
		if filed {
			created++
			// Resolved once, off the first page: a template with {id}
			// or a clock would scatter one archive's pages.
			if naming.Folder != nil {
				if filedDir == "" {
					rendered, folderErr := naming.FolderFor(ctx, cx.DB, pageID)
					if folderErr != nil {
						return done, created, fmt.Errorf("page %d/%d destination: %w", n, total, folderErr)
					}
					filedDir = path.Join(rendered, sub)
				}
				if _, moveErr := cx.PlaceImage(pageID, &filedDir, nil); moveErr != nil {
					return done, created, fmt.Errorf("page %d/%d file: %w", n, total, moveErr)
				}
			}
		}
		pos := n
		if err := cx.Edit(func(d *db.DB) error { return gallery.AddCollectionMembership(d, pageID, name, &pos) }); err != nil {
			return done, created, fmt.Errorf("page %d/%d membership: %w", n, total, err)
		}
		done = n
		s.jobs.Update(done, total, "generating…")
	}
	return done, created, nil
}

func (s *Server) generateCollectionCBZ(w http.ResponseWriter, r *http.Request) {
	name, cx, writeDir, naming, ok := s.collectionJobPrologue(w, r)
	if !ok {
		return
	}
	s.goGenerationJob(func(ctx context.Context) (string, error) {
		res, err := s.runCollectionCBZ(ctx, cx, name, writeDir, naming)
		return res.summary(), err
	})
	w.WriteHeader(http.StatusAccepted)
}

type collectionCBZResult struct {
	Pages    int
	Skipped  int
	Filename string
	Existing string
}

func (r collectionCBZResult) summary() string {
	if r.Existing != "" {
		return fmt.Sprintf("%s already packed as %s; nothing new was created.", pagesAre(r.Pages), r.Existing)
	}
	msg := fmt.Sprintf("Generated %d image%s as %s.", r.Pages, plural(r.Pages), r.Filename)
	if r.Skipped > 0 {
		msg += fmt.Sprintf(" %d member%s skipped (missing files).", r.Skipped, plural(r.Skipped))
	}
	return msg
}

func (s *Server) runCollectionCBZ(ctx context.Context, cx *library.Gallery, name, writeDir string, naming gallery.Naming) (collectionCBZResult, error) {
	var res collectionCBZResult
	members, err := gallery.CollectionCBZMembers(cx.DB, name)
	if err != nil {
		return res, err
	}
	if len(members) == 0 {
		return res, fmt.Errorf("collection %q has no visible members", name)
	}
	destDir, err := cx.Boundary().ResolveSubdir(writeDir)
	if err != nil {
		return res, err
	}
	filename := collectionCBZFilename(name)
	landed, err := cx.Land(ctx, destDir, filename, func(dst string) error {
		var werr error
		res.Pages, res.Skipped, werr = gallery.WriteCollectionCBZ(ctx, dst, members, name,
			func(processed, total int, message string) { s.jobs.Update(processed, total, message) })
		return werr
	}, library.LandSpec{Origin: models.OriginGenerate, Naming: naming, KeepOnFailure: true})
	if err != nil {
		if library.LandStageOf(err) == library.LandIngest {
			return collectionCBZResult{}, fmt.Errorf("ingest %q: %w", filename, err)
		}
		return collectionCBZResult{}, err
	}
	res.Filename = filepath.Base(landed.Path)
	// An unchanged collection packs byte for byte the same, so a rerun
	// lands on the earlier archive.
	if landed.Dropped {
		res.Existing = filepath.Base(landed.Image.CanonicalPath)
	}
	return res, nil
}

// The timestamp leads so a filesystem truncating a long label cannot cut
// it, and carries the time so two packs in one day differ.
func collectionCBZFilename(name string) string {
	return fmt.Sprintf("%s-%s.cbz", time.Now().Format("20060102-150405"), sanitizeCollectionFilename(name))
}

func shortHash(hash string) string { return hash[:min(8, len(hash))] }

// Leaves room under the 255-byte name limit for the timestamp, ".cbz" and
// a collision counter.
const maxCBZStemBytes = 180

func sanitizeCollectionFilename(name string) string {
	out := gallery.TruncateFilename(fsx.SanitizeFilename(name), maxCBZStemBytes)
	if out == "" {
		return "collection"
	}
	return out
}

// The bool is false when the page folded onto a row the gallery already held.
func (s *Server) extractMangaPageToGallery(ctx context.Context, cx *library.Gallery, img *models.Image, n int, destDir, prefix string, naming gallery.Naming) (int64, bool, error) {
	pagePath, err := gallery.EnsureMangaPage(cx.ThumbnailsPath, img.CanonicalPath, img.ID, n)
	if err != nil {
		return 0, false, err
	}
	// Copied, not moved: the manga reclaim can unlink the cache file
	// anytime. No size cap: the bytes are already in the library.
	landed, err := cx.Land(ctx, destDir, fmt.Sprintf("%s%04d", prefix, n)+filepath.Ext(pagePath),
		library.FromFile(pagePath), library.LandSpec{Origin: models.OriginExtract, Naming: naming})
	if err != nil {
		return 0, false, err
	}
	page, folded := landed.Image, landed.Dropped
	if cx.RelationsSvc != nil {
		// A conflicting relation is a standing operator decision; the
		// extract stands without the link.
		if err := cx.RelationsSvc.AddDerivativeEdge(img.ID, page.ID); err != nil {
			logx.Debugf("extract: link %d -> %d skipped: %v", img.ID, page.ID, err)
		}
	}
	return page.ID, !folded, nil
}

func pagesAre(n int) string {
	if n == 1 {
		return "This page is"
	}
	return fmt.Sprintf("These %d pages are", n)
}
