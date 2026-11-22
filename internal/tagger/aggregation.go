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

func CategoryThreshold(global float64, perCat map[string]float64, cat string) float64 {
	if v, ok := perCat[cat]; ok {
		return v
	}
	return global
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

type TagKey struct {
	Name  string
	CatID int64
}

type Scored struct {
	Score      float32
	TaggerName string
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

// A score under scoreFloor counts as no hit, so a frame travels as the
// labels at or above it.
const scoreFloor = 0.001

// SparseScores is one frame's scores at or above scoreFloor: I holds the
// label indexes in ascending order, S their scores.
type SparseScores struct {
	I []int32
	S []float32
}

func sparseScores(dense []float32) SparseScores {
	var out SparseScores
	for i, s := range dense {
		if s >= scoreFloor {
			out.I = append(out.I, int32(i))
			out.S = append(out.S, s)
		}
	}
	return out
}

// AggregateInferenceScores returns the survivors in no particular order.
func AggregateInferenceScores(perFrame []SparseScores, labels []CandidateLabel, opts AggregateOpts) []AggregatedCandidate {
	type accum struct {
		sum  float32
		hits int
	}
	agg := map[int]accum{}
	for _, frame := range perFrame {
		for j, idx := range frame.I {
			if j >= len(frame.S) || idx < 0 {
				break
			}
			// Written so a NaN fails it: NaN compares false both ways.
			if s := frame.S[j]; s >= scoreFloor {
				e := agg[int(idx)]
				e.sum += s
				e.hits++
				agg[int(idx)] = e
			}
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
		if mean < float32(CategoryThreshold(float64(opts.GlobalThreshold), opts.CategoryThresholds, lbl.CatName)) {
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
