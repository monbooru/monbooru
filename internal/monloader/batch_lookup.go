package monloader

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type BatchLookupOpts struct {
	Mode       string
	PTR, Booru bool
	Unsourced  bool
}

// Hash lookups take the background lane, being bulk work, but no budget: the
// operator asked for them, so the nightly budget must not refuse them.
func (l *Link) BatchLookup(ctx context.Context, cx *library.Gallery, opts BatchLookupOpts, ids []int64) {
	if opts.Mode == "hash" && opts.Unsourced {
		ids = unsourcedOnly(cx, ids)
	}

	enqueued, skipped, refused, matched := 0, 0, 0, 0
	ptrRefused := false
	total := len(ids)
	stop := func(err error) {
		if ctx.Err() != nil {
			l.jobs.Complete(fmt.Sprintf("Lookup cancelled after queueing %d.", enqueued))
			return
		}
		l.jobs.Fail("monloader unreachable: " + err.Error())
	}
	l.jobs.Update(0, total, "looking up…")
	if opts.Mode == "hash" && opts.PTR {
		var err error
		if matched, err = l.batchPTRLookup(ctx, cx, ids); err != nil {
			if ctx.Err() != nil {
				stop(err)
				return
			}
			if !errors.Is(err, ErrPTRUnavailable) && !errors.Is(err, errPTRBatchUnsupported) {
				l.jobs.Fail("PTR lookup failed: " + err.Error())
				return
			}
			ptrRefused = true
		}
	}
	if opts.Booru || opts.Mode == "refresh" {
		for i, id := range ids {
			if ctx.Err() != nil {
				stop(ctx.Err())
				return
			}
			if (i+1)%25 == 0 || i == total-1 {
				l.jobs.Update(i+1, total, "looking up…")
			}
			var canonPath, sha, storedMD5 string
			if err := cx.DB.Read.QueryRow(
				`SELECT canonical_path, sha256, md5 FROM images WHERE id = ?`, id,
			).Scan(&canonPath, &sha, &storedMD5); err != nil {
				continue
			}
			var err error
			switch opts.Mode {
			case "refresh":
				queued, qErr := l.enqueueSourceFetches(ctx, cx, id, sha)
				if qErr != nil {
					if authRefused(qErr) {
						refused++
						continue
					}
					if peer.IsStatusError(qErr) {
						skipped++
						continue
					}
					stop(qErr)
					return
				}
				if queued == 0 {
					skipped++
				} else {
					enqueued += queued
				}
				continue
			default:
				md5, hashErr := LookupMD5(ctx, cx, id, storedMD5)
				if hashErr != nil {
					skipped++
					continue
				}
				err = l.EnqueueLookup(ctx, cx, id, contract.LookupBooru, md5, sha, true, false)
			}
			if err != nil {
				if authRefused(err) {
					refused++
					continue
				}
				if peer.IsStatusError(err) {
					skipped++
					continue
				}
				stop(err)
				return
			}
			enqueued++
		}
	}
	if opts.Mode == "refresh" {
		msg := fmt.Sprintf("Queued %d source %s on monloader; skipped %d without a fetchable source", enqueued, models.Plural(enqueued, "fetch", "fetches"), skipped)
		if refused > 0 {
			msg += fmt.Sprintf("; monloader refused %d", refused)
		}
		l.jobs.Complete(msg + ".")
		return
	}
	parts := []string{}
	switch {
	case ptrRefused:
		parts = append(parts, "the PTR is unavailable on monloader")
	case opts.PTR:
		parts = append(parts, fmt.Sprintf("%d matched in the PTR", matched))
	}
	if opts.Booru {
		parts = append(parts, fmt.Sprintf("%d queued on monloader", enqueued))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	if refused > 0 {
		parts = append(parts, fmt.Sprintf("%d refused by monloader", refused))
	}
	l.jobs.Complete(fmt.Sprintf("Looked up %d %s: %s.", total, models.Plural(total, "image", "images"), strings.Join(parts, ", ")))
}

func authRefused(err error) bool {
	var se peer.StatusError
	return errors.As(err, &se) && (strings.HasPrefix(se.Status, "401") || strings.HasPrefix(se.Status, "403"))
}

func unsourcedOnly(cx *library.Gallery, ids []int64) []int64 {
	var out []int64
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		placeholders, args := db.InPlaceholders(chunk)
		found, err := db.QueryIDs(cx.DB.Read,
			`SELECT i.id FROM images i WHERE i.id IN (`+placeholders+`)
			   AND NOT EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')`,
			args...)
		if err != nil {
			logx.Warnf("batch lookup scope narrowing: %v", err)
			return ids
		}
		out = append(out, found...)
	}
	return out
}

func (l *Link) batchPTRLookup(ctx context.Context, cx *library.Gallery, ids []int64) (int, error) {
	matched := 0
	for start := 0; start < len(ids) && ctx.Err() == nil; start += contract.MaxPTRHashes {
		chunk := ids[start:min(start+contract.MaxPTRHashes, len(ids))]
		placeholders, args := db.InPlaceholders(chunk)
		hashes, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(v *contract.PTRLookupImage) []any {
			return []any{&v.ImageID, &v.SHA256}
		}), `SELECT id, sha256 FROM images WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return matched, err
		}
		byHash := make(map[string]int64, len(hashes))
		images := make([]contract.PTRLookupImage, 0, len(hashes))
		for _, h := range hashes {
			byHash[h.SHA256] = h.ImageID
			images = append(images, h)
		}
		if len(images) == 0 {
			continue
		}
		ans, err := l.ptrBatchLookup(ctx, cx.Name, false, images)
		if err != nil {
			return matched, err
		}
		// A record failure is logged inside and must not abort the batch.
		hits, _, _ := applyPTRResults(cx, byHash, ans, ans.Index)
		matched += hits
	}
	return matched, nil
}

func (l *Link) enqueueSourceFetches(ctx context.Context, cx *library.Gallery, id int64, sha string) (int, error) {
	galleryName := cx.Name
	type origin struct{ site, url string }
	// All or nothing: queueing what a partial read reached would go unreported.
	origins, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(o *origin) []any {
		return []any{&o.site, &o.url}
	}), `SELECT site, url FROM image_sources WHERE image_id = ? ORDER BY rowid`, id)
	if err != nil {
		logx.Warnf("source fetch origins for image %d: %v", id, err)
		return 0, nil
	}

	queued := 0
	seenURL := map[string]bool{}
	ptrDone := false
	for _, o := range origins {
		switch {
		case strings.TrimSpace(o.url) != "":
			u := strings.TrimSpace(o.url)
			if seenURL[u] {
				continue
			}
			seenURL[u] = true
			if err := l.EnqueueMetadataFetch(ctx, id, galleryName, u); err != nil {
				return queued, err
			}
		case strings.EqualFold(strings.TrimSpace(o.site), contract.LookupPTR):
			if ptrDone {
				continue
			}
			ptrDone = true
			if err := l.EnqueueLookup(ctx, cx, id, contract.LookupPTR, "", sha, true, false); err != nil {
				if errors.Is(err, ErrPTRUnavailable) {
					continue
				}
				return queued, err
			}
		default:
			continue
		}
		queued++
	}
	return queued, nil
}
