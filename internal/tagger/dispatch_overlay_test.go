package tagger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func readOverlayDoc(t *testing.T, modelPath, taggerName string) dispatchDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(modelPath, taggerName, "dispatch.json"))
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	var doc dispatchDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse overlay: %v", err)
	}
	return doc
}

func TestSaveDispatchOverlay_WritesSortedRules(t *testing.T) {
	modelPath := t.TempDir()
	mustMkdirAll(t, filepath.Join(modelPath, "wd-swinv2"), 0o755)
	err := SaveDispatchOverlay(modelPath, "wd-swinv2", []DispatchEntry{
		{Source: "zzz", Category: "meta"},
		{Source: "aaa", Category: ""},
	})
	if err != nil {
		t.Fatalf("SaveDispatchOverlay: %v", err)
	}
	doc := readOverlayDoc(t, modelPath, "wd-swinv2")
	if doc.Version != dispatchSchemaVersion {
		t.Errorf("version = %d, want %d", doc.Version, dispatchSchemaVersion)
	}
	if len(doc.Rules) != 2 || doc.Rules[0].Source != "aaa" || doc.Rules[1].Source != "zzz" {
		t.Errorf("rules = %+v, want aaa then zzz", doc.Rules)
	}
}

func TestSaveDispatchOverlay_DropsRulesIdenticalToEmbedded(t *testing.T) {
	modelPath := t.TempDir()
	mustMkdirAll(t, filepath.Join(modelPath, "wd-swinv2"), 0o755)
	embedded := DefaultDispatchRules("", "wd-swinv2")
	if len(embedded) == 0 {
		t.Fatal("wd-swinv2 has no embedded rules; test needs one")
	}

	err := SaveDispatchOverlay(modelPath, "wd-swinv2", []DispatchEntry{
		embedded[0],
		{Source: "engrish", Category: ""},
	})
	if err != nil {
		t.Fatalf("SaveDispatchOverlay: %v", err)
	}
	doc := readOverlayDoc(t, modelPath, "wd-swinv2")
	if len(doc.Rules) != 1 || doc.Rules[0].Source != "engrish" {
		t.Errorf("rules = %+v, want the engrish delta only", doc.Rules)
	}
}

