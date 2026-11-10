package gallery

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func newCollectionsTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func insertImage(t *testing.T, database *db.DB, missing bool) int64 {
	t.Helper()
	m := 0
	if missing {
		m = 1
	}
	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, is_missing) VALUES (?,?,?,?,?)`,
		randSHA(t), "/x.png", "image", 1, m)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func randSHA(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func member(t *testing.T, database *db.DB, imageID int64, name string) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?,?,NULL)`, imageID, name)
}

func tagImage(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?,?)`, imageID, tagID)
}

func ratingTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

var questionableCeiling = RankCeiling{Rank: contract.RatingRank("questionable"), Active: true}

func seedCollectionsFixture(t *testing.T, database *db.DB) {
	t.Helper()
	explicitID := ratingTagID(t, database, "explicit")
	for i := 0; i < 3; i++ {
		member(t, database, insertImage(t, database, false), "alpha")
	}
	for i := 0; i < 2; i++ {
		member(t, database, insertImage(t, database, true), "beta")
	}
	member(t, database, insertImage(t, database, false), "gamma")
	member(t, database, insertImage(t, database, true), "gamma")
	for i := 0; i < 2; i++ {
		id := insertImage(t, database, false)
		tagImage(t, database, id, explicitID)
		member(t, database, id, "delta")
	}
	member(t, database, insertImage(t, database, false), "epsilon")
	ex := insertImage(t, database, false)
	tagImage(t, database, ex, explicitID)
	member(t, database, ex, "epsilon")
}

func TestCountCollections(t *testing.T) {
	database := newCollectionsTestDB(t)
	seedCollectionsFixture(t, database)

	cases := []struct {
		name    string
		filter  string
		ceiling RankCeiling
		want    int
	}{
		{"unfiltered drops the all-missing label", "", RankCeiling{}, 4},
		{"ceiling drops a fully-explicit label", "", questionableCeiling, 3},
		{"substring filter", "alph", RankCeiling{}, 1},
		{"no match", "zzz", RankCeiling{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CountCollections(database, tc.filter, tc.ceiling)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("CountCollections(%q, %v) = %d, want %d", tc.filter, tc.ceiling, got, tc.want)
			}
		})
	}
}

func TestListCollections(t *testing.T) {
	database := newCollectionsTestDB(t)
	seedCollectionsFixture(t, database)

	names := func(list []CollectionSummary) []string {
		out := make([]string, len(list))
		for i, c := range list {
			out[i] = c.Name
		}
		return out
	}
	countOf := func(list []CollectionSummary, name string) int {
		for _, c := range list {
			if c.Name == name {
				return c.Count
			}
		}
		return -1
	}

	list, err := ListCollections(database, "", "name", 60, 0, RankCeiling{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(list), []string{"alpha", "delta", "epsilon", "gamma"}; !slices.Equal(got, want) {
		t.Fatalf("name sort = %v, want %v", got, want)
	}
	if c := countOf(list, "gamma"); c != 1 {
		t.Fatalf("gamma visible count = %d, want 1 (one member missing)", c)
	}
	if c := countOf(list, "alpha"); c != 3 {
		t.Fatalf("alpha visible count = %d, want 3", c)
	}

	bySize, err := ListCollections(database, "", "size", 60, 0, RankCeiling{})
	if err != nil {
		t.Fatal(err)
	}
	if s := names(bySize); s[0] != "alpha" || s[len(s)-1] != "gamma" {
		t.Fatalf("size sort = %v, want alpha first and gamma last", s)
	}

	underCeiling, err := ListCollections(database, "", "name", 60, 0, questionableCeiling)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(underCeiling), []string{"alpha", "epsilon", "gamma"}; !slices.Equal(got, want) {
		t.Fatalf("ceiling list = %v, want %v", got, want)
	}
	if c := countOf(underCeiling, "epsilon"); c != 1 {
		t.Fatalf("epsilon under ceiling = %d, want 1", c)
	}
}

func homeOf(t *testing.T, database *db.DB, imageID int64) string {
	t.Helper()
	var s sql.NullString
	if err := database.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func homeOrderOf(t *testing.T, database *db.DB, imageID int64) (int, bool) {
	t.Helper()
	var o sql.NullInt64
	if err := database.Read.QueryRow(`SELECT series_order FROM images WHERE id = ?`, imageID).Scan(&o); err != nil {
		t.Fatal(err)
	}
	return int(o.Int64), o.Valid
}

func TestCollectionMembershipMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)

	o1, o5, o2 := 1, 5, 2
	if err := AddCollectionMembership(database, id, "A", &o1); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "A" {
		t.Fatalf("home after first add = %q, want A (adopted)", got)
	}
	if v, ok := homeOrderOf(t, database, id); !ok || v != 1 {
		t.Fatalf("series_order after first add = (%d,%v), want 1", v, ok)
	}

	if err := AddCollectionMembership(database, id, "A", &o5); err != nil {
		t.Fatal(err)
	}
	if v, ok := homeOrderOf(t, database, id); !ok || v != 5 {
		t.Fatalf("series_order after re-add = (%d,%v), want 5 (mirror tracks home order)", v, ok)
	}

	if err := AddCollectionMembership(database, id, "B", &o2); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "A" {
		t.Fatalf("home after adding extra = %q, want A (unchanged)", got)
	}
	assertMemberships(t, database, id, "A", "B")

	if err := RemoveCollectionMembership(database, id, "A"); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "B" {
		t.Fatalf("home after removing A = %q, want B (promoted)", got)
	}
	assertMemberships(t, database, id, "B")

	if err := RemoveCollectionMembership(database, id, "B"); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "" {
		t.Fatalf("home after removing last = %q, want empty (mirror cleared)", got)
	}
	assertMemberships(t, database, id)
}

func TestRenameCollectionMembership(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	o1, o3 := 1, 3

	if err := AddCollectionMembership(database, id, "A", &o1); err != nil {
		t.Fatal(err)
	}
	if err := AddCollectionMembership(database, id, "B", &o1); err != nil {
		t.Fatal(err)
	}
	if err := RenameCollectionMembership(database, id, "A", "C", &o3); err != nil {
		t.Fatal(err)
	}
	assertMemberships(t, database, id, "B", "C")
	if got := homeOf(t, database, id); got != "C" {
		t.Fatalf("home after renaming A = %q, want C", got)
	}
	var order sql.NullInt64
	if err := database.Read.QueryRow(`SELECT series_order FROM images WHERE id = ?`, id).Scan(&order); err != nil {
		t.Fatal(err)
	}
	if order.Int64 != 3 {
		t.Fatalf("series_order after renaming the home = %v, want 3", order)
	}

	if err := RenameCollectionMembership(database, id, "C", "B", &o3); err != nil {
		t.Fatal(err)
	}
	assertMemberships(t, database, id, "B")

	if err := RenameCollectionMembership(database, id, "B", "  ", nil); err == nil {
		t.Fatal("blank destination accepted")
	}
	assertMemberships(t, database, id, "B")
}

func TestCollectionWriters_AdoptTheExistingSpelling(t *testing.T) {
	database := newCollectionsTestDB(t)
	owner := insertImage(t, database, false)
	added := insertImage(t, database, false)
	renamed := insertImage(t, database, false)
	homed := insertImage(t, database, false)
	batched := insertImage(t, database, false)
	if err := AddCollectionMembership(database, owner, "Beta Set", nil); err != nil {
		t.Fatal(err)
	}
	if err := AddCollectionMembership(database, added, "beta set", nil); err != nil {
		t.Fatal(err)
	}
	if err := AddCollectionMembership(database, renamed, "Gamma", nil); err != nil {
		t.Fatal(err)
	}
	if err := RenameCollectionMembership(database, renamed, "Gamma", "BETA SET", nil); err != nil {
		t.Fatal(err)
	}
	if err := SetHomeCollection(database, homed, "beta SET", nil); err != nil {
		t.Fatal(err)
	}
	if err := AddCollectionToImages(database, []int64{batched}, "Beta set"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{added, renamed, homed, batched} {
		assertMemberships(t, database, id, "Beta Set")
		if got := homeOf(t, database, id); got != "Beta Set" {
			t.Errorf("image %d home = %q, want the existing spelling", id, got)
		}
	}
}

func assertMemberships(t *testing.T, database *db.DB, imageID int64, want ...string) {
	t.Helper()
	cols, err := CollectionsForImage(database, imageID)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(cols))
	for i, c := range cols {
		got[i] = c.Name
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("memberships = %v, want %v", got, want)
	}
}

func TestSetHomeCollection_PromoteExistingMembership(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	one, five := 1, 5
	if err := SetHomeCollection(database, id, "A", &one); err != nil {
		t.Fatal(err)
	}
	if err := AddCollectionMembership(database, id, "B", &five); err != nil {
		t.Fatal(err)
	}

	if err := SetHomeCollection(database, id, "B", nil); err != nil {
		t.Fatal(err)
	}
	assertMemberships(t, database, id, "A", "B")
	if got := homeOf(t, database, id); got != "B" {
		t.Fatalf("home after promote = %q, want B", got)
	}

	if err := SetHomeCollection(database, id, "C", nil); err != nil {
		t.Fatal(err)
	}
	assertMemberships(t, database, id, "A", "C")
	if got := homeOf(t, database, id); got != "C" {
		t.Fatalf("home after relabel = %q, want C", got)
	}
}

func TestSetCollectionFindRelations(t *testing.T) {
	database := newCollectionsTestDB(t)
	a := insertImage(t, database, false)
	member(t, database, a, "alpha")

	flagOf := func(name string) bool {
		t.Helper()
		list, err := ListCollections(database, "", "name", 60, 0, RankCeiling{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range list {
			if c.Name == name {
				return c.FindRelations
			}
		}
		t.Fatalf("collection %q not listed", name)
		return false
	}

	if flagOf("alpha") {
		t.Fatal("find-relations should default to disabled")
	}
	if err := SetCollectionFindRelations(database, "Alpha", true); err != nil {
		t.Fatal(err)
	}
	if !flagOf("alpha") {
		t.Fatal("enable did not stick across casings")
	}
	if err := SetCollectionFindRelations(database, "ALPHA", false); err != nil {
		t.Fatal(err)
	}
	if flagOf("alpha") {
		t.Fatal("disable did not delete the opt-in row")
	}
}

func TestReorderCollection(t *testing.T) {
	database := newCollectionsTestDB(t)
	a := insertImage(t, database, false)
	b := insertImage(t, database, false)
	c := insertImage(t, database, false)
	other := insertImage(t, database, false)
	for _, id := range []int64{a, b, c} {
		three := 3
		if err := AddCollectionMembership(database, id, "Ser", &three); err != nil {
			t.Fatal(err)
		}
	}
	member(t, database, other, "Elsewhere")

	if err := ReorderCollection(database, "ser", []int64{c, a, other}); err != nil {
		t.Fatal(err)
	}

	members, err := CollectionMembers(database, "Ser", RankCeiling{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]*int{}
	for _, m := range members {
		got[m.ID] = m.Order
	}
	if got[c] == nil || *got[c] != 1 {
		t.Errorf("position of c = %v, want 1", got[c])
	}
	if got[a] == nil || *got[a] != 2 {
		t.Errorf("position of a = %v, want 2", got[a])
	}
	if got[b] != nil {
		t.Errorf("unclicked member kept position %d, want unordered", *got[b])
	}
	if members[0].ID != c || members[1].ID != a || members[2].ID != b {
		t.Errorf("member order = %v, want [c a b]", members)
	}
	n := countRows(t, database, "image_collections WHERE image_id = ? AND name = 'Ser'", other)
	if n != 0 {
		t.Errorf("non-member gained a Ser membership")
	}
	if o, ok := homeOrderOf(t, database, other); ok {
		t.Errorf("non-member home order = %d, want NULL", o)
	}
	if o, ok := homeOrderOf(t, database, c); !ok || o != 1 {
		t.Errorf("mirror of c = (%d, %v), want (1, true)", o, ok)
	}
	if o, ok := homeOrderOf(t, database, a); !ok || o != 2 {
		t.Errorf("mirror of a = (%d, %v), want (2, true)", o, ok)
	}
	if _, ok := homeOrderOf(t, database, b); ok {
		t.Errorf("mirror of b should be cleared")
	}
}

func positionOf(t *testing.T, database *db.DB, imageID int64, name string) sql.NullInt64 {
	t.Helper()
	var pos sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT position FROM image_collections WHERE image_id = ? AND name = ?`, imageID, name).Scan(&pos); err != nil {
		t.Fatal(err)
	}
	return pos
}

