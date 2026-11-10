package tagger

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var (
	wd14Profile, _   = parseStockProfile("", "wd-swinv2")
	joytagProfile, _ = parseStockProfile("", "joytag")
)

func TestResolveProfile_Heuristic(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		taggerName, tagsFile string
		wantLayout           string
		wantLabelFormat      string
	}{
		{"unknown-tagger", "tags.csv", "nhwc", "csv"},
		{"unknown-tagger", "tags.txt", "nchw", "txt"},
	}
	for _, c := range cases {
		p := mustResolveProfile(t, tmp, c.taggerName, c.tagsFile)
		if p.Layout != c.wantLayout {
			t.Errorf("%s: Layout = %q, want %q", c.tagsFile, p.Layout, c.wantLayout)
		}
		if p.LabelFormat != c.wantLabelFormat {
			t.Errorf("%s: LabelFormat = %q, want %q", c.tagsFile, p.LabelFormat, c.wantLabelFormat)
		}
	}
}

func TestResolveProfile_HeuristicIsTheShippedProfile(t *testing.T) {
	tmp := t.TempDir()
	for ext, shipped := range map[string]Profile{"tags.csv": wd14Profile, "tags.txt": joytagProfile} {
		got := mustResolveProfile(t, tmp, "unknown-tagger", ext)
		shipped.Name = "unknown-tagger"
		if !reflect.DeepEqual(got, shipped) {
			t.Errorf("%s: heuristic %+v, want the shipped %+v", ext, got, shipped)
		}
	}
	if wd14Profile.Layout != "nhwc" || wd14Profile.Channels != "bgr" || joytagProfile.Normalize != "clip" {
		t.Errorf("shipped profiles moved: wd14 %+v, joytag %+v", wd14Profile, joytagProfile)
	}
}

func TestResolveProfile_Embedded(t *testing.T) {
	tmp := t.TempDir()

	got := mustResolveProfile(t, tmp, "animetimm-eva02", "tags.csv")
	want := Profile{
		Name:           "animetimm-eva02",
		InputSize:      448,
		Layout:         "nchw",
		Channels:       "rgb",
		Normalize:      "clip",
		Pad:            "white_square",
		Activation:     "sigmoid_in_model",
		LabelFormat:    "csv",
		CategoryScheme: "wd14_numeric",
		OutputIndex:    2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("embedded animetimm-eva02 profile drift:\n got %+v\nwant %+v", got, want)
	}
}

func TestResolveProfile_PixaiEmbedded(t *testing.T) {
	p := mustResolveProfile(t, t.TempDir(), "pixai-v1", "tags.json")
	half := [3]float32{0.5, 0.5, 0.5}
	if p.Normalize != "mean_std" || *p.NormMean != half || *p.NormStd != half || p.NormScale != 255 {
		t.Errorf("normalisation = %s %v %v %v", p.Normalize, p.NormMean, p.NormStd, p.NormScale)
	}
	if p.Pad != "fill" || *p.FillColor != [3]uint8{} {
		t.Errorf("pad = %s %v, want a black fill", p.Pad, p.FillColor)
	}
	if p.LabelJSON == nil || p.LabelJSON.Shape != "blocks" || p.CategoryMap["style"] != "artist" {
		t.Errorf("labels = %+v, map %v", p.LabelJSON, p.CategoryMap)
	}
	rules := DefaultDispatchRules("", "pixai-v1")
	if len(rules) != 4 || rules[0].Category != "rating" {
		t.Errorf("dispatch rules = %+v, want the four rating renames", rules)
	}
}

func TestResolveProfile_SidecarOverridesEmbedded(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd-swinv2")
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "tagger.json"), []byte(`{"version":1,"profile":{"activation":"logits"}}`), 0o644)
	p := mustResolveProfile(t, tmp, "wd-swinv2", "tags.csv")
	if p.Activation != "logits" {
		t.Errorf("sidecar override missed: Activation = %q, want logits", p.Activation)
	}

	if p.Layout != "nhwc" {
		t.Errorf("sidecar should not have wiped Layout: got %q", p.Layout)
	}
}

