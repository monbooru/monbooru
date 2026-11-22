package tagger

import (
	"fmt"

	"github.com/monbooru/monbooru/internal/shared/worker"
)

type plannedTagger struct {
	input inputKey
	cands []CandidateLabel
}

func planTagger(dataPath string, t TaggerStatus, catIDs map[string]int64, generalCatID int64, inferredCats map[string]int64) (plannedTagger, error) {
	h, ok := currentPool().heldModel(t.Name)
	if !ok {
		return plannedTagger{}, fmt.Errorf("no worker holds tagger %q", t.Name)
	}
	if h.model.Input.Size < 1 || len(h.labels.Labels) == 0 {
		return plannedTagger{}, fmt.Errorf("tagger %q: its worker has not described it", t.Name)
	}
	cands := taggerCandidates(h.labels, LoadDispatch(stockRules(h.labels), dataPath, t.Name, catIDs), catIDs, generalCatID, inferredCats)
	return plannedTagger{input: inputKey{h.model.Input.Size, h.model.Input.Fill}, cands: cands}, nil
}

func taggerCandidates(l worker.Labels, dispatch *DispatchTable, catIDs map[string]int64, generalCatID int64, inferredCats map[string]int64) []CandidateLabel {
	profile := profileOf(l)
	labels := toTagLabels(l)
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
	return cands
}

// On a tie the earlier tagger keeps the tag.
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
