package gallery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// Indexed is what a row recorded of the bytes at one of its paths; a zero
// mtime was not recorded.
type Indexed struct {
	Size      int64
	SHA256    string
	Mtime     int64
	MtimeNano int64
}

// AliasCopy's Size and SHA256 are the row's, which every true copy shares.
type AliasCopy struct {
	Path string
	Indexed
}

// AliasPathsFor must run before the rows go: image_paths cascades with images.
func AliasPathsFor(q db.Querier, ids []int64) (map[int64][]AliasCopy, error) {
	placeholders, args := db.InPlaceholders(ids)
	if placeholders == "" {
		return nil, nil
	}
	type copyRow struct {
		id int64
		AliasCopy
	}
	rows, err := db.QueryAll(q, db.ScanRow(func(r *copyRow) []any {
		return []any{&r.id, &r.Path, &r.Size, &r.SHA256, &r.Mtime, &r.MtimeNano}
	}), `SELECT ip.image_id, ip.path, i.file_size, i.sha256, ip.mtime_unix, ip.mtime_nsec
		   FROM image_paths ip
		   JOIN images i ON i.id = ip.image_id
		  WHERE ip.is_canonical = 0 AND ip.image_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	out := map[int64][]AliasCopy{}
	for _, r := range rows {
		out[r.id] = append(out[r.id], r.AliasCopy)
	}
	return out, nil
}

// UnlinkAliasFiles takes the copies with the row, or the next sync ingests
// one as a new untagged image. It reports how many it kept as changed.
func UnlinkAliasFiles(b *Boundary, id int64, copies []AliasCopy) (changed int) {
	for _, c := range copies {
		if UnlinkImageFile(b, c.Path, c.Indexed, id) {
			changed++
		}
	}
	return changed
}

var (
	ErrChangedSinceIndexed = errors.New("changed since indexing")
	ErrCanonicalGone       = errors.New("its canonical file is gone, so it is the only copy left (a sync makes it canonical)")
)

// With the watcher off, a recorded path can hold another file written over
// it since it was indexed. Only a file whose length and mtime both match
// the row's skips the hash, so a touched file still goes.
func ChangedSinceIndexed(path string, rec Indexed) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Size() != rec.Size {
		return fmt.Errorf("%w (%d bytes, not the row's %d)", ErrChangedSinceIndexed, info.Size(), rec.Size)
	}
	known := syncKnownEntry{size: rec.Size, mtime: rec.Mtime, mtimeNano: rec.MtimeNano}
	if known.unchanged(info.Size(), info.ModTime().Unix(), info.ModTime().UnixNano()) {
		return nil
	}
	sha, err := HashFile(path)
	if err != nil {
		return fmt.Errorf("%w (could not read it to compare: %v)", ErrChangedSinceIndexed, err)
	}
	if sha != rec.SHA256 {
		return fmt.Errorf("%w (other bytes of the same length)", ErrChangedSinceIndexed)
	}
	return nil
}

func RewrittenBy(q db.RowQuerier, id int64) int64 {
	var path string
	var rec Indexed
	if err := q.QueryRow(
		`SELECT i.canonical_path, i.file_size, i.sha256, COALESCE(ip.mtime_unix, 0), COALESCE(ip.mtime_nsec, 0)
		   FROM images i LEFT JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 1
		  WHERE i.id = ? AND i.is_missing = 0`, id,
	).Scan(&path, &rec.Size, &rec.SHA256, &rec.Mtime, &rec.MtimeNano); err != nil {
		return 0
	}
	info, err := os.Stat(path)
	known := syncKnownEntry{size: rec.Size, mtime: rec.Mtime, mtimeNano: rec.MtimeNano}
	if err != nil || known.unchanged(info.Size(), info.ModTime().Unix(), info.ModTime().UnixNano()) {
		return 0
	}
	sha, err := HashFile(path)
	if err != nil || sha == rec.SHA256 {
		return 0
	}
	other, err := ImageIDBySHA(q, sha)
	if err != nil {
		return 0
	}
	return other
}

type CopyOutcome int

const (
	// CopyRemoved: unlinked, or already gone from disk.
	CopyRemoved CopyOutcome = iota
	// CopyForgotten: the row may go, the file stays.
	CopyForgotten
	// CopyKept: the row and the file both stay.
	CopyKept
)

// RemoveCopy unlinks one alias copy of an image whose canonical file is
// canonical and whose row recorded rec there; the error says why a copy
// was forgotten or kept.
func RemoveCopy(b *Boundary, path, canonical string, rec Indexed) (CopyOutcome, error) {
	if path == "" {
		return CopyRemoved, nil
	}
	if !PathInside(b.Root(), path) {
		return CopyForgotten, fmt.Errorf("outside the gallery root %q", b.Root())
	}
	if err := b.Check(path); err != nil {
		return CopyForgotten, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return CopyRemoved, nil
	}
	if isCanonicalEntry(path, canonical) {
		return CopyForgotten, errors.New("it reaches the canonical file through a link")
	}
	if err := ChangedSinceIndexed(path, rec); err != nil {
		return CopyKept, err
	}
	if CanonicalGone(b, canonical) {
		return CopyKept, ErrCanonicalGone
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return CopyKept, err
	}
	return CopyRemoved, nil
}

// Compares resolved names, not inodes: removing a symlink or a hard link
// never takes the canonical file.
func isCanonicalEntry(path, canonical string) bool {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	target, err := filepath.EvalSymlinks(canonical)
	return err == nil && resolved == target
}

func RemoveImageArtifacts(thumbnailsPath string, id int64, fileType string) {
	_ = os.Remove(ThumbnailPath(thumbnailsPath, id))
	_ = os.Remove(hoverPath(thumbnailsPath, id))
	_ = os.Remove(viewRenditionPath(thumbnailsPath, id))
	_ = os.Remove(midRenditionPath(thumbnailsPath, id))
	if fileType == "" || fileType == "cbz" {
		removeMangaCache(thumbnailsPath, id)
	}
}

// UnlinkImageFile does not trust path: a hand-edited DB or a repointed
// mount can point it outside the root. changed reports a file kept because
// it no longer holds rec's bytes; the next sync takes it as it is now.
func UnlinkImageFile(b *Boundary, path string, rec Indexed, id int64) (changed bool) {
	if path == "" {
		return false
	}
	if !PathInside(b.Root(), path) {
		logx.Warnf("delete image %d: refusing to unlink %q outside gallery root %q", id, path, b.Root())
		return false
	}
	if err := b.Check(path); err != nil {
		logx.Warnf("delete image %d: leaving %q alone: %v", id, path, err)
		return false
	}
	if err := ChangedSinceIndexed(path, rec); err != nil {
		logx.Warnf("delete image %d: %q %v - left alone", id, path, err)
		return true
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logx.Warnf("delete image file %q: %v", path, err)
	}
	return false
}
