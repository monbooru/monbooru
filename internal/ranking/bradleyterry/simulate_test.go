package bradleyterry

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"

	"github.com/monbooru/monbooru/internal/ranking"
)

// answerer is a simulated person answering pairs, following Rao & Kupper's
// (1967) tie model. Each time a pair is shown, they feel a preference: the
// true strength gap plus random noise. If the feeling is weaker than
// threshold, they don't pick a side, and either call it a tie or skip, 50/50.
// Otherwise they pick the side they felt. So:
//
//	left chance      = sigmoid(gap − threshold)
//	right chance     = sigmoid(−gap − threshold)
//	no side picked   = sigmoid(gap + threshold) − sigmoid(gap − threshold)
//
// Close images are often left undecided; far apart ones almost never. With a
// threshold of 0 it's plain Bradley–Terry: left wins with sigmoid(gap).
type answerer struct{ threshold float64 }

var (
	// realisticAnswers matches the following targets: 20% of pairs get no side,
	// so about 10% ties and 10% skips, and 40% left and 40% right. Per gap
	// (worked out in Python):
	//
	//	gap (ranking points)   0     1 (174)  2 (347)  3 (521)  4 (695)
	//	tie chance             13.6%  10.9%    6.0%     2.6%     1.0%
	realisticAnswers = answerer{threshold: 0.56}
	// modelOnlyAnswers always picks a side, by the win chance alone: exactly
	// what the fit assumes, for checking that the fit itself is right.
	modelOnlyAnswers = answerer{}
)

// answer is one answer to a pair whose true strengths differ by gap (left −
// right). skipped means no vote is stored.
func (a answerer) answer(rng *rand.Rand, gap float64) (o ranking.Outcome, skipped bool) {
	u := rng.Float64()
	felt := gap + math.Log(u/(1-u)) // logistic noise: P(felt > 0) = sigmoid(gap)
	switch {
	case felt > a.threshold:
		return leftWins, false
	case felt < -a.threshold:
		return rightWins, false
	case rng.IntN(2) == 0:
		return tie, false
	default:
		return ranking.Outcome{}, true
	}
}

// simulateVotes shows n random pairs of images whose true strengths are
// known, and returns the votes. Skipped pairs leave no vote, so with
// realisticAnswers there are about 0.9 × n. Image i has id i+1.
func simulateVotes(rng *rand.Rand, truth []float64, n int, a answerer) []ranking.Vote {
	votes := make([]ranking.Vote, 0, n)
	for range n {
		i := rng.IntN(len(truth))
		j := rng.IntN(len(truth) - 1)
		if j >= i {
			j++ // any image but i
		}
		if o, skipped := a.answer(rng, truth[i]-truth[j]); !skipped {
			votes = append(votes, vote(int64(i+1), int64(j+1), o))
		}
	}
	return votes
}

// At fixed gaps, the simulated answers follow Rao & Kupper's chances.
func TestAnswerChances(t *testing.T) {
	const draws = 200_000 // chance moves each share by about 0.1%
	rng := rand.New(rand.NewPCG(9, 9))
	for _, a := range []answerer{realisticAnswers, modelOnlyAnswers} {
		for _, gap := range []float64{-1, 0, 1, 2, 4} {
			counts := map[string]float64{}
			for range draws {
				o, skipped := a.answer(rng, gap)
				switch {
				case skipped:
					counts["skip"]++
				case o == leftWins:
					counts["left"]++
				case o == rightWins:
					counts["right"]++
				case o == tie:
					counts["tie"]++
				}
			}
			undecided := sigmoid(gap+a.threshold) - sigmoid(gap-a.threshold)
			want := map[string]float64{
				"left":  sigmoid(gap - a.threshold),
				"right": sigmoid(-gap - a.threshold),
				"tie":   undecided / 2,
				"skip":  undecided / 2,
			}
			for name, w := range want {
				if got := counts[name] / draws; !near(got, w, 0.005) {
					t.Errorf("threshold %v, gap %v: %s share = %.4f, want %.4f", a.threshold, gap, name, got, w)
				}
			}
		}
	}
}

