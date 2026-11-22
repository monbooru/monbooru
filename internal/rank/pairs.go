package rank

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync"
)

// PairInformation is how much a vote between images a and b is expected to
// teach about their strengths, in nats (ln 2 ≈ 0.69 at most, since a vote has
// two outcomes). It's Glickman & Jensen's (2005) expected Kullback–Leibler
// information, their eq. (4), for the Bradley–Terry model:
//
//	E(p ln p) + E(q ln q) − E(p) ln E(p) − E(q) ln E(q)
//
// where p = sigmoid(gap) is the chance that a beats b given their true
// strengths, q = 1 − p, and E averages over what the gap may be, from the two
// estimates: a bell curve centred on the estimated gap, with variance
// uncertainty_a² + uncertainty_b². In words: how unpredictable the vote is,
// minus how unpredictable it would still be if we knew the true gap. It's
// highest for close pairs that we're unsure about, and 0 for two images we
// already know exactly.
//
// Like the paper, it treats a vote as a win or a loss; "about the same" isn't
// part of the score.
func PairInformation(a, b Estimate) float64 {
	nodes, weights := hermiteRule()
	return pairInformation(a, b, nodes, weights)
}

// pairInformation is PairInformation with a given quadrature rule, so the
// tests can compare rules.
func pairInformation(a, b Estimate, nodes, weights []float64) float64 {
	gap := a.Strength - b.Strength
	spread := math.Sqrt(a.Uncertainty*a.Uncertainty + b.Uncertainty*b.Uncertainty)
	if spread == 0 {
		return 0 // the gap is known exactly: the vote can't teach anything
	}
	// Averages over the gap with Gauss–Hermite quadrature, which the paper
	// uses with 30 points: E[f(gap)] = Σ wᵢ f(gap + √2 × spread × xᵢ) / √π.
	var meanP, meanPLogP, meanQLogQ float64
	for i, x := range nodes {
		d := gap + math.Sqrt2*spread*x
		w := weights[i]
		p, q := winChance(d), winChance(-d)
		meanP += w * p
		meanPLogP += w * p * logWinChance(d)
		meanQLogQ += w * q * logWinChance(-d)
	}
	meanP /= math.SqrtPi
	meanPLogP /= math.SqrtPi
	meanQLogQ /= math.SqrtPi
	meanQ := 1 - meanP
	info := meanPLogP + meanQLogQ - xLogX(meanP) - xLogX(meanQ)
	return max(info, 0) // never below 0 (Jensen's inequality); guards rounding
}

// winChance is sigmoid(d): the chance of winning with a strength gap of d.
func winChance(d float64) float64 { return 1 / (1 + math.Exp(-d)) }

// logWinChance is ln(sigmoid(d)), written so a huge gap gives a finite answer.
func logWinChance(d float64) float64 {
	if d >= 0 {
		return -math.Log1p(math.Exp(-d))
	}
	return d - math.Log1p(math.Exp(d))
}

// xLogX is x ln x, with 0 ln 0 = 0.
func xLogX(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return x * math.Log(x)
}

// The paper: "We have found that using 30 grid points for Gauss–Hermite
// quadrature applied to functions of Bradley–Terry probabilities is
// sufficiently accurate".
const hermitePoints = 30

// hermiteRule returns the 30-point Gauss–Hermite rule's nodes and weights:
// ∫ f(x) e^(−x²) dx ≈ Σ wᵢ f(xᵢ), exact when f is a polynomial of degree up
// to 59. Computed on first use, then reused.
var hermiteRule = sync.OnceValues(func() (nodes, weights []float64) {
	return gaussHermite(hermitePoints)
})

// gaussHermite computes the n-point rule. The nodes are the roots of the
// n-th Hermite polynomial, found by scanning for sign changes and refining
// each with Newton's method. Each weight is the Christoffel number
// 1 / Σₖ₍ₖ₌₀…ₙ₋₁₎ pₖ(x)², where pₖ are the Hermite polynomials scaled to be
// orthonormal for the weight e^(−x²).
func gaussHermite(n int) (nodes, weights []float64) {
	// orthonormal returns pₙ(x), pₙ₋₁(x), and Σₖ₍ₖ<ₙ₎ pₖ(x)², from the
	// recurrence p₀ = π^(−1/4), pₖ₊₁ = √(2/(k+1)) x pₖ − √(k/(k+1)) pₖ₋₁.
	orthonormal := func(x float64) (pn, pnMinus1, sumSquares float64) {
		prev, cur := 0.0, math.Pow(math.Pi, -0.25)
		for k := 0; k < n; k++ {
			sumSquares += cur * cur
			next := math.Sqrt(2/float64(k+1))*x*cur - math.Sqrt(float64(k)/float64(k+1))*prev
			prev, cur = cur, next
		}
		return cur, prev, sumSquares
	}
	// All roots lie within ±√(2n + 1).
	limit := math.Sqrt(float64(2*n+1)) + 0.5
	const step = 1e-3
	prevValue, _, _ := orthonormal(-limit)
	for x := -limit + step; x <= limit; x += step {
		value, _, _ := orthonormal(x)
		if value == 0 || math.Signbit(value) != math.Signbit(prevValue) {
			root := x
			for range 50 {
				pn, pnMinus1, _ := orthonormal(root)
				// pₙ'(x) = √(2n) pₙ₋₁(x) for these orthonormal polynomials.
				delta := pn / (math.Sqrt(2*float64(n)) * pnMinus1)
				root -= delta
				if math.Abs(delta) < 1e-15 {
					break
				}
			}
			nodes = append(nodes, root)
		}
		prevValue = value
	}
	if len(nodes) != n {
		panic("gaussHermite: found the wrong number of roots") // a bug, caught by TestGaussHermitePoints
	}
	slices.Sort(nodes)
	// Mirror-average so the rule is exactly symmetric.
	for i := range nodes[:n/2] {
		j := n - 1 - i
		m := (nodes[j] - nodes[i]) / 2
		nodes[i], nodes[j] = -m, m
	}
	weights = make([]float64, len(nodes))
	for i, x := range nodes {
		_, _, sumSquares := orthonormal(x)
		weights[i] = 1 / sumSquares
	}
	return nodes, weights
}

// pickTieTolerance is how close a score must be to the best to count as tied
// with it. Only enough to absorb rounding: scores are at most ln 2 ≈ 0.69,
// and two pairs that are equally informative can differ in the last few
// digits depending on how their numbers were computed.
const pickTieTolerance = 1e-12

// PickBest returns the index of the highest score, the most informative pair,
// as Glickman & Jensen pick. Pairs tied for the best are chosen between at
// random: before anything is ranked every pair of new images ties, so the
// first pairs come from anywhere in the search, in a different order each
// time. Scores that aren't numbers (NaN) are never picked. It returns −1 when
// there's nothing to pick.
func PickBest(scores []float64, rng *rand.Rand) int {
	best := math.Inf(-1)
	for _, s := range scores {
		if s > best {
			best = s
		}
	}
	// One pass, keeping each tied pair with chance 1/(ties so far), gives
	// every tied pair the same chance without making a list of them.
	chosen, ties := -1, 0
	for i, s := range scores {
		if s >= best-pickTieTolerance {
			ties++
			if rng.IntN(ties) == 0 {
				chosen = i
			}
		}
	}
	return chosen
}
