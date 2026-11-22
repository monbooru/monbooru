package rank

import (
	"math"
	"testing"
)

// update runs bt.Update and fails the test on an error.
func update(t *testing.T, bt *Model, left, right Estimate, o Outcome) (Estimate, Estimate) {
	t.Helper()
	l, r, err := bt.Update(left, right, o)
	if err != nil {
		t.Fatalf("Update(%+v, %+v, %+v): %v", left, right, o, err)
	}
	return l, r
}

func checkEstimate(t *testing.T, name string, got, want Estimate) {
	t.Helper()
	if !near(got.Strength, want.Strength, 1e-9) || !near(got.Uncertainty, want.Uncertainty, 1e-9) {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}

// The next three tests are hand-computed from Weng & Lin (2011), Algorithm 1,
// with one image per team and PerfVariance ½, in updateOne's names:
//
//	spread     = √(variance + opponent's variance + 2 × PerfVariance)
//	winChance  = sigmoid((strength − opponent's strength) / spread)
//	strength  += variance / spread × (score − winChance)
//	shrinkRate = uncertainty / spread                    (their Section 6)
//	shrink     = shrinkRate × variance / spread² × winChance × (1 − winChance)
//	variance  ×= 1 − shrink
//
// where variance is uncertainty², and score is 1 for a win, ½ for a tie and
// 0 for a loss.
// The wanted values were also checked with a separate Python script.

func TestUpdateTwoNewImages(t *testing.T) {
	bt := newTest(t)
	start := bt.Start()
	// spread    = √(4 + 4 + 1) = 3
	// winChance = sigmoid(0 / 3) = ½
	// strength  = 0 ± 4/3 × ½ = ±2/3
	// shrink    = 2/3 × 4/9 × ½ × ½ = 2/27
	// variance  = 4 × (1 − 2/27) = 100/27, so uncertainty = 10/√27 ≈ 1.924501
	l, r := update(t, bt, start, start, LeftWins)
	checkEstimate(t, "winner", l, Estimate{Strength: 2.0 / 3, Uncertainty: 10 / math.Sqrt(27)})
	checkEstimate(t, "loser", r, Estimate{Strength: -2.0 / 3, Uncertainty: 10 / math.Sqrt(27)})
}

func TestUpdateUpset(t *testing.T) {
	bt := newTest(t)
	// A fairly certain strong image loses to an unsure average one.
	strong := Estimate{Strength: 1, Uncertainty: 0.5}
	unsure := Estimate{Strength: 0, Uncertainty: 1.5}
	// spread             = √(0.25 + 2.25 + 1) = √3.5 ≈ 1.870829
	// strong winChance   = sigmoid(1 / 1.870829) ≈ 0.630537
	// strong strength    = 1 + 0.25 / 1.870829 × (0 − 0.630537) ≈ 0.915741
	// strong shrink      = (0.5 / 1.870829) × (0.25 / 3.5) × 0.630537 × 0.369463 ≈ 0.004447
	// strong uncertainty = 0.5 × √(1 − 0.004447) ≈ 0.498887
	// unsure strength    = 0 + 2.25 / 1.870829 × (1 − 0.369463) ≈ 0.758332
	// unsure shrink      = (1.5 / 1.870829) × (2.25 / 3.5) × 0.630537 × 0.369463 ≈ 0.120075
	// unsure uncertainty = 1.5 × √(1 − 0.120075) ≈ 1.407065
	l, r := update(t, bt, strong, unsure, RightWins)
	checkEstimate(t, "strong loser", l, Estimate{Strength: 0.9157409103463072, Uncertainty: 0.49888695428742236})
	checkEstimate(t, "unsure winner", r, Estimate{Strength: 0.7583318068832351, Uncertainty: 1.407064652899043})
	// The unsure image learned far more than the certain one, in both
	// strength and uncertainty.
	if math.Abs(r.Strength-unsure.Strength) <= math.Abs(l.Strength-strong.Strength) {
		t.Errorf("the unsure image moved less than the certain one")
	}
}

func TestUpdateTie(t *testing.T) {
	bt := newTest(t)
	higher := Estimate{Strength: 1, Uncertainty: 1}
	lower := Estimate{Strength: 0, Uncertainty: 1}
	// spread             = √(1 + 1 + 1) = √3 ≈ 1.732051
	// higher winChance   = sigmoid(1 / 1.732051) ≈ 0.640457
	// higher strength    = 1 + 1 / 1.732051 × (0.5 − 0.640457) ≈ 0.918907
	// lower strength     = 0 + 1 / 1.732051 × (0.5 − 0.359543) ≈ 0.081093
	// shrink (both)      = (1 / 1.732051) × (1 / 3) × 0.640457 × 0.359543 ≈ 0.044316
	// uncertainty (both) = √(1 − 0.044316) ≈ 0.977591
	l, r := update(t, bt, higher, lower, Tie)
	checkEstimate(t, "higher", l, Estimate{Strength: 0.9189068386060943, Uncertainty: 0.9775910142533948})
	checkEstimate(t, "lower", r, Estimate{Strength: 0.08109316139390564, Uncertainty: 0.9775910142533948})
}

// updateSamples covers new, settled, strong, weak and lopsided images.
var updateSamples = []Estimate{
	{Strength: 0, Uncertainty: 2},
	{Strength: 0, Uncertainty: 0.3},
	{Strength: 1.5, Uncertainty: 1},
	{Strength: -2, Uncertainty: 0.5},
	{Strength: 4, Uncertainty: 0.1},
	{Strength: -0.5, Uncertainty: 3},
}

var allOutcomes = []Outcome{LeftWins, Tie, RightWins}

// Showing the same two images the other way round, with the pick on the
// same image, must give the same result.
func TestUpdateSidesAreSymmetric(t *testing.T) {
	bt := newTest(t)
	mirror := map[Outcome]Outcome{LeftWins: RightWins, Tie: Tie, RightWins: LeftWins}
	for _, a := range updateSamples {
		for _, b := range updateSamples {
			for _, o := range allOutcomes {
				l, r := update(t, bt, a, b, o)
				r2, l2 := update(t, bt, b, a, mirror[o])
				checkEstimate(t, "left image", l2, l)
				checkEstimate(t, "right image", r2, r)
			}
		}
	}
}

func TestUpdateDirection(t *testing.T) {
	bt := newTest(t)
	for _, a := range updateSamples {
		for _, b := range updateSamples {
			l, r := update(t, bt, a, b, LeftWins)
			if l.Strength <= a.Strength || r.Strength >= b.Strength {
				t.Errorf("%+v beat %+v: winner %v → %v, loser %v → %v; the winner must rise and the loser fall",
					a, b, a.Strength, l.Strength, b.Strength, r.Strength)
			}
			// A tie pulls the two towards each other, and changes nothing
			// between equals.
			l, r = update(t, bt, a, b, Tie)
			switch {
			case a.Strength > b.Strength:
				if l.Strength >= a.Strength || r.Strength <= b.Strength {
					t.Errorf("tie %+v vs %+v: the higher must fall and the lower rise, got %v, %v", a, b, l.Strength, r.Strength)
				}
			case a.Strength == b.Strength:
				if l.Strength != a.Strength || r.Strength != b.Strength {
					t.Errorf("tie between equals %+v vs %+v moved them to %v, %v", a, b, l.Strength, r.Strength)
				}
			}
		}
	}
}

// What one image gains, the other loses, in proportion to how unsure we are
// about each: change / uncertainty² is equal and opposite.
func TestUpdateBalance(t *testing.T) {
	bt := newTest(t)
	for _, a := range updateSamples {
		for _, b := range updateSamples {
			for _, o := range allOutcomes {
				l, r := update(t, bt, a, b, o)
				leftShare := (l.Strength - a.Strength) / (a.Uncertainty * a.Uncertainty)
				rightShare := (r.Strength - b.Strength) / (b.Uncertainty * b.Uncertainty)
				if !near(leftShare, -rightShare, 1e-9) {
					t.Errorf("%+v vs %+v, %+v: change/uncertainty² = %v and %v, want equal and opposite", a, b, o, leftShare, rightShare)
				}
			}
		}
	}
}

// Every vote teaches something, so uncertainty shrinks, but by less than a
// quarter of the variance per vote (shrink ≤ ¼), and never to 0.
func TestUpdateShrinksUncertaintyGently(t *testing.T) {
	bt := newTest(t)
	for _, a := range updateSamples {
		for _, b := range updateSamples {
			for _, o := range allOutcomes {
				l, r := update(t, bt, a, b, o)
				for _, pair := range [][2]Estimate{{a, l}, {b, r}} {
					before, after := pair[0].Uncertainty, pair[1].Uncertainty
					if after >= before || after*after < 0.75*before*before {
						t.Errorf("%+v vs %+v, %+v: uncertainty %v → %v, want smaller, and uncertainty² down by at most ¼", a, b, o, before, after)
					}
				}
			}
		}
	}
}

// Beating a stronger opponent is more surprising than beating a weaker one,
// so it moves the winner further.
func TestUpdateUpsetsMoveMore(t *testing.T) {
	bt := newTest(t)
	me := Estimate{Strength: 0, Uncertainty: 1}
	previousGain := 0.0
	for _, opponent := range []float64{-3, -1, 0, 1, 3} {
		l, _ := update(t, bt, me, Estimate{Strength: opponent, Uncertainty: 1}, LeftWins)
		gain := l.Strength - me.Strength
		if gain <= previousGain {
			t.Errorf("beating strength %v gained %v, not more than beating a weaker opponent (%v)", opponent, gain, previousGain)
		}
		previousGain = gain
	}
}

// The more unsure we are about an image, the more one vote moves it.
func TestUpdateUnsureImagesMoveMore(t *testing.T) {
	bt := newTest(t)
	opponent := Estimate{Strength: 0, Uncertainty: 1}
	previousGain := 0.0
	for _, uncertainty := range []float64{0.1, 0.5, 1, 2, 3} {
		l, _ := update(t, bt, Estimate{Strength: 0, Uncertainty: uncertainty}, opponent, LeftWins)
		if gain := l.Strength; gain <= previousGain {
			t.Errorf("uncertainty %v gained %v, not more than a surer image (%v)", uncertainty, gain, previousGain)
		} else {
			previousGain = gain
		}
	}
}

// With PerfVariance ½, once both images are certain the quick update
// predicts with the refit's sigmoid(gap), so both use the same strength
// units. With uncertainties near 0, spread is near 1, and a win moves
// strength by uncertainty² × (1 − sigmoid(gap)).
func TestUpdateMatchesRefitUnitsWhenCertain(t *testing.T) {
	bt := newTest(t)
	const u = 1e-4
	for _, gap := range []float64{-2, -1, 0, 1, 2} {
		l, _ := update(t, bt, Estimate{Strength: gap, Uncertainty: u}, Estimate{Strength: 0, Uncertainty: u}, LeftWins)
		implied := 1 - (l.Strength-gap)/(u*u) // the win chance the update used
		if !near(implied, sigmoid(gap), 1e-6) {
			t.Errorf("gap %v: quick update's win chance = %v, want the refit's sigmoid(%v) = %v", gap, implied, gap, sigmoid(gap))
		}
	}
}

func TestUpdateRejectsInvalidOutcomes(t *testing.T) {
	bt := newTest(t)
	for _, o := range []Outcome{0, 4, 255} {
		if _, _, err := bt.Update(bt.Start(), bt.Start(), o); err == nil {
			t.Errorf("Update with outcome %+v succeeded, want an error", o)
		}
	}
}

// Many votes in a row: the numbers stay finite, the uncertainty keeps
// shrinking, and an image that always wins ends up on top.
func TestUpdateManyVotes(t *testing.T) {
	bt := newTest(t)
	champion := bt.Start()
	other := bt.Start()
	for i := range 1000 {
		before := champion.Uncertainty
		champion, other = update(t, bt, champion, other, LeftWins)
		if math.IsNaN(champion.Strength) || math.IsInf(champion.Strength, 0) || champion.Uncertainty >= before {
			t.Fatalf("after %d wins: %+v (uncertainty before %v)", i+1, champion, before)
		}
	}
	if champion.Strength <= other.Strength {
		t.Errorf("after 1000 wins the champion %+v is not above %+v", champion, other)
	}
}
