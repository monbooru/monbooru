package relations

import (
	"database/sql"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func copyableSourceRule(originalColOrPlaceholder string) string {
	return `lower(s.site) != '` + contract.SourcePTR + `'
	  AND NOT EXISTS (SELECT 1 FROM image_sources o
	                  WHERE o.image_id = ` + originalColOrPlaceholder + ` AND o.site = s.site AND o.post_id = s.post_id)
	  AND (s.url = '' OR NOT EXISTS (SELECT 1 FROM image_sources o
	                                 WHERE o.image_id = ` + originalColOrPlaceholder + ` AND o.url = s.url))`
}

// A source the original already carries, by post or by url, is left alone:
// its own row is the one a re-pull keeps up to date. A PTR origin is a
// match for the duplicate's own file, so it stays there.
var sourcesToCopyFrom = `
	FROM image_sources s
	JOIN dup_group_members m ON m.image_id = s.image_id
	JOIN dup_groups g ON g.id = m.group_id
	WHERE m.image_id != g.original_image_id
	  AND ` + copyableSourceRule("g.original_image_id")

// SourceToCopy names a duplicate's origin and the member it comes from.
type SourceToCopy struct {
	ImageID int64
	Site    string
	PostID  string
	URL     string
}

// Two members can carry the same post; the first one's row is copied.
func sourcesToCopy(q db.Querier, groupID int64) ([]SourceToCopy, error) {
	rows, err := db.QueryAll(q, db.ScanRow(func(s *SourceToCopy) []any {
		return []any{&s.ImageID, &s.Site, &s.PostID, &s.URL}
	}), `SELECT s.image_id, s.site, s.post_id, s.url`+sourcesToCopyFrom+` AND g.id = ? ORDER BY s.image_id, s.rowid`, groupID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := rows[:0]
	for _, r := range rows {
		post := "p\x00" + strings.ToLower(r.Site) + "\x00" + r.PostID
		url := "u\x00" + r.URL
		if seen[post] || (r.URL != "" && seen[url]) {
			continue
		}
		seen[post] = true
		if r.URL != "" {
			seen[url] = true
		}
		out = append(out, r)
	}
	return out, nil
}

// SourcesToCopy is what a copy to the group's original would add.
func (s *Service) SourcesToCopy(groupID int64) ([]SourceToCopy, error) {
	return sourcesToCopy(s.db.Read, groupID)
}

func (s *Service) GroupsWithSourcesToCopy(groupIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(groupIDs) == 0 {
		return out, nil
	}
	placeholders, args := db.InPlaceholders(groupIDs)
	err := db.QueryIDsFunc(s.db.Read, func(id int64) bool { out[id] = true; return true },
		`SELECT DISTINCT g.id`+sourcesToCopyFrom+` AND g.id IN (`+placeholders+`)`, args...)
	return out, err
}

func (s *Service) CopySourcesFromDuplicatesToOriginal(groupID int64) (int, error) {
	var added int
	err := s.inWriteTx(func(tx *sql.Tx) error {
		original, err := GroupOriginal(tx, groupID)
		if err != nil {
			return err
		}
		rows, err := sourcesToCopy(tx, groupID)
		if err != nil || len(rows) == 0 {
			return err
		}
		refs := make([]gallery.SourceRef, len(rows))
		for i, r := range rows {
			refs[i] = gallery.SourceRef{ImageID: r.ImageID, Site: r.Site, PostID: r.PostID}
		}
		added, err = gallery.CopySourcesTx(tx, original, refs)
		return err
	})
	return added, err
}
