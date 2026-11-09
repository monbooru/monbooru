package monloader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func LookupMD5(ctx context.Context, cx *library.Gallery, imageID int64, stored string) (string, error) {
	if stored != "" {
		return stored, nil
	}
	return cx.StoreMD5(ctx, imageID)
}

const onlineLookupChunk = 100

func (l *Link) runPTRLookupPhase(ctx context.Context, cx *library.Gallery) (checked, matched int, err error) {
	cursor, cerr := l.ptrIndexCursor(ctx)
	if cerr != nil {
		return 0, 0, cerr
	}
	lookup.PTRCursor.Store(cursor)

	ids, err := lookup.DueIDs(cx.DB, contract.LookupPTR, time.Now())
	if err != nil {
		return 0, 0, err
	}
	for start := 0; start < len(ids); start += contract.MaxPTRHashes {
		if ctx.Err() != nil {
			return checked, matched, nil
		}
		batch, berr := lookup.Due(cx.DB, contract.LookupPTR, time.Now(), ids[start:min(start+contract.MaxPTRHashes, len(ids))])
		if berr != nil {
			return checked, matched, berr
		}
		if len(batch) == 0 {
			continue
		}
		images := make([]contract.PTRLookupImage, len(batch))
		for i, c := range batch {
			images[i] = contract.PTRLookupImage{ImageID: c.ImageID, SHA256: c.SHA256}
		}
		ans, lerr := l.ptrBatchLookup(ctx, cx.Name, true, images)
		if lerr != nil {
			return checked, matched, lerr
		}
		if ans.Index > 0 {
			cursor = ans.Index
			lookup.PTRCursor.Store(cursor)
		}
		byHash := make(map[string]int64, len(batch))
		for _, c := range batch {
			byHash[c.SHA256] = c.ImageID
		}
		hits, recorded, aerr := applyPTRResults(cx, byHash, ans, cursor)
		matched += hits
		checked += recorded
		if aerr != nil {
			return checked, matched, aerr
		}
		l.jobs.Update(min(start+contract.MaxPTRHashes, len(ids)), len(ids), fmt.Sprintf("[%s] PTR lookup…", cx.Name))
	}
	return checked, matched, nil
}

func applyPTRResults(cx *library.Gallery, byHash map[string]int64, ans contract.PTRLookupResponse, cursor uint64) (matched, recorded int, err error) {
	for sha, id := range byHash {
		tags, hit := ans.Results[sha]
		result := lookup.ResultMiss
		if hit {
			withheld := gallery.Withheld{Categories: ans.WithheldCategories, Tags: ans.WithheldTags[sha]}
			if aerr := cx.Edit(func(d *db.DB) error { return gallery.ApplyPTRTags(d, cx.TagSvc, id, tags, withheld) }); aerr != nil {
				logx.Warnf("ptr apply for image %d: %v", id, aerr)
				continue
			}
			result = lookup.ResultHit
			matched++
		}
		if rerr := cx.Edit(func(d *db.DB) error {
			return lookup.Record(d, id, contract.LookupPTR, result, cursor, time.Now())
		}); rerr != nil {
			logx.Warnf("ptr record for image %d: %v", id, rerr)
			if err == nil {
				err = rerr
			}
			continue
		}
		recorded++
	}
	return matched, recorded, err
}

func dueLookupCount(cx *library.Gallery, backend string) int {
	n, err := lookup.DueCount(cx.DB, backend, time.Now())
	if err != nil {
		logx.Warnf("lookup due count %q: %v", cx.Name, err)
	}
	return n
}

type onlineLookupResult struct {
	queued       int
	skipped      int
	stillDue     int
	budgetSpent  bool
	resolved     int
	inconclusive int
}

// Reconciles first so the selection does not see rows stuck from a
// previous run.
func (l *Link) runOnlineLookupPhase(ctx context.Context, cx *library.Gallery) (onlineLookupResult, error) {
	var res onlineLookupResult
	res.resolved, res.inconclusive = l.reconcileLookups(ctx, cx)
	ids, err := lookup.DueIDs(cx.DB, contract.LookupBooru, time.Now())
	if err != nil {
		return res, err
	}
	for start := 0; start < len(ids) && ctx.Err() == nil; start += onlineLookupChunk {
		batch, err := lookup.Due(cx.DB, contract.LookupBooru, time.Now(), ids[start:min(start+onlineLookupChunk, len(ids))])
		if err != nil {
			return res, err
		}
		for i, c := range batch {
			if ctx.Err() != nil {
				break
			}
			md5, herr := LookupMD5(ctx, cx, c.ImageID, c.MD5)
			if herr != nil {
				// Skipped, never recorded as a miss: nothing was looked up.
				res.skipped++
				continue
			}
			eerr := l.EnqueueLookup(ctx, cx, c.ImageID, contract.LookupBooru, md5, c.SHA256, true, true)
			switch {
			case errors.Is(eerr, errBudgetSpent):
				res.budgetSpent = true
				res.stillDue = dueLookupCount(cx, contract.LookupBooru)
				return res, nil
			case peer.IsStatusError(eerr):
				res.skipped++
				continue
			case eerr != nil:
				return res, eerr
			}
			res.queued++
			l.jobs.Update(start+i+1, len(ids), fmt.Sprintf("[%s] online lookup…", cx.Name))
		}
	}
	res.stillDue = dueLookupCount(cx, contract.LookupBooru)
	return res, nil
}

