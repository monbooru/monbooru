package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type DeleteOptions struct {
	// FilesGone takes only rows whose file is gone and unlinks nothing:
	// the watcher and sync promote a true copy before a row goes missing.
	FilesGone bool
	// ContinuePastFailure retries a failed chunk one image at a time and
	// skips the images that still fail.
	ContinuePastFailure bool
	RemoveEmptyFolder   bool
}

type DeletedImage struct {
	ID            int64
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
	FolderRemoved bool
}

type DeleteResult struct {
	Deleted []DeletedImage
	// Kept counts the FilesGone candidates whose file is still on disk.
	Kept int
	// Changed counts the files left on disk because their length is no
	// longer their row's.
	Changed   int
	Failed    int
	Cancelled bool
}

const deleteChunk = 500

// DeleteImages drops the caches after each chunk it commits, so progress
// sees what is left.
func (cx *Gallery) DeleteImages(ctx context.Context, ids []int64, opts DeleteOptions, progress func(done, total int)) (DeleteResult, error) {
	var res DeleteResult
	if opts.FilesGone {
		var err error
		if ids, res.Kept, err = cx.filesGone(ids); err != nil {
			return res, err
		}
	} else if err := gallery.RootReadable(cx.Boundary().Root()); err != nil {
		return res, err
	}
	total := len(ids)
	for start := 0; start < total; start += deleteChunk {
		if ctx.Err() != nil {
			res.Cancelled = true
			return res, nil
		}
		chunk := ids[start:min(start+deleteChunk, total)]
		deleted, changed, err := cx.deleteChunk(chunk, opts)
		if err != nil {
			if !opts.ContinuePastFailure {
				return res, err
			}
			deleted, changed = nil, 0
			for _, id := range chunk {
				one, oneChanged, err := cx.deleteChunk([]int64{id}, opts)
				if err != nil {
					logx.Warnf("delete image %d: %v", id, err)
					res.Failed++
					continue
				}
				deleted = append(deleted, one...)
				changed += oneChanged
			}
		}
		res.Deleted = append(res.Deleted, deleted...)
		res.Changed += changed
		if progress != nil {
			progress(start+len(chunk), total)
		}
	}
	return res, nil
}

// An unmounted volume answers "gone" for every file: its mount point is
// missing or an empty folder.
func (cx *Gallery) filesGone(ids []int64) (gone []int64, kept int, err error) {
	entries, err := os.ReadDir(cx.Boundary().Root())
	if err != nil {
		return nil, 0, fmt.Errorf("gallery path is unreadable: %w", err)
	}
	if len(entries) == 0 {
		return nil, 0, errors.New("gallery folder is empty - mount it before pruning")
	}
	err = db.Chunked(ids, deleteChunk, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		rows, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(d *DeletedImage) []any {
			return []any{&d.ID, &d.CanonicalPath}
		}), `SELECT id, canonical_path FROM images WHERE id IN (`+in+`)`, args...)
		for _, r := range rows {
			if _, statErr := os.Stat(r.CanonicalPath); errors.Is(statErr, os.ErrNotExist) {
				gone = append(gone, r.ID)
			} else {
				kept++
			}
		}
		return err
	})
	return gone, kept, err
}

type deleteRow struct {
	DeletedImage
	fileType string
	rec      gallery.Indexed
}

