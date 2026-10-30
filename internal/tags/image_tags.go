package tags

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func (s *Service) AddTagToImage(imageID, tagID int64, isAuto bool, confidence *float64) error {
	_, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, "")
	return err
}

func (s *Service) AddTagsToImageFromTagger(imageID int64, tagIDs []int64, isAuto bool, taggerName string) error {
	return s.AddTagsToImageFromTaggerConf(imageID, tagIDs, nil, isAuto, taggerName)
}

// AddTagsToImageFromTaggerConf pairs confs[i] with tagIDs[i]; a nil or
// missing entry stores NULL.
func (s *Service) AddTagsToImageFromTaggerConf(imageID int64, tagIDs []int64, confs []*float64, isAuto bool, taggerName string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		_, err := addTagsTx(tx, imageID, tagIDs, confs, isAuto, taggerName, s.ratingCatID)
		return err
	})
}

func addTagsTx(tx *sql.Tx, imageID int64, tagIDs []int64, confs []*float64, isAuto bool, via string, ratingCatID int64) ([]AddResult, error) {
	results := make([]AddResult, 0, len(tagIDs))
	for i, tagID := range tagIDs {
		var c *float64
		if i < len(confs) {
			c = confs[i]
		}
		added, promoted, displaced, err := addOneTagTx(tx, imageID, tagID, isAuto, c, via, ratingCatID)
		if err != nil {
			return results, err
		}
		results = append(results, AddResult{Added: added, Promoted: promoted, DisplacedRatings: displaced})
	}
	return results, nil
}

func addOneTagTx(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string, ratingCatID int64) (added, promoted bool, displaced []string, err error) {
	added, promoted, err = addTagToImageTxReportingDup(tx, imageID, tagID, isAuto, confidence, taggerName, ratingCatID)
	if err != nil {
		return false, false, nil, err
	}
	if added || promoted {
		if displaced, err = pruneRatingsAfterAddTx(tx, ratingCatID, imageID, tagID, isAuto); err != nil {
			return false, false, nil, err
		}
	}
	return added, promoted, displaced, nil
}

type AddResult struct {
	Added            bool
	Promoted         bool
	DisplacedRatings []string
}

