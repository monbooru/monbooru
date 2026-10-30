// Package lookup holds the scheduled hash lookup's retry ladders, due
// predicate and attempt history.
package lookup

import (
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

// Backends lists the history rows a lookup on monloader's backend covers.
func Backends(name string) []string {
	if name == contract.LookupAll {
		return []string{contract.LookupPTR, contract.LookupBooru}
	}
	return []string{name}
}

const (
	ResultHit   = "hit"
	ResultMiss  = "miss"
	ResultError = "error"
)

// Fixed on purpose: the only operator knob is monloader's budget. The
// free PTR never gives up; the online ladder stops after about 31 weeks
// so the budget goes to images that can still match.
var (
	PTRLadder   = []time.Duration{week, 2 * week, 4 * week}
	BooruLadder = []time.Duration{week, 2 * week, 4 * week, 8 * week, 16 * week}
)

const week = 7 * 24 * time.Hour

const BooruMaxAttempts = 6

// PTRCursor is monloader's last reported index cursor, zero until a probe
// lands.
var PTRCursor atomic.Uint64

func NextDue(now time.Time, backend string, attempts int) (time.Time, bool) {
	ladder := PTRLadder
	if backend == contract.LookupBooru {
		if attempts >= BooruMaxAttempts {
			return time.Time{}, false
		}
		ladder = BooruLadder
	}
	rung := min(max(attempts, 1), len(ladder)) - 1
	return now.Add(ladder[rung]), true
}

// The schema's timestamp format, so SQL can compare these as strings.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
