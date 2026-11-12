package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

var (
	ErrAliasNameInUse = errors.New("a tag with this name already has image_tags rows; merge it instead")
	ErrAliasToAlias   = errors.New("cannot alias to a tag that is itself an alias")
	ErrAliasToItself  = errors.New("cannot alias a tag to itself")
	ErrMergeIntoSelf  = errors.New("cannot merge a tag into itself")
	ErrMergeIntoAlias = errors.New("cannot merge into a tag that is itself an alias")
)

type ErrAliasExists struct {
	CanonicalID int64
	Canonical   string
}

func (e *ErrAliasExists) Error() string {
	return "this name is already an alias of " + e.Canonical + "; use Repoint to… on the alias's page to move it"
}

func (s *Service) CreateAlias(name string, categoryID, canonicalID int64) (*models.Tag, error) {
	return s.CreateAliasFrom(name, categoryID, canonicalID, "user")
}

// CreateAliasFrom stamps origin only on a fresh insert; a repointed or
// upgraded row keeps its creator.
func (s *Service) CreateAliasFrom(name string, categoryID, canonicalID int64, origin string) (*models.Tag, error) {
	return s.createAlias(name, categoryID, origin, func(*sql.Tx) (int64, error) { return canonicalID, nil })
}

// CreateAliasToNewTag creates the canonical in the alias's transaction, so
// a refused alias leaves no new tag behind.
func (s *Service) CreateAliasToNewTag(name string, categoryID int64, canonName string, canonCategoryID int64) (*models.Tag, error) {
	canonName, err := s.ValidateTagIn(canonName, canonCategoryID)
	if err != nil {
		return nil, err
	}
	return s.createAlias(name, categoryID, "user", func(tx *sql.Tx) (int64, error) {
		canon, err := getOrCreateTagTx(tx, canonName, canonCategoryID, "user")
		if err != nil {
			return 0, err
		}
		return canon.ID, nil
	})
}

