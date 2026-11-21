// Package ranking turns pairwise votes ("I like this one more") into a
// ranking per image. It holds what every algorithm shares (votes, outcomes,
// the Algorithm interface, the display scale); each algorithm lives in its
// own subpackage, such as ranking/bradleyterry. Math only: no database,
// routes or UI.
package ranking

import (
	"context"
	"time"
)

// AlgorithmID names an algorithm. Stored, like Outcome, so the values are
// written out and never reused.
type AlgorithmID uint8

const (
	// AlgorithmDefault is a setting, not an algorithm: whatever this
	// version recommends. Resolve turns it into one.
	AlgorithmDefault      AlgorithmID = 0
	AlgorithmBradleyTerry AlgorithmID = 1
	AlgorithmGlicko2      AlgorithmID = 2
	// Adding an algorithm? Update Valid, and its tests, to match, and add it
	// to algorithms.New: TestNewMatchesValid there checks the two agree.
)

// recommended is what AlgorithmDefault means in this version. Changing it
// moves every Default category over, and changes what their numbers mean.
const recommended = AlgorithmBradleyTerry

// Valid reports whether id is a known algorithm or the Default setting.
func (id AlgorithmID) Valid() bool {
	return id <= AlgorithmGlicko2
}

// Resolve turns the Default setting into this version's recommendation;
// an explicit choice is returned as is.
func Resolve(id AlgorithmID) AlgorithmID {
	if id == AlgorithmDefault {
		return recommended
	}
	return id
}

// Estimate is all the shared code sees of an image: display, sort, "done"
// and pair picking. Every algorithm reports it in the same units.
type Estimate struct {
	// Strength is the best guess of how much the image is liked; 0 is a
	// brand-new image, and a gap of 1 is 73% to 27% odds.
	Strength float64
	// Uncertainty is how unsure that guess is, as a standard deviation in
	// strength units.
	Uncertainty float64
}

// State is an algorithm's own bookkeeping for one image. Shared code
// stores it and hands it back but never looks inside; each algorithm keeps
// its own concrete type in it and unwraps it with a type assertion. This is
// needed for things like Glicko-2's volatility. Handing an algorithm another
// algorithm's State is a bug, and panics.
type State any

// Vote is one stored comparison.
type Vote struct {
	Left, Right int64 // image ids
	Outcome     Outcome
	At          time.Time // Used by some algorithms (like Glicko-2)
}

// Algorithm is one way of turning votes into estimates.
type Algorithm interface {
	ID() AlgorithmID
	Name() string
	// Start is a brand-new image's state: the starting assumption.
	Start() State
	// Shown is what the shared code sees of s. An algorithm method, not a
	// State one, because it can need the algorithm's settings.
	Shown(s State) Estimate
	// Update applies one vote, made at time at, to its two images only. It
	// fails on an invalid outcome. Algorithms that ignore time accept any.
	Update(left, right State, outcome Outcome, at time.Time) (State, State, error)
	// Advance brings s up to date at time now, without any new votes: for
	// algorithms with rating periods (Glicko-2), it closes the periods that
	// have ended. Others return s unchanged.
	Advance(s State, now time.Time) State
	// Fit recomputes every voted image from all votes: the recompute button.
	Fit(ctx context.Context, votes []Vote) (map[int64]State, error)
	// Encode and Decode turn a State into bytes for storage and back.
	Encode(s State) ([]byte, error)
	Decode(b []byte) (State, error)
}
