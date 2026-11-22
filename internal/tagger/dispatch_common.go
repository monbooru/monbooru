package tagger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/worker"
)

const dispatchSchemaVersion = 1

type dispatchDoc struct {
	Version int             `json:"version"`
	Rules   []DispatchEntry `json:"rules"`
}

// DispatchEntry routes Source into Category, renamed to Name when set; an
// empty Category drops the label.
type DispatchEntry struct {
	Source   string `json:"source"`
	Category string `json:"category"`
	Name     string `json:"name,omitempty"`
}

func DispatchTargetCategories(dataPath, taggerName string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range allDispatchRules(dataPath, taggerName) {
		if e.Category == "" || seen[e.Category] {
			continue
		}
		seen[e.Category] = true
		out = append(out, e.Category)
	}
	return out
}

// Overlay rules come last, so a map keyed on Source keeps them.
func allDispatchRules(dataPath, taggerName string) []DispatchEntry {
	return slices.Concat(DefaultDispatchRules(taggerName), OverlayDispatchRules(dataPath, taggerName))
}

// DefaultDispatchRules are a tagger's dispatch_default.json, as its worker
// sends them.
func DefaultDispatchRules(taggerName string) []DispatchEntry {
	h, ok := currentPool().heldModel(taggerName)
	if !ok {
		return nil
	}
	return stockRules(h.labels)
}

func stockRules(l worker.Labels) []DispatchEntry {
	out := make([]DispatchEntry, len(l.Mappings))
	for i, m := range l.Mappings {
		out[i] = DispatchEntry{Source: m.Source, Category: m.Category, Name: m.Name}
	}
	return out
}

// OverlayPath is where a tagger's mappings live: in the data folder, since
// they are about this library's tags, not about the model.
func OverlayPath(dataPath, taggerName string) string {
	return filepath.Join(dataPath, taggerName+".dispatch.json")
}

func OverlayDispatchRules(dataPath, taggerName string) []DispatchEntry {
	rules, err := ReadDispatchOverlay(dataPath, taggerName)
	if err != nil {
		logx.Warnf("tagger: %s: %v", OverlayPath(dataPath, taggerName), err)
		return nil
	}
	return rules
}

// ReadDispatchOverlay is the strict read for edits: a missing file is no
// rules, any other failure is an error.
func ReadDispatchOverlay(dataPath, taggerName string) ([]DispatchEntry, error) {
	data, err := os.ReadFile(OverlayPath(dataPath, taggerName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeDispatchDoc(data)
}

func decodeDispatchDoc(data []byte) ([]DispatchEntry, error) {
	var doc dispatchDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if doc.Version != dispatchSchemaVersion {
		return nil, fmt.Errorf("schema version %d unsupported (want %d)", doc.Version, dispatchSchemaVersion)
	}
	return doc.Rules, nil
}
