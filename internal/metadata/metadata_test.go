package metadata

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/searchkw"
)

func TestParseA1111Parameters(t *testing.T) {
	input := `a beautiful landscape, oil painting
Negative prompt: ugly, blurry
Steps: 20, Sampler: DPM++ 2M Karras, CFG scale: 7, Seed: 12345, Model: v1-5-pruned`

	sd := parseA1111Parameters(input)
	if sd == nil {
		t.Fatal("expected non-nil SDMetadata")
	}
	if sd.Prompt != "a beautiful landscape, oil painting" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
	if sd.NegativePrompt != "ugly, blurry" {
		t.Errorf("NegativePrompt = %q", sd.NegativePrompt)
	}
	if sd.Steps == nil || *sd.Steps != 20 {
		t.Errorf("Steps = %v", sd.Steps)
	}
	if sd.Sampler != "DPM++ 2M Karras" {
		t.Errorf("Sampler = %q", sd.Sampler)
	}
	if sd.CFGScale == nil || *sd.CFGScale != 7 {
		t.Errorf("CFGScale = %v", sd.CFGScale)
	}
	if sd.Seed == nil || *sd.Seed != 12345 {
		t.Errorf("Seed = %v", sd.Seed)
	}
	if sd.Model != "v1-5-pruned" {
		t.Errorf("Model = %q", sd.Model)
	}
}

func TestParseA1111Parameters_Empty(t *testing.T) {
	sd := parseA1111Parameters("")
	if sd != nil {
		t.Error("expected nil for empty input")
	}
}

func TestParseA1111Parameters_NotA1111(t *testing.T) {
	for _, s := range []string{
		"Paint Tool -SAI- JPEG Encoder v1.00",
		"Created with GIMP",
		"LEAD Technologies Inc. V1.01",
	} {
		if sd := parseA1111Parameters(s); sd != nil {
			t.Errorf("expected nil for %q, got %+v", s, sd)
		}
	}
}

func TestParseA1111Parameters_NoNegative(t *testing.T) {
	input := `just a prompt
Steps: 30, Sampler: Euler, CFG scale: 8, Seed: 999, Model: mymodel`

	sd := parseA1111Parameters(input)
	if sd == nil {
		t.Fatal("expected non-nil")
	}
	if sd.Prompt != "just a prompt" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
	if sd.Model != "mymodel" {
		t.Errorf("Model = %q", sd.Model)
	}
}

func TestParseComfyWorkflow(t *testing.T) {
	raw := `{
		"1": {
			"type": "CheckpointLoaderSimple",
			"inputs": {"ckpt_name": "v1-5-pruned.safetensors"}
		},
		"2": {
			"type": "CLIPTextEncode",
			"inputs": {"text": "positive prompt text"}
		},
		"3": {
			"type": "KSampler",
			"inputs": {"seed": 42, "steps": 20, "cfg": 7.5, "sampler_name": "euler"}
		}
	}`

	comfy := parseComfyWorkflow(raw)
	if comfy == nil {
		t.Fatal("expected non-nil ComfyUIMetadata")
	}
	if comfy.Prompt != "positive prompt text" {
		t.Errorf("Prompt = %q", comfy.Prompt)
	}
	if comfy.ModelCheckpoint != "v1-5-pruned.safetensors" {
		t.Errorf("ModelCheckpoint = %q", comfy.ModelCheckpoint)
	}
	if comfy.Seed == nil || *comfy.Seed != 42 {
		t.Errorf("Seed = %v", comfy.Seed)
	}
	if comfy.Steps == nil || *comfy.Steps != 20 {
		t.Errorf("Steps = %v", comfy.Steps)
	}
	if comfy.CFGScale == nil || *comfy.CFGScale != 7.5 {
		t.Errorf("CFGScale = %v", comfy.CFGScale)
	}
	if comfy.Sampler != "euler" {
		t.Errorf("Sampler = %q", comfy.Sampler)
	}
}

func TestParseComfyPromptChunk_StableAcrossParses(t *testing.T) {
	raw := `{
		"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "base.safetensors"}},
		"2": {"class_type": "LoraLoader", "inputs": {"lora_name": "styleA.safetensors"}},
		"3": {"class_type": "LoraLoader", "inputs": {"lora_name": "styleB.safetensors"}},
		"4": {"class_type": "CLIPTextEncode", "inputs": {"text": "a positive prompt"}},
		"5": {"class_type": "CLIPTextEncode", "inputs": {"text": "a negative prompt"}},
		"6": {"class_type": "KSampler", "inputs": {"seed": 7, "steps": 20, "cfg": 7.5, "sampler_name": "euler", "positive": ["4", 0]}}
	}`

	const wantModel = "base.safetensors + styleA.safetensors + styleB.safetensors"
	first := parseComfyPromptChunk(raw)
	if first == nil {
		t.Fatal("expected non-nil ComfyUIMetadata")
	}
	if first.ModelCheckpoint != wantModel {
		t.Fatalf("ModelCheckpoint = %q, want %q", first.ModelCheckpoint, wantModel)
	}

	for i := 0; i < 50; i++ {
		got := parseComfyPromptChunk(raw)
		if got.ModelCheckpoint != wantModel {
			t.Fatalf("parse %d: ModelCheckpoint = %q, want %q", i, got.ModelCheckpoint, wantModel)
		}
		if got.Prompt != first.Prompt || got.GenerationHash != first.GenerationHash {
			t.Fatalf("parse %d: prompt %q hash %q, want %q / %q", i, got.Prompt, got.GenerationHash, first.Prompt, first.GenerationHash)
		}
	}
}

func TestParseComfyWorkflow_ArrayFormat(t *testing.T) {
	raw := `{
		"nodes": [
			{"type": "KSampler", "inputs": {"seed": 100, "steps": 25, "cfg": 6.0, "sampler_name": "dpm_2"}},
			{"type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "model.ckpt"}}
		]
	}`

	comfy := parseComfyWorkflow(raw)
	if comfy == nil {
		t.Fatal("expected non-nil")
	}
	if comfy.Seed == nil || *comfy.Seed != 100 {
		t.Errorf("Seed = %v", comfy.Seed)
	}
	if comfy.ModelCheckpoint != "model.ckpt" {
		t.Errorf("ModelCheckpoint = %q", comfy.ModelCheckpoint)
	}
}

const savedNodeGraph = `{
  "last_node_id": 9, "last_link_id": 9,
  "nodes": [
    {"id": 4, "type": "CheckpointLoaderSimple", "inputs": [],
     "widgets_values": ["v1-5-pruned.safetensors"]},
    {"id": 6, "type": "CLIPTextEncode", "title": "Positive",
     "inputs": [{"name": "clip", "type": "CLIP", "link": 3}],
     "widgets_values": ["a beautiful landscape"]},
    {"id": 3, "type": "KSampler", "inputs": [{"name": "model", "type": "MODEL", "link": 1}],
     "widgets_values": [156680208700286, "randomize", 20, 8, "euler", "normal", 1]}
  ],
  "links": [[1, 4, 0, 3, 0, "MODEL"]], "extra": {"ds": {"scale": 1}}, "version": 0.4
}`

