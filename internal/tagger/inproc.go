//go:build tagger

package tagger

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	ort "github.com/yalue/onnxruntime_go"
)

// loadedTagger joins a cached session with this run's config, so
// threshold edits apply without a session rebuild.
type loadedTagger struct {
	*loadedSession
	cfg        config.TaggerInstance
	candidates []CandidateLabel
	dispatch   *DispatchTable
}

type loadedSession struct {
	modelFile  string
	tagsFile   string
	modelStamp fileStamp
	tagsStamp  fileStamp
	profileFP  string
	session    *ort.DynamicAdvancedSession
	labels     []tagLabel
	profile    Profile
	inputSize  int
}

// A file overwritten under the same name moves its size or mtime.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func stampOf(path string) (fileStamp, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, false
	}
	return fileStamp{fi.Size(), fi.ModTime()}, true
}

func (a fileStamp) same(b fileStamp) bool { return a.size == b.size && a.modTime.Equal(b.modTime) }

type inprocBackend struct {
	mu          sync.Mutex
	inUse       bool
	initialized bool
	provider    string
	workers     int
	sessionOpts *ort.SessionOptions
	epCleanup   func()
	sessions    map[string]*loadedSession
	lastUsed    time.Time
}

var defaultBackend = &inprocBackend{}

// UseInprocBackend is for the worker child, which must not spawn a grandchild.
func UseInprocBackend() {
	SetBackend(defaultBackend)
}

func init() {
	if os.Getenv("MONBOORU_TAGGER_BACKEND") == "inproc" {
		SetBackend(defaultBackend)
		return
	}
	SetBackend(&ipcBackend{})
}

// Caller must hold b.mu. The worker count joins the check since it also
// sets the intra-op threads, which only a reload changes; it is taken over
// the warm set, as ensure built it, so a subset of it reuses the sessions.
func (b *inprocBackend) satisfies(modelPath string, taggers []TaggerStatus, provider string, sessionParallel int) bool {
	if !b.initialized || b.provider != provider {
		return false
	}
	for _, t := range taggers {
		s, ok := b.sessions[t.Name]
		if !ok {
			return false
		}
		if s.modelFile != t.ModelFile || s.tagsFile != t.TagsFile {
			return false
		}
		model, okModel := stampOf(filepath.Join(modelPath, t.Name, t.ModelFile))
		tags, okTags := stampOf(filepath.Join(modelPath, t.Name, t.TagsFile))
		if !okModel || !okTags || !model.same(s.modelStamp) || !tags.same(s.tagsStamp) {
			return false
		}
		profile, err := ResolveProfile(modelPath, t.Name, t.TagsFile)
		if err != nil {
			return false
		}
		if profile.fingerprint() != s.profileFP {
			return false
		}
	}
	maxInput := 0
	for _, s := range b.sessions {
		maxInput = max(maxInput, s.inputSize)
	}
	return sessionWorkers(provider, max(1, sessionParallel), maxInput) == b.workers
}

