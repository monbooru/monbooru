package rank

import "math"

// Glicko-2 uses a scale factor of 400 / ln 10 (roughly 173.7), which is to say
// that a difference of 400 points implies 10-to-1 odds. An image with a rank
// of 1900 is 10x more likely to be picked by the user than an image with a
// rank of 1500, and 100x more likely to be picked than an image with a
// rank of 1100. We've decided to apply this scale factor to Bradley-Terry, as
// it doesn't have a set scale factor of its own, and using a shared scale
// factor means it'll be easier for users with an intuition of which Glicko-2
// ranks are high/low to transition to our Bradley-Terry ranks.
const (
	pointsPerStrength = 400 / math.Ln10
	// This is the rank of a brand new image (with a strength of 0). Note
	// that some places show a lower value, where a rank of 1200 might be
	// considered high. We use the value given by the paper.
	newImageRank = 1500
)

// RankFromStrength is the number shown for a strength. The database
// keeps the strength, so the scale can change without rewriting rows.
func RankFromStrength(strength float64) float64 {
	return newImageRank + pointsPerStrength*strength
}

// StrengthFromRank turns a typed rank, as in rank:>1700, back into the
// strength the query compares against.
func StrengthFromRank(rank float64) float64 {
	return (rank - newImageRank) / pointsPerStrength
}

// RDFromUncertainty is an uncertainty in display points, Glicko-2's
// "rating deviation".
func RDFromUncertainty(uncertainty float64) float64 {
	return pointsPerStrength * uncertainty
}

// UncertaintyFromRD is the inverse of RDFromUncertainty.
func UncertaintyFromRD(rd float64) float64 {
	return rd / pointsPerStrength
}
