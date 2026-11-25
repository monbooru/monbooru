package tagger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/worker"
)

func readOverlayDoc(t *testing.T, dataPath, taggerName string) dispatchDoc {
	t.Helper()
	data, err := os.ReadFile(OverlayPath(dataPath, taggerName))
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	var doc dispatchDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse overlay: %v", err)
	}
	return doc
}

func wdModel() fakeModel {
	return fakeModel{
		name: "wd-swinv2",
		labels: []worker.Label{
			{Name: "1girl"}, {Name: "monochrome"}, {Name: "hatsune miku", CategoryID: 4}, {Name: "engrish"},
		},
		mappings: []worker.Mapping{{Source: "monochrome", Category: "medium"}},
	}
}

func TestSaveDispatchOverlay_WritesSortedRules(t *testing.T) {
	useFakeWorker(t, wdModel())
	dataPath := t.TempDir()
	err := SaveDispatchOverlay(dataPath, "wd-swinv2", []DispatchEntry{
		{Source: "zzz", Category: "meta"},
		{Source: "aaa", Category: ""},
	})
	if err != nil {
		t.Fatalf("SaveDispatchOverlay: %v", err)
	}
	doc := readOverlayDoc(t, dataPath, "wd-swinv2")
	if doc.Version != dispatchSchemaVersion {
		t.Errorf("version = %d, want %d", doc.Version, dispatchSchemaVersion)
	}
	if len(doc.Rules) != 2 || doc.Rules[0].Source != "aaa" || doc.Rules[1].Source != "zzz" {
		t.Errorf("rules = %+v, want aaa then zzz", doc.Rules)
	}
}

func TestSaveDispatchOverlay_DropsRulesIdenticalToStock(t *testing.T) {
	useFakeWorker(t, wdModel())
	dataPath := t.TempDir()
	stock := DefaultDispatchRules("wd-swinv2")
	if len(stock) != 1 {
		t.Fatalf("stock = %+v, want the worker's one mapping", stock)
	}

	if err := SaveDispatchOverlay(dataPath, "wd-swinv2", []DispatchEntry{stock[0], {Source: "engrish", Category: ""}}); err != nil {
		t.Fatalf("SaveDispatchOverlay: %v", err)
	}
	doc := readOverlayDoc(t, dataPath, "wd-swinv2")
	if len(doc.Rules) != 1 || doc.Rules[0].Source != "engrish" {
		t.Errorf("rules = %+v, want the engrish delta only", doc.Rules)
	}

	if err := SaveDispatchOverlay(dataPath, "wd-swinv2", []DispatchEntry{stock[0]}); err != nil {
		t.Fatalf("SaveDispatchOverlay: %v", err)
	}
	if _, err := os.Stat(OverlayPath(dataPath, "wd-swinv2")); !os.IsNotExist(err) {
		t.Fatalf("overlay should be deleted, stat err = %v", err)
	}
	if err := SaveDispatchOverlay(dataPath, "wd-swinv2", nil); err != nil {
		t.Fatalf("SaveDispatchOverlay on empty: %v", err)
	}
}

