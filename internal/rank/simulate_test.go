package rank

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// With plenty of votes that follow the model exactly, the refit should find
// the true strengths: closely correlated, and each within a few
// standard deviations of the truth. This checks the fit itself; answers with ties
// and skips are in the next test. The random numbers are seeded, so the test
// always sees the same votes.
func TestFitRecoversSimulatedStrengths(t *testing.T) {
	truth := spacedTruth(40)
	votes := simulateVotes(rand.New(rand.NewPCG(502, 754)), truth, 4000, sidesOnly) // about 200 votes per image
	fit := fitStrengths(t, votes, DefaultSettings().StartUncertainty)

	fitted := make([]float64, len(truth))
	withinTwo := 0
	for i, want := range truth {
		got := fit[int64(i+1)]
		fitted[i] = got.Strength
		miss := math.Abs(got.Strength - want)
		if miss > 4*got.Uncertainty {
			t.Errorf("image %d: fitted %v ± %v, truth %v: more than 4 standard deviations off", i+1, got.Strength, got.Uncertainty, want)
		}
		if miss <= 2*got.Uncertainty {
			withinTwo++
		}
	}
	if c := correlation(truth, fitted); c < 0.98 {
		t.Errorf("correlation with the truth = %v, want at least 0.98", c)
	}
	// If the uncertainty is honest, about 95% land within 2 standard deviations.
	if share := float64(withinTwo) / float64(len(truth)); share < 0.85 {
		t.Errorf("%.0f%% of images within 2 standard deviations of the truth, want about 95%%", 100*share)
	}
}

// With ties and skips (likelier between close images),
// the fit should still be close: same order, about the true spread, and
// honest uncertainty. It isn't exact, because counting a tie as half a win
// isn't quite Rao & Kupper's model. Worked out separately in Python for
// these 40 images, with unlimited votes: fitted ≈ 1.06 × truth. Across six
// seeds (this one included) the measured slope was 1.03 to 1.11, and 37 to
// 40 of the 40 images were within 2 standard deviations.
func TestFitWithTiesAndSkips(t *testing.T) {
	truth := spacedTruth(40)
	votes := simulateVotes(rand.New(rand.NewPCG(502, 754)), truth, 4000, tiesAndSkips) // about 180 votes per image
	fit := fitStrengths(t, votes, DefaultSettings().StartUncertainty)

	fitted := make([]float64, len(truth))
	withinTwo := 0
	for i, want := range truth {
		got := fit[int64(i+1)]
		fitted[i] = got.Strength
		if math.Abs(got.Strength-want) <= 2*got.Uncertainty {
			withinTwo++
		}
	}
	if c := correlation(truth, fitted); c < 0.98 {
		t.Errorf("correlation with the truth = %v, want at least 0.98", c)
	}
	if got := slope(truth, fitted); got < 0.95 || got > 1.15 {
		t.Errorf("fitted / true strength = %v, want about 1.06 (0.95 to 1.15)", got)
	}
	if share := float64(withinTwo) / float64(len(truth)); share < 0.85 {
		t.Errorf("%.0f%% of images within 2 standard deviations of the truth, want about 95%%", 100*share)
	}
}

// The quick update sees the same votes one at a time. It should get close
// to the truth and to the refit, though less exactly than the refit.
func TestQuickUpdateTracksSimulatedStrengths(t *testing.T) {
	bt := newTest(t)
	truth := spacedTruth(40)
	votes := simulateVotes(rand.New(rand.NewPCG(502, 754)), truth, 4000, tiesAndSkips)

	estimates := map[int64]Estimate{}
	get := func(id int64) Estimate {
		if e, ok := estimates[id]; ok {
			return e
		}
		return bt.Start()
	}
	for _, v := range votes {
		l, r, err := bt.Update(get(v.Left), get(v.Right), v.Outcome)
		if err != nil {
			t.Fatal(err)
		}
		estimates[v.Left], estimates[v.Right] = l, r
	}

	fit := fitStrengths(t, votes, DefaultSettings().StartUncertainty)
	quick := make([]float64, len(truth))
	refit := make([]float64, len(truth))
	for i := range truth {
		quick[i] = estimates[int64(i+1)].Strength
		refit[i] = fit[int64(i+1)].Strength
	}
	if c := correlation(truth, quick); c < 0.95 {
		t.Errorf("quick update's correlation with the truth = %v, want at least 0.95", c)
	}
	if c := correlation(refit, quick); c < 0.95 {
		t.Errorf("quick update's correlation with the refit = %v, want at least 0.95", c)
	}
}

