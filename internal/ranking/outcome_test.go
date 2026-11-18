package ranking

import "testing"

// The numbers are stored in ranking_votes, so changing one would change
// what every stored vote means.
func TestOutcomeStoredValues(t *testing.T) {
	cases := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"WinnerLeft", uint8(WinnerLeft), 1},
		{"WinnerTie", uint8(WinnerTie), 2},
		{"WinnerRight", uint8(WinnerRight), 3},
		{"DegreeUnspecified", uint8(DegreeUnspecified), 0},
		{"DegreeSlight", uint8(DegreeSlight), 1},
		{"DegreeModerate", uint8(DegreeModerate), 2},
		{"DegreeStrong", uint8(DegreeStrong), 3},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestOutcomeValid(t *testing.T) {
	valid := map[Outcome]bool{
		{WinnerLeft, DegreeUnspecified}:  true,
		{WinnerLeft, DegreeSlight}:       true,
		{WinnerLeft, DegreeModerate}:     true,
		{WinnerLeft, DegreeStrong}:       true,
		{WinnerTie, DegreeUnspecified}:   true,
		{WinnerRight, DegreeUnspecified}: true,
		{WinnerRight, DegreeSlight}:      true,
		{WinnerRight, DegreeModerate}:    true,
		{WinnerRight, DegreeStrong}:      true,
	}
	// 0 and 4 sit just outside each type's range, so unknown values are
	// covered on both sides.
	for w := range 5 {
		for d := range 5 {
			o := Outcome{Winner: Winner(w), Degree: Degree(d)}
			if got := o.Valid(); got != valid[o] {
				t.Errorf("Outcome{Winner: %d, Degree: %d}.Valid() = %v, want %v", w, d, got, valid[o])
			}
		}
	}
	// Testing far outside each type's range.
	for _, o := range []Outcome{{255, DegreeUnspecified}, {WinnerRight, 255}} {
		if o.Valid() {
			t.Errorf("Outcome{Winner: %d, Degree: %d}.Valid() = true, want false", o.Winner, o.Degree)
		}
	}
}

func TestOutcomeLeftScore(t *testing.T) {
	cases := []struct {
		outcome Outcome
		want    float64
	}{
		{Outcome{WinnerLeft, DegreeUnspecified}, 1},
		{Outcome{WinnerTie, DegreeUnspecified}, 0.5},
		{Outcome{WinnerRight, DegreeUnspecified}, 0},
		// Degree doesn't change the score.
		{Outcome{WinnerLeft, DegreeStrong}, 1},
		{Outcome{WinnerRight, DegreeSlight}, 0},
	}
	for _, c := range cases {
		got, err := c.outcome.LeftScore()
		if err != nil || got != c.want {
			t.Errorf("%+v.LeftScore() = %v, %v; want %v, nil", c.outcome, got, err, c.want)
		}
	}
	for _, o := range []Outcome{{0, DegreeUnspecified}, {WinnerTie, DegreeStrong}, {WinnerLeft, 4}} {
		if _, err := o.LeftScore(); err == nil {
			t.Errorf("%+v.LeftScore() succeeded, want an error", o)
		}
	}
}