// Caller must hold b.mu.
func (b *inprocBackend) ensure(req RunRequest) (err error) {
	taggers := req.Taggers
	// Before the signature check, so "" matches a warm "cpu" cache.
	provider := cmp.Or(req.Provider, "cpu")
	if b.satisfies(req.ModelPath, taggers, provider, req.SessionParallel) {
		logx.Infof("tagger: reusing warm cache (%d %s)", len(b.sessions), models.Plural(len(b.sessions), "session", "sessions"))
		return nil
	}
	if b.initialized {
		b.teardownLocked()
	}

	mode := strings.ToUpper(provider)
	logx.Infof("tagger: loading %d %s on %s", len(taggers), models.Plural(len(taggers), "session", "sessions"), mode)
	if provider == "directml" {
		logx.Warnf("tagger: directml forces parallel=1 (EP is not thread-safe)")
	}
	loadStart := time.Now()

	// CUDA caches JIT-compiled kernels under $HOME/.nv, which a container
	// loses on restart; keep them under the data path unless
	// CUDA_CACHE_PATH is already set.
	if provider == "cuda" && os.Getenv("CUDA_CACHE_PATH") == "" {
		cacheDir := filepath.Join(req.DataPath, ".nv-cache")
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			_ = os.Setenv("CUDA_CACHE_PATH", cacheDir)
		}
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	b.initialized = true
	defer func() {
		if err != nil {
			b.teardownLocked()
		}
	}()

	type pendingSession struct {
		name, onnxPath, input, output string
		loaded                        *loadedSession
	}
	pending := make([]pendingSession, 0, len(taggers))
	maxInput := 0
	for _, t := range taggers {
		onnxPath := filepath.Join(req.ModelPath, t.Name, t.ModelFile)
		tagsPath := filepath.Join(req.ModelPath, t.Name, t.TagsFile)
		// Before the reads: a write landing during the load then reloads.
		modelStamp, _ := stampOf(onnxPath)
		tagsStamp, _ := stampOf(tagsPath)
		profile, err := ResolveProfile(req.ModelPath, t.Name, t.TagsFile)
		if err != nil {
			return fmt.Errorf("resolve profile for %q: %w", t.Name, err)
		}
		labels, err := loadLabels(tagsPath, profile)
		if err != nil {
			return fmt.Errorf("load labels for %q: %w", t.Name, err)
		}
		inputs, outputs, err := ort.GetInputOutputInfo(onnxPath)
		if err != nil {
			return fmt.Errorf("inspect ort model for %q: %w", t.Name, err)
		}
		if len(inputs) == 0 || len(outputs) == 0 {
			return fmt.Errorf("ort model for %q has no input/output", t.Name)
		}
		outIdx := profile.OutputIndex
		if outIdx < 0 || outIdx >= len(outputs) {
			return fmt.Errorf("ort model for %q: profile output_index %d out of range (have %d)", t.Name, outIdx, len(outputs))
		}
		if err := checkLabelWidth(outputs[outIdx].Dimensions, len(labels)); err != nil {
			return fmt.Errorf("ort model for %q: %w", t.Name, err)
		}
		inputSize := profile.InputSize
		if inputSize == 0 {
			inputSize = inferInputSize(inputs[0].Dimensions, profile.Layout)
			if inputSize <= 0 {
				return fmt.Errorf("ort model for %q: cannot infer input size from dimensions %v", t.Name, inputs[0].Dimensions)
			}
		}
		maxInput = max(maxInput, inputSize)
		pending = append(pending, pendingSession{
			name:     t.Name,
			onnxPath: onnxPath,
			input:    inputs[0].Name,
			output:   outputs[outIdx].Name,
			loaded: &loadedSession{
				modelFile:  t.ModelFile,
				tagsFile:   t.TagsFile,
				modelStamp: modelStamp,
				tagsStamp:  tagsStamp,
				profileFP:  profile.fingerprint(),
				labels:     labels,
				profile:    profile,
				inputSize:  inputSize,
			},
		})
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return fmt.Errorf("ort session options: %w", err)
	}
	b.sessionOpts = opts

	parallel := sessionWorkers(provider, max(1, req.SessionParallel), maxInput)
	if provider != "directml" && parallel < req.SessionParallel {
		logx.Infof("tagger: a %d px input runs %d %s", maxInput, parallel, models.Plural(parallel, "worker", "workers"))
	}
	b.workers = parallel
	intra := max(1, runtime.NumCPU()/parallel)
	if err := opts.SetIntraOpNumThreads(intra); err != nil {
		return fmt.Errorf("set intra-op threads: %w", err)
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		return fmt.Errorf("set inter-op threads: %w", err)
	}
	// The arena keeps every concurrent Run's peak working set until the
	// session is destroyed; plain malloc lets mallocTrim hand it back.
	if err := opts.SetCpuMemArena(false); err != nil {
		return fmt.Errorf("disable cpu mem arena: %w", err)
	}

	// Under the data path so the compiled-model cache survives a
	// container restart.
	epCacheDir := ""
	if provider == "openvino" {
		dir := filepath.Join(req.DataPath, ".openvino-cache")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			epCacheDir = dir
		}
	}
	cleanup, err := appendExecutionProvider(opts, provider, epCacheDir)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("append execution provider %q: %w", provider, err)
	}
	b.epCleanup = cleanup
	b.provider = provider

	newSession := ort.NewDynamicAdvancedSession
	if provider == "directml" {
		newSession = newDirectMLSession
	}
	b.sessions = make(map[string]*loadedSession, len(pending))
	for _, p := range pending {
		session, err := newSession(p.onnxPath,
			[]string{p.input}, []string{p.output}, b.sessionOpts)
		if err != nil {
			return fmt.Errorf("create ort session for %q: %w", p.name, err)
		}
		p.loaded.session = session
		b.sessions[p.name] = p.loaded
	}
	logx.Infof("tagger: cache ready in %s", time.Since(loadStart).Round(10*time.Millisecond))
	return nil
}