func TestParseComfyWorkflow_SavedNodeGraph(t *testing.T) {
	comfy := parseComfyWorkflow(savedNodeGraph)
	if comfy == nil {
		t.Fatal("a saved workflow must still produce a ComfyUI row")
	}
	if comfy.Prompt != "" || comfy.ModelCheckpoint != "" || comfy.Seed != nil {
		t.Errorf("recipe read from positional widgets: prompt=%q model=%q seed=%v",
			comfy.Prompt, comfy.ModelCheckpoint, comfy.Seed)
	}
	if comfy.GenerationHash != "" {
		t.Errorf("GenerationHash = %q, want empty - every such image would share it", comfy.GenerationHash)
	}
	want := []string{"node=CLIPTextEncode", "node=CheckpointLoaderSimple", "node=KSampler", "title=Positive"}
	if got := WorkflowTerms(comfy.RawWorkflow); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestParseComfyWorkflowNodes_SavedNodeGraph(t *testing.T) {
	nodes := ParseComfyWorkflowNodes(savedNodeGraph)
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d, want 3: %+v", len(nodes), nodes)
	}
	want := []struct{ key, title, class string }{
		{"4", "CheckpointLoaderSimple", "CheckpointLoaderSimple"},
		{"6", "Positive", "CLIPTextEncode"},
		{"3", "KSampler", "KSampler"},
	}
	for i, w := range want {
		if nodes[i].Key != w.key || nodes[i].Title != w.title || nodes[i].ClassType != w.class {
			t.Errorf("node %d = %+v, want key=%q title=%q class=%q", i, nodes[i], w.key, w.title, w.class)
		}
	}
}

func TestParseComfyWorkflowNodes_SkipsNonNodes(t *testing.T) {
	raw := `{"1": {"class_type": "KSampler", "inputs": {"steps": 20}},
	         "extra": {"ds": {"scale": 1}}, "version": 0.4}`
	nodes := ParseComfyWorkflowNodes(raw)
	if len(nodes) != 1 || nodes[0].ClassType != "KSampler" {
		t.Fatalf("nodes = %+v, want just the KSampler", nodes)
	}
}

func TestParseComfyTags_PromptOverWorkflow(t *testing.T) {
	m := ParseComfyTags(randomNoiseWorkflow, savedNodeGraph)
	if m == nil || m.ModelCheckpoint != "flux.safetensors" || m.RawWorkflow != sanitizeComfyJSON(strings.TrimSpace(randomNoiseWorkflow)) {
		t.Fatalf("got %+v, want the prompt's recipe and raw workflow", m)
	}
	m = ParseComfyTags("", savedNodeGraph)
	if m == nil || m.Prompt != "" || len(ParseComfyWorkflowNodes(m.RawWorkflow)) != 3 {
		t.Fatalf("workflow alone: %+v, want the node graph with no recipe", m)
	}
	if m := ParseComfyTags("", ""); m != nil {
		t.Errorf("no tags: %+v, want nil", m)
	}
}

func TestParseComfyWorkflow_InvalidJSON(t *testing.T) {
	comfy := parseComfyWorkflow("not json")
	if comfy != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestExtractFromPNG_SD(t *testing.T) {
	t.Parallel()
	param := "sample prompt\nNegative prompt: bad\nSteps: 12, Sampler: Euler, CFG scale: 6, Seed: 7, Model: mdl"
	buf := makePNGWithTextChunk("parameters", param)
	dir := t.TempDir()
	path := dir + "/sd.png"
	mustWriteFile(t, path, buf, 0o644)
	sd, comfy := Extract(path, "png")
	if sd == nil {
		t.Fatal("expected non-nil SDMetadata for PNG with `parameters` chunk")
	}
	if sd.Prompt != "sample prompt" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
	if comfy != nil {
		t.Error("SD-only PNG should not yield ComfyUIMetadata")
	}
}

func TestExtractFromPNG_ComfyUI(t *testing.T) {
	t.Parallel()
	workflow := `{"1":{"type":"KSampler","inputs":{"seed":9,"steps":14,"cfg":5,"sampler_name":"euler"}},"2":{"type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"mdl.ckpt"}}}`
	buf := makePNGWithTextChunk("workflow", workflow)
	dir := t.TempDir()
	path := dir + "/cf.png"
	mustWriteFile(t, path, buf, 0o644)
	sd, comfy := Extract(path, "png")
	if comfy == nil {
		t.Fatal("expected ComfyUIMetadata for PNG with `workflow` chunk")
	}
	if comfy.Seed == nil || *comfy.Seed != 9 {
		t.Errorf("Seed = %v, want 9", comfy.Seed)
	}
	if sd != nil {
		t.Error("ComfyUI-only PNG should not yield SDMetadata")
	}
}

func TestExtract_UnsupportedType(t *testing.T) {
	sd, comfy := Extract("test.mp4", "mp4")
	if sd != nil || comfy != nil {
		t.Error("expected nil for video type")
	}
}

func TestExtract_JPEG(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := dir + "/test.jpg"
	mustWriteFile(t, path, []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o644)

	sd, comfy := Extract(path, "jpeg")
	if comfy != nil {
		t.Error("JPEG must never produce ComfyUI metadata")
	}
	if sd != nil {
		t.Errorf("minimal JPEG without EXIF must produce nil SDMetadata, got %+v", sd)
	}
}

func TestExtract_JPEG_NonExistent(t *testing.T) {
	t.Parallel()
	sd, comfy := Extract("/nonexistent/path/image.jpg", "jpeg")
	if sd != nil || comfy != nil {
		t.Errorf("missing file must produce no metadata, got sd=%+v comfy=%+v", sd, comfy)
	}
}

func TestExtractComfyUI_FromPNG(t *testing.T) {
	t.Parallel()
	prompt := `{"1":{"type":"KSampler","inputs":{"seed":3,"steps":8,"cfg":4,"sampler_name":"dpm"}},"2":{"type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"m.ckpt"}}}`
	buf := makePNGWithTextChunk("prompt", prompt)
	dir := t.TempDir()
	path := dir + "/cfp.png"
	mustWriteFile(t, path, buf, 0o644)
	sd, comfy := Extract(path, "png")
	if comfy == nil {
		t.Fatal("expected ComfyUI metadata from `prompt` chunk")
	}
	if sd != nil {
		t.Error("ComfyUI-only PNG must not produce SDMetadata")
	}
}

func TestParseA1111_PromptOnly(t *testing.T) {
	input := "just a prompt with no parameters"
	if sd := parseA1111Parameters(input); sd != nil {
		t.Errorf("expected nil, got %+v", sd)
	}
}

func TestParseA1111_DropsANonFiniteCFGScale(t *testing.T) {
	for _, v := range []string{"inf", "-Infinity", "NaN"} {
		sd := parseA1111Parameters("a cat\nSteps: 20, Sampler: Euler, CFG scale: " + v + ", Seed: 1, Model: foo")
		if sd == nil {
			t.Fatalf("CFG scale %s: no metadata parsed", v)
		}
		if sd.CFGScale != nil {
			t.Errorf("CFG scale %s: CFGScale = %v, want none", v, *sd.CFGScale)
		}
	}
}

func TestGenerationHash_SeedExcluded(t *testing.T) {
	s1 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 111, Model: foo")
	s2 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 222, Model: foo")
	if s1 == nil || s2 == nil {
		t.Fatal("expected non-nil for both inputs")
	}
	if s1.GenerationHash == "" {
		t.Fatal("generation hash is empty")
	}
	if s1.GenerationHash != s2.GenerationHash {
		t.Errorf("hashes differ despite only seed changing: %q vs %q", s1.GenerationHash, s2.GenerationHash)
	}
}

func TestGenerationHash_ModelChangesHash(t *testing.T) {
	s1 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 1, Model: foo")
	s2 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 1, Model: bar")
	if s1.GenerationHash == s2.GenerationHash {
		t.Errorf("expected different hashes for different models, both = %q", s1.GenerationHash)
	}
}

func TestGenerationHash_Golden(t *testing.T) {
	i := func(v int) *int { return &v }
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		prompt, neg, model, sampler string
		steps                       *int
		cfg                         *float64
		want                        string
	}{
		{"a cat", "ugly", "foo", "Euler", i(20), f(7.0), "2318fed49b8a"},
		{"a cat", "", "", "", nil, nil, "41223d4bfdaa"},
		{" a cat ", "ugly", "foo", "Euler a", i(20), f(6.555), "14c81ef452d9"},
		{"", "", "", "", i(0), nil, "474f60c71f5f"},
		{"", "", "", "", nil, f(-1.005), "1eb72eeb7020"},
		{"line\nbreak 100%", "", "sd_xl", "", i(-3), f(12), "a5bbdecc082e"},
		{"  ", "", "", "", nil, nil, ""},
	} {
		if got := computeGenerationHash(tc.prompt, tc.neg, tc.model, tc.sampler, tc.steps, tc.cfg); got != tc.want {
			t.Errorf("computeGenerationHash(%q, %q, %q, %q, ...) = %q, want %q", tc.prompt, tc.neg, tc.model, tc.sampler, got, tc.want)
		}
	}
}

