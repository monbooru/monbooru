//go:build tagger

package tagger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
)

func stagedSession(t *testing.T, modelPath, name, model, tags string) *loadedSession {
	t.Helper()
	dir := filepath.Join(modelPath, name)
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, model), []byte("onnx"), 0o644)
	mustWriteFile(t, filepath.Join(dir, tags), []byte("tags"), 0o644)
	modelStamp, _ := stampOf(filepath.Join(dir, model))
	tagsStamp, _ := stampOf(filepath.Join(dir, tags))
	return &loadedSession{
		modelFile: model, tagsFile: tags, modelStamp: modelStamp, tagsStamp: tagsStamp,
		profileFP: mustResolveProfile(t, modelPath, name, tags).fingerprint(),
		inputSize: 448,
	}
}

func TestCacheSatisfies(t *testing.T) {
	tmp := t.TempDir()
	c := inprocBackend{
		initialized: true,
		provider:    "cpu",
		workers:     1,
		sessions: map[string]*loadedSession{
			"wd-swinv2": stagedSession(t, tmp, "wd-swinv2", "model.onnx", "tags.csv"),
			"joytag":    stagedSession(t, tmp, "joytag", "model.onnx", "tags.txt"),
		},
	}

	mk := func(name, model, tags string) TaggerStatus {
		return TaggerStatus{TaggerInstance: config.TaggerInstance{
			Name: name, ModelFile: model, TagsFile: tags,
		}}
	}

	tests := []struct {
		name     string
		req      []TaggerStatus
		provider string
		want     bool
	}{
		{"single match", []TaggerStatus{mk("wd-swinv2", "model.onnx", "tags.csv")}, "cpu", true},
		{"both match", []TaggerStatus{
			mk("wd-swinv2", "model.onnx", "tags.csv"),
			mk("joytag", "model.onnx", "tags.txt"),
		}, "cpu", true},
		{"provider flip invalidates", []TaggerStatus{mk("wd-swinv2", "model.onnx", "tags.csv")}, "cuda", false},
		{"unknown tagger", []TaggerStatus{mk("ghost", "model.onnx", "tags.csv")}, "cpu", false},
		{"model file swapped", []TaggerStatus{mk("wd-swinv2", "v3.onnx", "tags.csv")}, "cpu", false},
		{"tags file swapped", []TaggerStatus{mk("wd-swinv2", "model.onnx", "selected_tags.csv")}, "cpu", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.satisfies(tmp, tc.req, tc.provider, 1); got != tc.want {
				t.Fatalf("satisfies: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCacheSatisfies_AWorkerCountChangeReloads(t *testing.T) {
	tmp := t.TempDir()
	big := stagedSession(t, tmp, "big", "model.onnx", "tags.csv")
	big.inputSize = 896
	wdSession := stagedSession(t, tmp, "wd-swinv2", "model.onnx", "tags.csv")
	c := inprocBackend{
		initialized: true,
		provider:    "cpu",
		workers:     sessionWorkers("cpu", 4, big.inputSize),
		sessions:    map[string]*loadedSession{"wd-swinv2": wdSession, "big": big},
	}
	wd := TaggerStatus{TaggerInstance: config.TaggerInstance{Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv"}}
	bigReq := TaggerStatus{TaggerInstance: config.TaggerInstance{Name: "big", ModelFile: "model.onnx", TagsFile: "tags.csv"}}
	if !c.satisfies(tmp, []TaggerStatus{wd, bigReq}, "cpu", 4) {
		t.Fatal("the loaded worker count must satisfy the same request")
	}
	if !c.satisfies(tmp, []TaggerStatus{wd}, "cpu", 4) {
		t.Error("a request naming a subset of the warm set reloaded it")
	}
	if c.satisfies(tmp, []TaggerStatus{wd}, "cpu", 8) {
		t.Error("a raised worker cap kept the warm sessions")
	}
	small := inprocBackend{
		initialized: true,
		provider:    "cpu",
		workers:     sessionWorkers("cpu", 4, wdSession.inputSize),
		sessions:    map[string]*loadedSession{"wd-swinv2": wdSession},
	}
	if small.satisfies(tmp, []TaggerStatus{wd, bigReq}, "cpu", 4) {
		t.Error("adding an 896 px tagger kept the warm set without it")
	}
}

func TestCacheSatisfies_Uninitialized(t *testing.T) {
	c := inprocBackend{}
	if c.satisfies("", nil, "cpu", 1) {
		t.Fatalf("uninitialized cache must not satisfy any request")
	}
}

func TestCacheSatisfies_ProfileSidecarInvalidates(t *testing.T) {

	tmp := t.TempDir()
	c := inprocBackend{
		initialized: true,
		workers:     1,
		sessions: map[string]*loadedSession{
			"wd-swinv2": stagedSession(t, tmp, "wd-swinv2", "model.onnx", "tags.csv"),
		},
	}
	req := []TaggerStatus{{TaggerInstance: config.TaggerInstance{
		Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv",
	}}}
	if !c.satisfies(tmp, req, "", 1) {
		t.Fatalf("baseline satisfies must hold before sidecar edit")
	}
	sidecar := `{"version":1,"profile":{"activation":"logits"}}`
	mustWriteFile(t, filepath.Join(tmp, "wd-swinv2", "tagger.json"), []byte(sidecar), 0o644)
	if c.satisfies(tmp, req, "", 1) {
		t.Errorf("sidecar edit must invalidate the cached profile fingerprint")
	}
}

func TestCacheSatisfies_AFileOverwrittenInPlaceInvalidates(t *testing.T) {
	req := []TaggerStatus{{TaggerInstance: config.TaggerInstance{
		Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv",
	}}}
	for _, file := range []string{"model.onnx", "tags.csv"} {
		t.Run(file, func(t *testing.T) {
			tmp := t.TempDir()
			c := inprocBackend{
				initialized: true,
				workers:     1,
				sessions: map[string]*loadedSession{
					"wd-swinv2": stagedSession(t, tmp, "wd-swinv2", "model.onnx", "tags.csv"),
				},
			}
			if !c.satisfies(tmp, req, "", 1) {
				t.Fatal("baseline satisfies must hold before the overwrite")
			}
			mustWriteFile(t, filepath.Join(tmp, "wd-swinv2", file), []byte("a newer file"), 0o644)
			if c.satisfies(tmp, req, "", 1) {
				t.Errorf("%s overwritten in place kept the warm session", file)
			}
		})
	}
}

func TestReleaseIdle_Cold(t *testing.T) {

	if ReleaseIdle(time.Hour) {
		t.Fatalf("ReleaseIdle on a cold cache must return false")
	}
}

func TestReleaseAll_Cold(t *testing.T) {

	ReleaseAll()
	ReleaseAll()
}

func TestReleaseAll_KeepsTheSessionsOfARunningInference(t *testing.T) {
	c := &inprocBackend{initialized: true, inUse: true, provider: "cpu"}
	c.ReleaseAll()
	if !c.initialized {
		t.Error("ReleaseAll tore the sessions down under a running inference")
	}
}

func TestEnsureEmptyProviderReusesCPUCache(t *testing.T) {

	tmp := t.TempDir()
	c := inprocBackend{
		initialized: true,
		provider:    "cpu",
		workers:     1,
		sessions: map[string]*loadedSession{
			"wd-swinv2": stagedSession(t, tmp, "wd-swinv2", "model.onnx", "tags.csv"),
		},
	}
	req := []TaggerStatus{{TaggerInstance: config.TaggerInstance{
		Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv",
	}}}
	if err := c.ensure(RunRequest{ModelPath: tmp, Taggers: req}); err != nil {
		t.Fatalf("ensure with empty provider: %v", err)
	}
}
