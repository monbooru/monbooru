package gallery

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// images.series and series_order mirror one "home" membership so the
// order sort and the adjacency cursor can ride scalar columns:
// series != '' iff the image has a membership, and then names one of them.

const membershipOrder = `position IS NULL, position, name`

func CollectionsForImage(database *db.DB, imageID int64) ([]models.Collection, error) {
	return db.QueryAll(database.Read, db.ScanRow(func(c *models.Collection) []any {
		return []any{&c.Name, &c.Order}
	}), `SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY `+membershipOrder, imageID)
}

func AddCollectionMembership(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, err := addMembershipTx(tx, imageID, name, order)
		return err
	})
}

func ExistingLabelTx(tx *sql.Tx, name string) (string, error) {
	var existing sql.NullString
	if err := tx.QueryRow(`SELECT MIN(name) FROM image_collections WHERE name = ?`, name).Scan(&existing); err != nil {
		return "", err
	}
	if existing.Valid {
		return existing.String, nil
	}
	return name, nil
}

func addMembershipTx(tx *sql.Tx, imageID int64, name string, order *int) (string, error) {
	name, err := ExistingLabelTx(tx, name)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, order); err != nil {
		return "", err
	}
	home, err := homeName(tx, imageID)
	if err != nil {
		return "", err
	}
	switch {
	case home == "":
		_, err = tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
			name, order, imageID)
	case strings.EqualFold(home, name):
		_, err = tx.Exec(`UPDATE images SET series_order = ? WHERE id = ?`,
			order, imageID)
	}
	return name, err
}

func RemoveCollectionMembership(database *db.DB, imageID int64, name string) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return removeMembershipTx(tx, imageID, name)
	})
}

func removeMembershipTx(tx *sql.Tx, imageID int64, name string) error {
	if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
		imageID, name); err != nil {
		return err
	}
	return rebindHomeTx(tx, imageID, name)
}

func RenameCollectionMembership(database *db.DB, imageID int64, prev, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		home, err := homeName(tx, imageID)
		if err != nil {
			return err
		}
		if err := removeMembershipTx(tx, imageID, prev); err != nil {
			return err
		}
		name, err := addMembershipTx(tx, imageID, name, order)
		if err != nil {
			return err
		}
		if !strings.EqualFold(home, prev) {
			return nil
		}
		_, err = tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`, name, order, imageID)
		return err
	})
}

// SetHomeCollection promotes a name the image already holds and keeps the
// old home as a membership; a new name, or "", drops the old home.
func SetHomeCollection(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		oldHome, err := homeName(tx, imageID)
		if err != nil {
			return err
		}
		if name != "" {
			if name, err = ExistingLabelTx(tx, name); err != nil {
				return err
			}
		}
		relabel := oldHome != "" && !strings.EqualFold(oldHome, name)
		if relabel && name != "" {
			var x int
			switch err := tx.QueryRow(
				`SELECT 1 FROM image_collections WHERE image_id = ? AND name = ?`, imageID, name).Scan(&x); {
			case err == nil:
				relabel = false
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		if relabel {
			if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
				imageID, oldHome); err != nil {
				return err
			}
		}
		if name == "" {
			return rebindHomeTx(tx, imageID, oldHome)
		}
		if _, err := tx.Exec(
			`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
			 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
			imageID, name, order); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
			name, order, imageID)
		return err
	})
}

func homeName(tx *sql.Tx, imageID int64) (string, error) {
	var home sql.NullString
	if err := tx.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&home); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return home.String, nil
}

// Runs after changedName's row is deleted, or it could promote that row again.
func rebindHomeTx(tx *sql.Tx, imageID int64, changedName string) error {
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	if home == "" || !strings.EqualFold(home, changedName) {
		return nil
	}
	var name sql.NullString
	var pos sql.NullInt64
	err = tx.QueryRow(
		`SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY `+membershipOrder+` LIMIT 1`, imageID).Scan(&name, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET series = '', series_order = NULL WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	_, e := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name.String, pos, imageID)
	return e
}

type CollectionSummary struct {
	Name          string
	Count         int
	FindRelations bool
	Samples       []CollectionSample
}

type CollectionSample struct {
	ID       int64
	Order    *int
	Filename string
}

func collectionFilterWhere(col, nameFilter string) (string, []any) {
	if nameFilter == "" {
		return "", nil
	}
	return ` AND ` + col + ` LIKE ? ESCAPE '\'`, []any{"%" + db.EscapeLike(nameFilter) + "%"}
}