// Over random pairs of typical images, realisticAnswers gives the our targets:
// 10% skips, 10% ties, and left and right about 40% each.
func TestSimulateVotesMix(t *testing.T) {
	const shown = 100_000
	truth := make([]float64, 1000)
	rng := rand.New(rand.NewPCG(5, 5))
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	counts := map[ranking.Winner]int{}
	votes := simulateVotes(rng, truth, shown, realisticAnswers)
	for _, v := range votes {
		counts[v.Outcome.Winner]++
	}
	// With 100,000 pairs, chance alone moves each share by well under 1%.
	shares := []struct {
		name      string
		got, want float64
	}{
		{"skip", float64(shown-len(votes)) / shown, 0.1},
		{"tie", float64(counts[ranking.WinnerTie]) / shown, 0.1},
		{"left", float64(counts[ranking.WinnerLeft]) / shown, 0.4},
		{"right", float64(counts[ranking.WinnerRight]) / shown, 0.4},
	}
	for _, s := range shares {
		if !near(s.got, s.want, 0.01) {
			t.Errorf("%s share = %.3f, want about %.2f", s.name, s.got, s.want)
		}
	}
}

// correlation is the Pearson correlation of two lists: 1 when one is an
// exact rising straight line of the other, 0 when unrelated.
func correlation(a, b []float64) float64 {
	n := float64(len(a))
	var meanA, meanB float64
	for i := range a {
		meanA += a[i] / n
		meanB += b[i] / n
	}
	var cov, varA, varB float64
	for i := range a {
		cov += (a[i] - meanA) * (b[i] - meanB)
		varA += (a[i] - meanA) * (a[i] - meanA)
		varB += (b[i] - meanB) * (b[i] - meanB)
	}
	return cov / math.Sqrt(varA*varB)
}

// spacedTruth gives n true strengths evenly spread from −2 to 2 (rankings
// about 1150 to 1850), so they average 0, like the fit does.
func spacedTruth(n int) []float64 {
	truth := make([]float64, n)
	for i := range truth {
		truth[i] = -2 + 4*float64(i)/float64(n-1)
	}
	return truth
}

// With plenty of votes that follow the model exactly, the refit should find
// the true strengths: closely correlated, and each within a few
// uncertainties of the truth. This checks the fit itself; the realistic mix
// of answers is in the next test. The random numbers are seeded, so the test
// always sees the same votes.
func TestFitRecoversSimulatedStrengths(t *testing.T) {
	truth := spacedTruth(40)
	votes := simulateVotes(rand.New(rand.NewPCG(42, 1)), truth, 4000, modelOnlyAnswers) // about 200 votes per image
	fit := fitStrengths(t, votes, DefaultSettings().StartUncertainty)

	fitted := make([]float64, len(truth))
	withinTwo := 0
	for i, want := range truth {
		got := fit[int64(i+1)]
		fitted[i] = got.Strength
		miss := math.Abs(got.Strength - want)
		if miss > 4*got.Uncertainty {
			t.Errorf("image %d: fitted %v ± %v, truth %v: more than 4 uncertainties off", i+1, got.Strength, got.Uncertainty, want)
		}
		if miss <= 2*got.Uncertainty {
			withinTwo++
		}
	}
	if c := correlation(truth, fitted); c < 0.98 {
		t.Errorf("correlation with the truth = %v, want at least 0.98", c)
	}
	// If the uncertainty is honest, about 95% land within 2 of them.
	if share := float64(withinTwo) / float64(len(truth)); share < 0.85 {
		t.Errorf("%.0f%% of images within 2 uncertainties of the truth, want about 95%%", 100*share)
	}
}

