//go:build tagger

package tagger

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestConformance_MatchesTheReferenceScores(t *testing.T) {
	requireORT(t)
	evalDir := os.Getenv("MONBOORU_MODELS_EVAL")
	if evalDir == "" {
		t.Skip("MONBOORU_MODELS_EVAL is not set")
	}
	modelPath := modelsDir(t)
	expected, _ := filepath.Glob(filepath.Join(evalDir, "*", "expected.json"))
	if len(expected) == 0 {
		t.Skipf("no expected.json under %s", evalDir)
	}
	cats := map[string]int64{"general": 1, "character": 2, "artist": 3, "copyright": 4, "meta": 5, "rating": 6, "year": 7, "medium": 8}
	uncapped := map[string]int{}
	for c := range cats {
		uncapped[c] = 0
	}
	ran := 0
	for _, path := range expected {
		name := filepath.Base(filepath.Dir(path))
		ref := readExpected(t, path)
		status := DiscoverTaggers(&config.Config{Paths: config.PathsConfig{ModelPath: modelPath}})
		var tagger *TaggerStatus
		for i := range status {
			if status[i].Name == name && status[i].Available {
				tagger = &status[i]
			}
		}
		if tagger == nil {
			t.Logf("%s: not installed under %s, skipped", name, modelPath)
			continue
		}
		tagger.ConfidenceThreshold = 0.3
		tagger.PerCategoryTopK = uncapped
		dispatch := LoadDispatch(modelPath, name, cats)
		t.Run(name, func(t *testing.T) {
			b := &inprocBackend{}
			defer b.ReleaseAll()
			for image, scores := range ref {
				resp, err := b.Run(context.Background(), RunRequest{
					ModelPath: modelPath, SessionParallel: 1, Taggers: []TaggerStatus{*tagger},
					Provider: "cpu", CatIDs: cats, GeneralCatID: 1, Parallel: 1,
					Images: []BackendImageRequest{{ID: 1, FramePaths: []string{filepath.Join(evalDir, "images", image)}}},
				})
				if err != nil || resp.Results[0].Err != "" {
					t.Fatalf("%s: %v %s", image, err, resp.Results[0].Err)
				}
				got := map[string]float32{}
				for k, s := range resp.Results[0].Tags {
					got[k.Name] = s.Score
				}
				want := map[string]float32{}
				for label, score := range scores {
					tag, _ := sanitizeLabel(label, 0)
					if rule, ok := dispatch.Lookup(tag); ok {
						if rule.Drop {
							continue
						}
						tag = cmp.Or(rule.Name, tag)
					}
					want[tag] = float32(score)
				}
				for tag, score := range want {
					if score < 0.35 {
						continue
					}
					g, ok := got[tag]
					if !ok || math.Abs(float64(g-score)) > 0.05 {
						t.Errorf("%s: %s scored %.3f (emitted %v), reference %.3f", image, tag, g, ok, score)
					}
				}
				for tag, g := range got {
					if g >= 0.5 && want[tag] < 0.4 {
						t.Errorf("%s: %s scored %.3f, reference %.3f", image, tag, g, want[tag])
					}
				}
			}
		})
		ran++
	}
	if ran == 0 {
		t.Skip("no expected tagger is installed")
	}
}

func readExpected(t *testing.T, path string) map[string]map[string]float64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Images map[string]map[string]float64 `json:"images"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc.Images
}
