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

func TestResolve(t *testing.T) {
	// Default follows the version's recommendation, which must be a real
	// algorithm.
	got := Resolve(AlgorithmDefault)
	if got != recommended {
		t.Errorf("Resolve(AlgorithmDefault) = %d, want recommended (%d)", got, recommended)
	}
	if got == AlgorithmDefault || !got.Valid() {
		t.Errorf("Resolve(AlgorithmDefault) = %d, want a real algorithm", got)
	}
	// An explicit choice is never switched.
	for _, id := range []AlgorithmID{AlgorithmBradleyTerry, AlgorithmGlicko2} {
		if got := Resolve(id); got != id {
			t.Errorf("Resolve(%d) = %d, want %d", id, got, id)
		}
	}
}
