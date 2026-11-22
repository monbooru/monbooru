package rank

import "testing"

// The numbers are stored with every vote, so changing one would change what
// every stored vote means.
func TestOutcomeStoredValues(t *testing.T) {
	cases := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"LeftWins", uint8(LeftWins), 1},
		{"Tie", uint8(Tie), 2},
		{"RightWins", uint8(RightWins), 3},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestOutcomeValid(t *testing.T) {
	valid := map[Outcome]bool{LeftWins: true, Tie: true, RightWins: true}
	// Every value an Outcome can hold, so unknown values on both sides of the
	// known ones are covered.
	for n := range 256 {
		o := Outcome(n)
		if got := o.Valid(); got != valid[o] {
			t.Errorf("Outcome(%d).Valid() = %v, want %v", n, got, valid[o])
		}
	}
}

func TestOutcomeLeftScore(t *testing.T) {
	cases := []struct {
		outcome Outcome
		want    float64
	}{
		{LeftWins, 1},
		{Tie, 0.5},
		{RightWins, 0},
	}
	for _, c := range cases {
		got, err := c.outcome.LeftScore()
		if err != nil || got != c.want {
			t.Errorf("Outcome(%d).LeftScore() = %v, %v; want %v, nil", c.outcome, got, err, c.want)
		}
	}
	for _, o := range []Outcome{0, 4, 255} {
		if _, err := o.LeftScore(); err == nil {
			t.Errorf("Outcome(%d).LeftScore() succeeded, want an error", o)
		}
	}
}
