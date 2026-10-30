package library

import (
	"context"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/relations"
)

// These fields feed a filter or a cached count, so each write drops the
// caches.

func (cx *Gallery) ToggleFlag(id int64, flag gallery.ImageFlag) (bool, error) {
	on, err := gallery.ToggleFlag(cx.DB, id, flag)
	if err == nil {
		cx.InvalidateCaches()
	}
	return on, err
}

func (cx *Gallery) ToggleFlags(ids []int64, flag gallery.ImageFlag) error {
	defer cx.InvalidateCaches()
	return gallery.ToggleFlags(cx.DB, ids, flag)
}

func (cx *Gallery) SetFlag(ids []int64, flag gallery.ImageFlag, on bool) error {
	defer cx.InvalidateCaches()
	return gallery.SetFlag(cx.DB, ids, flag, on)
}

func (cx *Gallery) SetScheduledLookup(ids []int64, backends []string, on bool) error {
	defer cx.InvalidateCaches()
	return gallery.SetScheduledLookup(cx.DB, ids, backends, on)
}

func (cx *Gallery) SetDimensions(ctx context.Context, id int64, width, height int) error {
	defer cx.InvalidateCaches()
	return gallery.SetDimensions(ctx, cx.DB, id, width, height)
}

// Edit runs one of the gallery package's writes on this gallery's database
// and drops the caches after it, whatever it returned: a write that failed
// half-way may still have changed rows.
func (cx *Gallery) Edit(write func(*db.DB) error) error {
	defer cx.InvalidateCaches()
	return write(cx.DB)
}

// RecomputePhash also feeds the index and drops the missing-phash counts.
func (cx *Gallery) RecomputePhash(ctx context.Context, id int64) (int64, error) {
	h, err := gallery.RecomputeAndStorePhash(ctx, cx.DB, id, cx.ThumbnailsPath)
	if err != nil {
		return 0, err
	}
	cx.phashSink().Stored(id, &h)
	cx.invalidatePhashMissing()
	return h, nil
}

// BackfillPhashes leaves the index to rebuild on its next use, against the
// fully hashed table.
func (cx *Gallery) BackfillPhashes(ctx context.Context, progress relations.PhashBackfillProgress) (processed, updated int, err error) {
	defer func() {
		if cx.bkTree != nil {
			cx.bkTree.Reset()
		}
		cx.invalidatePhashMissing()
	}()
	return relations.BackfillPhashes(ctx, cx.DB, cx.bkTree, cx.ThumbnailsPath, progress)
}

// StoreMD5 drops the caches only when it stored a digest: the image page
// asks on every view of a file it cannot read.
func (cx *Gallery) StoreMD5(ctx context.Context, id int64) (string, error) {
	sum, err := gallery.ComputeAndStoreMD5(ctx, cx.DB, id)
	if err == nil {
		cx.InvalidateCaches()
	}
	return sum, err
}

func (cx *Gallery) BackfillMD5s(ctx context.Context, progress gallery.MD5BackfillProgress) (processed, updated int, err error) {
	defer cx.InvalidateCaches()
	return gallery.BackfillMD5s(ctx, cx.DB, progress)
}

// NameFacts drops the caches only when it stored a digest: the name
// preview runs it on every keystroke.
func (cx *Gallery) NameFacts(ctx context.Context, id, md5Cap int64, tmpls ...*gallery.NameTemplate) (gallery.NameFacts, error) {
	f, stored, err := gallery.LoadNameFacts(ctx, cx.DB, cx.Name, id, md5Cap, tmpls...)
	if stored {
		cx.InvalidateCaches()
	}
	return f, err
}

func (cx *Gallery) StampUploadBatch(ids []int64, token int64) error {
	defer cx.InvalidateCaches()
	return gallery.StampUploadBatch(cx.DB, ids, token)
}

// FindPairs also stores the phashes it computes, so it drops the
// missing-phash counts.
func (cx *Gallery) FindPairs(ctx context.Context, opts relations.FindPairsOptions, progress relations.FindPairsProgress) (int, error) {
	defer cx.invalidatePhashMissing()
	opts.ThumbnailsPath = cx.ThumbnailsPath
	opts.VisibleCount = cx.VisibleCount
	return relations.FindPairs(ctx, cx.DB, cx.bkTree, opts, progress)
}

// ReplaceFile also feeds the index the new file's phash.
func (cx *Gallery) ReplaceFile(id int64, staged, sha, md5, fileType string) error {
	defer cx.InvalidateCaches()
	phash, err := gallery.ApplyReplacedFile(cx.Handle, id, staged, sha, md5, fileType)
	if err != nil {
		return err
	}
	cx.phashSink().Stored(id, phash)
	return nil
}

// ReadPlaceholder also feeds the index the file's phash.
func (cx *Gallery) ReadPlaceholder(id int64, path string) error {
	defer cx.InvalidateCaches()
	phash, err := gallery.ReadPlaceholder(cx.Handle, path)
	if err != nil {
		return err
	}
	cx.phashSink().Stored(id, phash)
	return nil
}

func (cx *Gallery) PlaceImage(id int64, folder, name *string) (*gallery.MoveImageResult, error) {
	defer cx.InvalidateCaches()
	return gallery.PlaceImage(cx.DB, cx.Boundary(), id, folder, name)
}
