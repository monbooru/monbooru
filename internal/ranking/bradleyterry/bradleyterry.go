// Package bradleyterry is the Bradley–Terry ranking model (Bradley & Terry
// 1952): an image with strength a beats one with strength b with chance
// sigmoid(a − b). Each vote updates its two images (Weng & Lin 2011), and Fit
// recomputes everything from all votes.
package bradleyterry

import (
	"fmt"
	"math"

	"github.com/monbooru/monbooru/internal/ranking"
)

// Settings are Bradley–Terry's tunable values. V1 always uses
// DefaultSettings.
type Settings struct {
	// StartUncertainty is a brand-new image's uncertainty: the width of the
	// bell-curve starting assumption, in strength units. There's no standard
	// value; 2 (RD ≈ 347) is close to Glicko-2's starting RD of 350.
	StartUncertainty float64
	// PerfVariance is how much a single vote varies around the true
	// strengths, for the quick update (Weng & Lin's β²). At ½, the quick
	// update predicts like the refit once both images are certain.
	PerfVariance float64
}

func DefaultSettings() Settings {
	return Settings{StartUncertainty: 2, PerfVariance: 0.5}
}

// Model is Bradley–Terry with a given set of settings.
type Model struct {
	settings Settings
}

// New returns Bradley–Terry with the given settings, or an error if they
// aren't usable.
func New(settings Settings) (*Model, error) {
	if !positiveFinite(settings.StartUncertainty) {
		return nil, fmt.Errorf("start uncertainty must be a positive number, got %v", settings.StartUncertainty)
	}
	if !positiveFinite(settings.PerfVariance) {
		return nil, fmt.Errorf("perf variance must be a positive number, got %v", settings.PerfVariance)
	}
	return &Model{settings: settings}, nil
}

// positiveFinite is false for NaN too, since NaN > 0 is false.
func positiveFinite(x float64) bool { return x > 0 && !math.IsInf(x, 1) }

// usable reports whether e is a real estimate: a finite strength and a
// positive, finite uncertainty.
func usable(e ranking.Estimate) bool {
	return !math.IsNaN(e.Strength) && !math.IsInf(e.Strength, 0) && positiveFinite(e.Uncertainty)
}

// Start is "probably average": strength 0, as unsure as the settings say.
func (m *Model) Start() ranking.Estimate {
	return ranking.Estimate{Strength: 0, Uncertainty: m.settings.StartUncertainty}
}

// sigmoid is 1 / (1 + e^(−x)): the chance that an image beats one x weaker.
func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

// logSigmoid is log(sigmoid(x)), written so a huge gap gives a finite answer
// instead of log(0).
func logSigmoid(x float64) float64 {
	if x >= 0 {
		return -math.Log1p(math.Exp(-x))
	}
	return x - math.Log1p(math.Exp(x))
}
