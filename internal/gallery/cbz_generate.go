package gallery

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type CBZMember struct {
	Path     string
	FileType string
	filename string
}

// WriteCollectionCBZ refuses the whole set when a member cannot be a page,
// rather than dropping it, so the operator hears about it.
func WriteCollectionCBZ(ctx context.Context, dstPath string, members []CBZMember, title string, progress func(processed, total int, message string)) (pages, skipped int, err error) {
	for _, m := range members {
		if ft, _ := models.LookupFileType(m.FileType); !ft.PageImage {
			return 0, 0, fmt.Errorf("%s has file type %q, which cannot be a cbz page", filepath.Base(m.Path), m.FileType)
		}
	}
	total := len(members)
	if total == 0 {
		return 0, 0, errors.New("no members to generate")
	}
	if progress != nil {
		progress(0, total, "generating…")
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, 0, fmt.Errorf("create output dir: %w", err)
	}
	err = fsx.WriteAtomic(dstPath, ".cbz-generate-*", func(tmp *os.File) error {
		zw := zip.NewWriter(tmp)
		for _, m := range members {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			f, openErr := os.Open(m.Path)
			if openErr != nil {
				skipped++
				continue
			}
			// The extension comes from the stored type: the file on disk
			// may have none.
			entry, err := zw.CreateHeader(&zip.FileHeader{
				Name:   fmt.Sprintf("%04d.%s", pages+1, m.FileType),
				Method: zip.Store,
			})
			if err != nil {
				_ = f.Close()
				return err
			}
			_, copyErr := io.Copy(entry, f)
			_ = f.Close()
			if copyErr != nil {
				return fmt.Errorf("write page %d: %w", pages+1, copyErr)
			}
			pages++
			if progress != nil {
				progress(pages, total, "generating…")
			}
		}
		if pages == 0 {
			return errors.New("every member's file is missing from disk")
		}

		ci, err := metadata.MarshalComicInfo(title, pages)
		if err != nil {
			return fmt.Errorf("comic info: %w", err)
		}
		ciEntry, err := zw.CreateHeader(&zip.FileHeader{Name: "ComicInfo.xml", Method: zip.Deflate})
		if err != nil {
			return err
		}
		if _, err := ciEntry.Write(ci); err != nil {
			return fmt.Errorf("write comic info: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("close zip: %w", err)
		}
		return nil
	})
	if err != nil {
		return pages, skipped, err
	}
	// CreateTemp leaves 0600; other gallery files are 0644.
	if err := os.Chmod(dstPath, 0o644); err != nil {
		logx.Warnf("cbz generation: chmod %q: %v", dstPath, err)
	}
	return pages, skipped, nil
}
