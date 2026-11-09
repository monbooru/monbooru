package tags

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/monbooru/monbooru/internal/db"
)

type TaggerHit struct {
	Name       string
	CategoryID int64
	Score      float32
	Tagger     string
}

// ApplyTaggerResults makes the image's auto rows what the run emitted:
// only rows from the run's own taggers are dropped.
func (s *Service) ApplyTaggerResults(ctx context.Context, imageID int64, hits []TaggerHit, taggers []string) error {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Aliases resolve to their canonical: an alias never goes on an image.
	type target struct {
		score      float32
		taggerName string
	}
	targets := make(map[int64]target, len(hits))
	hasRating := false
	manualRated := false
	if s.ratingCatID != 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND t.category_id = ? AND it.is_auto = 0)`, imageID, s.ratingCatID).Scan(&manualRated); err != nil {
			return err
		}
	}
	for _, h := range hits {
		if s.ratingCatID != 0 && h.CategoryID == s.ratingCatID {
			// A mapping rule can name anything; only the four levels rate.
			if !IsCanonicalRating(h.Name) || manualRated {
				continue
			}
			hasRating = true
		}
		var tagID, resolvedCat int64
		var isAlias, rejected int
		var canonicalID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT t.id, t.is_alias, t.canonical_tag_id, COALESCE(c.category_id, t.category_id), COALESCE(c.rejected, t.rejected)
			 FROM tags t LEFT JOIN tags c ON c.id = t.canonical_tag_id
			 WHERE t.name = ? AND t.category_id = ?`, h.Name, h.CategoryID,
		).Scan(&tagID, &isAlias, &canonicalID, &resolvedCat, &rejected)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count, origin) VALUES (?, ?, 0, ?)`, h.Name, h.CategoryID, h.Tagger)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", h.Name, h.CategoryID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", h.Name, h.CategoryID, err)
		} else if rejected == 1 {
			if err := noteRejectedDrop(tx, cmp.Or(canonicalID.Int64, tagID), h.Tagger); err != nil {
				return err
			}
			continue
		} else if isAlias == 1 && canonicalID.Valid {
			tagID = canonicalID.Int64
			if s.ratingCatID != 0 && resolvedCat == s.ratingCatID && h.CategoryID != s.ratingCatID {
				if manualRated {
					continue
				}
				hasRating = true
			}
		}
		if prev, ok := targets[tagID]; !ok || h.Score > prev.score {
			targets[tagID] = target{score: h.Score, taggerName: h.Tagger}
		}
	}

	type rowInfo struct {
		isAuto, isImplied bool
		taggerName        string
	}
	current := map[int64]rowInfo{}
	type tagRow struct {
		id int64
		rowInfo
	}
	existing, err := db.QueryAllContext(ctx, tx, func(rows *sql.Rows) (tagRow, error) {
		var r tagRow
		var isAuto, isImplied int
		var tname sql.NullString
		err := rows.Scan(&r.id, &isAuto, &isImplied, &tname)
		r.isAuto, r.isImplied, r.taggerName = isAuto == 1, isImplied == 1, tname.String
		return r, err
	}, `SELECT tag_id, is_auto, is_implied, tagger_name FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	for _, r := range existing {
		current[r.id] = r.rowInfo
	}

	toRemove := map[int64]struct{}{}
	if len(taggers) > 0 {
		scope := make(map[string]struct{}, len(taggers))
		for _, n := range taggers {
			scope[n] = struct{}{}
		}
		for tid, info := range current {
			if !info.isAuto {
				continue
			}
			if _, ok := scope[info.taggerName]; !ok {
				continue
			}
			if _, keep := targets[tid]; keep {
				continue
			}
			toRemove[tid] = struct{}{}
		}
	}
	toAdd := map[int64]target{}
	for tid, t := range targets {
		if _, exists := current[tid]; !exists {
			toAdd[tid] = t
		}
	}

	// Demoted, not deleted: a tag left on the image may still imply it.
	for tid := range toRemove {
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_tags SET is_implied = 1, confidence = NULL, tagger_name = NULL
			 WHERE image_id = ? AND tag_id = ? AND is_auto = 1`, imageID, tid); err != nil {
			return fmt.Errorf("demote auto tag %d: %w", tid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM image_tag_sources WHERE image_id = ? AND tag_id = ?`, imageID, tid); err != nil {
			return fmt.Errorf("drop tag sources for tag %d: %w", tid, err)
		}
	}

	for tid, t := range targets {
		info, exists := current[tid]
		if !exists || (!info.isAuto && !info.isImplied) {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_tags SET is_implied = 0, is_auto = 1, confidence = ?, tagger_name = NULLIF(?, '')
			 WHERE image_id = ? AND tag_id = ? AND (is_auto = 1 OR is_implied = 1)`,
			t.score, t.taggerName, imageID, tid); err != nil {
			return fmt.Errorf("refresh attribution for tag %d: %w", tid, err)
		}
	}

	// Existing rows are recorded too: the ledger captures a tagger
	// re-confirming a tag.
	for tid, t := range targets {
		if err := recordTagSourceTx(tx, imageID, tid, t.taggerName); err != nil {
			return fmt.Errorf("record tag source %d: %w", tid, err)
		}
	}

	var inserted []int64
	for tid, t := range toAdd {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 1, 0, ?, NULLIF(?, ''))`,
			imageID, tid, t.score, t.taggerName)
		if err != nil {
			return fmt.Errorf("insert auto tag %d: %w", tid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := bumpTagUsageTx(tx, tid, imageID); err != nil {
			return fmt.Errorf("increment usage for tag %d: %w", tid, err)
		}
		inserted = append(inserted, tid)
	}
	// After every insert: a parent fanned out first would leave its emitted child implied.
	for _, tid := range inserted {
		if err := fanOutImpliedTxImpl(tx, imageID, tid, s.ratingCatID, 1); err != nil {
			return fmt.Errorf("fan out implications for tag %d: %w", tid, err)
		}
	}

	// Before the rating prune: a demoted rating would outrank the run's new one.
	if len(toRemove) > 0 {
		if _, err := pruneOrphanedImpliedTx(tx, []int64{imageID}); err != nil {
			return fmt.Errorf("prune orphaned implied tags: %w", err)
		}
	}

	// A tagger can emit several ratings in one pass; keep the highest, as
	// search resolves it.
	if hasRating {
		if err := pruneLowerRatingsTx(tx, s.ratingCatID, imageID); err != nil {
			return fmt.Errorf("prune lower ratings: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE images SET auto_tagged_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), imageID); err != nil {
		return fmt.Errorf("stamp auto_tagged_at on image %d: %w", imageID, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed()
	return nil
}
