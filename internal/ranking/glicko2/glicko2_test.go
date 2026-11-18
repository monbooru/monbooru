package glicko2

import (
	"math"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

var (
	leftWins  = ranking.Outcome{Winner: ranking.WinnerLeft}
	tie       = ranking.Outcome{Winner: ranking.WinnerTie}
	rightWins = ranking.Outcome{Winner: ranking.WinnerRight}
)

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

// newTest is Glicko-2 with the default settings, unwrapped so tests can
// reach its internals.
func newTest(t testing.TB) *algorithm {
	t.Helper()
	alg, err := New(DefaultSettings())
	if err != nil {
		t.Fatalf("New(defaults): %v", err)
	}
	return alg.(*algorithm)
}

// update runs alg.Update on two states and unwraps the results.
func update(t *testing.T, alg *algorithm, left, right state, o ranking.Outcome, at time.Time) (state, state) {
	t.Helper()
	l, r, err := alg.Update(left, right, o, at)
	if err != nil {
		t.Fatalf("Update(%+v, %+v, %+v, %v): %v", left, right, o, at, err)
	}
	return l.(state), r.(state)
}

// sameState compares states exactly. Times are compared with Equal, which
// ignores how the time zone is recorded.
func sameState(a, b state) bool {
	ta, tb := a.PeriodStart, b.PeriodStart
	a.PeriodStart, b.PeriodStart = time.Time{}, time.Time{}
	return a == b && ta.Equal(tb)
}

// checkState compares the numbers to 1e-9, and the period exactly.
func checkState(t *testing.T, name string, got, want state) {
	t.Helper()
	ok := near(got.Strength, want.Strength, 1e-9) &&
		near(got.Uncertainty, want.Uncertainty, 1e-9) &&
		near(got.Volatility, want.Volatility, 1e-9) &&
		got.PeriodStart.Equal(want.PeriodStart) &&
		got.PeriodGames == want.PeriodGames &&
		near(got.PeriodInformation, want.PeriodInformation, 1e-9) &&
		near(got.PeriodSurprise, want.PeriodSurprise, 1e-9)
	if !ok {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}

func TestDefaultSettings(t *testing.T) {
	got := DefaultSettings()
	want := Settings{StartRD: 350, StartVolatility: 0.06, VolConstraint: 0.5, PeriodLength: 7 * 24 * time.Hour}
	if got != want {
		t.Errorf("DefaultSettings() = %+v, want %+v", got, want)
	}
}

func TestNewRejectsBadSettings(t *testing.T) {
	inf, nan := math.Inf(1), math.NaN()
	good := DefaultSettings()
	cases := []struct {
		name   string
		change func(*Settings)
	}{
		{"zero start RD", func(s *Settings) { s.StartRD = 0 }},
		{"negative start RD", func(s *Settings) { s.StartRD = -350 }},
		{"infinite start RD", func(s *Settings) { s.StartRD = inf }},
		{"NaN start RD", func(s *Settings) { s.StartRD = nan }},
		{"zero start volatility", func(s *Settings) { s.StartVolatility = 0 }},
		{"negative start volatility", func(s *Settings) { s.StartVolatility = -0.06 }},
		{"NaN start volatility", func(s *Settings) { s.StartVolatility = nan }},
		{"zero τ", func(s *Settings) { s.VolConstraint = 0 }},
		{"negative τ", func(s *Settings) { s.VolConstraint = -0.5 }},
		{"infinite τ", func(s *Settings) { s.VolConstraint = inf }},
		{"zero period", func(s *Settings) { s.PeriodLength = 0 }},
		{"negative period", func(s *Settings) { s.PeriodLength = -time.Hour }},
		{"under a second", func(s *Settings) { s.PeriodLength = time.Second / 2 }},
		{"not whole seconds", func(s *Settings) { s.PeriodLength = 7*24*time.Hour + time.Millisecond }},
	}
	for _, c := range cases {
		s := good
		c.change(&s)
		if _, err := New(s); err == nil {
			t.Errorf("%s: New(%+v) succeeded, want an error", c.name, s)
		}
	}
	if _, err := New(good); err != nil {
		t.Errorf("New(defaults): %v", err)
	}
}

func TestIdentity(t *testing.T) {
	alg := newTest(t)
	if got := alg.ID(); got != ranking.AlgorithmGlicko2 {
		t.Errorf("ID() = %d, want AlgorithmGlicko2 (%d)", got, ranking.AlgorithmGlicko2)
	}
	if got := alg.Name(); got != "glicko-2" {
		t.Errorf("Name() = %q, want %q", got, "glicko-2")
	}
}

// Step 1(a): an unrated player starts at 1500, RD 350, volatility 0.06.
func TestStart(t *testing.T) {
	alg := newTest(t)
	st := alg.Start().(state)
	if st.Strength != 0 || !near(ranking.RDFromUncertainty(st.Uncertainty), 350, 1e-9) || st.Volatility != 0.06 {
		t.Errorf("Start() = %+v, want 1500 / RD 350 / volatility 0.06", st)
	}
	if !st.PeriodStart.IsZero() || st.PeriodGames != 0 || st.PeriodInformation != 0 || st.PeriodSurprise != 0 {
		t.Errorf("Start() = %+v, want no period and no games", st)
	}
	if got := alg.Shown(st); got != (ranking.Estimate{Strength: st.Strength, Uncertainty: st.Uncertainty}) {
		t.Errorf("Shown(Start()) = %+v", got)
	}
	// Start follows the settings.
	s := DefaultSettings()
	s.StartRD, s.StartVolatility = 200, 0.1
	other, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	st = other.Start().(state)
	if !near(ranking.RDFromUncertainty(st.Uncertainty), 200, 1e-9) || st.Volatility != 0.1 {
		t.Errorf("Start() with RD 200 and volatility 0.1 = %+v", st)
	}
}

// The stored format is pinned exactly: stored rows depend on it.
func TestEncodeFormat(t *testing.T) {
	alg := newTest(t)
	cases := []struct {
		s    state
		want string
	}{
		// Not yet in a period: the period fields are left out.
		{state{Strength: 0, Uncertainty: 2, Volatility: 0.06}, `{"strength":0,"uncertainty":2,"volatility":0.06}`},
		{state{Strength: -0.25, Uncertainty: 1.5, Volatility: 0.06, PeriodStart: monday},
			`{"strength":-0.25,"uncertainty":1.5,"volatility":0.06,"period_start":"2026-11-16T00:00:00Z"}`},
		{state{Strength: -0.25, Uncertainty: 1.5, Volatility: 0.06, PeriodStart: monday, PeriodGames: 2, PeriodInformation: 0.5, PeriodSurprise: -0.125},
			`{"strength":-0.25,"uncertainty":1.5,"volatility":0.06,"period_start":"2026-11-16T00:00:00Z","period_games":2,"period_information":0.5,"period_surprise":-0.125}`},
	}
	for _, c := range cases {
		got, err := alg.Encode(c.s)
		if err != nil || string(got) != c.want {
			t.Errorf("Encode(%+v) = %s, %v; want %s", c.s, got, err, c.want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	alg := newTest(t)
	states := []state{
		alg.Start().(state),
		paperPlayer(t),
		{Strength: 0.1, Uncertainty: 0.3, Volatility: 0.07, PeriodStart: monday.Add(time.Nanosecond)},
		{Strength: -1e-300, Uncertainty: 1e-300, Volatility: 1e300, PeriodStart: monday, PeriodGames: 1, PeriodInformation: 1e-300, PeriodSurprise: -1e300},
	}
	for _, s := range states {
		b, err := alg.Encode(s)
		if err != nil {
			t.Errorf("Encode(%+v): %v", s, err)
			continue
		}
		got, err := alg.Decode(b)
		if err != nil {
			t.Errorf("Decode(%s): %v", b, err)
			continue
		}
		// Exactly equal: storage must not round anything.
		if !sameState(got.(state), s) {
			t.Errorf("Decode(Encode(%+v)) = %+v", s, got)
		}
	}
}

// Decode reads any valid JSON with the needed fields, not just what Encode
// writes.
func TestDecodeAcceptsOtherLayouts(t *testing.T) {
	alg := newTest(t)
	want := state{Strength: 0.5, Uncertainty: 1, Volatility: 0.06, PeriodStart: monday}
	for _, b := range []string{
		`{"volatility":0.06,"period_start":"2026-11-16T00:00:00Z","uncertainty":1,"strength":0.5}`,      // other order
		`{"strength":0.5,"uncertainty":1,"volatility":0.06,"period_start":"2026-11-16T02:00:00+02:00"}`, // same moment, another zone
		`{"strength":0.5,"uncertainty":1,"volatility":0.06,"period_start":"2026-11-16T00:00:00Z","period_games":0}`,
		`{"strength":0.5,"uncertainty":1,"volatility":0.06,"period_start":"2026-11-16T00:00:00Z","later":42}`, // a later field
	} {
		got, err := alg.Decode([]byte(b))
		if err != nil || !sameState(got.(state), want) {
			t.Errorf("Decode(%s) = %+v, %v; want %+v", b, got, err, want)
		}
	}
}

func TestDecodeRejectsBadBytes(t *testing.T) {
	alg := newTest(t)
	const p = `,"period_start":"2026-11-16T00:00:00Z"`
	cases := []struct {
		name string
		b    string
	}{
		{"empty", ``},
		{"not JSON", `strength=0.5`},
		{"empty object", `{}`},
		{"no strength", `{"uncertainty":1,"volatility":0.06}`},
		{"no uncertainty", `{"strength":0.5,"volatility":0.06}`},
		{"no volatility", `{"strength":0.5,"uncertainty":1}`},
		{"null volatility", `{"strength":0.5,"uncertainty":1,"volatility":null}`},
		{"zero uncertainty", `{"strength":0.5,"uncertainty":0,"volatility":0.06}`},
		{"negative uncertainty", `{"strength":0.5,"uncertainty":-1,"volatility":0.06}`},
		{"zero volatility", `{"strength":0.5,"uncertainty":1,"volatility":0}`},
		{"too big to be a number", `{"strength":1e999,"uncertainty":1,"volatility":0.06}`},
		{"not a time", `{"strength":0.5,"uncertainty":1,"volatility":0.06,"period_start":"Monday"}`},
		{"games without a period", `{"strength":0.5,"uncertainty":1,"volatility":0.06,"period_games":1,"period_information":0.5}`},
		{"negative games", `{"strength":0.5,"uncertainty":1,"volatility":0.06` + p + `,"period_games":-1}`},
		{"sums without games", `{"strength":0.5,"uncertainty":1,"volatility":0.06` + p + `,"period_information":0.5}`},
		{"surprise without games", `{"strength":0.5,"uncertainty":1,"volatility":0.06` + p + `,"period_surprise":0.5}`},
		{"negative information", `{"strength":0.5,"uncertainty":1,"volatility":0.06` + p + `,"period_games":1,"period_information":-0.5}`},
	}
	for _, c := range cases {
		if s, err := alg.Decode([]byte(c.b)); err == nil {
			t.Errorf("%s: Decode(%s) = %+v, want an error", c.name, c.b, s)
		}
	}
}

func TestEncodeRejectsBadStates(t *testing.T) {
	alg := newTest(t)
	for _, s := range []state{
		{Strength: math.NaN(), Uncertainty: 1, Volatility: 0.06},
		{Strength: 0, Uncertainty: 0, Volatility: 0.06},
		{Strength: 0, Uncertainty: 1, Volatility: -0.06},
		{Strength: 0, Uncertainty: 1, Volatility: 0.06, PeriodGames: 1},
		{Strength: 0, Uncertainty: 1, Volatility: 0.06, PeriodStart: monday, PeriodSurprise: 1},
		{Strength: 0, Uncertainty: 1, Volatility: 0.06, PeriodStart: monday, PeriodGames: 1, PeriodInformation: math.Inf(1)},
	} {
		if b, err := alg.Encode(s); err == nil {
			t.Errorf("Encode(%+v) = %s, want an error", s, b)
		}
	}
}
