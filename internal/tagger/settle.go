package tagger

import (
	"context"
	"fmt"

	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
)

// Settle ends an autotag job; finished reports a run that went to the end,
// whose peak memory the caller may log.
func Settle(ctx context.Context, mgr *jobs.Manager, prefix, itemNoun string, total, skipped int, err error) (finished bool, _ error) {
	done := fmt.Sprintf("%sauto-tagged %d %s%s", prefix, total, itemNoun, models.Plural(total, "image", "images"))
	if skipped > 0 {
		done = fmt.Sprintf("%sauto-tagged %d of %d %s%s, %d skipped", prefix, total-skipped, total, itemNoun, models.Plural(total, "image", "images"), skipped)
	}
	finished = ctx.Err() == nil && err == nil
	return finished, settler(mgr, skipped)(ctx, err, fmt.Sprintf("%sauto-tagging cancelled (%d %s queued)", prefix, total, models.Plural(total, "image", "images")), done)
}

func SettleImage(ctx context.Context, mgr *jobs.Manager, id int64, skipped int, err error) (finished bool, _ error) {
	done := fmt.Sprintf("auto-tagged image #%d", id)
	if skipped > 0 {
		done = fmt.Sprintf("auto-tagger skipped image #%d", id)
	}
	finished = ctx.Err() == nil && err == nil
	return finished, settler(mgr, skipped)(ctx, err, "auto-tagging cancelled", done)
}

func settler(mgr *jobs.Manager, skipped int) func(context.Context, error, string, string) error {
	if skipped > 0 {
		return mgr.SettlePartial
	}
	return mgr.Settle
}
