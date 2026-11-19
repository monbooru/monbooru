package tagger

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"

	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tags"
)

// Profile is the part of a model's profile monbooru reads: how its labels'
// categories become this library's.
type Profile struct {
	CategoryScheme string
	CategoryMap    map[string]string
}

func profileOf(l worker.Labels) Profile {
	return Profile{CategoryScheme: l.CategoryScheme, CategoryMap: l.CategoryMap}
}

const profileSchemaVersion = 1

// ProfileExport is the model's resolved profile as a tagger.json
// document, and the fields its folder's own tagger.json sets; ok is false
// for a model no worker describes.
func ProfileExport(name string) (doc []byte, sidecar []string, ok bool) {
	h, found := currentPool().heldModel(name)
	if !found || len(h.model.Profile) == 0 {
		return nil, nil, false
	}
	doc, err := json.MarshalIndent(struct {
		Version int             `json:"version"`
		Profile json.RawMessage `json:"profile"`
	}{profileSchemaVersion, h.model.Profile}, "", "  ")
	if err != nil {
		return nil, nil, false
	}
	return doc, h.model.Sidecar, true
}

// EmittedCategories is informational: a mapping can still route a label
// into any category.
func EmittedCategories(name string) []string {
	h, ok := currentPool().heldModel(name)
	if !ok {
		return nil
	}
	profile := profileOf(h.labels)
	set := map[string]bool{}
	mapped := func(c string) string { return cmp.Or(profile.CategoryMap[c], c) }
	switch profile.CategoryScheme {
	case "single_general":
		return []string{"general"}
	case "wd14_numeric":
		for _, c := range wd14Category {
			set[mapped(c)] = true
		}
		set["rating"] = true
	}
	for _, l := range toTagLabels(h.labels) {
		if l.placeholder {
			continue
		}
		if tags.IsCanonicalRating(l.name) {
			set["rating"] = true
		} else if l.categoryName != "" {
			set[mapped(l.categoryName)] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}