// The sparse case, normal for a large library: most images get only one to
// three votes. The refit must still finish with finite numbers, and its
// uncertainty must be honest: a vote or two can't say much, and the
// uncertainty has to admit it.
func TestFitSparseVotes(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, 2000)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	votes := simulateVotes(rng, truth, 2000, tiesAndSkips) // 2000 pairs shown: about 1800 votes
	fit := fitStrengths(t, votes, 2)

	var trueOnes, fitted []float64
	withinTwo := 0
	for id, s := range fit {
		if math.IsNaN(s.Strength) || math.IsInf(s.Strength, 0) || !(s.Uncertainty > 0) || s.Uncertainty > 2 {
			t.Fatalf("image %d = %+v, want a finite strength and 0 < uncertainty ≤ 2", id, s)
		}
		if math.Abs(s.Strength) > 3 {
			t.Errorf("image %d strength = %v from a handful of votes, want it held near average", id, s.Strength)
		}
		want := truth[id-1]
		trueOnes, fitted = append(trueOnes, want), append(fitted, s.Strength)
		if math.Abs(s.Strength-want) <= 2*s.Uncertainty {
			withinTwo++
		}
	}
	// Few votes can only say so much. Across five seeds, this correlation
	// was 0.52 to 0.54; 0.3 still shows the votes are being used.
	if c := correlation(trueOnes, fitted); c < 0.3 {
		t.Errorf("correlation with the truth = %v, want at least 0.3", c)
	}
	// Honest uncertainty: at least 95% within 2 standard deviations (it was over
	// 99% across seeds, so the uncertainty is on the cautious side).
	if share := float64(withinTwo) / float64(len(fit)); share < 0.95 {
		t.Errorf("%.1f%% of images within 2 standard deviations of the truth, want at least 95%%", 100*share)
	}
}

// The very sparse case: half the images get one vote each, the other half
// none, so every vote is between two images that meet no one else. Each pair
// is then the one-vote case worked out by hand in TestFitOneVote: the winner
// at +0.7408 and the loser at −0.7408, uncertainty 1.5794; or both at 0 after
// a tie, uncertainty 1/√(¼ + ¼) = 1.4142 (the vote's ¼ plus the starting
// assumption's ¼). All a single vote can say is which of the two is better:
// across six seeds the correlation with the truth was 0.38 to 0.42, and over
// 99.8% of images were within 2 standard deviations.
func TestFitOneVoteEach(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, 2000)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	votes := simulateOneVoteEach(rng, truth, tiesAndSkips)
	fit := fitStrengths(t, votes, DefaultSettings().StartUncertainty)
	if len(fit) != 1000 {
		t.Errorf("%d images ranked, want the 1000 that got a vote", len(fit))
	}

	const winner, winnerUncertainty = 0.7407743930623084, 1.5793690933393363
	tieUncertainty := 1 / math.Sqrt(0.5)
	for _, v := range votes {
		l, r := fit[v.Left], fit[v.Right]
		var want [2]Estimate
		switch v.Outcome {
		case LeftWins:
			want = [2]Estimate{{Strength: winner, Uncertainty: winnerUncertainty}, {Strength: -winner, Uncertainty: winnerUncertainty}}
		case RightWins:
			want = [2]Estimate{{Strength: -winner, Uncertainty: winnerUncertainty}, {Strength: winner, Uncertainty: winnerUncertainty}}
		default:
			want = [2]Estimate{{Strength: 0, Uncertainty: tieUncertainty}, {Strength: 0, Uncertainty: tieUncertainty}}
		}
		for k, got := range [2]Estimate{l, r} {
			if !near(got.Strength, want[k].Strength, 1e-8) || !near(got.Uncertainty, want[k].Uncertainty, 1e-8) {
				t.Errorf("vote %+v: image %d = %+v, want %+v", v, [2]int64{v.Left, v.Right}[k], got, want[k])
			}
		}
	}

	var trueOnes, fitted []float64
	withinTwo := 0
	for id, s := range fit {
		trueOnes, fitted = append(trueOnes, truth[id-1]), append(fitted, s.Strength)
		if math.Abs(s.Strength-truth[id-1]) <= 2*s.Uncertainty {
			withinTwo++
		}
	}
	if c := correlation(trueOnes, fitted); c < 0.25 {
		t.Errorf("correlation with the truth = %v, want at least 0.25", c)
	}
	if share := float64(withinTwo) / float64(len(fit)); share < 0.95 {
		t.Errorf("%.1f%% of images within 2 standard deviations of the truth, want at least 95%%", 100*share)
	}
}

