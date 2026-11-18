package glicko2

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
	"github.com/monbooru/monbooru/internal/ranking/rankingtest"
)

const week = 7 * 24 * time.Hour

// shownFit runs Fit and returns what each image shows.
func shownFit(t testing.TB, alg *algorithm, votes []ranking.Vote) map[int64]ranking.Estimate {
	t.Helper()
	got, err := alg.Fit(context.Background(), votes)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	out := make(map[int64]ranking.Estimate, len(got))
	for id, s := range got {
		out[id] = alg.Shown(s)
	}
	return out
}

// checkRecovers runs 40 images with true strengths from −2 to 2 through 4000
// pairs shown over 20 weeks: about 10 games per image per week, the paper's
// "moderate to large" number per period. It checks the correlation with the
// truth, the slope (fitted / true), and the share within 2 uncertainties.
func checkRecovers(t *testing.T, answers rankingtest.Answerer, slopeLow, slopeHigh float64) {
	t.Helper()
	alg := newTest(t)
	truth := rankingtest.SpacedTruth(40)
	votes := rankingtest.SimulateVotes(rand.New(rand.NewPCG(502, 754)), truth, 4000, answers)
	shown := shownFit(t, alg, rankingtest.SpreadOverTime(votes, monday, 20*week))

	fitted := make([]float64, len(truth))
	withinTwo := 0
	for i, want := range truth {
		e := shown[int64(i+1)]
		fitted[i] = e.Strength
		if math.Abs(e.Strength-want) <= 2*e.Uncertainty {
			withinTwo++
		}
	}
	if c := rankingtest.Correlation(truth, fitted); c < 0.98 {
		t.Errorf("correlation with the truth = %v, want at least 0.98", c)
	}
	if s := rankingtest.Slope(truth, fitted); s < slopeLow || s > slopeHigh {
		t.Errorf("fitted / true strength = %v, want %v to %v", s, slopeLow, slopeHigh)
	}
	if share := float64(withinTwo) / float64(len(truth)); share < 0.85 {
		t.Errorf("%.0f%% of images within 2 uncertainties of the truth, want about 95%%", 100*share)
	}
}

// With votes that always pick a side, Glicko-2 should find the true
// strengths. Across six seeds (this one included) the correlation was 0.985
// to 0.992, the slope 0.99 to 1.05, and 38 to 40 of 40 images were within 2
// uncertainties.
func TestFitRecoversSimulatedStrengths(t *testing.T) {
	checkRecovers(t, rankingtest.SidesOnly, 0.9, 1.1)
}

// With ties and skips, the order still comes out right, but the spread is a
// little wider than the truth: across six seeds the slope was 1.04 to 1.14
// (1.14 at this seed), with 38 to 40 of 40 images within 2 uncertainties.
// As for Bradley–Terry, a tie as half a win isn't quite Rao & Kupper's
// model, which the simulated person follows.
func TestFitWithTiesAndSkips(t *testing.T) {
	checkRecovers(t, rankingtest.TiesAndSkips, 0.95, 1.25)
}

// The sparse case from the plan's assumptions: most images get only one to
// three votes, spread over a year. The numbers must stay finite, no image can
// be more unsure than a brand-new one plus a year of empty periods, and the
// uncertainty must be honest.
func TestFitSparseVotes(t *testing.T) {
	alg := newTest(t)
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, 2000)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	votes := rankingtest.SimulateVotes(rng, truth, 2000, rankingtest.TiesAndSkips) // about 1800 votes
	shown := shownFit(t, alg, rankingtest.SpreadOverTime(votes, monday, 52*week))

	start := alg.Start().(state)
	limit := math.Sqrt(start.Uncertainty*start.Uncertainty + 52*start.Volatility*start.Volatility)
	var trueOnes, fitted []float64
	withinTwo := 0
	for id, e := range shown {
		if math.IsNaN(e.Strength) || math.IsInf(e.Strength, 0) || !(e.Uncertainty > 0) || e.Uncertainty > limit {
			t.Fatalf("image %d shows %+v, want a finite strength and 0 < uncertainty ≤ %v", id, e, limit)
		}
		want := truth[id-1]
		trueOnes, fitted = append(trueOnes, want), append(fitted, e.Strength)
		if math.Abs(e.Strength-want) <= 2*e.Uncertainty {
			withinTwo++
		}
	}
	// Few votes can only say so much. Across six seeds, this correlation was
	// 0.50 to 0.54; 0.3 still shows the votes are being used.
	if c := rankingtest.Correlation(trueOnes, fitted); c < 0.3 {
		t.Errorf("correlation with the truth = %v, want at least 0.3", c)
	}
	// Honest uncertainty: it was over 99.5% across seeds, so on the cautious side.
	if share := float64(withinTwo) / float64(len(shown)); share < 0.95 {
		t.Errorf("%.1f%% of images within 2 uncertainties of the truth, want at least 95%%", 100*share)
	}
}

