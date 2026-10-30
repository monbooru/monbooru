package tags

import (
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// A tag on more images than this adds noise, and pulling in its carriers
// makes the candidate GROUP BY slow on a large library.
const relatedMaxTagUsage = 10000

// Candidates are ranked from image_tags alone under an inner LIMIT;
// images is joined last, so the is_missing filter only sees that buffer.
func (s *Service) RelatedImages(imageID int64, limit int, excluded []int64) ([]models.Image, error) {
	var sourceFileType string
	if err := s.db.Read.QueryRow(`SELECT file_type FROM images WHERE id = ?`, imageID).Scan(&sourceFileType); err != nil {
		return nil, err
	}
	typePredicate := "i.file_type = 'cbz'"
	if sourceFileType != "cbz" {
		typePredicate = "i.file_type != 'cbz'"
	}

	candidatesExtra := ""
	args := []any{imageID, relatedMaxTagUsage, relatedGeneralTagsCap, imageID}
	if len(excluded) > 0 {
		placeholders, excludedArgs := db.InPlaceholders(excluded)
		candidatesExtra = ` AND NOT EXISTS (
		         SELECT 1 FROM image_tags x
		         WHERE x.image_id = theirs.image_id
		           AND x.tag_id IN (` + placeholders + `)
		     )`
		args = append(args, excludedArgs...)
	}
	args = append(args, limit*2+5, limit)

	return db.QueryAll(s.db.Read, db.ScanRow(func(img *models.Image) []any { return []any{&img.ID, &img.FileType, &img.PageCount} }),
		`WITH my_tags AS (
		     SELECT tag_id FROM (
		         SELECT it.tag_id, tc.name AS cat_name,
		                ROW_NUMBER() OVER (PARTITION BY tc.name
		                                   ORDER BY t.usage_count ASC, t.id ASC) AS rn
		         FROM image_tags it
		         JOIN tags t ON t.id = it.tag_id
		         JOIN tag_categories tc ON tc.id = t.category_id
		         WHERE it.image_id = ? AND tc.name != 'meta'
		           AND t.usage_count <= ?
		     )
		     WHERE cat_name != 'general' OR rn <= ?
		 ),
		 candidates AS (
		     SELECT theirs.image_id, COUNT(*) AS shared
		     FROM image_tags theirs
		     WHERE theirs.tag_id IN (SELECT tag_id FROM my_tags)
		       AND theirs.image_id != ?`+candidatesExtra+`
		     GROUP BY theirs.image_id
		     ORDER BY shared DESC, theirs.image_id DESC
		     LIMIT ?
		 )
		 SELECT i.id, i.file_type, i.page_count
		 FROM candidates c
		 JOIN images i ON i.id = c.image_id
		 WHERE i.is_missing = 0 AND `+typePredicate+`
		 ORDER BY c.shared DESC, c.image_id DESC
		 LIMIT ?`,
		args...,
	)
}

// categoryName narrows the suggestions to one category; "" takes every one.
func (s *Service) SuggestTags(prefix, categoryName string, limit int) ([]models.Tag, error) {
	return SuggestUsageRanked(s.db, prefix, categoryName, false, limit)
}

func SuggestUsageRanked(database *db.DB, prefix, categoryName string, requireUsage bool, limit int) ([]models.Tag, error) {
	prefix = db.EscapeLike(NormalizeTagName(prefix))
	// Ranked on tags alone so idx_tags_active_usage can stop at the
	// limit; with the category join the planner drives from
	// tag_categories and temp-sorts every tag.
	baseSQL := `SELECT t.id, t.name, tc.name, tc.color, t.usage_count
	            FROM (SELECT id, name, category_id, usage_count
	                  FROM tags
	                  WHERE is_alias = 0
	                    %s
	                    AND name LIKE ? ESCAPE '\'
	                    %s
	                  ORDER BY usage_count DESC, name ASC
	                  LIMIT ?) t
	            JOIN tag_categories tc ON tc.id = t.category_id
	            ORDER BY t.usage_count DESC, t.name ASC`
	usageClause := ""
	if requireUsage {
		usageClause = "AND usage_count > 0"
	}
	catClause := ""
	var catArgs []any
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	return SuggestTwoPass(prefix, limit, func(pat, notLike string, n int) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 2+len(catArgs))
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if notLike != "" {
			extra = extra + ` AND name NOT LIKE ? ESCAPE '\'`
			qargs = append(qargs, notLike)
		}
		qargs = append(qargs, n)
		return db.QueryAll(database.Read, ScanTag, fmt.Sprintf(baseSQL, usageClause, extra), qargs...)
	})
}

// SuggestTwoPass fills limit with names starting with the LIKE-escaped
// likePrefix, then tops it up with names containing it. A failed second
// pass returns the first pass's names with the error.
func SuggestTwoPass(likePrefix string, limit int, query func(pat, notLike string, n int) ([]models.Tag, error)) ([]models.Tag, error) {
	out, err := query(likePrefix+"%", "", limit)
	if err != nil {
		return nil, err
	}
	if len(out) < limit {
		more, err := query("%"+likePrefix+"%", likePrefix+"%", limit-len(out))
		if err != nil {
			return out, err
		}
		out = append(out, more...)
	}
	return out, nil
}

func ScanTag(rows *sql.Rows) (models.Tag, error) {
	var t models.Tag
	err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount)
	return t, err
}
