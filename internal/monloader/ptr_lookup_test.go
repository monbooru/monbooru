package monloader

import (
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

func TestPTRSpellingStems(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"character:samus_aran_(metroid)", []string{
			"character:samus_aran_(metroid)", "character:samus_aran", "character:samus"}},
		{"character:pozyomka_(snowy_plains)_(arknights)", []string{
			"character:pozyomka_(snowy_plains)_(arknights)",
			"character:pozyomka_(snowy_plains)", "character:pozyomka"}},
		{"holding_plectrum", []string{"holding_plectrum", "holding"}},

		{"1girl", []string{"1girl"}},
		{"copyright:ao", nil},
		{"character:初音_ミク", []string{"character:初音_ミク"}},
	} {
		got := ptrSpellingStems(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("stems(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("stems(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestLookupGroupsClassifiesEachSpelling(t *testing.T) {
	cx := testGallery(t)
	svc := cx.TagSvc
	tagSvc := func() *tags.Service { return svc }
	general := cx.GeneralCategoryID
	mustTag := func(name string) int64 {
		t.Helper()
		tag, err := svc.GetOrCreateTag(name, general)
		if err != nil {
			t.Fatal(err)
		}
		return tag.ID
	}
	target := mustTag("blonde_hair")
	yellow := mustTag("yellow_hair")
	mustTag("long_hair")
	for _, a := range []struct {
		name  string
		canon int64
	}{{"blond_hair", target}, {"golden_hair", yellow}} {
		if _, err := svc.CreateAlias(a.name, general, a.canon); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddImplication(target, mustTag("hair")); err != nil {
		t.Fatal(err)
	}

	info := contract.PTRTagInfo{
		Known: true, Ideal: "yellow_hair",
		Aliases:      []string{"blond_hair", "golden_hair", "platinum_hair", "bad*wolf"},
		Implications: []string{"hair", "long_hair", "new_tag", "golden_hair"},
	}
	groups, err := LookupGroups(tagSvc, target, "blonde_hair", "blonde_hair", info)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name     string
		standing Standing
		adds     bool
	}
	want := map[LookupGroupKind][]row{
		LookupAliases: {
			{"yellow_hair", StandingTag, false}, {"blond_hair", StandingHeld, false},
			{"golden_hair", StandingAliasElsewhere, false}, {"platinum_hair", StandingFree, true},
			{"bad*wolf", StandingInvalid, false},
		},
		LookupImplies: {
			{"hair", StandingHeld, false}, {"long_hair", StandingTag, true},
			{"new_tag", StandingFree, true}, {"golden_hair", StandingAliasElsewhere, false},
		},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v, want the aliases and implies groups only", groups)
	}
	for _, g := range groups {
		var got []row
		adds := 0
		for _, r := range g.Rows {
			got = append(got, row{r.Name, r.Standing, r.Adds})
			if r.Adds {
				adds++
			}
		}
		if !slices.Equal(got, want[g.Kind]) || g.Adds != adds {
			t.Errorf("group %d = %+v (adds %d), want %+v", g.Kind, got, g.Adds, want[g.Kind])
		}
		if g.Kind == LookupAliases && g.Rows[2].Canonical != "yellow_hair" {
			t.Errorf("golden_hair names %q as its canonical, want yellow_hair", g.Rows[2].Canonical)
		}
	}

	for _, tc := range []struct {
		spelling string
		standing Standing
		adds     bool
	}{
		{"blonde_hair", StandingSelf, false},
		{"blond_hair", StandingHeld, false},
		{"golden_hair", StandingAliasElsewhere, true},
		{"long_hair", StandingTag, true},
		{"platinum_hair", StandingFree, true},
	} {
		if got := LookupLocalStatus(tagSvc, target, "blonde_hair", tc.spelling); got.Standing != tc.standing || got.Adds != tc.adds {
			t.Errorf("local status of %s = %+v, want standing %d adds %v", tc.spelling, got, tc.standing, tc.adds)
		}
	}
}
