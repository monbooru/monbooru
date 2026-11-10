package tagger

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeLabelFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	mustWriteFile(t, p, []byte(body), 0o644)
	return p
}

func TestLoadLabels_WD14CSV(t *testing.T) {
	p := writeLabelFile(t, "tags.csv",
		"tag_id,name,category\n"+
			"1,1girl,0\n"+
			"2,hatsune miku,4\n"+
			"3,(...),0\n")
	got, err := loadLabels(p, Profile{LabelFormat: "csv"})
	if err != nil {
		t.Fatalf("loadLabels: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].name != "1girl" || got[0].categoryID != 0 || got[0].placeholder {
		t.Errorf("row 0 = %+v", got[0])
	}

	if got[1].name != "hatsune_miku" || got[1].categoryID != 4 {
		t.Errorf("row 1 = %+v", got[1])
	}

	if !got[2].placeholder || got[2].name != "_unsupported_2" {
		t.Errorf("row 2 = %+v", got[2])
	}
}

func TestLoadLabels_JoytagTxt(t *testing.T) {
	p := writeLabelFile(t, "tags.txt", "solo\n\nblue sky\n")
	got, err := loadLabels(p, Profile{LabelFormat: "txt"})
	if err != nil {
		t.Fatalf("loadLabels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (blank lines skipped)", len(got))
	}
	if got[0].name != "solo" || got[0].categoryID != 0 {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[1].name != "blue_sky" {
		t.Errorf("row 1 = %+v", got[1])
	}
}

func TestLoadLabels_UnsupportedFormat(t *testing.T) {
	p := writeLabelFile(t, "tags.csv", "tag_id,name,category\n")
	if _, err := loadLabels(p, Profile{LabelFormat: "nope"}); err == nil {
		t.Fatal("want error for unsupported label_format")
	}
}

func TestLoadLabels_CSVCategoryNamesAndTwoColumns(t *testing.T) {
	p := writeLabelFile(t, "tags.csv", "idx,name,category\n0,bkub,style\n1,solo\n2,hatsune_miku,4\n")
	got, err := loadLabels(p, Profile{LabelFormat: "csv"})
	if err != nil {
		t.Fatalf("loadLabels: %v", err)
	}
	if len(got) != 3 || got[0].categoryName != "style" || got[1].name != "solo" || got[1].categoryName != "" || got[2].categoryID != 4 {
		t.Errorf("labels = %+v", got)
	}
}

func TestLoadLabels_JSONShapes(t *testing.T) {
	cases := []struct {
		name, body string
		spec       LabelJSON
		want       []tagLabel
	}{
		{"list", `{"labels":["a","b"]}`, LabelJSON{Shape: "list", Path: "labels"},
			[]tagLabel{{name: "a"}, {name: "b"}}},
		{"hf id2label", `{"id2label":{"1":"logo","0":"photograph"}}`, LabelJSON{Shape: "index_map", Path: "id2label"},
			[]tagLabel{{name: "photograph"}, {name: "logo"}}},
		{"blocks with offsets", `{"categories":[{"name":"general","offset":0,"count":2,"tags":["1girl","solo"]},{"name":"style","offset":3,"count":1,"tags":["bkub"]}]}`,
			LabelJSON{Shape: "blocks", Path: "categories"},
			[]tagLabel{{name: "1girl", categoryName: "general"}, {name: "solo", categoryName: "general"},
				{name: "_unsupported_2", placeholder: true}, {name: "bkub", categoryName: "style"}}},
		{"blocks in sequence", `[{"name":"general","tags":["a"]},{"name":"meta","tags":["b"]}]`,
			LabelJSON{Shape: "blocks"},
			[]tagLabel{{name: "a", categoryName: "general"}, {name: "b", categoryName: "meta"}}},
	}
	for _, c := range cases {
		p := writeLabelFile(t, "tags.json", c.body)
		got, err := loadLabels(p, Profile{LabelFormat: "json", LabelJSON: &c.spec})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestLoadLabels_JSONRefusals(t *testing.T) {
	cases := []struct {
		body string
		spec LabelJSON
		want string
	}{
		{`{"a":{}}`, LabelJSON{Shape: "list", Path: "a.b"}, `no "b" in "a.b"`},
		{`{"a":{}}`, LabelJSON{Shape: "list", Path: "a"}, `list at "a" is not an array`},
		{`[]`, LabelJSON{Shape: "list"}, `no labels`},
		{`{"9999999":"x"}`, LabelJSON{Shape: "index_map"}, `past`},
		{`[]`, LabelJSON{Shape: "nope"}, `unknown shape`},
		{`[{"offset":-1,"tags":["a","b"]}]`, LabelJSON{Shape: "blocks"}, `bad offset`},
		{`[{"offset":-5,"tags":["a"]}]`, LabelJSON{Shape: "blocks"}, `bad offset`},
		{`[{"offset":1e30,"tags":["a"]}]`, LabelJSON{Shape: "blocks"}, `bad offset`},
	}
	for _, c := range cases {
		p := writeLabelFile(t, "tags.json", c.body)
		_, err := loadLabels(p, Profile{LabelFormat: "json", LabelJSON: &c.spec})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s with %+v: err = %v, want %q", c.body, c.spec, err, c.want)
		}
	}
}