func TestParseA1111_AllWhitespace(t *testing.T) {
	sd := parseA1111Parameters("   \n  ")
	if sd != nil {
		t.Error("expected nil for whitespace-only input")
	}
}

func TestParseComfyWorkflow_EmptyJSON(t *testing.T) {
	comfy := parseComfyWorkflow("{}")
	if comfy != nil {
		t.Error("expected nil for empty workflow object")
	}
}

func TestParseComfyWorkflow_UnknownNodes(t *testing.T) {
	raw := `{
		"1": {"type": "UnknownNode", "inputs": {"foo": "bar"}},
		"2": {"type": "AnotherUnknown", "inputs": {}}
	}`
	comfy := parseComfyWorkflow(raw)
	if comfy == nil {
		t.Fatal("expected a row for a graph of custom nodes")
	}
	if comfy.Prompt != "" || comfy.ModelCheckpoint != "" || comfy.Seed != nil || comfy.GenerationHash != "" {
		t.Errorf("recipe filled from unknown nodes: %+v", comfy)
	}
	want := []string{"foo=bar", "node=AnotherUnknown", "node=UnknownNode"}
	if got := WorkflowTerms(comfy.RawWorkflow); !slices.Equal(got, want) {
		t.Errorf("terms = %q, want %q", got, want)
	}
	if comfy := parseComfyWorkflow(`{"a": 1, "b": "two"}`); comfy != nil {
		t.Errorf("JSON naming no node = %+v, want nil", comfy)
	}
}

func TestExtractFromPNG_WithParameters(t *testing.T) {
	paramText := "a beautiful prompt\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 42, Model: mymodel"
	buf := makePNGWithTextChunk("parameters", paramText)
	dir := t.TempDir()
	path := dir + "/test_sd.png"
	mustWriteFile(t, path, buf, 0644)
	sd, _ := extractFromPNG(path)
	if sd == nil {
		t.Fatal("expected non-nil SDMetadata from PNG with parameters chunk")
	}
	if sd.Prompt != "a beautiful prompt" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
}

func TestExtractFromPNG_NoParameters(t *testing.T) {
	buf := makePNGWithTextChunk("other_key", "some value")
	dir := t.TempDir()
	path := dir + "/test_nosd.png"
	mustWriteFile(t, path, buf, 0644)
	sd, _ := extractFromPNG(path)
	if sd != nil {
		t.Error("expected nil for PNG without parameters chunk")
	}
}

func TestExtractComfyUI_WithWorkflow(t *testing.T) {
	workflow := `{"1": {"type": "KSampler", "inputs": {"seed": 42, "steps": 20, "cfg": 7.0, "sampler_name": "euler"}}, "2": {"type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "model.safetensors"}}}`
	buf := makePNGWithTextChunk("workflow", workflow)
	path := filepath.Join(t.TempDir(), "wf.png")
	mustWriteFile(t, path, buf, 0644)
	_, comfy := extractFromPNG(path)
	if comfy == nil {
		t.Fatal("expected non-nil ComfyUIMetadata from PNG with workflow chunk")
	}
	if comfy.Seed == nil || *comfy.Seed != 42 {
		t.Errorf("Seed = %v", comfy.Seed)
	}
}

func TestExtractComfyUI_NoWorkflow(t *testing.T) {
	buf := makePNGWithTextChunk("other", "value")
	path := filepath.Join(t.TempDir(), "no_wf.png")
	mustWriteFile(t, path, buf, 0644)
	_, comfy := extractFromPNG(path)
	if comfy != nil {
		t.Error("expected nil for PNG without workflow chunk")
	}
}

func TestExtractComfyUI_NotPNG(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "notpng.png")
	mustWriteFile(t, path, []byte("not a png"), 0644)
	_, got := extractFromPNG(path)
	if got != nil {
		t.Errorf("non-PNG data must yield nil ComfyUIMetadata, got %+v", got)
	}
}

func TestReadPNGTextChunks_InvalidMagic(t *testing.T) {
	_, err := readPNGTextChunks(bytes.NewReader([]byte("not a png file at all")))
	if err == nil {
		t.Error("expected error for non-PNG data")
	}
}

