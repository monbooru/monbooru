//go:build tagger

package tagger

import (
	"fmt"
	"path/filepath"
)

// plannedTagger is what the parent knows of a tagger for a run: the frame
// it takes and the tag each output channel becomes.
type plannedTagger struct {
	input inputKey
	cands []CandidateLabel
}

func planTagger(modelPath string, t TaggerStatus, catIDs map[string]int64, generalCatID int64, inferredCats map[string]int64) (plannedTagger, error) {
	profile, err := ResolveProfile(modelPath, t.Name, t.TagsFile)
	if err != nil {
		return plannedTagger{}, fmt.Errorf("resolve profile for %q: %w", t.Name, err)
	}
	size := profile.InputSize
	if size == 0 {
		dims, err := onnxInputDims(filepath.Join(modelPath, t.Name, t.ModelFile))
		if err != nil {
			return plannedTagger{}, fmt.Errorf("inspect onnx model for %q: %w", t.Name, err)
		}
		if size = inferInputSize(dims, profile.Layout); size <= 0 {
			return plannedTagger{}, fmt.Errorf("onnx model for %q: cannot infer input size from dimensions %v", t.Name, dims)
		}
	}
	cands, err := taggerCandidates(modelPath, t, profile, catIDs, generalCatID, inferredCats)
	if err != nil {
		return plannedTagger{}, err
	}
	return plannedTagger{input: inputKey{size, padFill(profile)}, cands: cands}, nil
}

// taggerCandidates gives a tagger's output channels the tag each one
// becomes in this gallery: its category, its mapping, and for a
// single_general model the category the gallery already files the name in.
func taggerCandidates(modelPath string, t TaggerStatus, profile Profile, catIDs map[string]int64, generalCatID int64, inferredCats map[string]int64) ([]CandidateLabel, error) {
	labels, err := loadLabels(filepath.Join(modelPath, t.Name, t.TagsFile), profile)
	if err != nil {
		return nil, fmt.Errorf("load labels for %q: %w", t.Name, err)
	}
	dispatch := LoadDispatch(modelPath, t.Name, catIDs)
	catNames := make(map[int64]string, len(catIDs))
	for name, cid := range catIDs {
		catNames[cid] = name
	}
	cands := make([]CandidateLabel, len(labels))
	for idx, label := range labels {
		if label.placeholder {
			cands[idx].Placeholder = true
			continue
		}
		res := resolveCategory(profile, label, catIDs, dispatch)
		if res.skip {
			cands[idx].Placeholder = true
			continue
		}
		catID := res.catID
		name := label.name
		if rule, ok := dispatch.Lookup(label.name); ok && rule.Name != "" {
			name = rule.Name
		}
		if !res.override && profile.CategoryScheme == "single_general" && catID == generalCatID {
			if inferred, ok := inferredCats[label.name]; ok {
				catID = inferred
			}
		}
		cands[idx] = CandidateLabel{Name: name, CatID: catID, CatName: catNames[catID]}
	}
	return cands, nil
}

// mergeTaggers aggregates each tagger's frames and keeps a tag's best
// score; on a tie the earlier tagger keeps it. scores and plan follow
// taggers' order.
func mergeTaggers(taggers []TaggerStatus, plan []plannedTagger, scores [][]SparseScores, minHits int) map[TagKey]Scored {
	merged := map[TagKey]Scored{}
	for tIdx, t := range taggers {
		if tIdx >= len(scores) {
			break
		}
		for _, c := range AggregateInferenceScores(scores[tIdx], plan[tIdx].cands, AggregateOpts{
			MinHits:            minHits,
			GlobalThreshold:    float32(t.ConfidenceThreshold),
			CategoryThresholds: t.CategoryThresholds,
			PerCategoryTopK:    t.PerCategoryTopK,
			DisabledCategories: t.DisabledCategories,
		}) {
			k := TagKey{Name: c.Name, CatID: c.CatID}
			if prev, ok := merged[k]; !ok || c.Score > prev.Score {
				merged[k] = Scored{Score: c.Score, TaggerName: t.Name}
			}
		}
	}
	return merged
}