func ListCollections(database *db.DB, nameFilter, sort string, limit, offset int, ceiling RankCeiling) ([]CollectionSummary, error) {
	where, filterArgs := collectionFilterWhere("c.name", nameFilter)
	var query string
	var args []any
	if !ceiling.Active {
		// The trigger-maintained counts read one row per label instead of
		// walking every membership.
		orderBy := "c.visible_count DESC, c.name ASC"
		if sort == "name" {
			orderBy = "c.name ASC"
		}
		query = `SELECT c.name, c.visible_count,
		        EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = c.name)
		 FROM collection_counts c
		 WHERE c.visible_count > 0` + where + `
		 ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
		args = append(filterArgs, limit, offset)
	} else {
		// The stored counts are ceiling-blind. EXISTS rather than a join
		// lets the GROUP BY stream off idx_image_collections_name instead
		// of temp-sorting every member.
		under, underArgs := ceiling.and("i")
		orderBy := "cnt DESC, c.name ASC"
		if sort == "name" {
			orderBy = "c.name ASC"
		}
		query = `SELECT c.name, COUNT(*) cnt,
		        EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = c.name)
		 FROM image_collections c
		 WHERE EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0` + under + `)` + where + `
		 GROUP BY c.name ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
		args = append(append(underArgs, filterArgs...), limit, offset)
	}
	return db.QueryAll(database.Read, db.ScanRow(func(c *CollectionSummary) []any {
		return []any{&c.Name, &c.Count, &c.FindRelations}
	}), query, args...)
}

func SetCollectionFindRelations(database *db.DB, name string, enabled bool) error {
	if enabled {
		_, err := database.Write.Exec(
			`INSERT OR IGNORE INTO collection_find_relations (name) VALUES (?)`, name)
		return err
	}
	_, err := database.Write.Exec(
		`DELETE FROM collection_find_relations WHERE name = ?`, name)
	return err
}

func CountCollections(database *db.DB, nameFilter string, ceiling RankCeiling) (int, error) {
	var n int
	if !ceiling.Active {
		where, args := collectionFilterWhere("name", nameFilter)
		err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM collection_counts WHERE visible_count > 0`+where, args...).Scan(&n)
		return n, err
	}
	// The per-label EXISTS stops at the first visible member, so the cost
	// tracks labels, not memberships.
	under, args := ceiling.and("i")
	where, filterArgs := collectionFilterWhere("d.name", nameFilter)
	args = append(args, filterArgs...)
	err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT name FROM image_collections) d
		 WHERE EXISTS (SELECT 1 FROM image_collections c CROSS JOIN images i ON i.id = c.image_id
		   WHERE c.name = d.name AND i.is_missing = 0`+under+`)`+where, args...).Scan(&n)
	return n, err
}

// CollectionSamples keys by the lower-cased label: NOCASE labels may be
// stored in several cases. A LIMITed query per name stops each index walk
// early, where a ROW_NUMBER window would rank every member.
func CollectionSamples(database *db.DB, names []string, per int, ceiling RankCeiling) (map[string][]CollectionSample, error) {
	out := make(map[string][]CollectionSample, len(names))
	if len(names) == 0 || per <= 0 {
		return out, nil
	}
	for _, name := range names {
		samples, err := CollectionMembers(database, name, ceiling, per, 0)
		if err != nil {
			return out, err
		}
		key := strings.ToLower(name)
		if len(out[key]) == 0 {
			out[key] = samples
		}
	}
	return out, nil
}

// CollectionMembers' ORDER BY matches idx_image_collections_reading, so
// the LIMIT stops the scan early instead of sorting the whole label. The
// member walks CROSS JOIN so stats can't start them at idx_images_missing.
func CollectionMembers(database *db.DB, name string, ceiling RankCeiling, limit, offset int) ([]CollectionSample, error) {
	under, args := ceiling.and("i")
	args = append([]any{name}, args...)
	args = append(args, limit, offset)
	return db.QueryAll(database.Read, db.ScanRow(func(sample *CollectionSample) []any {
		return []any{&sample.ID, &sample.Order, &sample.Filename}
	}),
		`SELECT c.image_id, c.position, basename(i.canonical_path)
		 FROM image_collections c CROSS JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0`+under+`
		 ORDER BY c.position IS NULL, c.position, c.image_id LIMIT ? OFFSET ?`, args...)
}

// CollectionNav places an image in one of its collections. Rank is
// 1-based among the visible members, in reading order.
type CollectionNav struct {
	Name        string
	Rank, Total int
	Window      []CollectionSample
}

// The image counts even when missing or above the ceiling, so it always
// has a rank.
func CollectionNavs(database *db.DB, imageID int64, ceiling RankCeiling, size int) ([]CollectionNav, error) {
	memberships, err := CollectionsForImage(database, imageID)
	if err != nil {
		return nil, err
	}
	navs := make([]CollectionNav, 0, len(memberships))
	for _, m := range memberships {
		nav, err := collectionNav(database, imageID, m, ceiling, size)
		if err != nil {
			return nil, err
		}
		navs = append(navs, nav)
	}
	return navs, nil
}