// simulatePicked shows n pairs picked by information: each time it scores
// `candidates` random pairs with PairInformation on the current
// estimates, and shows the best, ties broken at random (PickBest).
// After each vote, the quick update brings the two images up to date, so
// later picks see what earlier votes taught. Skipped pairs leave no vote.
// Image i has id i+1.
func simulatePicked(rng *rand.Rand, truth []float64, n int, a answerer, bt *Model, candidates int) ([]Vote, error) {
	estimates := map[int]Estimate{}
	get := func(i int) Estimate {
		if e, ok := estimates[i]; ok {
			return e
		}
		return bt.Start()
	}
	votes := make([]Vote, 0, n)
	pairs := make([][2]int, candidates)
	scores := make([]float64, candidates)
	for range n {
		for c := range pairs {
			i := rng.IntN(len(truth))
			j := rng.IntN(len(truth) - 1)
			if j >= i {
				j++
			}
			pairs[c] = [2]int{i, j}
			scores[c] = PairInformation(get(i), get(j))
		}
		picked := pairs[PickBest(scores, rng)]
		i, j := picked[0], picked[1]
		o, skipped := a.Answer(rng, truth[i]-truth[j])
		if skipped {
			continue
		}
		l, r, err := bt.Update(get(i), get(j), o)
		if err != nil {
			return nil, err
		}
		estimates[i], estimates[j] = l, r
		votes = append(votes, Vote{Left: int64(i + 1), Right: int64(j + 1), Outcome: o})
	}
	return votes, nil
}

