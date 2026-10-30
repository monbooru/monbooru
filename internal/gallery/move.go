package gallery

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// MoveImageResult's Moved and Renamed report what actually changed, not
// what the caller asked for; PrevDir is set only when the file left its
// folder.
type MoveImageResult struct {
	NewCanonicalPath string
	NewFolderPath    string
	PrevDir          string
	Moved            bool
	Renamed          bool
	Suffixed         bool
}

type placement struct {
	oldCanonical string
	destDir      string
	newFolder    string
	base         string
	moved        bool
	renamed      bool
}

// plan touches nothing on disk, so a dry run can share it with the run.
func plan(database *db.DB, b *Boundary, id int64, folder, name *string) (placement, error) {
	var p placement
	if name != nil {
		base := strings.TrimSpace(*name)
		if base != filepath.Base(base) || base == "." || !filepath.IsLocal(base) {
			return p, fmt.Errorf("invalid file name %q", base)
		}
		p.base = TruncateFilename(base, maxNameBytes)
	}

	oldCanonical, oldFolder, err := loadMoveSource(database, b, id)
	if err != nil {
		return p, err
	}
	p.oldCanonical = oldCanonical

	// The destination is already root-bounded by ResolveSubdir.
	p.destDir, p.newFolder = filepath.Dir(oldCanonical), oldFolder
	if folder != nil {
		dir, resolveErr := b.ResolveSubdir(*folder)
		if resolveErr != nil {
			return p, resolveErr
		}
		rel, relErr := filepath.Rel(b.Root(), dir)
		if relErr != nil {
			return p, fmt.Errorf("resolve folder: %w", relErr)
		}
		if rel == "." {
			rel = ""
		}
		p.destDir, p.newFolder = dir, filepath.ToSlash(rel)
	}

	if name != nil {
		if ext := filepath.Ext(oldCanonical); !strings.EqualFold(filepath.Ext(p.base), ext) {
			p.base += ext
		}
	} else {
		p.base = filepath.Base(oldCanonical)
	}

	if err := b.Check(filepath.Join(p.destDir, p.base)); err != nil {
		return p, err
	}
	p.moved, p.renamed = p.newFolder != oldFolder, p.base != filepath.Base(oldCanonical)
	return p, nil
}

// PlannedPath writes nothing; one planned map passed through a run's rows in
// order sees each earlier destination taken and each path it frees free.
// numbered reports that the destination was taken.
func PlannedPath(database *db.DB, b *Boundary, id int64, folder, name *string, planned map[string]bool) (path string, numbered bool, err error) {
	p, err := plan(database, b, id, folder, name)
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(p.destDir, p.base)
	if !p.moved && !p.renamed {
		planned[path] = true
		return path, false, nil
	}
	resolved := uniquePathIn(p.destDir, p.base, planned, placementSuffix(p.renamed))
	// A path an earlier row frees still holds that row's canonical here.
	if _, freed := planned[resolved]; !freed {
		if err := refuseAliasCollision(database, id, resolved); err != nil {
			return "", false, err
		}
	}
	planned[p.oldCanonical] = false
	planned[resolved] = true
	return resolved, resolved != path, nil
}

// Follows reports that the row already sits where folder and name put it.
// A number a taken name earned counts as following, or every re-run would
// renumber the row against its own file.
func Follows(database *db.DB, b *Boundary, id int64, folder, name *string) (bool, error) {
	p, err := plan(database, b, id, folder, name)
	if err != nil || p.moved {
		return false, err
	}
	if !p.renamed {
		return true, nil
	}
	if !numberedFrom(filepath.Base(p.oldCanonical), p.base) {
		return false, nil
	}
	// img2024.png reads as img.png numbered 2024 until img.png is there.
	_, err = os.Stat(filepath.Join(p.destDir, p.base))
	return err == nil, nil
}