// A second InitializeEnvironment fails while one is held, so a probe
// reuses the live environment and leaves it up.
func (b *inprocBackend) probeProvider(provider string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.initialized {
		ort.SetSharedLibraryPath(sharedLibPath())
		if err := ort.InitializeEnvironment(); err != nil {
			return fmt.Errorf("ort init: %w", err)
		}
		defer func() { _ = ort.DestroyEnvironment() }()
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return fmt.Errorf("ort session options: %w", err)
	}
	defer func() { _ = opts.Destroy() }()

	cleanup, err := appendExecutionProvider(opts, provider, "")
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		return fmt.Errorf("libonnxruntime does not support %s: %w", provider, err)
	}
	return nil
}

func parseDirectMLDeviceID(v string) int {
	if v == "" {
		return 0
	}
	id, err := strconv.Atoi(v)
	if err != nil || id < 0 {
		logx.Warnf("tagger: invalid MONBOORU_TAGGER_DIRECTML_DEVICE_ID %q, falling back to device 0", v)
		return 0
	}
	return id
}

func directmlDeviceID() int {
	return parseDirectMLDeviceID(os.Getenv("MONBOORU_TAGGER_DIRECTML_DEVICE_ID"))
}

// The caller must run a non-nil cleanup to free the provider options. An
// empty cacheDir skips the compile cache.
func appendExecutionProvider(opts *ort.SessionOptions, provider, cacheDir string) (cleanup func(), err error) {
	switch provider {
	case "cpu", "":
		return nil, nil
	case "cuda":
		// Without this ORT keeps a host copy of the weights for the
		// session's lifetime.
		if err := opts.AddSessionConfigEntry("session.use_device_allocator_for_initializers", "1"); err != nil {
			return nil, fmt.Errorf("ort session config: %w", err)
		}
		// The device arena never shrinks, and the pattern's block would sit
		// in it beside the first run's chunks, doubling the VRAM held.
		if err := opts.SetMemPattern(false); err != nil {
			return nil, fmt.Errorf("set cuda mem pattern: %w", err)
		}
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			return nil, fmt.Errorf("ort cuda options (ensure libonnxruntime was built with CUDA): %w", err)
		}
		// HEURISTIC skips cuDNN's multi-second exhaustive search,
		// kSameAsRequested grows the arena by the request instead of
		// doubling, and default-stream copies avoid a cross-stream sync.
		if err := cudaOpts.Update(map[string]string{
			"cudnn_conv_algo_search":    "HEURISTIC",
			"arena_extend_strategy":     "kSameAsRequested",
			"do_copy_in_default_stream": "1",
		}); err != nil {
			_ = cudaOpts.Destroy()
			return nil, fmt.Errorf("update cuda options: %w", err)
		}
		if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
			_ = cudaOpts.Destroy()
			return nil, fmt.Errorf("append cuda provider: %w", err)
		}
		return func() { _ = cudaOpts.Destroy() }, nil
	case "directml":
		// DirectML requires the memory pattern off and sequential execution.
		if err := opts.SetMemPattern(false); err != nil {
			return nil, fmt.Errorf("set directml mem pattern: %w", err)
		}
		if err := opts.SetExecutionMode(ort.ExecutionModeSequential); err != nil {
			return nil, fmt.Errorf("set directml execution mode: %w", err)
		}
		if err := opts.AppendExecutionProviderDirectML(directmlDeviceID()); err != nil {
			return nil, fmt.Errorf("append directml provider: %w", err)
		}
		return nil, nil
	case "tensorrt":
		trtOpts, err := ort.NewTensorRTProviderOptions()
		if err != nil {
			return nil, fmt.Errorf("ort tensorrt options (ensure libonnxruntime was built with TensorRT): %w", err)
		}
		if err := trtOpts.Update(map[string]string{}); err != nil {
			_ = trtOpts.Destroy()
			return nil, fmt.Errorf("update tensorrt options: %w", err)
		}
		if err := opts.AppendExecutionProviderTensorRT(trtOpts); err != nil {
			_ = trtOpts.Destroy()
			return nil, fmt.Errorf("append tensorrt provider: %w", err)
		}
		return func() { _ = trtOpts.Destroy() }, nil
	case "openvino":
		openvinoOpts := map[string]string{
			"device_type": "GPU",
		}
		if cacheDir != "" {
			loadCfg := map[string]any{
				"GPU": map[string]string{
					"CACHE_DIR":           cacheDir,
					"EXECUTION_MODE_HINT": "ACCURACY",
					"PERFORMANCE_HINT":    "LATENCY",
				},
			}
			loadCfgJSON, err := json.Marshal(loadCfg)
			if err != nil {
				return nil, fmt.Errorf("marshal openvino load_config: %w", err)
			}
			openvinoOpts["load_config"] = string(loadCfgJSON)
		}
		if err := opts.AppendExecutionProviderOpenVINO(openvinoOpts); err != nil {
			return nil, fmt.Errorf("append openvino provider: %w", err)
		}
		return nil, nil
	case "coreml":
		if err := opts.AppendExecutionProviderCoreML(0); err != nil {
			return nil, fmt.Errorf("append coreml provider: %w", err)
		}
		return nil, nil
	case "coremlv2":
		if err := opts.AppendExecutionProviderCoreMLV2(map[string]string{}); err != nil {
			return nil, fmt.Errorf("append coremlv2 provider: %w", err)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported execution provider %q", provider)
	}
}

