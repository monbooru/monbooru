package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"runtime/debug"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/webui"
	"github.com/monbooru/monbooru/internal/tagger"
)

func (s *Server) pruneMissingImagesPost(w http.ResponseWriter, r *http.Request) {
	missing, err := db.QueryIDs(s.db().Read, `SELECT id FROM images WHERE is_missing = 1`)
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(missing) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 missing images.")
		return
	}

	active := s.active()
	s.goJob(w, models.JobTypeDelete, "Prune started.", func(ctx context.Context) {
		// The flag is also set for a path the gallery leaves out, or a file
		// sync skipped; only a file gone from disk takes its row.
		s.jobs.Update(0, len(missing), "checking files…")
		res, err := active.DeleteImages(ctx, missing, library.DeleteOptions{FilesGone: true}, func(done, total int) {
			s.jobs.Update(done, total, "pruning…")
		})
		removed := len(res.Deleted)
		summary := fmt.Sprintf("Removed %d missing image%s.", removed, plural(removed))
		if res.Kept > 0 {
			summary = fmt.Sprintf("Removed %d missing image%s; kept %d whose file is still on disk.", removed, plural(removed), res.Kept)
		}
		total := len(missing) - res.Kept
		s.jobs.Finish(err, res.Cancelled, fmt.Sprintf("prune cancelled (%d/%d removed)", removed, total), summary)
	})
}

func (s *Server) pruneOrphanedThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	if cx == nil {
		writeInlineFlash(w, "err", "No active gallery.")
		return
	}
	s.goJob(w, models.JobTypePruneThumbs, "Thumbnail prune started.", func(ctx context.Context) {
		removed, processed, total, err := gallery.PruneOrphanThumbnails(ctx, cx.DB, cx.ThumbnailsPath, func(done, total int) {
			s.jobs.Update(done, total, fmt.Sprintf("[%s] pruning…", cx.Name))
		})
		s.jobs.Finish(err, ctx.Err() != nil,
			fmt.Sprintf("orphan sweep cancelled (%d/%d scanned, %d removed)", processed, total, removed),
			fmt.Sprintf("Removed %d orphaned thumbnail%s.", removed, plural(removed)))
	})
}

// emptyFolderListCap bounds the review list; past it the operator removes
// what is listed and scans again.
const emptyFolderListCap = 2000

func (s *Server) emptyFoldersScanPost(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	// Every outcome but the list clears the list slot, so an old list
	// never stays under a new flash.
	if cx == nil || cx.Degraded {
		writeFlashOOB(w, "flash-empty-folders", "", "")
		writeInlineFlash(w, "err", "Gallery path is unreadable.")
		return
	}
	dirs, err := gallery.ScanEmptyDirs(cx.Boundary())
	if err != nil {
		writeFlashOOB(w, "flash-empty-folders", "", "")
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(dirs) == 0 {
		writeFlashOOB(w, "flash-empty-folders", "", "")
		writeInlineFlash(w, "ok", "No empty folders.")
		return
	}
	s.renderTemplate(w, "partials/empty_folders.html", map[string]any{
		"Dirs":     dirs[:min(len(dirs), emptyFolderListCap)],
		"Withheld": max(len(dirs)-emptyFolderListCap, 0),
	})
}

// Holds a job slot so a concurrent move cannot lose a folder it just created.
func (s *Server) emptyFoldersRemovePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	paths := r.Form["folder"]
	if len(paths) == 0 {
		writeInlineFlash(w, "err", "No folders selected.")
		return
	}
	if !s.startJob(w, models.JobTypePruneDirs) {
		return
	}
	s.active().WaitIngests(s.jobs.Context())
	removed := gallery.RemoveEmptyDirs(s.boundary(), paths)
	s.jobs.Complete(fmt.Sprintf("Removed %d empty folder%s.", removed, plural(removed)))
	kept := ""
	if n := len(paths) - removed; n > 0 {
		kept = fmt.Sprintf(", %d no longer empty", n)
	}
	writeFlashOOB(w, "flash-empty-folders", "", "")
	writeInlineFlash(w, "ok", fmt.Sprintf("Removed %d empty folder%s%s.", removed, plural(removed), kept))
}

