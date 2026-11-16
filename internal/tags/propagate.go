package tags

import (
	"context"
	"database/sql"

	"github.com/monbooru/monbooru/internal/db"
)

// Propagation reports a walk over the images carrying a parent; Walked
// holds the ones it reached before a cancel.
type Propagation struct {
	Walked    []int64
	Total     int
	Cancelled bool
}

// ImplicationClosure is tagID followed by every tag it implies. A sweep
// resolves it once, outside the writer-held chunks.
func (s *Service) ImplicationClosure(tagID int64) ([]int64, error) {
	tx, err := s.db.Read.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	closure, err := transitiveImpliedTx(tx, []int64{tagID})
	if err != nil {
		return nil, err
	}
	return append([]int64{tagID}, closure...), nil
}

// PropagateImplication fans parentID's implications out onto every image
// carrying it. Each row a walk adds or sweeps moves its tag's usage_count
// as it goes, so no recount follows.
func (s *Service) PropagateImplication(ctx context.Context, parentID int64, progress func(done, total int)) (Propagation, error) {
	closure, err := s.ImplicationClosure(parentID)
	if err != nil {
		return Propagation{}, err
	}
	return s.walkParent(ctx, parentID, progress, func(tx *sql.Tx, imageID int64) error {
		var isAuto int
		switch err := tx.QueryRow(
			`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, parentID,
		).Scan(&isAuto); {
		case err == sql.ErrNoRows:
			return nil
		case err != nil:
			return err
		}
		return applyImpliedClosureTx(tx, imageID, closure[1:], s.ratingCatID, isAuto)
	})
}

// ImplicationUnapplied reports whether some image carries parentID without
// impliedID.
func (s *Service) ImplicationUnapplied(parentID, impliedID int64) (bool, error) {
	var unapplied bool
	err := s.db.Read.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM image_tags p WHERE p.tag_id = ?
		   AND NOT EXISTS (SELECT 1 FROM image_tags c WHERE c.image_id = p.image_id AND c.tag_id = ?))`,
		parentID, impliedID,
	).Scan(&unapplied)
	return unapplied, err
}

// SweepImplicationRemoval drops, on every image carrying parentID, the rows
// of closure (the ImplicationClosure of the tag the removed edge implied)
// that nothing left on the image justifies.
func (s *Service) SweepImplicationRemoval(ctx context.Context, parentID int64, closure []int64, progress func(done, total int)) (Propagation, error) {
	return s.walkParent(ctx, parentID, progress, func(tx *sql.Tx, imageID int64) error {
		_, err := sweepImpliedClosureTx(tx, imageID, closure, 0)
		return err
	})
}

func (s *Service) walkParent(ctx context.Context, parentID int64, progress func(done, total int), perImage func(*sql.Tx, int64) error) (Propagation, error) {
	ids, err := db.QueryIDsContext(ctx, s.db.Read,
		`SELECT image_id FROM image_tags WHERE tag_id = ? ORDER BY image_id`, parentID)
	if err != nil {
		return Propagation{}, err
	}
	if progress != nil {
		progress(0, len(ids))
	}
	res, err := s.batchChunks(ctx, ids, 500, progress, func(tx *sql.Tx, chunk []int64) (BatchResult, error) {
		for _, imageID := range chunk {
			if err := perImage(tx, imageID); err != nil {
				return BatchResult{}, err
			}
		}
		return BatchResult{}, nil
	})
	return Propagation{Walked: ids[:res.Processed], Total: len(ids), Cancelled: res.Cancelled}, err
}
