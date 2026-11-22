// Package tagger runs ONNX models over images and turns what they emit
// into tags.
package tagger

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tags"
)

// IsAvailable is whether a worker that holds an enabled tagger can take a
// job now.
func IsAvailable(cfg *config.Config) bool { return len(EnabledTaggers(cfg)) > 0 }

func UnavailableReason(cfg *config.Config) string { return reasonFrom(DiscoverTaggers(cfg)) }

func reasonFrom(taggers []TaggerStatus) string {
	if len(enabledIn(taggers, nil)) > 0 {
		return ""
	}
	if !currentPool().HasWorker() {
		return "no worker"
	}
	if len(taggers) == 0 {
		return "no worker holds a tagger"
	}
	for _, t := range taggers {
		if t.Enabled && !t.Available {
			return t.Reason
		}
	}
	return "no enabled tagger"
}

func AvailableTaggers(cfg *config.Config) []TaggerStatus { return DiscoverTaggers(cfg) }

// RunWithTaggers takes only enabled, available taggers and returns how many
// ids were left untagged; a handle with no thumbnails path extracts pages
// under the system temp directory.
func RunWithTaggers(ctx context.Context, h gallery.Handle, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager) (Result, error) {
	return RunOn(ctx, h, cfg, ids, taggers, nil, mgr)
}

// Result is what a run left: the ids skipped, and the images each worker
// tagged, an image counting for the worker that answered its first call.
type Result struct {
	Skipped int
	Tagged  map[string]int
}

// Shares names each worker's images, most first, once more than one
// worker tagged: " (gpu-box 171, laptop 69)".
func (r Result) Shares() string {
	if len(r.Tagged) < 2 {
		return ""
	}
	return r.shares()
}

func (r Result) shares() string {
	names := slices.Collect(maps.Keys(r.Tagged))
	slices.SortFunc(names, func(a, b string) int { return cmp.Or(r.Tagged[b]-r.Tagged[a], strings.Compare(a, b)) })
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s %d", n, r.Tagged[n])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// Total is the images the run stored.
func (r Result) Total() int {
	n := 0
	for _, c := range r.Tagged {
		n += c
	}
	return n
}

// Workers names every worker's images, most first, one worker included:
// " (gpu-box 171)".
func (r Result) Workers() string {
	if len(r.Tagged) == 0 {
		return ""
	}
	return r.shares()
}

// RunOn is RunWithTaggers on the workers named in only, all of them
// sharing the images; nil leaves the choice to the pool.
func RunOn(ctx context.Context, h gallery.Handle, cfg *config.Config, ids []int64, taggers []TaggerStatus, only []string, mgr *jobs.Manager) (Result, error) {
	if len(taggers) == 0 {
		return Result{}, fmt.Errorf("no tagger is enabled or available")
	}
	pool := currentPool()
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
		h, ok := currentPool().heldModel(t.Name)
		return ok && h.labels.CategoryScheme == "single_general"
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
		if plan[i], err = planTagger(cfg.Paths.DataPath, t, catIDs, generalCatID, inferredCats); err != nil {
			return Result{}, err
		}
	}
	groups := callGroups(taggers, plan)
	// A person waiting on one image passes the batch jobs at the workers.
	priority := worker.PriorityBatch
	if len(ids) == 1 {
		priority = worker.PriorityInteractive
	}

	// One image per slot of the workers that can take the job, so each
	// worker pulls as fast as it answers and no frame waits decoded.
	calls := make([][]string, len(groups))
	for i, g := range groups {
		calls[i] = g.models
	}
	parallel := min(max(1, pool.Slots(calls, only)), len(ids))
	if n := cfg.Tagger.CPUThreads; n > 0 {
		parallel = min(parallel, n)
	}

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
	var taggedMu sync.Mutex
	tagged := map[string]int{}
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
		scores, inferred, by, err := inferFrames(ctx, pool, only, priority, groups, len(plan), imageID, framePaths, fileType, onPage)
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
			return nil
		}
		taggedMu.Lock()
		tagged[by]++
		taggedMu.Unlock()
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

	res := Result{Skipped: int(skipped.Load()), Tagged: tagged}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := context.Cause(runCtx); err != nil {
		return res, err
	}
	mgr.Update(int(completed.Load()), total, "tagging images")
	return res, nil
}

// inferFrames runs every frame through every tagger, one call per frame
// and input, and reports whether any frame was inferred and which worker
// answered the first call. A frame that fails to read or that a model
// refuses is logged and left out.
// callGroup is one call per frame: the taggers that take the same input.
type callGroup struct {
	input   inputKey
	models  []string
	taggers []int
}

func callGroups(taggers []TaggerStatus, plan []plannedTagger) []callGroup {
	var groups []callGroup
	for tIdx, p := range plan {
		g := slices.IndexFunc(groups, func(g callGroup) bool { return g.input == p.input })
		if g < 0 {
			groups = append(groups, callGroup{input: p.input})
			g = len(groups) - 1
		}
		groups[g].models = append(groups[g].models, taggers[tIdx].Name)
		groups[g].taggers = append(groups[g].taggers, tIdx)
	}
	return groups
}

func inferFrames(ctx context.Context, pool *Pool, only []string, priority string, groups []callGroup, nTaggers int, imageID int64, framePaths []string, fileType string, onPage func(int)) (_ [][]SparseScores, _ bool, by string, _ error) {
	inputs := make([]inputKey, len(groups))
	for i, g := range groups {
		inputs[i] = g.input
	}

	scores := make([][]SparseScores, nTaggers)
	inferred := false
	for fIdx, fp := range framePaths {
		if err := ctx.Err(); err != nil {
			return nil, false, "", err
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
			resp, name, err := pool.Tag(ctx, worker.TagRequest{Models: grp.models, Priority: priority, Frames: worker.FrameSpec{Size: grp.input.size}}, [][]byte{frames[g]}, only)
			if err != nil {
				return nil, false, "", err
			}
			by = cmp.Or(by, name)
			for m, tIdx := range grp.taggers {
				fr := resp.Results[m].Frames[0]
				if fr.Err != "" {
					logx.Warnf("tagger: inference failed: image %d via %q frame %d/%d (%s): %s",
						imageID, grp.models[m], fIdx+1, len(framePaths), fp, fr.Err)
					continue
				}
				inferred = true
				scores[tIdx] = append(scores[tIdx], SparseScores{I: fr.I, S: fr.S})
			}
		}
	}
	return scores, inferred, by, nil
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