func (l *Link) LookupOnline(ctx context.Context, cx *library.Gallery) ([]string, error) {
	res, err := l.runOnlineLookupPhase(ctx, cx)
	if err != nil && ctx.Err() == nil {
		l.jobs.Fail(err.Error())
		return nil, err
	}
	summary := "[" + cx.Name + "] " + onlinePhaseLine(res, ctx.Err() != nil)
	l.jobs.Complete(summary)
	if ctx.Err() != nil {
		return nil, nil
	}
	lines := []string{summary}
	if dropped := onlineDroppedLine(res); dropped != "" {
		lines = append(lines, "["+cx.Name+"] "+dropped)
	}
	return lines, nil
}

func onlinePhaseLine(res onlineLookupResult, cancelled bool) string {
	if cancelled {
		return fmt.Sprintf("online lookup cancelled (%d queued)", res.queued)
	}
	line := fmt.Sprintf("Online lookup: %d queued on monloader.", res.queued)
	if res.budgetSpent {
		line += fmt.Sprintf(" Daily budget reached; %d still due.", res.stillDue)
	} else if res.stillDue > 0 {
		line += fmt.Sprintf(" %d still due.", res.stillDue)
	}
	return line
}

func onlineDroppedLine(res onlineLookupResult) string {
	if res.resolved > 0 && res.inconclusive*2 > res.resolved {
		return fmt.Sprintf("Online lookup: monloader dropped %d of the %d lookups queued last run.", res.inconclusive, res.resolved)
	}
	return ""
}

func ptrPhaseLine(checked, matched int, err error, cancelled bool) string {
	switch {
	case errors.Is(err, errPTRBatchUnsupported):
		return "PTR lookup skipped: " + err.Error() + "."
	case cancelled:
		return fmt.Sprintf("PTR lookup cancelled (%d checked, %d matched)", checked, matched)
	case err != nil:
		return "PTR lookup failed: " + err.Error() + "."
	case checked == 0:
		return "PTR lookup: nothing is due."
	}
	return fmt.Sprintf("PTR lookup: %d checked, %d matched, %d no match.", checked, matched, checked-matched)
}

func (l *Link) LookupDue(ctx context.Context, cx *library.Gallery, ptrOn, booruOn bool) {
	var parts []string
	failed := false
	if ptrOn {
		if !l.PTRReady() {
			parts = append(parts, "PTR lookup skipped: the index is not ready on monloader.")
		} else {
			checked, matched, err := l.runPTRLookupPhase(ctx, cx)
			failed = err != nil && !errors.Is(err, errPTRBatchUnsupported) && ctx.Err() == nil
			parts = append(parts, ptrPhaseLine(checked, matched, err, ctx.Err() != nil))
		}
	}
	// A PTR failure ends that phase only.
	if booruOn && ctx.Err() == nil {
		if res, err := l.runOnlineLookupPhase(ctx, cx); err != nil && ctx.Err() == nil {
			failed = true
			parts = append(parts, "Online lookup failed: "+err.Error()+".")
		} else {
			parts = append(parts, onlinePhaseLine(res, ctx.Err() != nil))
			if dropped := onlineDroppedLine(res); dropped != "" {
				parts = append(parts, dropped)
			}
		}
	}
	if failed {
		l.jobs.Fail(strings.Join(parts, " "))
		return
	}
	l.jobs.Complete(strings.Join(parts, " "))
}

func (l *Link) LookupPTR(ctx context.Context, cx *library.Gallery) ([]string, error) {
	checked, matched, err := l.runPTRLookupPhase(ctx, cx)
	summary := "[" + cx.Name + "] " + ptrPhaseLine(checked, matched, err, ctx.Err() != nil)
	switch {
	case errors.Is(err, errPTRBatchUnsupported), ctx.Err() != nil:
		l.jobs.Complete(summary)
		return nil, nil
	case err != nil:
		l.jobs.Fail(err.Error())
		return nil, err
	}
	l.jobs.Complete(summary)
	return []string{summary}, nil
}