func TestReadPNGTextChunks_iTXt(t *testing.T) {
	buf := makePNGWithITXtChunk("Comment", "test value")
	chunks, err := readPNGTextChunks(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if chunks["Comment"] != "test value" {
		t.Errorf("iTXt chunk value = %q", chunks["Comment"])
	}
}

func TestReadPNGTextChunks_iTXtCompressed(t *testing.T) {
	chunks, err := readPNGTextChunks(bytes.NewReader(makePNGWithCompressedITXtChunk("parameters", "a prompt, steps: 20", 0)))
	if err != nil {
		t.Fatal(err)
	}
	if chunks["parameters"] != "a prompt, steps: 20" {
		t.Errorf("compressed iTXt value = %q", chunks["parameters"])
	}

	chunks, err = readPNGTextChunks(bytes.NewReader(makePNGWithCompressedITXtChunk("parameters", "a prompt", 7)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := chunks["parameters"]; ok {
		t.Errorf("unknown compression method stored %q", chunks["parameters"])
	}
}

func TestReadPNGTextChunks_zTXt(t *testing.T) {
	chunks, err := readPNGTextChunks(bytes.NewReader(makePNGWithZTXtChunk("parameters", "a prompt, steps: 20", 0)))
	if err != nil {
		t.Fatal(err)
	}
	if chunks["parameters"] != "a prompt, steps: 20" {
		t.Errorf("zTXt value = %q", chunks["parameters"])
	}

	chunks, err = readPNGTextChunks(bytes.NewReader(makePNGWithZTXtChunk("parameters", "a prompt", 7)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := chunks["parameters"]; ok {
		t.Errorf("unknown compression method stored %q", chunks["parameters"])
	}
}

func makePNGWithZTXtChunk(key, val string, method byte) []byte {
	return pngWithChunk("zTXt", slices.Concat([]byte(key), []byte{0, method}, zlibBytes(val)))
}

func makePNGWithCompressedITXtChunk(key, val string, method byte) []byte {
	return pngWithChunk("iTXt", slices.Concat([]byte(key), []byte{0, 1, method, 0, 0}, zlibBytes(val)))
}

func makePNGWithTextChunk(key, val string) []byte {
	return pngWithChunk("tEXt", slices.Concat([]byte(key), []byte{0}, []byte(val)))
}

func makePNGWithITXtChunk(key, val string) []byte {
	return pngWithChunk("iTXt", slices.Concat([]byte(key), []byte{0, 0, 0, 0, 0}, []byte(val)))
}

func pngWithChunk(typ string, data []byte) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 13)
	ihdr[3] = 1
	ihdr[7] = 1
	ihdr[8] = 8
	writeTestChunk(&buf, "IHDR", ihdr)
	if typ != "" {
		writeTestChunk(&buf, typ, data)
	}
	writeTestChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func zlibBytes(val string) []byte {
	var payload bytes.Buffer
	zw := zlib.NewWriter(&payload)
	if _, err := zw.Write([]byte(val)); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return payload.Bytes()
}

func writeTestChunk(w *bytes.Buffer, chunkType string, data []byte) {
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	w.Write(length)
	w.WriteString(chunkType)
	w.Write(data)
	w.Write([]byte{0, 0, 0, 0})
}

func makeJPEGWithUserComment(t *testing.T, comment string) string {
	t.Helper()
	return makeJPEGWithEXIFTag(t, 0x9286, comment)
}

func writeJPEGWithEXIF(t *testing.T, tiff []byte) string {
	t.Helper()

	exif := []byte{'E', 'x', 'i', 'f', 0, 0}
	exif = append(exif, tiff...)
	app1Len := uint16(len(exif) + 2)

	var jpeg bytes.Buffer
	jpeg.Write([]byte{0xFF, 0xD8})
	jpeg.Write([]byte{0xFF, 0xE1})
	mustBinaryWrite(t, &jpeg, binary.BigEndian, app1Len)
	jpeg.Write(exif)
	jpeg.Write([]byte{0xFF, 0xD9})

	tmp, err := os.CreateTemp(t.TempDir(), "*.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(jpeg.Bytes()); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	return tmp.Name()
}

func TestExtractSDFromJPEG_ParsesA1111UserComment(t *testing.T) {
	t.Parallel()
	params := "a beautiful landscape\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 12345, Model: v1-5"
	path := makeJPEGWithUserComment(t, params)

	sd := sdFromEXIF(decodeFileEXIF(path, "jpeg"))
	if sd == nil {
		t.Fatal("ASCII-typed UserComment with A1111 params must produce SDMetadata")
	}
	if sd.Prompt != "a beautiful landscape" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
	if sd.NegativePrompt != "ugly" {
		t.Errorf("NegativePrompt = %q", sd.NegativePrompt)
	}
	if sd.Steps == nil || *sd.Steps != 20 {
		t.Errorf("Steps = %v", sd.Steps)
	}
	if sd.Sampler != "Euler" {
		t.Errorf("Sampler = %q", sd.Sampler)
	}
	if sd.Seed == nil || *sd.Seed != 12345 {
		t.Errorf("Seed = %v", sd.Seed)
	}
}

func makeWebPWithExifChunk(t *testing.T, includeMagic bool, comment string) string {
	t.Helper()

	var tiff bytes.Buffer
	tiff.WriteString("II")
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(0x002A))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint32(8))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(1))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(0x9286))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(2))
	payload := append([]byte(comment), 0)
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint32(len(payload)))
	dataOffset := uint32(8 + 2 + 12 + 4)
	mustBinaryWrite(t, &tiff, binary.LittleEndian, dataOffset)
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint32(0))
	tiff.Write(payload)
	return writeWebPWithEXIF(t, tiff.Bytes(), includeMagic)
}

