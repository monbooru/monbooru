package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// MaxImplicationDepth sits well past real booru graphs, which stay under
// ten levels.
const MaxImplicationDepth = 16

var (
	ErrImplicationCycle    = errors.New("implication would form a cycle")
	ErrImplicationSelf     = errors.New("cannot imply a tag from itself")
	ErrImplicationAlias    = errors.New("cannot involve an alias in an implication; use its canonical")
	ErrImplicationNotFound = errors.New("implication not found")
)

func scanImplication(withImpliedCols bool) func(*sql.Rows) (models.Implication, error) {
	return func(rows *sql.Rows) (models.Implication, error) {
		var im models.Implication
		var created string
		var stale int64
		cols := []any{
			&im.ParentID, &im.ImpliedID,
			&im.ParentName, &im.ParentCategoryName, &im.ParentCategoryColor,
		}
		if withImpliedCols {
			cols = append(cols, &im.ImpliedName, &im.ImpliedCategoryName, &im.ImpliedCategoryColor)
		}
		cols = append(cols, &created, &im.Origin, &stale)
		if err := rows.Scan(cols...); err != nil {
			return im, err
		}
		im.CreatedAt, _ = time.Parse(time.RFC3339, created)
		im.Stale = stale == 1
		return im, nil
	}
}

func (s *Service) ListImplications(parentID int64) ([]models.Implication, error) {
	return db.QueryAll(s.db.Read, scanImplication(true),
		`SELECT ti.parent_tag_id, ti.implied_tag_id,
		        p.name, pc.name, pc.color,
		        i.name, ic.name, ic.color,
		        ti.created_at, ti.origin, ti.stale
		 FROM tag_implications ti
		 JOIN tags p ON p.id = ti.parent_tag_id
		 JOIN tag_categories pc ON pc.id = p.category_id
		 JOIN tags i ON i.id = ti.implied_tag_id
		 JOIN tag_categories ic ON ic.id = i.category_id
		 WHERE ti.parent_tag_id = ?
		 ORDER BY i.name`, parentID)
}

// ImplicationsDeclaredIn lists the edges whose parent sits in the
// category, wherever the implied tag lives.
func (s *Service) ImplicationsDeclaredIn(categoryID int64) ([]models.Implication, error) {
	return db.QueryAll(s.db.Read, db.ScanRow(func(im *models.Implication) []any {
		return []any{&im.ParentID, &im.ImpliedID}
	}), `SELECT ti.parent_tag_id, ti.implied_tag_id
	       FROM tag_implications ti
	       JOIN tags p ON p.id = ti.parent_tag_id
	      WHERE p.category_id = ?
	      ORDER BY ti.parent_tag_id, ti.implied_tag_id`, categoryID)
}

func (s *Service) ImpliedBy(tagID int64) ([]models.Implication, error) {
	return db.QueryAll(s.db.Read, scanImplication(false),
		`SELECT ti.parent_tag_id, ti.implied_tag_id,
		        p.name, pc.name, pc.color,
		        ti.created_at, ti.origin, ti.stale
		 FROM tag_implications ti
		 JOIN tags p ON p.id = ti.parent_tag_id
		 JOIN tag_categories pc ON pc.id = p.category_id
		 WHERE ti.implied_tag_id = ?
		 ORDER BY p.name`, tagID)
}

func (s *Service) SyncImplicationStaleness(parentID int64, origin string, fresh map[int64]bool) (int, error) {
	return writeTx(s, func(tx *sql.Tx) (int, error) {
		type edgeRow struct{ impliedID, stale int64 }
		present, err := db.QueryAll(tx, db.ScanRow(func(e *edgeRow) []any {
			return []any{&e.impliedID, &e.stale}
		}), `SELECT implied_tag_id, stale FROM tag_implications
			 WHERE parent_tag_id = ? AND origin = ?`, parentID, origin)
		if err != nil {
			return 0, err
		}
		var flag, clear []int64
		for _, e := range present {
			switch current := fresh[e.impliedID]; {
			case !current && e.stale == 0:
				flag = append(flag, e.impliedID)
			case current && e.stale == 1:
				clear = append(clear, e.impliedID)
			}
		}
		if err := setImplicationsStaleTx(tx, parentID, flag, 1); err != nil {
			return 0, err
		}
		if err := setImplicationsStaleTx(tx, parentID, clear, 0); err != nil {
			return 0, err
		}
		return len(flag), nil
	})
}

