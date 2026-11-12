//go:build tagger

package tagger

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
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
	if strings.Contains(err.Error(), "already been initialized") || !strings.Contains(err.Error(), "does not support cuda") {
		t.Errorf("probe with a live environment: %v, want the library's missing-provider error", err)
	}
	if !defaultBackend.Status().Loaded {
		t.Error("the probe tore down the live environment")
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

func TestPrepareInputsRefusesAnEmptyImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 0, 0)), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareInputs(path, []loadedTagger{{loadedSession: &loadedSession{inputSize: 448, profile: Profile{Pad: "white_square"}}}}); err == nil {
		t.Error("prepareInputs took an image with no pixels")
	}
}

func TestPrepareInputsTurnsByEXIFOrientation(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			img.Set(x, y, color.RGBA{0, 0, 255, 255})
			if x < 20 && y < 10 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	exif := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	app1 := append([]byte{0xff, 0xe1, 0, byte(len(exif) + 2)}, exif...)
	path := filepath.Join(t.TempDir(), "turned.jpg")
	if err := os.WriteFile(path, slices.Concat(buf.Bytes()[:2], app1, buf.Bytes()[2:]), 0o644); err != nil {
		t.Fatal(err)
	}

	inputs, err := prepareInputs(path, []loadedTagger{{loadedSession: &loadedSession{inputSize: 40, profile: Profile{Pad: "white_square"}}}})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		x, y int
		red  bool
	}{{25, 5, true}, {15, 5, false}, {25, 35, false}, {15, 35, false}} {
		px := inputs[0].RGBAAt(c.x, c.y)
		if red := px.R > 200 && px.B < 60; red != c.red {
			t.Errorf("pixel (%d,%d) = %v, red %v, want %v", c.x, c.y, px, red, c.red)
		}
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

func runOne(t *testing.T, b *inprocBackend, modelPath string, taggers []TaggerStatus, cats map[string]int64, frames []string) map[TagKey]Scored {
	t.Helper()
	resp, err := b.Run(context.Background(), RunRequest{

		ModelPath: modelPath, SessionParallel: 1, IdleReleaseMinutes: 15,
		Taggers: taggers, Provider: "cpu",
		CatIDs: cats, GeneralCatID: cats["general"],
		MinHitFraction: 0.05, Parallel: 1,
		Images: []BackendImageRequest{{ID: 1, FramePaths: frames}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Results[0].Err != "" {
		t.Fatalf("image error: %s", resp.Results[0].Err)
	}
	return resp.Results[0].Tags
}
