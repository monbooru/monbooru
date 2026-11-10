package tagger

import (
	"path/filepath"
	"testing"
)

var canonicalCatIDs = map[string]int64{
	"general":   1,
	"character": 2,
	"artist":    3,
	"copyright": 4,
	"meta":      5,
	"rating":    6,
	"medium":    7,
	"person":    8,
	"year":      9,
}

func writeOverlay(t *testing.T, modelPath, taggerName, body string) {
	t.Helper()
	dir := filepath.Join(modelPath, taggerName)
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "dispatch.json"), []byte(body), 0o644)
}

func TestLoadDispatch_EmbeddedDefaults(t *testing.T) {
	tmp := t.TempDir()
	d := LoadDispatch(tmp, "wd-swinv2", canonicalCatIDs)
	if rule, ok := d.Lookup("monochrome"); !ok || rule.Drop || rule.CatID != canonicalCatIDs["medium"] || rule.Name != "" {
		t.Errorf("monochrome rule = %+v ok=%v, want CatID=medium, Name='', Drop=false", rule, ok)
	}
}

func TestLoadDispatch_OverlayReplaceAndAppend(t *testing.T) {
	tmp := t.TempDir()
	writeOverlay(t, tmp, "wd-swinv2", `{
		"version": 1,
		"rules": [
			{"source": "monochrome", "category": "meta"},
			{"source": "brand_new_label", "category": "character", "name": "renamed"}
		]
	}`)

	d := LoadDispatch(tmp, "wd-swinv2", canonicalCatIDs)

	if rule, ok := d.Lookup("monochrome"); !ok || rule.CatID != canonicalCatIDs["meta"] {
		t.Errorf("overlay did not replace monochrome: %+v ok=%v", rule, ok)
	}
	if rule, ok := d.Lookup("brand_new_label"); !ok || rule.CatID != canonicalCatIDs["character"] || rule.Name != "renamed" {
		t.Errorf("overlay did not append brand_new_label: %+v ok=%v", rule, ok)
	}

	if _, ok := d.Lookup("comic"); !ok {
		t.Errorf("embedded comic rule should still resolve after overlay")
	}
}

func TestLoadDispatch_ARefusedOverlayKeepsTheEmbeddedRule(t *testing.T) {
	for _, tc := range []struct{ name, overlay string }{
		{"UnknownCategoryDropped", `{"version": 1, "rules": [{"source": "monochrome", "category": "definitely_not_a_category"}]}`},
		{"SchemaVersionMismatch", `{"version": 999, "rules": [{"source": "monochrome", "category": "meta"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			writeOverlay(t, tmp, "wd-swinv2", tc.overlay)
			d := LoadDispatch(tmp, "wd-swinv2", canonicalCatIDs)
			if rule, ok := d.Lookup("monochrome"); !ok || rule.CatID != canonicalCatIDs["medium"] {
				t.Errorf("the refused overlay touched the embedded monochrome rule: %+v ok=%v", rule, ok)
			}
		})
	}
}

func TestLoadDispatch_EmptyCategoryDrops(t *testing.T) {
	tmp := t.TempDir()
	writeOverlay(t, tmp, "custom", `{
		"version": 1,
		"rules": [
			{"source": "annoying_label", "category": ""}
		]
	}`)

	d := LoadDispatch(tmp, "custom", canonicalCatIDs)
	rule, ok := d.Lookup("annoying_label")
	if !ok {
		t.Fatalf("empty-category rule should still produce a Lookup hit (Drop=true)")
	}
	if !rule.Drop {
		t.Errorf("empty-category rule must produce Drop=true, got %+v", rule)
	}
}

func TestLoadDispatch_CamieRatingAndYearStripped(t *testing.T) {
	tmp := t.TempDir()
	installRegistryTagger(t, tmp, "camie-v2")
	d := LoadDispatch(tmp, "camie-v2", canonicalCatIDs)
	cases := []struct {
		source, wantName string
		wantCat          string
	}{
		{"rating_general", "general", "rating"},
		{"rating_sensitive", "sensitive", "rating"},
		{"rating_questionable", "questionable", "rating"},
		{"rating_explicit", "explicit", "rating"},
		{"year_2018", "2018", "year"},
		{"year_2024", "2024", "year"},
	}
	for _, c := range cases {
		rule, ok := d.Lookup(c.source)
		if !ok {
			t.Errorf("%s: no rule (canonical name should be stripped)", c.source)
			continue
		}
		if rule.Name != c.wantName {
			t.Errorf("%s: rule.Name = %q, want %q", c.source, rule.Name, c.wantName)
		}
		if rule.CatID != canonicalCatIDs[c.wantCat] {
			t.Errorf("%s: rule.CatID = %d, want %d (%s)", c.source, rule.CatID, canonicalCatIDs[c.wantCat], c.wantCat)
		}
	}
	if rule, ok := d.Lookup("greyscale"); !ok || rule.CatID != canonicalCatIDs["medium"] {
		t.Errorf("greyscale rule = %+v ok=%v, want CatID=medium", rule, ok)
	}
}

func TestLoadDispatch_OverlayMissingNameKeepsSource(t *testing.T) {
	tmp := t.TempDir()
	writeOverlay(t, tmp, "custom", `{
		"version": 1,
		"rules": [
			{"source": "kept_as_is", "category": "character"}
		]
	}`)

	d := LoadDispatch(tmp, "custom", canonicalCatIDs)
	rule, _ := d.Lookup("kept_as_is")
	if rule.Name != "" {
		t.Errorf("missing name must stay empty so caller keeps the source label, got %q", rule.Name)
	}
}

func TestLoadDispatch_NoConfigReturnsEmptyTable(t *testing.T) {
	tmp := t.TempDir()
	d := LoadDispatch(tmp, "no-such-tagger", canonicalCatIDs)
	if d == nil {
		t.Fatal("LoadDispatch must never return nil")
	}
	if _, ok := d.Lookup("anything"); ok {
		t.Errorf("empty table must not resolve any source")
	}
}

func TestDispatchTable_LookupOnNilTable(t *testing.T) {
	var d *DispatchTable
	if _, ok := d.Lookup("x"); ok {
		t.Errorf("nil DispatchTable Lookup should always be a miss")
	}
}

func TestLoadDispatch_GoldenShippedMappings(t *testing.T) {
	tmp := t.TempDir()

	cases := []struct {
		tagger, source string
		wantCat        string
		wantName       string
	}{
		{"joytag", "hatsune_miku", "character", ""},
		{"joytag", "photo_(medium)", "medium", "photo"},
		{"wd-swinv2", "dakimakura_(medium)", "medium", "dakimakura"},
		{"wd-swinv2", "monochrome", "medium", ""},
		{"wd-swinv2", "comic", "medium", ""},
	}
	for _, c := range cases {
		t.Run(c.tagger+"/"+c.source, func(t *testing.T) {
			d := LoadDispatch(tmp, c.tagger, canonicalCatIDs)
			rule, ok := d.Lookup(c.source)
			if !ok {
				t.Fatalf("missing dispatch rule for %s/%s", c.tagger, c.source)
			}
			if rule.Drop {
				t.Fatalf("%s/%s unexpectedly Drop=true", c.tagger, c.source)
			}
			if rule.CatID != canonicalCatIDs[c.wantCat] {
				t.Errorf("%s/%s catID = %d, want %s (%d)", c.tagger, c.source, rule.CatID, c.wantCat, canonicalCatIDs[c.wantCat])
			}
			if rule.Name != c.wantName {
				t.Errorf("%s/%s name = %q, want %q", c.tagger, c.source, rule.Name, c.wantName)
			}
		})
	}
}
