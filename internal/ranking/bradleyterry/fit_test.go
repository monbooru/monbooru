package bradleyterry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

func vote(left, right int64, o ranking.Outcome) ranking.Vote {
	return ranking.Vote{Left: left, Right: right, Outcome: o}
}

// fitStrengths runs the refit with the given starting uncertainty
// (math.Inf(1) = no starting assumption) and returns its states.
func fitStrengths(t *testing.T, votes []ranking.Vote, startUncertainty float64) map[int64]state {
	t.Helper()
	got, err := fitVotes(context.Background(), votes, 1/(startUncertainty*startUncertainty))
	if err != nil {
		t.Fatalf("fitVotes: %v", err)
	}
	return got
}

func strengthsOf(states map[int64]state) map[int64]float64 {
	out := make(map[int64]float64, len(states))
	for id, s := range states {
		out[id] = s.Strength
	}
	return out
}

func sameFit(t *testing.T, name string, got, want map[int64]state, tolerance float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d images, want %d", name, len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok || !near(g.Strength, w.Strength, tolerance) || !near(g.Uncertainty, w.Uncertainty, tolerance) {
			t.Errorf("%s: image %d = %+v, want %+v", name, id, g, w)
		}
	}
}

// metricGateVotes is MetricGate's Bradley–Terry (Extended) example as votes.
// Row i, column j is how often image i beat image j; images 1–4 are Alpha,
// Bravo, Charlie and Delta.
func metricGateVotes() []ranking.Vote {
	wins := [4][4]int{
		{0, 2, 3, 1},
		{1, 0, 2, 2},
		{0, 1, 0, 3},
		{2, 1, 0, 0},
	}
	var votes []ranking.Vote
	for i := range wins {
		for j := range wins[i] {
			for range wins[i][j] {
				votes = append(votes, vote(int64(i+1), int64(j+1), leftWins))
			}
		}
	}
	return votes
}

// mixedVotes has ties, upsets, repeat pairs, and both sides used.
func mixedVotes() []ranking.Vote {
	return []ranking.Vote{
		vote(1, 2, leftWins), vote(2, 1, rightWins), vote(1, 3, tie),
		vote(3, 2, leftWins), vote(2, 4, rightWins), vote(4, 1, leftWins),
		vote(5, 1, rightWins), vote(5, 4, tie), vote(3, 5, leftWins),
		vote(2, 3, tie), vote(4, 3, rightWins), vote(1, 5, leftWins),
	}
}

// balances is, per image, observed wins − expected wins at the given
// strengths. Observed counts a tie as half a win; expected is the sum of the
// image's win chances.
func balances(t *testing.T, votes []ranking.Vote, strengths map[int64]float64) map[int64]float64 {
	t.Helper()
	out := map[int64]float64{}
	for _, v := range votes {
		score, err := v.Outcome.LeftScore()
		if err != nil {
			t.Fatal(err)
		}
		p := sigmoid(strengths[v.Left] - strengths[v.Right])
		out[v.Left] += score - p
		out[v.Right] += (1 - score) - (1 - p)
	}
	return out
}

// MetricGate publishes strengths as π normalized to sum to 1. Ours are ln π,
// shifted so they average 0, which keeps the same ratios:
//
//	π = 0.393532, 0.275474, 0.194702, 0.136292
//	ln π − mean = 0.530181, 0.173512, −0.173511, −0.530182
//
// Their numbers are rounded to 6 digits, hence the tolerance.
func TestFitWithoutPriorMatchesMetricGate(t *testing.T) {
	votes := metricGateVotes()
	fit := fitStrengths(t, votes, math.Inf(1))
	want := map[int64]float64{1: 0.530181, 2: 0.173512, 3: -0.173511, 4: -0.530182}
	for id, w := range want {
		if got := fit[id].Strength; !near(got, w, 2e-5) {
			t.Errorf("image %d strength = %v, want %v", id, got, w)
		}
	}
	strengths := strengthsOf(fit)
	// MetricGate's win probabilities, to 4 digits.
	if got := sigmoid(strengths[1] - strengths[2]); !near(got, 0.5882, 5e-5) {
		t.Errorf("P(Alpha beats Bravo) = %v, want 0.5882", got)
	}
	if got := sigmoid(strengths[1] - strengths[4]); !near(got, 0.7428, 5e-5) {
		t.Errorf("P(Alpha beats Delta) = %v, want 0.7428", got)
	}
	// MetricGate's fit quality: log-likelihood and deviance (−2 × it).
	ll, err := logLikelihood(votes, strengths)
	if err != nil {
		t.Fatal(err)
	}
	if !near(ll, -11.6197, 5e-5) {
		t.Errorf("log-likelihood = %v, want -11.6197", ll)
	}
	if !near(-2*ll, 23.2393, 1e-4) {
		t.Errorf("deviance = %v, want 23.2393", -2*ll)
	}
}

