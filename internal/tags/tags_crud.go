package tags

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// A name made only of these is content-free; emoticons like ">_<" pass on
// their other runes.
const tagDecorationClass = "_()!@#$.~+:-"

// With foldReserved false, '"' and '*' survive so the validator can
// reject them.
func buildTagName(name string, foldReserved bool) string {
	name = strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(name))
	pendingFold := false
	for _, r := range name {
		switch {
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co):
		case unicode.IsSpace(r) || (foldReserved && (r == '"' || r == '*')):
			pendingFold = true
		default:
			if pendingFold && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingFold = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

func NormalizeTagName(name string) string { return buildTagName(name, false) }

func HasTagContent(name string) bool {
	return strings.ContainsFunc(name, func(r rune) bool {
		return !strings.ContainsRune(tagDecorationClass, r)
	})
}

func ValidateTagName(name string) (string, error) {
	name = NormalizeTagName(name)

	if n := utf8.RuneCountInString(name); n == 0 || n > 200 {
		return "", fmt.Errorf("%w: length must be 1-200 characters", ErrInvalidTagName)
	}
	if strings.ContainsAny(name, `"*`) {
		return "", fmt.Errorf(`%w: contains invalid characters (allowed: any character except whitespace, " and *)`, ErrInvalidTagName)
	}
	if !HasTagContent(name) {
		return "", fmt.Errorf("%w: name must contain at least one letter, digit, or emoticon character", ErrInvalidTagName)
	}
	return name, nil
}

// NormalizeName is for imported names: '"' and '*' fold to '_' instead of
// failing, and it may return "".
func NormalizeName(name string) string { return strings.Trim(buildTagName(name, true), "_") }

func (s *Service) GetOrCreateTag(name string, categoryID int64) (*models.Tag, error) {
	return s.GetOrCreateTagFrom(name, categoryID, "user")
}

// GetOrCreateTagFrom stamps origin only on insert; an existing row keeps
// its creator. A rejected name answers its row beside ErrTagRejected.
func (s *Service) GetOrCreateTagFrom(name string, categoryID int64, origin string) (*models.Tag, error) {
	normalized, err := s.ValidateTagIn(name, categoryID)
	if err != nil {
		return nil, err
	}

	tag, err := writeTx(s, func(tx *sql.Tx) (*models.Tag, error) {
		return getOrCreateTagTx(tx, normalized, categoryID, origin)
	})
	if errors.Is(err, ErrTagRejected) {
		// The refusal rolled the transaction back, so the drop is counted here.
		if tag, _ = lookupTag(s.db.Read, normalized, categoryID); tag != nil {
			err = errors.Join(err, noteRejectedDrop(s.db.Write, tag.ID, origin))
		}
	}
	return tag, err
}

// GetOrCreateTagsFrom leaves a rejected name out.
func (s *Service) GetOrCreateTagsFrom(names []string, categoryID int64, origin string) ([]int64, error) {
	return writeTx(s, func(tx *sql.Tx) ([]int64, error) {
		ids := make([]int64, 0, len(names))
		for _, name := range names {
			normalized, err := s.ValidateTagIn(name, categoryID)
			if err != nil {
				return nil, err
			}
			tag, err := getOrCreateTagTx(tx, normalized, categoryID, origin)
			if errors.Is(err, ErrTagRejected) {
				if err := noteRejectedDrop(tx, tag.ID, origin); err != nil {
					return nil, err
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			ids = append(ids, tag.ID)
		}
		return ids, nil
	})
}

// ValidateTagIn returns the name as it would land in the category, or why
// the category refuses it.
func (s *Service) ValidateTagIn(name string, categoryID int64) (string, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return "", err
	}
	if s.ratingCatID != 0 && categoryID == s.ratingCatID && !IsCanonicalRating(normalized) {
		return "", ErrNonCanonicalRating
	}
	return normalized, nil
}

// FindTag resolves a name as GetOrCreateTag does, normalised and an alias
// followed to its canonical, but never creates one: a miss is ErrTagNotFound.
func (s *Service) FindTag(name string, categoryID int64) (*models.Tag, error) {
	tag, err := lookupTag(s.db.Read, NormalizeTagName(name), categoryID)
	if err == nil && tag == nil {
		return nil, ErrTagNotFound
	}
	return tag, err
}

func getOrCreateTagTx(tx *sql.Tx, name string, categoryID int64, origin string) (*models.Tag, error) {
	tag, err := lookupTag(tx, name, categoryID)
	if err != nil || tag != nil {
		if err == nil && tag.Rejected {
			return tag, ErrTagRejected
		}
		return tag, err
	}
	var id int64
	if err := tx.QueryRow(
		`INSERT INTO tags (name, category_id, origin) VALUES (?, ?, ?) RETURNING id`,
		name, categoryID, origin,
	).Scan(&id); err != nil {
		return nil, fmt.Errorf("inserting tag: %w", err)
	}
	return &models.Tag{
		ID:         id,
		Name:       name,
		CategoryID: categoryID,
		Origin:     origin,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

// lookupTag returns nil, nil when nothing holds the name.
func lookupTag(q db.RowQuerier, name string, categoryID int64) (*models.Tag, error) {
	var tag models.Tag
	var createdAt string
	var canonicalID sql.NullInt64
	err := q.QueryRow(
		`SELECT id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at, rejected, rejected_drops FROM tags WHERE name = ? AND category_id = ?`,
		name, categoryID,
	).Scan(&tag.ID, &tag.Name, &tag.CategoryID, &tag.UsageCount, &tag.IsAlias, &canonicalID, &createdAt, &tag.Rejected, &tag.RejectedDrops)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// One hop: write paths never leave an alias pointing at another
	// alias. A dangling canonical is an error, not a fallback that keys
	// image_tags on the alias.
	if tag.IsAlias && canonicalID.Valid {
		var canon models.Tag
		var canonCreated string
		if err := q.QueryRow(
			`SELECT id, name, category_id, usage_count, is_alias, created_at, rejected, rejected_drops FROM tags WHERE id = ?`,
			canonicalID.Int64,
		).Scan(&canon.ID, &canon.Name, &canon.CategoryID, &canon.UsageCount, &canon.IsAlias, &canonCreated, &canon.Rejected, &canon.RejectedDrops); err != nil {
			return nil, fmt.Errorf("resolving canonical %d for alias %q: %w", canonicalID.Int64, tag.Name, err)
		}
		canon.CreatedAt, _ = time.Parse(time.RFC3339, canonCreated)
		return &canon, nil
	}

	tag.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &tag, nil
}

func tagFilterWhere(filter TagFilter) (string, []any) {
	args := []any{}
	where := "1=1"

	if filter.CategoryID != nil {
		where += " AND t.category_id = ?"
		args = append(args, *filter.CategoryID)
	}
	if filter.Prefix != "" {
		pat := db.EscapeLike(filter.Prefix)
		if strings.Contains(filter.Prefix, "*") {
			pat = strings.ReplaceAll(pat, "*", "%")
		} else {
			pat += "%"
		}
		where += " AND t.name LIKE ? ESCAPE '\\'"
		args = append(args, pat)
	}
	switch filter.Origin {
	case "":
	case "alias":
		// A documented API value: origin=alias narrows to alias rows.
		where += " AND t.is_alias = 1"
	default:
		where += " AND t.origin = ?"
		args = append(args, filter.Origin)
	}
	switch filter.Type {
	case "alias":
		where += " AND t.is_alias = 1"
	case "tag":
		where += " AND t.is_alias = 0 AND t.rejected = 0"
	case "rejected":
		where += " AND t.rejected = 1"
	}
	if filter.UsedBy != "" {
		// EXISTS stops at the first ledger row; the IN-subquery form
		// materialises every row the source ever wrote instead.
		where += ` AND EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.source = ? AND s.tag_id = t.id)`
		args = append(args, filter.UsedBy)
	}
	if filter.CreatedAfter != "" {
		where += " AND t.created_at >= ?"
		args = append(args, filter.CreatedAfter)
	}
	if filter.ConflictsOnly {
		where += ` AND ` + conflictWhere
	}
	if filter.DistinctOnly {
		where += ` AND ` + distinctWhere
	}
	// IN bounds the scan to tags with a stale row. Implied rows are never
	// stale, so live implied usage keeps a tag out of "full".
	switch filter.Stale {
	case "has":
		where += ` AND t.id IN (SELECT tag_id FROM image_tags WHERE stale = 1)`
	case "full":
		where += ` AND ` + fullyStaleWhere
	}
	if filter.FoldedOnly {
		where += ` AND t.id IN (SELECT fp.old_id FROM ` + liveFoldedPairs + `)`
	}
	switch {
	case filter.ZeroOnly:
		// Alias and rejected rows always sit at usage 0, so both cases
		// decide them by their flags.
		where += " AND t.usage_count = 0 AND t.is_alias = 0"
		if filter.Type != "rejected" {
			where += " AND t.rejected = 0"
		}
	case !filter.ShowZero:
		where += " AND (t.usage_count > 0 OR t.is_alias = 1 OR t.rejected = 1)"
	}
	return where, args
}

// The id tiebreak keeps prev/next on the detail page in step with the listing.
func tagOrderBy(filter TagFilter) string {
	dir := "ASC"
	if strings.EqualFold(filter.Order, "desc") {
		dir = "DESC"
	}
	orderBy := "t.name " + dir
	switch {
	case filter.ConflictsOnly, filter.DistinctOnly:
		// Same-name rows must sit together whatever the requested order.
		orderBy = "t.name ASC, t.category_id ASC"
	case filter.Sort == "usage" || filter.Sort == "created" || filter.Sort == "last_used":
		dir = "DESC"
		if strings.EqualFold(filter.Order, "asc") {
			dir = "ASC"
		}
		switch filter.Sort {
		case "usage":
			orderBy = "t.usage_count " + dir + ", t.name ASC"
		case "created":
			return "t.created_at " + dir + ", t.id " + dir
		case "last_used":
			orderBy = "t.last_used_at " + dir + ", t.name ASC"
		}
	}
	return orderBy + ", t.id ASC"
}

func (s *Service) ListTags(filter TagFilter) ([]models.Tag, int, error) {
	where, args := tagFilterWhere(filter)
	orderBy := tagOrderBy(filter)

	var total int
	if err := s.db.Read.QueryRow(
		"SELECT COUNT(*) FROM tags t WHERE "+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 40
	}
	offset := filter.PageIndex * limit

	foldedCol := "NULL"
	if filter.FoldedOnly {
		foldedCol = `(SELECT n.name FROM ` + liveFoldedPairs + ` WHERE fp.old_id = t.id AND fp.ambiguous = 0 LIMIT 1)`
	}

	// The page is picked from tags alone so the sort index can stop at
	// the limit; with the joins in the same SELECT the planner drives
	// from tag_categories and temp-sorts the whole catalog.
	query := fmt.Sprintf(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color,
		        t.usage_count, t.is_alias, t.canonical_tag_id, t.created_at,
		        t.origin, t.last_used_at, t.rejected, t.rejected_drops,
		        (SELECT COUNT(*) FROM image_tags it WHERE it.tag_id = t.id AND it.stale = 1),
		        %s,
		        c.name, cc.name, cc.color
		 FROM (SELECT t.* FROM tags t WHERE %s ORDER BY %s LIMIT ? OFFSET ?) t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 LEFT JOIN tag_categories cc ON cc.id = c.category_id
		 ORDER BY %s`,
		foldedCol, where, orderBy, orderBy,
	)
	args = append(args, limit, offset)

	tagList, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.Tag, error) {
		var t models.Tag
		var isAlias int
		var createdAt string
		var lastUsed, foldedInto, canonName, canonCatName, canonCatColor sql.NullString
		if err := rows.Scan(
			&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
			&t.UsageCount, &isAlias, &t.CanonicalTagID, &createdAt,
			&t.Origin, &lastUsed, &t.Rejected, &t.RejectedDrops, &t.StaleUsage, &foldedInto,
			&canonName, &canonCatName, &canonCatColor,
		); err != nil {
			return t, err
		}
		t.FoldedInto = foldedInto.String
		t.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
		t.IsAlias = isAlias == 1
		t.CanonicalName = canonName.String
		t.CanonicalCategoryName = canonCatName.String
		t.CanonicalCategoryColor = canonCatColor.String
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		return t, nil
	}, query, args...)
	if err != nil {
		return nil, 0, err
	}
	return tagList, total, nil
}

func (s *Service) StaleUsageCount() (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(DISTINCT tag_id) FROM image_tags WHERE stale = 1`).Scan(&n)
	return n, err
}

// usage_count counts visible images only, so the stale side must too.
const fullyStaleWhere = `t.usage_count > 0 AND t.id IN (SELECT tag_id FROM image_tags WHERE stale = 1)
	AND (SELECT COUNT(*) FROM image_tags it JOIN images mi ON mi.id = it.image_id AND mi.is_missing = 0
	     WHERE it.tag_id = t.id AND it.stale = 1) = t.usage_count`

func (s *Service) FullyStaleCount() (int, error) { return s.countTagsWhere(fullyStaleWhere) }

type OriginCount struct {
	Label string
	Count int
}

// typeFilter is the listing's Type, so a badge counts the rows its own
// link shows.
func (s *Service) OriginCounts(typeFilter string) ([]OriginCount, error) {
	where := ""
	switch typeFilter {
	case "alias":
		where = " AND is_alias = 1"
	case "tag":
		where = " AND is_alias = 0 AND rejected = 0"
	case "rejected":
		where = " AND rejected = 1"
	}
	return db.QueryAll(s.db.Read, db.ScanRow(func(oc *OriginCount) []any {
		return []any{&oc.Label, &oc.Count}
	}),
		`SELECT origin, COUNT(*) FROM tags WHERE origin <> ''`+where+
			` GROUP BY origin ORDER BY COUNT(*) DESC, origin ASC`,
	)
}

func (s *Service) AutoTaggerLabels(labels []string) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	wanted := slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(labels))), func(l string) bool { return l == "" })
	if len(wanted) == 0 {
		return set, nil
	}
	placeholders, args := db.InPlaceholders(wanted)
	// The IS NOT NULL and != '' terms restate idx_image_tags_auto_tagger's
	// partial predicate, without which the planner scans image_tags.
	labels, err := db.QueryStrings(s.db.Read,
		`SELECT DISTINCT tagger_name FROM image_tags
		 WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != ''
		   AND tagger_name IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		set[l] = struct{}{}
	}
	return set, nil
}

func (s *Service) ListTagIDs(filter TagFilter) ([]int64, error) {
	where, args := tagFilterWhere(filter)
	return db.QueryIDs(s.db.Read, `SELECT t.id FROM tags t WHERE `+where+` ORDER BY t.id`, args...)
}

// UnusedTags lists the plain tags no image row carries, missing images
// included: usage_count counts visible images only, so a zero there can
// still sit on a missing file that comes back. kept holds those with an
// alias, an implication either way, or a note, which an operator curated.
func (s *Service) UnusedTags() (plain, kept []int64, err error) {
	type row struct {
		id      int64
		curated bool
	}
	rows, err := db.QueryAll(s.db.Read, db.ScanRow(func(r *row) []any { return []any{&r.id, &r.curated} }),
		`SELECT t.id,
		        EXISTS (SELECT 1 FROM tags a WHERE a.canonical_tag_id = t.id)
		        OR EXISTS (SELECT 1 FROM tag_implications i WHERE i.parent_tag_id = t.id)
		        OR EXISTS (SELECT 1 FROM tag_implications i WHERE i.implied_tag_id = t.id)
		        OR EXISTS (SELECT 1 FROM tag_notes n WHERE n.tag_id = t.id AND (n.body != '' OR n.links != ''))
		   FROM tags t
		  WHERE t.is_alias = 0 AND t.rejected = 0 AND t.category_id != ?
		    AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.tag_id = t.id)
		  ORDER BY t.id`, s.ratingCatID)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		if r.curated {
			kept = append(kept, r.id)
		} else {
			plain = append(plain, r.id)
		}
	}
	return plain, kept, nil
}

// TagResetScope is everything a tag reset deletes: the aliases pointing at
// one of the four ratings, which no canonical delete sweeps, then every
// plain tag but those four. Deleting the canonicals takes every other alias
// and every implication with them.
func (s *Service) TagResetScope() (ratingAliases, ids []int64, aliases, implications int, err error) {
	canonical := []any{s.ratingCatID}
	for _, level := range contract.RatingLevels {
		canonical = append(canonical, level)
	}
	if ratingAliases, err = db.QueryIDs(s.db.Read,
		`SELECT a.id FROM tags a JOIN tags c ON c.id = a.canonical_tag_id
		  WHERE a.is_alias = 1 AND c.category_id = ? AND c.name IN (?, ?, ?, ?) ORDER BY a.id`, canonical...); err != nil {
		return nil, nil, 0, 0, err
	}
	if ids, err = db.QueryIDs(s.db.Read,
		`SELECT id FROM tags WHERE is_alias = 0 AND NOT (category_id = ? AND name IN (?, ?, ?, ?)) ORDER BY id`, canonical...); err != nil {
		return nil, nil, 0, 0, err
	}
	err = s.db.Read.QueryRow(
		`SELECT (SELECT COUNT(*) FROM tags WHERE is_alias = 1), (SELECT COUNT(*) FROM tag_implications)`,
	).Scan(&aliases, &implications)
	return ratingAliases, ids, aliases, implications, err
}

func (s *Service) AdjacentTags(filter TagFilter, id int64) (prev, next *int64, err error) {
	where, args := tagFilterWhere(filter)
	// No key to seek on, so the ordered scan streams and stops one row
	// past the match.
	var last int64
	var seen, found bool
	err = db.QueryIDsFunc(s.db.Read, func(cur int64) bool {
		switch {
		case found:
			n := cur
			next = &n
			return false
		case cur == id:
			found = true
			if seen {
				p := last
				prev = &p
			}
		}
		last, seen = cur, true
		return true
	}, `SELECT t.id FROM tags t WHERE `+where+` ORDER BY `+tagOrderBy(filter), args...)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, nil
	}
	return prev, next, nil
}

const tagSelect = `SELECT t.id, t.name, t.category_id, tc.name, tc.color, t.usage_count,
	        t.is_alias, t.canonical_tag_id, t.created_at, t.origin, t.last_used_at, t.rejected, t.rejected_drops
	 FROM tags t
	 JOIN tag_categories tc ON tc.id = t.category_id`

func scanTag(row interface{ Scan(...any) error }) (*models.Tag, error) {
	var t models.Tag
	var isAlias int
	var createdAt string
	var lastUsed sql.NullString
	if err := row.Scan(
		&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
		&t.UsageCount, &isAlias, &t.CanonicalTagID, &createdAt, &t.Origin, &lastUsed, &t.Rejected, &t.RejectedDrops,
	); err != nil {
		return nil, err
	}
	t.IsAlias = isAlias == 1
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	t.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
	return &t, nil
}

func (s *Service) GetTag(id int64) (*models.Tag, error) {
	t, err := scanTag(s.db.Read.QueryRow(tagSelect+` WHERE t.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrTagNotFound
	}
	return t, err
}

