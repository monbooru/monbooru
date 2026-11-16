package ranking

// Winner is which side a vote picked. The values are stored in
// ranking_votes, so they are written out and never reordered.
type Winner uint8

const (
	WinnerLeft  Winner = 1
	WinnerTie   Winner = 2 // "about the same"
	WinnerRight Winner = 3
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
)

// Outcome is what a vote recorded.
type Outcome struct {
	Winner Winner
	Degree Degree
}

// Valid reports whether o can be stored: a known winner and degree, and a
// tie with no degree. Go accepts any number in these types, so callers
// check it where outcomes come in, from a form or from the database.
func (o Outcome) Valid() bool {
	switch o.Winner {
	case WinnerLeft, WinnerRight:
		return o.Degree <= DegreeStrong
	case WinnerTie:
		return o.Degree == DegreeUnspecified
	}
	return false
}