// Without a starting assumption, the best fit is where every image's
// observed wins equal its expected wins (MetricGate's "observed vs.
// expected" check). This is the textbook likelihood equation.
func TestFitWithoutPriorObservedEqualsExpected(t *testing.T) {
	for name, votes := range map[string][]ranking.Vote{"MetricGate": metricGateVotes(), "mixed": mixedVotes()} {
		strengths := strengthsOf(fitStrengths(t, votes, math.Inf(1)))
		for id, b := range balances(t, votes, strengths) {
			if !near(b, 0, 1e-7) {
				t.Errorf("%s: image %d observed − expected = %v, want 0", name, id, b)
			}
		}
	}
}

// With the starting assumption, the votes' pull balances the assumption's
// pull back towards 0: observed − expected = strength / start_uncert².
func TestFitWithPriorBalancesVotesAndAssumption(t *testing.T) {
	sets := map[string][]ranking.Vote{
		"MetricGate": metricGateVotes(),
		"mixed":      mixedVotes(),
		"one vote":   {vote(1, 2, leftWins)},
		"undefeated": {vote(1, 2, leftWins), vote(1, 3, leftWins), vote(2, 3, tie)},
	}
	for name, votes := range sets {
		for _, startUncertainty := range []float64{0.5, 1, 2, 5} {
			strengths := strengthsOf(fitStrengths(t, votes, startUncertainty))
			for id, b := range balances(t, votes, strengths) {
				if want := strengths[id] / (startUncertainty * startUncertainty); !near(b, want, 1e-7) {
					t.Errorf("%s, start uncertainty %v: image %d observed − expected = %v, want strength/start_uncert² = %v",
						name, startUncertainty, id, b, want)
				}
			}
		}
	}
}

// Adding up the balance over all images: every vote's observed and expected
// wins both total 1, so the strengths must average exactly 0. The starting
// assumption fixes the average; nothing else needs to.
func TestFitWithPriorAveragesZero(t *testing.T) {
	for name, votes := range map[string][]ranking.Vote{"MetricGate": metricGateVotes(), "mixed": mixedVotes()} {
		fit := fitStrengths(t, votes, 2)
		total := 0.0
		for _, s := range fit {
			total += s.Strength
		}
		if mean := total / float64(len(fit)); !near(mean, 0, 1e-9) {
			t.Errorf("%s: mean strength = %v, want 0", name, mean)
		}
	}
}

// One vote between two new images, start_uncert 2. By symmetry the winner
// is at +t and the loser at −t, where the balance gives
// 1 − sigmoid(2t) = t/4, so t ≈ 0.740774 (solved by bisection in Python).
// Uncertainty = 1/√(sigmoid(2t) × (1 − sigmoid(2t)) + 1/4) ≈ 1.579369.
func TestFitOneVote(t *testing.T) {
	fit := fitStrengths(t, []ranking.Vote{vote(7, 9, rightWins)}, 2)
	sameFit(t, "one vote", fit, map[int64]state{
		9: {Strength: 0.7407743930623084, Uncertainty: 1.5793690933393363},
		7: {Strength: -0.7407743930623084, Uncertainty: 1.5793690933393363},
	}, 1e-8)
	// That's ranking ≈ 1629, RD ≈ 274.
	if got := math.Round(ranking.RankingFromStrength(fit[9].Strength)); got != 1629 {
		t.Errorf("winner's ranking = %v, want 1629", got)
	}
}

