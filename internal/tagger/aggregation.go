package tagger

import (
	"cmp"
	"math"
	"slices"
)

// Tuned for the WD14, JoyTag and Camie distributions: attribution
// categories sit low so a noisy run can't pile them onto one image, and
// rating and year take a single tag each.
var DefaultPerCategoryTopK = map[string]int{
	"character": 8,
	"copyright": 4,
	"artist":    4,
	"general":   25,
	"rating":    1,
	"medium":    4,
	"person":    8,
	"species":   8,
	"year":      1,
}

const DefaultTopKFallback = 10

// ResolveTopK returns 0 (uncapped) when the override is an explicit 0.
func ResolveTopK(overrides map[string]int, cat string) int {
	if v, ok := overrides[cat]; ok {
		return v
	}
	if v, ok := DefaultPerCategoryTopK[cat]; ok {
		return v
	}
	return DefaultTopKFallback
}

// The [2, 10] clamp: one flicker across a long archive is not enough, and
// a sparse but real label on a long manga still gets through.
func ResolveMinHits(fraction float64, frameCount int) int {
	if frameCount <= 1 {
		return 1
	}
	if fraction <= 0 {
		return 1
	}
	return min(max(int(math.Ceil(fraction*float64(frameCount))), 2), 10)
}

type CandidateLabel struct {
	Name        string
	CatID       int64
	CatName     string
	Placeholder bool
}

type AggregatedCandidate struct {
	Name  string
	CatID int64
	Score float32
}

type AggregateOpts struct {
	MinHits            int
	GlobalThreshold    float32
	CategoryThresholds map[string]float64
	PerCategoryTopK    map[string]int
	DisabledCategories []string
}

// AggregateInferenceScores returns the survivors in no particular order.
func AggregateInferenceScores(perFrame [][]float32, labels []CandidateLabel, opts AggregateOpts) []AggregatedCandidate {
	type accum struct {
		sum  float32
		hits int
	}
	agg := map[int]accum{}
	for _, scores := range perFrame {
		for idx, s := range scores {
			// Written so a NaN fails it: NaN compares false both ways.
			if !(s >= 0.001) {
				continue
			}
			e := agg[idx]
			e.sum += s
			e.hits++
			agg[idx] = e
		}
	}

	byCat := map[int64][]AggregatedCandidate{}
	catNames := map[int64]string{}
	for idx, e := range agg {
		if idx >= len(labels) {
			continue
		}
		lbl := labels[idx]
		if lbl.Placeholder {
			continue
		}
		if slices.Contains(opts.DisabledCategories, lbl.CatName) {
			continue
		}
		if e.hits < opts.MinHits {
			continue
		}
		mean := e.sum / float32(e.hits)
		threshold := opts.GlobalThreshold
		if v, ok := opts.CategoryThresholds[lbl.CatName]; ok {
			threshold = float32(v)
		}
		if mean < threshold {
			continue
		}
		byCat[lbl.CatID] = append(byCat[lbl.CatID],
			AggregatedCandidate{Name: lbl.Name, CatID: lbl.CatID, Score: mean})
		catNames[lbl.CatID] = lbl.CatName
	}

	var out []AggregatedCandidate
	for catID, list := range byCat {
		// Name breaks ties so two equal runs emit the same set.
		k := ResolveTopK(opts.PerCategoryTopK, catNames[catID])
		slices.SortFunc(list, func(a, b AggregatedCandidate) int {
			return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Name, b.Name))
		})
		if k > 0 && len(list) > k {
			list = list[:k]
		}
		out = append(out, list...)
	}
	return out
}