func TestUpdateDispatchOverlay_RefusesAnUnreadableFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"trailing comma", `{"version":1,"rules":[{"source":"engrish","category":""},]}`},
		{"other version", `{"version":2,"rules":[{"source":"engrish","category":""}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataPath := t.TempDir()
			writeOverlay(t, dataPath, "wd-swinv2", tc.body)
			err := UpdateDispatchOverlay(dataPath, "wd-swinv2", func(overlay map[string]DispatchEntry) error {
				overlay["monochrome"] = DispatchEntry{Source: "monochrome", Category: "meta"}
				return nil
			})
			if err == nil {
				t.Fatal("UpdateDispatchOverlay over a broken file: err = nil, want an error")
			}
			got, _ := os.ReadFile(OverlayPath(dataPath, "wd-swinv2"))
			if string(got) != tc.body {
				t.Errorf("dispatch.json = %q, want it untouched", got)
			}
		})
	}
}

func TestBrowseLabels_ResolvesRulesAndScheme(t *testing.T) {
	useFakeWorker(t, wdModel())
	dataPath := t.TempDir()
	writeOverlay(t, dataPath, "wd-swinv2", `{"version":1,"rules":[{"source":"engrish","category":""}]}`)

	views, err := BrowseLabels(dataPath, "wd-swinv2", canonicalCatIDs)
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
		t.Errorf("monochrome = %+v, want medium via the stock rule", v)
	}

	if v := byName["hatsune_miku"]; v.CatName != "character" {
		t.Errorf("hatsune_miku = %+v, want character", v)
	}
	if v := byName["engrish"]; !v.Muted || v.Rule != "custom" {
		t.Errorf("engrish = %+v, want muted by the custom rule", v)
	}
}

func TestBrowseLabels_FollowsAnOverlayEdit(t *testing.T) {
	useFakeWorker(t, fakeModel{name: "wd-swinv2", labels: labelsOf("1girl", "solo")})
	dataPath := t.TempDir()
	names := func() []string {
		t.Helper()
		views, err := BrowseLabels(dataPath, "wd-swinv2", canonicalCatIDs)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, v := range views {
			out = append(out, v.Source+"="+v.CatName)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{"1girl=general", "solo=general"}) {
		t.Fatalf("first read = %v", got)
	}
	writeOverlay(t, dataPath, "wd-swinv2", `{"version":1,"rules":[{"source":"solo","category":""}]}`)
	if got := names(); !slices.Equal(got, []string{"1girl=general", "solo="}) {
		t.Errorf("after an overlay edit = %v, want solo muted", got)
	}
}

func TestBrowseLabels_ATaggerNoWorkerHoldsIsAnError(t *testing.T) {
	useFakeWorker(t)
	if _, err := BrowseLabels(t.TempDir(), "gone", canonicalCatIDs); err == nil {
		t.Error("BrowseLabels: want an error")
	}
}

func TestMoveDispatchOverlays_CopiesOnceAndLeavesTheOld(t *testing.T) {
	modelPath, dataPath := t.TempDir(), t.TempDir()
	body := `{"version":1,"rules":[{"source":"1koma","category":""}]}`
	writeOverlayIn := func(name string) string {
		dir := filepath.Join(modelPath, name)
		mustMkdirAll(t, dir, 0o755)
		p := filepath.Join(dir, "dispatch.json")
		mustWriteFile(t, p, []byte(body), 0o644)
		return p
	}
	old := writeOverlayIn("wd-swinv2")
	writeOverlayIn("kept")
	mustWriteFile(t, OverlayPath(dataPath, "kept"), []byte("newer"), 0o644)

	MoveDispatchOverlays(modelPath, dataPath)
	if got, _ := os.ReadFile(OverlayPath(dataPath, "wd-swinv2")); string(got) != body {
		t.Errorf("copy = %q, want the model folder's file", got)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the old file is gone: %v", err)
	}
	if got, _ := os.ReadFile(OverlayPath(dataPath, "kept")); string(got) != "newer" {
		t.Errorf("an existing copy was overwritten: %q", got)
	}

	if err := os.Remove(OverlayPath(dataPath, "wd-swinv2")); err != nil {
		t.Fatal(err)
	}
	MoveDispatchOverlays(modelPath, dataPath)
	if _, err := os.Stat(OverlayPath(dataPath, "wd-swinv2")); !os.IsNotExist(err) {
		t.Errorf("a reset mapping came back: %v", err)
	}
}

func TestMoveDispatchOverlays_LeavesNoMarkerUntilEveryMappingIsRead(t *testing.T) {
	dataPath := t.TempDir()
	marked := func() bool {
		_, err := os.Stat(filepath.Join(dataPath, dispatchMoved))
		return err == nil
	}
	root := t.TempDir()
	notADir := filepath.Join(root, "file")
	mustWriteFile(t, notADir, nil, 0o644)
	empty := filepath.Join(root, "unmounted")
	mustMkdirAll(t, empty, 0o755)
	for _, modelPath := range []string{notADir, filepath.Join(root, "missing"), empty} {
		MoveDispatchOverlays(modelPath, dataPath)
		if marked() {
			t.Fatalf("%s: marked as copied", modelPath)
		}
	}

	modelPath := t.TempDir()
	unreadable := filepath.Join(modelPath, "wd-swinv2", "dispatch.json")
	mustMkdirAll(t, unreadable, 0o755)
	MoveDispatchOverlays(modelPath, dataPath)
	if marked() {
		t.Fatal("a mapping that could not be read: marked as copied")
	}

	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"rules":[{"source":"1koma","category":""}]}`
	mustWriteFile(t, unreadable, []byte(body), 0o644)
	MoveDispatchOverlays(modelPath, dataPath)
	if got, _ := os.ReadFile(OverlayPath(dataPath, "wd-swinv2")); string(got) != body || !marked() {
		t.Errorf("once readable: copy = %q, marked = %v", got, marked())
	}
}
