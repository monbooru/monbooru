package tagger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCatalog_DefaultEmbedded(t *testing.T) {
	tmp := t.TempDir()
	cat := LoadCatalog(tmp)
	if len(cat) < 2 {
		t.Fatalf("expected at least 2 default catalog entries, got %d", len(cat))
	}
	names := map[string]bool{}
	for _, e := range cat {
		names[e.Name] = true
	}
	for _, want := range []string{"wd-swinv2", "animetimm-eva02", "joytag", "pixai-v1"} {
		if !names[want] {
			t.Errorf("default catalog missing %q", want)
		}
	}
	if names["camie-v2"] {
		t.Error("default catalog still lists camie-v2")
	}
}

func TestLoadCatalog_IgnoresModelsJSON(t *testing.T) {
	tmp := t.TempDir()
	override := `{
		"version": 1,
		"models": [
			{"name": "wd-swinv2", "description": "custom",
			 "files": [{"url": "https://example.invalid/m.onnx", "filename": "model.onnx"}]},
			{"name": "extra", "description": "added",
			 "files": [{"url": "https://example.invalid/e.onnx", "filename": "model.onnx"}]}
		]
	}`
	mustWriteFile(t, filepath.Join(tmp, "models.json"), []byte(override), 0o644)
	cat := LoadCatalog(tmp)
	byName := map[string]CatalogEntry{}
	for _, e := range cat {
		byName[e.Name] = e
	}
	if e := byName["wd-swinv2"]; e.Description == "custom" {
		t.Errorf("models.json replaced wd-swinv2: %+v", e)
	}
	if _, ok := byName["extra"]; ok {
		t.Errorf("models.json added an entry")
	}
	if len(cat) != 4 {
		t.Errorf("catalog = %d entries, want the four built-in ones", len(cat))
	}
}

func TestCatalogEntry_HostCommand_QuotesPathsAndURLs(t *testing.T) {
	e := CatalogEntry{
		Name: "wd-swinv2",
		Files: []CatalogFile{
			{URL: "https://huggingface.co/foo/m.onnx", Filename: "model.onnx"},
			{URL: "https://huggingface.co/foo/tags.csv", Filename: "tags.csv"},
		},
	}
	cmd := e.HostCommand()
	if !strings.Contains(cmd, "mkdir -p 'wd-swinv2'") {
		t.Errorf("missing single-quoted mkdir target: %q", cmd)
	}
	if !strings.Contains(cmd, "'https://huggingface.co/foo/m.onnx'") {
		t.Errorf("URL not single-quoted: %q", cmd)
	}
	if !strings.Contains(cmd, "'wd-swinv2/model.onnx'") {
		t.Errorf("destination path not single-quoted: %q", cmd)
	}
	if !strings.Contains(cmd, " && \\\n") {
		t.Errorf("expected && line continuation between curl invocations: %q", cmd)
	}
}

func TestCatalogEntry_GatedCommands_CarryHFToken(t *testing.T) {
	e := CatalogEntry{
		Name:  "animetimm-eva02",
		Gated: true,
		Files: []CatalogFile{
			{URL: "https://huggingface.co/foo/m.onnx", Filename: "model.onnx"},
		},
	}
	host := e.HostCommand()
	if !strings.Contains(host, `curl -L -H "Authorization: Bearer $HF_TOKEN" -o `) {
		t.Errorf("gated host command missing auth header: %q", host)
	}
}

func TestCatalogEntry_UngatedCommands_NoAuthHeader(t *testing.T) {
	e := CatalogEntry{
		Name:  "wd-swinv2",
		Files: []CatalogFile{{URL: "https://example.com/m.onnx", Filename: "model.onnx"}},
	}
	if cmd := e.HostCommand(); strings.Contains(cmd, "HF_TOKEN") {
		t.Errorf("ungated host command should not mention HF_TOKEN: %q", cmd)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustResolveProfile(t *testing.T, modelPath, taggerName, tagsFile string) Profile {
	t.Helper()
	p, err := ResolveProfile(modelPath, taggerName, tagsFile)
	if err != nil {
		t.Fatalf("ResolveProfile(%q): %v", taggerName, err)
	}
	return p
}

func installRegistryTagger(t *testing.T, modelPath, name string) {
	t.Helper()
	src := filepath.Join("..", "registry", "plugins", "taggers", name)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read registry folder %s: %v", name, err)
	}
	dst := filepath.Join(modelPath, name)
	mustMkdirAll(t, dst, 0o755)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		mustWriteFile(t, filepath.Join(dst, e.Name()), data, 0o644)
	}
}