func (cx *Gallery) deleteChunk(chunk []int64, opts DeleteOptions) ([]DeletedImage, int, error) {
	var rows []deleteRow
	var aliases map[int64][]gallery.AliasCopy
	err := db.InWriteTx(cx.DB.Write, func(tx *sql.Tx) error {
		in, args := db.InPlaceholders(chunk)
		var err error
		rows, err = db.QueryAll(tx, func(r *sql.Rows) (deleteRow, error) {
			var d deleteRow
			var missing int
			err := r.Scan(&d.ID, &d.CanonicalPath, &d.FolderPath, &missing, &d.fileType,
				&d.rec.Size, &d.rec.SHA256, &d.rec.Mtime, &d.rec.MtimeNano)
			d.IsMissing = missing == 1
			return d, err
		}, `SELECT i.id, i.canonical_path, i.folder_path, i.is_missing, i.file_type,
		           i.file_size, i.sha256, COALESCE(ip.mtime_unix, 0), COALESCE(ip.mtime_nsec, 0)
		      FROM images i LEFT JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 1
		     WHERE i.id IN (`+in+`)`, args...)
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]int64, 0, len(rows))
		var visible []int64
		for _, r := range rows {
			ids = append(ids, r.ID)
			if !r.IsMissing {
				visible = append(visible, r.ID)
			}
		}
		// First: dup_groups.original_image_id has no ON DELETE.
		if err := cx.RelationsSvc.OnImagesDeleteTx(tx, ids); err != nil {
			return err
		}
		if err := tags.ImagesVisibilityChangedTx(tx, visible, false); err != nil {
			return err
		}
		// image_paths cascades with the rows.
		if aliases, err = gallery.AliasPathsFor(tx, ids); err != nil {
			return err
		}
		in, args = db.InPlaceholders(ids)
		_, err = tx.Exec(`DELETE FROM images WHERE id IN (`+in+`)`, args...)
		return err
	})
	if err != nil || len(rows) == 0 {
		return nil, 0, err
	}
	b := cx.Boundary()
	out := make([]DeletedImage, 0, len(rows))
	changed := 0
	for _, r := range rows {
		relations.PhashCleared(cx.bkTree, r.ID)
		gallery.RemoveImageArtifacts(cx.ThumbnailsPath, r.ID, r.fileType)
		if !opts.FilesGone {
			if !r.IsMissing && gallery.UnlinkImageFile(b, r.CanonicalPath, r.rec, r.ID) {
				changed++
			}
			changed += gallery.UnlinkAliasFiles(b, r.ID, aliases[r.ID])
			if opts.RemoveEmptyFolder && !r.IsMissing && r.FolderPath != "" {
				r.FolderRemoved = gallery.RemoveEmptyDirs(b, []string{r.FolderPath}) == 1
			}
		}
		out = append(out, r.DeletedImage)
	}
	cx.InvalidateCaches()
	return out, changed, nil
}

type KeptCopy struct {
	PathID int64
	Path   string
	Reason error
}

type CopiesResult struct {
	Removed   int
	Kept      []KeptCopy
	Cancelled bool
}

// DeleteCopies unlinks each alias copy before its row goes, so a copy whose
// file stays never loses its row; a forgotten copy's row goes uncounted.
func (cx *Gallery) DeleteCopies(ctx context.Context, pathIDs []int64, progress func(done, total int)) (CopiesResult, error) {
	type copyRow struct {
		id              int64
		path, canonical string
		rec             gallery.Indexed
	}
	var res CopiesResult
	b := cx.Boundary()
	if err := gallery.RootReadable(b.Root()); err != nil {
		return res, err
	}
	total := len(pathIDs)
	for start := 0; start < total; start += deleteChunk {
		if ctx.Err() != nil {
			res.Cancelled = true
			return res, nil
		}
		chunk := pathIDs[start:min(start+deleteChunk, total)]
		in, args := db.InPlaceholders(chunk)
		rows, err := db.QueryAll(cx.DB.Read, db.ScanRow(func(c *copyRow) []any {
			return []any{&c.id, &c.path, &c.canonical, &c.rec.Size, &c.rec.SHA256, &c.rec.Mtime, &c.rec.MtimeNano}
		}), `SELECT ip.id, ip.path, i.canonical_path, i.file_size, i.sha256, ip.mtime_unix, ip.mtime_nsec
		      FROM image_paths ip JOIN images i ON i.id = ip.image_id
		     WHERE ip.is_canonical = 0 AND ip.id IN (`+in+`)`, args...)
		if err != nil {
			return res, err
		}
		var drop []int64
		for _, c := range rows {
			outcome, err := gallery.RemoveCopy(b, c.path, c.canonical, c.rec)
			switch outcome {
			case gallery.CopyRemoved:
				res.Removed++
				drop = append(drop, c.id)
			case gallery.CopyForgotten:
				logx.Warnf("delete copy %q: %v - its row goes, the file stays", c.path, err)
				drop = append(drop, c.id)
			default:
				logx.Warnf("delete copy %q: kept: %v", c.path, err)
				res.Kept = append(res.Kept, KeptCopy{PathID: c.id, Path: c.path, Reason: err})
			}
		}
		if len(drop) > 0 {
			if err := gallery.DeleteAliasPaths(cx.DB, drop); err != nil {
				return res, err
			}
			cx.InvalidateCaches()
		}
		if progress != nil {
			progress(start+len(chunk), total)
		}
	}
	return res, nil
}
