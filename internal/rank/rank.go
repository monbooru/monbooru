// Package rank turns pairwise votes ("I like this one more") into a rank
// per image, with the Bradley–Terry model (Bradley & Terry 1952): an image
// with strength a beats one with strength b with chance sigmoid(a − b).
// Each vote updates its two images (Weng & Lin 2011), and Fit recomputes
// everything from all votes. Math only: no database, routes or UI.
package rank

// Estimate is what's known about one image.
type Estimate struct {
	// Strength is the best guess of how much the image is liked; 0 is a
	// brand-new image, and a gap of 1 is 73% to 27% odds.
	Strength float64
	// Uncertainty is how unsure that guess is, as a standard deviation in
	// strength units.
	Uncertainty float64
}

// Vote is one stored comparison.
type Vote struct {
	Left, Right int64 // image ids
	Outcome     Outcome
}