func (s *Service) AddTagToImageReportingDup(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (AddResult, error) {
	return writeTx(s, func(tx *sql.Tx) (AddResult, error) {
		out, err := addTagsTx(tx, imageID, []int64{tagID}, []*float64{confidence}, isAuto, taggerName, s.ratingCatID)
		if err != nil {
			return AddResult{}, err
		}
		return out[0], nil
	})
}

func addTagToImageTxReportingDup(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string, ratingCatID int64) (bool, bool, error) {
	isAutoInt := 0
	if isAuto {
		isAutoInt = 1
	}
	// tagger_name is the source label on manual rows too; NULL means a UI add.
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, ?, 0, ?, NULLIF(?, ''))`,
		imageID, tagID, isAutoInt, confidence, taggerName,
	)
	if err != nil {
		return false, false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	var promoted int64
	if added == 0 {
		// Promote, never demote: implied < auto < manual, so a promoted
		// implied row survives its parent's removal.
		upd, err := tx.Exec(
			`UPDATE image_tags SET is_implied = 0, is_auto = ?, confidence = ?, tagger_name = NULLIF(?, '')
			 WHERE image_id = ? AND tag_id = ?
			   AND (is_implied = 1 OR (is_auto = 1 AND ? = 0))`,
			isAutoInt, confidence, taggerName, imageID, tagID, isAutoInt,
		)
		if err != nil {
			return false, false, err
		}
		promoted, _ = upd.RowsAffected()
	} else {
		if err := bumpTagUsageTx(tx, tagID, imageID); err != nil {
			return false, false, err
		}
	}

	// A bare UI re-add of a present tag must not stamp a phantom 'user'
	// source, except over a tag only the derivation claims: Remove meta
	// tags would take a tag the operator typed.
	reclaimed := false
	if added == 0 && promoted == 0 && taggerName == "" {
		if err := tx.QueryRow(
			`SELECT COALESCE(MIN(source) = ?1 AND MAX(source) = ?1, 0) FROM image_tag_sources WHERE image_id = ?2 AND tag_id = ?3`,
			models.TagSourceMonbooru, imageID, tagID,
		).Scan(&reclaimed); err != nil {
			return false, false, err
		}
	}
	if added > 0 || promoted > 0 || taggerName != "" || reclaimed {
		if err := recordTagSourceTx(tx, imageID, tagID, taggerName); err != nil {
			return false, false, err
		}
	}

	if err := fanOutImpliedTxImpl(tx, imageID, tagID, ratingCatID, isAutoInt); err != nil {
		return false, false, err
	}

	if added == 0 {
		return false, promoted > 0 || reclaimed, nil
	}
	return true, false, nil
}

func transitiveImpliedTx(tx *sql.Tx, parents []int64) ([]int64, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	var out []int64
	err := bfsImpliedTx(tx, parents, func(id int64) bool {
		out = append(out, id)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) AddTagsToOneImage(imageID int64, tagIDs []int64, via string) ([]AddResult, error) {
	if len(tagIDs) == 0 {
		return nil, nil
	}
	return writeTx(s, func(tx *sql.Tx) ([]AddResult, error) {
		return addTagsTx(tx, imageID, tagIDs, nil, false, via, s.ratingCatID)
	})
}

type SyncResult struct {
	Added        int
	Retired      int
	RatingFilled bool
}

// A tag the source no longer lists is flagged stale, never removed. Pass
// reconcile false when the site has several posts on the image: they
// share one slice.
func (s *Service) SyncSourceTags(imageID int64, tagIDs []int64, site string, reconcile bool) (SyncResult, error) {
	if site == "" {
		return SyncResult{}, errors.New("source label required")
	}
	incoming := make(map[int64]bool, len(tagIDs))
	for _, id := range tagIDs {
		incoming[id] = true
	}
	return writeTx(s, func(tx *sql.Tx) (SyncResult, error) {
		alreadyRated := false
		if s.ratingCatID != 0 {
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
				 WHERE it.image_id = ? AND t.category_id = ?)`, imageID, s.ratingCatID).Scan(&alreadyRated); err != nil {
				return SyncResult{}, err
			}
		}

		type sourceTag struct {
			id       int64
			isRating bool
		}
		var listed []sourceTag
		// A PTR hash can carry several ratings: the highest lands, as an
		// auto-tagger's does.
		var rating int64
		bestRank := -2
		for _, tagID := range tagIDs {
			var cat int64
			var name string
			switch err := tx.QueryRow(`SELECT category_id, name FROM tags WHERE id = ?`, tagID).Scan(&cat, &name); {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return SyncResult{}, err
			}
			isRating := s.ratingCatID != 0 && cat == s.ratingCatID
			if isRating && contract.RatingRank(name) > bestRank {
				rating, bestRank = tagID, contract.RatingRank(name)
			}
			listed = append(listed, sourceTag{tagID, isRating})
		}

		var res SyncResult
		var applied []int64
		for _, t := range listed {
			if t.isRating && (alreadyRated || t.id != rating) {
				continue
			}
			added, err := addSourceTagTx(tx, imageID, t.id, site)
			if err != nil {
				return SyncResult{}, err
			}
			applied = append(applied, t.id)
			if added {
				res.Added++
				res.RatingFilled = res.RatingFilled || t.isRating
			}
		}
		// After every insert: a parent fanned out first would leave its listed child implied.
		for _, tagID := range applied {
			if err := fanOutImpliedTxImpl(tx, imageID, tagID, s.ratingCatID, 0); err != nil {
				return SyncResult{}, err
			}
		}

		for _, tagID := range tagIDs {
			if _, err := tx.Exec(
				`UPDATE image_tags SET stale = 0
				 WHERE image_id = ? AND tag_id = ? AND tagger_name = ? AND stale = 1`,
				imageID, tagID, site); err != nil {
				return SyncResult{}, err
			}
		}
		if !reconcile {
			return res, nil
		}
		// Never flag a rating: the image's rating is protected from the source.
		ratingCat := cmp.Or(s.ratingCatID, -1)
		current, err := db.QueryIDs(tx,
			`SELECT it.tag_id FROM image_tags it JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND it.tagger_name = ? AND it.is_auto = 0
			   AND it.stale = 0 AND t.category_id != ?`,
			imageID, site, ratingCat)
		if err != nil {
			return SyncResult{}, err
		}
		var stale []int64
		for _, tid := range current {
			if !incoming[tid] {
				stale = append(stale, tid)
			}
		}
		for _, tid := range stale {
			if _, err := tx.Exec(
				`UPDATE image_tags SET stale = 1 WHERE image_id = ? AND tag_id = ?`,
				imageID, tid); err != nil {
				return SyncResult{}, err
			}
			res.Retired++
		}
		return res, nil
	})
}

