//go:build tagger

package tagger

import (
	"context"
	"errors"
	"image/color"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	ort "github.com/yalue/onnxruntime_go"
)

func TestParseDirectMLDeviceID(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"0", 0},
		{"2", 2},
		{"-1", 0},
		{"gpu", 0},
	}
	for _, c := range cases {
		if got := parseDirectMLDeviceID(c.in); got != c.want {
			t.Errorf("parseDirectMLDeviceID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSessionWorkersShrinkWithTheInputArea(t *testing.T) {
	cases := []struct {
		provider                  string
		parallel, inputSize, want int
	}{
		{"cpu", 4, 0, 4},
		{"cpu", 4, 384, 4},
		{"cpu", 4, 448, 4},
		{"cpu", 4, 512, 3},
		{"cpu", 4, 1008, 1},
		{"cpu", 8, 1008, 1},
		{"cpu", 16, 1008, 3},
		{"cpu", 1, 1008, 1},
		{"cuda", 4, 448, 4},
		{"cuda", 4, 1008, 1},
		{"cuda", 16, 1008, 3},
		{"openvino", 4, 1008, 1},
		{"directml", 4, 448, 1},
		{"directml", 16, 1008, 1},
	}
	for _, c := range cases {
		if got := sessionWorkers(c.provider, c.parallel, c.inputSize); got != c.want {
			t.Errorf("sessionWorkers(%q, %d, %d) = %d, want %d", c.provider, c.parallel, c.inputSize, got, c.want)
		}
	}
}

func TestProviderProbeReusesALiveEnvironment(t *testing.T) {
	requireORT(t)
	defaultBackend.mu.Lock()
	err := defaultBackend.ensure(RunRequest{Provider: "cpu"})
	defaultBackend.mu.Unlock()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	t.Cleanup(defaultBackend.ReleaseAll)

	err = CheckProviderAvailable("cuda")
	if err == nil {
		t.Skip("this onnxruntime library supports CUDA; the probe needs a CPU-only one")
	}
	if strings.Contains(err.Error(), "already been initialized") || !strings.Contains(err.Error(), "could not load the cuda provider") {
		t.Errorf("probe with a live environment: %v, want the provider's load error", err)
	}
	if !defaultBackend.Status().Loaded {
		t.Error("the probe tore down the live environment")
	}
}

func TestProbeErrorKeepsTheRuntimeMessageAndExplainsError126(t *testing.T) {
	missing := errors.New(`LoadLibrary failed with error 126 "" when trying to load "C:\monbooru\onnxruntime_providers_tensorrt.dll"`)
	got := probeError("tensorrt", missing).Error()
	if strings.Contains(got, "does not support") {
		t.Errorf("a provider that is there with a DLL missing reads as unsupported: %s", got)
	}
	if !strings.Contains(got, missing.Error()) || !strings.HasSuffix(got, "(a DLL the provider needs was not found)") {
		t.Errorf("probeError = %s, want the runtime's message and the missing-DLL hint", got)
	}

	named := errors.New("Failed to load library libonnxruntime_providers_cuda.so with error: libcublasLt.so.12: cannot open shared object file")
	if got, want := probeError("cuda", named).Error(), "ONNX Runtime could not load the cuda provider: "+named.Error(); got != want {
		t.Errorf("probeError = %s, want %s", got, want)
	}
}

func TestRunWithTwoTaggersMatchesEachAlone(t *testing.T) {
	requireORT(t)
	modelPath := modelsDir(t)
	names := []string{"wd-swinv2", "animetimm-eva02"}
	var taggers []TaggerStatus
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(modelPath, name, "model.onnx")); err != nil {
			t.Skipf("%s model not present: %v", name, err)
		}
		taggers = append(taggers, TaggerStatus{TaggerInstance: config.TaggerInstance{
			Name: name, ModelFile: "model.onnx", TagsFile: "tags.csv", ConfidenceThreshold: 0.1,
		}})
	}

	dir := t.TempDir()
	frames := []string{filepath.Join(dir, "a.png"), filepath.Join(dir, "b.png")}
	mustWriteFile(t, frames[0], solidPNG(t, 300, 200, color.RGBA{90, 120, 160, 255}), 0o644)
	mustWriteFile(t, frames[1], solidPNG(t, 120, 260, color.RGBA{220, 180, 150, 255}), 0o644)

	b := &inprocBackend{}
	t.Cleanup(b.ReleaseAll)
	cats := map[string]int64{"general": 1, "character": 2, "artist": 3, "copyright": 4, "meta": 5, "rating": 6}
	run := func(ts []TaggerStatus) map[TagKey]Scored { return runOne(t, b, modelPath, ts, cats, frames) }

	both := run(taggers)
	want := run(taggers[:1])
	for k, s := range run(taggers[1:]) {
		if prev, ok := want[k]; !ok || s.Score > prev.Score {
			want[k] = s
		}
	}
	if len(want) == 0 {
		t.Fatal("no tag cleared the threshold; the comparison is vacuous")
	}
	if !maps.Equal(both, want) {
		t.Errorf("two taggers in one run gave %d tags, each alone merged %d:\n%v\n%v", len(both), len(want), both, want)
	}
}

