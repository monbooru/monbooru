// Package glicko2 is the Glicko-2 ranking algorithm, implemented as written
// in Glickman's "Example of the Glicko-2 system" (2022). Votes are grouped
// into rating periods: during a period, each image's shown ranking is the
// period's result as if it ended now; when it ends, those become the next
// period's starting values. Shared code uses it only through
// ranking.Algorithm.
package glicko2

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

// Settings are Glicko-2's tunable values. V1 always uses DefaultSettings.
// The starting rating is always the paper's 1500 (strength 0), which is also
// what an unranked image shows.
type Settings struct {
	// StartRD and StartVolatility are a brand-new image's RD and volatility
	// (the paper's step 1(a): 350 and 0.06, the latter "depends on the
	// particular application").
	StartRD         float64
	StartVolatility float64
	// VolConstraint is the paper's τ: how fast volatility can change.
	// "Reasonable choices are between 0.3 and 1.2".
	VolConstraint float64
	// PeriodLength is how long a rating period lasts ("at the discretion of
	// the administrator"), in whole seconds. Periods start at fixed times:
	// weekly ones on Mondays at 00:00 UTC.
	PeriodLength time.Duration
}

func DefaultSettings() Settings {
	return Settings{StartRD: 350, StartVolatility: 0.06, VolConstraint: 0.5, PeriodLength: 7 * 24 * time.Hour}
}

// algorithm is unexported: other packages only see it as a ranking.Algorithm.
type algorithm struct {
	settings Settings
}

// New returns Glicko-2 with the given settings, or an error if they aren't
// usable.
func New(settings Settings) (ranking.Algorithm, error) {
	if !positiveFinite(settings.StartRD) {
		return nil, fmt.Errorf("start RD must be a positive number, got %v", settings.StartRD)
	}
	if !positiveFinite(settings.StartVolatility) {
		return nil, fmt.Errorf("start volatility must be a positive number, got %v", settings.StartVolatility)
	}
	if !positiveFinite(settings.VolConstraint) {
		return nil, fmt.Errorf("τ must be a positive number, got %v", settings.VolConstraint)
	}
	if settings.PeriodLength < time.Second || settings.PeriodLength%time.Second != 0 {
		return nil, fmt.Errorf("rating period length must be a whole number of seconds, at least one, got %v", settings.PeriodLength)
	}
	return &algorithm{settings: settings}, nil
}

// positiveFinite is false for NaN too, since NaN > 0 is false.
func positiveFinite(x float64) bool { return x > 0 && !math.IsInf(x, 1) }

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// state is everything Glicko-2 keeps per image: its values at the start of
// the current rating period, and running sums of the period's games so far.
// The paper's symbols are in brackets; strength and uncertainty are already
// on its Glicko-2 scale.
type state struct {
	Strength    float64 // [µ] at the start of the period
	Uncertainty float64 // [φ] at the start of the period
	Volatility  float64 // [σ]
	// PeriodStart is when the current period began. Zero for a brand-new
	// image that hasn't joined a period yet.
	PeriodStart time.Time
	PeriodGames int
	// PeriodInformation is Σ g(φj)² E (1 − E) over the period's games: the
	// paper's v is 1 / PeriodInformation.
	PeriodInformation float64
	// PeriodSurprise is Σ g(φj) (s − E) over the period's games: the
	// paper's Δ is v × PeriodSurprise.
	PeriodSurprise float64
}

func (s state) valid() bool {
	if !finite(s.Strength) || !positiveFinite(s.Uncertainty) || !positiveFinite(s.Volatility) {
		return false
	}
	if !finite(s.PeriodInformation) || s.PeriodInformation < 0 || !finite(s.PeriodSurprise) || s.PeriodGames < 0 {
		return false
	}
	if s.PeriodGames == 0 && (s.PeriodInformation != 0 || s.PeriodSurprise != 0) {
		return false // sums without games
	}
	return s.PeriodGames == 0 || !s.PeriodStart.IsZero() // games need a period
}

func (alg *algorithm) ID() ranking.AlgorithmID { return ranking.AlgorithmGlicko2 }
func (alg *algorithm) Name() string            { return "glicko-2" }

// Start is the paper's unrated player: 1500, with the settings' RD and
// volatility, not yet in any period.
func (alg *algorithm) Start() ranking.State {
	return state{
		Strength:    0,
		Uncertainty: ranking.UncertaintyFromRD(alg.settings.StartRD),
		Volatility:  alg.settings.StartVolatility,
	}
}

// Shown is the current period's result as if it ended now (steps 3 to 8).
// With no games yet in the period, that's its starting values: uncertainty
// only grows when a period actually ends. Call Advance first if periods may
// have ended since s was last updated.
func (alg *algorithm) Shown(s ranking.State) ranking.Estimate {
	st := s.(state)
	strength, uncertainty, _ := alg.periodResult(st)
	return ranking.Estimate{Strength: strength, Uncertainty: uncertainty}
}

// storedState is how a state is saved in the database, as JSON. The json
// names are the stored format, so the Go names can change but those must
// not. The paper's three numbers are pointers so Decode can tell a missing
// one from a 0; the period fields are left out while they're empty. Fields
// it doesn't know are ignored, so a later version can add some.
type storedState struct {
	Strength          *float64  `json:"strength"`
	Uncertainty       *float64  `json:"uncertainty"`
	Volatility        *float64  `json:"volatility"`
	PeriodStart       time.Time `json:"period_start,omitzero"`
	PeriodGames       int       `json:"period_games,omitzero"`
	PeriodInformation float64   `json:"period_information,omitzero"`
	PeriodSurprise    float64   `json:"period_surprise,omitzero"`
}

// Encode saves a state as JSON. Go writes each number with just enough
// digits to read back exactly, so nothing is rounded.
func (alg *algorithm) Encode(s ranking.State) ([]byte, error) {
	st := s.(state)
	if !st.valid() {
		return nil, fmt.Errorf("invalid Glicko-2 state %+v", st)
	}
	return json.Marshal(storedState{
		Strength:          &st.Strength,
		Uncertainty:       &st.Uncertainty,
		Volatility:        &st.Volatility,
		PeriodStart:       st.PeriodStart.UTC(),
		PeriodGames:       st.PeriodGames,
		PeriodInformation: st.PeriodInformation,
		PeriodSurprise:    st.PeriodSurprise,
	})
}

func (alg *algorithm) Decode(b []byte) (ranking.State, error) {
	var stored storedState
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil, fmt.Errorf("not a stored Glicko-2 state: %w", err)
	}
	if stored.Strength == nil || stored.Uncertainty == nil || stored.Volatility == nil {
		return nil, fmt.Errorf("stored Glicko-2 state %s is missing strength, uncertainty or volatility", b)
	}
	st := state{
		Strength:          *stored.Strength,
		Uncertainty:       *stored.Uncertainty,
		Volatility:        *stored.Volatility,
		PeriodGames:       stored.PeriodGames,
		PeriodInformation: stored.PeriodInformation,
		PeriodSurprise:    stored.PeriodSurprise,
	}
	if !stored.PeriodStart.IsZero() {
		st.PeriodStart = stored.PeriodStart.UTC()
	}
	if !st.valid() {
		return nil, fmt.Errorf("invalid stored Glicko-2 state %s", b)
	}
	return st, nil
}