func TestLoadCatalog_FolderManifest(t *testing.T) {
	tmp := t.TempDir()
	installRegistryTagger(t, tmp, "camie-v2")
	mustMkdirAll(t, filepath.Join(tmp, "wd-swinv2"), 0o755)
	mustWriteFile(t, filepath.Join(tmp, "wd-swinv2", "model.json"), []byte(`{"version":1,"description":"stale copy"}`), 0o644)
	mustMkdirAll(t, filepath.Join(tmp, "bad name"), 0o755)
	mustWriteFile(t, filepath.Join(tmp, "bad name", "model.json"), []byte(`{"version":1,"description":"x"}`), 0o644)
	mustMkdirAll(t, filepath.Join(tmp, "future"), 0o755)
	mustWriteFile(t, filepath.Join(tmp, "future", "model.json"), []byte(`{"version":2,"description":"x"}`), 0o644)

	byName := map[string]CatalogEntry{}
	for _, e := range LoadCatalog(tmp) {
		byName[e.Name] = e
	}
	camie, ok := byName["camie-v2"]
	if !ok || camie.DefaultThreshold != 0.50 || camie.DefaultThresholds["character"] != 0.60 || len(camie.Files) == 0 {
		t.Errorf("camie-v2 from its folder = %+v, ok=%v", camie, ok)
	}
	if byName["wd-swinv2"].Description == "stale copy" {
		t.Error("a model.json in a built-in tagger's folder replaced the embedded entry")
	}
	for _, name := range []string{"bad name", "future"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%q should not be listed", name)
		}
	}
}

func TestCatalogEntry_Missing(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "model.onnx"), []byte("x"), 0o644)
	mustWriteFile(t, filepath.Join(tmp, "selected_tags.csv"), []byte("x"), 0o644)
	mustMkdirAll(t, filepath.Join(tmp, "extra.json"), 0o755)
	e := CatalogEntry{Name: "m", Files: []CatalogFile{
		{URL: "https://example.invalid/r/resolve/abc/model.onnx", Filename: "model.onnx"},
		{URL: "https://example.invalid/r/resolve/abc/selected_tags.csv?download=true", Filename: "tags.csv"},
		{URL: "https://example.invalid/r/extra.json", Filename: "extra.json"},
		{URL: "https://example.invalid/r/dispatch_default.json", Filename: "dispatch_default.json"},
	}}
	var got []string
	for _, f := range e.Missing(tmp).Files {
		got = append(got, f.Filename)
	}
	if want := []string{"extra.json", "dispatch_default.json"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Missing = %v, want %v", got, want)
	}
	if len(e.Files) != 4 {
		t.Errorf("Missing changed the entry it was called on: %v", e.Files)
	}
}

func TestStockFiles_BuiltinIgnoresTheFolder(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd-swinv2")
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "profile_default.json"), []byte(`{"version":1,"profile":{"layout":"nchw"}}`), 0o644)
	mustWriteFile(t, filepath.Join(dir, "dispatch_default.json"), []byte(`{"version":1,"rules":[]}`), 0o644)
	if p := mustResolveProfile(t, tmp, "wd-swinv2", "tags.csv"); p.Layout != "nhwc" {
		t.Errorf("layout = %q, want the embedded nhwc", p.Layout)
	}
	if len(DefaultDispatchRules(tmp, "wd-swinv2")) == 0 {
		t.Error("an empty dispatch_default.json in the folder hid the embedded rules")
	}
}

func TestResolveTaggerFiles_RegistryFilesAreNotLabels(t *testing.T) {
	tmp := t.TempDir()
	installRegistryTagger(t, tmp, "camie-v2")
	dir := filepath.Join(tmp, "camie-v2")
	mustWriteFile(t, filepath.Join(dir, "camie-tagger-v2.onnx"), []byte("x"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "camie-tagger-v2-metadata.json"), []byte("{}"), 0o644)
	model, tags := resolveTaggerFiles(dir, "", "")
	if model != "camie-tagger-v2.onnx" || tags != "camie-tagger-v2-metadata.json" {
		t.Errorf("resolved %q, %q", model, tags)
	}
}
