package bradleyterry

import (
	"math"
	"testing"

	"github.com/monbooru/monbooru/internal/ranking"
)

// newTest is Bradley–Terry with the default settings, unwrapped so tests can
// reach its internals.
func newTest(t testing.TB) *algorithm {
	t.Helper()
	bt, err := New(DefaultSettings())
	if err != nil {
		t.Fatalf("New(defaults): %v", err)
	}
	return bt.(*algorithm)
}

func near(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

func TestDefaultBradleyTerrySettings(t *testing.T) {
	got := DefaultSettings()
	want := Settings{StartUncertainty: 2, PerfVariance: 0.5}
	if got != want {
		t.Errorf("DefaultSettings() = %+v, want %+v", got, want)
	}
}

func TestNewRejectsBadSettings(t *testing.T) {
	inf, nan := math.Inf(1), math.NaN()
	cases := []struct {
		name     string
		settings Settings
	}{
		{"all zero", Settings{}},
		{"zero start uncertainty", Settings{0, 0.5}},
		{"negative start uncertainty", Settings{-2, 0.5}},
		{"infinite start uncertainty", Settings{inf, 0.5}},
		{"NaN start uncertainty", Settings{nan, 0.5}},
		{"zero perf variance", Settings{2, 0}},
		{"negative perf variance", Settings{2, -0.5}},
		{"infinite perf variance", Settings{2, inf}},
		{"NaN perf variance", Settings{2, nan}},
	}
	for _, c := range cases {
		if _, err := New(c.settings); err == nil {
			t.Errorf("%s: New(%+v) succeeded, want an error", c.name, c.settings)
		}
	}
}

func TestIdentity(t *testing.T) {
	bt := newTest(t)
	if got := bt.ID(); got != ranking.AlgorithmBradleyTerry {
		t.Errorf("ID() = %d, want AlgorithmBradleyTerry (%d)", got, ranking.AlgorithmBradleyTerry)
	}
	if got := bt.Name(); got != "bradley-terry" {
		t.Errorf("Name() = %q, want %q", got, "bradley-terry")
	}
}

func TestStart(t *testing.T) {
	bt := newTest(t)
	if got, want := bt.Shown(bt.Start()), (ranking.Estimate{Strength: 0, Uncertainty: 2}); got != want {
		t.Errorf("Shown(Start()) = %+v, want %+v", got, want)
	}
	// Start follows the settings.
	wide, err := New(Settings{StartUncertainty: 3, PerfVariance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := wide.Shown(wide.Start()), (ranking.Estimate{Strength: 0, Uncertainty: 3}); got != want {
		t.Errorf("Shown(Start()) with StartUncertainty 3 = %+v, want %+v", got, want)
	}
}

func TestSigmoid(t *testing.T) {
	cases := []struct{ x, want float64 }{
		{0, 0.5},
		{1, 0.7310585786300049}, // the plan's "a gap of 1 is 73%"
		{-1, 0.2689414213699951},
		{2, 0.8807970779778823},
		{3, 0.9525741268224334},
		// Huge gaps must give 0 or 1, not NaN.
		{1000, 1},
		{-1000, 0},
	}
	for _, c := range cases {
		if got := sigmoid(c.x); !near(got, c.want, 1e-15) {
			t.Errorf("sigmoid(%v) = %v, want %v", c.x, got, c.want)
		}
	}
}

func TestLogSigmoid(t *testing.T) {
	// Where sigmoid is comfortably inside 0–1, it's just log(sigmoid).
	for _, x := range []float64{-30, -5, -1, -0.5, 0, 0.5, 1, 5, 30} {
		if got, want := logSigmoid(x), math.Log(sigmoid(x)); !near(got, want, 1e-12) {
			t.Errorf("logSigmoid(%v) = %v, want log(sigmoid(%v)) = %v", x, got, x, want)
		}
	}
	// At huge gaps, log(sigmoid) would be log(0) = −∞; logSigmoid stays exact.
	if got := logSigmoid(-1000); !near(got, -1000, 1e-9) {
		t.Errorf("logSigmoid(-1000) = %v, want -1000", got)
	}
	if got := logSigmoid(1000); got != 0 {
		t.Errorf("logSigmoid(1000) = %v, want 0", got)
	}
}

// The stored format is pinned exactly: stored rows depend on it.
func TestEncodeFormat(t *testing.T) {
	bt := newTest(t)
	cases := []struct {
		s    state
		want string
	}{
		{state{Strength: 1.5, Uncertainty: 0.25}, `{"strength":1.5,"uncertainty":0.25}`},
		// A brand-new image: whole numbers are written without a decimal point.
		{state{Strength: 0, Uncertainty: 2}, `{"strength":0,"uncertainty":2}`},
		// Every digit needed to read the number back exactly, and no more.
		{state{Strength: -0.7407743930623084, Uncertainty: 1.5793690933393363},
			`{"strength":-0.7407743930623084,"uncertainty":1.5793690933393363}`},
		{state{Strength: 1e-300, Uncertainty: 1e300}, `{"strength":1e-300,"uncertainty":1e+300}`},
	}
	for _, c := range cases {
		got, err := bt.Encode(c.s)
		if err != nil || string(got) != c.want {
			t.Errorf("Encode(%+v) = %s, %v; want %s", c.s, got, err, c.want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	bt := newTest(t)
	states := []ranking.State{
		bt.Start(),
		state{Strength: 1.5, Uncertainty: 0.25},
		state{Strength: -3.25, Uncertainty: 1.9245008972987525},
		state{Strength: 0.1, Uncertainty: 0.3}, // not exact in binary
		state{Strength: 1e-300, Uncertainty: 1e-300},
		state{Strength: math.MaxFloat64, Uncertainty: math.SmallestNonzeroFloat64},
	}
	for _, s := range states {
		b, err := bt.Encode(s)
		if err != nil {
			t.Errorf("Encode(%+v): %v", s, err)
			continue
		}
		got, err := bt.Decode(b)
		if err != nil {
			t.Errorf("Decode(Encode(%+v)): %v", s, err)
			continue
		}
		// Exactly equal: storage must not round anything.
		if got != s {
			t.Errorf("Decode(Encode(%+v)) = %+v", s, got)
		}
	}
}

// Decode reads any valid JSON with both fields, not just what Encode writes.
func TestDecodeAcceptsOtherLayouts(t *testing.T) {
	bt := newTest(t)
	want := state{Strength: 0.5, Uncertainty: 1}
	for _, b := range []string{
		`{"uncertainty":1,"strength":0.5}`,            // other order
		` { "strength" : 0.5 , "uncertainty" : 1 } `,  // spaces
		`{"strength":5e-1,"uncertainty":1.0}`,         // other number spellings
		`{"strength":0.5,"uncertainty":1,"later":42}`, // a field from a later version
	} {
		got, err := bt.Decode([]byte(b))
		if err != nil || got != want {
			t.Errorf("Decode(%s) = %+v, %v; want %+v", b, got, err, want)
		}
	}
}

func TestDecodeRejectsBadBytes(t *testing.T) {
	bt := newTest(t)
	cases := []struct {
		name string
		b    string
	}{
		{"empty", ``},
		{"not JSON", `strength=0.5`},
		{"cut off", `{"strength":0.5,"uncertainty":1`},
		{"extra after the object", `{"strength":0.5,"uncertainty":1}x`},
		{"a list, not an object", `[0.5,1]`},
		{"empty object", `{}`},
		{"no strength", `{"uncertainty":1}`},
		{"no uncertainty", `{"strength":0.5}`},
		{"null strength", `{"strength":null,"uncertainty":1}`},
		{"strength as text", `{"strength":"0.5","uncertainty":1}`},
		{"misspelled field", `{"strenght":0.5,"uncertainty":1}`},
		{"too big to be a number", `{"strength":1e999,"uncertainty":1}`},
		{"zero uncertainty", `{"strength":0.5,"uncertainty":0}`},
		{"negative uncertainty", `{"strength":0.5,"uncertainty":-1}`},
	}
	for _, c := range cases {
		if s, err := bt.Decode([]byte(c.b)); err == nil {
			t.Errorf("%s: Decode(%s) = %+v, want an error", c.name, c.b, s)
		}
	}
	if s, err := bt.Decode(nil); err == nil {
		t.Errorf("Decode(nil) = %+v, want an error", s)
	}
}

func TestEncodeRejectsBadStates(t *testing.T) {
	bt := newTest(t)
	for _, s := range []state{
		{Strength: math.NaN(), Uncertainty: 1},
		{Strength: math.Inf(-1), Uncertainty: 1},
		{Strength: 0, Uncertainty: 0},
		{Strength: 0, Uncertainty: -1},
		{Strength: 0, Uncertainty: math.Inf(1)},
	} {
		if b, err := bt.Encode(s); err == nil {
			t.Errorf("Encode(%+v) = % x, want an error", s, b)
		}
	}
}
