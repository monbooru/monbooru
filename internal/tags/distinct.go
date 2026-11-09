package tags

import (
	"database/sql"
	"encoding/json"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// A rename or a merge drops its pairs, but an imported document can still
// carry one whose tags no longer share a name. CROSS JOIN keeps the few
// pairs as the driver instead of every active tag.
const liveDistinctPairs = `tag_distinct_pairs dp
	CROSS JOIN tags ta ON ta.id = dp.a_tag_id AND ta.is_alias = 0
	CROSS JOIN tags tb ON tb.id = dp.b_tag_id AND tb.is_alias = 0 AND tb.name = ta.name`

const distinctTagIDs = `SELECT dp.a_tag_id FROM ` + liveDistinctPairs + `
	UNION SELECT dp.b_tag_id FROM ` + liveDistinctPairs

// A row conflicts while one same-name tag holds no distinct mark with it.
// The IN narrows to shared names off idx_tags_active_name before the
// per-row probe.
const conflictWhere = `t.is_alias = 0 AND t.name IN (
		SELECT name FROM tags WHERE is_alias = 0
		GROUP BY name HAVING COUNT(*) >= 2)
	AND EXISTS (SELECT 1 FROM tags u
		WHERE u.name = t.name AND u.is_alias = 0 AND u.id <> t.id
		  AND NOT EXISTS (SELECT 1 FROM tag_distinct_pairs dp
		                  WHERE dp.a_tag_id = min(t.id, u.id) AND dp.b_tag_id = max(t.id, u.id)))`

const distinctWhere = `t.id IN (` + distinctTagIDs + `)`

type DistinctPartner struct {
	ID       int64
	Category string
}

func idsJSON(ids []int64) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

func (s *Service) countTagsWhere(where string) (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(*) FROM tags t WHERE ` + where).Scan(&n)
	return n, err
}

func (s *Service) ConflictsCount() (int, error) { return s.countTagsWhere(conflictWhere) }

func (s *Service) DistinctCount() (int, error) { return s.countTagsWhere(distinctWhere) }

func (s *Service) DistinctSettledCount() (int, error) {
	return s.countTagsWhere(distinctWhere + ` AND NOT (` + conflictWhere + `)`)
}

// MarkDistinct marks every pair of same-name tags among ids; a tag with no
// same-name tag in ids, or an alias, is skipped.
func (s *Service) MarkDistinct(ids []int64) (marked, skipped int, err error) {
	type row struct {
		id   int64
		name string
	}
	rows, err := db.QueryAll(s.db.Read, db.ScanRow(func(r *row) []any { return []any{&r.id, &r.name} }),
		`SELECT id, name FROM tags WHERE is_alias = 0 AND id IN (SELECT value FROM json_each(?)) ORDER BY name, id`,
		idsJSON(ids))
	if err != nil {
		return 0, 0, err
	}
	byName := map[string][]int64{}
	for _, r := range rows {
		byName[r.name] = append(byName[r.name], r.id)
	}
	skipped = len(ids) - len(rows)
	err = s.inWriteTx(func(tx *sql.Tx) error {
		for _, group := range byName {
			if len(group) < 2 {
				skipped++
				continue
			}
			for i, a := range group {
				for _, b := range group[i+1:] {
					res, err := tx.Exec(`INSERT OR IGNORE INTO tag_distinct_pairs (a_tag_id, b_tag_id) VALUES (?, ?)`, a, b)
					if err != nil {
						return err
					}
					n, _ := res.RowsAffected()
					marked += int(n)
				}
			}
		}
		return nil
	})
	return marked, skipped, err
}

func (s *Service) UnmarkDistinct(ids []int64) (int, error) {
	return writeTx(s, func(tx *sql.Tx) (int, error) {
		res, err := tx.Exec(`DELETE FROM tag_distinct_pairs
			WHERE a_tag_id IN (SELECT value FROM json_each(?1)) OR b_tag_id IN (SELECT value FROM json_each(?1))`,
			idsJSON(ids))
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		return int(n), err
	})
}

func (s *Service) UnmarkDistinctPair(a, b int64) (bool, error) {
	return writeTx(s, func(tx *sql.Tx) (bool, error) {
		res, err := tx.Exec(`DELETE FROM tag_distinct_pairs WHERE a_tag_id = min(?1, ?2) AND b_tag_id = max(?1, ?2)`, a, b)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		return n > 0, err
	})
}

func dropDistinctPairsTx(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`DELETE FROM tag_distinct_pairs WHERE a_tag_id = ?1 OR b_tag_id = ?1`, id)
	return err
}

func (s *Service) DistinctPartners(ids []int64) (map[int64][]DistinctPartner, error) {
	out := map[int64][]DistinctPartner{}
	if len(ids) == 0 {
		return out, nil
	}
	type pair struct {
		a, b       int64
		aCat, bCat string
	}
	pairs, err := db.QueryAll(s.db.Read, db.ScanRow(func(p *pair) []any { return []any{&p.a, &p.b, &p.aCat, &p.bCat} }),
		`SELECT dp.a_tag_id, dp.b_tag_id, ca.name, cb.name FROM `+liveDistinctPairs+`
		 JOIN tag_categories ca ON ca.id = ta.category_id
		 JOIN tag_categories cb ON cb.id = tb.category_id
		 WHERE dp.a_tag_id IN (SELECT value FROM json_each(?1)) OR dp.b_tag_id IN (SELECT value FROM json_each(?1))
		 ORDER BY dp.a_tag_id, dp.b_tag_id`,
		idsJSON(ids))
	if err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, p := range pairs {
		if want[p.a] {
			out[p.a] = append(out[p.a], DistinctPartner{ID: p.b, Category: p.bCat})
		}
		if want[p.b] {
			out[p.b] = append(out[p.b], DistinctPartner{ID: p.a, Category: p.aCat})
		}
	}
	return out, nil
}

// NamedTags lists the plain tags of every category holding one of names.
func (s *Service) NamedTags(names []string) ([]models.Tag, error) {
	in, args := db.InPlaceholders(names)
	return db.QueryAll(s.db.Read, db.ScanRow(func(t *models.Tag) []any {
		return []any{&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor, &t.UsageCount}
	}),
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color, t.usage_count
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.is_alias = 0 AND t.rejected = 0 AND t.name IN (`+in+`)
		 ORDER BY t.name, tc.name`, args...)
}

func (s *Service) SameNameTags(id int64) ([]models.Tag, error) {
	return db.QueryAll(s.db.Read, db.ScanRow(func(t *models.Tag) []any {
		return []any{&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor, &t.UsageCount}
	}),
		`SELECT u.id, u.name, u.category_id, tc.name, tc.color, u.usage_count
		 FROM tags t
		 JOIN tags u ON u.name = t.name AND u.is_alias = 0 AND u.id <> t.id
		 JOIN tag_categories tc ON tc.id = u.category_id
		 WHERE t.id = ? AND t.is_alias = 0
		 ORDER BY tc.name`, id)
}
