package gallery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// ApplyReplacedFile commits the row before the staged file renames into
// place, so the watcher finds a sha the DB already carries. stagedPath,
// and the backup beside it, must lie outside the watched tree.
func ApplyReplacedFile(h Handle, imageID int64, stagedPath, newSHA, newMD5, newType string) (*int64, error) {
	database, b, thumbnailsPath := h.DB, h.Boundary(), h.ThumbnailsPath
	galleryPath := b.Root()
	var oldPath string
	var oldW, oldH *int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, width, height FROM images WHERE id = ?`, imageID,
	).Scan(&oldPath, &oldW, &oldH); err != nil {
		return nil, fmt.Errorf("load image %d: %w", imageID, err)
	}
	if !NamedInside(galleryPath, oldPath) {
		return nil, fmt.Errorf("refusing to replace %q outside gallery root %q", oldPath, galleryPath)
	}
	if err := b.Check(oldPath); err != nil {
		return nil, fmt.Errorf("refusing to replace image %d: %w", imageID, err)
	}

	fi, err := os.Stat(stagedPath)
	if err != nil {
		return nil, fmt.Errorf("stat staged file: %w", err)
	}
	// newType comes from the client's filename; the bytes decide.
	if actual, magicErr := MagicFileType(stagedPath); magicErr == nil {
		newType = actual
	}
	newW, newH := stillDimensions(stagedPath, newType)
	sdMeta, comfyMeta, sourceType := ExtractGenerationMeta(stagedPath, newType)

	newPath := oldPath
	if newExt := extForFileType(newType); newExt != "" && models.ExtFileType(oldPath) != newType {
		stem := filepath.Base(oldPath)
		stem = stem[:len(stem)-len(filepath.Ext(stem))]
		if newPath, err = CreateUnique(filepath.Dir(oldPath), stem+newExt); err != nil {
			return nil, fmt.Errorf("reserve the new name: %w", err)
		}
		if err := b.Check(newPath); err != nil {
			_ = os.Remove(newPath)
			return nil, fmt.Errorf("refusing to replace image %d: %w", imageID, err)
		}
		if err := refuseAliasCollision(database, imageID, newPath); err != nil {
			_ = os.Remove(newPath)
			return nil, fmt.Errorf("refusing to replace image %d: %w", imageID, err)
		}
	}
	dropReservation := func() {
		if newPath != oldPath {
			_ = os.Remove(newPath)
		}
	}

	backupPath := stagedPath + ".old"
	hadOld := true
	if err := moveIntoPlace(oldPath, backupPath); errors.Is(err, fs.ErrNotExist) {
		hadOld = false
	} else if err != nil {
		dropReservation()
		return nil, fmt.Errorf("move old file aside: %w", err)
	}

	commit := func(tx *sql.Tx) error {
		// phash and the auto-tag stamp go with the old bytes: the backfill
		// and the nightly auto-tag revisit a NULL, never a stale one.
		if _, err := tx.Exec(
			`UPDATE images SET sha256 = ?, md5 = ?, canonical_path = ?, file_type = ?, file_size = ?,
			        width = ?, height = ?, source_type = ?, phash = NULL, auto_tagged_at = NULL WHERE id = ?`,
			newSHA, newMD5, newPath, newType, fi.Size(), newW, newH, sourceType, imageID,
		); err != nil {
			return fmt.Errorf("update images row: %w", err)
		}
		if _, err := setMissingTx(tx, []int64{imageID}, false); err != nil {
			return fmt.Errorf("mark row present: %w", err)
		}
		// The alias paths still hold the old bytes.
		if _, err := tx.Exec(`DELETE FROM image_paths WHERE image_id = ? AND is_canonical = 0`, imageID); err != nil {
			return fmt.Errorf("drop stale aliases: %w", err)
		}
		// The recorded lookup misses are about the old bytes.
		if err := lookup.DeleteForImage(tx, imageID); err != nil {
			return fmt.Errorf("drop lookup history: %w", err)
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ?, mtime_unix = ?, mtime_nsec = ? WHERE image_id = ? AND is_canonical = 1`,
			newPath, fi.ModTime().Unix(), fi.ModTime().UnixNano(), imageID,
		); err != nil {
			return fmt.Errorf("update canonical path: %w", err)
		}
		if err := replaceGenerationMetadata(context.Background(), tx, imageID, sdMeta, comfyMeta); err != nil {
			return err
		}
		if oldW != nil && oldH != nil && newW != nil && newH != nil &&
			*oldW > 0 && *oldH > 0 && (*oldW != *newW || *oldH != *newH) {
			rw := float64(*newW) / float64(*oldW)
			rh := float64(*newH) / float64(*oldH)
			if _, err := tx.Exec(
				`UPDATE image_annotations SET
				   x = CAST(ROUND(x * ?) AS INTEGER), w = CAST(ROUND(w * ?) AS INTEGER),
				   y = CAST(ROUND(y * ?) AS INTEGER), h = CAST(ROUND(h * ?) AS INTEGER)
				 WHERE image_id = ?`, rw, rw, rh, rh, imageID,
			); err != nil {
				return fmt.Errorf("scale annotations: %w", err)
			}
		}
		return nil
	}
	if err := db.InWriteTx(database.Write, commit); err != nil {
		dropReservation()
		if hadOld {
			if rbErr := moveIntoPlace(backupPath, oldPath); rbErr != nil {
				logx.Warnf("replace: restore of %q failed after aborted swap: %v", oldPath, rbErr)
			}
		}
		return nil, err
	}

	// The staged file is a 0600 os.CreateTemp, and the rename would carry
	// that mode into the gallery.
	if err := os.Chmod(stagedPath, 0o644); err != nil {
		logx.Warnf("replace: chmod staged file %q: %v", stagedPath, err)
	}
	if err := moveIntoPlace(stagedPath, newPath); err != nil {
		putOldFileBack(database, galleryPath, imageID, oldPath, newPath, backupPath, hadOld)
		return nil, fmt.Errorf("place replaced file: %w", err)
	}
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		logx.Warnf("replace: removing backup %q: %v", backupPath, err)
	}

	stored := regenerateDerived(database, thumbnailsPath, newPath, imageID, newType, "replace")
	if err := ApplyMetaTags(h, imageID); err != nil {
		logx.Warnf("replace: meta tags for %q: %v", newPath, err)
	}
	logx.Infof("replace: image id=%d now %q (sha %s)", imageID, newPath, newSHA)
	return stored, nil
}

