package tags

import (
	"strings"
	"testing"
)

func mustCount(t *testing.T, label string, count func() (int, error), want int) {
	t.Helper()
	n, err := count()
	if err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("%s = %d; want %d", label, n, want)
	}
}

func mustMarkDistinct(t *testing.T, svc *Service, ids ...int64) (marked, skipped int) {
	t.Helper()
	marked, skipped, err := svc.MarkDistinct(ids)
	if err != nil {
		t.Fatal(err)
	}
	return marked, skipped
}

func TestDistinctMarkSettlesTheConflictUntilAThirdCategoryArrives(t *testing.T) {
	_, svc := setupTestDB(t)
	artist := categoryIDOf(t, svc, "artist")
	character := categoryIDOf(t, svc, "character")
	a := mustTag(t, svc, "kirin", artist)
	c := mustTag(t, svc, "kirin", character)

	if marked, _ := mustMarkDistinct(t, svc, a.ID, c.ID); marked != 1 {
		t.Fatalf("marked = %d; want 1", marked)
	}
	mustCount(t, "ConflictsCount", svc.ConflictsCount, 0)
	mustCount(t, "DistinctCount", svc.DistinctCount, 2)
	mustCount(t, "DistinctSettledCount", svc.DistinctSettledCount, 2)
	if _, total := mustListTags(t, svc, TagFilter{DistinctOnly: true, ShowZero: true}); total != 2 {
		t.Errorf("distinct listing total = %d; want 2", total)
	}

	g := mustTag(t, svc, "kirin", generalCategoryID(t, svc))
	mustCount(t, "ConflictsCount", svc.ConflictsCount, 3)
	mustCount(t, "DistinctSettledCount", svc.DistinctSettledCount, 0)
	list, total := mustListTags(t, svc, TagFilter{ConflictsOnly: true, ShowZero: true})
	if total != 3 || len(list) != 3 {
		t.Fatalf("conflicts listing total = %d; want 3", total)
	}

	if marked, _ := mustMarkDistinct(t, svc, a.ID, c.ID, g.ID); marked != 2 {
		t.Errorf("marked = %d; want 2, the a/c pair already held", marked)
	}
	mustCount(t, "ConflictsCount", svc.ConflictsCount, 0)
}

func TestMarkDistinctSkipsLoneTagsAndAliases(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	species := categoryIDOf(t, svc, "species")
	fox := mustTag(t, svc, "fox", general)
	mustTag(t, svc, "fox", species)
	canon := mustTag(t, svc, "vulpes", general)
	alias := mustCreateAlias(t, svc, "fox", categoryIDOf(t, svc, "meta"), canon.ID)

	marked, skipped := mustMarkDistinct(t, svc, fox.ID, alias.ID)
	if marked != 0 || skipped != 2 {
		t.Errorf("marked, skipped = %d, %d; want 0, 2", marked, skipped)
	}
	mustCount(t, "ConflictsCount", svc.ConflictsCount, 2)
}