// tagger.parallel is sized for the catalog's 448 px inputs. Activations
// grow with the input's pixel count, so a larger input runs fewer workers:
// on CPU the intra-op threads already fill the cores, and on a GPU every
// worker's working set shares the device's memory.
func sessionWorkers(provider string, parallel, inputSize int) int {
	const refSize = 448
	if provider == "directml" {
		// Concurrent Run on one session crashes the EP's allocator
		// (onnxruntime issue #22147), so its one worker gets every core.
		return 1
	}
	if inputSize <= refSize {
		return parallel
	}
	return max(1, parallel*refSize*refSize/(inputSize*inputSize))
}

func inferInputSize(dims ort.Shape, layout string) int {
	if len(dims) != 4 {
		return 0
	}
	var d int64
	switch layout {
	case "nhwc":
		d = dims[1]
	case "nchw":
		d = dims[2]
	default:
		return 0
	}
	if d <= 0 {
		return 0
	}
	return int(d)
}

func checkLabelWidth(dims ort.Shape, n int) error {
	if len(dims) == 0 {
		return nil
	}
	if w := dims[len(dims)-1]; w > 0 && w != int64(n) {
		return fmt.Errorf("%d labels for %d outputs", n, w)
	}
	return nil
}

func (b *inprocBackend) teardownLocked() {
	for _, s := range b.sessions {
		_ = s.session.Destroy()
	}
	b.sessions = nil
	if b.epCleanup != nil {
		b.epCleanup()
		b.epCleanup = nil
	}
	if b.sessionOpts != nil {
		_ = b.sessionOpts.Destroy()
		b.sessionOpts = nil
	}
	if b.initialized {
		_ = ort.DestroyEnvironment()
		b.initialized = false
	}
	b.provider = ""
	mallocTrim()
}

func (b *inprocBackend) ReleaseIdle(after time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse || !b.initialized {
		return false
	}
	if time.Since(b.lastUsed) < after {
		return false
	}
	b.teardownLocked()
	return true
}

// A run holds no lock while it infers, so its sessions stay until it ends.
func (b *inprocBackend) ReleaseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initialized && !b.inUse {
		b.teardownLocked()
	}
}

func (b *inprocBackend) Status() CacheStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.initialized {
		return CacheStatus{}
	}
	return CacheStatus{
		Loaded:   true,
		Provider: b.provider,
		InUse:    b.inUse,
		LastUsed: b.lastUsed,
		Sessions: slices.Sorted(maps.Keys(b.sessions)),
	}
}

