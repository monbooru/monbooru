package tagger

import "testing"

func TestResolveCategory(t *testing.T) {
	overrides := map[string]DispatchRule{
		"monochrome": {CatID: canonicalCatIDs["medium"]},
		"annoying":   {Drop: true},
	}
	nameString := Profile{CategoryScheme: "name_string"}
	for _, tc := range []struct {
		name     string
		prof     Profile
		rules    map[string]DispatchRule
		label    tagLabel
		cat      string
		catName  string
		skip     bool
		override bool
	}{
		{name: "WD14Numeric/general", prof: wd14Profile, label: tagLabel{name: "1girl", categoryID: 0}, cat: "general"},
		{name: "WD14Numeric/character", prof: wd14Profile, label: tagLabel{name: "hatsune_miku", categoryID: 4}, cat: "character"},
		{name: "WD14Numeric/copyright", prof: wd14Profile, label: tagLabel{name: "touhou", categoryID: 3}, cat: "copyright"},
		{name: "WD14Numeric/unknown id falls to general", prof: wd14Profile, label: tagLabel{name: "stray", categoryID: 99}, cat: "general"},
		{name: "WD14Numeric/category 9 without a rating name falls to general", prof: wd14Profile, label: tagLabel{name: "rating_safe", categoryID: 9}, cat: "general"},
		{name: "RatingShortCircuits", prof: wd14Profile, label: tagLabel{name: "general", categoryID: 9}, cat: "rating"},
		{name: "DispatchOverrides/route", prof: wd14Profile, rules: overrides, label: tagLabel{name: "monochrome", categoryID: 0}, cat: "medium", override: true},
		{name: "DispatchOverrides/drop", prof: wd14Profile, rules: overrides, label: tagLabel{name: "annoying", categoryID: 0}, skip: true, override: true},
		{name: "NameStringScheme/copyright", prof: nameString, label: tagLabel{name: "neon_genesis_evangelion", categoryName: "copyright"}, cat: "copyright", catName: "copyright"},
		{name: "NameStringScheme/unknown name falls to general", prof: nameString, label: tagLabel{name: "x", categoryName: "totally-not-a-cat"}, cat: "general"},
		{name: "NameStringRatingShortCircuit", prof: nameString, label: tagLabel{name: "explicit", categoryName: "rating"}, cat: "rating", catName: "rating"},
		{name: "SingleGeneral", prof: joytagProfile, label: tagLabel{name: "1girl", categoryID: 4}, cat: "general"},
		{name: "CategoryMapAndNamedCSVRows/mapped", prof: Profile{CategoryScheme: "name_string", CategoryMap: map[string]string{"style": "artist"}}, label: tagLabel{name: "bkub", categoryName: "style"}, catName: "artist"},
		{name: "CategoryMapAndNamedCSVRows/csv row naming its category", prof: wd14Profile, label: tagLabel{name: "x", categoryName: "character"}, catName: "character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := resolveCategory(tc.prof, tc.label, canonicalCatIDs, &DispatchTable{rules: tc.rules})
			if tc.cat != "" && res.catID != canonicalCatIDs[tc.cat] {
				t.Errorf("catID = %d, want %s's %d: %+v", res.catID, tc.cat, canonicalCatIDs[tc.cat], res)
			}
			if tc.catName != "" && res.catName != tc.catName {
				t.Errorf("catName = %q, want %q: %+v", res.catName, tc.catName, res)
			}
			if res.skip != tc.skip || res.override != tc.override {
				t.Errorf("skip, override = %v, %v, want %v, %v: %+v", res.skip, res.override, tc.skip, tc.override, res)
			}
		})
	}
}