// GetTags leaves an id that matches no tag out of the map.
func (s *Service) GetTags(ids []int64) (map[int64]*models.Tag, error) {
	out := make(map[int64]*models.Tag, len(ids))
	err := db.Chunked(ids, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		found, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (*models.Tag, error) { return scanTag(rows) },
			tagSelect+` WHERE t.id IN (`+placeholders+`)`, args...)
		for _, t := range found {
			out[t.ID] = t
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) AliasesForTagIDs(canonicalIDs []int64) (map[int64][]models.Tag, error) {
	out := make(map[int64][]models.Tag, len(canonicalIDs))
	if len(canonicalIDs) == 0 {
		return out, nil
	}
	err := db.Chunked(canonicalIDs, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		aliases, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.Tag, error) {
			var t models.Tag
			var canonicalID int64
			var stale int64
			err := rows.Scan(
				&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
				&canonicalID, &t.Origin, &stale,
				&t.CanonicalName, &t.CanonicalCategoryName, &t.CanonicalCategoryColor,
			)
			t.Stale = stale == 1
			t.IsAlias = true
			t.CanonicalTagID = &canonicalID
			return t, err
		},
			`SELECT a.id, a.name, a.category_id, ac.name, ac.color,
			        a.canonical_tag_id, a.origin, a.stale,
			        c.name, cc.name, cc.color
			 FROM tags a
			 JOIN tag_categories ac ON ac.id = a.category_id
			 JOIN tags c ON c.id = a.canonical_tag_id
			 JOIN tag_categories cc ON cc.id = c.category_id
			 WHERE a.is_alias = 1 AND a.canonical_tag_id IN (`+placeholders+`)
			 ORDER BY a.name`,
			args...)
		if err != nil {
			return err
		}
		for _, t := range aliases {
			out[*t.CanonicalTagID] = append(out[*t.CanonicalTagID], t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type AliasKey struct {
	CategoryID int64
	Name       string
}

// SyncAliasStaleness only flags or clears: the operator decides what to remove.
func (s *Service) SyncAliasStaleness(canonicalID int64, origin string, fresh map[AliasKey]bool) (int, error) {
	return writeTx(s, func(tx *sql.Tx) (int, error) {
		type aliasRow struct {
			id, catID, stale int64
			name             string
		}
		present, err := db.QueryAll(tx, db.ScanRow(func(a *aliasRow) []any {
			return []any{&a.id, &a.catID, &a.name, &a.stale}
		}), `SELECT id, category_id, name, stale FROM tags
			 WHERE is_alias = 1 AND canonical_tag_id = ? AND origin = ?`, canonicalID, origin)
		if err != nil {
			return 0, err
		}
		var flag, clear []int64
		for _, a := range present {
			switch current := fresh[AliasKey{CategoryID: a.catID, Name: a.name}]; {
			case !current && a.stale == 0:
				flag = append(flag, a.id)
			case current && a.stale == 1:
				clear = append(clear, a.id)
			}
		}
		if err := setTagsStaleTx(tx, flag, 1); err != nil {
			return 0, err
		}
		if err := setTagsStaleTx(tx, clear, 0); err != nil {
			return 0, err
		}
		return len(flag), nil
	})
}

func setTagsStaleTx(tx *sql.Tx, ids []int64, stale int) error {
	return setStaleTx(tx, "tags", "id", "", ids, stale)
}

// extraWhere is raw SQL ending in " AND ", for a table with a second key.
func setStaleTx(tx *sql.Tx, table, keyCol, extraWhere string, ids []int64, stale int) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders, args := db.InPlaceholders(ids)
	_, err := tx.Exec(
		`UPDATE `+table+` SET stale = `+strconv.Itoa(stale)+
			` WHERE `+extraWhere+keyCol+` IN (`+placeholders+`)`,
		args...)
	return err
}

type AppliedByCount struct {
	Label   string // tagger_name; "" = anonymous UI adds and every Implied row
	IsAuto  bool
	Implied bool
	Count   int
}

type UsageMonth struct {
	Month string // YYYY-MM
	Count int
}

// One pass for both views: on a popular tag the row read dominates.
func (s *Service) UsageBreakdown(tagID int64) ([]AppliedByCount, []UsageMonth, error) {
	// An implied row has no tagger and the parent's is_auto: apart, or it reads as a manual add.
	rows, err := s.db.Read.Query(
		`SELECT CASE WHEN it.is_implied = 1 THEN '' ELSE COALESCE(it.tagger_name, '') END AS label,
		        CASE WHEN it.is_implied = 1 THEN 0 ELSE it.is_auto END AS auto,
		        it.is_implied, strftime('%Y-%m', it.created_at) AS month, COUNT(*)
		 FROM image_tags it JOIN images i ON i.id = it.image_id AND i.is_missing = 0
		 WHERE it.tag_id = ?
		 GROUP BY label, auto, it.is_implied, month`,
		tagID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	appliedIdx := make(map[[3]any]int)
	monthIdx := make(map[string]int)
	var applied []AppliedByCount
	var months []UsageMonth
	for rows.Next() {
		var label, month string
		var isAuto, implied, count int
		if err := rows.Scan(&label, &isAuto, &implied, &month, &count); err != nil {
			return nil, nil, err
		}
		ak := [3]any{label, isAuto, implied}
		if i, ok := appliedIdx[ak]; ok {
			applied[i].Count += count
		} else {
			appliedIdx[ak] = len(applied)
			applied = append(applied, AppliedByCount{Label: label, IsAuto: isAuto == 1, Implied: implied == 1, Count: count})
		}
		if i, ok := monthIdx[month]; ok {
			months[i].Count += count
		} else {
			monthIdx[month] = len(months)
			months = append(months, UsageMonth{Month: month, Count: count})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(applied, func(a, b AppliedByCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Label, b.Label))
	})
	sort.Slice(months, func(i, j int) bool { return months[i].Month < months[j].Month })
	return applied, months, nil
}

// GetImageTags also returns the image's folder_path, empty for an unknown
// image.
func (s *Service) GetImageTags(imageID int64) (string, []models.ImageTag, error) {
	rows, err := s.db.Read.Query(
		`SELECT i.folder_path,
		        it.image_id, it.tag_id, t.name, tc.name, tc.color, tc.position, t.usage_count,
		        it.is_auto, it.is_implied, it.confidence, it.tagger_name, it.stale, it.created_at
		 FROM images i
		 LEFT JOIN image_tags it ON it.image_id = i.id
		 LEFT JOIN tags t ON t.id = it.tag_id
		 LEFT JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE i.id = ?
		 ORDER BY tc.name, t.usage_count DESC, t.name`, imageID,
	)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rows.Close() }()

	var folder string
	var result []models.ImageTag
	for rows.Next() {
		var (
			folderPath sql.NullString
			imgID      sql.NullInt64
			tagID      sql.NullInt64
			tagName    sql.NullString
			category   sql.NullString
			color      sql.NullString
			catPos     sql.NullInt64
			usage      sql.NullInt64
			isAuto     sql.NullInt64
			isImplied  sql.NullInt64
			conf       *float64
			tagger     sql.NullString
			stale      sql.NullInt64
			createdAt  sql.NullString
		)
		if err := rows.Scan(
			&folderPath,
			&imgID, &tagID, &tagName, &category, &color, &catPos, &usage,
			&isAuto, &isImplied, &conf, &tagger, &stale, &createdAt,
		); err != nil {
			return "", nil, err
		}
		folder = folderPath.String
		if !imgID.Valid || !tagID.Valid {
			continue
		}
		it := models.ImageTag{
			ImageID:          imgID.Int64,
			TagID:            tagID.Int64,
			TagName:          tagName.String,
			Category:         category.String,
			Color:            color.String,
			CategoryPosition: int(catPos.Int64),
			UsageCount:       int(usage.Int64),
			IsAuto:           isAuto.Int64 == 1,
			IsImplied:        isImplied.Int64 == 1,
			Stale:            stale.Int64 == 1,
			Confidence:       conf,
			TaggerName:       tagger.String,
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		result = append(result, it)
	}
	return folder, result, rows.Err()
}

// The FK cascade alone would strand implied rows, so the sweep runs here.
// The caller deletes the tag rows.
func deleteTagsTx(tx *sql.Tx, ids []int64) error {
	if err := stripTagsTx(tx, ids); err != nil {
		return err
	}
	for _, id := range ids {
		// Old rows can chain alias -> alias, even back through this tag.
		// Clearing its own pointer first and dropping the chain in one
		// statement keeps every FK check satisfied.
		if _, err := tx.Exec(`UPDATE tags SET canonical_tag_id = NULL WHERE id = ?`, id); err != nil {
			return fmt.Errorf("unlink canonical: %w", err)
		}
		if _, err := tx.Exec(
			`DELETE FROM tags WHERE id IN (
			     WITH RECURSIVE sub(id) AS (
			         SELECT id FROM tags WHERE canonical_tag_id = ?
			         UNION
			         SELECT t.id FROM tags t JOIN sub ON t.canonical_tag_id = sub.id
			     )
			     SELECT id FROM sub
			 )`, id,
		); err != nil {
			return fmt.Errorf("delete aliases: %w", err)
		}
	}
	return nil
}

// Only the images that carried a stripped tag can have lost a parent.
func stripTagsTx(tx *sql.Tx, ids []int64) error {
	var carriers []int64
	for _, id := range ids {
		got, err := db.QueryIDs(tx, `DELETE FROM image_tags WHERE tag_id = ? RETURNING image_id`, id)
		if err != nil {
			return fmt.Errorf("strip parent image_tags: %w", err)
		}
		carriers = append(carriers, got...)
	}
	slices.Sort(carriers)
	carriers = slices.Compact(carriers)
	if err := db.Chunked(carriers, 500, func(chunk []int64) error {
		_, err := pruneOrphanedImpliedTx(tx, chunk)
		return err
	}); err != nil {
		return fmt.Errorf("sweep implied: %w", err)
	}
	return nil
}

// DeleteTag keeps a canonical rating row and only strips it from every image.
func (s *Service) DeleteTag(id int64) error {
	locked, err := s.isLockedRatingTag(id)
	if err != nil {
		return err
	}
	if locked {
		return s.stripTagFromAllImages(id)
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := deleteTagsTx(tx, []int64{id}); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrTagNotFound
		}
		return nil
	})
}

func (s *Service) stripTagFromAllImages(tagID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := stripTagsTx(tx, []int64{tagID}); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tags SET usage_count = 0 WHERE id = ?`, tagID); err != nil {
			return fmt.Errorf("zero usage: %w", err)
		}
		return nil
	})
}

func (s *Service) isRatingTag(id int64) (bool, error) {
	_, ok, err := s.ratingRowName(id)
	return ok, err
}

// Only the canonical four are locked: an import can leave other names in
// the rating category, and those must stay repairable.
func (s *Service) isLockedRatingTag(id int64) (bool, error) {
	name, ok, err := s.ratingRowName(id)
	return ok && IsCanonicalRating(name), err
}

func (s *Service) ratingRowName(id int64) (string, bool, error) {
	if s.ratingCatID == 0 {
		return "", false, nil
	}
	var catID int64
	var name string
	switch err := s.db.Read.QueryRow(`SELECT category_id, name FROM tags WHERE id = ?`, id).Scan(&catID, &name); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return name, catID == s.ratingCatID, nil
}

func (s *Service) RenameTag(id int64, newName string) error { return s.renameTag(id, newName, false) }

func nameTaken(q db.RowQuerier, name string, catID, exceptID int64) (int64, error) {
	var existing int64
	switch err := q.QueryRow(
		`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`, name, catID, exceptID,
	).Scan(&existing); {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("check name %q in category %d: %w", name, catID, err)
	}
	return existing, nil
}

// RenameTagKeepAlias leaves the old name behind as an alias of the renamed tag.
func (s *Service) RenameTagKeepAlias(id int64, newName string) error {
	return s.renameTag(id, newName, true)
}

func (s *Service) renameTag(id int64, newName string, keepAlias bool) error {
	normalized, err := ValidateTagName(newName)
	if err != nil {
		return err
	}
	return s.inWriteTx(func(tx *sql.Tx) error { return s.renameTagTx(tx, id, normalized, keepAlias) })
}

// RenameAndMoveTag stores both or neither, so a refused move leaves the
// old name.
func (s *Service) RenameAndMoveTag(id int64, newName string, newCategoryID int64) error {
	return s.renameAndMoveTag(id, newName, newCategoryID, false)
}

func (s *Service) RenameAndMoveTagKeepAlias(id int64, newName string, newCategoryID int64) error {
	return s.renameAndMoveTag(id, newName, newCategoryID, true)
}

func (s *Service) renameAndMoveTag(id int64, newName string, newCategoryID int64, keepAlias bool) error {
	normalized, err := ValidateTagName(newName)
	if err != nil {
		return err
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		var oldName string
		var oldCatID int64
		var isAlias int
		if err := tx.QueryRow(`SELECT name, category_id, is_alias FROM tags WHERE id = ?`, id).Scan(&oldName, &oldCatID, &isAlias); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTagNotFound
			}
			return fmt.Errorf("look up tag %d: %w", id, err)
		}
		if keepAlias && isAlias == 1 {
			return errKeepAliasOfAlias
		}
		if oldCatID == newCategoryID {
			return s.renameTagTx(tx, id, normalized, keepAlias)
		}
		if err := s.changeTagCategoryTx(tx, id, newCategoryID, normalized); err != nil {
			return err
		}
		if oldName != normalized {
			if err := dropRenamedTagPairsTx(tx, id); err != nil {
				return err
			}
		}
		if !keepAlias {
			return nil
		}
		return insertOldNameAliasTx(tx, oldName, oldCatID, id)
	})
}

var errKeepAliasOfAlias = errors.New("cannot keep the old name of an alias; rename it plainly")

func insertOldNameAliasTx(tx *sql.Tx, name string, catID, canonicalID int64) error {
	_, err := tx.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin) VALUES (?, ?, 1, ?, 0, 'user')`,
		name, catID, canonicalID,
	)
	return err
}