func writeWebPWithEXIF(t *testing.T, tiff []byte, includeMagic bool) string {
	t.Helper()
	exifChunk := tiff
	if includeMagic {
		exifChunk = append([]byte("Exif\x00\x00"), exifChunk...)
	}

	var inner bytes.Buffer
	inner.WriteString("WEBP")
	inner.WriteString("EXIF")
	mustBinaryWrite(t, &inner, binary.LittleEndian, uint32(len(exifChunk)))
	inner.Write(exifChunk)
	if len(exifChunk)%2 == 1 {
		inner.WriteByte(0)
	}

	var webp bytes.Buffer
	webp.WriteString("RIFF")
	mustBinaryWrite(t, &webp, binary.LittleEndian, uint32(inner.Len()))
	webp.Write(inner.Bytes())

	tmp, err := os.CreateTemp(t.TempDir(), "*.webp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(webp.Bytes()); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	return tmp.Name()
}

func TestExtractSDFromWebP_ParsesA1111UserComment(t *testing.T) {
	t.Parallel()
	params := "a beautiful landscape\nNegative prompt: ugly\nSteps: 30, Sampler: Euler, CFG scale: 7, Seed: 42, Model: webp_v1"

	for _, includeMagic := range []bool{false, true} {
		t.Run(fmt.Sprintf("magic=%v", includeMagic), func(t *testing.T) {
			t.Parallel()
			path := makeWebPWithExifChunk(t, includeMagic, params)
			sd := sdFromEXIF(decodeFileEXIF(path, "webp"))
			if sd == nil {
				t.Fatal("WebP A1111 EXIF must parse")
			}
			if sd.Prompt != "a beautiful landscape" {
				t.Errorf("Prompt = %q", sd.Prompt)
			}
			if sd.Steps == nil || *sd.Steps != 30 {
				t.Errorf("Steps = %v", sd.Steps)
			}
			if sd.Seed == nil || *sd.Seed != 42 {
				t.Errorf("Seed = %v", sd.Seed)
			}
		})
	}
}

func piexifUserComment(t *testing.T, order binary.ByteOrder, comment []byte) []byte {
	t.Helper()
	var tiff bytes.Buffer
	if order == binary.BigEndian {
		tiff.WriteString("MM")
	} else {
		tiff.WriteString("II")
	}
	mustBinaryWrite(t, &tiff, order, uint16(0x002A))
	mustBinaryWrite(t, &tiff, order, uint32(8))
	subIFD := uint32(8 + 2 + 12 + 4)
	mustBinaryWrite(t, &tiff, order, uint16(1))
	mustBinaryWrite(t, &tiff, order, uint16(0x8769))
	mustBinaryWrite(t, &tiff, order, uint16(4))
	mustBinaryWrite(t, &tiff, order, uint32(1))
	mustBinaryWrite(t, &tiff, order, subIFD)
	mustBinaryWrite(t, &tiff, order, uint32(0))
	mustBinaryWrite(t, &tiff, order, uint16(1))
	mustBinaryWrite(t, &tiff, order, uint16(0x9286))
	mustBinaryWrite(t, &tiff, order, uint16(7))
	mustBinaryWrite(t, &tiff, order, uint32(len(comment)))
	mustBinaryWrite(t, &tiff, order, subIFD+2+12+4)
	mustBinaryWrite(t, &tiff, order, uint32(0))
	tiff.Write(comment)
	return tiff.Bytes()
}

func unicodeComment(order binary.ByteOrder, bom bool, text string) []byte {
	units := utf16.Encode([]rune(text))
	if bom {
		units = append([]uint16{0xFEFF}, units...)
	}
	out := []byte("UNICODE\x00")
	for _, u := range units {
		var b [2]byte
		order.PutUint16(b[:], u)
		out = append(out, b[:]...)
	}
	return out
}

const piexifParams = "a castle \U0001F3F0 at dusk\nNegative prompt: blurry\nSteps: 28, Sampler: DPM++ 2M, CFG scale: 6.5, Seed: 777, Model: forge_xl"

func wantPiexifParams(t *testing.T, sd *models.SDMetadata) {
	t.Helper()
	if sd == nil {
		t.Fatal("the UserComment produced no SDMetadata")
	}
	if sd.Prompt != "a castle \U0001F3F0 at dusk" || sd.Seed == nil || *sd.Seed != 777 {
		t.Errorf("sd = prompt %q seed %v", sd.Prompt, sd.Seed)
	}
}

func TestExtractSDFromJPEG_ParsesUnicodeUserComment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		order binary.ByteOrder
		bom   bool
	}{
		{"big-endian, as piexif writes it", binary.BigEndian, false},
		{"little-endian with a BOM", binary.LittleEndian, true},
		{"little-endian without a BOM", binary.LittleEndian, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeJPEGWithEXIF(t, piexifUserComment(t, binary.BigEndian, unicodeComment(tc.order, tc.bom, piexifParams)))
			sd := sdFromEXIF(decodeFileEXIF(path, "jpeg"))
			wantPiexifParams(t, sd)
		})
	}
}

func TestExtractSDFromJPEG_ParsesUndefinedASCIIUserComment(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"ASCII\x00\x00\x00", "\x00\x00\x00\x00\x00\x00\x00\x00"} {
		path := writeJPEGWithEXIF(t, piexifUserComment(t, binary.BigEndian, []byte(prefix+piexifParams)))
		sd := sdFromEXIF(decodeFileEXIF(path, "jpeg"))
		wantPiexifParams(t, sd)
	}
}

func TestExtractSDFromWebP_ParsesUnicodeUserComment(t *testing.T) {
	t.Parallel()
	path := writeWebPWithEXIF(t, piexifUserComment(t, binary.BigEndian, unicodeComment(binary.BigEndian, false, piexifParams)), true)
	wantPiexifParams(t, sdFromEXIF(decodeFileEXIF(path, "webp")))
}

func TestExtractSDFromJPEG_UnknownCharsetYieldsNothing(t *testing.T) {
	t.Parallel()
	path := writeJPEGWithEXIF(t, piexifUserComment(t, binary.BigEndian, []byte("JIS\x00\x00\x00\x00\x00"+piexifParams)))
	sd := sdFromEXIF(decodeFileEXIF(path, "jpeg"))
	if sd != nil {
		t.Errorf("a JIS UserComment is not decoded, got %+v", sd)
	}
}

func TestReadPNGTextChunks_TooShort(t *testing.T) {
	_, err := readPNGTextChunks(bytes.NewReader([]byte{0x89, 0x50}))
	if err == nil {
		t.Error("expected error for too-short input")
	}
}

func TestReadPNGTextChunks_TruncatedChunkData(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	header := make([]byte, 8)
	header[0], header[1], header[2], header[3] = 0, 0, 0, 100
	copy(header[4:], "tEXt")
	buf.Write(header)
	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("truncated chunk must not surface error, got %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("truncated chunk must not emit entries, got %v", chunks)
	}
}

func TestReadPNGTextChunks_tEXtWithoutNull(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 13)
	ihdr[3] = 1
	ihdr[7] = 1
	ihdr[8] = 8
	writeTestChunk(&buf, "IHDR", ihdr)
	writeTestChunk(&buf, "tEXt", []byte("keywordwithnonull"))
	writeTestChunk(&buf, "IEND", nil)

	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected no chunks for tEXt without null, got %v", chunks)
	}
}

func TestReadPNGTextChunks_iTXtTooShort(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 13)
	ihdr[3] = 1
	ihdr[7] = 1
	ihdr[8] = 8
	writeTestChunk(&buf, "IHDR", ihdr)
	writeTestChunk(&buf, "iTXt", []byte("keyword\x00X"))
	writeTestChunk(&buf, "IEND", nil)

	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("malformed iTXt must not surface error, got %v", err)
	}
	if _, ok := chunks["keyword"]; ok {
		t.Errorf("short iTXt must not emit an entry, got %v", chunks)
	}
}

func TestParseA1111_NegativeOnly(t *testing.T) {
	input := "positive prompt\nNegative prompt: some bad stuff"
	sd := parseA1111Parameters(input)
	if sd == nil {
		t.Fatal("expected non-nil")
	}
	if sd.NegativePrompt != "some bad stuff" {
		t.Errorf("NegativePrompt = %q", sd.NegativePrompt)
	}
}

