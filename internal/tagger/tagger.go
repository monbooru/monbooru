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

	plan := make([]plannedTagger, len(taggers))
	for i, t := range taggers {
		if plan[i], err = planTagger(cfg.Paths.ModelPath, cfg.Paths.DataPath, t, catIDs, generalCatID, inferredCats); err != nil {
			return 0, err
		}
	}
	base := RunRequest{
		ModelPath:       cfg.Paths.ModelPath,
		DataPath:        cfg.Paths.DataPath,
		SessionParallel: cfg.Tagger.Parallel,
		Taggers:         taggers,
		Provider:        provider,
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
	tagOne := func(ctx context.Context, workerIdx int, imageID int64) error {
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
			skipped.Add(1)
			return nil
		}
		framePaths, cleanup := framesForTagging(canonPath, fileType, mangaCacheDir, imageID)
		defer cleanup()
		if len(framePaths) == 0 {
			logx.Warnf("tagger: skip image %d: no frames available (missing file, archive, or ffmpeg)", imageID)
			skipped.Add(1)
			return nil
		}
		var onPage func(int)
		if fileType == models.FileTypeCBZ && len(framePaths) > 1 {
			onPage = func(i int) {
				emitStatus(workerIdx, fmt.Sprintf("image %d: page %d/%d", imageID, i+1, len(framePaths)))
			}
		}
		scores, inferred, err := inferFrames(ctx, backend, base, plan, imageID, framePaths, fileType, onPage)
		if err != nil {
			return err
		}
		// An image without a single inferred frame is skipped, not stored:
		// an empty result deletes its auto-tags.
		if !inferred {
			skipped.Add(1)
			return nil
		}
		minHits := ResolveMinHits(cfg.Tagger.Aggregation.MinHitFraction, len(framePaths))
		merged := mergeTaggers(taggers, plan, scores, minHits)
		if err := h.TagSvc.ApplyTaggerResults(ctx, imageID, taggerHits(merged), taggerNames); err != nil {
			logx.Warnf("tagger: store results for image %d: %v", imageID, err)
			skipped.Add(1)
		}
		return nil
	}

	// Each worker prepares an image's frames and waits on the backend, so
	// at most parallel images sit decoded or extracted at once.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	queue := make(chan int64)
	var wg sync.WaitGroup
	for workerIdx := range parallel {
		wg.Go(func() {
			for id := range queue {
				if runCtx.Err() != nil {
					continue
				}
				if err := tagOne(runCtx, workerIdx, id); err != nil {
					stop(err)
					continue
				}
				if runCtx.Err() == nil {
					completed.Add(1)
				}
				emitStatus(workerIdx, "")
			}
		})
	}
feed:
	for _, id := range ids {
		select {
		case queue <- id:
		case <-runCtx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	if cfg.Tagger.IdleReleaseAfterMinutes <= 0 {
		backend.ReleaseAll()
	}
	if err := ctx.Err(); err != nil {
		return int(skipped.Load()), err
	}
	if err := context.Cause(runCtx); err != nil {
		return int(skipped.Load()), err
	}
	mgr.Update(int(completed.Load()), total, "tagging images")
	return int(skipped.Load()), nil
}

// inferFrames runs every frame through every tagger, one backend call per
// frame and input, and reports whether any frame was inferred. A frame
// that fails to read or that a model refuses is logged and left out.
func inferFrames(ctx context.Context, backend Backend, base RunRequest, plan []plannedTagger, imageID int64, framePaths []string, fileType string, onPage func(int)) ([][]SparseScores, bool, error) {
	type group struct {
		input   inputKey
		models  []string
		taggers []int
	}
	var groups []group
	for tIdx, p := range plan {
		g := slices.IndexFunc(groups, func(g group) bool { return g.input == p.input })
		if g < 0 {
			groups = append(groups, group{input: p.input})
			g = len(groups) - 1
		}
		groups[g].models = append(groups[g].models, base.Taggers[tIdx].Name)
		groups[g].taggers = append(groups[g].taggers, tIdx)
	}
	inputs := make([]inputKey, len(groups))
	for i, g := range groups {
		inputs[i] = g.input
	}

	scores := make([][]SparseScores, len(plan))
	inferred := false
	for fIdx, fp := range framePaths {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if onPage != nil {
			onPage(fIdx)
		}
		frames, err := prepareFrame(fp, fileType, inputs)
		if err != nil {
			logx.Warnf("tagger: read frame failed: image %d frame %d/%d (%s): %v",
				imageID, fIdx+1, len(framePaths), fp, err)
			continue
		}
		for g, grp := range groups {
			req := base
			req.Models, req.Size, req.Frames = grp.models, grp.input.size, [][]byte{frames[g]}
			resp, err := backend.Run(ctx, req)
			if err != nil {
				return nil, false, err
			}
			if len(resp.Results) != len(grp.models) {
				return nil, false, fmt.Errorf("tagger backend answered %d models, want %d", len(resp.Results), len(grp.models))
			}
			for m, tIdx := range grp.taggers {
				if len(resp.Results[m]) != 1 {
					return nil, false, fmt.Errorf("tagger backend answered %d frames, want 1", len(resp.Results[m]))
				}
				fr := resp.Results[m][0]
				if fr.Err != "" {
					logx.Warnf("tagger: inference failed: image %d via %q frame %d/%d (%s): %s",
						imageID, grp.models[m], fIdx+1, len(framePaths), fp, fr.Err)
					continue
				}
				inferred = true
				scores[tIdx] = append(scores[tIdx], fr.SparseScores)
			}
		}
	}
	return scores, inferred, nil
}

func framesForTagging(canonPath, fileType, mangaCacheDir string, imageID int64) ([]string, func()) {
	switch {
	case gallery.IsVideoType(fileType):
		positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
		frames, _ := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
		cleanup := func() {
			for _, p := range frames {
				_ = os.Remove(p)
			}
		}
		return frames, cleanup
	case gallery.IsFFmpegStill(fileType):
		frame, err := gallery.RenderStillFrame(canonPath, os.TempDir(), gallery.ViewMaxDim)
		if err != nil {
			return nil, func() {}
		}
		return []string{frame}, func() { _ = os.Remove(frame) }
	case fileType == models.FileTypeCBZ:
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
