package algorithms

import (
	"testing"

	"github.com/monbooru/monbooru/internal/ranking"
)

// An AlgorithmID is a uint8, so all 256 values can be checked: New must
// succeed exactly for the IDs that AlgorithmID.Valid accepts. This catches a
// new algorithm added to one but not the other, in either direction.
func TestNewMatchesValid(t *testing.T) {
	for n := range 256 {
		id := ranking.AlgorithmID(n)
		alg, err := New(id)
		if (err == nil) != id.Valid() {
			t.Errorf("New(%d) error = %v, but AlgorithmID(%d).Valid() = %v", id, err, id, id.Valid())
			continue
		}
		if err == nil && alg.ID() != ranking.Resolve(id) {
			t.Errorf("New(%d).ID() = %d, want %d", id, alg.ID(), ranking.Resolve(id))
		}
	}
}

func TestNewNamesEachAlgorithm(t *testing.T) {
	cases := []struct {
		id   ranking.AlgorithmID
		name string
	}{
		{ranking.AlgorithmBradleyTerry, "bradley-terry"},
		{ranking.AlgorithmGlicko2, "glicko-2"},
	}
	for _, c := range cases {
		alg, err := New(c.id)
		if err != nil || alg.Name() != c.name {
			t.Errorf("New(%d) = %v, %v; want %q", c.id, alg, err, c.name)
		}
	}
	// Default follows the recommendation.
	def, err := New(ranking.AlgorithmDefault)
	if err != nil || def.ID() != ranking.Resolve(ranking.AlgorithmDefault) {
		t.Errorf("New(AlgorithmDefault) = %v, %v; want the recommended algorithm", def, err)
	}
}