func (s *Server) recalcTagsPost(w http.ResponseWriter, r *http.Request) {
	updated, err := s.tagSvc().RecalcCount()
	if err != nil {
		writeInlineFlash(w, "err", fmt.Sprintf("Recalc partially completed (%d updated): %s", updated, err.Error()))
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Recalculated %d tag count%s.", updated, plural(int(updated))))
}

func (s *Server) unusedTagsGet(w http.ResponseWriter, r *http.Request) {
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/settings#maintenance", http.StatusSeeOther)
		return
	}
	plain, kept, err := s.tagSvc().UnusedTags()
	data := map[string]any{"Unused": len(plain), "Kept": len(kept), "All": len(plain) + len(kept)}
	if err != nil {
		data["Err"] = "Error: " + err.Error()
	}
	s.renderTemplate(w, "partials/unused_tags.html", data)
}

func (s *Server) unusedTagsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, kept, err := s.tagSvc().UnusedTags()
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if r.FormValue("include_kept") == "1" {
		ids = append(ids, kept...)
	}
	if len(ids) == 0 {
		writeInlineFlash(w, "ok", "No unused tags to delete.")
		return
	}
	s.goJob(w, models.JobTypeTag, fmt.Sprintf("Deleting %d unused tag%s.", len(ids), plural(len(ids))), func(context.Context) {
		s.runDeleteTagsByIDs(ids)
	})
}

func (s *Server) resetTagsGet(w http.ResponseWriter, r *http.Request) {
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/settings#maintenance", http.StatusSeeOther)
		return
	}
	_, ids, aliases, implications, err := s.tagSvc().TagResetScope()
	data := map[string]any{"Gallery": s.active().Name, "Tags": len(ids), "Aliases": aliases, "Implications": implications}
	if err != nil {
		data["Err"] = "Error: " + err.Error()
	}
	s.renderTemplate(w, "partials/reset_tags.html", data)
}

func (s *Server) resetTagsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if strings.TrimSpace(r.FormValue("confirm_name")) != s.active().Name {
		writeInlineFlash(w, "err", "type-to-confirm name does not match")
		return
	}
	ratingAliases, ids, _, _, err := s.tagSvc().TagResetScope()
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(ids) == 0 && len(ratingAliases) == 0 {
		writeInlineFlash(w, "ok", "No tags to reset.")
		return
	}
	s.goJob(w, models.JobTypeTag, fmt.Sprintf("Resetting the tags: deleting %d tag%s.", len(ids), plural(len(ids))), func(context.Context) {
		for _, id := range ratingAliases {
			if err := s.tagSvc().DeleteTag(id); err != nil {
				logx.Warnf("delete tag %d: %v", id, err)
			}
		}
		s.runDeleteTagsByIDs(ids)
	})
}

func (s *Server) tagCategoryConflictsPost(w http.ResponseWriter, r *http.Request) {
	n, err := s.tagSvc().ConflictsCount()
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	settled, err := s.tagSvc().DistinctSettledCount()
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	more := ""
	if settled > 0 {
		word := "more"
		if n == 0 {
			word = models.Plural(settled, "tag", "tags")
		}
		more = fmt.Sprintf(` <strong>%d</strong> %s %s <a href="/tags?conflicts=distinct">marked distinct</a> and not counted.`,
			settled, word, models.Plural(settled, "is", "are"))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if n == 0 {
		_, _ = fmt.Fprintf(w, `<div class="flash flash-ok">No tags share a name across categories.%s</div>`, more)
		return
	}
	_, _ = fmt.Fprintf(w,
		`<div class="flash flash-ok"><strong>%d</strong> tag%s share a name across categories - <a href="/tags?conflicts=1">review them on the Tags page</a>.%s</div>`,
		n, plural(n), more)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (s *Server) findFoldedDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.goJob(w, models.JobTypeFold, "Folded-duplicate scan started.", func(context.Context) {
		n, err := s.tagSvc().ScanFoldedDuplicates()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Found %d folded duplicate%s.", n, plural(n)))
	})
}

func (s *Server) removeDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// all=true is required, so a stray POST cannot wipe every alias file.
	if r.FormValue("all") != "true" {
		writeInlineFlash(w, "err", "No duplicate paths selected.")
		return
	}
	// Ceiling-filtered, so delete-all cannot take aliases the operator
	// cannot see.
	query := `
		SELECT ip.id
		FROM image_paths ip
		JOIN images i ON i.id = ip.image_id
		WHERE ip.is_canonical = 0`
	var args []any
	if where, wargs := resolveCeiling(r, s.active()).WhereOne("i.id"); where != "" {
		query += ` AND ` + where
		args = append(args, wargs...)
	}

	pathIDs, err := db.QueryIDs(s.db().Read, query, args...)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	if len(pathIDs) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 duplicate paths.")
		return
	}

	cx := s.active()
	s.goJob(w, models.JobTypeDelete, "Duplicate-path removal started.", func(ctx context.Context) {
		total := len(pathIDs)
		s.jobs.Update(0, total, "removing…")
		res, err := cx.DeleteCopies(ctx, pathIDs, func(done, total int) {
			s.jobs.Update(done, total, "removing…")
		})
		s.jobs.Finish(err, res.Cancelled, fmt.Sprintf("remove duplicates cancelled (%d/%d)", res.Removed, total), copiesSummary(res))
	})
}