// The committed row names bytes that never landed. With the old file back
// where the row points, the watcher or the next sync re-derives the row
// from it, tags kept, rather than both versions being lost.
func putOldFileBack(database *db.DB, galleryPath string, imageID int64, oldPath, newPath, backupPath string, hadOld bool) {
	if newPath != oldPath {
		if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			// No mtime: the row names the new bytes, so the old file must be hashed.
			return repointCanonical(tx, imageID, oldPath, FolderPath(galleryPath, oldPath), newPath, 0, 0)
		}); err != nil {
			logx.Warnf("replace: pointing image %d back at %q: %v", imageID, oldPath, err)
		}
		if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
			logx.Warnf("replace: removing reservation %q: %v", newPath, err)
		}
	}
	if !hadOld {
		return
	}
	if err := moveIntoPlace(backupPath, oldPath); err != nil {
		logx.Warnf("replace: putting %q back after a failed placement: %v", oldPath, err)
	}
}

// moveIntoPlace copies across devices (the data and gallery mounts, a
// linked folder). A source that cannot go undoes the copy: a file in two
// places is worse than one not moved.
func moveIntoPlace(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	// Only EXDEV: a copy that succeeds where the rename failed turns an
	// atomic move into a copy plus an unlink.
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := CopyFileContents(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func CopyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