func TestReorderCollection_KeepsTheOrderOfAMissingMember(t *testing.T) {
	database := newCollectionsTestDB(t)
	a := insertImage(t, database, false)
	b := insertImage(t, database, false)
	gone := insertImage(t, database, true)
	for i, id := range []int64{a, b, gone} {
		pos := i + 1
		if err := AddCollectionMembership(database, id, "Ser", &pos); err != nil {
			t.Fatal(err)
		}
	}

	hidden, err := CollectionHiddenOrderedIDs(database, "Ser", RankCeiling{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReorderCollection(database, "Ser", append([]int64{b, a}, hidden...)); err != nil {
		t.Fatal(err)
	}
	if pos := positionOf(t, database, gone, "Ser"); pos.Int64 != 3 {
		t.Errorf("missing member's position = %v, want 3 behind the clicked two", pos)
	}
}

func TestSortCollectionByFilename_OrdersAMissingMemberToo(t *testing.T) {
	database := newCollectionsTestDB(t)
	insertNamed := func(fname string, missing int) int64 {
		t.Helper()
		res, err := database.Write.Exec(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, is_missing) VALUES (?,?,?,?,?)`,
			randSHA(t), "/dir/"+fname, "image", 1, missing)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?, 'Ser', NULL)`, id)
		return id
	}
	b := insertNamed("b.png", 0)
	a := insertNamed("a.png", 1)

	if err := SortCollectionByFilename(database, "Ser"); err != nil {
		t.Fatal(err)
	}
	if pa, pb := positionOf(t, database, a, "Ser"), positionOf(t, database, b, "Ser"); pa.Int64 != 1 || pb.Int64 != 2 {
		t.Errorf("positions a=%v b=%v, want the missing a.png 1 and b.png 2", pa, pb)
	}
}

func TestSortCollectionByFilename(t *testing.T) {
	database := newCollectionsTestDB(t)
	insertNamed := func(fname string) int64 {
		t.Helper()
		res, err := database.Write.Exec(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, is_missing) VALUES (?,?,?,?,0)`,
			randSHA(t), "/dir/"+fname, "image", 1)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?, 'Ser', NULL)`, id)
		return id
	}
	i10 := insertNamed("10.png")
	i2 := insertNamed("2.png")
	i1 := insertNamed("1.png")

	if err := SortCollectionByFilename(database, "Ser"); err != nil {
		t.Fatal(err)
	}

	members, err := CollectionMembers(database, "Ser", RankCeiling{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int64, len(members))
	for i, m := range members {
		got[i] = m.ID
		if m.Order == nil || *m.Order != i+1 {
			t.Errorf("member %d position = %v, want %d", m.ID, m.Order, i+1)
		}
	}
	want := []int64{i1, i2, i10}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("filename order = %v, want %v (natural 1,2,10)", got, want)
	}
}