func copiesSummary(res library.CopiesResult) string {
	var changed, gone, failed int
	for _, k := range res.Kept {
		switch {
		case errors.Is(k.Reason, gallery.ErrChangedSinceIndexed):
			changed++
		case errors.Is(k.Reason, gallery.ErrCanonicalGone):
			gone++
		default:
			failed++
		}
	}
	var kept []string
	if changed > 0 {
		kept = append(kept, fmt.Sprintf("%d changed since indexing", changed))
	}
	if gone > 0 {
		kept = append(kept, fmt.Sprintf("%d whose canonical file is gone", gone))
	}
	if failed > 0 {
		kept = append(kept, fmt.Sprintf("%d that could not be removed", failed))
	}
	if len(kept) == 0 {
		return fmt.Sprintf("Removed %d duplicate path%s.", res.Removed, plural(res.Removed))
	}
	return fmt.Sprintf("Removed %d duplicate path%s; kept %s.", res.Removed, plural(res.Removed), strings.Join(kept, ", "))
}

func (s *Server) rebuildThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.startRebuildThumbsJob(s.active()); err != nil {
		if errors.Is(err, jobs.ErrJobRunning) {
			flashStatus(w, http.StatusConflict, "A job is already running.")
			return
		}
		writeInlineFlash(w, "err", err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Thumbnail rebuild started.")
}

func (s *Server) startRebuildThumbsJob(cx *library.Gallery) error {
	if cx == nil || cx.DB == nil {
		return fmt.Errorf("no gallery context")
	}
	if err := s.jobs.Start(models.JobTypeRebuildThumbs); err != nil {
		return err
	}
	thumbnailsPath := cx.ThumbnailsPath
	galleryName := cx.Name
	go func() {
		type imgRow struct {
			ID       int64
			Path     string
			FileType string
			Width    sql.NullInt64
		}
		imgs, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(img *imgRow) []any {
			return []any{&img.ID, &img.Path, &img.FileType, &img.Width}
		}), `SELECT id, canonical_path, file_type, width FROM images WHERE is_missing = 0`)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		ctx := s.jobs.Context()
		processed, rebuilt := 0, 0
		total := len(imgs)
		failedNote := func() string {
			if processed == rebuilt {
				return ""
			}
			return fmt.Sprintf(", %d failed", processed-rebuilt)
		}
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("[%s] rebuild cancelled (%d/%d rebuilt%s)", galleryName, rebuilt, total, failedNote()))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("[%s] rebuilding…", galleryName))
			if err := gallery.Rebuild(img.Path, thumbnailsPath, img.ID, img.FileType); err != nil {
				logx.Warnf("rebuild thumbnail for %d: %v", img.ID, err)
			} else {
				rebuilt++
			}
			// An AVIF or JPEG XL decodes to answer, so it is probed only
			// when its size is missing.
			if gallery.IsVideoType(img.FileType) || (gallery.IsFFmpegStill(img.FileType) && !img.Width.Valid) {
				if w, h, ok := gallery.ProbeVideoDimensions(img.Path); ok {
					if err := cx.SetDimensions(ctx, img.ID, w, h); err != nil {
						logx.Warnf("backfill video dimensions for %d: %v", img.ID, err)
					}
				}
			}
			processed++
		}
		s.jobs.Complete(fmt.Sprintf("[%s] rebuilt %d thumbnail%s%s.", galleryName, rebuilt, plural(rebuilt), failedNote()))
	}()
	return nil
}