func (b *inprocBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	if len(req.Taggers) == 0 {
		return RunResponse{}, fmt.Errorf("no tagger is enabled or available")
	}

	b.mu.Lock()
	if err := b.ensure(req); err != nil {
		b.mu.Unlock()
		return RunResponse{}, err
	}
	loaded := make([]loadedTagger, len(req.Taggers))
	for i, t := range req.Taggers {
		loaded[i] = loadedTagger{
			loadedSession: b.sessions[t.Name],
			cfg:           t.TaggerInstance,
			dispatch:      LoadDispatch(req.ModelPath, t.Name, req.CatIDs),
		}
	}
	workers := b.workers
	b.inUse = true
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.inUse = false
		b.lastUsed = time.Now()
		if req.IdleReleaseMinutes <= 0 {
			b.teardownLocked()
		} else {
			mallocTrim()
		}
		b.mu.Unlock()
	}()

	catNames := make(map[int64]string, len(req.CatIDs))
	for name, cid := range req.CatIDs {
		catNames[cid] = name
	}
	for i := range loaded {
		lt := &loaded[i]
		cands := make([]CandidateLabel, len(lt.labels))
		for idx, label := range lt.labels {
			if label.placeholder {
				cands[idx].Placeholder = true
				continue
			}
			res := resolveCategory(lt.profile, label, req.CatIDs, lt.dispatch)
			if res.skip {
				cands[idx].Placeholder = true
				continue
			}
			catID := res.catID
			name := label.name
			if rule, ok := lt.dispatch.Lookup(label.name); ok && rule.Name != "" {
				name = rule.Name
			}
			if !res.override &&
				lt.profile.CategoryScheme == "single_general" &&
				catID == req.GeneralCatID {
				if inferred, ok := req.InferredCats[label.name]; ok {
					catID = inferred
				}
			}
			cands[idx] = CandidateLabel{
				Name:    name,
				CatID:   catID,
				CatName: catNames[catID],
			}
		}
		lt.candidates = cands
	}

	parallel := min(max(1, req.Parallel), workers, len(req.Images))

	results := make([]BackendImageResult, len(req.Images))
	for i, im := range req.Images {
		results[i].ID = im.ID
	}

	// Without a periodic trim a long run grows glibc's arena until the
	// next idle release.
	const trimInterval = 256
	var sinceTrim atomic.Int64

	processOne := func(idx int, workerIdx int) {
		if ctx.Err() != nil {
			return
		}
		im := req.Images[idx]
		if len(im.FramePaths) == 0 {
			results[idx].Err = "no frames"
			return
		}
		merged := map[TagKey]Scored{}
		anyInferred := false
		minHits := ResolveMinHits(req.MinHitFraction, len(im.FramePaths))
		perFrame := make([][][]float32, len(loaded))
		for fIdx, fp := range im.FramePaths {
			if ctx.Err() != nil {
				return
			}
			if im.MangaProgress && req.OnProgress != nil {
				req.OnProgress(workerIdx, fmt.Sprintf("image %d: page %d/%d", im.ID, fIdx+1, len(im.FramePaths)))
			}
			inputs, err := prepareInputs(fp, im.FileType, loaded)
			if err != nil {
				logx.Warnf("tagger: read frame failed: image %d frame %d/%d (%s): %v",
					im.ID, fIdx+1, len(im.FramePaths), fp, err)
				continue
			}
			for tIdx, lt := range loaded {
				if ctx.Err() != nil {
					return
				}
				scores, err := inferImage(lt, inputs[tIdx])
				if err != nil {
					logx.Warnf("tagger: inference failed: image %d via %q frame %d/%d (%s): %v",
						im.ID, lt.cfg.Name, fIdx+1, len(im.FramePaths), fp, err)
					continue
				}
				anyInferred = true
				perFrame[tIdx] = append(perFrame[tIdx], scores)
			}
		}
		for tIdx, lt := range loaded {
			cands := AggregateInferenceScores(perFrame[tIdx], lt.candidates, AggregateOpts{
				MinHits:            minHits,
				GlobalThreshold:    float32(lt.cfg.ConfidenceThreshold),
				CategoryThresholds: lt.cfg.CategoryThresholds,
				PerCategoryTopK:    lt.cfg.PerCategoryTopK,
				DisabledCategories: lt.cfg.DisabledCategories,
			})
			for _, c := range cands {
				mk := TagKey{Name: c.Name, CatID: c.CatID}
				if prev, ok := merged[mk]; !ok || c.Score > prev.Score {
					merged[mk] = Scored{Score: c.Score, TaggerName: lt.cfg.Name}
				}
			}
		}
		// An error, not an empty map: storing an empty result deletes the
		// image's auto-tags.
		if !anyInferred {
			results[idx].Err = "all frames failed"
			return
		}
		results[idx].Tags = merged
	}

	queue := make(chan int, parallel)
	var wg sync.WaitGroup
	for workerIdx := range parallel {
		wg.Go(func() {
			for idx := range queue {
				if ctx.Err() != nil {
					continue
				}
				processOne(idx, workerIdx)
				if sinceTrim.Add(1) >= trimInterval {
					sinceTrim.Store(0)
					mallocTrim()
				}
				if req.OnProgress != nil {
					req.OnProgress(workerIdx, "")
				}
			}
		})
	}

	for idx := range req.Images {
		if ctx.Err() != nil {
			break
		}
		queue <- idx
	}
	close(queue)
	wg.Wait()

	return RunResponse{Results: results}, ctx.Err()
}