func setImplicationsStaleTx(tx *sql.Tx, parentID int64, impliedIDs []int64, stale int) error {
	return setStaleTx(tx, "tag_implications", "implied_tag_id",
		`parent_tag_id = `+strconv.FormatInt(parentID, 10)+` AND `, impliedIDs, stale)
}

func (s *Service) ImplicationsForParents(parentIDs []int64) (map[int64][]models.Implication, error) {
	out := make(map[int64][]models.Implication, len(parentIDs))
	if len(parentIDs) == 0 {
		return out, nil
	}
	err := db.Chunked(parentIDs, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		imps, err := db.QueryAll(s.db.Read, db.ScanRow(func(im *models.Implication) []any {
			return []any{
				&im.ParentID, &im.ImpliedID,
				&im.ImpliedName, &im.ImpliedCategoryName, &im.ImpliedCategoryColor,
			}
		}),
			`SELECT ti.parent_tag_id, ti.implied_tag_id,
			        i.name, ic.name, ic.color
			 FROM tag_implications ti
			 JOIN tags i ON i.id = ti.implied_tag_id
			 JOIN tag_categories ic ON ic.id = i.category_id
			 WHERE ti.parent_tag_id IN (`+placeholders+`)
			 ORDER BY i.name`,
			args...)
		if err != nil {
			return err
		}
		for _, im := range imps {
			out[im.ParentID] = append(out[im.ParentID], im)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddImplication reports false for an edge that already existed. Aliases
// are refused on either side: an edge keyed on an alias would never fire.
func (s *Service) AddImplication(parentID, impliedID int64) (bool, error) {
	return s.AddImplicationFrom(parentID, impliedID, "user")
}

// AddImplicationFrom stamps origin only on a new edge.
func (s *Service) AddImplicationFrom(parentID, impliedID int64, origin string) (bool, error) {
	if parentID == impliedID {
		return false, ErrImplicationSelf
	}

	return writeTx(s, func(tx *sql.Tx) (bool, error) {
		for _, id := range [2]int64{parentID, impliedID} {
			var isAlias int
			if err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, id).Scan(&isAlias); err == sql.ErrNoRows {
				return false, ErrTagNotFound
			} else if err != nil {
				return false, err
			}
			if isAlias == 1 {
				return false, ErrImplicationAlias
			}
		}

		if reaches, err := implicationReachesTx(tx, impliedID, parentID); err != nil {
			return false, err
		} else if reaches {
			return false, ErrImplicationCycle
		}

		res, err := tx.Exec(
			`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id, origin) VALUES (?, ?, ?)`,
			parentID, impliedID, origin,
		)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	})
}

// RemoveImplication leaves the rows the edge implied in place; sweeping
// them is the caller's job, kept off this path so the click stays fast.
func (s *Service) RemoveImplication(parentID, impliedID int64) error {
	res, err := s.db.Write.Exec(
		`DELETE FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`,
		parentID, impliedID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrImplicationNotFound
	}
	s.changed()
	return nil
}

// Start ids are never visited, even when a cycle leads back to them.
func bfsImpliedTx(tx *sql.Tx, start []int64, visit func(int64) bool) error {
	seen := make(map[int64]struct{}, len(start))
	for _, p := range start {
		seen[p] = struct{}{}
	}
	frontier := append([]int64(nil), start...)
	for depth := 0; depth < MaxImplicationDepth && len(frontier) > 0; depth++ {
		placeholders, args := db.InPlaceholders(frontier)
		// The whole level is read first: visit must not run with a cursor open.
		ids, err := db.QueryIDs(tx,
			`SELECT DISTINCT implied_tag_id FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return err
		}
		var next []int64
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if !visit(id) {
				return nil
			}
			next = append(next, id)
		}
		frontier = next
	}
	return nil
}

func implicationReachesTx(tx *sql.Tx, start, target int64) (bool, error) {
	reached := false
	err := bfsImpliedTx(tx, []int64{start}, func(id int64) bool {
		if id == target {
			reached = true
			return false
		}
		return true
	})
	return reached, err
}

// Implied rows take the parent's is_auto so the detail page groups them
// by source.
func fanOutImpliedTxImpl(tx *sql.Tx, imageID, parentID, ratingCatID int64, isAutoInt int) error {
	implied, err := transitiveImpliedTx(tx, []int64{parentID})
	if err != nil {
		return err
	}
	return applyImpliedClosureTx(tx, imageID, implied, ratingCatID, isAutoInt)
}

func applyImpliedClosureTx(tx *sql.Tx, imageID int64, implied []int64, ratingCatID int64, isAutoInt int) error {
	insertedRating := false
	for _, id := range implied {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name)
			 VALUES (?, ?, ?, 1, NULL, NULL)`,
			imageID, id, isAutoInt,
		)
		if err != nil {
			return fmt.Errorf("inserting implied tag %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := bumpTagUsageTx(tx, id, imageID); err != nil {
			return err
		}
		if ratingCatID != 0 && !insertedRating {
			var catID int64
			if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID); err != nil {
				return err
			}
			insertedRating = catID == ratingCatID
		}
	}
	if insertedRating && ratingCatID != 0 {
		if err := pruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
			return fmt.Errorf("prune lower ratings after implied fan-out: %w", err)
		}
	}
	return nil
}

// Removing an implied row its parent still justifies breaks the operator's
// own declaration: the image drops out of searches for a tag its tags imply.
func impliedByParentOnImage(tx *sql.Tx, imageID, tagID int64) (bool, error) {
	var isImplied int
	switch err := tx.QueryRow(
		`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	).Scan(&isImplied); {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	if isImplied != 1 {
		return false, nil
	}
	rows, err := db.QueryAll(tx, func(rows *sql.Rows) (models.ImageTag, error) {
		t := models.ImageTag{ImageID: imageID}
		err := rows.Scan(&t.TagID, &t.IsImplied)
		return t, err
	}, `SELECT tag_id, is_implied FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return false, err
	}
	type edge struct{ parent, child int64 }
	edges, err := db.QueryAll(tx, db.ScanRow(func(e *edge) []any {
		return []any{&e.parent, &e.child}
	}), `SELECT ti.parent_tag_id, ti.implied_tag_id FROM tag_implications ti
		 JOIN image_tags p ON p.image_id = ?1 AND p.tag_id = ti.parent_tag_id
		 JOIN image_tags c ON c.image_id = ?1 AND c.tag_id = ti.implied_tag_id AND c.is_implied = 1`, imageID)
	if err != nil {
		return false, err
	}
	implied := make(map[int64][]int64, len(edges))
	for _, e := range edges {
		implied[e.parent] = append(implied[e.parent], e.child)
	}
	// An orphan stays removable: refusing it would trap a row nothing
	// justifies.
	return JustifiedImplied(rows, implied)[tagID], nil
}

// JustifiedImplied lists the implied rows reached from the image's own rows
// through implied (parent -> implied tags on the image); the rest are orphans.
func JustifiedImplied(imageTags []models.ImageTag, implied map[int64][]int64) map[int64]bool {
	justified := map[int64]bool{}
	var walk func(parent int64)
	walk = func(parent int64) {
		for _, child := range implied[parent] {
			if justified[child] {
				continue
			}
			justified[child] = true
			walk(child)
		}
	}
	for _, t := range imageTags {
		if !t.IsImplied {
			walk(t.TagID)
		}
	}
	return justified
}

func implicationParentsOnImageExcluding(tx *sql.Tx, imageID, impliedID, excludeParent int64) ([]int64, error) {
	return db.QueryIDs(tx,
		`SELECT ti.parent_tag_id
		 FROM tag_implications ti
		 JOIN image_tags it ON it.tag_id = ti.parent_tag_id
		 WHERE ti.implied_tag_id = ? AND it.image_id = ? AND ti.parent_tag_id != ?`,
		impliedID, imageID, excludeParent,
	)
}