func numberedFrom(base, want string) bool {
	ext := filepath.Ext(want)
	if filepath.Ext(base) != ext {
		return false
	}
	n, ok := strings.CutPrefix(strings.TrimSuffix(base, ext), strings.TrimSuffix(want, ext))
	if !ok {
		return false
	}
	if moveN, isMove := strings.CutPrefix(n, "_"); isMove {
		n = moveN
	} else if len(n) < 2 {
		return false
	}
	return n != "" && strings.Trim(n, "0123456789") == ""
}

// PlaceImage leaves alone whichever of folder and name is nil. Callers
// holding a watcher must run it under a job type the watcher suppresses,
// or its events race the DB update.
func PlaceImage(database *db.DB, b *Boundary, id int64, folder, name *string) (*MoveImageResult, error) {
	p, err := plan(database, b, id, folder, name)
	if err != nil {
		return nil, err
	}
	oldCanonical, destDir, newFolder, base := p.oldCanonical, p.destDir, p.newFolder, p.base
	moved, renamed := p.moved, p.renamed
	if !moved && !renamed {
		return &MoveImageResult{
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    newFolder,
		}, nil
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create destination folder: %w", err)
	}

	newPath, err := createUniqueBy(destDir, base, placementSuffix(renamed))
	if err != nil {
		return nil, fmt.Errorf("reserve destination: %w", err)
	}
	if err := refuseAliasCollision(database, id, newPath); err != nil {
		_ = os.Remove(newPath)
		return nil, err
	}
	folderArg := &newFolder
	if folder == nil {
		folderArg = nil
	}
	if err := commitRename(database, id, oldCanonical, newPath, folderArg); err != nil {
		return nil, err
	}

	res := &MoveImageResult{
		NewCanonicalPath: newPath,
		NewFolderPath:    newFolder,
		Moved:            moved,
		Renamed:          renamed,
		Suffixed:         newPath != filepath.Join(destDir, base),
	}
	if moved {
		res.PrevDir = filepath.Dir(oldCanonical)
	}
	return res, nil
}

// An empty dropOldPath demotes the current canonical to an alias instead
// of dropping it, as a reactivation wants.
func repointCanonical(tx *sql.Tx, id int64, newPath, newFolder, dropOldPath string, mtime, mtimeNano int64) error {
	var owner int64
	switch err := tx.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ? AND image_id != ?`, newPath, id,
	).Scan(&owner); {
	case err == nil:
		return fmt.Errorf("%q is a path of image %d", newPath, owner)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check the new path's owner: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newPath, newFolder, id,
	); err != nil {
		return fmt.Errorf("point row at the new path: %w", err)
	}
	if _, err := setMissingTx(tx, []int64{id}, false); err != nil {
		return fmt.Errorf("mark row present: %w", err)
	}
	if dropOldPath == "" {
		if _, err := tx.Exec(
			`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND is_canonical = 1`, id,
		); err != nil {
			return fmt.Errorf("demote previous canonical: %w", err)
		}
	} else if _, err := tx.Exec(
		`DELETE FROM image_paths WHERE image_id = ? AND path = ?`, id, dropOldPath,
	); err != nil {
		return fmt.Errorf("drop old canonical: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 1, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET is_canonical = 1, mtime_unix = excluded.mtime_unix, mtime_nsec = excluded.mtime_nsec`,
		id, newPath, mtime, mtimeNano,
	); err != nil {
		return fmt.Errorf("install new canonical: %w", err)
	}
	return nil
}

// setMissingTx flips only the rows whose flag differs, so their tags'
// usage moves once per image.
func setMissingTx(tx *sql.Tx, ids []int64, missing bool) (int, error) {
	var flipped []int64
	if err := db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		got, err := db.QueryIDs(tx, `UPDATE images SET is_missing = ? WHERE id IN (`+in+`) AND is_missing != ? RETURNING id`,
			append(append([]any{missing}, args...), missing)...)
		flipped = append(flipped, got...)
		return err
	}); err != nil {
		return 0, err
	}
	return len(flipped), tags.ImagesVisibilityChangedTx(tx, flipped, !missing)
}

