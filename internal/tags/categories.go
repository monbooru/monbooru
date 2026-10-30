package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// A failed read is an error, not ok=false: "unknown category" would blame
// the caller for the server's fault.
func CategoryIDByName(database *db.DB, name string) (int64, bool, error) {
	var id int64
	err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func CategoryIDsByName(q db.Querier) (map[string]int64, error) {
	type catRow struct {
		id   int64
		name string
	}
	cats, err := db.QueryAll(q, db.ScanRow(func(c *catRow) []any {
		return []any{&c.id, &c.name}
	}), `SELECT id, name FROM tag_categories`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(cats))
	for _, c := range cats {
		out[c.name] = c.id
	}
	return out, nil
}

func (s *Service) ListCategories() ([]models.TagCategory, error) {
	return db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.TagCategory, error) {
		var c models.TagCategory
		var isBuiltin int
		err := rows.Scan(&c.ID, &c.Name, &c.Color, &isBuiltin, &c.Position)
		c.IsBuiltin = isBuiltin == 1
		return c, err
	}, `SELECT id, name, color, is_builtin, position FROM tag_categories ORDER BY position, id`)
}

func (s *Service) DefaultCategoryOrder() ([]int64, error) {
	return db.QueryIDs(s.db.Read, `SELECT id FROM tag_categories ORDER BY `+db.DefaultCategoryOrder)
}

// ReorderCategories takes every category id exactly once, in the new order.
func (s *Service) ReorderCategories(ids []int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
			return err
		}
		if len(ids) != n {
			return ErrCategoryOrderStale
		}
		seen := make(map[int64]bool, len(ids))
		for pos, id := range ids {
			res, err := tx.Exec(`UPDATE tag_categories SET position = ? WHERE id = ?`, pos, id)
			if err != nil {
				return err
			}
			if affected, _ := res.RowsAffected(); affected == 0 || seen[id] {
				return ErrCategoryOrderStale
			}
			seen[id] = true
		}
		return nil
	})
}

