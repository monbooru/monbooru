package tagger

import (
	"math"
	"slices"
	"sort"
	"testing"
)

func TestResolveTopK_FallsBackToDefault(t *testing.T) {
	cases := []struct {
		cat  string
		want int
	}{
		{"character", 8},
		{"copyright", 4},
		{"artist", 4},
		{"general", 25},
		{"rating", 1},
		{"medium", 4},
		{"person", 8},
		{"species", 8},
		{"year", 1},
		{"homegrown", DefaultTopKFallback},
	}
	for _, c := range cases {
		got := ResolveTopK(nil, c.cat)
		if got != c.want {
			t.Errorf("ResolveTopK(nil, %q) = %d, want %d", c.cat, got, c.want)
		}
	}
}

func TestResolveTopK_OverrideWins(t *testing.T) {
	o := map[string]int{"general": 50, "character": 0}
	if got := ResolveTopK(o, "general"); got != 50 {
		t.Errorf("override general = %d, want 50", got)
	}
	if got := ResolveTopK(o, "character"); got != 0 {
		t.Errorf("override character = %d, want 0 (uncapped)", got)
	}
}

func TestResolveMinHits(t *testing.T) {
	cases := []struct {
		frac   float64
		frames int
		want   int
	}{

		{0.05, 1, 1},
		{0.50, 1, 1},

		{0, 100, 1},
		{-0.1, 100, 1},

		{0.05, 50, 3},

		{0.05, 30, 2},

		{0.05, 4, 2},

		{0.05, 500, 10},

		{0.20, 200, 10},
	}
	for _, c := range cases {
		got := ResolveMinHits(c.frac, c.frames)
		if got != c.want {
			t.Errorf("ResolveMinHits(%v, %d) = %d, want %d", c.frac, c.frames, got, c.want)
		}
	}
}

func labelsGeneral(n int, catID int64) []CandidateLabel {
	out := make([]CandidateLabel, n)
	for i := 0; i < n; i++ {
		out[i] = CandidateLabel{
			Name:    "tag_" + tagSuffix(i),
			CatID:   catID,
			CatName: "general",
		}
	}
	return out
}

func tagSuffix(i int) string {

	const digits = "0123456789"
	if i < 10 {
		return "0" + string(digits[i])
	}
	return string(digits[i/10]) + string(digits[i%10])
}

func TestAggregation_HitCountGatesShortTail(t *testing.T) {

	labels := []CandidateLabel{
		{Name: "A", CatID: 1, CatName: "general"},
	}
	perFrame := make([][]float32, 50)
	for i := range perFrame {
		perFrame[i] = make([]float32, 1)
	}
	perFrame[7][0] = 0.95
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         ResolveMinHits(0.05, 50),
		GlobalThreshold: 0.35,
	})
	if len(cands) != 0 {
		t.Errorf("expected empty, got %+v", cands)
	}
}

func TestAggregation_AvgConfidence(t *testing.T) {
	labels := []CandidateLabel{
		{Name: "B", CatID: 1, CatName: "general"},
	}
	perFrame := [][]float32{
		{0.6},
		{0.8},
		{0.7},
		{0},
		{0},
	}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         ResolveMinHits(0.05, 5),
		GlobalThreshold: 0.35,
	})
	if len(cands) != 1 {
		t.Fatalf("got %d cands, want 1: %+v", len(cands), cands)
	}
	const want = float32(0.7)
	if got := cands[0].Score; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("score = %v, want %v (mean of 0.6, 0.8, 0.7)", got, want)
	}
}

func TestAggregation_PerTaggerTopK(t *testing.T) {

	const total = 30
	labels := labelsGeneral(total, 1)
	perFrame := [][]float32{make([]float32, total)}
	for i := 0; i < total; i++ {

		perFrame[0][i] = float32(0.95) - float32(i)*0.01
	}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         1,
		GlobalThreshold: 0.35,
	})
	if len(cands) != 25 {
		t.Fatalf("got %d cands, want 25", len(cands))
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.Name
	}
	sort.Strings(names)
	for i := 0; i < 25; i++ {
		want := "tag_" + tagSuffix(i)
		if names[i] != want {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want)
		}
	}
}

func TestAggregation_DisabledByZeroFraction(t *testing.T) {
	labels := []CandidateLabel{
		{Name: "C", CatID: 1, CatName: "general"},
	}
	perFrame := make([][]float32, 50)
	for i := range perFrame {
		perFrame[i] = make([]float32, 1)
	}
	perFrame[12][0] = 0.9
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         ResolveMinHits(0, 50),
		GlobalThreshold: 0.35,
	})
	if len(cands) != 1 {
		t.Fatalf("got %d cands, want 1", len(cands))
	}
	if cands[0].Score != 0.9 {
		t.Errorf("score = %v, want 0.9", cands[0].Score)
	}
}

