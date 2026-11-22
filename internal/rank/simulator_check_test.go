package rank

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// At fixed gaps, the simulated answers follow Rao & Kupper's chances.
func TestAnswerChances(t *testing.T) {
	const draws = 200_000 // chance moves each share by about 0.1%
	rng := rand.New(rand.NewPCG(502, 754))
	for _, a := range []answerer{tiesAndSkips, sidesOnly} {
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

// Over random pairs of typical images, tiesAndSkips gives its targets: 10%
// skips, 10% ties, and left and right about 40% each.
func TestSimulateVotesMix(t *testing.T) {
	const shown = 100_000
	truth := make([]float64, 1000)
	rng := rand.New(rand.NewPCG(502, 754))
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	counts := map[Winner]int{}
	votes := simulateVotes(rng, truth, shown, tiesAndSkips)
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
		{"tie", float64(counts[WinnerTie]) / shown, 0.1},
		{"left", float64(counts[WinnerLeft]) / shown, 0.4},
		{"right", float64(counts[WinnerRight]) / shown, 0.4},
	}
	for _, s := range shares {
		if !near(s.got, s.want, 0.01) {
			t.Errorf("%s share = %.3f, want about %.2f", s.name, s.got, s.want)
		}
	}
}

func TestSimulateOneVoteEach(t *testing.T) {
	truth := make([]float64, 2000)
	rng := rand.New(rand.NewPCG(502, 754))
	for i := range truth {
		truth[i] = rng.NormFloat64()
	}
	votes := simulateOneVoteEach(rng, truth, tiesAndSkips)
	if len(votes) != 500 {
		t.Errorf("%d votes, want 500 (half of 2000 images, two per vote)", len(votes))
	}
	seen := map[int64]int{}
	ties := 0
	for _, v := range votes {
		seen[v.Left]++
		seen[v.Right]++
		if v.Outcome == tie {
			ties++
		}
		if v.Left == v.Right || v.Left < 1 || v.Left > 2000 || v.Right < 1 || v.Right > 2000 {
			t.Fatalf("vote %+v: want two different images with ids 1 to 2000", v)
		}
	}
	if len(seen) != 1000 {
		t.Errorf("%d images voted, want exactly 1000", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("image %d is in %d votes, want 1", id, n)
		}
	}
	// With skips re-asked, ties are about 10/90 of the votes.
	if share := float64(ties) / float64(len(votes)); !near(share, 1.0/9, 0.04) {
		t.Errorf("tie share = %.3f, want about 0.11", share)
	}
}

func TestSpacedTruth(t *testing.T) {
	got := spacedTruth(5)
	want := []float64{-2, -1, 0, 1, 2}
	if !slices.Equal(got, want) {
		t.Errorf("spacedTruth(5) = %v, want %v", got, want)
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
		if got := correlation(a, c.b); !near(got, c.want, 1e-12) {
			t.Errorf("%s: correlation = %v, want %v", c.name, got, c.want)
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
		if got := slope(a, c.b); !near(got, c.want, 1e-12) {
			t.Errorf("%s: slope = %v, want %v", c.name, got, c.want)
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
	estimates := map[int64]Estimate{
		201: {Strength: 0, Uncertainty: 0.5},
		1:   {Strength: 0, Uncertainty: 1.5},
	}
	avgUncertainty, impliedRange, withinRange := accuracy(estimates, truth, truth)
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
	_, impliedRange, _ = accuracy(map[int64]Estimate{1: {Strength: 0, Uncertainty: 1}}, bell, bell)
	if !near(impliedRange, 0.6827, 0.005) {
		t.Errorf("uncertainty 1 on a bell curve of width 1 spans %v of the library, want about 0.68", impliedRange)
	}
}