func TestParseAllSDParams_KeepsAQuotedValueWhole(t *testing.T) {
	line := `Steps: 20, Template: "a, b: c", Lora hashes: "detail: 0123abcd, style: 4567ef01", ` +
		`Hashes: {"model": "abc", "vae": "d, e"}, Prompt note: "say \"hi, there\"", Height: 5" tall, Seed: 1`

	got := ParseAllSDParams(line)

	want := []models.SDParam{
		{Key: "Steps", Val: "20"},
		{Key: "Template", Val: "a, b: c"},
		{Key: "Lora hashes", Val: "detail: 0123abcd, style: 4567ef01"},
		{Key: "Hashes", Val: `{"model": "abc", "vae": "d, e"}`},
		{Key: "Prompt note", Val: `say "hi, there"`},
		{Key: "Height", Val: `5" tall`},
		{Key: "Seed", Val: "1"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("params =\n%q\nwant\n%q", got, want)
	}
}

func TestParseA1111_LoraHashesReachTheModelWhole(t *testing.T) {
	input := "a cat\nNegative prompt: ugly\n" +
		`Steps: 20, Sampler: Euler a, Model: base, Lora hashes: "detail: 0123abcd, style: 4567ef01", TI hashes: "bad: 89ab, worse: cdef", Version: v1.10.1`

	sd := parseA1111Parameters(input)

	if sd == nil {
		t.Fatal("expected non-nil")
	}
	if want := "base | LoRA: detail: 0123abcd, style: 4567ef01"; sd.Model != want {
		t.Errorf("Model = %q, want %q", sd.Model, want)
	}
	for _, p := range sd.ParsedParams {
		if p.Key == "style" || p.Key == "worse" {
			t.Errorf("a quoted value's inner key %q read as a parameter", p.Key)
		}
	}
}

func TestParseA1111_EmptyPromptKeepsTheNegativeApart(t *testing.T) {
	input := "Negative prompt: ugly, blurry\nSteps: 20, Sampler: Euler a, Seed: 7"

	sd := parseA1111Parameters(input)

	if sd == nil {
		t.Fatal("expected non-nil")
	}
	if sd.Prompt != "" || sd.NegativePrompt != "ugly, blurry" {
		t.Errorf("Prompt = %q, NegativePrompt = %q; want empty and %q", sd.Prompt, sd.NegativePrompt, "ugly, blurry")
	}
	if sd.Steps == nil || *sd.Steps != 20 {
		t.Errorf("Steps = %v", sd.Steps)
	}
}

func makeWebPWithMakeModel(t *testing.T, makeTag, modelTag string) string {
	t.Helper()
	order := binary.BigEndian
	makeVal, modelVal := append([]byte(makeTag), 0), append([]byte(modelTag), 0)
	const dataStart = 8 + 2 + 2*12 + 4
	entries := [][]byte{
		entryBytes(order, 0x010F, exifASCII, uint32(len(makeVal)), order.AppendUint32(nil, dataStart)),
		entryBytes(order, 0x0110, exifASCII, uint32(len(modelVal)), order.AppendUint32(nil, dataStart+uint32(len(makeVal)))),
	}
	return writeWebPWithEXIF(t, tiffBlock(order, entries, 0, append(makeVal, modelVal...)), true)
}

func TestExtract_ComfyUIWebPEXIF(t *testing.T) {
	t.Parallel()
	path := makeWebPWithMakeModel(t, "workflow:"+savedNodeGraph, "prompt:"+randomNoiseWorkflow)
	sd, comfy := Extract(path, "webp")
	if sd != nil || comfy == nil || comfy.ModelCheckpoint != "flux.safetensors" ||
		comfy.RawWorkflow != sanitizeComfyJSON(strings.TrimSpace(randomNoiseWorkflow)) {
		t.Fatalf("got sd %+v, comfy %+v, want the Model tag's prompt graph", sd, comfy)
	}
	for _, p := range ExtractGeneric(path, "webp") {
		if p.Key == "Make" || p.Key == "Model" {
			t.Errorf("Other metadata repeats the %s tag ComfyUI's parser read", p.Key)
		}
	}

	path = makeWebPWithMakeModel(t, "workflow:"+savedNodeGraph, "")
	if _, comfy := Extract(path, "webp"); comfy == nil || len(ParseComfyWorkflowNodes(comfy.RawWorkflow)) != 3 {
		t.Errorf("workflow alone: %+v, want the node graph", comfy)
	}
}

func TestExtract_CameraMakeModelIsNotComfyUI(t *testing.T) {
	t.Parallel()
	path := makeWebPWithMakeModel(t, "Canon", "EOS R5")
	if _, comfy := Extract(path, "webp"); comfy != nil {
		t.Errorf("a camera's Make and Model read as ComfyUI: %+v", comfy)
	}
	got := map[string]string{}
	for _, p := range ExtractGeneric(path, "webp") {
		got[p.Key] = p.Val
	}
	if !strings.Contains(got["Make"], "Canon") || !strings.Contains(got["Model"], "EOS R5") {
		t.Errorf("Other metadata = %+v, want the camera's Make and Model", got)
	}
}

func TestExtractSDFromWebP_NoEXIF(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, 4)
	buf.Write(size)
	buf.WriteString("WEBP")
	dir := t.TempDir()
	path := dir + "/empty.webp"
	mustWriteFile(t, path, buf.Bytes(), 0o644)
	sd, _ := Extract(path, "webp")
	if sd != nil {
		t.Error("expected nil SDMetadata for WebP without EXIF chunk")
	}
}

func TestExtractSDFromWebP_NotWebP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/junk.webp"
	mustWriteFile(t, path, []byte("not a webp at all"), 0o644)
	sd, _ := Extract(path, "webp")
	if sd != nil {
		t.Error("expected nil for non-WebP data")
	}
}

type maxReadRecorder struct {
	r   io.Reader
	max int
}

func (m *maxReadRecorder) Read(p []byte) (int, error) {
	m.max = max(m.max, len(p))
	return m.r.Read(p)
}

func TestReadWebPEXIF_SkipsChunksBeforeEXIF(t *testing.T) {
	t.Parallel()
	const imageSize = 1<<20 + 1
	var inner bytes.Buffer
	inner.WriteString("WEBP")
	inner.WriteString("VP8 ")
	mustBinaryWrite(t, &inner, binary.LittleEndian, uint32(imageSize))
	inner.Write(make([]byte, imageSize+1))
	inner.WriteString("EXIF")
	mustBinaryWrite(t, &inner, binary.LittleEndian, uint32(4))
	inner.WriteString("II*\x00")
	var webp bytes.Buffer
	webp.WriteString("RIFF")
	mustBinaryWrite(t, &webp, binary.LittleEndian, uint32(inner.Len()))
	webp.Write(inner.Bytes())

	rec := &maxReadRecorder{r: &webp}
	got := readWebPEXIF(rec)
	if string(got) != "II*\x00" {
		t.Fatalf("readWebPEXIF = %q, want the EXIF payload", got)
	}
	if rec.max >= imageSize {
		t.Errorf("largest read %d bytes: the image chunk was buffered", rec.max)
	}
}

func makeJPEGWithEXIFTag(t *testing.T, tagID uint16, value string) string {
	t.Helper()
	payload := append([]byte(value), 0)

	entry := entryBytes(binary.LittleEndian, tagID, exifASCII, uint32(len(payload)), binary.LittleEndian.AppendUint32(nil, 8+2+12+4))
	return writeJPEGWithEXIF(t, tiffBlock(binary.LittleEndian, [][]byte{entry}, 0, payload))
}