func (s *Service) renameTagTx(tx *sql.Tx, id int64, normalized string, keepAlias bool) error {
	var catID int64
	var oldName string
	var isAlias int
	if err := tx.QueryRow(`SELECT category_id, name, is_alias FROM tags WHERE id = ?`, id).Scan(&catID, &oldName, &isAlias); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTagNotFound
		}
		return fmt.Errorf("look up tag %d: %w", id, err)
	}
	if keepAlias && isAlias == 1 {
		return errKeepAliasOfAlias
	}
	if s.ratingCatID != 0 && catID == s.ratingCatID && IsCanonicalRating(oldName) {
		return ErrRatingTagImmutable
	}
	if oldName == normalized {
		return nil
	}
	existing, err := nameTaken(tx, normalized, catID, id)
	if err != nil {
		return err
	}
	if existing != 0 {
		return fmt.Errorf("a tag named %q %w", normalized, ErrTagNameTaken)
	}
	if _, err := tx.Exec(`UPDATE tags SET name = ? WHERE id = ?`, normalized, id); err != nil {
		return err
	}
	if err := dropRenamedTagPairsTx(tx, id); err != nil {
		return err
	}
	if !keepAlias {
		return nil
	}
	return insertOldNameAliasTx(tx, oldName, catID, id)
}

