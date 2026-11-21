package glicko2

import (
	"math"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

// volatilityTolerance is step 5's ε: "The value ε = 0.000001 is a
// sufficiently small choice."
const volatilityTolerance = 0.000001

// volatilityMaxSteps is a guard on step 5's loop. The paper found a median
// of 5 steps and at most 19 in 10,000 simulations.
const volatilityMaxSteps = 100

// opponentWeight is the paper's g(φ): how much a game against an opponent
// counts, given how unsure we are about that opponent. 1 for a perfectly
// known opponent, less for an unsure one.
func opponentWeight(phi float64) float64 {
	return 1 / math.Sqrt(1+3*phi*phi/(math.Pi*math.Pi))
}

// expectedScore is the paper's E(µ, µj, φj): the chance of beating the
// opponent, with the gap discounted by the opponent's weight g(φj).
func expectedScore(strength, opponentStrength, weight float64) float64 {
	return 1 / (1 + math.Exp(-weight*(strength-opponentStrength)))
}

// volatilityObjective is step 5's f(x), where x stands for ln(σ'²) and
// a = ln(σ²). The new volatility is where it crosses 0.
func volatilityObjective(x, a, delta, phi, v, tau float64) float64 {
	ex := math.Exp(x)
	return ex*(delta*delta-phi*phi-v-ex)/(2*(phi*phi+v+ex)*(phi*phi+v+ex)) - (x-a)/(tau*tau)
}

// newVolatility is step 5: the new volatility σ', found with the paper's
// "Illinois algorithm", which first brackets the answer between A and B and
// then narrows the bracket.
func newVolatility(sigma, phi, v, delta, tau float64) float64 {
	a := math.Log(sigma * sigma)
	f := func(x float64) float64 { return volatilityObjective(x, a, delta, phi, v, tau) }

	// Step 5.2: the starting bracket.
	A := a
	var B float64
	if delta*delta > phi*phi+v {
		B = math.Log(delta*delta - phi*phi - v)
	} else {
		k := 1.0
		for f(a-k*tau) < 0 {
			k++
		}
		B = a - k*tau
	}
	// Steps 5.3 and 5.4.
	fA, fB := f(A), f(B)
	for range volatilityMaxSteps {
		if math.Abs(B-A) <= volatilityTolerance {
			break
		}
		C := A + (A-B)*fA/(fB-fA)
		fC := f(C)
		if fC*fB <= 0 {
			A, fA = B, fB
		} else {
			fA /= 2
		}
		B, fB = C, fC
	}
	// Step 5.5.
	return math.Exp(A / 2)
}

// periodResult is steps 3 to 7: the image's strength, uncertainty and
// volatility at the end of its current period, from its starting values and
// the period's running sums. With no games, the starting values themselves.
func (alg *algorithm) periodResult(st state) (strength, uncertainty, volatility float64) {
	if st.PeriodGames == 0 {
		return st.Strength, st.Uncertainty, st.Volatility
	}
	if st.PeriodInformation == 0 {
		// Only games so lopsided that their expected score rounded to
		// exactly 0 or 1: they carry no information (v would be infinite),
		// so this is a period without games.
		return st.Strength, math.Sqrt(st.Uncertainty*st.Uncertainty + st.Volatility*st.Volatility), st.Volatility
	}
	v := 1 / st.PeriodInformation  // step 3
	delta := v * st.PeriodSurprise // step 4
	phi := st.Uncertainty
	sigma := newVolatility(st.Volatility, phi, v, delta, alg.settings.VolConstraint) // step 5
	phiStar := math.Sqrt(phi*phi + sigma*sigma)                                      // step 6
	newPhi := 1 / math.Sqrt(1/(phiStar*phiStar)+1/v)                                 // step 7
	newMu := st.Strength + newPhi*newPhi*st.PeriodSurprise
	return newMu, newPhi, sigma
}

// periodEpoch is the first Monday after 1 January 1970, in Unix seconds.
// Periods are counted from it, so weekly periods start on Mondays at 00:00
// UTC.
var periodEpoch = time.Date(1970, 1, 5, 0, 0, 0, 0, time.UTC).Unix()

// periodStart is when the rating period that contains t began. It counts in
// whole seconds rather than time.Duration so that a vote from a badly wrong
// clock (say, a date far in the future) still lands in the right period: a
// Duration measured from 1970 stops being correct after April 2262, and would
// put such a vote in the wrong period without any error. length is a whole
// number of seconds (New checks).
func periodStart(t time.Time, length time.Duration) time.Time {
	seconds := int64(length / time.Second)
	since := t.Unix() - periodEpoch // t.Unix() rounds down, so a moment just before a boundary stays before it
	n := since / seconds
	if since%seconds < 0 {
		n-- // round down for times before the epoch too
	}
	return time.Unix(periodEpoch+n*seconds, 0).UTC()
}

// periodsBetween is how many whole periods of length fit from a to b, in
// whole seconds like periodStart.
func periodsBetween(a, b time.Time, length time.Duration) int64 {
	return (b.Unix() - a.Unix()) / int64(length/time.Second)
}

// Advance closes every rating period of s that has ended by now.
func (alg *algorithm) Advance(s ranking.State, now time.Time) ranking.State {
	return alg.advance(s.(state), now)
}

// advance brings st into the period containing now:
//
//   - A brand-new image joins it as it is.
//   - Otherwise its current period closes: steps 3 to 8 if it had games,
//     step 6 alone if not. Every whole period after that, until now, had no
//     games, so each adds σ'² to φ² (step 6 again).
//
// It changes nothing if now is in the same period, or earlier (a clock that
// went back), or zero.
func (alg *algorithm) advance(st state, now time.Time) state {
	if now.IsZero() {
		return st
	}
	target := periodStart(now, alg.settings.PeriodLength)
	if st.PeriodStart.IsZero() {
		st.PeriodStart = target
		return st
	}
	if !target.After(st.PeriodStart) {
		return st
	}
	strength, uncertainty, volatility := alg.periodResult(st)
	if st.PeriodGames == 0 {
		// Step 6 alone; periodResult returns the starting values here.
		uncertainty = math.Sqrt(uncertainty*uncertainty + volatility*volatility)
	}
	// Whole periods passed; at least the one just closed. (After a change of
	// period length the two starts may not be a whole number apart.)
	periods := max(1, periodsBetween(st.PeriodStart, target, alg.settings.PeriodLength))
	emptyAfter := float64(periods - 1)
	uncertainty = math.Sqrt(uncertainty*uncertainty + emptyAfter*volatility*volatility)
	return state{
		Strength:    strength,
		Uncertainty: uncertainty,
		Volatility:  volatility,
		PeriodStart: target,
	}
}