// simulatePicked's votes are well formed: with a simulated person who never
// skips, every pair shown becomes one vote, between two different images in
// range; with skips allowed, there are fewer votes. (How often the simulated
// person skips is checked in TestSimulateVotesMix, so it isn't pinned here.)
// Whether picking chooses better pairs is TestPickedPairsBeatRandomPairs.
func TestSimulatePicked(t *testing.T) {
	bt := newTest(t)
	truth := spacedTruth(300)
	votes, err := simulatePicked(rand.New(rand.NewPCG(502, 754)), truth, 1000, sidesOnly, bt, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(votes) != 1000 {
		t.Fatalf("%d votes from 1000 pairs shown with no skips, want 1000", len(votes))
	}
	for _, v := range votes {
		if v.Left == v.Right || v.Left < 1 || v.Left > 300 || v.Right < 1 || v.Right > 300 {
			t.Fatalf("vote %+v: want two different images with ids 1 to 300", v)
		}
	}
	votes, err = simulatePicked(rand.New(rand.NewPCG(502, 754)), truth, 1000, tiesAndSkips, bt, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(votes) >= 1000 || len(votes) == 0 {
		t.Fatalf("%d votes from 1000 pairs shown with skips, want some but fewer than 1000", len(votes))
	}
}

// Picking pairs by information (PairInformation, with
// PickBest) should teach more than random pairs, as Glickman & Jensen
// (2005) found for chess tournaments.
//
// 500 images, 2000 pairs shown (about 4 votes per image), the same simulated
// person either way. Every image is scored, an unvoted one at its start (0).
// Over 20 seeds the average correlation with the truth went from 0.759
// (random) to 0.793 (picked), with picking ahead in all 20. The uncertainty
// stays honest: at least 92% within 2 standard deviations with picked pairs.
func TestPickedPairsBeatRandomPairs(t *testing.T) {
	rng := rand.New(rand.NewPCG(502, 754))
	truth := make([]float64, 500)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	const shown = 2000
	bt := newTest(t)
	// The votes use their own seed: the same seed as the truth would give
	// the same random numbers, quietly linking the pairs to the truth.
	picked, err := simulatePicked(rand.New(rand.NewPCG(754, 502)), truth, shown, tiesAndSkips, bt, 100)
	if err != nil {
		t.Fatal(err)
	}
	random := simulateVotes(rand.New(rand.NewPCG(754, 502)), truth, shown, tiesAndSkips)

	score := func(name string, votes []Vote) float64 {
		fit, err := bt.Fit(context.Background(), votes)
		if err != nil {
			t.Fatalf("%s: Fit: %v", name, err)
		}
		all := make([]float64, len(truth)) // unvoted images stay at their start, 0
		withinTwo := 0
		for id, e := range fit {
			all[id-1] = e.Strength
			if math.Abs(e.Strength-truth[id-1]) <= 2*e.Uncertainty {
				withinTwo++
			}
		}
		if share := float64(withinTwo) / float64(len(fit)); share < 0.9 {
			t.Errorf("%s: %.1f%% of images within 2 standard deviations of the truth, want about 95%%", name, 100*share)
		}
		return correlation(truth, all)
	}
	r, p := score("random pairs", random), score("picked pairs", picked)
	if p <= r {
		t.Errorf("correlation with the truth: picked pairs %v, random pairs %v; want picked to do better", p, r)
	}
}

// BenchmarkFit times the refit in a 1M-image library at the counts of pairs
// shown we expect from people voting by hand (with tiesAndSkips, about 90%
// become votes): 10k is years of casual use, and 100k is a heavy user.
//
// Fit only ever sees votes, so the library size matters through how spread
// out they are: picked from the whole library, almost every vote meets two
// images nobody has compared yet; picked within one search, votes link up.
//
// Besides time and memory, it reports how accurate the result is (see
// accuracy), over the ranked images: avg_uncertainty (and as avg_RD),
// implied_range_% (how much of the library, or of the search, that average
// uncertainty spans), and truth_in_range_% (about 68 if the uncertainty is
// honest). An unranked image's uncertainty is 2, which spans about 95% of a
// library spread like this one. Run with:
//
//	go test -run '^$' -bench BenchmarkFit -benchmem ./internal/rank/
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
				vs := simulateVotes(rand.New(rand.NewPCG(754, 502)), poolTruth, shown, tiesAndSkips)
				bt := newTest(b)
				var fit map[int64]Estimate
				for b.Loop() {
					var err error
					if fit, err = bt.Fit(context.Background(), vs); err != nil {
						b.Fatal(err)
					}
				}
				avgUncertainty, impliedRange, withinRange := accuracy(fit, poolTruth, sortedTruth)
				b.ReportMetric(float64(len(fit)), "ranked_images")
				b.ReportMetric(avgUncertainty, "avg_uncertainty")
				b.ReportMetric(RDFromUncertainty(avgUncertainty), "avg_RD")
				b.ReportMetric(100*impliedRange, "implied_range_%")
				b.ReportMetric(100*withinRange, "truth_in_range_%")
			})
		}
	}
}
