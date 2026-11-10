package web

import (
	"testing"
	"time"
)

func TestReclaimDue(t *testing.T) {
	cases := []struct {
		name     string
		jobEnded bool
		since    time.Duration
		want     bool
	}{
		{"idle inside the interval waits", false, time.Minute, false},
		{"idle past the interval reclaims", false, reclaimInterval, true},
		{"a finished job reclaims immediately", true, time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reclaimDue(tc.jobEnded, tc.since); got != tc.want {
				t.Errorf("reclaimDue = %v, want %v", got, tc.want)
			}
		})
	}
}