// With realistic answers (ties and skips likelier between close images),
// the fit should still be close: same order, about the true spread, and
// honest uncertainty. It isn't exact, because counting a tie as half a win
// isn't quite Rao & Kupper's model. Worked out separately in Python for
// these 40 images, with unlimited votes: fitted ≈ 1.06 × truth. Across five
// seeds the measured slope was 1.03 to 1.08, and 38 to 40 of the 40 images
// were within 2 uncertainties.
func TestFitWithRealisticAnswers(t *testing.T) {
	truth := spacedTruth(40)
	votes := simulateVotes(rand.New(rand.NewPCG(42, 1)), truth, 4000, realisticAnswers) // about 180 votes per image
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
		t.Errorf("%.0f%% of images within 2 uncertainties of the truth, want about 95%%", 100*share)
	}
}

// slope is how much b changes per unit of a, on the best straight line
// through the points (least squares).
func slope(a, b []float64) float64 {
	n := float64(len(a))
	var meanA, meanB float64
	for i := range a {
		meanA += a[i] / n
		meanB += b[i] / n
	}
	var cov, varA float64
	for i := range a {
		cov += (a[i] - meanA) * (b[i] - meanB)
		varA += (a[i] - meanA) * (a[i] - meanA)
	}
	return cov / varA
}

// The quick update sees the same votes one at a time. It should get close
// to the truth and to the refit, though less exactly than the refit.
func TestQuickUpdateTracksSimulatedStrengths(t *testing.T) {
	bt := newTest(t)
	truth := spacedTruth(40)
	votes := simulateVotes(rand.New(rand.NewPCG(42, 1)), truth, 4000, realisticAnswers)

	states := map[int64]ranking.State{}
	get := func(id int64) ranking.State {
		if s, ok := states[id]; ok {
			return s
		}
		return bt.Start()
	}
	for _, v := range votes {
		l, r, err := bt.Update(get(v.Left), get(v.Right), v.Outcome)
		if err != nil {
			t.Fatal(err)
		}
		states[v.Left], states[v.Right] = l, r
	}

	fit := fitStrengths(t, votes, DefaultSettings().StartUncertainty)
	quick := make([]float64, len(truth))
	refit := make([]float64, len(truth))
	for i := range truth {
		quick[i] = bt.Shown(states[int64(i+1)]).Strength
		refit[i] = fit[int64(i+1)].Strength
	}
	if c := correlation(truth, quick); c < 0.95 {
		t.Errorf("quick update's correlation with the truth = %v, want at least 0.95", c)
	}
	if c := correlation(refit, quick); c < 0.95 {
		t.Errorf("quick update's correlation with the refit = %v, want at least 0.95", c)
	}
}

// The sparse case from the plan's assumptions: most images get only one to
// three votes. The refit must still finish with finite numbers, and its
// uncertainty must be honest: a vote or two can't say much, and the
// uncertainty has to admit it.
func TestFitSparseVotes(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	truth := make([]float64, 2000)
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	votes := simulateVotes(rng, truth, 2000, realisticAnswers) // 2000 pairs shown: about 1800 votes
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
	// Honest uncertainty: at least 95% within 2 uncertainties (it was over
	// 99% across seeds, so the uncertainty is on the cautious side).
	if share := float64(withinTwo) / float64(len(fit)); share < 0.95 {
		t.Errorf("%.1f%% of images within 2 uncertainties of the truth, want at least 95%%", 100*share)
	}
}

// accuracy sums up a fit against the known truth, over the images it ranked.
//
//   - avgUncertainty: the average uncertainty, in strength units (× 173.72
//     for RD in ranking points).
//   - impliedRange: how wide that average uncertainty is, as a share of the
//     library: the share of all the library's true strengths within ± the
//     average uncertainty of the library's middle. Near 1 = an image could
//     be almost anywhere; small = its place is pinned down. With a library
//     spread like a bell curve of width w, it's about 68% when the average
//     uncertainty equals w.
//   - withinRange: the share of ranked images whose true strength is within
//     ± their own uncertainty of their fitted one. About 0.68 if the
//     uncertainty is honest, whatever its size.
//
// sortedTruth is every library image's true strength, sorted.
func accuracy(fit map[int64]ranking.State, truth, sortedTruth []float64) (avgUncertainty, impliedRange, withinRange float64) {
	for id, s := range fit {
		e := s.(state)
		avgUncertainty += e.Uncertainty
		if t := truth[id-1]; math.Abs(t-e.Strength) <= e.Uncertainty {
			withinRange++
		}
	}
	n := float64(len(fit))
	avgUncertainty, withinRange = avgUncertainty/n, withinRange/n

	middle := 0.0
	for _, t := range sortedTruth {
		middle += t / float64(len(sortedTruth))
	}
	inside := sort.SearchFloat64s(sortedTruth, middle+avgUncertainty) - sort.SearchFloat64s(sortedTruth, middle-avgUncertainty)
	return avgUncertainty, float64(inside) / float64(len(sortedTruth)), withinRange
}

