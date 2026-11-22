package rank

import (
	"math"
	"math/rand/v2"
	"testing"
)

// Gauss–Hermite quadrature with n points is exact for x^k × e^(−x²) when
// k ≤ 2n − 1, so with 30 points every even power up to 58 must come out as
// its known integral, Γ((k+1)/2). (Odd powers are 0 by symmetry.)
func TestGaussHermitePoints(t *testing.T) {
	nodes, weights := hermiteNodes, hermiteWeights
	if len(nodes) != 30 || len(weights) != 30 {
		t.Fatalf("%d nodes and %d weights, want 30 of each", len(nodes), len(weights))
	}
	for i := range nodes {
		if i > 0 && nodes[i] <= nodes[i-1] {
			t.Errorf("nodes not increasing at %d: %v, %v", i, nodes[i-1], nodes[i])
		}
		if j := len(nodes) - 1 - i; !near(nodes[i], -nodes[j], 1e-12) || !near(weights[i], weights[j], 1e-15) {
			t.Errorf("nodes %d and %d aren't mirror images: %v, %v (weights %v, %v)", i, j, nodes[i], nodes[j], weights[i], weights[j])
		}
		if !(weights[i] > 0) {
			t.Errorf("weight %d = %v, want positive", i, weights[i])
		}
	}
	for k := 0; k <= 58; k += 2 {
		got := 0.0
		for i := range nodes {
			got += weights[i] * math.Pow(nodes[i], float64(k))
		}
		want := math.Gamma(float64(k+1) / 2)
		if math.Abs(got-want) > 1e-12*want {
			t.Errorf("∫ x^%d e^(−x²) = %v, want %v", k, got, want)
		}
	}
}

// Checked against a separate Python script that integrates Glickman &
// Jensen's eq. (4) by brute force (a 200,001-point trapezoid rule, no
// Gauss–Hermite), so the two don't share any code or method.
//
// The paper's 30-point rule is itself an approximation: for the widest pairs
// (a brand-new image) it's off by up to about 0.00005 nats (0.015%), far
// below anything that could change which pair is picked. So the 30-point
// answer is checked to 1e-4, and the same formula with a 100-point rule to
// 1e-8, which shows the formula is right and only the rule's precision
// differs.
func TestPairInformationMatchesBruteForce(t *testing.T) {
	newImage := UncertaintyFromRD(350)
	cases := []struct {
		name string
		a, b Estimate
		want float64
	}{
		{"new vs new", Estimate{0, newImage}, Estimate{0, newImage}, 0.31965400289704093},
		{"new vs RD 174", Estimate{0, newImage}, Estimate{0, UncertaintyFromRD(174)}, 0.2601248364958681},
		{"new vs settled (RD 87)", Estimate{0, newImage}, Estimate{0, UncertaintyFromRD(87)}, 0.24022888990226715},
		{"settled vs settled", Estimate{0, UncertaintyFromRD(87)}, Estimate{0, UncertaintyFromRD(87)}, 0.053344460804908345},
		{"gap 1, both uncertainty 1", Estimate{1, 1}, Estimate{0, 1}, 0.13873786905243485},
		{"gap 3, both uncertainty 1", Estimate{0, 1}, Estimate{3, 1}, 0.06280725091151562},
		{"gap 0, both uncertainty 0.1", Estimate{0.5, 0.1}, Estimate{0.5, 0.1}, 0.002481455291206802},
		{"gap 2, uncertainties 2 and 0.5", Estimate{2, 2}, Estimate{0, 0.5}, 0.18416971466907267},
	}
	nodes100, weights100 := gaussHermite(100)
	for _, c := range cases {
		if got := PairInformation(c.a, c.b); !near(got, c.want, 1e-4) {
			t.Errorf("%s: PairInformation = %v, want %v", c.name, got, c.want)
		}
		if got := pairInformation(c.a, c.b, nodes100, weights100); !near(got, c.want, 1e-8) {
			t.Errorf("%s: with 100 points = %v, want %v", c.name, got, c.want)
		}
	}
}

// sampleEstimates covers new, settled, strong, weak and lopsided images.
var sampleEstimates = []Estimate{
	{0, UncertaintyFromRD(350)},
	{0, 0.3},
	{1.5, 1},
	{-2, 0.5},
	{4, 0.1},
	{-0.5, 3},
	{0.2, 0},
}

func TestPairInformationProperties(t *testing.T) {
	for _, a := range sampleEstimates {
		for _, b := range sampleEstimates {
			got := PairInformation(a, b)
			// A vote has two outcomes, so it can teach at most ln 2 nats.
			if got < 0 || got > math.Ln2 || math.IsNaN(got) {
				t.Errorf("PairInformation(%+v, %+v) = %v, want 0 to ln 2", a, b, got)
			}
			// It doesn't matter which side each image is on.
			if other := PairInformation(b, a); !near(got, other, 1e-12) {
				t.Errorf("PairInformation is %v one way round and %v the other, for %+v and %+v", got, other, a, b)
			}
			// Only the gap matters, not where the pair sits.
			shifted := PairInformation(Estimate{a.Strength + 3, a.Uncertainty}, Estimate{b.Strength + 3, b.Uncertainty})
			if !near(got, shifted, 1e-12) {
				t.Errorf("moving both of %+v and %+v by 3 changed the information from %v to %v", a, b, got, shifted)
			}
		}
	}
}