// prepareInputs decodes the frame once and pads it once per distinct
// input shape; the decoded frame is dropped before any inference runs.
func prepareInputs(path, fileType string, loaded []loadedTagger) ([]*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	img, err := gallery.DecodeImageWithCap(f)
	if err != nil {
		return nil, err
	}
	if img.Bounds().Empty() {
		return nil, fmt.Errorf("image has no pixels (%dx%d)", img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Turned after the padding: the square is centred, so it turns the
	// same, and the full-size frame is never copied.
	// A still image is read as the type sniffed at ingest, which can differ
	// from its extension; an extracted frame or page has its own name.
	switch fileType {
	case "", models.FileTypeMP4, models.FileTypeWEBM, models.FileTypeAVIF, models.FileTypeJXL, models.FileTypeCBZ:
		fileType = models.ExtFileType(path)
	}
	o, _ := metadata.EXIFOrientation(path, fileType)
	type padKey struct {
		size int
		fill [3]uint8
	}
	padded := make(map[padKey]*image.RGBA, len(loaded))
	inputs := make([]*image.RGBA, len(loaded))
	for i, lt := range loaded {
		k := padKey{lt.inputSize, padFill(lt.profile)}
		if padded[k] == nil {
			padded[k] = gallery.Orient(padAndResize(img, lt.inputSize, lt.profile), o).(*image.RGBA)
		}
		inputs[i] = padded[k]
	}
	return inputs, nil
}

func inferImage(lt loadedTagger, processed *image.RGBA) ([]float32, error) {
	tensor := acquireTensor(lt.inputSize)
	inputShape, err := buildTensor(processed, tensor, lt.inputSize, lt.profile)
	if err != nil {
		releaseTensor(lt.inputSize, tensor)
		return nil, err
	}
	inputTensor, err := ort.NewTensor(inputShape, tensor)
	if err != nil {
		releaseTensor(lt.inputSize, tensor)
		return nil, err
	}

	// The pool gets the buffer back only after Destroy: the tensor aliases it.
	outputs := []ort.Value{nil}
	runErr := lt.session.Run([]ort.Value{inputTensor}, outputs)
	_ = inputTensor.Destroy()
	releaseTensor(lt.inputSize, tensor)
	if runErr != nil {
		return nil, runErr
	}
	defer func() { _ = outputs[0].Destroy() }()

	outTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output type: %T", outputs[0])
	}
	// GetData is ORT-owned memory that the deferred Destroy frees, so it
	// is copied out.
	data := outTensor.GetData()
	out := make([]float32, len(data))
	if lt.profile.Activation == "logits" {
		for i, v := range data {
			out[i] = float32(1 / (1 + math.Exp(-float64(v))))
		}
		return out, nil
	}
	copy(out, data)
	return out, nil
}
