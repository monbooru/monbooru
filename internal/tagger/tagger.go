//go:build tagger

// Package tagger runs ONNX models over images and turns what they emit
// into tags.
package tagger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func IsAvailable(cfg *config.Config) bool { return len(EnabledTaggers(cfg)) > 0 }

func buildSupportsInference() bool { return true }

func UnavailableReason(cfg *config.Config) string { return reasonFrom(DiscoverTaggers(cfg)) }

func reasonFrom(taggers []TaggerStatus) string {
	if len(enabledIn(taggers, nil)) > 0 {
		return ""
	}
	if len(taggers) == 0 {
		return "no tagger subfolders found under paths.model_path"
	}
	for _, t := range taggers {
		if t.Enabled && !t.Available {
			return t.Reason
		}
	}
	return "no enabled tagger"
}

func CheckProviderAvailable(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || provider == "cpu" {
		return nil
	}
	if !config.IsValidExecutionProvider(provider) {
		return fmt.Errorf("unsupported execution provider %q", provider)
	}

	if err := defaultBackend.probeProvider(provider); err != nil {
		return err
	}

	// A CUDA-capable library says nothing about the device: in a container
	// without the GPU passed in, session creation only fails at job time.
	if provider == "cuda" && runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err != nil {
			return fmt.Errorf("no NVIDIA GPU device found (pass the GPU into the container, e.g. Podman AddDevice=nvidia.com/gpu=all)")
		}
	}
	return nil
}

func AvailableTaggers(cfg *config.Config) []TaggerStatus { return DiscoverTaggers(cfg) }

func Status() CacheStatus {
	if b := activeBackend(); b != nil {
		return b.Status()
	}
	return CacheStatus{}
}

func ReleaseIdle(after time.Duration) bool {
	if b := activeBackend(); b != nil {
		return b.ReleaseIdle(after)
	}
	return false
}

func ReleaseAll() {
	if b := activeBackend(); b != nil {
		b.ReleaseAll()
	}
}

// A chunk's frames all sit on disk until its backend call returns, so a
// whole-library scope runs in chunks of this many images or frames.
const autotagChunkSize = 200

// RunWithTaggers takes only enabled, available taggers and returns how many
// ids were left untagged. provider overrides the configured one; a handle with
// no thumbnails path extracts pages under the system temp directory.
func RunWithTaggers(ctx context.Context, h gallery.Handle, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string) (int, error) {
	if len(taggers) == 0 {
		return 0, fmt.Errorf("no tagger is enabled or available")
	}
	backend := activeBackend()
	if backend == nil {
		return 0, fmt.Errorf("auto-tagger disabled (no backend registered)")
	}
	database := h.DB
	mangaCacheDir := gallery.MangaCacheDir(h.ThumbnailsPath)

	type catRow struct {
		id   int64
		name string
	}
	catIDs, err := tags.CategoryIDsByName(database.Read)
	if err != nil {
		logx.Warnf("tagger: read tag_categories: %v", err)
	}
	generalCatID := catIDs["general"]

	// single_general taggers can't tell a character from a general tag,
	// so a name found in exactly one other category (bar general and
	// meta) is filed there.
	inferredCats := map[string]int64{}
	hasSingleGeneral := slices.ContainsFunc(taggers, func(t TaggerStatus) bool {
		profile, perr := ResolveProfile(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		return perr == nil && profile.CategoryScheme == "single_general"
	})
	if hasSingleGeneral && generalCatID != 0 {
		// A name the user has tagged as general by hand stays general.
		inferred, err := db.QueryAllContext(ctx, database.Read, db.ScanRow(func(c *catRow) []any {
			return []any{&c.name, &c.id}
		}), `
			SELECT t.name, t.category_id
			FROM tags t
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE t.is_alias = 0
			  AND tc.name NOT IN ('general', 'meta')
			  AND NOT EXISTS (
			      SELECT 1 FROM tags g
			      JOIN image_tags it ON it.tag_id = g.id
			      WHERE g.name = t.name
			        AND g.category_id = ?
			        AND g.is_alias = 0
			        AND it.is_auto = 0
			  )`, generalCatID)
		if err == nil {
			ambiguous := map[string]bool{}
			for _, r := range inferred {
				if ambiguous[r.name] {
					continue
				}
				if existing, ok := inferredCats[r.name]; ok && existing != r.id {
					ambiguous[r.name] = true
					delete(inferredCats, r.name)
					continue
				}
				inferredCats[r.name] = r.id
			}
		}
	}

	parallel := min(max(1, cfg.Tagger.Parallel), len(ids))

	// The job has one status line, so each worker writes its own slot and
	// every update joins them all.
	total := len(ids)
	var completed atomic.Int64
	var statusMu sync.Mutex
	workerStatus := make([]string, parallel)
	// More than three workers' pages overflow the status slot.
	const maxVisibleWorkers = 3
	emitStatus := func(workerIdx int, msg string) {
		statusMu.Lock()
		defer statusMu.Unlock()
		workerStatus[workerIdx] = msg
		active := slices.DeleteFunc(slices.Clone(workerStatus), func(s string) bool { return s == "" })
		out := "tagging images"
		if len(active) > 0 {
			shown := active
			if len(shown) > maxVisibleWorkers {
				shown = shown[:maxVisibleWorkers]
			}
			out = strings.Join(shown, " · ")
			if extra := len(active) - len(shown); extra > 0 {
				out = fmt.Sprintf("%s (+%d more)", out, extra)
			}
		}
		mgr.Update(int(completed.Load()), total, out)
	}

	taggerNames := make([]string, 0, len(taggers))
	for _, t := range taggers {
		taggerNames = append(taggerNames, t.Name)
	}

	var skipped atomic.Int64
	var requests []BackendImageRequest
	var cleanups []func()
	frames := 0
	runChunk := func() error {
		defer func() {
			for _, c := range cleanups {
				c()
			}
			requests, cleanups, frames = nil, nil, 0
		}()
		resp, err := backend.Run(ctx, RunRequest{
			ModelPath:          cfg.Paths.ModelPath,
			DataPath:           cfg.Paths.DataPath,
			SessionParallel:    cfg.Tagger.Parallel,
			IdleReleaseMinutes: cfg.Tagger.IdleReleaseAfterMinutes,
			Taggers:            taggers,
			Provider:           provider,
			CatIDs:             catIDs,
			GeneralCatID:       generalCatID,
			InferredCats:       inferredCats,
			MinHitFraction:     cfg.Tagger.Aggregation.MinHitFraction,
			Parallel:           parallel,
			Images:             requests,
			OnProgress: func(workerIdx int, msg string) {
				if msg == "" {
					completed.Add(1)
				}
				emitStatus(workerIdx, msg)
			},
		})
		if err != nil {
			return err
		}

		for _, r := range resp.Results {
			if r.Err != "" {
				skipped.Add(1)
				continue
			}
			if r.Tags == nil {
				// Cancelled mid-image - skip writing partial state.
				continue
			}
			if storeErr := h.TagSvc.ApplyTaggerResults(ctx, r.ID, taggerHits(r.Tags), taggerNames); storeErr != nil {
				logx.Warnf("tagger: store results for image %d: %v", r.ID, storeErr)
				skipped.Add(1)
			}
		}
		return nil
	}

	for i, imageID := range ids {
		if ctx.Err() != nil {
			break
		}
		// Extraction can take minutes on a video-heavy scope, so it
		// reports progress too.
		mgr.Update(int(completed.Load()), total, fmt.Sprintf("preparing %d/%d", i+1, total))
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
			skipped.Add(1)
			continue
		}
		framePaths, cleanup := framesForTagging(canonPath, fileType, mangaCacheDir, imageID)
		if len(framePaths) == 0 {
			logx.Warnf("tagger: skip image %d: no frames available (missing file, archive, or ffmpeg)", imageID)
			skipped.Add(1)
			cleanup()
			continue
		}
		requests = append(requests, BackendImageRequest{
			ID:            imageID,
			FramePaths:    framePaths,
			MangaProgress: fileType == "cbz" && len(framePaths) > 1,
			FileType:      fileType,
		})
		cleanups = append(cleanups, cleanup)
		frames += len(framePaths)
		// An image is the unit of parallelism, so a chunk of long
		// archives still holds one per worker.
		if len(requests) >= autotagChunkSize || (frames >= autotagChunkSize && len(requests) >= parallel) {
			if err := runChunk(); err != nil {
				return int(skipped.Load()), err
			}
		}
	}
	if len(requests) > 0 {
		if err := runChunk(); err != nil {
			return int(skipped.Load()), err
		}
	}

	mgr.Update(int(completed.Load()), total, "tagging images")
	return int(skipped.Load()), ctx.Err()
}

