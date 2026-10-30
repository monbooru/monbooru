// Package tags owns the tag catalog and every image's membership in it.
package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

var (
	ErrInvalidTagName       = errors.New("invalid tag name")
	ErrTagNotFound          = errors.New("tag not found")
	ErrTagNameTaken         = errors.New("already exists in this category")
	ErrTagImplied           = errors.New("this tag is implied by another tag on the image; remove the parent")
	ErrCategoryNotFound     = errors.New("category not found")
	ErrBuiltinCategory      = errors.New("cannot delete built-in category")
	ErrBuiltinCategoryName  = errors.New("cannot rename a built-in category")
	ErrReservedCategoryName = errors.New("this name is used by a search filter (e.g. " + reservedCategoryHint() + ")")
	ErrNonCanonicalRating   = errors.New("rating category accepts only general, sensitive, questionable, explicit")
	ErrRatingTagImmutable   = errors.New("rating category tags cannot be renamed, moved, or turned into aliases")
	ErrRatingCategoryClosed = errors.New("nothing new can go into the rating category")
	ErrInvalidMoveTarget    = errors.New("tags must move to another existing category")
	ErrAliasNote            = errors.New("an alias carries no note; edit the tag it resolves to")

	// A matching colour goes into style attributes as trusted CSS.
	categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

	ErrInvalidCategoryColor = errors.New("invalid category color (must be #rgb or #rrggbb)")

	// Names land in form-field names and in search syntax, where ':'
	// separates category from tag.
	categoryNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

	ErrInvalidCategoryName = errors.New("invalid category name (use lowercase letters, digits, underscore, or hyphen)")

	ErrCategoryExists = errors.New("a category with this name already exists")

	ErrCategoryOrderStale = errors.New("the categories changed since this page loaded; reload to see them")
)

func IsValidCategoryColor(s string) bool { return categoryColorRe.MatchString(s) }

// Theme --cat-<rrggbb> variables, the seed colours and <input type=color>
// all need lowercase six-digit hex.
func normalizeCategoryColor(s string) string {
	if !IsValidCategoryColor(s) {
		return s
	}
	s = strings.ToLower(s)
	if len(s) == 4 {
		return string([]byte{'#', s[1], s[1], s[2], s[2], s[3], s[3]})
	}
	return s
}

func SafeCategoryColor(s string) string {
	if IsValidCategoryColor(s) {
		return s
	}
	return "#888888"
}

// Search keywords and "system" (the search-bar cheat sheet) already own the
// "name:" prefix.
var reservedCategoryList = append(append([]string{}, searchkw.Keywords...), "system")

func IsCanonicalRating(name string) bool { return contract.RatingRank(name) >= 0 }

func isReservedCategoryName(name string) bool { return slices.Contains(reservedCategoryList, name) }

func reservedCategoryHint() string {
	parts := make([]string, len(reservedCategoryList))
	for i, n := range reservedCategoryList {
		parts[i] = n + ":"
	}
	return strings.Join(parts, ", ")
}

type TagFilter struct {
	CategoryID *int64
	Prefix     string
	Sort       string // "name" | "usage" | "created" | "last_used"
	Order      string // "asc" or "desc"; empty keeps the sort's default
	PageIndex  int
	Limit      int
	Origin     string
	Type       string
	// UsedBy matches any source that applied the tag, not only the one
	// that created it.
	UsedBy        string
	CreatedAfter  string
	ConflictsOnly bool
	ShowZero      bool
	ZeroOnly      bool
	Stale         string
	FoldedOnly    bool
	DistinctOnly  bool
}

type Service struct {
	db           *db.DB
	ratingCatID  int64
	generalCatID int64
	onChange     func()
}

// onChange runs after every write the service commits itself; a write
// into a caller's transaction leaves it to that caller.
func New(database *db.DB, onChange func()) *Service {
	s := &Service{db: database, onChange: onChange}
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'rating'`,
	).Scan(&s.ratingCatID); err != nil {
		logx.Warnf("tags.New: rating category lookup failed: %v", err)
	}
	if id, ok, err := CategoryIDByName(database, "general"); err != nil {
		logx.Warnf("tags.New: general category lookup failed: %v", err)
	} else if ok {
		s.generalCatID = id
	}
	return s
}

func (s *Service) RatingCategoryID() int64 { return s.ratingCatID }

func (s *Service) inWriteTx(work func(*sql.Tx) error) error {
	if err := db.InWriteTx(s.db.Write, work); err != nil {
		return err
	}
	s.changed()
	return nil
}

func writeTx[T any](s *Service, work func(*sql.Tx) (T, error)) (T, error) {
	var out T
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var err error
		out, err = work(tx)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func (s *Service) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

// A correlated-subquery UPDATE over every tag holds the writer for the
// whole catalog, so this zeroes the unused ones and fills the rest from
// one GROUP BY per tag_id chunk, freeing the writer between chunks.
func RecalcDBCount(database *db.DB) (int64, error) {
	const chunkSize = 2000

	var updated int64
	var maxID int64
	if err := database.Read.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM tags`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("max tag id: %w", err)
	}

	for start := int64(0); start <= maxID; start += chunkSize {
		end := start + chunkSize
		res, err := database.Write.Exec(`
			UPDATE tags SET usage_count = 0
			WHERE usage_count != 0
			  AND id >= ? AND id < ?
			  AND NOT EXISTS (
			      SELECT 1 FROM image_tags it
			      JOIN images i ON i.id = it.image_id
			      WHERE it.tag_id = tags.id AND i.is_missing = 0
			  )
		`, start, end)
		if err != nil {
			logx.Warnf("RecalcDBCount zero-out chunk [%d, %d): %v", start, end, err)
			return updated, fmt.Errorf("zero-out chunk [%d, %d): %w", start, end, err)
		}
		n, _ := res.RowsAffected()
		updated += n

		res, err = database.Write.Exec(`
			UPDATE tags SET usage_count = c.cnt
			FROM (
			    SELECT it.tag_id, COUNT(*) AS cnt FROM image_tags it
			    JOIN images i ON i.id = it.image_id
			    WHERE i.is_missing = 0 AND it.tag_id >= ? AND it.tag_id < ?
			    GROUP BY it.tag_id
			) c
			WHERE c.tag_id = tags.id AND tags.usage_count != c.cnt
		`, start, end)
		if err != nil {
			logx.Warnf("RecalcDBCount fill chunk [%d, %d): %v", start, end, err)
			return updated, fmt.Errorf("fill chunk [%d, %d): %w", start, end, err)
		}
		n, _ = res.RowsAffected()
		updated += n
	}
	return updated, nil
}

func (s *Service) RecalcCount() (int64, error) {
	n, err := RecalcDBCount(s.db)
	if n > 0 {
		s.changed()
	}
	return n, err
}
