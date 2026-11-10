package metadata

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestWorkflowTerms_APIFormat(t *testing.T) {
	raw := `{
		"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "illustriousXL.safetensors"}},
		"5": {"class_type": "KSampler", "_meta": {"title": "Base pass"},
		      "inputs": {"seed": 88123, "steps": 28, "cfg": 6.5, "sampler_name": "dpmpp_2m",
		                 "scheduler": "karras", "denoise": 1.0, "model": ["1", 0], "vae": null}},
		"9": {"class_type": "LoraLoader", "inputs": {"lora_name": "elong_style.safetensors", "strength_model": 0.45}}
	}`
	want := []string{
		"cfg=6.5",
		"ckpt_name=illustriousXL.safetensors",
		"denoise=1",
		"lora_name=elong_style.safetensors",
		"node=CheckpointLoaderSimple",
		"node=KSampler",
		"node=LoraLoader",
		"sampler_name=dpmpp_2m",
		"scheduler=karras",
		"steps=28",
		"strength_model=0.45",
		"title=Base pass",
	}
	if got := WorkflowTerms(raw); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestComfyScalar_KeepsIntegersPast2To53(t *testing.T) {
	for raw, want := range map[string]string{
		"9007199254740993":     "9007199254740993",
		"-9007199254740993":    "-9007199254740993",
		"18446744073709551615": "18446744073709551615",
		"7.0":                  "7",
		"0.85":                 "0.85",
		"1e3":                  "1000",
	} {
		if got, ok := comfyScalar(json.RawMessage(raw)); !ok || got != want {
			t.Errorf("comfyScalar(%s) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

func TestWorkflowTerms_NodeGraphFormat(t *testing.T) {
	raw := `{
		"last_node_id": 9,
		"nodes": [
			{"id": 1, "type": "KSampler", "title": "Sampler", "inputs": [{"name": "model", "link": 4}]},
			{"id": 2, "type": "SaveImage", "inputs": {"filename_prefix": "ComfyUI"}}
		],
		"links": [[4, 1, 0, 2, 0]]
	}`
	want := []string{"filename_prefix=ComfyUI", "node=KSampler", "node=SaveImage", "title=Sampler"}
	if got := WorkflowTerms(raw); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestWorkflowTerms_NestedJSONString(t *testing.T) {
	raw := `{
		"3": {"class_type": "Lora Loader Stack (rgthree)", "inputs": {
			"lora_str": "[{\"name\":\"detail_enhancer.safetensors\",\"hidden\":false,\"strength\":0.8},{\"name\":\"elong_style.safetensors\",\"hidden\":true,\"strength\":0.45}]"
		}}
	}`
	want := []string{
		"lora_str.hidden=false",
		"lora_str.hidden=true",
		"lora_str.name=detail_enhancer.safetensors",
		"lora_str.name=elong_style.safetensors",
		"lora_str.strength=0.45",
		"lora_str.strength=0.8",
		"node=Lora Loader Stack (rgthree)",
	}
	if got := WorkflowTerms(raw); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestWorkflowTerms_NestedBeatsLengthCap(t *testing.T) {
	raw := `{"1": {"class_type": "Stack", "inputs": {"slots": "[{\"name\":\"` +
		strings.Repeat("a", 100) + `\"},{\"name\":\"` + strings.Repeat("b", 100) + `\"}]"}}}`
	got := WorkflowTerms(raw)
	want := []string{
		"node=Stack",
		"slots.name=" + strings.Repeat("a", 100),
		"slots.name=" + strings.Repeat("b", 100),
	}
	if !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestWorkflowTerms_Skips(t *testing.T) {
	long := strings.Repeat("masterpiece, best quality, ", 40)
	raw := fmt.Sprintf(`{
		"1": {"class_type": "KSampler", "inputs": {
			"seed": 42, "noise_seed": 7, "latent": ["2", 0], "vae": null,
			"opts": {"a": 1}, "empty": "", "text": %q}}
	}`, long)
	want := []string{"node=KSampler"}
	if got := WorkflowTerms(raw); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestWorkflowTerms_DedupeAndRedundantTitle(t *testing.T) {
	raw := `{
		"1": {"class_type": "Reroute", "_meta": {"title": "Reroute"}, "inputs": {"value": 1}},
		"2": {"class_type": "Reroute", "_meta": {"title": "Reroute"}, "inputs": {"value": 1}},
		"3": {"class_type": "Reroute", "inputs": {"value": 2}}
	}`
	want := []string{"node=Reroute", "value=1", "value=2"}
	if got := WorkflowTerms(raw); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestWorkflowTerms_PerImageCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{`)
	for i := range 1000 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%d": {"class_type": "N%d", "inputs": {"v": %d}}`, i, i, i)
	}
	b.WriteString(`}`)
	if got := len(WorkflowTerms(b.String())); got != comfyTermsPerImage {
		t.Errorf("term count = %d, want %d", got, comfyTermsPerImage)
	}
}

func TestWorkflowTerms_Normalisation(t *testing.T) {
	raw := "{\"1\": {\"class_type\": \"Note\", \"inputs\": {\"text\": \"  two\\n\\nlines\\there  \", " +
		"\"flag\": true, \"count\": 12.0, \"ratio\": 0.125}}}"
	want := []string{"count=12", "flag=true", "node=Note", "ratio=0.125", "text=two lines here"}
	if got := WorkflowTerms(raw); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestWorkflowTerms_PythonLiteralsAndMalformed(t *testing.T) {
	if got := WorkflowTerms(`{"1": {"class_type": "KSampler", "inputs": {"cfg": NaN, "steps": 20}}}`); !slices.Equal(got, []string{"node=KSampler", "steps=20"}) {
		t.Errorf("sanitised terms = %q", got)
	}
	for _, raw := range []string{"", "   ", "not json", "[1,2,3]"} {
		if got := WorkflowTerms(raw); got != nil {
			t.Errorf("WorkflowTerms(%q) = %q, want nil", raw, got)
		}
	}
}

func TestParseComfyWorkflowNodes_SearchTerm(t *testing.T) {
	raw := `{"1": {"class_type": "KSampler", "inputs": {
		"sampler_name": "dpmpp_2m", "steps": 28, "seed": 42, "model": ["2", 0],
		"stack": "[{\"name\":\"a.safetensors\"}]"}}}`
	want := map[string]string{
		"sampler_name": "sampler_name=dpmpp_2m",
		"steps":        "steps=28",
		"seed":         "",
		"model":        "",
		"stack":        "",
	}
	nodes := ParseComfyWorkflowNodes(raw)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	for _, p := range nodes[0].Params {
		if got := p.SearchTerm; got != want[p.Name] {
			t.Errorf("param %q SearchTerm = %q, want %q", p.Name, got, want[p.Name])
		}
	}
}