func TestSidecarProfileFields_NamesEveryMergedAxis(t *testing.T) {
	tmp := t.TempDir()
	write := func(name, profile string) {
		dir := filepath.Join(tmp, name)
		mustMkdirAll(t, dir, 0o755)
		mustWriteFile(t, filepath.Join(dir, "tagger.json"), []byte(`{"version":1,"profile":`+profile+`}`), 0o644)
	}
	write("wd-swinv2", `{"input_size":512,"layout":"nchw","channels":"rgb","normalize":"mean_std","norm_mean":[0.5,0.5,0.5],"norm_std":[0.5,0.5,0.5],"norm_scale":255,"pad":"mean_color_aspect",
		"fill_color":[1,2,3],"activation":"logits","label_format":"joytag_txt","category_scheme":"single_general","output_index":1}`)
	write("empty", `{}`)
	write("two", `{"fill_color":[0,0,9],"output_index":2}`)

	all := []string{"input_size", "layout", "channels", "normalize", "norm_mean", "norm_std", "norm_scale", "pad", "fill_color", "activation", "label_format", "category_scheme", "output_index"}
	if got := SidecarProfileFields(tmp, "wd-swinv2"); !slices.Equal(got, all) {
		t.Errorf("all axes: got %v, want %v", got, all)
	}
	if got := SidecarProfileFields(tmp, "empty"); got != nil {
		t.Errorf("empty profile: got %v, want nil", got)
	}
	if got := SidecarProfileFields(tmp, "two"); !slices.Equal(got, []string{"fill_color", "output_index"}) {
		t.Errorf("two axes: got %v", got)
	}
	if got := SidecarProfileFields(tmp, "absent"); got != nil {
		t.Errorf("no sidecar: got %v, want nil", got)
	}

	half := &[3]float32{0.5, 0.5, 0.5}
	want := Profile{Name: "wd-swinv2", InputSize: 512, Layout: "nchw", Channels: "rgb", Normalize: "mean_std", NormMean: half, NormStd: half, NormScale: 255, Pad: "mean_color_aspect",
		FillColor: &[3]uint8{1, 2, 3}, Activation: "logits", LabelFormat: "txt", CategoryScheme: "single_general", OutputIndex: 1}
	if got := mustResolveProfile(t, tmp, "wd-swinv2", "tags.csv"); !reflect.DeepEqual(got, want) {
		t.Errorf("merged profile: got %+v, want %+v", got, want)
	}
}

func TestProfileAxesNameTheJSONKeys(t *testing.T) {
	var keys []string
	pt := reflect.TypeFor[Profile]()
	for i := range pt.NumField() {
		if name, _, _ := strings.Cut(pt.Field(i).Tag.Get("json"), ","); name != "-" {
			keys = append(keys, name)
		}
	}
	var axes []string
	for _, axis := range profileAxes {
		axes = append(axes, axis.name)
	}
	slices.Sort(keys)
	slices.Sort(axes)
	if !slices.Equal(axes, keys) {
		t.Errorf("profileAxes names %v, Profile JSON keys %v", axes, keys)
	}
}

func TestResolveProfile_BadSchemaVersionFallsThrough(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd-swinv2")
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "tagger.json"), []byte(`{"version":999,"profile":{"activation":"logits"}}`), 0o644)
	p := mustResolveProfile(t, tmp, "wd-swinv2", "tags.csv")

	if p.Activation != "sigmoid_in_model" {
		t.Errorf("schema-mismatch sidecar should not override embedded: got Activation=%q", p.Activation)
	}
}

func TestEmittedCategories(t *testing.T) {
	tmp := t.TempDir()
	write := func(tagger, file, body, profile string) {
		dir := filepath.Join(tmp, tagger)
		mustMkdirAll(t, dir, 0o755)
		mustWriteFile(t, filepath.Join(dir, file), []byte(body), 0o644)
		if profile != "" {
			mustWriteFile(t, filepath.Join(dir, "tagger.json"), []byte(`{"version":1,"profile":`+profile+`}`), 0o644)
		}
	}
	installRegistryTagger(t, tmp, "camie-v2")
	write("wd", "tags.csv", "id,name,category\n0,a,0\n", "")
	write("wd2", "tags.csv", "id,name,category\n0,a,0\n", `{"category_map":{"rating":"general"}}`)
	write("joy", "tags.txt", "a\n", "")
	write("camie-v2", "meta.json", `{"dataset_info":{"tag_mapping":{"idx_to_tag":{"0":"a","1":"b","2":"explicit"},"tag_to_category":{"a":"year","b":"artist","explicit":"rating"}}}}`, "")
	write("pix", "tags.json", `{"categories":[{"name":"general","tags":["a"]},{"name":"style","tags":["b"]}]}`,
		`{"layout":"nchw","channels":"rgb","normalize":"none","pad":"white_square","activation":"logits","label_format":"json","label_json":{"shape":"blocks","path":"categories"},"category_scheme":"name_string","category_map":{"style":"artist"}}`)
	cases := []struct {
		tagger, file string
		want         []string
	}{
		{"wd", "tags.csv", []string{"artist", "character", "copyright", "general", "meta", "rating"}},
		{"wd2", "tags.csv", []string{"artist", "character", "copyright", "general", "meta", "rating"}},
		{"joy", "tags.txt", []string{"general"}},
		{"camie-v2", "meta.json", []string{"artist", "rating", "year"}},
		{"pix", "tags.json", []string{"artist", "general"}},
		{"absent", "tags.bin", nil},
	}
	for _, c := range cases {
		if got := EmittedCategories(tmp, c.tagger, c.file); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.tagger, got, c.want)
		}
	}
}

