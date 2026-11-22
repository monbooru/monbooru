package rank

import "fmt"

// Outcome is which side a vote picked. Skipping a vote is not an Outcome.
// We store the side (rather than just the winner's ID) because in some cases
// we might specifically care about which side was chosen. Users tend to be
// slightly biased to one side (usually the side they see first, which is the
// left side for native English readers because they read from left to
// right), and we might want to compensate for that bias.
type Outcome uint8

const (
	LeftWins  Outcome = 1
	Tie       Outcome = 2 // "about the same"
	RightWins Outcome = 3
	// Adding an Outcome? Update Outcome.Valid, and its tests, to match.
)

// Valid reports whether the Outcome can be stored. Go accepts any number in
// this type, so callers check it where outcomes come in, from a form or
// from the database.
func (o Outcome) Valid() bool {
	return o == LeftWins || o == Tie || o == RightWins
}

// LeftScore is the left image's result as a number: 1 for a win, ½ for "about
// the same" and 0 for a loss. The right image's is 1 − LeftScore. Ties as half
// a win are from Glickman (1999), also used by Weng & Lin (2011, eq. 51) and
// the Glicko-2 paper.
func (o Outcome) LeftScore() (float64, error) {
	switch o {
	case LeftWins:
		return 1, nil
	case Tie:
		return 0.5, nil
	case RightWins:
		return 0, nil
	}
	return 0, fmt.Errorf("invalid outcome %d", o)
}
