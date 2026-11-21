package glicko2

import (
	"errors"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

// Update counts one vote, made at time at, in both images' rating period.
// Each image first closes any of its periods that have ended. The vote then
// adds to the period's running sums, against the opponent's values from the
// start of the period, as the paper does for all of a period's games. So the
// order of votes within a period doesn't matter.
func (alg *algorithm) Update(left, right ranking.State, outcome ranking.Outcome, at time.Time) (ranking.State, ranking.State, error) {
	score, err := outcome.LeftScore()
	if err != nil {
		return nil, nil, err
	}
	if at.IsZero() {
		return nil, nil, errors.New("glicko-2 needs the time of each vote")
	}
	l := alg.advance(left.(state), at)
	r := alg.advance(right.(state), at)
	return addGame(l, r, score), addGame(r, l, 1-score), nil
}

// addGame adds one game against opponent to me's period sums (steps 3 and 4),
// where score is me's result: 1 won, ½ tie, 0 lost.
func addGame(me, opponent state, score float64) state {
	weight := opponentWeight(opponent.Uncertainty)                    // [g(φj)]
	expected := expectedScore(me.Strength, opponent.Strength, weight) // [E(µ, µj, φj)]
	me.PeriodGames++
	me.PeriodInformation += weight * weight * expected * (1 - expected)
	me.PeriodSurprise += weight * (score - expected)
	return me
}