func TestDistinctMarkDropsOnRenameAndMergeButSurvivesACategoryMove(t *testing.T) {
	_, svc := setupTestDB(t)
	artist := categoryIDOf(t, svc, "artist")
	character := categoryIDOf(t, svc, "character")
	copyright := categoryIDOf(t, svc, "copyright")
	general := generalCategoryID(t, svc)

	partners := func(id int64) []DistinctPartner {
		t.Helper()
		m, err := svc.DistinctPartners([]int64{id})
		if err != nil {
			t.Fatal(err)
		}
		return m[id]
	}

	a := mustTag(t, svc, "yoshida", artist)
	c := mustTag(t, svc, "yoshida", character)
	mustMarkDistinct(t, svc, a.ID, c.ID)
	if err := svc.ChangeTagCategory(a.ID, copyright); err != nil {
		t.Fatal(err)
	}
	if got := partners(c.ID); len(got) != 1 || got[0].ID != a.ID || got[0].Category != "copyright" {
		t.Errorf("after a category move partners = %+v; want copyright:yoshida", got)
	}
	if err := svc.RenameTag(a.ID, "yoshida_(artist)"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenameTag(a.ID, "yoshida"); err != nil {
		t.Fatal(err)
	}
	if got := partners(c.ID); len(got) != 0 {
		t.Errorf("a rename kept the mark: %+v", got)
	}

	mustMarkDistinct(t, svc, a.ID, c.ID)
	g := mustTag(t, svc, "yoshida", general)
	mustMarkDistinct(t, svc, a.ID, g.ID)
	mustMerge(t, svc, a.ID, g.ID)
	if got := partners(c.ID); len(got) != 0 {
		t.Errorf("the merged tag kept its mark: %+v", got)
	}
	if got := partners(g.ID); len(got) != 0 {
		t.Errorf("the canonical inherited a mark: %+v", got)
	}

	mustMarkDistinct(t, svc, c.ID, g.ID)
	if err := svc.DeleteTag(g.ID); err != nil {
		t.Fatal(err)
	}
	if got := partners(c.ID); len(got) != 0 {
		t.Errorf("a deleted tag kept its mark: %+v", got)
	}
}

func TestUnmarkDistinctAndSameNameTags(t *testing.T) {
	_, svc := setupTestDB(t)
	a := mustTag(t, svc, "saber", categoryIDOf(t, svc, "artist"))
	c := mustTag(t, svc, "saber", categoryIDOf(t, svc, "character"))
	g := mustTag(t, svc, "saber", generalCategoryID(t, svc))
	mustMarkDistinct(t, svc, a.ID, c.ID, g.ID)

	same, err := svc.SameNameTags(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(same) != 2 || same[0].CategoryName != "character" || same[1].CategoryName != "general" {
		t.Errorf("SameNameTags = %+v; want character then general", same)
	}

	if ok, err := svc.UnmarkDistinctPair(c.ID, a.ID); err != nil || !ok {
		t.Fatalf("UnmarkDistinctPair = %v, %v", ok, err)
	}
	mustCount(t, "ConflictsCount", svc.ConflictsCount, 2)
	n, err := svc.UnmarkDistinct([]int64{g.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("UnmarkDistinct removed %d; want 2", n)
	}
	mustCount(t, "ConflictsCount", svc.ConflictsCount, 3)
	mustCount(t, "DistinctCount", svc.DistinctCount, 0)
}

func TestDistinctCountDrivesFromThePairs(t *testing.T) {
	database, _ := setupTestDB(t)

	if _, err := database.Write.Exec(`ANALYZE; DELETE FROM sqlite_stat1; DELETE FROM sqlite_stat4;
		INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES
			('tags', 'idx_tags_active_name', '152000 1'),
			('tags', 'idx_tags_name', '160000 1'),
			('tags', 'sqlite_autoindex_tags_1', '160000 1 1');
		ANALYZE sqlite_schema`); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Write.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM tags t WHERE ` + distinctWhere)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	reads := 0
	for i, step := range plan {
		if strings.HasPrefix(step, "SEARCH ta ") || strings.HasPrefix(step, "SCAN ta ") {
			reads++
			if i == 0 || !strings.HasPrefix(plan[i-1], "SCAN dp ") {
				t.Errorf("tag ta read before the pairs:\n%s", strings.Join(plan, "\n"))
			}
		}
	}
	if reads == 0 {
		t.Errorf("no read of ta in:\n%s", strings.Join(plan, "\n"))
	}
}

func TestNamedTagsListsThePlainTagsOfEveryCategory(t *testing.T) {
	_, svc := setupTestDB(t)
	c := mustTag(t, svc, "miku", categoryIDOf(t, svc, "character"))
	g := mustTag(t, svc, "hatsune_miku", generalCategoryID(t, svc))
	if _, err := svc.CreateAlias("miku", generalCategoryID(t, svc), g.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.NamedTags([]string{"miku", "hatsune_miku", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != g.ID || got[1].ID != c.ID {
		t.Errorf("NamedTags = %+v; want hatsune_miku then the character miku, not the alias", got)
	}
	if got, err := svc.NamedTags(nil); err != nil || len(got) != 0 {
		t.Errorf("NamedTags(nil) = %+v, %v; want none", got, err)
	}
}
