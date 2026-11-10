package tagger

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func makeTaggerDir(t *testing.T, tmpDir, name string, files []string) string {
	t.Helper()
	dir := filepath.Join(tmpDir, name)
	mustMkdirAll(t, dir, 0o755)
	for _, f := range files {
		mustWriteFile(t, filepath.Join(dir, f), nil, 0o644)
	}
	return tmpDir
}

func stubRuntimeLibrary(t *testing.T) {
	t.Helper()
	lib := filepath.Join(t.TempDir(), "libonnxruntime.so")
	mustWriteFile(t, lib, nil, 0o644)
	t.Setenv("ORT_LIB_PATH", lib)
}

func TestResolveTaggerFiles(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		files                       []string
		explicitModel, explicitTags string
		model, tags                 string
	}{
		{"Defaults", []string{"model.onnx", "tags.csv"}, "", "", "model.onnx", "tags.csv"},
		{"LoneOnnxAndCSV", []string{"weights.onnx", "top_tags.txt"}, "", "", "weights.onnx", "top_tags.txt"},
		{"ExplicitTOMLWins", []string{"model.onnx", "tags.csv", "selected.csv"}, "model.onnx", "selected.csv", "model.onnx", "selected.csv"},
		{"LoneJSONLabel", []string{"model.onnx", "camie-tagger-v2-metadata.json"}, "", "", "model.onnx", "camie-tagger-v2-metadata.json"},
		{"DefaultsBeatLone", []string{"model.onnx", "tags.csv", "extra.txt"}, "", "", "model.onnx", "tags.csv"},
		{"AmbiguousLabelFalls", []string{"model.onnx", "labels_a.csv", "labels_b.txt"}, "", "", "model.onnx", DefaultTagsFile},
		{"SidecarJSONIgnored", []string{"model.onnx", "tagger.json", "dispatch.json"}, "", "", "model.onnx", DefaultTagsFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			makeTaggerDir(t, tmp, "tagger", tc.files)
			model, tags := resolveTaggerFiles(filepath.Join(tmp, "tagger"), tc.explicitModel, tc.explicitTags)
			if model != tc.model || tags != tc.tags {
				t.Errorf("model, tags = %q, %q; want %q, %q", model, tags, tc.model, tc.tags)
			}
		})
	}
}

func TestDiscoverTaggers_FromDiskOnly(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "joytag", []string{"weights.onnx", "tags.txt"})

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := DiscoverTaggers(cfg)
	if len(got) != 2 {
		t.Fatalf("got %d taggers, want 2: %+v", len(got), got)
	}
	for _, ts := range got {
		if !ts.Available {
			t.Errorf("tagger %q expected Available=true, reason=%q", ts.Name, ts.Reason)
		}
		if !ts.Enabled {
			t.Errorf("tagger %q expected default Enabled=true", ts.Name)
		}
	}
}

func TestDiscoverTaggers_TOMLOverlayWins(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})

	cfg := &config.Config{
		Paths: config.PathsConfig{ModelPath: tmp},
		Tagger: config.TaggerConfig{
			Taggers: []config.TaggerInstance{{
				Name:                "wd",
				Enabled:             false,
				ConfidenceThreshold: 0.42,
			}},
		},
	}
	got := DiscoverTaggers(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d taggers, want 1", len(got))
	}
	if got[0].Enabled {
		t.Error("explicit Enabled=false from TOML should override discovery default")
	}
	if got[0].ConfidenceThreshold != 0.42 {
		t.Errorf("threshold = %v, want 0.42", got[0].ConfidenceThreshold)
	}
}

