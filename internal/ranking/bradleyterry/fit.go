package bradleyterry

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/monbooru/monbooru/internal/ranking"
)

// How the refit searches. These only control how precisely the best guess
// is found, not what it is, so they aren't settings.
const (
	// fitMaxStep caps how far one image moves in one step (a gap of 1 is
	// 73% to 27% odds). A guard: Newton's method can overshoot wildly when
	// it starts far from the answer. Starting everyone at 0 keeps the
	// steps small, so no test case has needed it yet.
	fitMaxStep = 1.0
	// fitTolerance: stop once a whole pass moves no image further than this.
	fitTolerance = 1e-10
	// fitMaxPasses: give up rather than loop forever.
	fitMaxPasses = 10_000
)

// Fit recomputes every voted image from all votes at once: the strengths
// that best explain the votes together with the bell-curve starting
// assumption (the MAP estimate), and how unsure each one is. Vote order
// doesn't matter. Images without votes aren't in the result.
func (bt *algorithm) Fit(ctx context.Context, votes []ranking.Vote) (map[int64]ranking.State, error) {
	u := bt.settings.StartUncertainty
	fitted, err := fitVotes(ctx, votes, 1/(u*u))
	if err != nil {
		return nil, err
	}
	out := make(map[int64]ranking.State, len(fitted))
	for id, s := range fitted {
		out[id] = s
	}
	return out, nil
}

// match is one vote, seen from one of its images.
type match struct {
	opponent int     // the opponent's position in the fit
	score    float64 // this image's outcome_value: 1 won, ½ tie, 0 lost
}

// fitVotes is Fit with the starting assumption given as its
// certainty, 1 / start_uncert². 0 means no starting assumption: plain
// textbook Bradley–Terry (maximum likelihood), which the tests compare with
// published examples.
//
// It uses Newton's method one image at a time: each image moves to where its
// votes and the starting assumption balance, given everyone else's current
// strength. For each image:
//
//	balance    = Σ (outcome_value − win_chance) − strength / start_uncert²
//	certainty  = Σ win_chance × (1 − win_chance) + 1 / start_uncert²
//	strength  += balance / certainty   (at most fitMaxStep either way)
//
// The sums are over the image's votes. After each pass over all images,
// each group of linked images is shifted to average 0 (see
// shiftToAverageZero), and passes repeat until nothing moves. At the best
// guess every balance is 0, and the image's uncertainty is 1/√certainty (the
// plan's per-image approximation, "diagonal Laplace").
func fitVotes(ctx context.Context, votes []ranking.Vote, priorPrecision float64) (map[int64]state, error) {
	scores := make([]float64, len(votes))
	position := map[int64]int{}
	for k, v := range votes {
		if v.Left == v.Right {
			return nil, fmt.Errorf("image %d is compared with itself", v.Left)
		}
		score, err := v.Outcome.LeftScore()
		if err != nil {
			return nil, fmt.Errorf("vote between images %d and %d: %w", v.Left, v.Right, err)
		}
		scores[k] = score
		position[v.Left], position[v.Right] = 0, 0
	}
	// Number the images in id order, so vote order can't change the result.
	ids := slices.Sorted(maps.Keys(position))
	for i, id := range ids {
		position[id] = i
	}
	matches := make([][]match, len(ids))
	for k, v := range votes {
		l, r := position[v.Left], position[v.Right]
		matches[l] = append(matches[l], match{opponent: r, score: scores[k]})
		matches[r] = append(matches[r], match{opponent: l, score: 1 - scores[k]})
	}

	group, groupCount := connectedGroups(matches)
	strength := make([]float64, len(ids)) // everyone starts average
	for pass := 0; ; pass++ {
		if pass == fitMaxPasses {
			return nil, fmt.Errorf("refit did not settle after %d passes", fitMaxPasses)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		largest := 0.0
		for i := range strength {
			balance, certainty := imageBalance(strength, i, matches[i], priorPrecision)
			step := 0.0
			if certainty > 0 {
				step = max(-fitMaxStep, min(fitMaxStep, balance/certainty))
			}
			strength[i] += step
			largest = max(largest, math.Abs(step))
		}
		shiftToAverageZero(strength, group, groupCount)
		if largest < fitTolerance {
			break
		}
	}

	out := make(map[int64]state, len(ids))
	for i, id := range ids {
		_, certainty := imageBalance(strength, i, matches[i], priorPrecision)
		s := state{Strength: strength[i], Uncertainty: 1 / math.Sqrt(certainty)}
		if !s.valid() {
			// Only possible without a starting assumption, e.g. for an image
			// that never lost, whose strength runs off to infinity.
			return nil, fmt.Errorf("the votes can't pin down image %d's strength", id)
		}
		out[id] = s
	}
	return out, nil
}

// imageBalance is image i's balance and certainty at the current strengths
// (see fitVotes).
func imageBalance(strength []float64, i int, matches []match, priorPrecision float64) (balance, certainty float64) {
	balance = -strength[i] * priorPrecision
	certainty = priorPrecision
	for _, m := range matches {
		gap := strength[i] - strength[m.opponent]
		// win_chance and 1 − win_chance, each computed directly so neither
		// rounds to 0 while the other is close to 1.
		win, lose := sigmoid(gap), sigmoid(-gap)
		balance += m.score*lose - (1-m.score)*win // = outcome_value − win_chance
		certainty += win * lose
	}
	return balance, certainty
}

// connectedGroups numbers the groups of images linked by chains of votes:
// group[i] is image i's group, from 0 to count − 1.
func connectedGroups(matches [][]match) (group []int, count int) {
	group = make([]int, len(matches))
	for i := range group {
		group[i] = -1
	}
	var stack []int
	for start := range matches {
		if group[start] >= 0 {
			continue
		}
		group[start] = count
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, m := range matches[i] {
				if group[m.opponent] < 0 {
					group[m.opponent] = count
					stack = append(stack, m.opponent)
				}
			}
		}
		count++
	}
	return group, count
}

