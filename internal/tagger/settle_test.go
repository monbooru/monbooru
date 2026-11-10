package tagger

import (
	"context"
	"errors"
	"testing"

	"github.com/monbooru/monbooru/internal/jobs"
)

func TestSettleMessages(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name                 string
		cancelled            bool
		skipped              int
		err                  error
		finished, partial    bool
		summary, image, fail string
	}{
		{"clean", false, 0, nil, true, false, "x: auto-tagged 3 page images", "auto-tagged image #7", ""},
		{"skipped", false, 1, nil, true, true, "x: auto-tagged 2 of 3 page images, 1 skipped", "auto-tagger skipped image #7", ""},
		{"error", false, 0, boom, false, false, "", "", "boom"},
		{"cancel outranks the error", true, 1, boom, false, false, "x: auto-tagging cancelled (3 images queued)", "auto-tagging cancelled", ""},
	}
	for _, tc := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		if tc.cancelled {
			cancel()
		}
		for _, run := range []struct {
			summary string
			settle  func(*jobs.Manager) (bool, error)
		}{
			{tc.summary, func(m *jobs.Manager) (bool, error) { return Settle(ctx, m, "x: ", "page ", 3, tc.skipped, tc.err) }},
			{tc.image, func(m *jobs.Manager) (bool, error) { return SettleImage(ctx, m, 7, tc.skipped, tc.err) }},
		} {
			m := jobs.NewManager()
			if err := m.Start("autotag"); err != nil {
				t.Fatal(err)
			}
			finished, err := run.settle(m)
			if s := m.Get(); finished != tc.finished || s.Summary != run.summary || s.Partial != tc.partial || s.Error != tc.fail || (err != nil) != (tc.fail != "") {
				t.Errorf("%s: finished %v err %v summary %q partial %v error %q, want %v %q %v %q", tc.name, finished, err, s.Summary, s.Partial, s.Error, tc.finished, run.summary, tc.partial, tc.fail)
			}
		}
		cancel()
	}
}
