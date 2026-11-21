package algorithms

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
	"github.com/monbooru/monbooru/internal/ranking/rankingtest"
)

// An AlgorithmID is a uint8, so all 256 values can be checked: New must
// succeed exactly for the IDs that AlgorithmID.Valid accepts. This catches a
// new algorithm added to one but not the other, in either direction.
func TestNewMatchesValid(t *testing.T) {
	for n := range 256 {
		id := ranking.AlgorithmID(n)
		alg, err := New(id)
		if (err == nil) != id.Valid() {
			t.Errorf("New(%d) error = %v, but AlgorithmID(%d).Valid() = %v", id, err, id, id.Valid())
			continue
		}
		if err == nil && alg.ID() != ranking.Resolve(id) {
			t.Errorf("New(%d).ID() = %d, want %d", id, alg.ID(), ranking.Resolve(id))
		}
	}
}

func TestNewNamesEachAlgorithm(t *testing.T) {
	cases := []struct {
		id   ranking.AlgorithmID
		name string
	}{
		{ranking.AlgorithmBradleyTerry, "bradley-terry"},
		{ranking.AlgorithmGlicko2, "glicko-2"},
	}
	for _, c := range cases {
		alg, err := New(c.id)
		if err != nil || alg.Name() != c.name {
			t.Errorf("New(%d) = %v, %v; want %q", c.id, alg, err, c.name)
		}
	}
	// Default follows the recommendation.
	def, err := New(ranking.AlgorithmDefault)
	if err != nil || def.ID() != ranking.Resolve(ranking.AlgorithmDefault) {
		t.Errorf("New(AlgorithmDefault) = %v, %v; want the recommended algorithm", def, err)
	}
}

// Picking pairs by information (ranking.PairInformation, with
// ranking.PickBest) should teach more than random pairs, as Glickman & Jensen
// (2005) found for chess tournaments. Checked for every algorithm, so a new
// one is covered without copying this test.
//
// 500 images, 2000 pairs shown over a year (about 4 votes per image), the
// same simulated person either way. Every image is scored, an unvoted one at
// what it shows (its start). Over 20 seeds the average correlation with the
// truth went from 0.759 (random) to 0.793 (picked) for Bradley–Terry, and
// from 0.752 to 0.779 for Glicko-2, with picking ahead in 20 and 17 of the 20
// seeds. The uncertainty stays honest: at least 92% within 2 standard
// deviations with picked pairs.
func TestPickedPairsBeatRandomPairs(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, 500)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	const shown = 2000
	year := 365 * 24 * time.Hour
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	for n := 1; n < 256; n++ {
		id := ranking.AlgorithmID(n)
		if !id.Valid() {
			continue
		}
		alg, err := New(id)
		if err != nil {
			t.Fatal(err)
		}
		// The votes use their own seed: the same seed as the truth would give
		// the same random numbers, quietly linking the pairs to the truth.
		picked, err := rankingtest.SimulatePicked(rand.New(rand.NewPCG(754, 502)), truth, shown, rankingtest.TiesAndSkips, alg, 100, start, year/shown)
		if err != nil {
			t.Fatalf("%s: %v", alg.Name(), err)
		}
		random := rankingtest.SpreadOverTime(rankingtest.SimulateVotes(rand.New(rand.NewPCG(754, 502)), truth, shown, rankingtest.TiesAndSkips), start, year)

		score := func(name string, votes []ranking.Vote) float64 {
			fit, err := alg.Fit(context.Background(), votes)
			if err != nil {
				t.Fatalf("%s, %s: Fit: %v", alg.Name(), name, err)
			}
			all := make([]float64, len(truth)) // unvoted images stay at their start, 0
			withinTwo := 0
			for id, s := range fit {
				e := alg.Shown(s)
				all[id-1] = e.Strength
				if math.Abs(e.Strength-truth[id-1]) <= 2*e.Uncertainty {
					withinTwo++
				}
			}
			if share := float64(withinTwo) / float64(len(fit)); share < 0.9 {
				t.Errorf("%s, %s: %.1f%% of images within 2 standard deviations of the truth, want about 95%%", alg.Name(), name, 100*share)
			}
			return rankingtest.Correlation(truth, all)
		}
		r, p := score("random pairs", random), score("picked pairs", picked)
		if p <= r {
			t.Errorf("%s: correlation with the truth: picked pairs %v, random pairs %v; want picked to do better", alg.Name(), p, r)
		}
	}
}
