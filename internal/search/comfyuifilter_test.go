package search

import (
	"context"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
)

func seedComfyWorkflow(t *testing.T, database *db.DB, imageID int64, raw string) {
	t.Helper()
	if _, err := database.Write.Exec(
		`INSERT INTO comfyui_metadata (image_id, raw_workflow) VALUES (?, ?)`, imageID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := gallery.BackfillComfyTerms(context.Background(), database, nil); err != nil {
		t.Fatal(err)
	}
}

func comfyFixture(t *testing.T) (*db.DB, int64, int64, int64) {
	t.Helper()
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "comfy_a.png")
	b := ingestTestImageWithID(t, database, env, "comfy_b.png")
	c := ingestTestImageWithID(t, database, env, "comfy_c.png")
	seedComfyWorkflow(t, database, a, `{
		"1": {"class_type": "KSampler", "_meta": {"title": "Base pass"},
		      "inputs": {"sampler_name": "dpmpp_2m", "cfg": 8.5, "steps": 30}},
		"2": {"class_type": "LoraLoader", "inputs": {"lora_name": "elong_style.safetensors"}},
		"3": {"class_type": "IPAdapterSimple", "inputs": {"weight": 0.85}}}`)
	seedComfyWorkflow(t, database, b, `{
		"1": {"class_type": "KSampler", "inputs": {"sampler_name": "euler", "cfg": 5.0, "steps": 20}},
		"2": {"class_type": "IPAdapterUnifiedLoader", "inputs": {"preset": "PLUS (high strength)"}},
		"3": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "v1..5-pruned.safetensors"}}}`)
	seedComfyWorkflow(t, database, c, `{
		"0": {"class_type": "SamplerZoom", "inputs": {}},
		"1": {"class_type": "Lora Loader Stack (rgthree)",
		      "inputs": {"lora_str": "[{\"name\":\"elong_v2.safetensors\",\"hidden\":true}]"}}}`)
	return database, a, b, c
}

func TestComfyUIFilter_Forms(t *testing.T) {
	database, a, b, c := comfyFixture(t)
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"comfyui:KSampler", []int64{a, b}},
		{"comfyui:ksampler", []int64{a, b}},
		{"comfyui:node=KSampler", []int64{a, b}},
		{"comfyui:IPAdapter*", []int64{a, b}},
		{"comfyui:*adapter*", []int64{a, b}},
		{"comfyui:SamplerZ*", []int64{c}},
		{"comfyui:IPAdapter", nil},
		{`comfyui:"title=Base pass"`, []int64{a}},
		{"comfyui:sampler_name=dpmpp_2m", []int64{a}},
		{"comfyui:lora_name=*elong*", []int64{a}},
		{"comfyui:*_name=", []int64{a, b}},
		{"comfyui:lora_name=", []int64{a}},
		{"comfyui:lora*=", []int64{a, c}},
		{"comfyui:*=*elong*", []int64{a, c}},
		{"comfyui:lora_str.name=elong_v2.safetensors", []int64{c}},
		{"comfyui:cfg>=8", []int64{a}},
		{"comfyui:cfg=5..9", []int64{a, b}},
		{"comfyui:cfg=5", []int64{b}},
		{"comfyui:cfg=5.0", nil},
		{"comfyui:cfg=5..5", []int64{b}},
		{"comfyui:steps<25", []int64{b}},
		{"comfyui:ckpt_name=v1..5-pruned.safetensors", []int64{b}},
		{"comfyui:", []int64{a, b, c}},
		{"comfyui:=nothing", nil},
		{"comfyui:cfg>=notanumber", nil},
		{"-comfyui:KSampler", []int64{c}},
		{"comfyui:KSampler comfyui:lora_name=elong_style.safetensors", []int64{a}},
		{"comfyui:IPAdapterSimple OR comfyui:*rgthree*", []int64{a, c}},
	} {
		checkSearchIDs(t, database, tc.query, tc.want)
	}
}

func TestComfyUIFilter_UnderscoreIsLiteral(t *testing.T) {
	database, env := setupSearchDB(t)
	hit := ingestTestImageWithID(t, database, env, "comfy_underscore.png")
	miss := ingestTestImageWithID(t, database, env, "comfy_underscore_miss.png")
	seedComfyWorkflow(t, database, hit, `{"1": {"class_type": "L", "inputs": {"n": "a_b.safetensors"}}}`)
	seedComfyWorkflow(t, database, miss, `{"1": {"class_type": "L", "inputs": {"n": "axb.safetensors"}}}`)

	checkSearchIDs(t, database, "comfyui:n=*a_b*", []int64{hit})
}

func TestComfyUIFilter_ComparesANumberUnderANonASCIIName(t *testing.T) {
	database, env := setupSearchDB(t)
	hit := ingestTestImageWithID(t, database, env, "comfy_strength.png")
	miss := ingestTestImageWithID(t, database, env, "comfy_strength_miss.png")
	seedComfyWorkflow(t, database, hit, `{"1": {"class_type": "L", "inputs": {"强度": 0.8}}}`)
	seedComfyWorkflow(t, database, miss, `{"1": {"class_type": "L", "inputs": {"强度": 0.2}}}`)

	checkSearchIDs(t, database, "comfyui:强度>=0.5", []int64{hit})
	checkSearchIDs(t, database, "comfyui:强度=0.1..0.3", []int64{miss})
}

func TestComfyUIFilter_ComposesWithRatingCeiling(t *testing.T) {
	database, env := setupSearchDB(t)
	safe := ingestTestImageWithID(t, database, env, "comfy_safe.png")
	adult := ingestTestImageWithID(t, database, env, "comfy_adult.png")
	seedComfyWorkflow(t, database, safe, `{"1": {"class_type": "KSampler", "inputs": {}}}`)
	seedComfyWorkflow(t, database, adult, `{"1": {"class_type": "KSampler", "inputs": {}}}`)
	attachTag(t, database, adult, ratingTagID(t, database, "explicit"))

	ceiling := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	expr := AndExpr{Left: Parse("comfyui:KSampler"), Right: ceiling}
	res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 1 || len(res.Results) != 1 || res.Results[0].ID != safe {
		t.Fatalf("under a SFW ceiling = %d matches %+v, want just image %d", res.Total, res.Results, safe)
	}
}

func TestComfyUIFilter_ScopeStreamHonoursTerm(t *testing.T) {
	database, a, _, _ := comfyFixture(t)
	var got []int64
	if err := NewScope(Parse("comfyui:lora_name=elong_style.safetensors"), "").
		Stream(testSource(database), func(id int64) error {
			got = append(got, id)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a {
		t.Fatalf("scope = %v, want [%d]", got, a)
	}
}