// The key compares unnumbered members as position 0 behind the IS NULL
// flag: a NULL inside a row value would make the comparison NULL.
const readingKey = `(c.position IS NULL, COALESCE(c.position, 0), c.image_id)`

func collectionNav(database *db.DB, imageID int64, m models.Collection, ceiling RankCeiling, size int) (CollectionNav, error) {
	under, underArgs := ceiling.and("i")
	from := ` FROM image_collections c CROSS JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND (c.image_id = ? OR (i.is_missing = 0` + under + `))`
	fromArgs := append([]any{m.Name, imageID}, underArgs...)
	selfNull, selfPos := 1, 0
	if m.Order != nil {
		selfNull, selfPos = 0, *m.Order
	}
	selfKey := []any{selfNull, selfPos, imageID}

	nav := CollectionNav{Name: m.Name}
	var before int
	if err := database.Read.QueryRow(`SELECT COUNT(*), COALESCE(SUM(`+readingKey+` < (?, ?, ?)), 0)`+from,
		append(append([]any{}, selfKey...), fromArgs...)...).Scan(&nav.Total, &before); err != nil {
		return nav, err
	}
	nav.Rank = before + 1

	side := func(cmp, dir string) ([]CollectionSample, error) {
		args := append(append(append([]any{}, fromArgs...), selfKey...), size-1)
		return db.QueryAll(database.Read, db.ScanRow(func(sample *CollectionSample) []any {
			return []any{&sample.ID, &sample.Order}
		}), `SELECT c.image_id, c.position`+from+` AND `+readingKey+` `+cmp+` (?, ?, ?)
			 ORDER BY c.position IS NULL `+dir+`, c.position `+dir+`, c.image_id `+dir+` LIMIT ?`, args...)
	}
	prev, err := side("<", "DESC")
	if err != nil {
		return nav, err
	}
	next, err := side(">", "ASC")
	if err != nil {
		return nav, err
	}
	others := size - 1
	nb := min(len(prev), others/2)
	na := min(len(next), others-nb)
	nb = min(len(prev), others-na)
	nav.Window = make([]CollectionSample, 0, nb+1+na)
	for i := nb - 1; i >= 0; i-- {
		nav.Window = append(nav.Window, prev[i])
	}
	nav.Window = append(nav.Window, CollectionSample{ID: imageID, Order: m.Order})
	nav.Window = append(nav.Window, next[:na]...)
	return nav, nil
}

// ReorderCollection leaves the members missing from ids unordered. Past
// one chunk the writes are split, so a large reorder is not atomic.
func ReorderCollection(database *db.DB, name string, ids []int64) error {
	const chunkSize = 500
	const clearAll = `UPDATE image_collections SET position = NULL WHERE name = ?`
	const setPos = `UPDATE image_collections SET position = ? WHERE image_id = ? AND name = ?`
	const resync = `UPDATE images SET series_order =
		   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
		 WHERE series = ? COLLATE NOCASE`

	if len(ids) <= chunkSize {
		return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			if _, err := tx.Exec(clearAll, name); err != nil {
				return err
			}
			for i, id := range ids {
				if _, err := tx.Exec(setPos, i+1, id, name); err != nil {
					return err
				}
			}
			_, err := tx.Exec(resync, name, name)
			return err
		})
	}

	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, e := tx.Exec(clearAll, name)
		return e
	}); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += chunkSize {
		lo, hi := start, min(start+chunkSize, len(ids))
		if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			for i := lo; i < hi; i++ {
				if _, err := tx.Exec(setPos, i+1, ids[i], name); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, e := tx.Exec(resync, name, name)
		return e
	})
}

func SortCollectionByFilename(database *db.DB, name string) error {
	type member struct {
		id       int64
		filename string
	}
	members, err := db.QueryAll(database.Read, db.ScanRow(func(m *member) []any {
		return []any{&m.id, &m.filename}
	}), `SELECT c.image_id, basename(i.canonical_path)
		 FROM image_collections c CROSS JOIN images i ON i.id = c.image_id
		 WHERE c.name = ?
		 ORDER BY c.image_id`, name)
	if err != nil {
		return err
	}
	sort.SliceStable(members, func(i, j int) bool {
		return NaturalLess(strings.ToLower(members[i].filename), strings.ToLower(members[j].filename))
	})
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.id
	}
	return ReorderCollection(database, name, ids)
}