func TestDiscoverTaggers_MissingFilesUnavailable(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx"})
	makeTaggerDir(t, tmp, "joytag", []string{"tags.txt"})

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := DiscoverTaggers(cfg)
	byName := map[string]TaggerStatus{}
	for _, t := range got {
		byName[t.Name] = t
	}
	wd, ok := byName["wd"]
	if !ok {
		t.Fatal("wd not discovered")
	}
	if wd.Available {
		t.Errorf("wd should be unavailable: %+v", wd)
	}
	if !strings.Contains(wd.Reason, "tags") {
		t.Errorf("wd.Reason should mention the missing tags file, got %q", wd.Reason)
	}

	jt, ok := byName["joytag"]
	if !ok {
		t.Fatal("joytag not discovered")
	}
	if jt.Available {
		t.Errorf("joytag should be unavailable: %+v", jt)
	}
	if !strings.Contains(jt.Reason, "model") {
		t.Errorf("joytag.Reason should mention the missing model file, got %q", jt.Reason)
	}
}

func TestDiscoverTaggers_UnresolvedProfileIsUnavailable(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "camie-tagger-v2", []string{"model.onnx", "camie-tagger-v2-metadata.json"})

	got := DiscoverTaggers(&config.Config{Paths: config.PathsConfig{ModelPath: tmp}})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Available {
		t.Errorf("row = %+v, want unavailable", got[0])
	}
	if !strings.HasPrefix(got[0].Reason, "no inference profile: ") || !strings.HasSuffix(got[0].Reason, "; add a tagger.json") {
		t.Errorf("reason = %q, want the profile error and the tagger.json hint", got[0].Reason)
	}

	mustWriteFile(t, filepath.Join(tmp, "camie-tagger-v2", "tagger.json"),
		[]byte(`{"version":1,"profile":{"layout":"nchw","channels":"rgb","normalize":"imagenet","pad":"mean_color_aspect","activation":"logits","label_format":"camie_json","category_scheme":"name_string"}}`), 0o644)
	got = DiscoverTaggers(&config.Config{Paths: config.PathsConfig{ModelPath: tmp}})
	if !got[0].Available {
		t.Errorf("with a tagger.json: row = %+v, want available", got[0])
	}
}

func TestDiscoverTaggers_InvalidFolderNameIsUnavailable(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd-v1.4-moat", []string{"model.onnx", "tags.csv"})

	got := DiscoverTaggers(&config.Config{Paths: config.PathsConfig{ModelPath: tmp}})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Available {
		t.Errorf("row = %+v, want unavailable", got[0])
	}
	if got[0].Reason != "folder name must match [A-Za-z0-9_-]+; rename the folder" {
		t.Errorf("reason = %q", got[0].Reason)
	}
}

func TestDiscoverTaggers_SkipsEmptyFolder(t *testing.T) {
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "empty", nil)

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := DiscoverTaggers(cfg)
	for _, ts := range got {
		if ts.Name == "empty" {
			t.Errorf("empty folder should not appear in the tagger list: %+v", ts)
		}
	}
	if len(got) != 1 || got[0].Name != "wd" {
		t.Errorf("DiscoverTaggers = %+v, want only `wd`", got)
	}
}

