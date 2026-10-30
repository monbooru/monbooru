package monloader

import (
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// Fetch states a detail page's fetch status passes through: pending and
// ok are monbooru's own, the rest arrive from monloader's fetch-status
// report, a failure as one of its queue error codes.
const (
	StatePending       = "pending"
	StateOK            = "ok"
	StateHashNotFound  = contract.CodeHashNotFound
	StateCanceled      = contract.CodeCanceled
	StateAlreadyExists = contract.CodeAlreadyExists
)

// Outcome reads a fetch state as a lookup result. Only hash_not_found says
// anything about the image.
func Outcome(state string) (result string, concluded bool) {
	switch state {
	case StatePending, StateOK:
		return "", false
	case StateHashNotFound:
		return lookup.ResultMiss, true
	}
	return lookup.ResultError, true
}
