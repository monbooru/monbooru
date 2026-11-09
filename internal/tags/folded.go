package tags

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
)

var (
	// Runs outside the tag charset used before it was widened; names
	// stored then were folded into it. Kept in step with monloader's
	// mapping.LegacyFoldTag.
	legacyDisallowedChars = regexp.MustCompile(`[^a-z0-9_()!@#$.~+:?<>=^-]+`)
	underscoreRuns        = regexp.MustCompile(`_+`)
)

// LegacyFold returns the spelling the old charset would have stored for name.
func LegacyFold(name string) string {
	name = legacyDisallowedChars.ReplaceAllString(name, "_")
	name = underscoreRuns.ReplaceAllString(name, "_")
	return strings.Trim(name, "_")
}

// ScanFoldedDuplicates pairs each tag A with a richer B in the same
// category that folds onto it; an A with several such B is ambiguous and
// left to the operator.
func (s *Service) ScanFoldedDuplicates() (int, error) {
	type tagRow struct {
		id   int64
		name string
		cat  int64
	}
	all, err := db.QueryAll(s.db.Read, db.ScanRow(func(tr *tagRow) []any {
		return []any{&tr.id, &tr.name, &tr.cat}
	}), `SELECT id, name, category_id FROM tags WHERE is_alias = 0 AND rejected = 0`)
	if err != nil {
		return 0, err
	}
	byNameCat := map[int64]map[string]int64{}
	for _, tr := range all {
		if byNameCat[tr.cat] == nil {
			byNameCat[tr.cat] = map[string]int64{}
		}
		byNameCat[tr.cat][tr.name] = tr.id
	}

	type pair struct{ old, new, cat int64 }
	var pairs []pair
	countByOld := map[int64]int{}
	for _, b := range all {
		fold := LegacyFold(b.name)
		if fold == "" || fold == b.name {
			continue
		}
		// Differing only by collapsed underscores (girls__frontline) is
		// no widening; pairing it would retire the clean spelling.
		if !legacyDisallowedChars.MatchString(b.name) {
			continue
		}
		aID, ok := byNameCat[b.cat][fold]
		if !ok || aID == b.id {
			continue
		}
		pairs = append(pairs, pair{old: aID, new: b.id, cat: b.cat})
		countByOld[aID]++
	}

	err = s.inWriteTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM folded_tag_pairs`); err != nil {
			return err
		}
		for _, p := range pairs {
			ambiguous := 0
			if countByOld[p.old] > 1 {
				ambiguous = 1
			}
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO folded_tag_pairs (old_id, new_id, category_id, ambiguous) VALUES (?, ?, ?, ?)`,
				p.old, p.new, p.cat, ambiguous,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(countByOld), nil
}

// CROSS JOIN keeps the few pairs as the driver: planner statistics taken
// while the catalog was small pair every tag with its whole category.
const liveFoldedPairs = `folded_tag_pairs fp
	CROSS JOIN tags o ON o.id = fp.old_id AND o.is_alias = 0 AND o.rejected = 0
	CROSS JOIN tags n ON n.id = fp.new_id AND n.is_alias = 0 AND n.rejected = 0 AND n.category_id = o.category_id`

func (s *Service) FoldedDuplicatesCount() (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(DISTINCT fp.old_id) FROM ` + liveFoldedPairs).Scan(&n)
	return n, err
}

// Refused holds the distinct merge errors; a pair that no longer holds
// only counts in Skipped.
type FoldedMergeResult struct {
	Merged    int
	Skipped   int
	Refused   []error
	Cancelled bool
}

func (r *FoldedMergeResult) addRefusal(err error) {
	if slices.ContainsFunc(r.Refused, func(seen error) bool { return seen.Error() == err.Error() }) {
		return
	}
	r.Refused = append(r.Refused, err)
}

func (s *Service) MergeFolded(ctx context.Context, oldIDs []int64) (FoldedMergeResult, error) {
	var res FoldedMergeResult
	for _, oldID := range oldIDs {
		if ctx.Err() != nil {
			res.Cancelled = true
			return res, nil
		}
		// The pairs are as of the last scan: a merge, alias, rename or move
		// since can have undone one.
		var newID int64
		var oldName, newName string
		e := s.db.Read.QueryRow(
			`SELECT fp.new_id, o.name, n.name FROM `+liveFoldedPairs+` WHERE fp.old_id = ? AND fp.ambiguous = 0`, oldID,
		).Scan(&newID, &oldName, &newName)
		if e == sql.ErrNoRows || (e == nil && LegacyFold(newName) != oldName) {
			res.Skipped++
			continue
		}
		if e != nil {
			return res, e
		}
		if e := s.MergeTags(oldID, newID); e != nil {
			res.Skipped++
			res.addRefusal(e)
			continue
		}
		res.Merged++
		s.changed()
	}
	return res, nil
}
