package tagger

import (
	"fmt"
	"sync"

	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tags"
)

func sanitizeLabel(raw string, idx int) (string, bool) {
	name := tags.NormalizeName(raw)
	if rs := []rune(name); len(rs) > 200 {
		name = string(rs[:200])
	}
	if name == "" || !tags.HasTagContent(name) {
		return fmt.Sprintf("_unsupported_%d", idx), false
	}
	return name, true
}

// tagLabel slices are indexed by the model's output channel: an unusable
// label becomes a placeholder, which inference must skip, rather than
// being dropped.
type tagLabel struct {
	name         string
	categoryID   int
	categoryName string
	placeholder  bool
}

// The labels a worker sends are kept converted per etag: the tag page and
// the labels panel read a tagger's every label on each view.
var converted = struct {
	sync.Mutex
	byETag map[string][]tagLabel
}{byETag: map[string][]tagLabel{}}

func toTagLabels(l worker.Labels) []tagLabel {
	if l.ETag != "" {
		converted.Lock()
		cached, ok := converted.byETag[l.ETag]
		converted.Unlock()
		if ok {
			return cached
		}
	}
	out := make([]tagLabel, len(l.Labels))
	for i, raw := range l.Labels {
		name, ok := sanitizeLabel(raw.Name, i)
		out[i] = tagLabel{name: name, categoryID: raw.CategoryID, categoryName: raw.Category, placeholder: !ok}
	}
	if l.ETag != "" {
		converted.Lock()
		converted.byETag[l.ETag] = out
		converted.Unlock()
	}
	return out
}