// phash first: it reads small thumbnails, so a cancel during the md5 pass
// over every original still leaves it done.
func (s *Server) computeHashesPost(w http.ResponseWriter, r *http.Request) {
	active := s.active()
	s.goJob(w, models.JobTypeHashes, "Hash backfill started.", func(ctx context.Context) {
		phashed, phashUpdated, err := active.BackfillPhashes(ctx, func(p, total int, _ string) {
			s.jobs.Update(p, total, "Perceptual hashes…")
		})
		if err != nil || ctx.Err() != nil {
			_ = s.jobs.Settle(ctx, err, fmt.Sprintf("hashes cancelled (%d phash, 0 md5)", phashUpdated), "")
			return
		}
		summed, sumUpdated, err := active.BackfillMD5s(ctx, func(p, total int, _ string) {
			s.jobs.Update(p, total, "MD5…")
		})
		_ = s.jobs.Settle(ctx, err, fmt.Sprintf("hashes cancelled (%d phash, %d md5)", phashUpdated, sumUpdated),
			fmt.Sprintf("Computed %d perceptual %s of %d and %d md5 digest%s of %d.",
				phashUpdated, models.Plural(phashUpdated, "hash", "hashes"), phashed, sumUpdated, plural(sumUpdated), summed))
	})
}

func (s *Server) generateMetaTagsPost(w http.ResponseWriter, r *http.Request) {
	active := s.active()
	s.goJob(w, models.JobTypeMetaTags, "Meta tag pass started.", func(ctx context.Context) {
		processed, updated, err := gallery.BackfillMetaTags(ctx, active.Handle, func(p, total int, msg string) {
			s.jobs.Update(p, total, cmp.Or(msg, "Meta tags…"))
		})
		_ = s.jobs.Settle(ctx, err, fmt.Sprintf("meta tags cancelled (%d processed, %d updated)", processed, updated),
			fmt.Sprintf("Derived meta tags for %d image%s; %d changed.", processed, plural(processed), updated))
	})
}

func (s *Server) indexWorkflowsPost(w http.ResponseWriter, r *http.Request) {
	active := s.active()
	s.goJob(w, models.JobTypeIndexWork, "Workflow indexing started.", func(ctx context.Context) {
		var indexed int
		err := active.Edit(func(d *db.DB) error {
			var err error
			indexed, err = gallery.RebuildComfyTerms(ctx, d, func(p, total int, msg string) {
				s.jobs.Update(p, total, cmp.Or(msg, "Workflows…"))
			})
			return err
		})
		_ = s.jobs.Settle(ctx, err, fmt.Sprintf("workflow index cancelled (%d indexed)", indexed),
			fmt.Sprintf("Indexed %d ComfyUI workflow%s.", indexed, plural(indexed)))
	})
}

func (s *Server) removeMetaTagsPost(w http.ResponseWriter, r *http.Request) {
	active := s.active()
	s.goJob(w, models.JobTypeMetaTags, "Removing generated meta tags.", func(ctx context.Context) {
		processed, removed, err := gallery.RemoveMetaTags(ctx, active.Handle, func(p, total int, msg string) {
			s.jobs.Update(p, total, cmp.Or(msg, "Meta tags…"))
		})
		_ = s.jobs.Settle(ctx, err, fmt.Sprintf("removal cancelled (%d image%s, %d tag%s removed)", processed, plural(processed), removed, plural(removed)),
			fmt.Sprintf("Removed %d generated tag%s from %d image%s.", removed, plural(removed), processed, plural(processed)))
	})
}

func (s *Server) vacuumDBPost(w http.ResponseWriter, r *http.Request) {
	s.goJob(w, models.JobTypeVacuum, "Vacuum started. Watch the status bar for the reclaimed-space report.", func(context.Context) {
		cx := s.active()
		beforeSize := dbFileSize(cx.DBPath)
		if err := cx.DB.Vacuum(context.Background()); errors.Is(err, db.ErrCheckpoint) {
			logx.Warnf("vacuum: %v", err)
		} else if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		afterSize := dbFileSize(cx.DBPath)
		freed := max(beforeSize-afterSize, 0)
		s.jobs.Complete(fmt.Sprintf("Vacuumed (reclaimed %s).", webui.HumanBytes(freed)))
	})
}

// Holds a job slot so an autotag cannot start before ReleaseAll and lose
// its worker mid-inference.
func (s *Server) freeMemoryPost(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Start(models.JobTypeFreeMemory); err != nil {
		flashStatus(w, http.StatusConflict, "A job is running; try again when it finishes.")
		return
	}
	defer s.jobs.Complete("Memory caches released.")
	before := desktop.ProcStatus("self", "VmRSS")
	ctxs := s.allContexts()
	for _, cx := range ctxs {
		if err := cx.ShrinkMemory(); errors.Is(err, db.ErrPoolBusy) {
			logx.Infof("free memory: shrink %q: %v", cx.Name, err)
		} else if err != nil {
			logx.Warnf("free memory: shrink %q: %v", cx.Name, err)
		}
	}
	debug.FreeOSMemory()
	tagger.ReleaseAll()
	after := desktop.ProcStatus("self", "VmRSS")
	if before > 0 && after > 0 && before > after {
		writeInlineFlash(w, "ok", "Freed "+webui.HumanBytes(int64(before-after))+".")
		return
	}
	writeInlineFlash(w, "ok", "Memory caches released.")
}

