package ranking

import "fmt"

// Winner is which side a vote picked. We store the side (rather than just the
// winner's ID) because in some cases we might specifically care about which
// side was chosen. Users tend to be slightly biased to one side (usually the
// side they see first, which is the left side for native English readers
// because they read from left to right), and we might want to compensate for
// that bias.
type Winner uint8

const (
	WinnerLeft  Winner = 1
	WinnerTie   Winner = 2 // "about the same"
	WinnerRight Winner = 3
	// Adding a Winner? Update Outcome.Valid, and its tests, to match.
)

// Degree is how strongly a side was picked. Only graded algorithms read
// it; a plain pick is DegreeUnspecified, not a medium preference, so plain
// and graded votes can be mixed later. Stored like Winner.
type Degree uint8

const (
	DegreeUnspecified Degree = 0
	DegreeSlight      Degree = 1
	DegreeModerate    Degree = 2
	DegreeStrong      Degree = 3
	// Adding a Degree? Update Outcome.Valid, and its tests, to match.
)

// Outcome is what a vote recorded. Skipping a vote is not an Outcome.
type Outcome struct {
	Winner Winner
	Degree Degree
}

// Valid reports whether the Outcome can be stored. It must use existing values
// of Winner and Degree, and ties must use DegreeUnspecified. Go accepts any
// number in these types, so callers check it where outcomes come in, from a
// form or from the database.
func (o Outcome) Valid() bool {
	switch o.Winner {
	case WinnerLeft, WinnerRight:
		return o.Degree <= DegreeStrong
	case WinnerTie:
		return o.Degree == DegreeUnspecified
	}
	return false
}

// LeftScore is the left image's result as a number: 1 for a win, ½ for "about
// the same" and 0 for a loss. The right image's is 1 − LeftScore. Ties as half
// a win are from Glickman (1999), also used by Weng & Lin (2011, eq. 51) and
// the Glicko-2 paper. Degree is ignored: this is for algorithms that only
// look at which side won.
func (o Outcome) LeftScore() (float64, error) {
	if !o.Valid() {
		return 0, fmt.Errorf("invalid outcome (winner %d, degree %d)", o.Winner, o.Degree)
	}
	switch o.Winner {
	case WinnerLeft:
		return 1, nil
	case WinnerTie:
		return 0.5, nil
	}
	return 0, nil
}