func dropRenamedTagPairsTx(tx *sql.Tx, id int64) error {
	if _, err := tx.Exec(`DELETE FROM folded_tag_pairs WHERE old_id = ? OR new_id = ?`, id, id); err != nil {
		return err
	}
	return dropDistinctPairsTx(tx, id)
}

type ErrCategoryCollision struct {
	Name       string
	ExistingID int64
}

func (e *ErrCategoryCollision) Error() string {
	return fmt.Sprintf("a tag named %q already exists in the target category", e.Name)
}

// ChangeTagCategoryMerge merges into the target's same-name tag on a
// collision and reports whether it did. A merge always leaves the old
// spelling as an alias; keepAlias does the same for a plain move.
func (s *Service) ChangeTagCategoryMerge(tagID, newCategoryID int64, keepAlias bool) (bool, error) {
	move := s.ChangeTagCategory
	if keepAlias {
		move = s.ChangeTagCategoryKeepAlias
	}
	err := move(tagID, newCategoryID)
	var coll *ErrCategoryCollision
	if errors.As(err, &coll) {
		if err := s.MergeTags(tagID, coll.ExistingID); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, err
}

// An alias row moves plainly: an alias of an alias would never resolve.
func (s *Service) ChangeTagCategoryKeepAlias(tagID, newCategoryID int64) error {
	t, err := s.GetTag(tagID)
	if err != nil {
		return err
	}
	if t.IsAlias {
		return s.ChangeTagCategory(tagID, newCategoryID)
	}
	return s.RenameAndMoveTagKeepAlias(tagID, t.Name, newCategoryID)
}

func (s *Service) ChangeTagCategory(tagID, newCategoryID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return s.changeTagCategoryTx(tx, tagID, newCategoryID, "") })
}

