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

func DispatchTargetCategories(modelPath, taggerName string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range allDispatchRules(modelPath, taggerName) {
		if e.Category == "" || seen[e.Category] {
			continue
		}
		seen[e.Category] = true
		out = append(out, e.Category)
	}
	return out
}

// Overlay rules come last, so a map keyed on Source keeps them.
func allDispatchRules(modelPath, taggerName string) []DispatchEntry {
	return slices.Concat(DefaultDispatchRules(modelPath, taggerName), OverlayDispatchRules(modelPath, taggerName))
}

// DefaultDispatchRules are the rules a tagger ships with: monbooru's own for
// a tagger it ships, else the model folder's dispatch_default.json, which a
// registry entry installs. Either is stock; dispatch.json is the overlay.
func DefaultDispatchRules(modelPath, taggerName string) []DispatchEntry {
	data, source, err := stockFile(modelPath, taggerName, stockDispatch)
	if err != nil {
		return nil
	}
	rules, err := decodeDispatchDoc(data)
	if err != nil {
		logx.Warnf("tagger: %s dispatch: %v", source, err)
		return nil
	}
	return rules
}

func OverlayDispatchRules(modelPath, taggerName string) []DispatchEntry {
	rules, err := ReadDispatchOverlay(modelPath, taggerName)
	if err != nil {
		logx.Warnf("tagger: %s: %v", filepath.Join(modelPath, taggerName, "dispatch.json"), err)
		return nil
	}
	return rules
}

// ReadDispatchOverlay is the strict read for edits: a missing file is no
// rules, any other failure is an error.
func ReadDispatchOverlay(modelPath, taggerName string) ([]DispatchEntry, error) {
	data, err := os.ReadFile(filepath.Join(modelPath, taggerName, "dispatch.json"))
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
