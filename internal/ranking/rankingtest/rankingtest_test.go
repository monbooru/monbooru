package rankingtest

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

// At fixed gaps, the simulated answers follow Rao & Kupper's chances.
func TestAnswerChances(t *testing.T) {
	const draws = 200_000 // chance moves each share by about 0.1%
	rng := rand.New(rand.NewPCG(502, 754))
	for _, a := range []Answerer{TiesAndSkips, SidesOnly} {
		for _, gap := range []float64{-1, 0, 1, 2, 4} {
			counts := map[string]float64{}
			for range draws {
				o, skipped := a.Answer(rng, gap)
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
			undecided := sigmoid(gap+a.Threshold) - sigmoid(gap-a.Threshold)
			want := map[string]float64{
				"left":  sigmoid(gap - a.Threshold),
				"right": sigmoid(-gap - a.Threshold),
				"tie":   undecided / 2,
				"skip":  undecided / 2,
			}
			for name, w := range want {
				if got := counts[name] / draws; !near(got, w, 0.005) {
					t.Errorf("threshold %v, gap %v: %s share = %.4f, want %.4f", a.Threshold, gap, name, got, w)
				}
			}
		}
	}
}

// Over random pairs of typical images, TiesAndSkips gives its targets: 10%
// skips, 10% ties, and left and right about 40% each.
func TestSimulateVotesMix(t *testing.T) {
	const shown = 100_000
	truth := make([]float64, 1000)
	rng := rand.New(rand.NewPCG(502, 754))
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	counts := map[ranking.Winner]int{}
	votes := SimulateVotes(rng, truth, shown, TiesAndSkips)
	for _, v := range votes {
		counts[v.Outcome.Winner]++
		if v.Left == v.Right || v.Left < 1 || v.Left > 1000 || v.Right < 1 || v.Right > 1000 {
			t.Fatalf("vote %+v: want two different images with ids 1 to 1000", v)
		}
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

func TestSpreadOverTime(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	votes := []ranking.Vote{{Left: 1, Right: 2}, {Left: 2, Right: 3}, {Left: 3, Right: 1}, {Left: 1, Right: 3}}
	got := SpreadOverTime(votes, start, 4*time.Hour)
	for i, v := range got {
		if want := start.Add(time.Duration(i) * time.Hour); !v.At.Equal(want) || v.Left != votes[i].Left || v.Right != votes[i].Right {
			t.Errorf("vote %d = %+v, want images %d and %d at %v", i, v, votes[i].Left, votes[i].Right, want)
		}
	}
	if !votes[1].At.IsZero() {
		t.Errorf("SpreadOverTime changed the votes it was given")
	}
}

func TestSpacedTruth(t *testing.T) {
	got := SpacedTruth(5)
	want := []float64{-2, -1, 0, 1, 2}
	if !slices.Equal(got, want) {
		t.Errorf("SpacedTruth(5) = %v, want %v", got, want)
	}
}

func TestCorrelation(t *testing.T) {
	a := []float64{1, 2, 3, 4}
	cases := []struct {
		name string
		b    []float64
		want float64
	}{
		{"rising line", []float64{2, 4, 6, 8}, 1},
		{"rising line, shifted", []float64{-9, -8, -7, -6}, 1},
		{"falling line", []float64{8, 6, 4, 2}, -1},
		{"unrelated", []float64{1, -1, -1, 1}, 0},
	}
	for _, c := range cases {
		if got := Correlation(a, c.b); !near(got, c.want, 1e-12) {
			t.Errorf("%s: Correlation = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSlope(t *testing.T) {
	a := []float64{-2, 0, 1, 5}
	cases := []struct {
		name string
		b    []float64
		want float64
	}{
		{"b = 2a + 1", []float64{-3, 1, 3, 11}, 2},
		{"b = −a/2", []float64{1, 0, -0.5, -2.5}, -0.5},
		{"b flat", []float64{7, 7, 7, 7}, 0},
	}
	for _, c := range cases {
		if got := Slope(a, c.b); !near(got, c.want, 1e-12) {
			t.Errorf("%s: Slope = %v, want %v", c.name, got, c.want)
		}
	}
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
	estimates := map[int64]ranking.Estimate{
		201: {Strength: 0, Uncertainty: 0.5},
		1:   {Strength: 0, Uncertainty: 1.5},
	}
	avgUncertainty, impliedRange, withinRange := Accuracy(estimates, truth, truth)
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
	rng := rand.New(rand.NewPCG(502, 754))
	bell := make([]float64, 200_000)
	for i := range bell {
		bell[i] = rng.NormFloat64()
	}
	slices.Sort(bell)
	_, impliedRange, _ = Accuracy(map[int64]ranking.Estimate{1: {Strength: 0, Uncertainty: 1}}, bell, bell)
	if !near(impliedRange, 0.6827, 0.005) {
		t.Errorf("uncertainty 1 on a bell curve of width 1 spans %v of the library, want about 0.68", impliedRange)
	}
}
