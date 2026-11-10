package registry_test

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/registry"
	"github.com/monbooru/monbooru/internal/tagger"
)

func registryTaggers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(registry.TaggersDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestBuiltinsAreEmbedded(t *testing.T) {
	for _, name := range registry.Taggers {
		for _, file := range []string{"model.json", "profile_default.json", "dispatch_default.json"} {
			if _, err := fs.ReadFile(registry.FS, path.Join(registry.TaggersDir, name, file)); err != nil {
				t.Errorf("%s/%s: %v", name, file, err)
			}
		}
	}
	for _, file := range []string{"theme.css", "logo.png", "favicon.png", "mascot-hello.png", "mascot-lost.png", "mascot-sleep.png", "mascot-panic.png", "mascot-sparkle-head.png"} {
		if _, err := fs.ReadFile(registry.FS, path.Join(registry.LightThemeDir, file)); err != nil {
			t.Errorf("light theme %s: %v", file, err)
		}
	}
	for _, dir := range []string{registry.LegacyDarkThemeDir, registry.LegacyLightThemeDir} {
		for _, file := range []string{"theme.css", "logo.png", "favicon.png"} {
			if _, err := fs.ReadFile(registry.FS, path.Join(dir, file)); err != nil {
				t.Errorf("%s %s: %v", dir, file, err)
			}
		}
	}
	entries, err := fs.ReadDir(registry.FS, registry.TaggersDir)
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, e := range entries {
		embedded = append(embedded, e.Name())
	}
	if want := slices.Sorted(slices.Values(registry.Taggers)); !slices.Equal(embedded, want) {
		t.Errorf("embedded taggers %v, registry.Taggers %v", embedded, want)
	}
}

func TestEveryRegistryTaggerLoads(t *testing.T) {
	byName := map[string]tagger.CatalogEntry{}
	for _, e := range tagger.LoadCatalog(registry.TaggersDir) {
		byName[e.Name] = e
	}
	for _, name := range registryTaggers(t) {
		e, ok := byName[name]
		if !ok {
			t.Errorf("%s: no catalog entry from its model.json", name)
			continue
		}
		if e.Description == "" || len(e.Files) == 0 {
			t.Errorf("%s: entry lacks a description or files: %+v", name, e)
		}
		label := ""
		for _, f := range e.Files {
			switch strings.ToLower(filepath.Ext(f.Filename)) {
			case ".csv", ".txt":
				label = f.Filename
			case ".json":
				if !strings.HasSuffix(f.Filename, "_default.json") {
					label = f.Filename
				}
			}
		}
		if label == "" {
			t.Errorf("%s: no label file among its files", name)
		} else if _, err := tagger.ResolveProfile(registry.TaggersDir, name, label); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(registry.TaggersDir, name, "dispatch_default.json")); err == nil && len(tagger.DefaultDispatchRules(registry.TaggersDir, name)) == 0 {
			t.Errorf("%s: dispatch_default.json holds no rule monbooru reads", name)
		}
	}
}
