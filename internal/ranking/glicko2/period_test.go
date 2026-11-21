package glicko2

import (
	"math"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

// monday is the start of a weekly rating period: Monday 16 November 2026,
// 00:00 UTC.
var monday = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

// rated is an image in the paper's own units: rating, RD and volatility, at
// the start of the period beginning at monday.
func rated(rating, rd, volatility float64) state {
	return state{
		Strength:    ranking.StrengthFromRanking(rating),
		Uncertainty: ranking.UncertaintyFromRD(rd),
		Volatility:  volatility,
		PeriodStart: monday,
	}
}

// paperPlayer plays the games of Glickman's worked example ("Example of the
// Glicko-2 system", 2022): a player rated 1500, RD 200, volatility 0.06,
// beats a 1400 (RD 30), then loses to a 1550 (RD 100) and a 1700 (RD 300),
// all in one rating period, with τ = 0.5.
func paperPlayer(t *testing.T) state {
	t.Helper()
	alg := newTest(t)
	player := rated(1500, 200, 0.06)
	games := []struct {
		opponent state
		outcome  ranking.Outcome
	}{
		{rated(1400, 30, 0.06), leftWins},
		{rated(1550, 100, 0.06), rightWins},
		{rated(1700, 300, 0.06), rightWins},
	}
	for i, g := range games {
		player, _ = update(t, alg, player, g.opponent, g.outcome, monday.Add(time.Duration(i+1)*time.Hour))
	}
	return player
}

// The paper's table: each opponent's g(φj) and E(µ, µj, φj).
func TestOpponentWeightAndExpectedScoreMatchThePaper(t *testing.T) {
	cases := []struct {
		rating, rd       float64
		weight, expected float64 // the paper's, to 4 and 3 digits
	}{
		{1400, 30, 0.9955, 0.639},
		{1550, 100, 0.9531, 0.432},
		{1700, 300, 0.7242, 0.303},
	}
	for _, c := range cases {
		w := opponentWeight(ranking.UncertaintyFromRD(c.rd))
		if !near(w, c.weight, 5e-5) {
			t.Errorf("g(RD %v) = %v, want %v", c.rd, w, c.weight)
		}
		if e := expectedScore(0, ranking.StrengthFromRanking(c.rating), w); !near(e, c.expected, 5e-4) {
			t.Errorf("E(1500 vs %v) = %v, want %v", c.rating, e, c.expected)
		}
	}
	// g of a perfectly known opponent is 1: the game counts fully.
	if w := opponentWeight(0); w != 1 {
		t.Errorf("g(0) = %v, want 1", w)
	}
}

func TestPaperExample(t *testing.T) {
	player := paperPlayer(t)
	if player.PeriodGames != 3 {
		t.Fatalf("PeriodGames = %d, want 3", player.PeriodGames)
	}
	// Steps 3 and 4. The paper rounds E to 3 digits along the way, so its v
	// and Δ are only good to about 0.001; the exact values are from a
	// separate Python script written from the paper.
	v := 1 / player.PeriodInformation
	delta := v * player.PeriodSurprise
	if !near(v, 1.7785, 1e-3) || !near(v, 1.7789771178226887, 1e-9) {
		t.Errorf("v = %v, want 1.7785 (paper), 1.7789771178 (exact)", v)
	}
	if !near(delta, -0.4834, 1e-3) || !near(delta, -0.4839332587277324, 1e-9) {
		t.Errorf("Δ = %v, want -0.4834 (paper), -0.4839332587 (exact)", delta)
	}

	// Steps 5 to 8, as shown during the period.
	alg := newTest(t)
	shown := alg.Shown(player)
	if !near(shown.Strength, -0.2069, 1e-4) || !near(shown.Strength, -0.20694097368427727, 1e-9) {
		t.Errorf("µ' = %v, want -0.2069 (paper), -0.2069409737 (exact)", shown.Strength)
	}
	if !near(shown.Uncertainty, 0.8722, 1e-4) || !near(shown.Uncertainty, 0.8721992118223543, 1e-9) {
		t.Errorf("φ' = %v, want 0.8722 (paper), 0.8721992118 (exact)", shown.Uncertainty)
	}
	// The paper's 1464.06 comes from its rounded µ' (−0.2069 × 173.7178);
	// unrounded it's 1464.0507.
	if r := ranking.RankingFromStrength(shown.Strength); !near(r, 1464.06, 0.01) || !near(r, 1464.0506708196929, 1e-6) {
		t.Errorf("r' = %v, want 1464.06 (paper), 1464.0506708 (exact)", r)
	}
	if rd := ranking.RDFromUncertainty(shown.Uncertainty); !near(rd, 151.52, 0.005) || !near(rd, 151.5165219259256, 1e-6) {
		t.Errorf("RD' = %v, want 151.52 (paper), 151.5165219 (exact)", rd)
	}

	// Closing the period makes those the next period's starting values,
	// with the new volatility (the paper's 0.05999 is cut off, not rounded).
	next := alg.advance(player, monday.AddDate(0, 0, 7))
	want := state{
		Strength:    -0.20694097368427727,
		Uncertainty: 0.8721992118223543,
		Volatility:  0.05999598428664987,
		PeriodStart: monday.AddDate(0, 0, 7),
	}
	checkState(t, "after the period", next, want)
	if !near(next.Volatility, 0.05999, 1e-5) {
		t.Errorf("σ' = %v, want 0.05999 (paper)", next.Volatility)
	}
}

// Step 5's function, at the starting values of the paper's table (computed
// from its rounded φ, v and Δ): fA = −0.00053567 and fB = 1.999675.
func TestVolatilityObjectiveMatchesThePaper(t *testing.T) {
	phi, v, delta, tau := 1.1513, 1.7785, -0.4834, 0.5
	a := math.Log(0.06 * 0.06)
	if !near(a, -5.62682, 5e-6) {
		t.Errorf("A = ln(0.06²) = %v, want -5.62682", a)
	}
	if fA := volatilityObjective(a, a, delta, phi, v, tau); !near(fA, -0.00053567, 5e-9) {
		t.Errorf("fA = %v, want -0.00053567", fA)
	}
	if fB := volatilityObjective(a-tau, a, delta, phi, v, tau); !near(fB, 1.999675, 5e-7) {
		t.Errorf("fB = %v, want 1.999675", fB)
	}
}

// newVolatility is step 5. Its answer is where the objective crosses 0, so
// plugging it back in must give (nearly) 0. The cases cover both ways the
// paper starts its search (Δ² above or below φ² + v), and a range of τ.
func TestNewVolatilityFindsTheRoot(t *testing.T) {
	cases := []struct {
		name                      string
		sigma, phi, v, delta, tau float64
		want                      float64 // exact, from the Python script; 0 = only check the root
	}{
		{"the paper's example", 0.06, ranking.UncertaintyFromRD(200), 1.7789771178226887, -0.4839332587277324, 0.5, 0.05999598428664987},
		{"Δ² above φ² + v (a huge upset)", 0.06, ranking.UncertaintyFromRD(30), 0, 0, 0.5, 0},
		{"small τ", 0.06, 1, 2, 1, 0.3, 0},
		{"large τ", 0.06, 1, 2, 1, 1.2, 0},
		{"high volatility", 0.5, 0.5, 1, -2, 0.5, 0},
		{"nothing surprising", 0.06, 1, 4, 0, 0.5, 0},
	}
	// The huge upset: a 1500 with RD 30 beats ten 2300s with RD 30.
	upsetInfo, upsetSurprise := 0.0, 0.0
	w := opponentWeight(ranking.UncertaintyFromRD(30))
	e := expectedScore(0, ranking.StrengthFromRanking(2300), w)
	for range 10 {
		upsetInfo += w * w * e * (1 - e)
		upsetSurprise += w * (1 - e)
	}
	cases[1].v = 1 / upsetInfo
	cases[1].delta = cases[1].v * upsetSurprise
	cases[1].want = 0.06137689977641344
	if d2 := cases[1].delta * cases[1].delta; d2 <= cases[1].phi*cases[1].phi+cases[1].v {
		t.Fatalf("the upset case has Δ² = %v, not above φ² + v; it doesn't test that branch", d2)
	}

	for _, c := range cases {
		got := newVolatility(c.sigma, c.phi, c.v, c.delta, c.tau)
		if !(got > 0) || math.IsInf(got, 0) {
			t.Errorf("%s: σ' = %v, want a positive number", c.name, got)
			continue
		}
		a := math.Log(c.sigma * c.sigma)
		if f := volatilityObjective(math.Log(got*got), a, c.delta, c.phi, c.v, c.tau); math.Abs(f) > 1e-4 {
			t.Errorf("%s: σ' = %v leaves the objective at %v, want about 0", c.name, got, f)
		}
		if c.want != 0 && !near(got, c.want, 1e-10) {
			t.Errorf("%s: σ' = %v, want %v", c.name, got, c.want)
		}
	}
}

// The huge upset through the whole period, checked against the Python
// script: a 1500 with RD 30 beats ten 2300s with RD 30.
func TestHugeUpset(t *testing.T) {
	alg := newTest(t)
	player := rated(1500, 30, 0.06)
	for i := range 10 {
		player, _ = update(t, alg, player, rated(2300, 30, 0.06), leftWins, monday.Add(time.Duration(i+1)*time.Minute))
	}
	next := alg.advance(player, monday.AddDate(0, 0, 7))
	checkState(t, "after the upset", next, state{
		Strength:    0.3299126285524569,
		Uncertainty: 0.18297215134445433,
		Volatility:  0.06137689977641344,
		PeriodStart: monday.AddDate(0, 0, 7),
	})
}

// "If a player does not compete during the rating period, then only Step 6
// applies": the rating and volatility stay, and φ' = √(φ² + σ²).
func TestEmptyPeriodsOnlyGrowUncertainty(t *testing.T) {
	alg := newTest(t)
	st := rated(1500, 200, 0.06)
	next := alg.advance(st, monday.AddDate(0, 0, 7))
	// From the Python script: RD 200 becomes 200.27141667616647.
	checkState(t, "one empty week", next, state{
		Strength:    0,
		Uncertainty: ranking.UncertaintyFromRD(200.27141667616647),
		Volatility:  0.06,
		PeriodStart: monday.AddDate(0, 0, 7),
	})
	// Ten empty weeks: Step 6 ten times, so φ² grows by 10σ².
	phi := ranking.UncertaintyFromRD(200)
	ten := alg.advance(st, monday.AddDate(0, 0, 70))
	checkState(t, "ten empty weeks", ten, state{
		Strength:    0,
		Uncertainty: math.Sqrt(phi*phi + 10*0.06*0.06),
		Volatility:  0.06,
		PeriodStart: monday.AddDate(0, 0, 70),
	})
}

// A period with games, then two empty ones: the played period closes with
// steps 3 to 8, and each empty one adds σ'² to φ².
func TestPlayedThenEmptyPeriods(t *testing.T) {
	alg := newTest(t)
	player := paperPlayer(t)
	got := alg.advance(player, monday.AddDate(0, 0, 21).Add(36*time.Hour)) // Tuesday noon, three weeks on
	phi, sigma := 0.8721992118223543, 0.05999598428664987
	checkState(t, "three weeks on", got, state{
		Strength:    -0.20694097368427727,
		Uncertainty: math.Sqrt(phi*phi + 2*sigma*sigma),
		Volatility:  sigma,
		PeriodStart: monday.AddDate(0, 0, 21),
	})
}

func TestAdvanceLeavesTheCurrentPeriodAlone(t *testing.T) {
	alg := newTest(t)
	player := paperPlayer(t)
	for _, now := range []time.Time{
		monday,                         // the same period
		monday.Add(6 * 24 * time.Hour), // still the same period
		monday.AddDate(0, 0, -3),       // earlier: the clock went back
		{},                             // no time at all
	} {
		if got := alg.advance(player, now); !sameState(got, player) {
			t.Errorf("advance to %v changed the state: %+v, want %+v", now, got, player)
		}
	}
}

// A brand-new image joins the current period as it is: it hasn't existed
// for the periods before, so its uncertainty doesn't grow.
func TestNewImageJoinsWithoutGrowth(t *testing.T) {
	alg := newTest(t)
	start := alg.Start().(state)
	if !start.PeriodStart.IsZero() {
		t.Fatalf("Start().PeriodStart = %v, want none", start.PeriodStart)
	}
	got := alg.advance(start, monday.Add(50*time.Hour))
	want := start
	want.PeriodStart = monday
	checkState(t, "a new image", got, want)
}

// Weekly periods start on a Monday at 00:00 UTC, every time: they're whole
// weeks of elapsed time, and a leap day moves the weekday on like any other
// day, so leap years can't make them drift. Checked across leap years and
// the century years that skip them (1900, 2100, 2200), every 19 hours and
// 7 minutes so the moments fall on all days and times. (This test found the
// year-2262 limit of counting in time.Duration; see periodStart.)
func TestWeeklyPeriodsAlwaysStartOnMonday(t *testing.T) {
	week := 7 * 24 * time.Hour
	end := time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)
	for at := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC); at.Before(end); at = at.Add(19*time.Hour + 7*time.Minute) {
		start := periodStart(at, week)
		if start.Weekday() != time.Monday || start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 || start.Nanosecond() != 0 {
			t.Fatalf("periodStart(%v) = %v, want a Monday at 00:00 UTC", at, start)
		}
		if at.Before(start) || !at.Before(start.Add(week)) {
			t.Fatalf("periodStart(%v) = %v, but that period doesn't contain it", at, start)
		}
	}
}

