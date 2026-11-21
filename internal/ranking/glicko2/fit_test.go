package glicko2

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/ranking"
)

func vote(left, right int64, o ranking.Outcome, at time.Time) ranking.Vote {
	return ranking.Vote{Left: left, Right: right, Outcome: o, At: at}
}

// timedVotes spans three weekly periods, with an empty week in between, and
// images that sit some periods out.
func timedVotes() []ranking.Vote {
	week := func(n int, hour int) time.Time { return monday.AddDate(0, 0, 7*n).Add(time.Duration(hour) * time.Hour) }
	return []ranking.Vote{
		vote(1, 2, leftWins, week(0, 1)), vote(2, 3, tie, week(0, 2)), vote(3, 1, rightWins, week(0, 30)),
		vote(4, 1, leftWins, week(0, 100)),
		// week 1: no votes
		vote(2, 4, rightWins, week(2, 5)), vote(1, 2, leftWins, week(2, 6)), vote(5, 4, tie, week(2, 7)),
		vote(3, 5, leftWins, week(3, 1)), vote(1, 5, leftWins, week(3, 2)),
	}
}

func fit(t *testing.T, alg *algorithm, votes []ranking.Vote) map[int64]state {
	t.Helper()
	got, err := alg.Fit(context.Background(), votes)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	out := make(map[int64]state, len(got))
	for id, s := range got {
		out[id] = s.(state)
	}
	return out
}

// Fit is a replay: the same as applying every vote with Update, in time
// order, then bringing every image up to the last vote's period.
func TestFitReplaysTheVotes(t *testing.T) {
	alg := newTest(t)
	votes := timedVotes()
	want := map[int64]state{}
	get := func(id int64) state {
		if s, ok := want[id]; ok {
			return s
		}
		return alg.Start().(state)
	}
	for _, v := range votes {
		want[v.Left], want[v.Right] = update(t, alg, get(v.Left), get(v.Right), v.Outcome, v.At)
	}
	last := votes[len(votes)-1].At
	for id, s := range want {
		want[id] = alg.advance(s, last)
	}

	got := fit(t, alg, votes)
	if len(got) != len(want) {
		t.Errorf("Fit returned %d images, want %d", len(got), len(want))
	}
	for id, w := range want {
		checkState(t, "image", got[id], w)
	}
	// Image 4 last played in week 2, so it's closed and in week 3's period.
	if got[4].PeriodGames != 0 || !got[4].PeriodStart.Equal(monday.AddDate(0, 0, 21)) {
		t.Errorf("image 4 = %+v, want an empty period starting in week 3", got[4])
	}
}

// Fit goes by each vote's time, not by its place in the list.
func TestFitIgnoresListOrder(t *testing.T) {
	alg := newTest(t)
	votes := timedVotes()
	want := fit(t, alg, votes)
	rng := rand.New(rand.NewPCG(502, 754))
	for range 5 {
		shuffled := append([]ranking.Vote(nil), votes...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := fit(t, alg, shuffled)
		for id, w := range want {
			checkState(t, "image", got[id], w)
		}
	}
}

// Votes in the same period can be in any order, even with the same time.
func TestFitSameTimeVotes(t *testing.T) {
	alg := newTest(t)
	at := monday.Add(time.Hour)
	a := []ranking.Vote{vote(1, 2, leftWins, at), vote(2, 3, leftWins, at), vote(3, 1, tie, at)}
	b := []ranking.Vote{a[2], a[0], a[1]}
	got, want := fit(t, alg, b), fit(t, alg, a)
	for id, w := range want {
		checkState(t, "image", got[id], w)
	}
}

// Moving the votes of one period to other days of the same week changes
// nothing: only which period a vote falls in matters.
func TestFitOnlyThePeriodMatters(t *testing.T) {
	alg := newTest(t)
	votes := timedVotes()
	moved := append([]ranking.Vote(nil), votes...)
	for i := range moved {
		moved[i].At = periodStart(moved[i].At, DefaultSettings().PeriodLength).Add(time.Duration(i) * time.Minute)
	}
	got, want := fit(t, alg, moved), fit(t, alg, votes)
	for id, w := range want {
		checkState(t, "image", got[id], w)
	}
}

func TestFitRejectsBadVotes(t *testing.T) {
	alg := newTest(t)
	cases := map[string][]ranking.Vote{
		"image against itself": {vote(1, 2, leftWins, monday), vote(3, 3, leftWins, monday)},
		"no winner":            {vote(1, 2, ranking.Outcome{}, monday)},
		"no time":              {vote(1, 2, leftWins, time.Time{})},
	}
	for name, votes := range cases {
		if _, err := alg.Fit(context.Background(), votes); err == nil {
			t.Errorf("%s: Fit succeeded, want an error", name)
		}
	}
}

func TestFitEmptyAndCancelled(t *testing.T) {
	alg := newTest(t)
	empty, err := alg.Fit(context.Background(), nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("Fit(no votes) = %v, %v; want an empty map and no error", empty, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := alg.Fit(ctx, timedVotes()); !errors.Is(err, context.Canceled) {
		t.Errorf("Fit with a cancelled context: err = %v, want context.Canceled", err)
	}
}

// What Fit returns is a normal state: Shown, Encode and Update take it.
func TestFitResultsAreUsable(t *testing.T) {
	alg := newTest(t)
	got, err := alg.Fit(context.Background(), timedVotes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alg.Encode(got[1]); err != nil {
		t.Errorf("Encode(Fit result): %v", err)
	}
	if _, _, err := alg.Update(got[1], got[2], leftWins, monday.AddDate(0, 0, 30)); err != nil {
		t.Errorf("Update(Fit results): %v", err)
	}
}
