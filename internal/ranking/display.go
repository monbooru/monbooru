package ranking

import "math"

// Glicko-2 uses a scale factor of 400 / ln 10 (roughly 173), which is to say
// that a difference of 400 points implies 10-to-1 odds. A post with a rating
// of 1900 is 10x more likely to be picked by the user than an image with a
// ranking of 1500, and 100x more likely to be picked than an image with a
// ranking of 1100. We've decided to apply this scale factor to all algorithms
// as algorithms like Bradley-Terry aren't particularly picky about their scale
// factor, and by picking a consistent scale factor we can make it easier for
// users to compare numbers accross different algorithms.
const (
	// glickoScale is 400 / ln 10, the paper's 173.7178: 400 points is
	// 10-to-1 odds.
	glickoScale = 400 / math.Ln10
	// unrankedRanking is what a strength of 0, a brand-new image, shows.
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
