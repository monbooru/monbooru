package tagger

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadLabelsCamieJSON_Fixture(t *testing.T) {
	f, err := os.Open("testdata/camie_v2_min.json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	got, err := loadLabelsJSON(f, camieLabelJSON)
	if err != nil {
		t.Fatalf("loadLabelsJSON: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	cases := []struct {
		idx     int
		name    string
		catName string
	}{
		{0, "1girl", "general"},
		{1, "hatsune_miku", "character"},
		{2, "wlop", "artist"},
		{3, "neon_genesis_evangelion", "copyright"},
		{4, "explicit", "rating"},
	}
	for _, c := range cases {
		if got[c.idx].name != c.name {
			t.Errorf("idx %d name = %q, want %q", c.idx, got[c.idx].name, c.name)
		}
		if got[c.idx].categoryName != c.catName {
			t.Errorf("idx %d categoryName = %q, want %q", c.idx, got[c.idx].categoryName, c.catName)
		}
		if got[c.idx].placeholder {
			t.Errorf("idx %d unexpectedly placeholder", c.idx)
		}
	}
}

func TestLoadLabelsCamieJSON_FillsHoles(t *testing.T) {

	body := `{
		"dataset_info": {
			"tag_mapping": {
				"idx_to_tag": {"0":"a","1":"b","3":"d"},
				"tag_to_category": {"a":"general","b":"general","d":"general"}
			}
		}
	}`
	got, err := loadLabelsJSON(strings.NewReader(body), camieLabelJSON)
	if err != nil {
		t.Fatalf("loadLabelsJSON: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (max idx 3 + 1)", len(got))
	}
	if !got[2].placeholder {
		t.Errorf("idx 2 should be a placeholder, got %+v", got[2])
	}
	if got[2].name != "_unsupported_2" {
		t.Errorf("idx 2 name = %q, want _unsupported_2", got[2].name)
	}
}

func TestResolveProfile_CamieRegistryFolder(t *testing.T) {
	tmp := t.TempDir()
	installRegistryTagger(t, tmp, "camie-v2")
	got := mustResolveProfile(t, tmp, "camie-v2", "camie-tagger-v2-metadata.json")
	want := Profile{
		Name:           "camie-v2",
		Layout:         "nchw",
		Channels:       "rgb",
		Normalize:      "imagenet",
		Pad:            "mean_color_aspect",
		Activation:     "logits",
		LabelFormat:    "json",
		LabelJSON:      &camieLabelJSON,
		CategoryScheme: "name_string",
		OutputIndex:    1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("camie-v2 profile drift:\n got %+v\nwant %+v", got, want)
	}
}