func TestEveryConfigProviderHasACase(t *testing.T) {
	requireORT(t)
	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		t.Skipf("ort init: %v", err)
	}
	defer func() { _ = ort.DestroyEnvironment() }()
	for _, p := range config.ExecutionProviders {
		opts, err := ort.NewSessionOptions()
		if err != nil {
			t.Fatal(err)
		}
		cleanup, err := appendExecutionProvider(opts, p.Name, "")
		if cleanup != nil {
			cleanup()
		}
		_ = opts.Destroy()
		if err != nil && strings.Contains(err.Error(), "unsupported execution provider") {
			t.Errorf("%s is a config value with no case: %v", p.Name, err)
		}
	}
}

func requireORT(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(sharedLibPath()); err != nil {
		t.Skipf("onnxruntime shared library not available: %v", err)
	}
}

func modelsDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("MONBOORU_PATHS_MODEL_PATH")
	if dir == "" {
		t.Skip("MONBOORU_PATHS_MODEL_PATH is not set")
	}
	modelPath, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return modelPath
}

func runOne(t *testing.T, b Backend, modelPath string, taggers []TaggerStatus, cats map[string]int64, frames []string) map[TagKey]Scored {
	t.Helper()
	plan := make([]plannedTagger, len(taggers))
	for i, tg := range taggers {
		var err error
		if plan[i], err = planTagger(modelPath, tg, cats, cats["general"], nil); err != nil {
			t.Fatal(err)
		}
	}
	base := RunRequest{ModelPath: modelPath, SessionParallel: 1, Taggers: taggers, Provider: "cpu"}
	scores, inferred, err := inferFrames(context.Background(), b, base, plan, 1, frames, "", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !inferred {
		t.Fatal("no frame was inferred")
	}
	return mergeTaggers(taggers, plan, scores, ResolveMinHits(0.05, len(frames)))
}

func TestONNXInputDimsMatchTheRuntime(t *testing.T) {
	requireORT(t)
	modelPath := modelsDir(t)
	defaultBackend.mu.Lock()
	defer defaultBackend.mu.Unlock()
	if !defaultBackend.initialized {
		ort.SetSharedLibraryPath(sharedLibPath())
		if err := ort.InitializeEnvironment(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ort.DestroyEnvironment() }()
	}
	models, _ := filepath.Glob(filepath.Join(modelPath, "*", "*.onnx"))
	if len(models) == 0 {
		t.Skip("no model installed")
	}
	for _, m := range models {
		inputs, _, err := ort.GetInputOutputInfo(m)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		dims, err := onnxInputDims(m)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if !slices.Equal(dims, []int64(inputs[0].Dimensions)) {
			t.Errorf("%s: read %v, the runtime says %v", m, dims, inputs[0].Dimensions)
		}
	}
}
