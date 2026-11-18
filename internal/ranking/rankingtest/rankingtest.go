// Package rankingtest helps test ranking algorithms against simulated votes:
// a simulated person answering pairs of images whose true strengths are
// known, and measures of how close a fit came to the truth. Only tests
// import it.
package rankingtest

import (
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

var (
	leftWins  = ranking.Outcome{Winner: ranking.WinnerLeft}
	tie       = ranking.Outcome{Winner: ranking.WinnerTie}
	rightWins = ranking.Outcome{Winner: ranking.WinnerRight}
)

// Answerer is a simulated person answering pairs, following Rao & Kupper's
// (1967) tie model. Each time a pair is shown, they feel a preference: the
// true strength gap plus random noise. If the feeling is weaker than
// Threshold, they don't pick a side, and either call it a tie or skip, 50/50.
// Otherwise they pick the side they felt. So:
//
//	left chance      = sigmoid(gap − Threshold)
//	right chance     = sigmoid(−gap − Threshold)
//	no side picked   = sigmoid(gap + Threshold) − sigmoid(gap − Threshold)
//
// Close images are often left undecided; far apart ones almost never. With a
// Threshold of 0 it's plain Bradley–Terry: left wins with sigmoid(gap).
type Answerer struct{ Threshold float64 }

var (
	// TiesAndSkips is a rough guess, not based on real data. For random pairs
	// of typical images, we'll assume that about 10% are ties, about 10% are
	// skips, about 40% of the time the user picks left and about 40% of the
	// time the user picks right. Per gap (worked out in Python):
	//
	//	gap (ranking points)   0     1 (174)  2 (347)  3 (521)  4 (695)
	//	tie chance             13.6%  10.9%    6.0%     2.6%     1.0%
	TiesAndSkips = Answerer{Threshold: 0.56}
	// SidesOnly always picks a side, by the win chance alone: exactly what
	// Bradley–Terry assumes, for checking that a fit itself is right.
	SidesOnly = Answerer{}
)

// Answer is one answer to a pair whose true strengths differ by gap (left −
// right). skipped means no vote is stored.
func (a Answerer) Answer(rng *rand.Rand, gap float64) (o ranking.Outcome, skipped bool) {
	u := rng.Float64()
	felt := gap + math.Log(u/(1-u)) // logistic noise: P(felt > 0) = sigmoid(gap)
	switch {
	case felt > a.Threshold:
		return leftWins, false
	case felt < -a.Threshold:
		return rightWins, false
	case rng.IntN(2) == 0:
		return tie, false
	default:
		return ranking.Outcome{}, true
	}
}

// SimulateVotes shows n random pairs of images whose true strengths are
// known, and returns the votes. Skipped pairs leave no vote, so with
// TiesAndSkips there are about 0.9 × n. Image i has id i+1.
func SimulateVotes(rng *rand.Rand, truth []float64, n int, a Answerer) []ranking.Vote {
	votes := make([]ranking.Vote, 0, n)
	for range n {
		i := rng.IntN(len(truth))
		j := rng.IntN(len(truth) - 1)
		if j >= i {
			j++ // any image but i
		}
		if o, skipped := a.Answer(rng, truth[i]-truth[j]); !skipped {
			votes = append(votes, ranking.Vote{Left: int64(i + 1), Right: int64(j + 1), Outcome: o})
		}
	}
	return votes
}

// SpreadOverTime returns a copy of votes made evenly over span, starting at
// start, in their order: for algorithms that care when votes were made.
func SpreadOverTime(votes []ranking.Vote, start time.Time, span time.Duration) []ranking.Vote {
	out := slices.Clone(votes)
	for i := range out {
		out[i].At = start.Add(time.Duration(float64(span) * float64(i) / float64(len(out))))
	}
	return out
}

// SpacedTruth gives n true strengths evenly spread from −2 to 2 (rankings
// about 1150 to 1850), so they average 0, like a fit does.
func SpacedTruth(n int) []float64 {
	truth := make([]float64, n)
	for i := range truth {
		truth[i] = -2 + 4*float64(i)/float64(n-1)
	}
	return truth
}

// Correlation is the Pearson correlation of two lists: 1 when one is an
// exact rising straight line of the other, −1 for a falling one, 0 when
// unrelated.
func Correlation(a, b []float64) float64 {
	meanA, meanB := mean(a), mean(b)
	var cov, varA, varB float64
	for i := range a {
		cov += (a[i] - meanA) * (b[i] - meanB)
		varA += (a[i] - meanA) * (a[i] - meanA)
		varB += (b[i] - meanB) * (b[i] - meanB)
	}
	return cov / math.Sqrt(varA*varB)
}

// Slope is how much b changes per unit of a, on the best straight line
// through the points (least squares).
func Slope(a, b []float64) float64 {
	meanA, meanB := mean(a), mean(b)
	var cov, varA float64
	for i := range a {
		cov += (a[i] - meanA) * (b[i] - meanB)
		varA += (a[i] - meanA) * (a[i] - meanA)
	}
	return cov / varA
}

func mean(x []float64) float64 {
	total := 0.0
	for _, v := range x {
		total += v
	}
	return total / float64(len(x))
}

// Accuracy sums up a fit against the known truth, over the images it ranked
// (see ranking.Algorithm.Shown for turning states into estimates).
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
// truth[i] is image i+1's true strength, and sortedTruth the same, sorted.
func Accuracy(estimates map[int64]ranking.Estimate, truth, sortedTruth []float64) (avgUncertainty, impliedRange, withinRange float64) {
	for id, e := range estimates {
		avgUncertainty += e.Uncertainty
		if t := truth[id-1]; math.Abs(t-e.Strength) <= e.Uncertainty {
			withinRange++
		}
	}
	n := float64(len(estimates))
	avgUncertainty, withinRange = avgUncertainty/n, withinRange/n

	middle := mean(sortedTruth)
	inside := sort.SearchFloat64s(sortedTruth, middle+avgUncertainty) - sort.SearchFloat64s(sortedTruth, middle-avgUncertainty)
	return avgUncertainty, float64(inside) / float64(len(sortedTruth)), withinRange
}
