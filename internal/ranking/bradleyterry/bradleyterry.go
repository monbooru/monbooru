// Package bradleyterry is the Bradley–Terry ranking algorithm (Bradley &
// Terry 1952): an image with strength a beats one with strength b with chance
// sigmoid(a − b). Each vote updates its two images (Weng & Lin 2011), and Fit
// recomputes everything from all votes. Shared code uses it only through
// ranking.Algorithm.
package bradleyterry

import (
	"encoding/json"
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

// algorithm is unexported: other packages only see it as a ranking.Algorithm.
type algorithm struct {
	settings Settings
}

// New returns Bradley–Terry with the given settings, or an error if they
// aren't usable.
func New(settings Settings) (ranking.Algorithm, error) {
	if !positiveFinite(settings.StartUncertainty) {
		return nil, fmt.Errorf("start uncertainty must be a positive number, got %v", settings.StartUncertainty)
	}
	if !positiveFinite(settings.PerfVariance) {
		return nil, fmt.Errorf("perf variance must be a positive number, got %v", settings.PerfVariance)
	}
	return &algorithm{settings: settings}, nil
}

// positiveFinite is false for NaN too, since NaN > 0 is false.
func positiveFinite(x float64) bool { return x > 0 && !math.IsInf(x, 1) }

// state is everything Bradley–Terry keeps per image: just the
// Estimate itself.
type state struct {
	Strength    float64
	Uncertainty float64
}

func (s state) valid() bool {
	return !math.IsNaN(s.Strength) && !math.IsInf(s.Strength, 0) && positiveFinite(s.Uncertainty)
}

func (bt *algorithm) ID() ranking.AlgorithmID { return ranking.AlgorithmBradleyTerry }
func (bt *algorithm) Name() string            { return "bradley-terry" }

// Start is "probably average": strength 0, as unsure as the settings say.
func (bt *algorithm) Start() ranking.State {
	return state{Strength: 0, Uncertainty: bt.settings.StartUncertainty}
}

func (bt *algorithm) Shown(s ranking.State) ranking.Estimate {
	st := s.(state)
	return ranking.Estimate{Strength: st.Strength, Uncertainty: st.Uncertainty}
}

// storedState is how a state is saved in the database: as JSON, such as
// {"strength":0.74,"uncertainty":1.58}. The json names are the stored
// format, so the Go names can change but these must not. Pointers let Decode
// tell a missing field from a 0. Fields it doesn't know are ignored, so a
// later version can add some without breaking this one. JSON is slightly
// slower than bytes, but we're using JSON for readability and maintainability.
// If we need to extract a bit of performance later we could switch to bytes.
type storedState struct {
	Strength    *float64 `json:"strength"`
	Uncertainty *float64 `json:"uncertainty"`
}

// Encode saves a state as JSON. Go writes each number with just enough
// digits to read back exactly, so nothing is rounded.
func (bt *algorithm) Encode(s ranking.State) ([]byte, error) {
	st := s.(state)
	if !st.valid() {
		return nil, fmt.Errorf("invalid Bradley–Terry state %+v", st)
	}
	return json.Marshal(storedState{Strength: &st.Strength, Uncertainty: &st.Uncertainty})
}

func (bt *algorithm) Decode(b []byte) (ranking.State, error) {
	var stored storedState
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil, fmt.Errorf("not a stored Bradley–Terry state: %w", err)
	}
	if stored.Strength == nil || stored.Uncertainty == nil {
		return nil, fmt.Errorf("stored Bradley–Terry state %s is missing strength or uncertainty", b)
	}
	st := state{Strength: *stored.Strength, Uncertainty: *stored.Uncertainty}
	if !st.valid() {
		return nil, fmt.Errorf("invalid stored Bradley–Terry state %s", b)
	}
	return st, nil
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