// Counts the WAL and shm too: after a mass delete the WAL can hold most
// of the pages.
func dbFileSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

func sameSeed(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func (s *Server) reExtractMetadataPost(w http.ResponseWriter, r *http.Request) {
	active := s.active()
	s.goJob(w, models.JobTypeReExtract, "Re-extraction started.", func(ctx context.Context) {
		type imgRow struct {
			ID        int64
			Path      string
			FileType  string
			sdHash    string
			comfyHash string
			sdSeed    *int64
			comfySeed *int64
			source    string
			duration  *float64
		}
		// Read into a slice rather than holding a read connection for the
		// whole run.
		imgs, err := db.QueryAll(active.DB.Read, db.ScanRow(func(img *imgRow) []any {
			return []any{&img.ID, &img.Path, &img.FileType, &img.source, &img.duration, &img.sdHash, &img.comfyHash, &img.sdSeed, &img.comfySeed}
		}), `
			SELECT i.id, i.canonical_path, i.file_type, i.source_type, i.duration_seconds,
			       COALESCE(sm.generation_hash, ''),
			       COALESCE(cm.generation_hash, ''),
			       sm.seed, cm.seed
			FROM images i
			LEFT JOIN sd_metadata sm ON sm.image_id = i.id
			LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id
			WHERE i.is_missing = 0
		`)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		processed := 0
		updated := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("re-extraction cancelled (%d/%d processed, %d updated)", processed, total, updated))
				return
			}
			s.jobs.Update(processed, total, "Processing…")
			if _, err := active.RecomputePhash(ctx, img.ID); err != nil {
				logx.Debugf("re-extract phash %d: %v", img.ID, err)
			}
			sdMeta, comfyMeta, sourceType := gallery.ExtractGenerationMeta(img.Path, img.FileType)

			newSDHash := ""
			var newSDSeed *int64
			if sdMeta != nil {
				newSDHash, newSDSeed = sdMeta.GenerationHash, sdMeta.Seed
			}
			newComfyHash := ""
			var newComfySeed *int64
			if comfyMeta != nil {
				newComfyHash, newComfySeed = comfyMeta.GenerationHash, comfyMeta.Seed
			}

			var durationSec *float64
			if gallery.IsVideoType(img.FileType) {
				if d, ok := gallery.ProbeDurationSeconds(img.Path); ok && (img.duration == nil || *img.duration != d) {
					durationSec = &d
				}
			}

			var manga *models.MangaMetadata
			mangaMoved := false
			if img.FileType == models.FileTypeCBZ {
				if m, err := gallery.ReadComicInfo(img.Path); err != nil {
					logx.Warnf("re-extract manga %d: %v", img.ID, err)
				} else {
					if m != nil {
						m.ImageID = img.ID
					}
					manga, mangaMoved = m, !reflect.DeepEqual(m, loadMangaMeta(ctx, active.DB, img.ID))
				}
			}

			// Unchanged hashes and seeds skip the rewrite (the hash leaves the
			// seed out).
			if newSDHash == img.sdHash && newComfyHash == img.comfyHash && sourceType == img.source && durationSec == nil && !mangaMoved &&
				sameSeed(newSDSeed, img.sdSeed) && sameSeed(newComfySeed, img.comfySeed) {
				processed++
				continue
			}

			if err := active.Edit(func(d *db.DB) error {
				return gallery.ApplyReExtract(ctx, d, img.ID, sourceType, durationSec, sdMeta, comfyMeta, manga, mangaMoved)
			}); err != nil {
				logx.Warnf("re-extract image %d: %v", img.ID, err)
				processed++
				continue
			}
			processed++
			updated++
			// source_type moved, so the ai-generated tag has to follow it.
			if err := gallery.ApplyMetaTags(active.Handle, img.ID); err != nil {
				logx.Debugf("re-extract meta tags %d: %v", img.ID, err)
			}
		}
		s.jobs.Complete(fmt.Sprintf("Re-extracted metadata for %d image%s (%d updated).", processed, plural(processed), updated))
	})
}
