package tagger

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/worker"
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
	// Differs is a tagger two workers hold with different files.
	Differs bool
	// OnDemand is a tagger only on-demand workers can run now: a job that
	// names no worker leaves it out.
	OnDemand bool
}

// DiscoverTaggers lists every tagger a worker holds, then every configured
// one no worker holds.
func DiscoverTaggers(cfg *config.Config) []TaggerStatus {
	p := CurrentPool()
	p.mu.Lock()
	order, held := p.held()
	p.mu.Unlock()

	byName := map[string]config.TaggerInstance{}
	for _, name := range order {
		byName[name] = SeedTaggerInstance(name, true, held[name].catalog)
	}
	for _, t := range cfg.Tagger.Taggers {
		if _, seen := byName[t.Name]; !seen {
			order = append(order, t.Name)
		}
		byName[t.Name] = t
	}

	out := make([]TaggerStatus, 0, len(order))
	for _, name := range order {
		t := byName[name]
		status := TaggerStatus{TaggerInstance: t, Available: true}
		h := held[name]
		if h != nil {
			status.ModelFile, status.TagsFile = h.model.ModelFile, h.model.LabelFile
			status.Differs = h.differs
			status.OnDemand = h.usable && !h.standing
		}
		switch {
		case ValidateTaggerName(name) != nil:
			status.Available, status.Malformed = false, true
			status.Reason = "folder name must match [A-Za-z0-9_-]+; rename the folder"
		case h == nil:
			status.Available, status.Missing = false, true
			status.Reason = "no worker holds it"
		case !h.usable && h.model.Reason != "" && h.model.Reason != reasonReading:
			status.Available, status.Malformed = false, true
			status.Reason = h.model.Reason
		case !h.usable:
			status.Available = false
			status.Reason = cmp.Or(readingReason(h), "the workers that hold it are off")
		}
		out = append(out, status)
	}
	return out
}

// The worker's reason while it reads a model's weights for its digest.
const reasonReading = "reading the weights"

func readingReason(h *heldModel) string {
	if h.model.Reason == reasonReading {
		return reasonReading
	}
	return ""
}

// EnabledTaggers ignores gallery scoping; a job on one gallery uses
// EnabledTaggersForGallery. It passes no predicate: AppliesToGallery("")
// is false for a gallery-scoped tagger.
func EnabledTaggers(cfg *config.Config) []TaggerStatus {
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

// WithoutOnDemand is taggers less those a job that names no worker cannot
// reach.
func WithoutOnDemand(taggers []TaggerStatus) []TaggerStatus {
	return slices.DeleteFunc(slices.Clone(taggers), func(t TaggerStatus) bool { return t.OnDemand })
}

func EnabledTaggersForGallery(cfg *config.Config, gallery string) []TaggerStatus {
	enabled, _, _ := GalleryTaggers(cfg, gallery)
	return enabled
}

// present separates "no tagger set up", which hides the auto-tag
// controls, from "set up but unusable", which only disables them;
// reason is UnavailableReason's.
func GalleryTaggers(cfg *config.Config, gallery string) (enabled []TaggerStatus, present bool, reason string) {
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

func SeedTaggerInstance(name string, enabled bool, catalog *worker.CatalogEntry) config.TaggerInstance {
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
