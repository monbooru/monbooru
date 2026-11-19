package tagger

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func MergedDispatchRules(dataPath, taggerName string) []DispatchEntry {
	merged := map[string]DispatchEntry{}
	for _, e := range allDispatchRules(dataPath, taggerName) {
		merged[e.Source] = e
	}
	out := slices.Collect(maps.Values(merged))
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func MarshalDispatchDoc(rules []DispatchEntry) ([]byte, error) {
	return json.MarshalIndent(dispatchDoc{Version: dispatchSchemaVersion, Rules: rules}, "", "  ")
}

// Atomic writes alone would still lose one of two edits made from the
// same snapshot.
var overlayMu sync.Mutex

func UpdateDispatchOverlay(dataPath, taggerName string, mutate func(map[string]DispatchEntry) error) error {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	current, err := ReadDispatchOverlay(dataPath, taggerName)
	if err != nil {
		return err
	}
	overlay := map[string]DispatchEntry{}
	for _, e := range current {
		overlay[e.Source] = e
	}
	if err := mutate(overlay); err != nil {
		return err
	}
	return SaveDispatchOverlay(dataPath, taggerName, slices.Collect(maps.Values(overlay)))
}

// The overlay stays a pure delta against stock, and an empty one is
// deleted: the file existing is what "differs from stock" means.
func SaveDispatchOverlay(dataPath, taggerName string, rules []DispatchEntry) error {
	embedded := map[string]DispatchEntry{}
	for _, e := range DefaultDispatchRules(taggerName) {
		embedded[e.Source] = e
	}
	kept := make([]DispatchEntry, 0, len(rules))
	for _, r := range rules {
		if def, ok := embedded[r.Source]; ok && def == r {
			continue
		}
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Source < kept[j].Source })

	path := OverlayPath(dataPath, taggerName)
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(dispatchDoc{Version: dispatchSchemaVersion, Rules: kept}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsx.WriteFile(path, data)
}

// dispatchMoved, in the data folder, records that the model folders'
// mappings were copied there, so a mapping reset since is not copied back.
const dispatchMoved = ".dispatch-moved"

// MoveDispatchOverlays copies each model folder's dispatch.json to the
// data folder, once; the old file is left where it is. A copy that fails
// is tried again at the next start.
func MoveDispatchOverlays(modelPath, dataPath string) {
	marker := filepath.Join(dataPath, dispatchMoved)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	dirs, _ := os.ReadDir(modelPath)
	for _, d := range dirs {
		name := d.Name()
		if ValidateTaggerName(name) != nil {
			continue
		}
		src := filepath.Join(modelPath, name, "dispatch.json")
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		dst := OverlayPath(dataPath, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := fsx.WriteFile(dst, data); err != nil {
			logx.Warnf("tagger: copy %s to %s: %v", src, dst, err)
			return
		}
		logx.Infof("tagger: mappings of %s copied to %s", name, dst)
	}
	if err := fsx.WriteFile(marker, nil); err != nil {
		logx.Warnf("tagger: %s: %v", marker, err)
	}
}
