// Package algorithms looks up a ranking algorithm by its ID. It lives apart
// from package ranking because it imports every algorithm, and every
// algorithm imports ranking: Go doesn't allow that loop.
package algorithms

import (
	"fmt"

	"github.com/monbooru/monbooru/internal/ranking"
	"github.com/monbooru/monbooru/internal/ranking/bradleyterry"
	"github.com/monbooru/monbooru/internal/ranking/glicko2"
)

// New returns the algorithm id names, with its default settings.
// ranking.AlgorithmDefault gives this version's recommendation. An unknown
// id is an error.
func New(id ranking.AlgorithmID) (ranking.Algorithm, error) {
	switch ranking.Resolve(id) {
	case ranking.AlgorithmBradleyTerry:
		return bradleyterry.New(bradleyterry.DefaultSettings())
	case ranking.AlgorithmGlicko2:
		return glicko2.New(glicko2.DefaultSettings())
		// Adding an algorithm? Add it here, and to ranking.AlgorithmID.Valid;
		// TestNewMatchesValid checks that the two agree.
	}
	return nil, fmt.Errorf("unknown ranking algorithm %d", id)
}