func framesForTagging(canonPath, fileType, mangaCacheDir string, imageID int64) ([]string, func()) {
	switch fileType {
	case "mp4", "webm":
		positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
		frames, _ := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
		cleanup := func() {
			for _, p := range frames {
				_ = os.Remove(p)
			}
		}
		return frames, cleanup
	case "avif", "jxl":
		frame, err := gallery.RenderStillFrame(canonPath, os.TempDir(), gallery.ViewMaxDim)
		if err != nil {
			return nil, func() {}
		}
		return []string{frame}, func() { _ = os.Remove(frame) }
	case "cbz":
		archive, err := gallery.OpenManga(canonPath)
		if err != nil {
			logx.Warnf("tagger: open manga %q: %v", canonPath, err)
			return nil, func() {}
		}
		pageCount := len(archive.Pages)
		_ = archive.Close()
		// Beside the reader's cache, not in it: its reclaimer unlinks pages
		// idle past their TTL, which inference over a chunk can outlast.
		root := ""
		if mangaCacheDir != "" {
			root = filepath.Dir(mangaCacheDir)
		}
		tempDir, err := os.MkdirTemp(root, "autotag-*")
		if err != nil {
			logx.Warnf("tagger: temp dir for manga frames: %v", err)
			return nil, func() {}
		}
		paths := make([]string, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			path, err := gallery.EnsureMangaPageInCache(tempDir, canonPath, imageID, i)
			if err != nil {
				logx.Warnf("tagger: extract page %d of %q: %v", i, canonPath, err)
				continue
			}
			if gallery.IsFFmpegStill(models.ExtFileType(path)) {
				frame, err := gallery.RenderStillFrame(path, tempDir, gallery.ViewMaxDim)
				if err != nil {
					logx.Warnf("tagger: render page %d of %q: %v", i, canonPath, err)
					continue
				}
				path = frame
			}
			paths = append(paths, path)
		}
		return paths, func() { _ = os.RemoveAll(tempDir) }
	}
	return []string{canonPath}, func() {}
}

func taggerHits(merged map[TagKey]Scored) []tags.TaggerHit {
	hits := make([]tags.TaggerHit, 0, len(merged))
	for k, s := range merged {
		hits = append(hits, tags.TaggerHit{Name: k.Name, CategoryID: k.CatID, Score: s.Score, Tagger: s.TaggerName})
	}
	return hits
}