func (s *Service) GetCategory(id int64) (models.TagCategory, error) {
	var c models.TagCategory
	var isBuiltin int
	err := s.db.Read.QueryRow(
		`SELECT id, name, color, is_builtin, position FROM tag_categories WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Color, &isBuiltin, &c.Position)
	if err == sql.ErrNoRows {
		return c, ErrCategoryNotFound
	}
	c.IsBuiltin = isBuiltin == 1
	return c, err
}

func (s *Service) CreateCategory(name, color string) (*models.TagCategory, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if !categoryNameRe.MatchString(name) {
		return nil, ErrInvalidCategoryName
	}
	if isReservedCategoryName(name) {
		return nil, ErrReservedCategoryName
	}
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return nil, ErrInvalidCategoryColor
	}
	color = normalizeCategoryColor(color)
	var id int64
	var position int
	err := s.db.Write.QueryRow(
		`INSERT INTO tag_categories (name, color, position)
		 VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM tag_categories))
		 RETURNING id, position`,
		name, color,
	).Scan(&id, &position)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, ErrCategoryExists
		}
		return nil, fmt.Errorf("creating category: %w", err)
	}
	s.changed()
	return &models.TagCategory{ID: id, Name: name, Color: color, Position: position}, nil
}

var builtinCategoryColors = map[string]string{
	"general":   "#3d90e3",
	"character": "#00aa00",
	"artist":    "#cc0000",
	"copyright": "#aa00aa",
	"meta":      "#ffaa00",
	"rating":    "#996666",
	"medium":    "#7d4fbf",
	"person":    "#b85c9e",
	"year":      "#4a8fa8",
	"species":   "#ed5d1f",
}

func DefaultCategoryColor(name string) string { return builtinCategoryColors[name] }

func (s *Service) UpdateCategoryColor(id int64, color string) error {
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return ErrInvalidCategoryColor
	}
	if _, err := s.db.Write.Exec(
		`UPDATE tag_categories SET color = ? WHERE id = ?`, normalizeCategoryColor(color), id,
	); err != nil {
		return err
	}
	s.changed()
	return nil
}

func (s *Service) RenameCategory(id int64, newName string) error {
	newName = strings.TrimSpace(strings.ToLower(newName))
	if !categoryNameRe.MatchString(newName) {
		return ErrInvalidCategoryName
	}
	if isReservedCategoryName(newName) {
		return ErrReservedCategoryName
	}
	var isBuiltin int
	if err := s.db.Read.QueryRow(
		`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&isBuiltin); err == sql.ErrNoRows {
		return ErrCategoryNotFound
	} else if err != nil {
		return err
	}
	if isBuiltin == 1 {
		return ErrBuiltinCategoryName
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET name = ? WHERE id = ?`, newName, id,
	)
	if err != nil && isUniqueConstraintErr(err) {
		return ErrCategoryExists
	}
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

const collideNamesShown = 5

type ErrCategoryMoveCollision struct {
	Names []string
	More  int
}

func (e *ErrCategoryMoveCollision) Error() string {
	msg := "tags named " + strings.Join(e.Names, ", ") + " already exist in the target category"
	if e.More > 0 {
		msg = fmt.Sprintf("%s (and %d more)", msg, e.More)
	}
	return msg
}

func collidingNames(tx *sql.Tx, id, targetID int64) ([]string, error) {
	return db.QueryStrings(tx,
		`SELECT t.name FROM tags t
		 WHERE t.category_id = ?
		   AND EXISTS (SELECT 1 FROM tags o WHERE o.category_id = ? AND o.name = t.name)
		 ORDER BY t.name`, id, targetID)
}

func isUniqueConstraintErr(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CategoryCounts is what deleting a category takes: its tags and aliases,
// and on delete_all the aliases elsewhere that resolve to its tags.
// Implications counts the edges declared on its tags.
type CategoryCounts struct {
	Tags, Aliases, LinkedAliases, Implications int
}

func (s *Service) CategoryDeleteCounts(id int64) (CategoryCounts, error) {
	var c CategoryCounts
	err := s.db.Read.QueryRow(
		`SELECT
		   (SELECT COUNT(*) FROM tags WHERE category_id = c.id AND is_alias = 0),
		   (SELECT COUNT(*) FROM tags WHERE category_id = c.id AND is_alias = 1),
		   (SELECT COUNT(*) FROM tags a JOIN tags t ON t.id = a.canonical_tag_id
		     WHERE a.is_alias = 1 AND a.category_id <> c.id AND t.category_id = c.id),
		   (SELECT COUNT(*) FROM tag_implications ti JOIN tags p ON p.id = ti.parent_tag_id
		     WHERE p.category_id = c.id)
		 FROM tag_categories c WHERE c.id = ?`, id,
	).Scan(&c.Tags, &c.Aliases, &c.LinkedAliases, &c.Implications)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrCategoryNotFound
	}
	return c, err
}

// DeleteCategoryMoveOrDelete deletes the tags on "delete_all" and
// otherwise moves them to targetID, general when 0.
func (s *Service) DeleteCategoryMoveOrDelete(id int64, action string, targetID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		var isBuiltin int
		if err := tx.QueryRow(
			`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
		).Scan(&isBuiltin); err == sql.ErrNoRows {
			return ErrCategoryNotFound
		} else if err != nil {
			return err
		}
		if isBuiltin == 1 {
			return ErrBuiltinCategory
		}

		switch action {
		case "delete_all":
			tagIDs, err := db.QueryIDs(tx, `SELECT id FROM tags WHERE category_id = ?`, id)
			if err != nil {
				return err
			}
			if err := deleteTagsTx(tx, tagIDs); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM tags WHERE category_id = ?`, id); err != nil {
				return err
			}
		default: // "move"
			if s.ratingCatID != 0 && targetID == s.ratingCatID {
				return ErrRatingCategoryClosed
			}
			switch targetID {
			case 0:
				if err := tx.QueryRow(
					`SELECT id FROM tag_categories WHERE name = 'general'`,
				).Scan(&targetID); err != nil {
					return fmt.Errorf("finding general category: %w", err)
				}
			case id:
				return ErrInvalidMoveTarget
			default:
				var exists int
				switch err := tx.QueryRow(
					`SELECT 1 FROM tag_categories WHERE id = ?`, targetID,
				).Scan(&exists); {
				case err == sql.ErrNoRows:
					return ErrInvalidMoveTarget
				case err != nil:
					return err
				}
			}
			clash, err := collidingNames(tx, id, targetID)
			if err != nil {
				return err
			}
			if len(clash) > 0 {
				more := max(len(clash)-collideNamesShown, 0)
				return &ErrCategoryMoveCollision{Names: clash[:min(len(clash), collideNamesShown)], More: more}
			}
			if _, err := tx.Exec(
				`UPDATE tags SET category_id = ? WHERE category_id = ?`, targetID, id,
			); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(`DELETE FROM tag_categories WHERE id = ?`, id); err != nil {
			return err
		}
		return nil
	})
}