// Insert-only: an existing row keeps its attribution, so the stale
// flagging never reaches a row this source didn't add.
func addSourceTagTx(tx *sql.Tx, imageID, tagID int64, site string) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 0, 0, NULL, ?)`,
		imageID, tagID, site,
	)
	if err != nil {
		return false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	if added > 0 {
		if err := bumpTagUsageTx(tx, tagID, imageID); err != nil {
			return false, err
		}
	}
	if err := recordTagSourceTx(tx, imageID, tagID, site); err != nil {
		return false, err
	}
	return added > 0, nil
}

// touched counts the images a row was added to, ratingsReplaced those
// whose rating the add replaced.
func (s *Service) BatchAddTagsTx(tx *sql.Tx, imageIDs []int64, tagIDs []int64) (added, touched, ratingsReplaced int, err error) {
	for _, imageID := range imageIDs {
		replaced := false
		before := added
		for _, tagID := range tagIDs {
			a, _, displaced, err := addOneTagTx(tx, imageID, tagID, false, nil, "", s.ratingCatID)
			if err != nil {
				return added, touched, ratingsReplaced, err
			}
			if a {
				added++
			}
			if len(displaced) > 0 {
				replaced = true
			}
		}
		if added > before {
			touched++
		}
		if replaced {
			ratingsReplaced++
		}
	}
	return added, touched, ratingsReplaced, nil
}

// A row a parent still justifies is kept and counted, not an error: a
// batch names tags, not rows.
func batchRemoveTagsTx(tx *sql.Tx, imageIDs []int64, tagIDs []int64) (removed, touched, kept int, err error) {
	for _, imageID := range imageIDs {
		n, implied, demoted, err := removeTagsUnlessImpliedTx(tx, imageID, tagIDs)
		removed += n
		kept += len(implied) + demoted
		if n > 0 {
			touched++
		}
		if err != nil {
			return removed, touched, kept, err
		}
	}
	return removed, touched, kept, nil
}

// A row named with its parent is only held until the parent goes, so the
// held rows are checked again once the others are removed; an owned one a
// parent still implies is then demoted.
func removeTagsUnlessImpliedTx(tx *sql.Tx, imageID int64, tagIDs []int64) (removed int, implied []int64, demoted int, err error) {
	var demotedIDs []int64
	pass := func(ids []int64, last bool) ([]int64, error) {
		var held []int64
		for _, tagID := range ids {
			switch justified, err := impliedByParentOnImage(tx, imageID, tagID); {
			case err != nil:
				return nil, err
			case justified:
				held = append(held, tagID)
				continue
			}
			if !last {
				parents, err := implicationParentsOnImageExcluding(tx, imageID, tagID, 0)
				if err != nil {
					return nil, err
				}
				if len(parents) > 0 {
					held = append(held, tagID)
					continue
				}
			}
			n, wasDemoted, err := demoteOrRemoveTagTx(tx, imageID, tagID)
			if err != nil {
				return nil, err
			}
			if wasDemoted {
				demotedIDs = append(demotedIDs, tagID)
			}
			removed += n
		}
		return held, nil
	}
	held, err := pass(tagIDs, false)
	if err != nil || len(held) == 0 {
		return removed, nil, 0, err
	}
	if implied, err = pass(held, true); err != nil || len(demotedIDs) == 0 {
		return removed, implied, 0, err
	}
	// A demoted row whose parent the same pass took went with the sweep.
	in, args := db.InPlaceholders(demotedIDs)
	err = tx.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id IN (`+in+`)`,
		append([]any{imageID}, args...)...).Scan(&demoted)
	return removed, implied, demoted, err
}