func TestExtractGeneric_JPEGEXIFTags(t *testing.T) {
	t.Parallel()

	path := makeJPEGWithEXIFTag(t, 0x010E, "a scenic photo")

	params := ExtractGeneric(path, "jpeg")
	got := map[string]string{}
	for _, p := range params {
		got[p.Key] = p.Val
	}
	val, ok := got["ImageDescription"]
	if !ok {
		t.Fatalf("expected an ImageDescription row, got %+v", params)
	}

	if !strings.Contains(val, "a scenic photo") {
		t.Errorf("ImageDescription = %q, want it to contain %q", val, "a scenic photo")
	}
}

func TestExtractGeneric_JPEGNumericTag(t *testing.T) {
	t.Parallel()
	var tiff bytes.Buffer
	tiff.WriteString("II")
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(0x002A))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint32(8))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(1))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(0x0112))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(3))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint32(1))

	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(6))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint16(0))
	mustBinaryWrite(t, &tiff, binary.LittleEndian, uint32(0))

	exif := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	app1Len := make([]byte, 2)
	binary.BigEndian.PutUint16(app1Len, uint16(len(exif)+2))
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE1}, app1Len...)
	jpeg = append(jpeg, exif...)
	jpeg = append(jpeg, 0xFF, 0xD9)

	path := filepath.Join(t.TempDir(), "orient.jpg")
	mustWriteFile(t, path, jpeg, 0o644)

	params := ExtractGeneric(path, "jpeg")
	got := map[string]string{}
	for _, p := range params {
		got[p.Key] = p.Val
	}
	val, ok := got["Orientation"]
	if !ok {
		t.Fatalf("expected an Orientation row, got %+v", params)
	}
	if !strings.Contains(val, "6") {
		t.Errorf("Orientation = %q, want it to contain the integer 6", val)
	}
}

func TestExtractGeneric_JPEGSkipsUserComment(t *testing.T) {
	t.Parallel()
	path := makeJPEGWithUserComment(t, "some prompt text")
	params := ExtractGeneric(path, "jpeg")
	for _, p := range params {
		if p.Key == "UserComment" {
			t.Errorf("UserComment must be excluded from the generic panel, got %+v", params)
		}
	}
}

func TestExtractGeneric_PNGTextChunks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	authorPath := dir + "/author.png"
	mustWriteFile(t, authorPath, makePNGWithTextChunk("Author", "Jane Doe"), 0o644)
	params := ExtractGeneric(authorPath, "png")
	if len(params) != 1 || params[0].Key != "Author" || params[0].Val != "Jane Doe" {
		t.Fatalf("expected a single Author=Jane Doe row, got %+v", params)
	}

	for _, skip := range []string{"parameters", "prompt", "workflow"} {
		p := dir + "/" + skip + ".png"
		mustWriteFile(t, p, makePNGWithTextChunk(skip, "ignored"), 0o644)
		if got := ExtractGeneric(p, "png"); len(got) != 0 {
			t.Errorf("%q chunk must be skipped by the generic panel, got %+v", skip, got)
		}
	}
}

func TestExtractGeneric_MalformedNoPanic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	garbage := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	exif := append([]byte("Exif\x00\x00"), bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x12}, 16)...)
	app1Len := make([]byte, 2)
	binary.BigEndian.PutUint16(app1Len, uint16(len(exif)+2))
	garbage = append(garbage, app1Len...)
	garbage = append(garbage, exif...)
	garbage = append(garbage, 0xFF, 0xD9)

	var badTiff bytes.Buffer
	badTiff.WriteString("II")
	_ = binary.Write(&badTiff, binary.LittleEndian, uint16(0x002A))
	_ = binary.Write(&badTiff, binary.LittleEndian, uint32(0xFFFFFFFF))
	truncExif := append([]byte("Exif\x00\x00"), badTiff.Bytes()...)
	truncLen := make([]byte, 2)
	binary.BigEndian.PutUint16(truncLen, uint16(len(truncExif)+2))
	truncJPEG := append([]byte{0xFF, 0xD8, 0xFF, 0xE1}, truncLen...)
	truncJPEG = append(truncJPEG, truncExif...)
	truncJPEG = append(truncJPEG, 0xFF, 0xD9)

	cases := []struct {
		name     string
		fileType string
		data     []byte
	}{
		{"jpeg_garbage_exif", "jpeg", garbage},
		{"jpeg_truncated_tiff", "jpeg", truncJPEG},
		{"jpeg_no_exif", "jpeg", []byte{0xFF, 0xD8, 0xFF, 0xD9}},
		{"jpeg_not_a_jpeg", "jpeg", []byte("totally not a jpeg")},
		{"jpeg_empty", "jpeg", nil},
		{"png_no_text", "png", makeBarePNG()},
		{"png_not_a_png", "png", []byte("not a png at all")},
		{"webp_garbage_exif", "webp", makeWebPGarbageEXIF()},
		{"webp_not_a_webp", "webp", []byte("RIFFnope")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), tc.name)
			mustWriteFile(t, path, tc.data, 0o644)
			params := ExtractGeneric(path, tc.fileType)
			if len(params) != 0 {
				t.Errorf("malformed %s must emit no generic rows, got %+v", tc.name, params)
			}
		})
	}

	if got := ExtractGeneric(dir+"/does-not-exist.jpg", "jpeg"); len(got) != 0 {
		t.Errorf("missing file must yield no rows, got %+v", got)
	}
	if got := ExtractGeneric(dir+"/x.mp4", "mp4"); got != nil {
		t.Errorf("unsupported type must yield nil, got %+v", got)
	}
}

func makeBarePNG() []byte {
	return pngWithChunk("", nil)
}

func makeWebPGarbageEXIF() []byte {
	exifChunk := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 8)
	var inner bytes.Buffer
	inner.WriteString("WEBP")
	inner.WriteString("EXIF")
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(len(exifChunk)))
	inner.Write(size)
	inner.Write(exifChunk)
	if len(exifChunk)%2 == 1 {
		inner.WriteByte(0)
	}
	var webp bytes.Buffer
	webp.WriteString("RIFF")
	riffSize := make([]byte, 4)
	binary.LittleEndian.PutUint32(riffSize, uint32(inner.Len()))
	webp.Write(riffSize)
	webp.Write(inner.Bytes())
	return webp.Bytes()
}

func TestExtractGeneric_WebPEXIFTags(t *testing.T) {
	t.Parallel()
	var tiff bytes.Buffer
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x002A))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x013B))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
	payload := append([]byte("A. Photographer"), 0)
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(len(payload)))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8+2+12+4))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	tiff.Write(payload)

	exifChunk := tiff.Bytes()
	var inner bytes.Buffer
	inner.WriteString("WEBP")
	inner.WriteString("EXIF")
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(len(exifChunk)))
	inner.Write(size)
	inner.Write(exifChunk)
	if len(exifChunk)%2 == 1 {
		inner.WriteByte(0)
	}
	var webp bytes.Buffer
	webp.WriteString("RIFF")
	riffSize := make([]byte, 4)
	binary.LittleEndian.PutUint32(riffSize, uint32(inner.Len()))
	webp.Write(riffSize)
	webp.Write(inner.Bytes())

	path := filepath.Join(t.TempDir(), "artist.webp")
	mustWriteFile(t, path, webp.Bytes(), 0o644)

	params := ExtractGeneric(path, "webp")
	got := map[string]string{}
	for _, p := range params {
		got[p.Key] = p.Val
	}
	val, ok := got["Artist"]
	if !ok {
		t.Fatalf("expected an Artist row from WebP EXIF, got %+v", params)
	}
	if !strings.Contains(val, "A. Photographer") {
		t.Errorf("Artist = %q, want it to contain %q", val, "A. Photographer")
	}
}