func CollectionCeilingHidden(database *db.DB, name string, ceiling RankCeiling) (int, error) {
	if !ceiling.Active {
		return 0, nil
	}
	var blind int
	err := database.Read.QueryRow(
		`SELECT COALESCE(visible_count, 0) FROM collection_counts WHERE name = ?`, name).Scan(&blind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	under, underArgs := ceiling.and("i")
	var filtered int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_collections c
		 WHERE c.name = ? AND EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0`+under+`)`,
		append([]any{name}, underArgs...)...).Scan(&filtered); err != nil {
		return 0, err
	}
	if blind < filtered {
		return 0, nil
	}
	return blind - filtered, nil
}

// CollectionHiddenOrderedIDs, the positioned members the dialog cannot
// show (missing, or above the ceiling), must be appended to a reorder's
// ids, or ReorderCollection clears positions the operator never saw.
func CollectionHiddenOrderedIDs(database *db.DB, name string, ceiling RankCeiling) ([]int64, error) {
	overCeiling, args := "", []any{name}
	if ceiling.Active {
		overCeiling, args = " OR i.rating_rank > ?", append(args, ceiling.Rank)
	}
	return db.QueryIDs(database.Read,
		`SELECT c.image_id FROM image_collections c CROSS JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND c.position IS NOT NULL AND (i.is_missing = 1`+overCeiling+`)
		 ORDER BY c.position, c.image_id`, args...)
}

// CollectionMemberIDs includes missing rows, so a rename or dissolve
// reaches the whole collection.
func CollectionMemberIDs(database *db.DB, name string) ([]int64, error) {
	return db.QueryIDs(database.Read,
		`SELECT image_id FROM image_collections WHERE name = ? COLLATE NOCASE`, name)
}

func CollectionCBZMembers(database *db.DB, name string) ([]CBZMember, error) {
	type memberRow struct {
		CBZMember
		pos sql.NullInt64
	}
	rows, err := db.QueryAll(database.Read, db.ScanRow(func(r *memberRow) []any {
		return []any{&r.Path, &r.FileType, &r.filename, &r.pos}
	}), `SELECT i.canonical_path, i.file_type, basename(i.canonical_path), c.position
		 FROM image_collections c CROSS JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0
		 ORDER BY c.position IS NULL, c.position, c.image_id`, name)
	if err != nil {
		return nil, err
	}
	var out, unordered []CBZMember
	for _, r := range rows {
		if r.pos.Valid {
			out = append(out, r.CBZMember)
		} else {
			unordered = append(unordered, r.CBZMember)
		}
	}
	sort.SliceStable(unordered, func(i, j int) bool {
		return NaturalLess(strings.ToLower(unordered[i].filename), strings.ToLower(unordered[j].filename))
	})
	return append(out, unordered...), nil
}

func AddCollectionToImages(database *db.DB, ids []int64, name string) error {
	placeholders, args := db.InPlaceholders(ids)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		name, err := ExistingLabelTx(tx, name)
		if err != nil {
			return err
		}
		labelArgs := append([]any{name}, args...)
		if _, err := tx.Exec(
			`INSERT INTO image_collections (image_id, name, position)
			 SELECT id, ?, NULL FROM images WHERE id IN (`+placeholders+`)
			 ON CONFLICT(image_id, name) DO NOTHING`,
			labelArgs...,
		); err != nil {
			return err
		}
		_, err = tx.Exec(
			`UPDATE images SET series = ?, series_order = NULL
			 WHERE series = '' AND id IN (`+placeholders+`)`,
			labelArgs...,
		)
		return err
	})
}

func RemoveCollectionFromImages(database *db.DB, ids []int64, name string) error {
	placeholders, args := db.InPlaceholders(ids)
	labelArgs := append([]any{name}, args...)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`DELETE FROM image_collections WHERE name = ? AND image_id IN (`+placeholders+`)`,
			labelArgs...,
		); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET
			   series = COALESCE((SELECT name FROM image_collections c WHERE c.image_id = images.id
			                      ORDER BY `+membershipOrder+` LIMIT 1), ''),
			   series_order = (SELECT position FROM image_collections c WHERE c.image_id = images.id
			                   ORDER BY `+membershipOrder+` LIMIT 1)
			 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			labelArgs...,
		)
		return err
	})
}

// RenameCollectionForImages, when merging, first drops oldName where the
// image already holds newName, so the rename cannot collide and the
// existing position wins the home resync.
func RenameCollectionForImages(database *db.DB, ids []int64, oldName, newName string, posOffset int, merging bool) error {
	placeholders, args := db.InPlaceholders(ids)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if merging {
			if _, err := tx.Exec(
				`DELETE FROM image_collections
				 WHERE name = ? AND image_id IN (`+placeholders+`)
				   AND image_id IN (SELECT image_id FROM image_collections WHERE name = ?)`,
				append(append([]any{oldName}, args...), newName)...,
			); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`UPDATE image_collections SET name = ?, position = position + ?
			 WHERE name = ? AND image_id IN (`+placeholders+`)`,
			append([]any{newName, posOffset, oldName}, args...)...,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ? WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, oldName}, args...)...,
		); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET series_order =
			   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
			 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, newName}, args...)...,
		)
		return err
	})
}