func (s *Service) changeTagCategoryTx(tx *sql.Tx, tagID, newCategoryID int64, newName string) error {
	var currentCatID int64
	var name string
	switch err := tx.QueryRow(
		`SELECT category_id, name FROM tags WHERE id = ?`, tagID,
	).Scan(&currentCatID, &name); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrTagNotFound
	case err != nil:
		return err
	}
	if currentCatID == newCategoryID {
		return nil
	}
	if s.ratingCatID != 0 && newCategoryID == s.ratingCatID {
		return ErrRatingCategoryClosed
	}
	if s.ratingCatID != 0 && currentCatID == s.ratingCatID && IsCanonicalRating(name) {
		return ErrRatingTagImmutable
	}
	var catExists int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tag_categories WHERE id = ?`, newCategoryID,
	).Scan(&catExists); err != nil {
		return err
	}
	if catExists == 0 {
		return ErrCategoryNotFound
	}
	newName = cmp.Or(newName, name)
	existing, err := nameTaken(tx, newName, newCategoryID, tagID)
	if err != nil {
		return err
	}
	if existing != 0 && newName == name {
		res, err := tx.Exec(`DELETE FROM tags WHERE id = ? AND is_alias = 1 AND canonical_tag_id = ?`, existing, tagID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			existing = 0
		}
	}
	if existing != 0 {
		return &ErrCategoryCollision{Name: newName, ExistingID: existing}
	}
	_, err = tx.Exec(`UPDATE tags SET name = ?, category_id = ? WHERE id = ?`, newName, newCategoryID, tagID)
	return err
}
