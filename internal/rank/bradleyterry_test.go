package rank

import (
	"math"
	"testing"
)

// newTest is Bradley–Terry with the default settings.
func newTest(t testing.TB) *Model {
	t.Helper()
	bt, err := New(DefaultSettings())
	if err != nil {
		t.Fatalf("New(defaults): %v", err)
	}
	return bt
}

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

func TestStart(t *testing.T) {
	bt := newTest(t)
	if got, want := bt.Start(), (Estimate{Strength: 0, Uncertainty: 2}); got != want {
		t.Errorf("Start() = %+v, want %+v", got, want)
	}
	// Start follows the settings.
	wide, err := New(Settings{StartUncertainty: 3, PerfVariance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := wide.Start(), (Estimate{Strength: 0, Uncertainty: 3}); got != want {
		t.Errorf("Start() with StartUncertainty 3 = %+v, want %+v", got, want)
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

func TestUsable(t *testing.T) {
	inf, nan := math.Inf(1), math.NaN()
	for _, e := range []Estimate{
		{Strength: 0, Uncertainty: 2}, {Strength: -3.25, Uncertainty: 0.1}, {Strength: 1e300, Uncertainty: 1e-300},
	} {
		if !usable(e) {
			t.Errorf("usable(%+v) = false, want true", e)
		}
	}
	for _, e := range []Estimate{
		{Strength: nan, Uncertainty: 1}, {Strength: inf, Uncertainty: 1}, {Strength: -inf, Uncertainty: 1},
		{Strength: 0, Uncertainty: 0}, {Strength: 0, Uncertainty: -1}, {Strength: 0, Uncertainty: inf}, {Strength: 0, Uncertainty: nan},
	} {
		if usable(e) {
			t.Errorf("usable(%+v) = true, want false", e)
		}
	}
}