// SetMissing reports whether the row's flag changed.
func SetMissing(database *db.DB, id int64, missing bool) (bool, error) {
	var n int
	err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		var err error
		n, err = setMissingTx(tx, []int64{id}, missing)
		return err
	})
	return n > 0, err
}

func loadMoveSource(database *db.DB, b *Boundary, id int64) (oldCanonical, oldFolder string, err error) {
	galleryPath := b.Root()
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&oldCanonical, &oldFolder, &isMissing); err != nil {
		return "", "", fmt.Errorf("image %d not found: %w", id, err)
	}
	if isMissing == 1 {
		return "", "", fmt.Errorf("image %d is missing from disk", id)
	}
	if galleryPath != "" && !PathInside(galleryPath, oldCanonical) {
		return "", "", fmt.Errorf("refusing to place %q outside gallery root %q", oldCanonical, galleryPath)
	}
	if err := b.Check(oldCanonical); err != nil {
		return "", "", fmt.Errorf("refusing to place image %d: %w", id, err)
	}
	return oldCanonical, oldFolder, nil
}

// The unique helpers check only the disk: a stale alias row holding the path
// would otherwise fail the UNIQUE constraint mid-tx with no useful error.
func refuseAliasCollision(database *db.DB, id int64, newPath string) error {
	var collidingImage int64
	switch err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ? AND image_id != ?`,
		newPath, id,
	).Scan(&collidingImage); {
	case err == nil:
		return fmt.Errorf("destination collides with an existing alias on image %d", collidingImage)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("check destination for an alias collision: %w", err)
	}
}

// The file moves before the tx opens: across filesystems the move is a full
// copy, which would hold the only write connection throughout. newPath is
// the caller's reservation, which the move replaces.
func commitRename(database *db.DB, id int64, oldCanonical, newPath string, newFolder *string) error {
	if err := moveIntoPlace(oldCanonical, newPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("rename file: %w", err)
	}
	err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		update, args := `UPDATE images SET canonical_path = ? WHERE id = ?`, []any{newPath, id}
		if newFolder != nil {
			update = `UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`
			args = []any{newPath, *newFolder, id}
		}
		if _, err := tx.Exec(update, args...); err != nil {
			return fmt.Errorf("update images row: %w", err)
		}
		if _, err := tx.Exec(
			`DELETE FROM image_paths WHERE image_id = ? AND path = ? AND is_canonical = 0`, id, newPath,
		); err != nil {
			return fmt.Errorf("drop the image's own alias at the new path: %w", err)
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
			newPath, id,
		); err != nil {
			return fmt.Errorf("update image_paths row: %w", err)
		}
		return nil
	})
	if err != nil {
		if rnErr := moveIntoPlace(newPath, oldCanonical); rnErr != nil {
			logx.Errorf("place: reverse rename for %d after a failed update: %v (original: %v)", id, rnErr, err)
		}
		return err
	}
	return nil
}

func renameSuffix(stem, ext string, i int) string { return fmt.Sprintf("%s%02d%s", stem, i, ext) }

// renamed must say whether the name actually changes, not whether one was
// given.
func placementSuffix(renamed bool) func(stem, ext string, i int) string {
	if renamed {
		return renameSuffix
	}
	return uploadSuffix
}

func PromoteCanonicalByPath(database *db.DB, b *Boundary, imageID int64, newPath string) error {
	if err := b.Check(newPath); err != nil {
		return err
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, imageID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`, imageID, newPath); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
			newPath, FolderPath(b.Root(), newPath), imageID)
		return err
	})
}

// DeleteAliasPaths only forgets the rows; the files are the caller's to unlink.
func DeleteAliasPaths(database *db.DB, pathIDs []int64) error {
	placeholders, args := db.InPlaceholders(pathIDs)
	_, err := database.Write.Exec(`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...)
	return err
}