func (s *Service) createAlias(name string, categoryID int64, origin string, canonical func(*sql.Tx) (int64, error)) (*models.Tag, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return nil, err
	}
	if s.ratingCatID != 0 && categoryID == s.ratingCatID {
		return nil, ErrRatingCategoryClosed
	}

	var resultID, canonicalID int64
	var gained bool
	err = s.inWriteTx(func(tx *sql.Tx) error {
		id, err := canonical(tx)
		if err != nil {
			return err
		}
		canonicalID = id
		var canonIsAlias, canonRejected int
		if err := tx.QueryRow(`SELECT is_alias, rejected FROM tags WHERE id = ?`, canonicalID).Scan(&canonIsAlias, &canonRejected); err == sql.ErrNoRows {
			return ErrTagNotFound
		} else if err != nil {
			return err
		}
		if canonIsAlias == 1 {
			return ErrAliasToAlias
		}
		if canonRejected == 1 {
			return ErrAliasToRejected
		}

		// Test for image_tags rows, not usage_count: a tag whose images
		// all went missing reads 0 but still has rows, which an alias
		// would strand.
		var existingID int64
		var existingCanonID sql.NullInt64
		var existingIsAlias, existingCarried, existingRejected int
		err = tx.QueryRow(
			`SELECT id, is_alias, canonical_tag_id, EXISTS (SELECT 1 FROM image_tags WHERE tag_id = tags.id), rejected
			 FROM tags WHERE name = ? AND category_id = ?`,
			normalized, categoryID,
		).Scan(&existingID, &existingIsAlias, &existingCanonID, &existingCarried, &existingRejected)
		switch {
		case err == sql.ErrNoRows:
			var id int64
			if err := tx.QueryRow(
				`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin) VALUES (?, ?, 1, ?, 0, ?) RETURNING id`,
				normalized, categoryID, canonicalID, origin,
			).Scan(&id); err != nil {
				return fmt.Errorf("inserting alias: %w", err)
			}
			resultID = id
			return nil
		case err != nil:
			return err
		}
		if existingID == canonicalID {
			return ErrAliasToItself
		}
		if existingRejected == 1 {
			return ErrTagRejected
		}
		if existingIsAlias == 1 {
			if existingCanonID.Int64 == canonicalID {
				resultID = existingID
				return nil
			}
			var canonical string
			if err := tx.QueryRow(
				`SELECT c.name || ':' || t.name FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE t.id = ?`,
				existingCanonID.Int64,
			).Scan(&canonical); err != nil {
				return err
			}
			return &ErrAliasExists{CanonicalID: existingCanonID.Int64, Canonical: canonical}
		} else if existingCarried == 1 {
			return ErrAliasNameInUse
		} else {
			if _, err := tx.Exec(
				`UPDATE tags SET is_alias = 1, canonical_tag_id = ?, usage_count = 0 WHERE id = ?`,
				canonicalID, existingID,
			); err != nil {
				return err
			}
			if err := dropDistinctPairsTx(tx, existingID); err != nil {
				return err
			}
		}
		if gained, err = repointImplicationsToCanonicalTx(tx, existingID, canonicalID); err != nil {
			return err
		}
		if err := moveTagNoteTx(tx, existingID, canonicalID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE tags SET canonical_tag_id = ? WHERE canonical_tag_id = ?`,
			canonicalID, existingID,
		); err != nil {
			return err
		}
		resultID = existingID
		return nil
	})
	if err != nil {
		return nil, err
	}
	if gained {
		if _, err := s.PropagateImplication(context.Background(), canonicalID, nil); err != nil {
			return nil, err
		}
	}
	return s.GetTag(resultID)
}

func (s *Service) MergeTags(aliasID, canonicalID int64) error {
	if aliasID == canonicalID {
		return ErrMergeIntoSelf
	}
	switch locked, err := s.isLockedRatingTag(aliasID); {
	case err != nil:
		return err
	case locked:
		return ErrRatingTagImmutable
	}
	ratingTarget, err := s.isRatingTag(canonicalID)
	if err != nil {
		return err
	}

	var gained bool
	err = s.inWriteTx(func(tx *sql.Tx) error {
		var aliasExists bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM tags WHERE id = ?)`, aliasID).Scan(&aliasExists); err != nil {
			return err
		} else if !aliasExists {
			return ErrTagNotFound
		}
		var targetIsAlias, targetRejected int
		if err := tx.QueryRow(`SELECT is_alias, rejected FROM tags WHERE id = ?`, canonicalID).Scan(&targetIsAlias, &targetRejected); err == sql.ErrNoRows {
			return ErrTagNotFound
		} else if err != nil {
			return err
		}
		if targetIsAlias == 1 {
			return ErrMergeIntoAlias
		}
		if targetRejected == 1 {
			return ErrAliasToRejected
		}
		if rejected, err := rejectedTx(tx, aliasID); err != nil {
			return err
		} else if rejected {
			return ErrTagRejected
		}

		// Images that gain the canonical here need its implications
		// fanned out below.
		newCarriers, err := db.QueryAll(tx, db.ScanRow(func(c *carrier) []any { return []any{&c.imageID, &c.isAuto} }),
			`SELECT image_id, is_auto FROM image_tags WHERE tag_id = ?
			 AND image_id NOT IN (SELECT image_id FROM image_tags WHERE tag_id = ?)`,
			aliasID, canonicalID,
		)
		if err != nil {
			return fmt.Errorf("merge enumerate alias-only images: %w", err)
		}

		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at, stale)
			 SELECT image_id, ?, is_auto, is_implied, confidence, tagger_name, created_at, stale
			 FROM image_tags WHERE tag_id = ?`,
			canonicalID, aliasID,
		); err != nil {
			return fmt.Errorf("merge insert canonical rows: %w", err)
		}
		// Where the alias row was owned and the canonical only implied,
		// the canonical takes that ownership, or removing its parent
		// would sweep it.
		if _, err := tx.Exec(
			`UPDATE image_tags AS c
			 SET is_implied = 0, is_auto = a.is_auto, confidence = a.confidence, tagger_name = a.tagger_name, stale = a.stale
			 FROM image_tags AS a
			 WHERE c.image_id = a.image_id
			   AND c.tag_id = ?
			   AND a.tag_id = ?
			   AND c.is_implied = 1
			   AND a.is_implied = 0`,
			canonicalID, aliasID,
		); err != nil {
			return fmt.Errorf("merge promote canonical rows: %w", err)
		}
		// Before the delete below: its trigger drops the alias rows' ledger.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at)
			 SELECT image_id, ?, source, created_at FROM image_tag_sources WHERE tag_id = ?`,
			canonicalID, aliasID,
		); err != nil {
			return fmt.Errorf("merge move tag sources: %w", err)
		}
		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE tag_id = ?`, aliasID,
		); err != nil {
			return fmt.Errorf("merge drop alias rows: %w", err)
		}

		if _, err := tx.Exec(
			`UPDATE tags SET is_alias = 1, canonical_tag_id = ?, usage_count = 0 WHERE id = ?`,
			canonicalID, aliasID,
		); err != nil {
			return err
		}

		if _, err := tx.Exec(
			`UPDATE tags SET canonical_tag_id = ? WHERE canonical_tag_id = ?`,
			canonicalID, aliasID,
		); err != nil {
			return err
		}

		if gained, err = repointImplicationsToCanonicalTx(tx, aliasID, canonicalID); err != nil {
			return err
		}
		if err := moveTagNoteTx(tx, aliasID, canonicalID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM folded_tag_pairs WHERE old_id = ? OR new_id = ?`, aliasID, aliasID); err != nil {
			return err
		}
		if err := dropDistinctPairsTx(tx, aliasID); err != nil {
			return err
		}

		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = (
				SELECT COUNT(*) FROM image_tags it
				JOIN images i ON i.id = it.image_id
				WHERE it.tag_id = ? AND i.is_missing = 0
			) WHERE id = ?`,
			canonicalID, canonicalID,
		); err != nil {
			return err
		}

		if len(newCarriers) > 0 {
			impliedClosure, err := transitiveImpliedTx(tx, []int64{canonicalID})
			if err != nil {
				return fmt.Errorf("merge resolve canonical closure: %w", err)
			}
			for _, c := range newCarriers {
				if err := applyImpliedClosureTx(tx, c.imageID, impliedClosure, s.ratingCatID, c.isAuto); err != nil {
					return fmt.Errorf("merge fan out implications onto image %d: %w", c.imageID, err)
				}
				// Highest wins here: a merge must never lower a rating
				// and expose an image the ceiling hides.
				if ratingTarget {
					if err := pruneLowerRatingsTx(tx, s.ratingCatID, c.imageID); err != nil {
						return fmt.Errorf("merge prune ratings on image %d: %w", c.imageID, err)
					}
				}
			}
		}

		return nil
	})
	if err != nil || !gained {
		return err
	}
	_, err = s.PropagateImplication(context.Background(), canonicalID, nil)
	return err
}

// An alias left in tag_implications would never fire, stranding the rows
// its edges implied. The != filters skip would-be self-edges.
func repointImplicationsToCanonicalTx(tx *sql.Tx, aliasID, canonicalID int64) (gained bool, err error) {
	if closes, err := repointClosesCycleTx(tx, aliasID, canonicalID); err != nil {
		return false, err
	} else if closes {
		return false, ErrImplicationCycle
	}
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id, origin, created_at, stale)
		 SELECT ?, implied_tag_id, origin, created_at, stale FROM tag_implications
		 WHERE parent_tag_id = ? AND implied_tag_id != ?`,
		canonicalID, aliasID, canonicalID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id, origin, created_at, stale)
		 SELECT parent_tag_id, ?, origin, created_at, stale FROM tag_implications
		 WHERE implied_tag_id = ? AND parent_tag_id != ?`,
		canonicalID, aliasID, canonicalID,
	); err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`DELETE FROM tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?`,
		aliasID, aliasID,
	); err != nil {
		return false, err
	}
	return n > 0, nil
}

type carrier struct {
	imageID int64
	isAuto  int
}

// The graph was acyclic, so a new cycle runs through one alias edge.
func repointClosesCycleTx(tx *sql.Tx, aliasID, canonicalID int64) (bool, error) {
	implied, err := db.QueryIDs(tx,
		`SELECT implied_tag_id FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id != ?`,
		aliasID, canonicalID)
	if err != nil {
		return false, err
	}
	for _, id := range implied {
		if reaches, err := implicationReachesTx(tx, id, canonicalID); err != nil || reaches {
			return reaches, err
		}
	}
	parents, err := db.QueryIDs(tx,
		`SELECT parent_tag_id FROM tag_implications WHERE implied_tag_id = ? AND parent_tag_id != ?`,
		aliasID, canonicalID)
	if err != nil {
		return false, err
	}
	for _, id := range parents {
		if reaches, err := implicationReachesTx(tx, canonicalID, id); err != nil || reaches {
			return reaches, err
		}
	}
	return false, nil
}