func TestPeriodStart(t *testing.T) {
	week := 7 * 24 * time.Hour
	cases := []struct {
		name   string
		t      time.Time
		length time.Duration
		want   time.Time
	}{
		{"the Monday itself", monday, week, monday},
		{"midweek", monday.Add(58 * time.Hour), week, monday},
		{"the last moment of the week", monday.Add(week - time.Nanosecond), week, monday},
		{"the next Monday", monday.Add(week), week, monday.Add(week)},
		{"Sunday night", monday.Add(-time.Nanosecond), week, monday.Add(-week)},
		// Periods follow UTC: 01:00 Monday in UTC+2 is still Sunday in UTC.
		{"another time zone", time.Date(2026, 11, 23, 1, 0, 0, 0, time.FixedZone("UTC+2", 2*3600)), week, monday},
		{"daily periods", monday.Add(30 * time.Hour), 24 * time.Hour, monday.Add(24 * time.Hour)},
		{"before 1970", time.Date(1969, 12, 31, 12, 0, 0, 0, time.UTC), week, time.Date(1969, 12, 29, 0, 0, 0, 0, time.UTC)},
		// Past where a time.Duration from 1970 would overflow (April 2262).
		{"year 2300 (1 January is a Monday)", time.Date(2300, 1, 1, 12, 0, 0, 0, time.UTC), week, time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"year 1600", time.Date(1600, 1, 1, 12, 0, 0, 0, time.UTC), week, time.Date(1599, 12, 27, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := periodStart(c.t, c.length); !got.Equal(c.want) {
			t.Errorf("%s: periodStart(%v) = %v, want %v", c.name, c.t, got, c.want)
		}
	}
}