func (s *Service) RemoveTagFromImage(imageID, tagID int64) (int, error) {
	return writeTx(s, func(tx *sql.Tx) (int, error) {
		switch implied, err := impliedByParentOnImage(tx, imageID, tagID); {
		case err != nil:
			return 0, err
		case implied:
			return 0, ErrTagImplied
		}
		n, _, err := demoteOrRemoveTagTx(tx, imageID, tagID)
		return n, err
	})
}

func removeTagIDsFromImageTx(tx *sql.Tx, imageID int64, tagIDs []int64) (int, error) {
	removed := 0
	for _, tagID := range tagIDs {
		n, _, err := demoteOrRemoveTagTx(tx, imageID, tagID)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (s *Service) RemoveTagsFromOneImage(imageID int64, tagIDs []int64) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		_, implied, _, err := removeTagsUnlessImpliedTx(tx, imageID, tagIDs)
		if err == nil && len(implied) > 0 {
			return ErrTagImplied
		}
		return err
	})
}

// bumpTagUsageTx skips a missing image: usage_count counts visible images only.
func bumpTagUsageTx(tx *sql.Tx, tagID, imageID int64) error {
	_, err := tx.Exec(
		`UPDATE tags SET usage_count = usage_count + 1,
		        last_used_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	)
	return err
}

func dropTagUsageTx(tx *sql.Tx, tagID, imageID int64) error {
	_, err := tx.Exec(
		`UPDATE tags SET usage_count = MAX(0, usage_count - 1)
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	)
	return err
}

// ImagesVisibilityChangedTx is for images that just entered or left the
// visible set - a flipped missing flag, or a visible row about to be
// deleted: unlike dropTagUsageTx it does not read the flag.
func ImagesVisibilityChangedTx(tx *sql.Tx, imageIDs []int64, visible bool) error {
	delta := -1
	if visible {
		delta = 1
	}
	return db.Chunked(imageIDs, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		_, err := tx.Exec(`UPDATE tags SET usage_count = MAX(0, usage_count + ? * d.n)
			FROM (SELECT tag_id, COUNT(*) AS n FROM image_tags WHERE image_id IN (`+in+`) GROUP BY tag_id) d
			WHERE tags.id = d.tag_id`, append([]any{delta}, args...)...)
		return err
	})
}

// parentOnImage holds for an image_tags row a tag on its image implies.
const parentOnImage = `EXISTS (SELECT 1 FROM tag_implications ti
	JOIN image_tags p ON p.image_id = image_tags.image_id AND p.tag_id = ti.parent_tag_id
	WHERE ti.implied_tag_id = image_tags.tag_id)`

// demoteOrRemoveTagTx turns an owned row a parent on the image still
// implies into an implied one, keeping its usage and dropping its ledger,
// and removes any other.
func demoteOrRemoveTagTx(tx *sql.Tx, imageID, tagID int64) (removed int, demoted bool, err error) {
	res, err := tx.Exec(
		`UPDATE image_tags SET is_implied = 1, confidence = NULL, tagger_name = NULL, stale = 0
		 WHERE image_id = ? AND tag_id = ? AND is_implied = 0 AND `+parentOnImage, imageID, tagID)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err := tx.Exec(`DELETE FROM image_tag_sources WHERE image_id = ? AND tag_id = ?`, imageID, tagID)
		return 0, true, err
	}
	removed, err = removeTagFromImageTx(tx, imageID, tagID)
	return removed, false, err
}