func TestResolveProfile_SidecarBlackFillOverridesStock(t *testing.T) {
	tmp := t.TempDir()
	installRegistryTagger(t, tmp, "camie-v2")
	dir := filepath.Join(tmp, "camie-v2")
	mustWriteFile(t, filepath.Join(dir, "tagger.json"), []byte(`{"version":1,"profile":{"fill_color":[0,0,0]}}`), 0o644)
	p := mustResolveProfile(t, tmp, "camie-v2", "camie-tagger-v2-metadata.json")
	if p.FillColor == nil || *p.FillColor != [3]uint8{0, 0, 0} {
		t.Errorf("FillColor = %v, want [0 0 0]", p.FillColor)
	}
	if got := SidecarProfileFields(tmp, "camie-v2"); !slices.Equal(got, []string{"fill_color"}) {
		t.Errorf("sidecar fields = %v, want [fill_color]", got)
	}
}

func TestProfileFingerprint_Stable(t *testing.T) {
	a := wd14Profile
	a.Name = "x"
	b := wd14Profile
	b.Name = "y"
	if a.fingerprint() != b.fingerprint() {
		t.Errorf("Name field leaks into fingerprint")
	}

	c := wd14Profile
	c.Activation = "logits"
	if a.fingerprint() == c.fingerprint() {
		t.Errorf("Activation change must change fingerprint")
	}
}

func TestProfileValidate_NamesTheBadAxis(t *testing.T) {
	cases := []struct {
		set  func(*Profile)
		want string
	}{
		{func(p *Profile) { p.Layout = "hwc" }, `profile "x": bad layout "hwc"`},
		{func(p *Profile) { p.Channels = "" }, `profile "x": bad channels ""`},
		{func(p *Profile) { p.Normalize = "div256" }, `profile "x": bad normalize "div256"`},
		{func(p *Profile) { p.Pad = "black_square" }, `profile "x": bad pad "black_square"`},
		{func(p *Profile) { p.Activation = "softmax" }, `profile "x": bad activation "softmax"`},
		{func(p *Profile) { p.LabelFormat = "yaml" }, `profile "x": bad label_format "yaml"`},
		{func(p *Profile) { p.LabelFormat = "json" }, `profile "x": label_format json needs label_json`},
		{func(p *Profile) { p.LabelJSON = &LabelJSON{Shape: "tree"} }, `profile "x": bad label_json shape "tree"`},
		{func(p *Profile) { p.CategoryScheme = "numeric" }, `profile "x": bad category_scheme "numeric"`},
		{func(p *Profile) { p.Layout, p.Pad = "", "" }, `profile "x": bad layout ""`},
		{func(p *Profile) { p.Normalize = "mean_std"; p.NormMean = &[3]float32{} }, `profile "x": normalize mean_std needs norm_mean and norm_std`},
		{func(p *Profile) { p.NormScale = 255 }, `profile "x": norm_mean, norm_std and norm_scale need normalize mean_std, not "none"`},
		{func(p *Profile) {
			p.Normalize, p.NormMean, p.NormStd = "mean_std", &[3]float32{}, &[3]float32{1, 0, 1}
		}, `profile "x": norm_std has a zero`},
		{func(p *Profile) { p.Pad = "fill" }, `profile "x": pad fill needs fill_color`},
	}
	for _, c := range cases {
		p := wd14Profile
		p.Name = "x"
		c.set(&p)
		if err := p.validate(); err == nil || err.Error() != c.want {
			t.Errorf("validate() = %v, want %s", err, c.want)
		}
	}
	for _, p := range []Profile{wd14Profile, joytagProfile} {
		if err := p.validate(); err != nil {
			t.Errorf("validate(%+v) = %v, want nil", p, err)
		}
	}
}
