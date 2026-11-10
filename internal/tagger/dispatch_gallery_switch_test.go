//go:build tagger

package tagger

import (
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

var switchCatsA = map[string]int64{
	"general": 1, "character": 2, "artist": 3, "copyright": 4,
	"meta": 5, "rating": 6, "year": 7, "person": 8, "medium": 9,
}

var switchCatsB = map[string]int64{
	"general": 1, "character": 2, "artist": 3, "copyright": 4,
	"meta": 5, "rating": 6, "year": 7, "person": 9, "medium": 10,
}

func dispatchTargets(modelPath string, cats map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for source, rule := range LoadDispatch(modelPath, "wd-swinv2", cats).rules {
		if rule.Drop {
			continue
		}
		name := rule.Name
		if name == "" {
			name = source
		}
		out[name] = rule.CatID
	}
	return out
}

func TestRunDispatchFollowsGallerySwitch(t *testing.T) {
	requireORT(t)
	modelPath := modelsDir(t)
	if _, err := os.Stat(filepath.Join(modelPath, "wd-swinv2", "model.onnx")); err != nil {
		t.Skipf("wd-swinv2 model not present: %v", err)
	}

	uncapped := map[string]int{}
	for name := range switchCatsA {
		uncapped[name] = 0
	}
	taggers := []TaggerStatus{{TaggerInstance: config.TaggerInstance{
		Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv",
		ConfidenceThreshold: 0, PerCategoryTopK: uncapped,
	}}}

	frame := filepath.Join(t.TempDir(), "frame.png")
	mustWriteFile(t, frame, solidPNG(t, 64, 64, color.RGBA{90, 120, 160, 255}), 0o644)

	b := &inprocBackend{}
	t.Cleanup(b.ReleaseAll)

	run := func(cats map[string]int64) map[TagKey]Scored {
		return runOne(t, b, modelPath, taggers, cats, []string{frame})
	}

	audit := func(label string, tags map[TagKey]Scored, cats map[string]int64) {
		t.Helper()
		want := dispatchTargets(modelPath, cats)
		names := make(map[int64]string, len(cats))
		for n, id := range cats {
			names[id] = n
		}
		checked := 0
		for k := range tags {
			exp, ok := want[k.Name]
			if !ok {
				continue
			}
			checked++
			if k.CatID != exp {
				t.Errorf("%s: %q filed under %q, want %q", label, k.Name,
					names[k.CatID], names[exp])
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no routed label cleared the pre-floor; the audit is vacuous", label)
		}
		t.Logf("%s: %d routed labels checked", label, checked)
	}

	audit("cold cache", run(switchCatsA), switchCatsA)
	audit("warm cache after switch", run(switchCatsB), switchCatsB)
}
