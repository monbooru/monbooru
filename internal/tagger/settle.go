package tagger

import (
	"context"
	"fmt"
	"time"

	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/models"
)

// Settle ends an autotag job; finished reports a run that went to the end,
// whose peak memory the caller may log.
func Settle(ctx context.Context, mgr *jobs.Manager, prefix, itemNoun string, total int, res Result, err error) (finished bool, _ error) {
	skipped := res.Skipped
	done := fmt.Sprintf("%sauto-tagged %d %s%s", prefix, total, itemNoun, models.Plural(total, "image", "images"))
	if skipped > 0 {
		done = fmt.Sprintf("%sauto-tagged %d of %d %s%s, %d skipped", prefix, total-skipped, total, itemNoun, models.Plural(total, "image", "images"), skipped)
	}
	finished = ctx.Err() == nil && err == nil
	return finished, settler(mgr, skipped)(ctx, err, fmt.Sprintf("%sauto-tagging cancelled (%d %s queued)", prefix, total, models.Plural(total, "image", "images")), done+res.shareLine()+res.took())
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

// shareLine names each worker's images once two tagged, and the one
// worker too when it was started for the job.
func (r Result) shareLine() string {
	if r.Took > 0 {
		return r.Workers()
	}
	return r.Shares()
}

// took is the job's time in the end line, " in 1 min 12 s", when a worker
// started for it took part.
func (r Result) took() string {
	if r.Took <= 0 {
		return ""
	}
	return " in " + Spell(r.Took)
}

// Spell writes a duration the way the status bar does: "34 s", "1 min 12 s".
func Spell(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	return fmt.Sprintf("%d min %d s", s/60, s%60)
}