func removeTagFromImageTx(tx *sql.Tx, imageID, tagID int64) (int, error) {
	implied, err := transitiveImpliedTx(tx, []int64{tagID})
	if err != nil {
		return 0, err
	}

	res, err := tx.Exec(
		`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, nil
	}

	if err := dropTagUsageTx(tx, tagID, imageID); err != nil {
		return 0, err
	}
	removed := 1

	n, err := sweepImpliedClosureTx(tx, imageID, implied, tagID)
	return removed + n, err
}

// sweepImpliedClosureTx repeats until a pass drops nothing: a closure row
// can be the only justification for another, in any order within a tier.
// excludeParent never counts as a justifying parent; 0 excludes none.
func sweepImpliedClosureTx(tx *sql.Tx, imageID int64, closure []int64, excludeParent int64) (int, error) {
	removed := 0
	for {
		dropped := false
		for _, impID := range closure {
			var rowImplied int
			err := tx.QueryRow(
				`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
			).Scan(&rowImplied)
			if err == sql.ErrNoRows {
				continue
			} else if err != nil {
				return removed, err
			}
			if rowImplied != 1 {
				continue
			}
			stillImplied, err := implicationParentsOnImageExcluding(tx, imageID, impID, excludeParent)
			if err != nil {
				return removed, err
			}
			if len(stillImplied) > 0 {
				continue
			}
			if _, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
			); err != nil {
				return removed, err
			}
			if err := dropTagUsageTx(tx, impID, imageID); err != nil {
				return removed, err
			}
			removed++
			dropped = true
		}
		if !dropped {
			return removed, nil
		}
	}
}

// andWhere is raw SQL starting with AND.
func (s *Service) removeMatchingTx(imageID int64, andWhere string, args ...any) (int, error) {
	return writeTx(s, func(tx *sql.Tx) (int, error) {
		tagIDs, err := db.QueryIDs(tx,
			`SELECT tag_id FROM image_tags WHERE image_id = ? `+andWhere,
			append([]any{imageID}, args...)...)
		if err != nil {
			return 0, err
		}
		return removeTagIDsFromImageTx(tx, imageID, tagIDs)
	})
}

func (s *Service) RemoveUserTagsFromImage(imageID int64) (int, error) {
	return s.removeMatchingTx(imageID, userRowsPred)
}

func (s *Service) RemoveStaleTagsFromImage(imageID int64) (int, error) {
	return s.removeMatchingTx(imageID, staleRowsPred)
}

func (s *Service) RemoveSourceTagsFromImage(imageID int64, sources []string, stale string) (int, error) {
	if len(sources) == 0 {
		return 0, nil
	}
	placeholders, args := db.InPlaceholders(sources)
	where := sourceRowsPred + ` AND tagger_name IN (` + placeholders + `)`
	switch stale {
	case "1":
		where += staleRowsPred
	case "0":
		where += ` AND stale = 0`
	}
	return s.removeMatchingTx(imageID, where, args...)
}

// Implied rows go with their parent, which may sit in another category.
func (s *Service) RemoveCategoryTagsFromImage(imageID int64, category string) (int, error) {
	return s.removeMatchingTx(imageID,
		`AND is_implied = 0 AND tag_id IN (
		   SELECT t.id FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		   WHERE tc.name = ?)`, category)
}

