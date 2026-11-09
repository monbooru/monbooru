package galleryio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio/compatibility"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// TransferOneImage merges into a target row with the same sha, else
// copies and ingests the file; removeAfter then deletes the source, whose
// files changed since indexing it keeps and counts.
func TransferOneImage(srcCx, dstCx *library.Gallery, id int64, removeAfter bool) (kept int, err error) {
	var canonPath, folderPath, origin, note, originalSource string
	var isFav, isMissing int
	var indexed gallery.Indexed
	if err := srcCx.DB.Read.QueryRow(
		`SELECT i.sha256, i.canonical_path, i.folder_path, i.origin, i.note, i.original_source, i.is_favorited, i.is_missing,
		        i.file_size, COALESCE(ip.mtime_unix, 0), COALESCE(ip.mtime_nsec, 0)
		   FROM images i LEFT JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 1
		  WHERE i.id = ?`, id,
	).Scan(&indexed.SHA256, &canonPath, &folderPath, &origin, &note, &originalSource, &isFav, &isMissing,
		&indexed.Size, &indexed.Mtime, &indexed.MtimeNano); err != nil {
		return 0, fmt.Errorf("load image %d: %w", id, err)
	}
	sha := indexed.SHA256
	srcInfo, statErr := os.Stat(canonPath)
	if isMissing == 1 || statErr != nil {
		return 0, fmt.Errorf("file missing from disk")
	}

	groups, err := transferTagGroups(srcCx.Handle, dstCx.Handle, id)
	if err != nil {
		return 0, err
	}
	generalID := LookupCategoryID(dstCx.DB, "general")

	var dstID int64
	var dstMissing int
	var dstCanon string
	var dstSize int64
	lookupErr := dstCx.DB.Read.QueryRow(`SELECT id, is_missing, canonical_path, file_size FROM images WHERE sha256 = ?`, sha).Scan(&dstID, &dstMissing, &dstCanon, &dstSize)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return 0, fmt.Errorf("target sha lookup: %w", lookupErr)
	}
	// file_size 0 marks a row whose bytes were never read: an import placeholder.
	markPresent := func() error {
		if dstSize == 0 {
			return dstCx.ReadPlaceholder(dstID, dstCanon)
		}
		_, err := gallery.SetMissing(dstCx.DB, dstID, false)
		return err
	}
	// The provenance is written outside the landing and the tag service.
	if err := dstCx.Edit(func(*db.DB) error {
		switch lookupErr {
		case nil:
			same := gallery.SameFile(srcInfo, dstCanon)
			if same && removeAfter {
				return errSameFileMove
			}
			// The flag trails the disk until the next sync.
			_, dstStatErr := os.Stat(dstCanon)
			if dstMissing == 1 && !os.IsNotExist(dstStatErr) {
				got, err := gallery.HashFile(dstCanon)
				if err != nil {
					return fmt.Errorf("read the target's file %q: %w", dstCanon, err)
				}
				if got != sha {
					return fmt.Errorf("the target's file %q now holds other bytes; sync the target first", dstCanon)
				}
				if err := markPresent(); err != nil {
					return err
				}
			}
			// Restore a missing file, or the merge passes on an unviewable row.
			if os.IsNotExist(dstStatErr) {
				if !gallery.NamedInside(dstCx.GalleryPath, dstCanon) {
					return fmt.Errorf("refusing to restore %q outside gallery root %q", dstCanon, dstCx.GalleryPath)
				}
				if err := dstCx.Boundary().Check(dstCanon); err != nil {
					return fmt.Errorf("refusing to restore %q: %w", dstCanon, err)
				}
				if err := gallery.ChangedSinceIndexed(canonPath, indexed); err != nil {
					return fmt.Errorf("refusing to restore from %q: %w", canonPath, err)
				}
				if err := os.MkdirAll(filepath.Dir(dstCanon), 0o755); err != nil {
					return fmt.Errorf("mkdir dest: %w", err)
				}
				if err := gallery.CopyFileContents(canonPath, dstCanon); err != nil {
					return fmt.Errorf("restore file: %w", err)
				}
				if err := markPresent(); err != nil {
					return err
				}
			}
		case sql.ErrNoRows:
			rel := filepath.ToSlash(filepath.Join(folderPath, filepath.Base(canonPath)))
			safeBase, err := SafeArchiveDest(dstCx.GalleryPath, rel)
			if err != nil {
				return err
			}
			var landed library.LandResult
			if gallery.SameFile(srcInfo, safeBase) {
				// Both galleries read this folder: a copy beside it would be
				// a second file both of them index.
				if removeAfter {
					return errSameFileMove
				}
				landed, err = dstCx.LandInPlace(context.Background(), safeBase, library.LandSpec{Origin: origin})
			} else {
				landed, err = dstCx.Land(context.Background(), filepath.Dir(safeBase), filepath.Base(safeBase),
					library.FromFile(canonPath), library.LandSpec{Origin: origin})
			}
			if err != nil {
				return err
			}
			// The new row stays in the inbox even on a move: relations and
			// collections don't travel.
			dstID = landed.Image.ID
		}
		if _, err := applyTransferTags(dstCx.DB, dstCx.TagSvc, dstID, groups, generalID); err != nil {
			return err
		}
		return transferProvenance(srcCx.Handle, dstCx.Handle, id, dstID, note, originalSource, isFav)
	}); err != nil {
		return 0, err
	}

	if removeAfter {
		res, err := srcCx.DeleteImages(context.Background(), []int64{id}, library.DeleteOptions{}, nil)
		if err != nil {
			return 0, fmt.Errorf("remove source: %w", err)
		}
		return res.Changed, nil
	}
	return 0, nil
}