func TestSaveDispatchOverlay_EmptyDeletesFile(t *testing.T) {
	modelPath := t.TempDir()
	dir := filepath.Join(modelPath, "wd-swinv2")
	mustMkdirAll(t, dir, 0o755)
	if err := SaveDispatchOverlay(modelPath, "wd-swinv2", []DispatchEntry{{Source: "engrish", Category: ""}}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
	embedded := DefaultDispatchRules("", "wd-swinv2")

	if err := SaveDispatchOverlay(modelPath, "wd-swinv2", []DispatchEntry{embedded[0]}); err != nil {
		t.Fatalf("SaveDispatchOverlay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dispatch.json")); !os.IsNotExist(err) {
		t.Fatalf("overlay should be deleted, stat err = %v", err)
	}

	if err := SaveDispatchOverlay(modelPath, "wd-swinv2", nil); err != nil {
		t.Fatalf("SaveDispatchOverlay on empty: %v", err)
	}
}

func TestUpdateDispatchOverlay_RefusesAnUnreadableFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"trailing comma", `{"version":1,"rules":[{"source":"engrish","category":""},]}`},
		{"other version", `{"version":2,"rules":[{"source":"engrish","category":""}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelPath := t.TempDir()
			writeOverlay(t, modelPath, "wd-swinv2", tc.body)
			err := UpdateDispatchOverlay(modelPath, "wd-swinv2", func(overlay map[string]DispatchEntry) error {
				overlay["monochrome"] = DispatchEntry{Source: "monochrome", Category: "meta"}
				return nil
			})
			if err == nil {
				t.Fatal("UpdateDispatchOverlay over a broken file: err = nil, want an error")
			}
			got, _ := os.ReadFile(filepath.Join(modelPath, "wd-swinv2", "dispatch.json"))
			if string(got) != tc.body {
				t.Errorf("dispatch.json = %q, want it untouched", got)
			}
		})
	}
}

func TestBrowseLabels_ResolvesRulesAndScheme(t *testing.T) {
	modelPath := t.TempDir()
	dir := filepath.Join(modelPath, "wd-swinv2")
	mustMkdirAll(t, dir, 0o755)
	csv := "tag_id,name,category\n" +
		"1,1girl,0\n" +
		"2,monochrome,0\n" +
		"3,hatsune_miku,4\n" +
		"4,engrish,0\n"
	mustWriteFile(t, filepath.Join(dir, "tags.csv"), []byte(csv), 0o644)
	writeOverlay(t, modelPath, "wd-swinv2", `{"version":1,"rules":[{"source":"engrish","category":""}]}`)

	views, err := BrowseLabels(modelPath, "wd-swinv2", "tags.csv", canonicalCatIDs)
	if err != nil {
		t.Fatalf("BrowseLabels: %v", err)
	}
	byName := map[string]LabelView{}
	for _, v := range views {
		byName[v.Source] = v
	}
	if v := byName["1girl"]; v.CatName != "general" || v.Rule != "" {
		t.Errorf("1girl = %+v, want general via the wd14 scheme", v)
	}

	if v := byName["monochrome"]; v.CatName != "medium" || v.Rule != "default" {
		t.Errorf("monochrome = %+v, want medium via the default rule", v)
	}
	if v := byName["hatsune_miku"]; v.CatName != "character" {
		t.Errorf("hatsune_miku = %+v, want character", v)
	}
	if v := byName["engrish"]; !v.Muted || v.Rule != "custom" {
		t.Errorf("engrish = %+v, want muted by the custom rule", v)
	}
}

func TestBrowseLabels_SeesALabelFileOverwrittenInPlace(t *testing.T) {
	modelPath := t.TempDir()
	dir := filepath.Join(modelPath, "wd-swinv2")
	mustMkdirAll(t, dir, 0o755)
	tags := filepath.Join(dir, "tags.csv")
	mustWriteFile(t, tags, []byte("tag_id,name,category\n1,1girl,0\n"), 0o644)
	names := func() []string {
		t.Helper()
		views, err := BrowseLabels(modelPath, "wd-swinv2", "tags.csv", canonicalCatIDs)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, v := range views {
			out = append(out, v.Source+"="+v.CatName)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{"1girl=general"}) {
		t.Fatalf("first read = %v", got)
	}
	mustWriteFile(t, tags, []byte("tag_id,name,category\n1,1girl,0\n2,solo,0\n"), 0o644)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(tags, later, later); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"1girl=general", "solo=general"}) {
		t.Errorf("after the overwrite = %v, want solo too", got)
	}
	writeOverlay(t, modelPath, "wd-swinv2", `{"version":1,"rules":[{"source":"solo","category":""}]}`)
	if got := names(); !slices.Equal(got, []string{"1girl=general", "solo="}) {
		t.Errorf("after an overlay edit = %v, want solo muted", got)
	}
}

func TestAFoldersDispatchDefaultIsStock(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "registry-tagger")
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "model.onnx"), []byte("x"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "tags.csv"), []byte("id,name,category\n0,1koma,0\n1,solo,0\n"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "dispatch_default.json"), []byte(`{"version":1,"rules":[{"source":"1koma","category":""}]}`), 0o644)

	if _, labels := listTaggerFiles(dir); len(labels) != 1 || labels[0] != "tags.csv" {
		t.Errorf("label pool = %v, want tags.csv alone", labels)
	}
	if rule, ok := LoadDispatch(tmp, "registry-tagger", canonicalCatIDs).Lookup("1koma"); !ok || !rule.Drop {
		t.Errorf("the folder's default rule was not applied: %+v %v", rule, ok)
	}
	views, err := BrowseLabels(tmp, "registry-tagger", "tags.csv", canonicalCatIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Source == "1koma" && (v.Rule != "default" || !v.Muted) {
			t.Errorf("1koma = %+v, want a muted default rule", v)
		}
	}

	if err := SaveDispatchOverlay(tmp, "registry-tagger", []DispatchEntry{{Source: "1koma", Category: ""}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dispatch.json")); !os.IsNotExist(err) {
		t.Errorf("a rule identical to the folder's default was written to the overlay: %v", err)
	}
	if err := SaveDispatchOverlay(tmp, "registry-tagger", []DispatchEntry{{Source: "1koma", Category: "meta"}}); err != nil {
		t.Fatal(err)
	}
	if rule, _ := LoadDispatch(tmp, "registry-tagger", canonicalCatIDs).Lookup("1koma"); rule.CatName != "meta" {
		t.Errorf("the overlay did not win over the folder's default: %+v", rule)
	}
}