func TestAggregation_SingleImagePath(t *testing.T) {
	labels := []CandidateLabel{
		{Name: "D", CatID: 1, CatName: "general"},
		{Name: "E", CatID: 1, CatName: "general"},
	}
	perFrame := [][]float32{{0.75, 0.20}}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         ResolveMinHits(0.05, 1),
		GlobalThreshold: 0.50,
	})
	if len(cands) != 1 {
		t.Fatalf("got %d cands, want 1 (only D passes the 0.50 threshold)", len(cands))
	}
	if cands[0].Name != "D" || cands[0].Score != 0.75 {
		t.Errorf("got %+v, want D@0.75", cands[0])
	}
}

func TestAggregation_DropsNonFiniteScores(t *testing.T) {
	labels := []CandidateLabel{
		{Name: "a", CatID: 1, CatName: "general"},
		{Name: "b", CatID: 1, CatName: "general"},
	}
	nan := float32(math.NaN())
	opts := AggregateOpts{MinHits: 1, GlobalThreshold: 0.04}
	cands := AggregateInferenceScores([][]float32{{0.1, nan}, {0.1, 0.05}}, labels, opts)
	if len(cands) != 2 || slices.ContainsFunc(cands, func(c AggregatedCandidate) bool { return c.Score != c.Score }) {
		t.Errorf("one NaN frame: got %+v, want a and b at finite scores", cands)
	}
	if cands := AggregateInferenceScores([][]float32{{nan, nan}}, labels, opts); len(cands) != 0 {
		t.Errorf("an all-NaN tensor tagged %+v", cands)
	}
}

func TestAggregation_PerCategoryThreshold(t *testing.T) {
	labels := []CandidateLabel{
		{Name: "miku", CatID: 2, CatName: "character"},
		{Name: "blue_eyes", CatID: 1, CatName: "general"},
	}
	perFrame := [][]float32{{0.70, 0.70}}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:            1,
		GlobalThreshold:    0.35,
		CategoryThresholds: map[string]float64{"character": 0.85},
	})
	if len(cands) != 1 || cands[0].Name != "blue_eyes" {
		t.Errorf("got %+v, want one survivor blue_eyes", cands)
	}
}

func TestAggregation_DisabledCategorySuppressed(t *testing.T) {
	labels := []CandidateLabel{
		{Name: "miku", CatID: 2, CatName: "character"},
		{Name: "blue_eyes", CatID: 1, CatName: "general"},
	}
	perFrame := [][]float32{{0.95, 0.95}}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:            1,
		GlobalThreshold:    0.35,
		DisabledCategories: []string{"general"},
	})
	if len(cands) != 1 || cands[0].Name != "miku" {
		t.Errorf("got %+v, want one survivor miku (general disabled)", cands)
	}
}

func TestAggregation_TopKExplicitZeroIsUncapped(t *testing.T) {
	const total = 40
	labels := labelsGeneral(total, 1)
	perFrame := [][]float32{make([]float32, total)}
	for i := 0; i < total; i++ {
		perFrame[0][i] = 0.5 + float32(i)*0.005
	}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         1,
		GlobalThreshold: 0.35,
		PerCategoryTopK: map[string]int{"general": 0},
	})
	if len(cands) != total {
		t.Errorf("got %d cands, want %d (cap disabled)", len(cands), total)
	}
}

func TestAggregation_PerCategoryCapIsIndependent(t *testing.T) {
	general := labelsGeneral(30, 1)
	chars := labelsGeneral(10, 2)
	for i := range chars {
		chars[i].CatName = "character"
	}
	labels := append(general, chars...)
	perFrame := [][]float32{make([]float32, len(labels))}
	for i := range labels {
		perFrame[0][i] = 0.6
	}
	cands := AggregateInferenceScores(perFrame, labels, AggregateOpts{
		MinHits:         1,
		GlobalThreshold: 0.35,
	})
	var generalCount, charCount int
	for _, c := range cands {
		switch c.CatID {
		case 1:
			generalCount++
		case 2:
			charCount++
		}
	}
	if generalCount != 25 {
		t.Errorf("general count = %d, want 25 (default cap)", generalCount)
	}
	if charCount != 8 {
		t.Errorf("character count = %d, want 8 (default cap)", charCount)
	}
}
