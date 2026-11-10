package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestSettleAndFinish_Precedence(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name                     string
		err                      error
		cancelled                bool
		settleSummary, settleErr string
		finishSummary, finishErr string
	}{
		{"done", nil, false, "done", "", "done", ""},
		{"error", boom, false, "", "boom", "", "boom"},
		{"late cancel", nil, true, "cancelled", "", "cancelled", ""},
		{"cancel and error", boom, true, "cancelled", "", "", "boom"},
		{"wrapped canceled", fmt.Errorf("walk: %w", context.Canceled), false, "cancelled", "", "", "walk: context canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}

			m := NewManager()
			mustStart(t, m, "sync")
			_ = m.Settle(ctx, tc.err, "cancelled", "done")
			if s := m.Get(); s.Summary != tc.settleSummary || s.Error != tc.settleErr {
				t.Errorf("Settle: summary %q error %q, want %q %q", s.Summary, s.Error, tc.settleSummary, tc.settleErr)
			}

			m = NewManager()
			mustStart(t, m, "sync")
			m.Finish(tc.err, tc.cancelled, "cancelled", "done")
			if s := m.Get(); s.Summary != tc.finishSummary || s.Error != tc.finishErr {
				t.Errorf("Finish: summary %q error %q, want %q %q", s.Summary, s.Error, tc.finishSummary, tc.finishErr)
			}
		})
	}
}
