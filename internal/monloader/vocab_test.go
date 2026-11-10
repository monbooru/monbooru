package monloader

import (
	"testing"

	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestOutcome(t *testing.T) {
	for _, tc := range []struct {
		state     string
		result    string
		concluded bool
	}{
		{StatePending, "", false},
		{StateOK, "", false},
		{StateHashNotFound, lookup.ResultMiss, true},
		{StateCanceled, lookup.ResultError, true},
		{contract.CodeRateLimited, lookup.ResultError, true},
		{"ptr_syncing", lookup.ResultError, true},
	} {
		result, concluded := Outcome(tc.state)
		if result != tc.result || concluded != tc.concluded {
			t.Errorf("Outcome(%q) = %q, %v; want %q, %v", tc.state, result, concluded, tc.result, tc.concluded)
		}
	}
}