func TestExtractGeneric_WebPDropsAnOversizedArray(t *testing.T) {
	t.Parallel()
	le := binary.LittleEndian
	const count = 40000
	make4 := []byte("Cam\x00")
	arrayOffset := make([]byte, 4)
	le.PutUint32(arrayOffset, 8+2+2*12+4)
	tiff := tiffBlock(le, [][]byte{
		entryBytes(le, 0x010F, 2, 4, make4),
		entryBytes(le, 0x9C9E, 3, count, arrayOffset),
	}, 0, make([]byte, 2*count))
	params := ExtractGeneric(writeWebPWithEXIF(t, tiff, false), "webp")
	got := map[string]bool{}
	for _, p := range params {
		got[p.Key] = true
	}
	if !got["Make"] || got["XPKeywords"] {
		t.Errorf("keys %v: want Make kept and the %d-byte XPKeywords array dropped", got, 2*count)
	}
}

func makeJPEGWithRawIFD0(t *testing.T, tag, typ uint16, count, value uint32) string {
	t.Helper()
	entry := entryBytes(binary.LittleEndian, tag, typ, count, binary.LittleEndian.AppendUint32(nil, value))
	return writeJPEGWithEXIF(t, tiffBlock(binary.LittleEndian, [][]byte{entry}, 0, nil))
}

func TestExtract_JPEG_OverflowingTagCount(t *testing.T) {
	t.Parallel()
	path := makeJPEGWithRawIFD0(t, 0x9286, 4, 0x40000001, 26)

	sd, comfy := Extract(path, "jpeg")
	if sd != nil || comfy != nil {
		t.Errorf("crafted count must yield no metadata, got sd=%+v comfy=%+v", sd, comfy)
	}
	if params := ExtractGeneric(path, "jpeg"); len(params) != 0 {
		t.Errorf("crafted count must yield no generic params, got %+v", params)
	}
}

func TestExtract_JPEG_OversizedTagCount(t *testing.T) {
	t.Parallel()
	path := makeJPEGWithRawIFD0(t, 0x9286, 2, 1_000_000, 26)

	sd, _ := Extract(path, "jpeg")
	if sd != nil {
		t.Errorf("oversized count must yield no metadata, got %+v", sd)
	}
}

func TestJPEGEXIF_FindsBlockAndRejectsBadCount(t *testing.T) {
	t.Parallel()
	path := makeJPEGWithRawIFD0(t, 0x9286, 4, 0x40000001, 26)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	block := jpegEXIF(f)
	if len(block) == 0 {
		t.Fatal("APP1 walk found no EXIF block")
	}
	if _, err := decodeEXIF(block); err == nil {
		t.Error("a count wrapping uint32 must fail the decode")
	}
}

func TestDecodeEXIF_AcceptsWellFormed(t *testing.T) {
	t.Parallel()
	path := makeJPEGWithUserComment(t, "Steps: 20, Sampler: Euler")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	block := jpegEXIF(f)
	if _, err := decodeEXIF(block); err != nil {
		t.Errorf("well-formed EXIF block failed to decode (%d bytes): %v", len(block), err)
	}
}

func mustBinaryWrite(t *testing.T, w io.Writer, order binary.ByteOrder, data any) {
	t.Helper()
	if err := binary.Write(w, order, data); err != nil {
		t.Fatalf("binary write: %v", err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustParseComicInfo(t *testing.T, zr *zip.Reader) *models.MangaMetadata {
	t.Helper()
	meta, err := ParseComicInfo(zr)
	if err != nil {
		t.Fatalf("ParseComicInfo: %v", err)
	}
	return meta
}

func TestReadPNGTextChunks_BoundsTheTextAFileKeeps(t *testing.T) {
	var payload bytes.Buffer
	zw := zlib.NewWriter(&payload)
	if _, err := zw.Write(bytes.Repeat([]byte("a"), 16<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	for i := range 9 {
		var data bytes.Buffer
		fmt.Fprintf(&data, "key%d", i)
		data.Write([]byte{0, 0})
		data.Write(payload.Bytes())
		writeTestChunk(&buf, "zTXt", data.Bytes())
	}
	writeTestChunk(&buf, "IEND", nil)

	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for k, v := range chunks {
		kept += len(k) + len(v)
	}
	if kept > maxPNGTextBytes {
		t.Errorf("kept %d bytes of text, want at most %d", kept, maxPNGTextBytes)
	}
}

func TestParseA1111_SeedPastInt64(t *testing.T) {
	sd := parseA1111Parameters("a cat\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 18446744073709551615, Model: foo")
	if sd == nil || sd.Seed == nil {
		t.Fatal("Seed unset")
	}
	if *sd.Seed != -1 {
		t.Fatalf("Seed = %d, want the two's-complement -1", *sd.Seed)
	}
	sd = parseA1111Parameters("a cat\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 18446744073709551616, Model: foo")
	if sd == nil || sd.Seed != nil {
		t.Fatalf("a seed past uint64 should stay unset, got %v", *sd.Seed)
	}
}

func TestParseComfyPromptChunk_SeedPastInt64(t *testing.T) {
	cases := map[string]string{
		"KSampler literal": `{
			"1": {"class_type": "CLIPTextEncode", "inputs": {"text": "a prompt"}},
			"2": {"class_type": "KSampler", "inputs": {"seed": 18446744073709551615, "steps": 20}}
		}`,
		"KSampler reference": `{
			"1": {"class_type": "CLIPTextEncode", "inputs": {"text": "a prompt"}},
			"2": {"class_type": "KSampler", "inputs": {"seed": ["3", 0], "steps": 20}},
			"3": {"class_type": "PrimitiveNode", "inputs": {"seed": 18446744073709551615}}
		}`,
		"seed node": `{
			"1": {"class_type": "CLIPTextEncode", "inputs": {"text": "a prompt"}},
			"2": {"class_type": "Seed (rgthree)", "inputs": {"seed": 18446744073709551615}}
		}`,
	}
	for name, raw := range cases {
		comfy := parseComfyPromptChunk(raw)
		switch {
		case comfy == nil || comfy.Seed == nil:
			t.Errorf("%s: Seed unset", name)
		case *comfy.Seed != -1:
			t.Errorf("%s: Seed = %d, want the two's-complement -1", name, *comfy.Seed)
		}
	}
}

func TestEveryGeneratorIsAnAIFilterValue(t *testing.T) {
	for _, p := range pngTextParsers {
		if !slices.Contains(searchkw.Expansions["ai"], p.Source) {
			t.Errorf("chunk %q fills source %q, which ai: does not offer", p.Key, p.Source)
		}
	}
}
