package search

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func seedWalkFixture(t *testing.T, database *db.DB) {
	t.Helper()
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	tag := func(name, category string) int64 {
		var id int64
		if err := tx.QueryRow(
			`INSERT INTO tags (name, category_id) SELECT ?, id FROM tag_categories WHERE name = ? RETURNING id`, name, category,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, c, x := tag("walk_a", "general"), tag("walk_b", "general"), tag("walk_c", "general"), tag("walk_x", "character")
	if _, err := tx.Exec(`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) SELECT 'walk_d', category_id, 1, id FROM tags WHERE id = ?`, a); err != nil {
		t.Fatal(err)
	}
	var general, explicit int64
	for name, id := range map[string]*int64{"general": &general, "explicit": &explicit} {
		if err := tx.QueryRow(`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating' AND t.name = ?`, name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 90; i++ {
		var imgID int64
		if err := tx.QueryRow(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, width, is_missing) VALUES (?, ?, 'png', 100, ?, ?) RETURNING id`,
			fmt.Sprintf("walk%03d", i), fmt.Sprintf("/gallery/walk%03d.png", i), 10+i%20, i%17 == 0,
		).Scan(&imgID); err != nil {
			t.Fatal(err)
		}
		rating := general
		if i%6 == 0 {
			rating = explicit
		}
		on := []int64{rating}
		for _, s := range []struct {
			every int
			tag   int64
		}{{2, a}, {3, b}, {5, c}, {4, x}} {
			if i%s.every == 0 {
				on = append(on, s.tag)
			}
		}
		for _, tagID := range on {
			if _, err := tx.Exec(`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, tagID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func sqlOrderIDs(t *testing.T, database *db.DB, q Query) []int64 {
	t.Helper()
	where, args, _, _ := visibleWhere(testSource(database), q.Expr, nil)
	ids, err := db.QueryIDsContext(context.Background(), database.Read,
		"SELECT i.id FROM images i WHERE "+where+" "+buildOrder(q.Sort, q.Order, 0), args...)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestExecute_TheTagWalkPagesAsTheSortIndexDoes(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedWalkFixture(t, database)
	queries := map[string]Expr{
		"one tag":          Parse("walk_a"),
		"two tags":         Parse("walk_a walk_b"),
		"three tags":       Parse("walk_a walk_b walk_c"),
		"a negation":       Parse("walk_a -walk_b"),
		"an alias":         Parse("walk_d walk_c"),
		"a category tag":   Parse("character:walk_x walk_a"),
		"a column filter":  Parse("walk_b width:>20"),
		"a rating keyword": Parse("walk_c rating:explicit"),
		"the ceiling":      ApplyCeiling(Parse("walk_a"), "general"),
		"no such tag":      Parse("walk_a walk_none"),
		"an OR":            Parse("walk_b OR walk_c"),
		"a wildcard":       Parse("walk_* -walk_a"),
		"a suffix":         Parse("*_c walk_b"),
		"a category":       Parse("cat:character walk_b"),
		"a tag count":      Parse("walk_a tagcount:character>=1"),
		"a zero tag count": Parse("walk_b tagcount:character<1"),
	}
	const limit = 4
	for name, expr := range queries {
		for _, order := range []string{"desc", "asc"} {
			t.Run(name+" "+order, func(t *testing.T) {
				q := Query{Expr: expr, Sort: "newest", Order: order, Page: 1, Limit: limit, SkipCount: true}
				if _, ok := tagWalkLegs(testSource(database), q); !ok {
					t.Fatal("not a walk")
				}
				full := sqlOrderIDs(t, database, q)
				page := func(lo int) []int64 { return full[min(lo, len(full)):min(lo+limit, len(full))] }
				if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, page(0)) && len(got)+len(page(0)) > 0 {
					t.Errorf("page 1 = %v, want %v", got, page(0))
				}
				if len(full) < 3*limit {
					return
				}
				q.Page, q.After = 2, full[limit-1]
				if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, page(limit)) {
					t.Errorf("after=%d: %v, want %v", q.After, got, page(limit))
				}
				q.After, q.Before = 0, full[2*limit]
				if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, page(limit)) {
					t.Errorf("before=%d: %v, want %v", q.Before, got, page(limit))
				}
			})
		}
	}
}

func TestTagWalkLegs_LeavesOtherShapesToSQL(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedWalkFixture(t, database)
	for _, q := range []Query{
		{Expr: Parse("walk_a"), Sort: "filesize"},
		{Expr: Parse("walk_a"), Sort: "random", RandomSeed: 3},
		{Expr: Parse("walk_a missing:true"), Sort: "newest"},
		{Expr: Parse("width:>5"), Sort: "newest"},
		{Expr: Parse("-walk_a"), Sort: "newest"},
		{Expr: Parse("walk_a OR -walk_b"), Sort: "newest"},
	} {
		if _, ok := tagWalkLegs(testSource(database), q); ok {
			t.Errorf("%s under %s walks", q.Expr, q.Sort)
		}
	}
}

func TestWalkPage_GivesUpOnAWalkTheRestOfTheQueryKeepsRejecting(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedWalkFixture(t, database)
	old := walkMaxCandidates
	walkMaxCandidates = 10
	t.Cleanup(func() { walkMaxCandidates = old })
	q := Query{Expr: Parse("walk_a width:>100"), Sort: "newest", Page: 1, Limit: 4, SkipCount: true}
	legs, _ := tagWalkLegs(testSource(database), q)
	where, args, _, _ := visibleWhere(testSource(database), q.Expr, nil)
	if _, ok, err := walkPage(context.Background(), database, newTagWalk(database, legs, false, walkStart(0, false)), where, args, 4); err != nil || ok {
		t.Errorf("ok=%v err=%v, want a walk that gives up", ok, err)
	}
	if res := mustExecute(t, database, q); len(res.Results) != 0 {
		t.Errorf("the SQL path should still answer: %d rows", len(res.Results))
	}
}

func TestTagWalkLegs_LeavesADenseUnionToTheSortIndex(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedWalkFixture(t, database)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = 60 WHERE name IN ('walk_a', 'walk_b')`)
	src := testSource(database)
	if _, ok := tagWalkLegs(src, Query{Expr: Parse("walk_a OR walk_b"), Sort: "newest"}); ok {
		t.Error("an OR whose usage covers the library walks")
	}
	if _, ok := tagWalkLegs(src, Query{Expr: Parse("walk_a"), Sort: "newest"}); !ok {
		t.Error("a lone tag stays a walk however common")
	}
}

func TestExecuteAdjacent_TheTagWalkStepsAsTheSortIndexDoes(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedWalkFixture(t, database)
	for _, expr := range []Expr{Parse("walk_a walk_b"), Parse("walk_b OR walk_c -walk_a"), ApplyCeiling(Parse("character:walk_x"), "general")} {
		for _, order := range []string{"desc", "asc"} {
			q := Query{Expr: expr, Sort: "newest", Order: order}
			assertAdjacencyChain(t, database, q, sqlOrderIDs(t, database, q))
		}
	}
}
