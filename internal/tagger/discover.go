package tagger

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
)

const (
	DefaultModelFile    = "model.onnx"
	DefaultTagsFile     = "tags.csv"
	DefaultTextTagsFile = "tags.txt"
)

// The name becomes a TOML key and a folder under the model path, which the
// settings delete hands to os.RemoveAll.
func ValidateTaggerName(name string) error { return config.ValidateFolderName("tagger", name) }

type TaggerStatus struct {
	config.TaggerInstance
	Available bool
	Reason    string
	// Malformed rows have a folder name the routes refuse or no inference
	// profile; only a change on disk makes them usable.
	Malformed bool
	// Set whichever Reason the row shows.
	Missing bool
}

// DiscoverTaggers keeps a row for a configured tagger whose folder is gone.
func DiscoverTaggers(cfg *config.Config) []TaggerStatus {
	byName := map[string]config.TaggerInstance{}
	order := []string{}

	catalogDefaults := catalogDefaultsByName(cfg.Paths.ModelPath)

	if entries, err := os.ReadDir(cfg.Paths.ModelPath); err == nil {
		for _, e := range entries {
			name := e.Name()
			// Stat, not the entry's type: a symlinked model directory
			// reports as a link.
			fi, err := os.Stat(filepath.Join(cfg.Paths.ModelPath, name))
			if err != nil || !fi.IsDir() {
				continue
			}
			if onnx, labels := listTaggerFiles(filepath.Join(cfg.Paths.ModelPath, name)); len(onnx)+len(labels) == 0 {
				continue
			}
			byName[name] = SeedTaggerInstance(name, true, catalogDefaults[name])
			order = append(order, name)
		}
	}

	for _, t := range cfg.Tagger.Taggers {
		if _, seen := byName[t.Name]; !seen {
			order = append(order, t.Name)
		}
		byName[t.Name] = t
	}

	noLibrary := ""
	if buildSupportsInference() {
		noLibrary = missingRuntimeLibrary()
	}

	out := make([]TaggerStatus, 0, len(order))
	for _, name := range order {
		t := byName[name]
		dir := filepath.Join(cfg.Paths.ModelPath, name)
		t.ModelFile, t.TagsFile = resolveTaggerFiles(dir, t.ModelFile, t.TagsFile)

		status := TaggerStatus{TaggerInstance: t, Available: true}
		onnxPath := filepath.Join(dir, t.ModelFile)
		tagsPath := filepath.Join(dir, t.TagsFile)
		missing := ""
		switch {
		case statMissing(onnxPath):
			missing = "missing " + t.ModelFile
		case statMissing(tagsPath):
			missing = "missing " + t.TagsFile
		}
		status.Missing = missing != ""
		switch {
		case ValidateTaggerName(name) != nil:
			status.Available, status.Malformed = false, true
			status.Reason = "folder name must match [A-Za-z0-9_-]+; rename the folder"
		case noLibrary != "":
			status.Available = false
			status.Reason = noLibrary
		case missing != "":
			status.Available = false
			status.Reason = missing
		default:
			if _, err := ResolveProfile(cfg.Paths.ModelPath, name, t.TagsFile); err != nil {
				status.Available, status.Malformed = false, true
				status.Reason = "no inference profile: " + err.Error() + "; add a tagger.json"
			}
		}
		out = append(out, status)
	}
	return out
}

func statMissing(path string) bool {
	_, err := os.Stat(path)
	return err != nil
}

// EnabledTaggers ignores gallery scoping; a job on one gallery uses
// EnabledTaggersForGallery. It passes no predicate: AppliesToGallery("")
// is false for a gallery-scoped tagger.
func EnabledTaggers(cfg *config.Config) []TaggerStatus {
	if !buildSupportsInference() {
		return nil
	}
	return enabledIn(DiscoverTaggers(cfg), nil)
}