// DropSourceFromImageTags withdraws one source's claim: a tag another
// source also vouches for stays on the image.
func (s *Service) DropSourceFromImageTags(imageID int64, source string, tagIDs []int64) (covered, removed int, err error) {
	err = s.inWriteTx(func(tx *sql.Tx) error {
		// Rows with no ledger entry count under their own tagger_name, as
		// the by-source view lists them.
		claim := `SELECT tag_id FROM (
		            SELECT tag_id FROM image_tag_sources
		             WHERE image_id = ? AND source = ?
		            UNION
		            SELECT it.tag_id FROM image_tags it
		             WHERE it.image_id = ? AND it.is_implied = 0
		               AND COALESCE(NULLIF(it.tagger_name, ''), 'user') = ?
		               AND NOT EXISTS (SELECT 1 FROM image_tag_sources s
		                                WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id))`
		args := []any{imageID, source, imageID, source}
		if len(tagIDs) > 0 {
			placeholders, ids := db.InPlaceholders(tagIDs)
			claim += ` WHERE tag_id IN (` + placeholders + `)`
			args = append(args, ids...)
		}
		claimed, err := db.QueryIDs(tx, claim, args...)
		if err != nil {
			return err
		}
		covered = len(claimed)
		if covered == 0 {
			return nil
		}
		placeholders, ids := db.InPlaceholders(claimed)
		if _, err := tx.Exec(
			`DELETE FROM image_tag_sources
			  WHERE image_id = ? AND source = ? AND tag_id IN (`+placeholders+`)`,
			append([]any{imageID, source}, ids...)...); err != nil {
			return err
		}
		orphaned, err := db.QueryIDs(tx,
			`SELECT it.tag_id FROM image_tags it
			  WHERE it.image_id = ? AND it.is_implied = 0 AND it.tag_id IN (`+placeholders+`)
			    AND NOT EXISTS (SELECT 1 FROM image_tag_sources s
			                     WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id)`,
			append([]any{imageID}, ids...)...)
		if err != nil {
			return err
		}
		removed, err = removeTagIDsFromImageTx(tx, imageID, orphaned)
		return err
	})
	return covered, removed, err
}

func (s *Service) RemoveAutoTagsFromImage(imageID int64, taggerNames []string) (int, error) {
	if len(taggerNames) == 0 {
		return s.removeMatchingTx(imageID, autoRowsPred)
	}
	placeholders, args := db.InPlaceholders(taggerNames)
	return s.removeMatchingTx(imageID, autoRowsPred+` AND tagger_name IN (`+placeholders+`)`, args...)
}

// Loops because a dropped row can be the only justification for another.
func pruneOrphanedImpliedTx(tx *sql.Tx, imageIDs []int64) (int, error) {
	placeholders, args := db.InPlaceholders(imageIDs)
	type orphan struct{ imageID, tagID int64 }
	removed := 0
	for {
		orphans, err := db.QueryAll(tx, db.ScanRow(func(o *orphan) []any {
			return []any{&o.imageID, &o.tagID}
		}), `SELECT it.image_id, it.tag_id FROM image_tags it
			 WHERE it.is_implied = 1 AND it.image_id IN (`+placeholders+`)
			   AND NOT EXISTS (SELECT 1 FROM tag_implications ti
			                   JOIN image_tags p ON p.image_id = it.image_id AND p.tag_id = ti.parent_tag_id
			                   WHERE ti.implied_tag_id = it.tag_id)`, args...)
		if err != nil {
			return removed, err
		}
		if len(orphans) == 0 {
			return removed, nil
		}
		for _, o := range orphans {
			if _, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, o.imageID, o.tagID,
			); err != nil {
				return removed, err
			}
			if err := dropTagUsageTx(tx, o.tagID, o.imageID); err != nil {
				return removed, err
			}
			removed++
		}
	}
}

func (s *Service) RemoveAllTagsFromImage(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeAllTagsFromImageTx(tx, imageID) })
}