// The very sparse case: half the images get one vote each, spread over the
// year, the other half none. Each pair is then TestNewBeatsNew's case: the
// winner at +0.9343 (1662.31) and the loser at −0.9343, or both at 0 after a
// tie. The uncertainty is the one-game result (φ' ≈ 1.6712, RD 290), grown by
// the empty weeks between the pair's vote and the year's last vote: never more
// than √(φ'² + 52σ²). Across six seeds the correlation with the truth was 0.38
// to 0.42, and over 99.8% of images were within 2 uncertainties.
func TestFitOneVoteEach(t *testing.T) {
	alg := newTest(t)
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, 2000)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	votes := rankingtest.SpreadOverTime(rankingtest.SimulateOneVoteEach(rng, truth, rankingtest.TiesAndSkips), monday, 52*week)
	shown := shownFit(t, alg, votes)
	if len(shown) != 1000 {
		t.Errorf("%d images ranked, want the 1000 that got a vote", len(shown))
	}

	const winner = 0.9343366183338649
	const oneGame = 1.6712102732775158 // φ' after a tie; a win or loss is 1.2e-8 more
	start := alg.Start().(state)
	maxUncertainty := math.Sqrt(1.671210285389904*1.671210285389904 + 52*start.Volatility*start.Volatility)
	for _, v := range votes {
		want := map[ranking.Outcome][2]float64{leftWins: {winner, -winner}, rightWins: {-winner, winner}, tie: {0, 0}}[v.Outcome]
		for k, id := range [2]int64{v.Left, v.Right} {
			got := shown[id]
			if !near(got.Strength, want[k], 1e-9) {
				t.Errorf("vote %+v: image %d strength = %v, want %v", v, id, got.Strength, want[k])
			}
			if got.Uncertainty < oneGame-1e-9 || got.Uncertainty > maxUncertainty {
				t.Errorf("vote %+v: image %d uncertainty = %v, want %v to %v", v, id, got.Uncertainty, oneGame, maxUncertainty)
			}
		}
	}

	var trueOnes, fitted []float64
	withinTwo := 0
	for id, e := range shown {
		trueOnes, fitted = append(trueOnes, truth[id-1]), append(fitted, e.Strength)
		if math.Abs(e.Strength-truth[id-1]) <= 2*e.Uncertainty {
			withinTwo++
		}
	}
	if c := rankingtest.Correlation(trueOnes, fitted); c < 0.25 {
		t.Errorf("correlation with the truth = %v, want at least 0.25", c)
	}
	if share := float64(withinTwo) / float64(len(shown)); share < 0.95 {
		t.Errorf("%.1f%% of images within 2 uncertainties of the truth, want at least 95%%", 100*share)
	}
}

// BenchmarkFit times Glicko-2's replay in a 1M-image library, at the same
// counts of pairs shown as Bradley–Terry's BenchmarkFit, with the votes spread
// evenly over a year (52 weekly periods). It reports the same accuracy
// figures (see rankingtest.Accuracy), so the two can be compared directly.
// Run with:
//
//	go test -run '^$' -bench BenchmarkFit -benchmem ./internal/ranking/glicko2/
func BenchmarkFit(b *testing.B) {
	const library = 1_000_000
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, library)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	pools := []struct {
		name   string
		images int
	}{
		{"whole_library", library},
		{"one_5k_search", 5_000},
	}
	for _, shown := range []int{100, 1_000, 10_000, 100_000} {
		for _, pool := range pools {
			b.Run(fmt.Sprintf("%d_pairs_shown/%s", shown, pool.name), func(b *testing.B) {
				poolTruth := truth[:pool.images]
				sortedTruth := slices.Sorted(slices.Values(poolTruth))
				vs := rankingtest.SimulateVotes(rand.New(rand.NewPCG(754, 502)), poolTruth, shown, rankingtest.TiesAndSkips)
				vs = rankingtest.SpreadOverTime(vs, monday, 52*week)
				alg := newTest(b)
				var fit map[int64]ranking.State
				for b.Loop() {
					var err error
					if fit, err = alg.Fit(context.Background(), vs); err != nil {
						b.Fatal(err)
					}
				}
				estimates := make(map[int64]ranking.Estimate, len(fit))
				for id, s := range fit {
					estimates[id] = alg.Shown(s)
				}
				avgUncertainty, impliedRange, withinRange := rankingtest.Accuracy(estimates, poolTruth, sortedTruth)
				b.ReportMetric(float64(len(fit)), "ranked_images")
				b.ReportMetric(avgUncertainty, "avg_uncertainty")
				b.ReportMetric(ranking.RDFromUncertainty(avgUncertainty), "avg_RD")
				b.ReportMetric(100*impliedRange, "implied_range_%")
				b.ReportMetric(100*withinRange, "truth_in_range_%")
			})
		}
	}
}
