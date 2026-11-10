package lookup

import (
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestNextDueWalksTheLadder(t *testing.T) {
	now := time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		backend  string
		attempts int
		want     time.Duration
		ok       bool
	}{
		{contract.LookupPTR, 1, week, true},
		{contract.LookupPTR, 2, 2 * week, true},
		{contract.LookupPTR, 3, 4 * week, true},
		{contract.LookupPTR, 9, 4 * week, true},
		{contract.LookupBooru, 1, week, true},
		{contract.LookupBooru, 2, 2 * week, true},
		{contract.LookupBooru, 5, 16 * week, true},
	} {
		got, ok := NextDue(now, tc.backend, tc.attempts)
		if ok != tc.ok {
			t.Errorf("%s miss %d: ok = %v, want %v", tc.backend, tc.attempts, ok, tc.ok)
			continue
		}
		if want := now.Add(tc.want); !got.Equal(want) {
			t.Errorf("%s miss %d: due %s, want %s", tc.backend, tc.attempts, got, want)
		}
	}
}

func TestBooruLadderGivesUp(t *testing.T) {
	now := time.Now()
	if _, ok := NextDue(now, contract.LookupBooru, BooruMaxAttempts); ok {
		t.Errorf("the online ladder should stop at %d misses", BooruMaxAttempts)
	}
	var total time.Duration
	for _, d := range BooruLadder {
		total += d
	}
	if total < 30*week || total > 32*week {
		t.Errorf("the online ladder spans %s, want about 31 weeks", total)
	}
}

func TestDueClausePTRGatesOnTheCursor(t *testing.T) {
	PTRCursor.Store(0)
	t.Cleanup(func() { PTRCursor.Store(0) })

	cold, args := DueClause(contract.LookupPTR, time.Now())
	if len(args) != 2 {
		t.Errorf("cold-cursor args = %v, want just the backend and the time gate", args)
	}
	if got := len(cold); got == 0 {
		t.Fatal("empty clause")
	}

	PTRCursor.Store(4812)
	warm, args := DueClause(contract.LookupPTR, time.Now())
	if len(args) != 3 || args[2] != int64(4812) {
		t.Errorf("warm-cursor args = %v, want the cursor bound last", args)
	}
	if warm == cold {
		t.Error("a known cursor should add the index gate to the clause")
	}

	if _, args := DueClause(contract.LookupBooru, time.Now()); len(args) != 2 {
		t.Errorf("booru args = %v, want no cursor gate", args)
	}
}