func removeAllTagsFromImageTx(tx *sql.Tx, imageID int64) error {
	var isMissing int
	if err := tx.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, imageID).Scan(&isMissing); err != nil && err != sql.ErrNoRows {
		return err
	}
	if isMissing == 0 {
		if err := ImagesVisibilityChangedTx(tx, []int64{imageID}, false); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DELETE FROM image_tags WHERE image_id = ?`, imageID)
	return err
}

// The related-images probe keeps only this many of the rarest general
// tags; other categories are distinctive enough to scan uncapped.
const relatedGeneralTagsCap = 15

// A manual add replaces the image's rating even with a lower one; an auto
// add keeps the highest, as search resolves several.
func pruneRatingsAfterAddTx(tx *sql.Tx, ratingCatID, imageID, tagID int64, isAuto bool) ([]string, error) {
	if ratingCatID == 0 {
		return nil, nil
	}
	var catID int64
	if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, tagID).Scan(&catID); err != nil {
		return nil, err
	}
	if catID != ratingCatID {
		return nil, nil
	}
	if isAuto {
		return nil, pruneLowerRatingsTx(tx, ratingCatID, imageID)
	}
	return pruneOtherRatingsTx(tx, ratingCatID, imageID, tagID)
}

type ratingRow struct {
	tagID int64
	name  string
}

func ratingRowsOnImageTx(tx *sql.Tx, ratingCatID, imageID int64) ([]ratingRow, error) {
	return db.QueryAll(tx, db.ScanRow(func(r *ratingRow) []any {
		return []any{&r.tagID, &r.name}
	}), `SELECT it.tag_id, t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ? AND t.is_alias = 0`,
		imageID, ratingCatID)
}

// pruneLowerRatingsTx keeps the highest rating; search's rating: fast
// count assumes one per image.
func pruneLowerRatingsTx(tx *sql.Tx, ratingCatID, imageID int64) error {
	present, err := ratingRowsOnImageTx(tx, ratingCatID, imageID)
	if err != nil {
		return fmt.Errorf("scan rating rows for prune: %w", err)
	}
	if len(present) <= 1 {
		return nil
	}
	bestRank := -1
	for _, r := range present {
		bestRank = max(bestRank, contract.RatingRank(r.name))
	}
	_, err = pruneRatingsTx(tx, imageID, present, func(r ratingRow) bool {
		return contract.RatingRank(r.name) >= bestRank
	})
	if err != nil {
		return fmt.Errorf("prune lower rating: %w", err)
	}
	return nil
}

func pruneRatingsTx(tx *sql.Tx, imageID int64, present []ratingRow, keep func(ratingRow) bool) ([]string, error) {
	var displaced []string
	for _, r := range present {
		if keep(r) {
			continue
		}
		if _, err := removeTagFromImageTx(tx, imageID, r.tagID); err != nil {
			return displaced, fmt.Errorf("remove rating %d: %w", r.tagID, err)
		}
		displaced = append(displaced, r.name)
	}
	return displaced, nil
}

func pruneOtherRatingsTx(tx *sql.Tx, ratingCatID, imageID, keepTagID int64) ([]string, error) {
	present, err := ratingRowsOnImageTx(tx, ratingCatID, imageID)
	if err != nil {
		return nil, fmt.Errorf("scan rating rows for overwrite: %w", err)
	}
	displaced, err := pruneRatingsTx(tx, imageID, present, func(r ratingRow) bool {
		return r.tagID == keepTagID
	})
	if err != nil {
		return nil, fmt.Errorf("overwrite prior rating: %w", err)
	}
	return displaced, nil
}

func (s *Service) RatingTagIDsAbove(ceiling string) []int64 {
	if s.ratingCatID == 0 {
		return nil
	}
	rank := contract.RatingRank(ceiling)
	if rank < 0 || rank >= len(contract.RatingLevels)-1 {
		return nil
	}
	above := contract.RatingLevels[rank+1:]
	placeholders, nameArgs := db.InPlaceholders(above)
	args := append([]any{s.ratingCatID}, nameArgs...)
	ids, err := db.QueryIDs(s.db.Read,
		`SELECT id FROM tags WHERE category_id = ? AND name IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil
	}
	return ids
}