func TestAccuracy(t *testing.T) {
	// 401 true strengths, −2 to 2 in steps of 0.01, so the middle is 0.
	truth := make([]float64, 401)
	for i := range truth {
		truth[i] = -2 + float64(i)/100
	}
	// Image 201's truth is 0, inside its 0 ± 0.5; image 1's truth is −2,
	// outside its 0 ± 1.5. The average uncertainty is 1, and −1 to 1 holds
	// 201 of the 401 true strengths.
	fit := map[int64]ranking.State{
		201: state{Strength: 0, Uncertainty: 0.5},
		1:   state{Strength: 0, Uncertainty: 1.5},
	}
	avgUncertainty, impliedRange, withinRange := accuracy(fit, truth, truth)
	if !near(avgUncertainty, 1, 1e-12) {
		t.Errorf("avgUncertainty = %v, want 1", avgUncertainty)
	}
	// A bit of tolerance: −1 and 1 themselves are computed, so they may land
	// a hair either side.
	if !near(impliedRange, 201.0/401, 1.0/401) {
		t.Errorf("impliedRange = %v, want about %v", impliedRange, 201.0/401)
	}
	if withinRange != 0.5 {
		t.Errorf("withinRange = %v, want 0.5", withinRange)
	}

	// With true strengths on a bell curve of width 1 (just what we're assuming
	// for this example, not always the case), an average uncertainty of 1
	// spans about 68% of the library.
	rng := rand.New(rand.NewPCG(3, 3))
	bell := make([]float64, 200_000)
	for i := range bell {
		bell[i] = rng.NormFloat64()
	}
	slices.Sort(bell)
	_, impliedRange, _ = accuracy(map[int64]ranking.State{1: state{Strength: 0, Uncertainty: 1}}, bell, bell)
	if !near(impliedRange, 0.6827, 0.005) {
		t.Errorf("uncertainty 1 on a bell curve of width 1 spans %v of the library, want about 0.68", impliedRange)
	}
}

// BenchmarkFit times the refit in a 1M-image library at realistic counts
// of pairs shown (with realisticAnswers, about 90% become votes). Votes are
// made by hand, so 10k is years of casual use, and 100k is a heavy user.
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
//	go test -run '^$' -bench BenchmarkFit -benchmem ./internal/ranking/bradleyterry/
func BenchmarkFit(b *testing.B) {
	const library = 1_000_000
	rng := rand.New(rand.NewPCG(1, 2))
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
				vs := simulateVotes(rand.New(rand.NewPCG(3, 4)), poolTruth, shown, realisticAnswers)
				bt := newTest(b)
				var fit map[int64]ranking.State
				for b.Loop() {
					var err error
					if fit, err = bt.Fit(context.Background(), vs); err != nil {
						b.Fatal(err)
					}
				}
				avgUncertainty, impliedRange, withinRange := accuracy(fit, poolTruth, sortedTruth)
				b.ReportMetric(float64(len(fit)), "ranked_images")
				b.ReportMetric(avgUncertainty, "avg_uncertainty")
				b.ReportMetric(ranking.RDFromUncertainty(avgUncertainty), "avg_RD")
				b.ReportMetric(100*impliedRange, "implied_range_%")
				b.ReportMetric(100*withinRange, "truth_in_range_%")
			})
		}
	}
}
