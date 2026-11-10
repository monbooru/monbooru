package web

import (
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func TestGroupTagsByOrigin_FreshGroupsFirstThenStaleInFirstSeenOrder(t *testing.T) {
	group := templateFuncs()["groupTagsByOrigin"].(func([]models.Tag) []originTagGroup)
	list := []models.Tag{
		{Name: "t1", Origin: "a"},
		{Name: "t2", Origin: "a", Stale: true},
		{Name: "t3", Origin: "b"},
		{Name: "t4", Origin: "c", Stale: true},
		{Name: "t5", Origin: "a"},
		{Name: "t6", Origin: "b", Stale: true},
		{Name: "t7", Origin: "a", Stale: true},
	}
	type want struct {
		origin string
		stale  bool
		names  string
	}
	wants := []want{
		{"a", false, "t1 t5"},
		{"b", false, "t3"},
		{"a", true, "t2 t7"},
		{"c", true, "t4"},
		{"b", true, "t6"},
	}
	got := group(list)
	if len(got) != len(wants) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(wants), got)
	}
	for i, g := range got {
		names := ""
		for j, tag := range g.Tags {
			if j > 0 {
				names += " "
			}
			names += tag.Name
		}
		if g.Origin != wants[i].origin || g.Stale != wants[i].stale || names != wants[i].names {
			t.Errorf("group %d = {%s %v %s}, want %+v", i, g.Origin, g.Stale, names, wants[i])
		}
	}
}