var errSameFileMove = errors.New("the target gallery reads this same file: moving it would delete it")

type transferTagGroup struct {
	isAuto     bool
	taggerName string
	tokens     []string
	confs      []*float64 // aligned with tokens; nil entry means no score
}

// Custom categories the target lacks are created first, or their tags
// would land in general; built-ins exist everywhere.
func transferTagGroups(srcCx, dstCx gallery.Handle, id int64) ([]transferTagGroup, error) {
	rows, err := srcCx.DB.Read.Query(
		`SELECT t.name, tc.name, tc.color, tc.is_builtin, it.is_auto, it.tagger_name, it.confidence
		 FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.is_alias = 0 AND it.is_implied = 0`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type attrKey struct {
		isAuto     bool
		taggerName string
	}
	idx := map[attrKey]int{}
	var groups []transferTagGroup
	for rows.Next() {
		var name, cat, color string
		var builtin, isAuto int
		var taggerName sql.NullString
		var conf *float64
		if err := rows.Scan(&name, &cat, &color, &builtin, &isAuto, &taggerName, &conf); err != nil {
			return nil, err
		}
		if builtin == 0 && LookupCategoryID(dstCx.DB, cat) == 0 {
			if _, err := dstCx.TagSvc.CreateCategory(cat, color); err != nil {
				logx.Warnf("transfer: create category %q: %v", cat, err)
			}
		}
		k := attrKey{isAuto: isAuto == 1, taggerName: taggerName.String}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, transferTagGroup{isAuto: k.isAuto, taggerName: k.taggerName})
		}
		groups[i].tokens = append(groups[i].tokens, compatibility.TagToken(cat, name))
		groups[i].confs = append(groups[i].confs, conf)
	}
	return groups, rows.Err()
}

// Fills only an empty note or original source and only ever raises
// is_favorited, so an existing target row keeps what it curated.
func transferProvenance(srcCx, dstCx gallery.Handle, srcID, dstID int64, note, originalSource string, isFav int) error {
	sources, err := gallery.SourcesForImage(srcCx.DB, srcID)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if src.Site == "" && src.URL == "" {
			continue
		}
		if err := gallery.AddSourceMembership(dstCx.DB, dstID, src.Site, src.PostID, src.URL); err != nil {
			return err
		}
		if err := gallery.SetSourceSimilarity(dstCx.DB, dstID, src.Site, src.PostID, src.Similarity); err != nil {
			return err
		}
		if _, err := gallery.ApplySourceProvenance(dstCx.DB, dstID, src.Site, src.PostID, src.Commentary, src.CommentaryTranslated, src.Original, nil); err != nil {
			return err
		}
	}

	anns, err := gallery.AnnotationsForImage(srcCx.DB, srcID)
	if err != nil {
		return err
	}
	// Manual boxes have no source key, so dedupe on geometry and body to
	// keep a re-transfer idempotent.
	dstAnns, err := gallery.AnnotationsForImage(dstCx.DB, dstID)
	if err != nil {
		return err
	}
	type manualBox struct {
		x, y, w, h int
		body       string
	}
	haveManual := map[manualBox]bool{}
	for _, a := range dstAnns {
		if a.Manual {
			haveManual[manualBox{a.X, a.Y, a.W, a.H, a.Body}] = true
		}
	}
	byKey := map[[2]string][]models.Annotation{}
	var order [][2]string
	for _, a := range anns {
		if a.Manual {
			box := manualBox{a.X, a.Y, a.W, a.H, a.Body}
			if haveManual[box] {
				continue
			}
			if err := gallery.AddManualAnnotation(dstCx.DB, dstID, a.X, a.Y, a.W, a.H, a.Body); err != nil {
				return err
			}
			haveManual[box] = true
			continue
		}
		k := [2]string{a.Site, a.PostID}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], a)
	}
	for _, k := range order {
		if err := gallery.ReplaceSourceAnnotations(dstCx.DB, dstID, k[0], k[1], byKey[k]); err != nil {
			return err
		}
	}

	if _, err := dstCx.DB.Write.Exec(
		`UPDATE images SET note = CASE WHEN note = '' THEN ? ELSE note END,
		                   original_source = CASE WHEN original_source = '' THEN ? ELSE original_source END,
		                   is_favorited = MAX(is_favorited, ?) WHERE id = ?`, note, originalSource, isFav, dstID); err != nil {
		return err
	}
	return nil
}
