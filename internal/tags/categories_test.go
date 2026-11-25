package tags

import (
	"errors"
	"slices"
	"testing"
)

func categoryIDs(t *testing.T, svc *Service) []int64 {
	t.Helper()
	cats := mustListCategories(t, svc)
	ids := make([]int64, len(cats))
	for i, c := range cats {
		ids[i] = c.ID
	}
	return ids
}

func TestCreateCategory_GoesAtTheEndOfTheOrder(t *testing.T) {
	_, svc := setupTestDB(t)
	cat := mustCreateCategory(t, svc, "aaa", "#123456")
	ids := categoryIDs(t, svc)
	if ids[len(ids)-1] != cat.ID {
		t.Errorf("order = %v, want the new category %d last", ids, cat.ID)
	}
	def, err := svc.DefaultCategoryOrder()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(def, ids) {
		t.Error("the shipped order puts a custom category before general, so it should differ")
	}
}

func TestReorderCategories_StoresTheOrder(t *testing.T) {
	_, svc := setupTestDB(t)
	ids := categoryIDs(t, svc)
	slices.Reverse(ids)

	if err := svc.ReorderCategories(ids); err != nil {
		t.Fatal(err)
	}
	if got := categoryIDs(t, svc); !slices.Equal(got, ids) {
		t.Errorf("order = %v, want %v", got, ids)
	}
}

func TestReorderCategories_RefusesAListThatIsNotEveryCategoryOnce(t *testing.T) {
	_, svc := setupTestDB(t)
	before := categoryIDs(t, svc)
	dup := slices.Clone(before)
	dup[1] = dup[0]
	unknown := slices.Clone(before)
	unknown[0] = 987654

	for name, ids := range map[string][]int64{
		"partial":   before[1:],
		"duplicate": dup,
		"unknown":   unknown,
		"longer":    append(slices.Clone(before), before[0]),
	} {
		if err := svc.ReorderCategories(ids); !errors.Is(err, ErrCategoryOrderStale) {
			t.Errorf("%s: err = %v, want ErrCategoryOrderStale", name, err)
		}
		if got := categoryIDs(t, svc); !slices.Equal(got, before) {
			t.Errorf("%s: a refused list changed the order to %v", name, got)
		}
	}
}

func TestDeleteCategoryWithItsTags_RefusesAGlobalTag(t *testing.T) {
	database, svc := setupTestDB(t)
	cat := mustCreateCategory(t, svc, "style", "#aabbcc")
	shared := mustTag(t, svc, "animation", cat.ID)
	mustTag(t, svc, "watercolor", cat.ID)
	if err := svc.SetGlobal([]int64{shared.ID}, true); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "delete_all", 0); !errors.Is(err, ErrTagGlobal) {
		t.Fatalf("delete with its tags = %v, want ErrTagGlobal", err)
	}
	if n := countRows(t, database, "tags WHERE category_id = ?", cat.ID); n != 2 {
		t.Errorf("tags left in the category = %d, want 2", n)
	}
	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "move", 0); err != nil {
		t.Fatalf("moving the tags out = %v", err)
	}
}