func TestCollectionCountsTriggers(t *testing.T) {
	database := newCollectionsTestDB(t)
	count := func(name string) int {
		t.Helper()
		var n int
		err := database.Read.QueryRow(
			`SELECT visible_count FROM collection_counts WHERE name = ?`, name).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	a := insertImage(t, database, false)
	b := insertImage(t, database, false)
	m := insertImage(t, database, true)
	member(t, database, a, "Ser")
	member(t, database, b, "Ser")
	member(t, database, m, "Ser")
	member(t, database, a, "Other")
	if got := count("Ser"); got != 2 {
		t.Errorf("Ser after inserts = %d, want 2 (missing member uncounted)", got)
	}
	if got := count("Other"); got != 1 {
		t.Errorf("Other after inserts = %d, want 1", got)
	}

	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, b)
	if got := count("Ser"); got != 1 {
		t.Errorf("Ser after hiding b = %d, want 1", got)
	}
	mustExec(t, database.Write, `UPDATE images SET is_missing = 0 WHERE id = ?`, b)
	if got := count("Ser"); got != 2 {
		t.Errorf("Ser after unhiding b = %d, want 2", got)
	}

	mustExec(t, database.Write, `DELETE FROM images WHERE id = ?`, a)
	if got := count("Ser"); got != 1 {
		t.Errorf("Ser after deleting a = %d, want 1", got)
	}
	if got := count("Other"); got != 0 {
		t.Errorf("Other after deleting a = %d, want 0", got)
	}

	mustExec(t, database.Write, `UPDATE image_collections SET name = 'Renamed' WHERE name = 'Ser'`)
	if got := count("Ser"); got != 0 {
		t.Errorf("Ser after relabel = %d, want 0", got)
	}
	if got := count("Renamed"); got != 1 {
		t.Errorf("Renamed after relabel = %d, want 1", got)
	}

	if err := RemoveCollectionMembership(database, b, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if got := count("Renamed"); got != 0 {
		t.Errorf("Renamed after removal = %d, want 0", got)
	}
}

func TestCollectionCBZMembers(t *testing.T) {
	database := newCollectionsTestDB(t)
	insert := func(fname, fileType string, missing bool, pos any) {
		t.Helper()
		m := 0
		if missing {
			m = 1
		}
		res, err := database.Write.Exec(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, is_missing) VALUES (?,?,?,?,?)`,
			randSHA(t), "/dir/"+fname, fileType, 1, m)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?, 'Ser', ?)`, id, pos)
	}
	insert("z.png", "png", false, 2)
	insert("a.png", "png", false, 1)
	insert("10.jpg", "jpeg", false, nil)
	insert("2.jpg", "jpeg", false, nil)
	insert("1.jpg", "jpeg", false, nil)
	insert("gone.png", "png", true, nil)

	members, err := CollectionCBZMembers(database, "Ser")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range members {
		got = append(got, m.filename)
	}
	if want := []string{"a.png", "z.png", "1.jpg", "2.jpg", "10.jpg"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if members[0].FileType != "png" || members[2].FileType != "jpeg" {
		t.Errorf("file types = %q, %q; want png, jpeg", members[0].FileType, members[2].FileType)
	}
}

func orderedMembers(t *testing.T, database *db.DB, name string, n int) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = insertImage(t, database, false)
		pos := 2 * (i + 1)
		if err := AddCollectionMembership(database, ids[i], name, &pos); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func soleNav(t *testing.T, database *db.DB, imageID int64, ceiling RankCeiling) CollectionNav {
	t.Helper()
	navs, err := CollectionNavs(database, imageID, ceiling, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(navs) != 1 {
		t.Fatalf("navs = %d, want 1", len(navs))
	}
	return navs[0]
}

func windowIDs(nav CollectionNav) []int64 {
	ids := make([]int64, len(nav.Window))
	for i, s := range nav.Window {
		ids[i] = s.ID
	}
	return ids
}

func neighbours(nav CollectionNav, self int64) (prev, next int64) {
	at := slices.Index(windowIDs(nav), self)
	if at > 0 {
		prev = nav.Window[at-1].ID
	}
	if at+1 < len(nav.Window) {
		next = nav.Window[at+1].ID
	}
	return prev, next
}

func TestCollectionNavs_WindowFollowsTheImage(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids := orderedMembers(t, database, "Saga", 10)
	cases := []struct {
		at         int
		window     []int64
		prev, next int64
	}{
		{0, ids[0:6], 0, ids[1]},
		{1, ids[0:6], ids[0], ids[2]},
		{4, ids[2:8], ids[3], ids[5]},
		{8, ids[4:10], ids[7], ids[9]},
		{9, ids[4:10], ids[8], 0},
	}
	for _, c := range cases {
		nav := soleNav(t, database, ids[c.at], RankCeiling{})
		if nav.Rank != c.at+1 || nav.Total != 10 {
			t.Errorf("at %d: rank %d / %d, want %d / 10", c.at, nav.Rank, nav.Total, c.at+1)
		}
		if got := windowIDs(nav); !slices.Equal(got, c.window) {
			t.Errorf("at %d: window %v, want %v", c.at, got, c.window)
		}
		if prev, next := neighbours(nav, ids[c.at]); prev != c.prev || next != c.next {
			t.Errorf("at %d: prev %d next %d, want %d %d", c.at, prev, next, c.prev, c.next)
		}
	}
	if nav := soleNav(t, database, ids[4], RankCeiling{}); nav.Window[2].Order == nil || *nav.Window[2].Order != 10 {
		t.Errorf("window keeps the stored position: got %v, want 10", nav.Window[2].Order)
	}
}

func TestCollectionNavs_UnnumberedMembersComeLast(t *testing.T) {
	database := newCollectionsTestDB(t)
	a := insertImage(t, database, false)
	b := insertImage(t, database, false)
	c := insertImage(t, database, false)
	d := insertImage(t, database, false)
	five, one := 5, 1
	for _, m := range []struct {
		id  int64
		pos *int
	}{{a, nil}, {b, &five}, {c, nil}, {d, &one}} {
		if err := AddCollectionMembership(database, m.id, "Saga", m.pos); err != nil {
			t.Fatal(err)
		}
	}
	nav := soleNav(t, database, a, RankCeiling{})
	if nav.Rank != 3 {
		t.Errorf("rank %d, want 3", nav.Rank)
	}
	if got := windowIDs(nav); !slices.Equal(got, []int64{d, b, a, c}) {
		t.Errorf("window %v, want [d b a c]", got)
	}
}

func TestCollectionNavs_SkipsMissingAndOverCeilingMembers(t *testing.T) {
	database := newCollectionsTestDB(t)
	ids := orderedMembers(t, database, "Saga", 5)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, ids[1])
	tagImage(t, database, ids[3], ratingTagID(t, database, "explicit"))

	nav := soleNav(t, database, ids[2], RankCeiling{})
	if got := windowIDs(nav); nav.Rank != 2 || nav.Total != 4 || !slices.Equal(got, []int64{ids[0], ids[2], ids[3], ids[4]}) {
		t.Errorf("no ceiling: rank %d / %d window %v", nav.Rank, nav.Total, got)
	}

	nav = soleNav(t, database, ids[2], questionableCeiling)
	if nav.Rank != 2 || nav.Total != 3 {
		t.Errorf("ceiling: rank %d / %d, want 2 / 3", nav.Rank, nav.Total)
	}
	if got := windowIDs(nav); !slices.Equal(got, []int64{ids[0], ids[2], ids[4]}) {
		t.Errorf("ceiling: window %v", got)
	}

	nav = soleNav(t, database, ids[3], questionableCeiling)
	if nav.Rank != 3 || nav.Total != 4 {
		t.Errorf("an image above the ceiling still counts itself: rank %d / %d, want 3 / 4", nav.Rank, nav.Total)
	}
}

func TestCollectionNavs_OneNavPerMembershipInMembershipOrder(t *testing.T) {
	database := newCollectionsTestDB(t)
	self := insertImage(t, database, false)
	other := insertImage(t, database, false)
	one, two := 1, 2
	for _, m := range []struct {
		id   int64
		name string
		pos  *int
	}{{self, "Alpha", &two}, {self, "Beta", &one}, {other, "Alpha", &one}} {
		if err := AddCollectionMembership(database, m.id, m.name, m.pos); err != nil {
			t.Fatal(err)
		}
	}
	navs, err := CollectionNavs(database, self, RankCeiling{}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(navs) != 2 || navs[0].Name != "Beta" || navs[1].Name != "Alpha" {
		t.Fatalf("navs = %+v, want Beta then Alpha", navs)
	}
	if prev, _ := neighbours(navs[1], self); navs[0].Total != 1 || navs[1].Rank != 2 || prev != other {
		t.Errorf("Beta %d / %d, Alpha %d / %d prev %d", navs[0].Rank, navs[0].Total, navs[1].Rank, navs[1].Total, prev)
	}
}

const largeLibraryStat1 = `image_collections|idx_image_collections_name|751140 4609 1 1
image_collections|idx_image_collections_reading|751140 4609 4609 1582 1
image_collections|sqlite_autoindex_image_collections_1|751140 1 1
images|idx_images_missing|1001364 401
images|idx_images_rating_rank_visible|1001364 401
images|sqlite_autoindex_images_1|1001364 1`

func injectStat1(t *testing.T, database *db.DB, stat1 string) {
	t.Helper()
	mustExec(t, database.Write, `ANALYZE; DELETE FROM sqlite_stat1; DELETE FROM sqlite_stat4`)
	for _, line := range strings.Split(stat1, "\n") {
		f := strings.Split(line, "|")
		mustExec(t, database.Write, `INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES (?, ?, ?)`, f[0], f[1], f[2])
	}
}

func TestCollectionMemberWalksDriveFromTheLabel(t *testing.T) {
	path := t.TempDir() + "/test.db"
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	var first int64
	for i := range 20 {
		id := insertImage(t, database, i%5 == 0)
		mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name, position) VALUES (?, 'naruto', ?)`, id, i+1)
		if first == 0 {
			first = id
		}
	}
	injectStat1(t, database, largeLibraryStat1)
	planned, rec := recordPlans(t, database, path)

	for _, ceiling := range []RankCeiling{{}, questionableCeiling} {
		if _, err := CountCollections(planned, "", ceiling); err != nil {
			t.Fatal(err)
		}
		if _, err := CollectionSamples(planned, []string{"naruto"}, 4, ceiling); err != nil {
			t.Fatal(err)
		}
		if _, err := CollectionNavs(planned, first+10, ceiling, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := CollectionHiddenOrderedIDs(planned, "naruto", ceiling); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CollectionCBZMembers(planned, "naruto"); err != nil {
		t.Fatal(err)
	}
	if err := SortCollectionByFilename(planned, "naruto"); err != nil {
		t.Fatal(err)
	}

	walks := 0
	for query, plan := range rec.plans {
		if !strings.Contains(query, "JOIN images i") {
			continue
		}
		walks++
		c, i := strings.Index(plan, "SEARCH c "), strings.Index(plan, "SEARCH i ")
		if c < 0 || i < c || strings.Contains(plan, "idx_images_missing") {
			t.Errorf("not driven from the label:\n%s\nplan:\n%s", query, plan)
		}
		if strings.Contains(query, "position IS NULL") && strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("reading-order walk sorts:\n%s\nplan:\n%s", query, plan)
		}
	}
	if walks < 9 {
		t.Errorf("%d member walks recorded, want the count, sample, nav, hidden-order, export and sort walks", walks)
	}
}
