package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type LandStage int

const (
	LandPlace LandStage = iota
	LandWrite
	LandSize
	LandType
	LandCheck
	LandIngest
)

func (s LandStage) String() string {
	switch s {
	case LandPlace:
		return "place"
	case LandWrite:
		return "write"
	case LandSize:
		return "size"
	case LandType:
		return "type"
	case LandCheck:
		return "check"
	case LandIngest:
		return "ingest"
	}
	return fmt.Sprintf("LandStage(%d)", int(s))
}

// LandError names the step a landing failed at.
type LandError struct {
	Stage LandStage
	Err   error
}

func (e *LandError) Error() string { return e.Err.Error() }
func (e *LandError) Unwrap() error { return e.Err }

func landErr(stage LandStage, err error) error { return &LandError{Stage: stage, Err: err} }

// LandStageOf is -1 for an error that did not come from a landing.
func LandStageOf(err error) LandStage {
	if le, ok := errors.AsType[*LandError](err); ok {
		return le.Stage
	}
	return -1
}

// LogLandFailure keeps a refused file for the debug log and warns of a
// failure on the server's side.
func LogLandFailure(what string, err error) {
	switch stage := LandStageOf(err); stage {
	case LandPlace, LandSize, LandType, LandCheck:
		logx.Debugf("%s: %s refused: %v", what, stage, err)
	default:
		logx.Warnf("%s: %s failed: %v", what, stage, err)
	}
}

type LandSpec struct {
	Origin string
	// MaxBytes refuses a larger file once written; 0 takes any size.
	MaxBytes int64
	// Check runs once the file's type is known, before the ingest.
	Check func(path, fileType string) error
	// Naming files a row the landing created; NameSource and NamePostID
	// feed its {source} and {post_id}.
	Naming     gallery.Naming
	NameSource string
	NamePostID string
	// KeepOnFailure leaves a file whose ingest failed for a reason other
	// than its type, for the watcher or the next sync to take.
	KeepOnFailure bool
}

type LandResult struct {
	Image *models.Image
	// Duplicate: the gallery already held these bytes; Dropped: the landed
	// copy was removed for it.
	Duplicate bool
	Dropped   bool
	Path      string
}

// Land writes a file into dir, which the caller resolved through the
// gallery's boundary, under name suffixed when taken, and ingests it. A
// failure removes what it wrote, unless spec keeps it.
func (cx *Gallery) Land(ctx context.Context, dir, name string, write func(dst string) error, spec LandSpec) (LandResult, error) {
	want := filepath.Join(dir, name)
	if err := cx.landable(want); err != nil {
		return LandResult{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return LandResult{}, landErr(LandWrite, err)
	}
	dst, err := gallery.CreateUnique(dir, name)
	if err != nil {
		return LandResult{}, landErr(LandWrite, err)
	}
	if dst != want {
		// A suffix can meet an ignore glob the plain name did not.
		if err := cx.landable(dst); err != nil {
			_ = os.Remove(dst)
			return LandResult{}, err
		}
	}
	// Straight to the final name: the watcher would record a temp name
	// and mark it missing after the rename.
	if err := write(dst); err != nil {
		_ = os.Remove(dst)
		return LandResult{}, landErr(LandWrite, err)
	}
	return cx.land(ctx, dst, spec, false)
}

func FromReader(r io.Reader) func(dst string) error {
	return func(dst string) error {
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, r)
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		return err
	}
}

func FromFile(src string) func(dst string) error {
	return func(dst string) error { return gallery.CopyFileContents(src, dst) }
}

// LandInPlace ingests a file already under the gallery root. The file is
// the operator's: nothing removes it, and a duplicate stays as an alias.
func (cx *Gallery) LandInPlace(ctx context.Context, path string, spec LandSpec) (LandResult, error) {
	if err := cx.landable(path); err != nil {
		return LandResult{}, err
	}
	return cx.land(ctx, path, spec, true)
}

func (cx *Gallery) landable(path string) error {
	if !gallery.PathInside(cx.GalleryPath, path) {
		return landErr(LandPlace, fmt.Errorf("%q is outside the gallery root", path))
	}
	if err := cx.Boundary().Check(path); err != nil {
		return landErr(LandPlace, err)
	}
	return nil
}

func (cx *Gallery) land(ctx context.Context, path string, spec LandSpec, inPlace bool) (LandResult, error) {
	discard := func() {
		if !inPlace {
			_ = os.Remove(path)
		}
	}
	if spec.MaxBytes > 0 {
		if info, err := os.Stat(path); err == nil && info.Size() > spec.MaxBytes {
			discard()
			return LandResult{}, landErr(LandSize, fmt.Errorf("file exceeds %d bytes", spec.MaxBytes))
		}
	}
	fileType, err := gallery.DetectFileType(path)
	if err != nil {
		discard()
		return LandResult{}, landErr(LandType, err)
	}
	if spec.Check != nil {
		if err := spec.Check(path, fileType); err != nil {
			discard()
			return LandResult{}, landErr(LandCheck, err)
		}
	}
	img, dup, err := gallery.Ingest(cx.Handle, path, spec.Origin)
	if errors.Is(err, gallery.ErrUnsupportedType) {
		discard()
		return LandResult{}, landErr(LandType, err)
	}
	if err != nil {
		if !spec.KeepOnFailure {
			discard()
		}
		return LandResult{}, landErr(LandIngest, err)
	}
	cx.phashSink().Landed(img)
	res := LandResult{Image: img, Duplicate: dup, Path: path}
	// A duplicate whose canonical is the landed path is a deleted file
	// coming back, and stays.
	if dup && !inPlace && img.CanonicalPath != path {
		gallery.DropDuplicateCopy(cx.DB, img.ID, path, spec.Origin)
		res.Dropped = true
	}
	if named := !dup || (!inPlace && !res.Dropped); named && !spec.Naming.Empty() {
		if _, err := spec.Naming.Apply(ctx, cx.DB, cx.Boundary(), img.ID, spec.NameSource, spec.NamePostID); err != nil {
			logx.Warnf("%s: name image %d: %v", spec.Origin, img.ID, err)
		}
	}
	cx.InvalidateCaches()
	return res, nil
}
