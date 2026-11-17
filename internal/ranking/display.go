package ranking

import "math"

// Glicko-2 uses a scale factor of 400 / ln 10 (roughly 173.7), which is to say
// that a difference of 400 points implies 10-to-1 odds. An image with a rating
// of 1900 is 10x more likely to be picked by the user than an image with a
// ranking of 1500, and 100x more likely to be picked than an image with a
// ranking of 1100. We've decided to apply this scale factor to all algorithms,
// as algorithms like Bradley-Terry don't have a set scale factor of their own,
// and picking a consistent scale factor means it'll be easier for users to
// build an intuition for what rankings are high or low.
const (
	// See comment above
	glickoScale = 400 / math.Ln10
	// This is the ranking of a brand new image (with a strength of 0). Note
	// that some places show a lower value, where a ranking of 1200 might be
	// considered high. We use the value given by the paper.
	unrankedRanking = 1500
)

// RankingFromStrength is the number shown for a strength. The database
// keeps the strength, so the scale can change without rewriting rows.
func RankingFromStrength(strength float64) float64 {
	return unrankedRanking + glickoScale*strength
}

// StrengthFromRanking turns a typed ranking, as in ranking:>1700, back
// into the strength the query compares against.
func StrengthFromRanking(ranking float64) float64 {
	return (ranking - unrankedRanking) / glickoScale
}

// RDFromUncertainty is an uncertainty in display points, Glicko-2's
// "rating deviation".
func RDFromUncertainty(uncertainty float64) float64 {
	return glickoScale * uncertainty
}

// UncertaintyFromRD is the inverse of RDFromUncertainty.
func UncertaintyFromRD(rd float64) float64 {
	return rd / glickoScale
}