// The plan's "no prior" mode: as the starting assumption gets wider, the
// fit approaches plain textbook Bradley–Terry, getting closer every time.
func TestFitWideningPriorApproachesNoPrior(t *testing.T) {
	votes := metricGateVotes()
	noPrior := fitStrengths(t, votes, math.Inf(1))
	previous := math.Inf(1)
	for _, startUncertainty := range []float64{0.5, 1, 2, 10, 100, 1000} {
		fit := fitStrengths(t, votes, startUncertainty)
		largest := 0.0
		for id, s := range fit {
			largest = math.Max(largest, math.Abs(s.Strength-noPrior[id].Strength))
		}
		if largest >= previous {
			t.Errorf("start uncertainty %v: %v from the no-prior fit, not closer than before (%v)", startUncertainty, largest, previous)
		}
		previous = largest
	}
	if previous > 1e-5 {
		t.Errorf("start uncertainty 1000 is %v from the no-prior fit, want almost 0", previous)
	}
}

// The order votes come in, and which side each image was shown on, must not
// matter to the refit.
func TestFitIgnoresVoteOrderAndSides(t *testing.T) {
	votes := append(metricGateVotes(), mixedVotes()...)
	want := fitStrengths(t, votes, 2)

	rng := rand.New(rand.NewPCG(502, 754))
	for round := range 5 {
		shuffled := append([]ranking.Vote(nil), votes...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for i, v := range shuffled {
			if rng.IntN(2) == 0 {
				shuffled[i] = flipSides(v)
			}
		}
		sameFit(t, fmt.Sprintf("shuffle %d", round), fitStrengths(t, shuffled, 2), want, 1e-8)
	}
}

// flipSides shows the same vote the other way round.
func flipSides(v ranking.Vote) ranking.Vote {
	switch v.Outcome.Winner {
	case ranking.WinnerLeft:
		v.Outcome.Winner = ranking.WinnerRight
	case ranking.WinnerRight:
		v.Outcome.Winner = ranking.WinnerLeft
	}
	v.Left, v.Right = v.Right, v.Left
	return v
}

// A tie is half a win and half a loss, so two ties carry exactly what one
// win and one loss carry.
func TestFitTiesAreHalfWins(t *testing.T) {
	cases := []struct {
		name       string
		ties, wins []ranking.Vote
	}{
		{"two ties = a win and a loss",
			[]ranking.Vote{vote(1, 2, tie), vote(1, 2, tie)},
			[]ranking.Vote{vote(1, 2, leftWins), vote(1, 2, rightWins)}},
		{"a win and two ties = two wins and a loss",
			[]ranking.Vote{vote(1, 2, leftWins), vote(1, 2, tie), vote(2, 1, tie)},
			[]ranking.Vote{vote(1, 2, leftWins), vote(1, 2, leftWins), vote(1, 2, rightWins)}},
	}
	for _, c := range cases {
		sameFit(t, c.name, fitStrengths(t, c.ties, 2), fitStrengths(t, c.wins, 2), 1e-9)
	}
	// Only ties: everyone is average.
	for id, s := range fitStrengths(t, []ranking.Vote{vote(1, 2, tie), vote(2, 3, tie), vote(3, 1, tie)}, 2) {
		if !near(s.Strength, 0, 1e-12) {
			t.Errorf("only ties: image %d strength = %v, want 0", id, s.Strength)
		}
	}
}

// Two groups never compared with each other are fitted independently, and
// each sits around 0 on its own (the starting assumption's job).
func TestFitSeparateGroups(t *testing.T) {
	groupA := metricGateVotes()
	groupB := []ranking.Vote{vote(11, 12, leftWins), vote(12, 13, leftWins), vote(11, 13, tie)}
	together := fitStrengths(t, append(append([]ranking.Vote{}, groupA...), groupB...), 2)
	want := fitStrengths(t, groupA, 2)
	for id, s := range fitStrengths(t, groupB, 2) {
		want[id] = s
	}
	sameFit(t, "groups fitted together", together, want, 1e-8)
}

func TestConnectedGroups(t *testing.T) {
	// Positions 0–1–2 are linked in a chain, 3–4 are a pair, 5 is linked to
	// 0 only through 2 (the chain is followed, not just direct opponents).
	matches := [][]match{
		{{opponent: 1}},
		{{opponent: 0}, {opponent: 2}},
		{{opponent: 1}, {opponent: 5}},
		{{opponent: 4}},
		{{opponent: 3}},
		{{opponent: 2}},
	}
	group, count := connectedGroups(matches)
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	same := func(a, b int) bool { return group[a] == group[b] }
	if !same(0, 1) || !same(0, 2) || !same(0, 5) || !same(3, 4) || same(0, 3) {
		t.Errorf("groups = %v, want {0, 1, 2, 5} and {3, 4}", group)
	}
	for i, g := range group {
		if g < 0 || g >= count {
			t.Errorf("group[%d] = %d, want 0 to %d", i, g, count-1)
		}
	}
}

// Each linked group averages 0 on its own, as shiftToAverageZero assumes.
func TestFitEachGroupAveragesZero(t *testing.T) {
	groupA := []ranking.Vote{vote(1, 2, leftWins), vote(1, 2, leftWins), vote(2, 3, leftWins)}
	groupB := []ranking.Vote{vote(11, 12, leftWins), vote(13, 12, tie)}
	fit := fitStrengths(t, append(groupA, groupB...), 2)
	for name, ids := range map[string][]int64{"A": {1, 2, 3}, "B": {11, 12, 13}} {
		total := 0.0
		for _, id := range ids {
			total += fit[id].Strength
		}
		if !near(total, 0, 1e-9) {
			t.Errorf("group %s strengths add up to %v, want 0", name, total)
		}
	}
}

// An image that never lost: textbook Bradley–Terry has no answer (its
// strength runs off to infinity), so the fit must fail. With the starting
// assumption it gets a finite strength, above the others.
func TestFitUndefeatedImage(t *testing.T) {
	votes := []ranking.Vote{vote(1, 2, leftWins), vote(3, 1, rightWins), vote(2, 3, leftWins), vote(3, 2, leftWins)}
	if _, err := fitVotes(context.Background(), votes, 0); err == nil {
		t.Errorf("fit without a starting assumption succeeded, want an error")
	}
	fit := fitStrengths(t, votes, 2)
	if s := fit[1].Strength; math.IsInf(s, 0) || s <= fit[2].Strength || s <= fit[3].Strength {
		t.Errorf("undefeated image = %v, want finite and above %v and %v", s, fit[2].Strength, fit[3].Strength)
	}
}

// After a refit, uncertainty = 1/√certainty, where certainty adds up
// win_chance × (1 − win_chance) for each of the image's votes, plus
// 1/start_uncert² from the starting assumption.
func TestFitUncertainty(t *testing.T) {
	votes := mixedVotes()
	fit := fitStrengths(t, votes, 2)
	certainty := map[int64]float64{}
	for _, v := range votes {
		p := sigmoid(fit[v.Left].Strength - fit[v.Right].Strength)
		certainty[v.Left] += p * (1 - p)
		certainty[v.Right] += p * (1 - p)
	}
	for id, c := range certainty {
		if want := 1 / math.Sqrt(c+0.25); !near(fit[id].Uncertainty, want, 1e-9) {
			t.Errorf("image %d uncertainty = %v, want %v", id, fit[id].Uncertainty, want)
		}
		if fit[id].Uncertainty >= 2 {
			t.Errorf("image %d uncertainty = %v, want less than the starting 2", id, fit[id].Uncertainty)
		}
	}
	// More votes between the same images: more certain.
	doubled := fitStrengths(t, append(append([]ranking.Vote{}, votes...), votes...), 2)
	for id := range certainty {
		if doubled[id].Uncertainty >= fit[id].Uncertainty {
			t.Errorf("image %d: twice the votes gave uncertainty %v, not less than %v", id, doubled[id].Uncertainty, fit[id].Uncertainty)
		}
	}
}

func TestFitReturnsOnlyVotedImages(t *testing.T) {
	bt := newTest(t)
	got, err := bt.Fit(context.Background(), []ranking.Vote{vote(5, 8, leftWins), vote(8, 13, tie)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[5] == nil || got[8] == nil || got[13] == nil {
		t.Errorf("Fit returned %v, want exactly images 5, 8 and 13", got)
	}
	// What Fit returns is a normal state: Shown, Encode and Update take it.
	if _, err := bt.Encode(got[5]); err != nil {
		t.Errorf("Encode(Fit result): %v", err)
	}
	if _, _, err := bt.Update(got[5], got[8], leftWins, time.Time{}); err != nil {
		t.Errorf("Update(Fit results): %v", err)
	}

	empty, err := bt.Fit(context.Background(), nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("Fit(no votes) = %v, %v; want an empty map and no error", empty, err)
	}
}

// Fit uses the settings' starting uncertainty.
func TestFitUsesSettings(t *testing.T) {
	for _, startUncertainty := range []float64{1, 2, 3} {
		bt, err := New(Settings{StartUncertainty: startUncertainty, PerfVariance: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		got, err := bt.Fit(context.Background(), mixedVotes())
		if err != nil {
			t.Fatal(err)
		}
		want := fitStrengths(t, mixedVotes(), startUncertainty)
		for id, s := range got {
			if s.(state) != want[id] {
				t.Errorf("start uncertainty %v, image %d: Fit = %+v, want %+v", startUncertainty, id, s, want[id])
			}
		}
	}
}

func TestFitRejectsBadVotes(t *testing.T) {
	bt := newTest(t)
	cases := map[string][]ranking.Vote{
		"image against itself": {vote(1, 2, leftWins), vote(3, 3, leftWins)},
		"no winner":            {vote(1, 2, ranking.Outcome{})},
		"tie with a degree":    {vote(1, 2, ranking.Outcome{Winner: ranking.WinnerTie, Degree: ranking.DegreeSlight})},
	}
	for name, votes := range cases {
		if _, err := bt.Fit(context.Background(), votes); err == nil {
			t.Errorf("%s: Fit succeeded, want an error", name)
		}
	}
}

func TestFitStopsWhenCancelled(t *testing.T) {
	bt := newTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bt.Fit(ctx, metricGateVotes()); !errors.Is(err, context.Canceled) {
		t.Errorf("Fit with a cancelled context: err = %v, want context.Canceled", err)
	}
}

func TestLogLikelihood(t *testing.T) {
	cases := []struct {
		name      string
		votes     []ranking.Vote
		strengths map[int64]float64
		want      float64
	}{
		{"coin flip", []ranking.Vote{vote(1, 2, leftWins)}, nil, math.Log(0.5)},
		{"tie between equals", []ranking.Vote{vote(1, 2, tie)}, nil, math.Log(0.5)},
		{"expected win", []ranking.Vote{vote(1, 2, leftWins)}, map[int64]float64{1: 1}, math.Log(sigmoid(1))},
		{"upset", []ranking.Vote{vote(1, 2, rightWins)}, map[int64]float64{1: 1}, math.Log(sigmoid(-1))},
		// A tie is half a win and half a loss: log √(p × (1 − p)).
		{"tie", []ranking.Vote{vote(1, 2, tie)}, map[int64]float64{1: 1}, 0.5*math.Log(sigmoid(1)) + 0.5*math.Log(sigmoid(-1))},
		// Huge gaps stay finite.
		{"huge upset", []ranking.Vote{vote(1, 2, rightWins)}, map[int64]float64{1: 1000}, -1000},
		{"several votes add up", []ranking.Vote{vote(1, 2, leftWins), vote(2, 1, leftWins)}, nil, 2 * math.Log(0.5)},
	}
	for _, c := range cases {
		got, err := logLikelihood(c.votes, c.strengths)
		if err != nil || !near(got, c.want, 1e-9) {
			t.Errorf("%s: logLikelihood = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := logLikelihood([]ranking.Vote{vote(1, 2, ranking.Outcome{})}, nil); err == nil {
		t.Errorf("logLikelihood with an invalid outcome succeeded, want an error")
	}
}

// The refit claims to find the strengths that explain the votes best: the
// highest log-likelihood (see logLikelihood). This checks that claim
// directly, the way you'd check you're on top of a hill by stepping off it
// in each direction: move one image's fitted strength up or down a little,
// and the votes must be explained worse every time. If the fit had stopped
// early, or settled on the wrong point, some nudge would do better. It's run
// without the starting assumption, so "best" means the votes alone.
func TestFitWithoutPriorMaximizesLikelihood(t *testing.T) {
	votes := mixedVotes()
	best := strengthsOf(fitStrengths(t, votes, math.Inf(1)))
	bestLL, err := logLikelihood(votes, best)
	if err != nil {
		t.Fatal(err)
	}
	for id := range best {
		for _, nudge := range []float64{-0.1, -0.01, 0.01, 0.1} {
			moved := make(map[int64]float64, len(best))
			for k, v := range best {
				moved[k] = v
			}
			moved[id] += nudge
			if ll, _ := logLikelihood(votes, moved); ll >= bestLL {
				t.Errorf("moving image %d by %v gave log-likelihood %v, not less than the fit's %v", id, nudge, ll, bestLL)
			}
		}
	}
}
