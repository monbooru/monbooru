package gallery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// PruneOrphanThumbnails fails the sweep on a failed id read: a partial set
// would make live thumbnails look orphaned.
func PruneOrphanThumbnails(ctx context.Context, database *db.DB, thumbnailsPath string, progress func(done, total int)) (removed, processed, total int, err error) {
	entries, err := os.ReadDir(thumbnailsPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read thumbnails dir: %w", err)
	}
	mangaDir := MangaCacheDir(thumbnailsPath)
	caches, _ := os.ReadDir(mangaDir)
	total = len(entries) + len(caches)
	ids, err := db.QueryIDsContext(ctx, database.Read, `SELECT id FROM images`)
	if err != nil {
		return 0, 0, total, err
	}
	known := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		known[id] = struct{}{}
	}
	report := func(done int) {
		if progress != nil {
			progress(done, total)
		}
	}
	report(0)
	for i, e := range entries {
		if ctx.Err() != nil {
			return removed, processed, total, nil
		}
		if (i+1)%50 == 0 {
			report(i + 1)
		}
		if e.IsDir() {
			continue
		}
		processed++
		id, ok := thumbnailOwner(e.Name())
		if !ok {
			continue
		}
		if _, held := known[id]; held {
			continue
		}
		if err := os.Remove(filepath.Join(thumbnailsPath, e.Name())); err == nil {
			removed++
		}
	}
	for i, e := range caches {
		if ctx.Err() != nil {
			return removed, processed, total, nil
		}
		if (len(entries)+i+1)%50 == 0 {
			report(len(entries) + i + 1)
		}
		if !e.IsDir() {
			continue
		}
		processed++
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		if _, held := known[id]; held {
			continue
		}
		if err := os.RemoveAll(filepath.Join(mangaDir, e.Name())); err == nil {
			removed++
		}
	}
	// An auto-tag killed mid-chunk leaves its page folder; the sweep holds
	// the job lane, so none is in use.
	dataDir := filepath.Dir(thumbnailsPath)
	siblings, _ := os.ReadDir(dataDir)
	for _, e := range siblings {
		if e.IsDir() && strings.HasPrefix(e.Name(), "autotag-") {
			if err := os.RemoveAll(filepath.Join(dataDir, e.Name())); err == nil {
				removed++
			}
		}
	}
	report(total)
	return removed, processed, total, nil
}

func thumbnailOwner(name string) (int64, bool) {
	for _, suffix := range []string{"_hover.webp", "_view.jpg", ".jpg"} {
		if idStr, ok := strings.CutSuffix(name, suffix); ok {
			id, err := strconv.ParseInt(idStr, 10, 64)
			return id, err == nil
		}
	}
	return 0, false
}

var staleTempPrefixes = []string{"export-", "import-", "merge-", "thumbnails-import-"}

func SweepStaleTemps(dataDir string, before time.Time) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !slices.ContainsFunc(staleTempPrefixes, func(p string) bool { return strings.HasPrefix(e.Name(), p) }) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		path := filepath.Join(dataDir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			logx.Warnf("remove stale temp %q: %v", path, err)
			continue
		}
		logx.Infof("removed stale temp %q", path)
	}
}