func TestEnabledTaggersForGallery_FiltersByGalleries(t *testing.T) {
	stubRuntimeLibrary(t)
	if !buildSupportsInference() {
		t.Skip("noop build: covered elsewhere")
	}
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "ok-everywhere", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "ok-default", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "ok-stock", []string{"model.onnx", "tags.csv"})

	cfg := &config.Config{
		Paths: config.PathsConfig{ModelPath: tmp},
		Tagger: config.TaggerConfig{
			Taggers: []config.TaggerInstance{
				{Name: "ok-everywhere", Enabled: true, ConfidenceThreshold: 0.4},
				{Name: "ok-default", Enabled: true, ConfidenceThreshold: 0.4, Galleries: []string{"default"}},
				{Name: "ok-stock", Enabled: true, ConfidenceThreshold: 0.4, Galleries: []string{"stock"}},
			},
		},
	}

	gotDefault := EnabledTaggersForGallery(cfg, "default")
	if len(gotDefault) != 2 {
		t.Errorf("default: got %d taggers, want 2 (ok-everywhere + ok-default)", len(gotDefault))
	}
	gotStock := EnabledTaggersForGallery(cfg, "stock")
	if len(gotStock) != 2 {
		t.Errorf("stock: got %d taggers, want 2 (ok-everywhere + ok-stock)", len(gotStock))
	}
	gotPhantom := EnabledTaggersForGallery(cfg, "missing")

	if len(gotPhantom) != 1 {
		t.Errorf("phantom gallery: got %d taggers, want only ok-everywhere", len(gotPhantom))
	}

	for i := range cfg.Tagger.Taggers {
		cfg.Tagger.Taggers[i].Galleries = []string{"default"}
	}
	want := `no enabled tagger applies to gallery "stock"`
	if _, err := SelectForGallery(cfg, "stock", ""); err == nil || err.Error() != want {
		t.Errorf("SelectForGallery(stock) error = %v, want %q", err, want)
	}
	if _, _, reason := GalleryTaggers(cfg, "stock"); reason != want {
		t.Errorf("GalleryTaggers(stock) reason = %q, want %q", reason, want)
	}
	if got, err := SelectForGallery(cfg, "default", ""); err != nil || len(got) != 3 {
		t.Errorf("SelectForGallery(default) = %d taggers, %v; want all three", len(got), err)
	}
}

func TestAppliesToGallery(t *testing.T) {
	t1 := config.TaggerInstance{}
	if !t1.AppliesToGallery("anything") {
		t.Errorf("nil Galleries should apply everywhere")
	}
	t2 := config.TaggerInstance{Galleries: []string{"a", "b"}}
	if !t2.AppliesToGallery("a") {
		t.Errorf("matching name should apply")
	}
	if t2.AppliesToGallery("c") {
		t.Errorf("non-matching name should not apply")
	}

	t3 := config.TaggerInstance{Galleries: []string{}}
	if t3.AppliesToGallery("anything") {
		t.Errorf("explicit-empty Galleries should apply to nothing")
	}
}

func TestEnabledTaggers_NoopBuildReturnsNil(t *testing.T) {
	if buildSupportsInference() {
		t.Skip("tagger build: covered elsewhere")
	}

	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "ok", []string{"model.onnx", "tags.csv"})

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	if got := EnabledTaggers(cfg); len(got) != 0 {
		t.Errorf("EnabledTaggers on noop build = %+v, want nil", got)
	}
}

func TestGalleryTaggersPresentSeparatesAbsentFromDisabled(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	empty := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	if _, present, _ := GalleryTaggers(empty, "default"); present {
		t.Errorf("present = true with nothing installed or configured")
	}

	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})
	cfg := &config.Config{
		Paths:  config.PathsConfig{ModelPath: tmp},
		Tagger: config.TaggerConfig{Taggers: []config.TaggerInstance{{Name: "wd", Enabled: false}}},
	}
	if _, got, _ := GalleryTaggers(cfg, "default"); got != buildSupportsInference() {
		t.Errorf("present = %v with a tagger installed but disabled, want %v", got, buildSupportsInference())
	}
	if len(EnabledTaggers(cfg)) != 0 {
		t.Errorf("EnabledTaggers = %+v, want none while the tagger is disabled", EnabledTaggers(cfg))
	}
}

func TestValidateTaggerName(t *testing.T) {
	for _, name := range []string{"wd-swinv2", "joytag", "A_1", "x"} {
		if err := ValidateTaggerName(name); err != nil {
			t.Errorf("ValidateTaggerName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"", "..", ".", "a/b", "../etc", "a\\b", "a b", "a.b", "foo$bar",
		"wd\nswinv2", "wd-swinv2/../..", "/abs", "café",
	} {
		if err := ValidateTaggerName(name); err == nil {
			t.Errorf("ValidateTaggerName(%q) = nil, want a refusal", name)
		}
	}
}
