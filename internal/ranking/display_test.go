package ranking

import (
	"math"
	"testing"
)

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

// Glickman's "Example of the Glicko-2 system" rounds the scale to 173.7178.
func TestGlickoScaleMatchesThePaper(t *testing.T) {
	if !near(glickoScale, 173.7178, 1e-4) {
		t.Errorf("glickoScale = %v, want 173.7178", glickoScale)
	}
}

func TestRankingFromStrength(t *testing.T) {
	cases := []struct {
		strength, want float64
	}{
		{0, 1500},
		{1, 1500 + 400/math.Ln10},
		{-1, 1500 - 400/math.Ln10},
		// 400 points is 10-to-1 odds: a strength gap of ln 10.
		{math.Ln10, 1900},
		{-math.Ln10, 1100},
	}
	for _, c := range cases {
		if got := RankingFromStrength(c.strength); !near(got, c.want, 1e-9) {
			t.Errorf("RankingFromStrength(%v) = %v, want %v", c.strength, got, c.want)
		}
	}
}

func TestRDFromUncertainty(t *testing.T) {
	cases := []struct {
		uncertainty, want float64
	}{
		{0, 0},
		{1, 400 / math.Ln10},
		// Bradley–Terry's start_uncert of 2 sits just under Glicko-2's RD 350.
		{2, 800 / math.Ln10},
	}
	for _, c := range cases {
		if got := RDFromUncertainty(c.uncertainty); !near(got, c.want, 1e-9) {
			t.Errorf("RDFromUncertainty(%v) = %v, want %v", c.uncertainty, got, c.want)
		}
	}
	// Glicko-2's starting RD of 350 is an uncertainty of about 2.0148.
	if got := UncertaintyFromRD(350); !near(got, 2.0148, 1e-4) {
		t.Errorf("UncertaintyFromRD(350) = %v, want about 2.0148", got)
	}
}

// Filters convert a typed ranking back to a strength, so the two directions
// must agree everywhere, including far below and above 1500.
func TestDisplayRoundTrips(t *testing.T) {
	for _, strength := range []float64{-10, -3.5, -1, -0.001, 0, 0.001, 1, 3.5, 10} {
		if got := StrengthFromRanking(RankingFromStrength(strength)); !near(got, strength, 1e-12) {
			t.Errorf("StrengthFromRanking(RankingFromStrength(%v)) = %v", strength, got)
		}
	}
	for _, uncertainty := range []float64{0, 0.25, 1, 2, 2.0148, 5} {
		if got := UncertaintyFromRD(RDFromUncertainty(uncertainty)); !near(got, uncertainty, 1e-12) {
			t.Errorf("UncertaintyFromRD(RDFromUncertainty(%v)) = %v", uncertainty, got)
		}
	}
}

// The sort and filters rely on a higher strength always showing a higher
// ranking, so an index on strength can serve a filter on ranking.
func TestRankingRisesWithStrength(t *testing.T) {
	previous := math.Inf(-1)
	for strength := -10.0; strength <= 10; strength += 0.5 {
		got := RankingFromStrength(strength)
		if got <= previous {
			t.Fatalf("RankingFromStrength(%v) = %v, not above the previous %v", strength, got, previous)
		}
		previous = got
	}
}
