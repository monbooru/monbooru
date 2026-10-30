package web

import (
	"cmp"
	"database/sql"
	"io"
	"mime"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

type downloadRow struct {
	id      int64
	path    string
	size    int64
	missing bool
}

type downloadPlan struct {
	rows     []downloadRow
	bytes    int64
	missing  int
	zipName  string
	entries  []galleryio.DownloadEntry
	manifest []byte
}

// No job lane: the download writes nothing, so it runs beside any job.
func (s *Server) batchDownload(w http.ResponseWriter, r *http.Request) {
	progress, plan, ok := s.planDownload(w, r)
	if !ok {
		return
	}
	defer s.downloads.remove(progress)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": plan.zipName}))
	w.Header().Set("Cache-Control", "no-store")
	written, skipped, err := galleryio.WriteDownload(r.Context(), countingWriter{w, &progress.sent}, plan.manifest, plan.entries,
		func(n int) { progress.done.Store(int64(n)) })
	if err != nil && r.Context().Err() == nil {
		logx.Warnf("batch download %q: %v", plan.zipName, err)
		// The headers are out: a clean end would pass a truncated zip off
		// as whole.
		panic(http.ErrAbortHandler)
	}
	logx.Infof("batch download: %d %s as %q, %d skipped", written, models.Plural(written, "file", "files"), plan.zipName, skipped)
}

// The read lock is released before the first byte: the stream reads files
// by path only, and a switch queued behind a long download would stall
// every request after it.
func (s *Server) planDownload(w http.ResponseWriter, r *http.Request) (*activeDownload, *downloadPlan, bool) {
	s.set.RLock()
	defer s.set.RUnlock()
	if pageGalleryStale(w, r, s.activeGallery()) {
		return nil, nil, false
	}
	plan, ok := s.scopeDownload(w, r)
	if !ok {
		return nil, nil, false
	}
	if len(plan.rows) == 0 {
		flashStatus(w, http.StatusBadRequest, "Nothing to download: every file in the scope is missing on disk.")
		return nil, nil, false
	}
	progress := &activeDownload{Name: plan.zipName, Files: len(plan.rows), Bytes: plan.bytes}
	s.downloads.add(progress)
	if err := s.nameDownload(plan, r.FormValue("tags") == "1"); err != nil {
		s.downloads.remove(progress)
		logx.Errorf("batch download %q: %v", plan.zipName, err)
		flashStatus(w, http.StatusInternalServerError, "Download failed.")
		return nil, nil, false
	}
	return progress, plan, true
}

func (s *Server) scopeDownload(w http.ResponseWriter, r *http.Request) (*downloadPlan, bool) {
	if !parseFormOK(w, r) {
		return nil, false
	}
	cx := s.active()
	if cx == nil || cx.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Download unavailable: gallery path is unreadable.")
		return nil, false
	}
	ids, ok := s.resolveBatchScope(w, r, "batch-download", true)
	if !ok {
		return nil, false
	}
	rows, err := downloadRows(cx.DB, ids)
	if err != nil {
		logx.Errorf("batch download: %v", err)
		flashStatus(w, http.StatusInternalServerError, "Download failed.")
		return nil, false
	}
	plan := &downloadPlan{zipName: downloadZipName(r.FormValue("q"), cx.Name)}
	plan.rows, plan.missing = downloadable(cx, ids, rows)
	for _, row := range plan.rows {
		plan.bytes += row.size
	}
	return plan, true
}

func (s *Server) nameDownload(plan *downloadPlan, withTags bool) error {
	var reserved []string
	if withTags {
		reserved = append(reserved, "tags.json")
	}
	namer := gallery.NewZipNamer(reserved...)
	names := make(map[int64]string, len(plan.rows))
	plan.entries = make([]galleryio.DownloadEntry, len(plan.rows))
	for i, row := range plan.rows {
		names[row.id] = namer.Name(row.path)
		plan.entries[i] = galleryio.DownloadEntry{Path: row.path, Name: names[row.id]}
	}
	if !withTags {
		return nil
	}
	var err error
	plan.manifest, err = galleryio.DownloadManifest(s.active().Handle, names)
	return err
}

func (s *Server) batchDownloadCount(w http.ResponseWriter, r *http.Request) {
	plan, ok := s.scopeDownload(w, r)
	if !ok {
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{
		"files":   len(plan.rows),
		"bytes":   plan.bytes,
		"size":    webui.HumanBytes(plan.bytes),
		"missing": plan.missing,
	})
}

func downloadRows(database *db.DB, ids []int64) (map[int64]downloadRow, error) {
	rows := make(map[int64]downloadRow, len(ids))
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		got, err := db.QueryAll(database.Read, scanDownloadRow,
			`SELECT id, canonical_path, file_size, is_missing
			 FROM images WHERE id IN (`+placeholders+`)`, args...)
		for _, row := range got {
			rows[row.id] = row
		}
		return err
	})
	return rows, err
}

func scanDownloadRow(r *sql.Rows) (downloadRow, error) {
	var row downloadRow
	err := r.Scan(&row.id, &row.path, &row.size, &row.missing)
	return row, err
}

// A path outside the gallery root is left out, as the byte routes refuse it.
func downloadable(cx *library.Gallery, ids []int64, rows map[int64]downloadRow) (kept []downloadRow, left int) {
	for _, id := range ids {
		row, found := rows[id]
		if !found || row.missing || !gallery.NamedInside(cx.GalleryPath, row.path) {
			left++
			continue
		}
		kept = append(kept, row)
	}
	return kept, left
}

func downloadZipName(q, galleryName string) string {
	label := cmp.Or(search.PinnedCollectionName(search.Parse(q)), galleryName)
	return time.Now().Format("20060102-150405") + "-" + sanitizeCollectionFilename(label) + ".zip"
}

type activeDownload struct {
	Name  string
	Files int
	Bytes int64
	done  atomic.Int64
	sent  atomic.Int64
}

// Done is how many files the zip holds so far.
func (d *activeDownload) Done() int64 { return d.done.Load() }

// Sent is how many bytes of the zip have gone out.
func (d *activeDownload) Sent() int64 { return d.sent.Load() }

type downloadList struct {
	mu   sync.Mutex
	list []*activeDownload
}

func (l *downloadList) add(d *activeDownload) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, d)
}

func (l *downloadList) remove(d *activeDownload) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = slices.DeleteFunc(l.list, func(x *activeDownload) bool { return x == d })
}

func (l *downloadList) snapshot() []*activeDownload {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list)
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}