func SelectForGallery(cfg *config.Config, gallery, name string) ([]TaggerStatus, error) {
	enabled := EnabledTaggersForGallery(cfg, gallery)
	if len(enabled) == 0 {
		return nil, fmt.Errorf("no enabled tagger applies to gallery %q", gallery)
	}
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available for gallery %q", name, gallery)
}

func EnabledTaggersForGallery(cfg *config.Config, gallery string) []TaggerStatus {
	enabled, _, _ := GalleryTaggers(cfg, gallery)
	return enabled
}

// present separates "no tagger set up", which hides the auto-tag
// controls, from "set up but unusable", which only disables them;
// reason is UnavailableReason's.
func GalleryTaggers(cfg *config.Config, gallery string) (enabled []TaggerStatus, present bool, reason string) {
	if !buildSupportsInference() {
		return nil, false, reasonFrom(nil)
	}
	all := DiscoverTaggers(cfg)
	enabled = enabledIn(all, func(t TaggerStatus) bool { return t.AppliesToGallery(gallery) })
	reason = reasonFrom(all)
	if len(enabled) == 0 && reason == "" {
		reason = fmt.Sprintf("no enabled tagger applies to gallery %q", gallery)
	}
	return enabled, len(all) > 0, reason
}

func enabledIn(list []TaggerStatus, extra func(TaggerStatus) bool) []TaggerStatus {
	var out []TaggerStatus
	for _, t := range list {
		if !t.Enabled || !t.Available {
			continue
		}
		if extra != nil && !extra(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// resolveTaggerFiles falls back to the default names so a missing file
// still gets named in the reason.
func resolveTaggerFiles(dir, explicitModel, explicitTags string) (string, string) {
	modelFile := explicitModel
	tagsFile := explicitTags

	onnxFiles, labelFiles := listTaggerFiles(dir)

	if modelFile == "" {
		switch {
		case slices.Contains(onnxFiles, DefaultModelFile):
			modelFile = DefaultModelFile
		case len(onnxFiles) == 1:
			modelFile = onnxFiles[0]
		default:
			modelFile = DefaultModelFile
		}
	}

	if tagsFile == "" {
		switch {
		case slices.Contains(labelFiles, DefaultTagsFile):
			tagsFile = DefaultTagsFile
		case slices.Contains(labelFiles, DefaultTextTagsFile):
			tagsFile = DefaultTextTagsFile
		case len(labelFiles) == 1:
			tagsFile = labelFiles[0]
		default:
			tagsFile = DefaultTagsFile
		}
	}

	return modelFile, tagsFile
}

func isTaggerSidecar(name string) bool {
	switch name {
	case "tagger.json", "dispatch.json", modelManifest, stockProfile, stockDispatch:
		return true
	}
	return false
}

func listTaggerFiles(dir string) (onnx, labels []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch strings.ToLower(filepath.Ext(n)) {
		case ".onnx":
			onnx = append(onnx, n)
		case ".csv", ".txt":
			labels = append(labels, n)
		case ".json":
			if !isTaggerSidecar(n) {
				labels = append(labels, n)
			}
		}
	}
	return onnx, labels
}

func SeedTaggerInstance(name string, enabled bool, catalog *CatalogEntry) config.TaggerInstance {
	t := config.TaggerInstance{
		Name:                name,
		Enabled:             enabled,
		ConfidenceThreshold: config.DefaultConfidenceThreshold,
	}
	if catalog == nil {
		return t
	}
	if catalog.DefaultThreshold > 0 {
		t.ConfidenceThreshold = catalog.DefaultThreshold
	}
	if len(catalog.DefaultThresholds) > 0 {
		t.CategoryThresholds = maps.Clone(catalog.DefaultThresholds)
	}
	if len(catalog.DefaultTopK) > 0 {
		t.PerCategoryTopK = maps.Clone(catalog.DefaultTopK)
	}
	return t
}

func catalogDefaultsByName(modelPath string) map[string]*CatalogEntry {
	cat := LoadCatalog(modelPath)
	out := make(map[string]*CatalogEntry, len(cat))
	for i := range cat {
		out[cat[i].Name] = &cat[i]
	}
	return out
}
