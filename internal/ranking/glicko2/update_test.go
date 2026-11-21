package glicko2

import (
	"math"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

// One vote between two brand-new images, checked against the Python script
// written from the paper: the winner shows 1662.31 with RD 290.32, the loser
// the mirror image.
func TestNewBeatsNew(t *testing.T) {
	alg := newTest(t)
	l, r := update(t, alg, alg.Start().(state), alg.Start().(state), leftWins, monday.Add(time.Hour))
	winner, loser := alg.Shown(l), alg.Shown(r)
	if got := ranking.RankingFromStrength(winner.Strength); !near(got, 1662.310895033017, 1e-6) {
		t.Errorf("winner = %v, want 1662.310895", got)
	}
	if got := ranking.RDFromUncertainty(winner.Uncertainty); !near(got, 290.3189620179176, 1e-6) {
		t.Errorf("winner's RD = %v, want 290.318962", got)
	}
	if !near(loser.Strength, -winner.Strength, 1e-12) || !near(loser.Uncertainty, winner.Uncertainty, 1e-9) {
		t.Errorf("loser = %+v, want the mirror of %+v", loser, winner)
	}
}

// A tie between equals: nothing to learn about who's better, so the rating
// stays, but the uncertainty still shrinks (Python: φ' = 1.6712102733).
func TestTieBetweenNewImages(t *testing.T) {
	alg := newTest(t)
	l, _ := update(t, alg, alg.Start().(state), alg.Start().(state), tie, monday.Add(time.Hour))
	got := alg.Shown(l)
	if got.Strength != 0 || !near(got.Uncertainty, 1.6712102732775158, 1e-9) {
		t.Errorf("after a tie = %+v, want strength 0, uncertainty 1.6712102733", got)
	}
}

// sampleStates covers new, settled, strong, weak and lopsided images, all at
// the start of the period beginning at monday.
var sampleStates = []state{
	rated(1500, 350, 0.06),
	rated(1500, 50, 0.06),
	rated(1760, 174, 0.06),
	rated(1150, 87, 0.09),
	rated(2200, 30, 0.06),
	rated(1400, 500, 0.06),
}

var allOutcomes = []ranking.Outcome{leftWins, tie, rightWins}

// Showing the same two images the other way round, with the pick on the
// same image, must give the same result.
func TestUpdateSidesAreSymmetric(t *testing.T) {
	alg := newTest(t)
	mirror := map[ranking.Outcome]ranking.Outcome{leftWins: rightWins, tie: tie, rightWins: leftWins}
	at := monday.Add(time.Hour)
	for _, a := range sampleStates {
		for _, b := range sampleStates {
			for _, o := range allOutcomes {
				l, r := update(t, alg, a, b, o, at)
				r2, l2 := update(t, alg, b, a, mirror[o], at)
				checkState(t, "left image", l2, l)
				checkState(t, "right image", r2, r)
			}
		}
	}
}

// The winner's shown rating rises and the loser's falls; a tie pulls the
// two towards each other.
func TestUpdateDirection(t *testing.T) {
	alg := newTest(t)
	at := monday.Add(time.Hour)
	for _, a := range sampleStates {
		for _, b := range sampleStates {
			l, r := update(t, alg, a, b, leftWins, at)
			if alg.Shown(l).Strength <= a.Strength || alg.Shown(r).Strength >= b.Strength {
				t.Errorf("%+v beat %+v: shown %v and %v; the winner must rise and the loser fall", a, b, alg.Shown(l), alg.Shown(r))
			}
			if a.Strength > b.Strength {
				l, r = update(t, alg, a, b, tie, at)
				if alg.Shown(l).Strength >= a.Strength || alg.Shown(r).Strength <= b.Strength {
					t.Errorf("tie %+v vs %+v: the higher must fall and the lower rise, got %v, %v", a, b, alg.Shown(l), alg.Shown(r))
				}
			}
		}
	}
}

// Games in a rating period all count against the opponents' ratings from
// the start of the period, so the order of a period's votes doesn't matter
// at all, unlike Bradley–Terry's quick update.
func TestOrderWithinAPeriodDoesNotMatter(t *testing.T) {
	alg := newTest(t)
	votes := []struct {
		left, right int
		outcome     ranking.Outcome
	}{
		{0, 1, leftWins}, {1, 2, leftWins}, {2, 0, tie}, {3, 0, rightWins}, {1, 3, rightWins},
	}
	play := func(order []int) []state {
		images := []state{sampleStates[0], sampleStates[1], sampleStates[2], sampleStates[3]}
		for n, k := range order {
			v := votes[k]
			images[v.left], images[v.right] = update(t, alg, images[v.left], images[v.right], v.outcome, monday.Add(time.Duration(n+1)*time.Hour))
		}
		return images
	}
	want := play([]int{0, 1, 2, 3, 4})
	for _, order := range [][]int{{4, 3, 2, 1, 0}, {2, 0, 4, 1, 3}} {
		for i, got := range play(order) {
			checkState(t, "image", got, want[i])
		}
	}
}

// A vote in a later period first closes each image's old period (and grows
// the uncertainty for the empty weeks in between), then counts in the new one.
func TestVoteInALaterPeriodClosesTheOldOne(t *testing.T) {
	alg := newTest(t)
	player := paperPlayer(t)
	later := monday.AddDate(0, 0, 14).Add(time.Hour) // two weeks on
	got, _ := update(t, alg, player, rated(1500, 350, 0.06), leftWins, later)

	closed := alg.advance(player, later)
	if !closed.PeriodStart.Equal(monday.AddDate(0, 0, 14)) || closed.PeriodGames != 0 {
		t.Fatalf("advance = %+v, want an empty period starting two Mondays on", closed)
	}
	if got.Strength != closed.Strength || got.Uncertainty != closed.Uncertainty || got.Volatility != closed.Volatility {
		t.Errorf("starting values = %+v, want the closed period's %+v", got, closed)
	}
	if got.PeriodGames != 1 || !got.PeriodStart.Equal(closed.PeriodStart) {
		t.Errorf("after the vote = %+v, want 1 game in the new period", got)
	}
}

// If the clock goes back, a vote that seems to come from an earlier period
// still counts, in the image's current one.
func TestVoteFromAnEarlierTimeCountsInTheCurrentPeriod(t *testing.T) {
	alg := newTest(t)
	a, b := rated(1500, 200, 0.06), rated(1600, 200, 0.06)
	want, _ := update(t, alg, a, b, leftWins, monday.Add(time.Hour))
	got, _ := update(t, alg, a, b, leftWins, monday.Add(-time.Hour))
	checkState(t, "a vote from before the period", got, want)
}

// What Shown gives during a period is exactly what the period's close will
// make the new starting values.
func TestShownDuringAPeriodMatchesItsClose(t *testing.T) {
	alg := newTest(t)
	at := monday.Add(time.Hour)
	for _, a := range sampleStates {
		for _, b := range sampleStates {
			for _, o := range allOutcomes {
				l, _ := update(t, alg, a, b, o, at)
				shown := alg.Shown(l)
				closed := alg.advance(l, monday.AddDate(0, 0, 7))
				if !near(shown.Strength, closed.Strength, 1e-12) || !near(shown.Uncertainty, closed.Uncertainty, 1e-12) {
					t.Errorf("%+v vs %+v, %+v: shown %+v during the period, but the close gives %+v", a, b, o, shown, closed)
				}
			}
		}
	}
	// With no games yet, Shown is just the starting values.
	if got := alg.Shown(sampleStates[2]); got.Strength != sampleStates[2].Strength || got.Uncertainty != sampleStates[2].Uncertainty {
		t.Errorf("Shown with no games = %+v, want the starting values of %+v", got, sampleStates[2])
	}
}

// Every game is information, so a period with games ends more certain than
// a period without would: below Step 6's φ* = √(φ² + σ'²), however the games
// went. (σ' is the new volatility, which an upset can raise.)
func TestGamesShrinkUncertainty(t *testing.T) {
	alg := newTest(t)
	at := monday.Add(time.Hour)
	for _, a := range sampleStates {
		for _, b := range sampleStates {
			for _, o := range allOutcomes {
				l, _ := update(t, alg, a, b, o, at)
				newVolatility := alg.advance(l, monday.AddDate(0, 0, 7)).Volatility
				phiStar := math.Sqrt(a.Uncertainty*a.Uncertainty + newVolatility*newVolatility)
				if shown := alg.Shown(l); !(shown.Uncertainty < phiStar) {
					t.Errorf("%+v vs %+v, %+v: uncertainty %v, want below φ* = %v", a, b, o, shown.Uncertainty, phiStar)
				}
			}
		}
	}
}

func TestUpdateIgnoresDegree(t *testing.T) {
	alg := newTest(t)
	a, b := sampleStates[2], sampleStates[3]
	plainL, plainR := update(t, alg, a, b, leftWins, monday.Add(time.Hour))
	for _, d := range []ranking.Degree{ranking.DegreeSlight, ranking.DegreeModerate, ranking.DegreeStrong} {
		l, r := update(t, alg, a, b, ranking.Outcome{Winner: ranking.WinnerLeft, Degree: d}, monday.Add(time.Hour))
		if !sameState(l, plainL) || !sameState(r, plainR) {
			t.Errorf("degree %d changed the update", d)
		}
	}
}

func TestUpdateRejectsBadVotes(t *testing.T) {
	alg := newTest(t)
	for _, o := range []ranking.Outcome{{}, {Winner: 4}, {Winner: ranking.WinnerTie, Degree: ranking.DegreeStrong}} {
		if _, _, err := alg.Update(alg.Start(), alg.Start(), o, monday); err == nil {
			t.Errorf("Update with outcome %+v succeeded, want an error", o)
		}
	}
	// Glicko-2 needs to know when a vote was made.
	if _, _, err := alg.Update(alg.Start(), alg.Start(), leftWins, time.Time{}); err == nil {
		t.Errorf("Update with no time succeeded, want an error")
	}
}

// A game against an absurdly far-apart opponent carries no information
// (its expected score rounds to exactly 1). The numbers must stay finite.
func TestAbsurdGapStaysFinite(t *testing.T) {
	alg := newTest(t)
	far := rated(1500, 30, 0.06)
	far.Strength = 1000
	l, r := update(t, alg, rated(1500, 30, 0.06), far, rightWins, monday.Add(time.Hour))
	for _, s := range []state{l, r, alg.advance(l, monday.AddDate(0, 0, 7))} {
		e := alg.Shown(s)
		if math.IsNaN(e.Strength) || math.IsInf(e.Strength, 0) || !(e.Uncertainty > 0) || math.IsInf(e.Uncertainty, 0) {
			t.Errorf("state %+v shows %+v, want finite numbers", s, e)
		}
	}
}

// Many periods in a row: the numbers stay finite, and an image that always
// wins ends up on top.
func TestManyPeriods(t *testing.T) {
	alg := newTest(t)
	champion, other := alg.Start().(state), alg.Start().(state)
	for week := range 200 {
		at := monday.AddDate(0, 0, 7*week).Add(time.Hour)
		for range 5 {
			champion, other = update(t, alg, champion, other, leftWins, at)
		}
		if e := alg.Shown(champion); math.IsNaN(e.Strength) || math.IsInf(e.Strength, 0) || !(e.Uncertainty > 0) {
			t.Fatalf("week %d: champion shows %+v", week, e)
		}
	}
	if alg.Shown(champion).Strength <= alg.Shown(other).Strength {
		t.Errorf("after 1000 wins the champion %+v is not above %+v", alg.Shown(champion), alg.Shown(other))
	}
}
