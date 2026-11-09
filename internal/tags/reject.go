package tags

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

var (
	ErrTagRejected     = errors.New("rejected in this gallery")
	ErrRejectAlias     = errors.New("an alias follows its target; reject the target instead")
	ErrRejectRating    = errors.New("ratings cannot be rejected")
	ErrAliasToRejected = errors.New("the target is rejected; un-reject it first")
)

// RejectTag strips the tag from every image, drops its implication edges
// and flags the row so every add path refuses the name from now on. Its
// aliases stay and resolve to it, so they are refused with it.
func (s *Service) RejectTag(id int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		var isAlias, catID int64
		switch err := tx.QueryRow(`SELECT is_alias, category_id FROM tags WHERE id = ?`, id).Scan(&isAlias, &catID); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrTagNotFound
		case err != nil:
			return err
		}
		return s.rejectTx(tx, id, isAlias == 1, catID)
	})
}

// RejectTagName rejects a name whether or not the gallery has seen it.
func (s *Service) RejectTagName(name string, categoryID int64) (*models.Tag, error) {
	normalized, err := s.ValidateTagIn(name, categoryID)
	if err != nil {
		return nil, err
	}
	return writeTx(s, func(tx *sql.Tx) (*models.Tag, error) {
		tag, err := lookupTag(tx, normalized, categoryID)
		if err != nil {
			return nil, err
		}
		if tag == nil {
			if tag, err = getOrCreateTagTx(tx, normalized, categoryID, "user"); err != nil {
				return nil, err
			}
		} else if tag.Name != normalized || tag.CategoryID != categoryID {
			return nil, ErrRejectAlias
		}
		if tag.Rejected {
			return tag, nil
		}
		if err := s.rejectTx(tx, tag.ID, false, tag.CategoryID); err != nil {
			return nil, err
		}
		tag.Rejected = true
		tag.UsageCount = 0
		return tag, nil
	})
}

func (s *Service) rejectTx(tx *sql.Tx, id int64, isAlias bool, catID int64) error {
	if isAlias {
		return ErrRejectAlias
	}
	if s.ratingCatID != 0 && catID == s.ratingCatID {
		return ErrRejectRating
	}
	if err := stripTagsTx(tx, []int64{id}); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?`, id, id); err != nil {
		return fmt.Errorf("drop implications: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tags SET rejected = 1, usage_count = 0 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("reject tag: %w", err)
	}
	return nil
}

func (s *Service) UnrejectTag(id int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE tags SET rejected = 0, rejected_drops = 0 WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrTagNotFound
		}
		return nil
	})
}

func (s *Service) RejectedCount() (int, error) { return s.countTagsWhere(`t.rejected = 1`) }

// The operator's own typing is a refusal, not an arrival, so it is not
// counted.
func noteRejectedDrop(x db.Execer, id int64, origin string) error {
	if origin == "user" {
		return nil
	}
	if _, err := x.Exec(`UPDATE tags SET rejected_drops = rejected_drops + 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("count rejected drop: %w", err)
	}
	return nil
}

func rejectedTx(tx *sql.Tx, id int64) (bool, error) {
	var rejected int
	switch err := tx.QueryRow(`SELECT rejected FROM tags WHERE id = ?`, id).Scan(&rejected); {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrTagNotFound
	case err != nil:
		return false, err
	}
	return rejected == 1, nil
}
