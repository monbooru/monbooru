package glicko2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

// fitCheckEvery is how many votes Fit replays between checks for
// cancellation.
const fitCheckEvery = 1024

// Fit recomputes every voted image by replaying all votes, in time order,
// through Update, then bringing every image up to the last vote's period.
// That's the paper's method applied to every rating period in turn, so it
// gives exactly what voting one at a time did. It's needed when votes in
// closed periods change (undo of an older vote, forget). Images without
// votes aren't in the result.
func (alg *algorithm) Fit(ctx context.Context, votes []ranking.Vote) (map[int64]ranking.State, error) {
	// Sort by time. Stable, so votes with the same time keep their order.
	order := make([]int, len(votes))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int { return votes[i].At.Compare(votes[j].At) })

	states := map[int64]state{}
	get := func(id int64) state {
		if s, ok := states[id]; ok {
			return s
		}
		return alg.Start().(state)
	}
	var last time.Time
	for n, k := range order {
		if n%fitCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		v := votes[k]
		if v.Left == v.Right {
			return nil, fmt.Errorf("image %d is compared with itself", v.Left)
		}
		l, r, err := alg.Update(get(v.Left), get(v.Right), v.Outcome, v.At)
		if err != nil {
			return nil, fmt.Errorf("vote between images %d and %d: %w", v.Left, v.Right, err)
		}
		states[v.Left], states[v.Right] = l.(state), r.(state)
		last = v.At
	}

	out := make(map[int64]ranking.State, len(states))
	for id, s := range states {
		out[id] = alg.advance(s, last)
	}
	return out, nil
}