// shiftToAverageZero moves each group of linked images, all by the same
// amount, so the group averages 0.
//
// Why that's always a step towards the best guess: moving a whole group
// changes no gap that any vote sees, so the votes are explained exactly as
// well; and the best guess averages 0 within each group (its balances add up
// to Σ strength / start_uncert², which must be 0). Without a starting
// assumption only gaps matter at all, and average 0 is just the convention.
// The image-by-image steps are slowest in exactly this direction, so this
// saves many passes. It relies on every image having the same starting
// assumption; a per-image one (e.g. from tags) would need a weighted shift.
func shiftToAverageZero(strength []float64, group []int, groupCount int) {
	sum := make([]float64, groupCount)
	size := make([]float64, groupCount)
	for i, s := range strength {
		sum[group[i]] += s
		size[group[i]]++
	}
	for i := range strength {
		strength[i] -= sum[group[i]] / size[group[i]]
	}
}

// logLikelihood is how well the strengths explain the votes: the log of the
// chance the model gave each result, added up, with a tie as half a win and
// half a loss (Weng & Lin 2011, eq. 51). 0 is a perfect fit; more negative
// is worse. Images missing from strengths count as 0. MetricGate's
// "deviance" is −2 times this.
func logLikelihood(votes []ranking.Vote, strengths map[int64]float64) (float64, error) {
	total := 0.0
	for _, v := range votes {
		score, err := v.Outcome.LeftScore()
		if err != nil {
			return 0, fmt.Errorf("vote between images %d and %d: %w", v.Left, v.Right, err)
		}
		gap := strengths[v.Left] - strengths[v.Right]
		total += score*logSigmoid(gap) + (1-score)*logSigmoid(-gap)
	}
	return total, nil
}
