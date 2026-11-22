package rank

import (
	"math"
)

// minShrink is Weng & Lin's κ, their Section 6 value: one vote can shrink
// uncert² to at most this fraction of what it was. In a two-image vote the
// shrink is under ¼, so this never applies; it's kept so the formula reads
// like their Algorithm 1.
const minShrink = 0.0001

// Update is the quick update after one vote: Weng & Lin (2011), Algorithm 1,
// for the Bradley–Terry model, with two "teams" of one image each. Each
// image's strength moves by how surprising the result was, scaled by how
// unsure we are about it, and its uncertainty shrinks a little.
func (m *Model) Update(left, right Estimate, outcome Outcome) (Estimate, Estimate, error) {
	score, err := outcome.LeftScore()
	if err != nil {
		return Estimate{}, Estimate{}, err
	}
	// Both use the values from before the vote.
	return m.updateOne(left, right, score), m.updateOne(right, left, 1-score), nil
}

// updateOne updates image me after a vote against opponent, where score is
// me's outcome_value (1 won, ½ tie, 0 lost). The paper's symbols are in
// brackets. Some equations are simplified because our team size is 1.
func (m *Model) updateOne(me, opponent Estimate, score float64) Estimate {
	variance := me.Uncertainty * me.Uncertainty // [σi²]
	// [c_iq], eq. (49). Grows when either image is unsure, which pulls the
	// prediction towards 50% and makes the vote move things less.
	spread := math.Sqrt(variance + opponent.Uncertainty*opponent.Uncertainty + 2*m.settings.PerfVariance)
	winChance := sigmoid((me.Strength - opponent.Strength) / spread) // [p̂_iq], eq. (49)
	strength := me.Strength + variance/spread*(score-winChance)      // [δq], eq. (49)
	// [γq]: Section 6 sets it to σi / c_iq for Bradley–Terry, which slows
	// the shrinking down as an image gets more certain.
	shrinkRate := me.Uncertainty / spread
	shrink := shrinkRate * variance / (spread * spread) * winChance * (1 - winChance) // [ηq], eq. (50)
	variance *= math.Max(1-shrink, minShrink)
	return Estimate{Strength: strength, Uncertainty: math.Sqrt(variance)}
}