// Two perfectly known images: the vote can't teach anything.
func TestPairInformationOfCertainImagesIsZero(t *testing.T) {
	for _, gap := range []float64{0, 1, 5} {
		if got := PairInformation(Estimate{gap, 0}, Estimate{0, 0}); got != 0 {
			t.Errorf("gap %v, both certain: PairInformation = %v, want 0", gap, got)
		}
	}
}

// Glickman & Jensen's Fig. 1: for the same uncertainty, the information
// falls as the gap grows; for the same gap, it rises with the uncertainty.
func TestPairInformationTrends(t *testing.T) {
	for _, u := range []float64{0.3, 1, 2, 4} {
		previous := math.Inf(1)
		for _, gap := range []float64{0, 0.5, 1, 2, 4, 8} {
			got := PairInformation(Estimate{gap, u}, Estimate{0, u})
			if got >= previous {
				t.Errorf("uncertainty %v: gap %v gives %v, not less than a smaller gap (%v)", u, gap, got, previous)
			}
			previous = got
		}
	}
	for _, gap := range []float64{0, 1, 3} {
		previous := -1.0
		for _, u := range []float64{0.1, 0.3, 1, 2, 4} {
			got := PairInformation(Estimate{gap, u}, Estimate{0, u})
			if got <= previous {
				t.Errorf("gap %v: uncertainty %v gives %v, not more than a smaller uncertainty (%v)", gap, u, got, previous)
			}
			previous = got
		}
	}
}

// Extreme pairs stay finite and in range.
func TestPairInformationExtremes(t *testing.T) {
	cases := []struct{ a, b Estimate }{
		{Estimate{50, 1}, Estimate{0, 1}},     // a huge gap
		{Estimate{0, 100}, Estimate{0, 100}},  // hugely unsure
		{Estimate{0, 1e-9}, Estimate{0, 0}},   // almost certain
		{Estimate{500, 0.5}, Estimate{0, 50}}, // both at once
	}
	for _, c := range cases {
		if got := PairInformation(c.a, c.b); math.IsNaN(got) || got < 0 || got > math.Ln2 {
			t.Errorf("PairInformation(%+v, %+v) = %v, want 0 to ln 2", c.a, c.b, got)
		}
	}
}

// The single best is always picked.
func TestPickBestPicksTheBest(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	scores := []float64{0.1, 0.5, 0.3, 0.9, 0.2, 0.7}
	for range 1000 {
		if got := PickBest(scores, rng); got != 3 {
			t.Fatalf("PickBest = %d, want 3 (score 0.9)", got)
		}
	}
}

// Pairs tied for the best are picked at random, about equally often, and
// nothing below them ever is.
func TestPickBestBreaksTiesAtRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	scores := []float64{0.5, 0.9, 0.2, 0.9, 0.9, 0.85}
	counts := map[int]int{}
	const draws = 30_000
	for range draws {
		counts[PickBest(scores, rng)]++
	}
	if len(counts) != 3 {
		t.Errorf("picked %v, want only indexes 1, 3 and 4", counts)
	}
	for _, i := range []int{1, 3, 4} {
		// Standard deviation is 0.00272, so the odds of being off 0.02 through
		// through chance alone is 1 in 5 trillion. If this test failed for you
		// by pure random chance, buy a lottery tricket.
		if share := float64(counts[i]) / draws; !near(share, 1.0/3, 0.02) {
			t.Errorf("index %d picked %.3f of the time, want about 1/3", i, share)
		}
	}
}

// Before anything is ranked, every pair of new images scores exactly the
// same, so the first pick can be any of them.
func TestPickBestAllTied(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	newImage := Estimate{Strength: 0, Uncertainty: UncertaintyFromRD(350)}
	scores := make([]float64, 50)
	for i := range scores {
		scores[i] = PairInformation(newImage, newImage)
	}
	seen := map[int]bool{}
	for range 5000 {
		seen[PickBest(scores, rng)] = true
	}
	if len(seen) != len(scores) {
		t.Errorf("picked %d different pairs out of 50 equally good ones, want all 50", len(seen))
	}
}

// Ties allow for rounding: two scores a hair apart count as tied, but a real
// difference, however small to the eye, doesn't.
func TestPickBestTieTolerance(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	rounding := []float64{0.3, 0.3 + 1e-15}
	seen := map[int]bool{}
	for range 1000 {
		seen[PickBest(rounding, rng)] = true
	}
	if len(seen) != 2 {
		t.Errorf("scores 1e-15 apart: picked %v, want both (a tie)", seen)
	}
	real := []float64{0.3, 0.3 + 1e-6}
	for range 1000 {
		if got := PickBest(real, rng); got != 1 {
			t.Fatalf("scores 1e-6 apart: picked %d, want the higher one, 1", got)
		}
	}
}

func TestPickBestNothingToPick(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	if got := PickBest(nil, rng); got != -1 {
		t.Errorf("PickBest(no scores) = %d, want -1", got)
	}
	if got := PickBest([]float64{math.NaN(), math.NaN()}, rng); got != -1 {
		t.Errorf("PickBest(only NaN) = %d, want -1", got)
	}
	// A NaN among real scores is never picked.
	for range 1000 {
		if got := PickBest([]float64{math.NaN(), 0.2, math.NaN(), 0.1}, rng); got != 1 {
			t.Fatalf("PickBest with NaNs = %d, want 1", got)
		}
	}
}

// BenchmarkPairInformation: the plan expects microseconds per pair, so a few
// hundred candidates score in well under a millisecond.
func BenchmarkPairInformation(b *testing.B) {
	a, c := Estimate{0.3, 1.2}, Estimate{-0.4, 0.8}
	for b.Loop() {
		_ = PairInformation(a, c)
	}
}
