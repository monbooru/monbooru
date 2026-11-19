package tagger

import (
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

func writeOverlay(t *testing.T, dataPath, taggerName, body string) {
	t.Helper()
	mustWriteFile(t, OverlayPath(dataPath, taggerName), []byte(body), 0o644)
}

var sampleStock = []DispatchEntry{
	{Source: "monochrome", Category: "medium"},
	{Source: "comic", Category: "medium"},
	{Source: "photo_(medium)", Category: "medium", Name: "photo"},
}

func TestLoadDispatch_StockRules(t *testing.T) {
	tmp := t.TempDir()
	d := LoadDispatch(sampleStock, tmp, "wd-swinv2", canonicalCatIDs)
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

	d := LoadDispatch(sampleStock, tmp, "wd-swinv2", canonicalCatIDs)

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
			d := LoadDispatch(sampleStock, tmp, "wd-swinv2", canonicalCatIDs)
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

	d := LoadDispatch(nil, tmp, "custom", canonicalCatIDs)
	rule, ok := d.Lookup("annoying_label")
	if !ok {
		t.Fatalf("empty-category rule should still produce a Lookup hit (Drop=true)")
	}
	if !rule.Drop {
		t.Errorf("empty-category rule must produce Drop=true, got %+v", rule)
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

	d := LoadDispatch(nil, tmp, "custom", canonicalCatIDs)
	rule, _ := d.Lookup("kept_as_is")
	if rule.Name != "" {
		t.Errorf("missing name must stay empty so caller keeps the source label, got %q", rule.Name)
	}
}

func TestLoadDispatch_NoConfigReturnsEmptyTable(t *testing.T) {
	tmp := t.TempDir()
	d := LoadDispatch(nil, tmp, "no-such-tagger", canonicalCatIDs)
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
