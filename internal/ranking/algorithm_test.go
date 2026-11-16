package ranking

import "testing"

// Stored in ranking_categories and image_rankings, like Outcome.
func TestAlgorithmIDStoredValues(t *testing.T) {
	cases := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"AlgorithmDefault", uint8(AlgorithmDefault), 0},
		{"AlgorithmBradleyTerry", uint8(AlgorithmBradleyTerry), 1},
		{"AlgorithmGlicko2", uint8(AlgorithmGlicko2), 2},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestAlgorithmIDValid(t *testing.T) {
	cases := []struct {
		id   AlgorithmID
		want bool
	}{
		{AlgorithmDefault, true},
		{AlgorithmBradleyTerry, true},
		{AlgorithmGlicko2, true},
		{3, false},
		{255, false},
	}
	for _, c := range cases {
		if got := c.id.Valid(); got != c.want {
			t.Errorf("AlgorithmID(%d).Valid() = %v, want %v", c.id, got, c.want)
		}
	}
}

// Default follows the version's recommendation; an explicit choice is
// never switched.
func TestResolve(t *testing.T) {
	cases := []struct {
		id, want AlgorithmID
	}{
		{AlgorithmDefault, AlgorithmBradleyTerry},
		{AlgorithmBradleyTerry, AlgorithmBradleyTerry},
		{AlgorithmGlicko2, AlgorithmGlicko2},
	}
	for _, c := range cases {
		if got := Resolve(c.id); got != c.want {
			t.Errorf("Resolve(%d) = %d, want %d", c.id, got, c.want)
		}
	}
}
