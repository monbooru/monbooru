package metadata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const randomNoiseWorkflow = `{
	"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "flux.safetensors"}},
	"2": {"class_type": "RandomNoise", "inputs": {"noise_seed": 42}},
	"3": {"class_type": "SamplerCustomAdvanced", "inputs": {"noise": ["2", 0]}}
}`

func TestNodeOverlayAddsAClass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comfyui_nodes.json")
	SetNodeOverlay(path)
	t.Cleanup(func() { SetNodeOverlay("") })

	if m := parseComfyPromptChunk(randomNoiseWorkflow); m == nil || m.Seed != nil {
		t.Fatalf("without an overlay: %+v, want no seed", m)
	}

	writeOverlay(t, path, `{"version": 1, "nodes": {"RandomNoise": {"seed": "noise_seed"}}}`)
	m := parseComfyPromptChunk(randomNoiseWorkflow)
	if m == nil || m.Seed == nil || *m.Seed != 42 || m.ModelCheckpoint != "flux.safetensors" {
		t.Fatalf("with the overlay: %+v, want seed 42 and the shipped model column", m)
	}

	writeOverlay(t, path, `{"version": 1, "nodes": {"RandomNoise": {}}}`)
	if m := parseComfyPromptChunk(randomNoiseWorkflow); m.Seed != nil {
		t.Errorf("an edited overlay was not read again: seed %v", *m.Seed)
	}

	writeOverlay(t, path, `{"version": 1, "nodes": `)
	if m := parseComfyPromptChunk(randomNoiseWorkflow); m == nil || m.ModelCheckpoint != "flux.safetensors" {
		t.Errorf("a broken overlay should leave the shipped table: %+v", m)
	}
}

var overlayWrites int

func writeOverlay(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayWrites++
	stamp := time.Now().Add(time.Duration(overlayWrites) * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestASecondSamplerAddsNoScheduler(t *testing.T) {
	m := parseComfyPromptChunk(`{
		"1": {"class_type": "KSampler", "inputs": {"seed": 1, "steps": 20, "cfg": 7, "sampler_name": "euler", "scheduler": "normal"}},
		"2": {"class_type": "KSampler", "inputs": {"seed": 2, "steps": 10, "cfg": 5, "sampler_name": "dpmpp_2m", "scheduler": "karras"}}
	}`)
	if m == nil || m.Sampler != "euler/normal" {
		t.Errorf("sampler = %q, want the first node's euler/normal alone", m.Sampler)
	}
}

func TestKSamplerAdvancedReadsItsNoiseSeed(t *testing.T) {
	m := parseComfyPromptChunk(`{
		"1": {"class_type": "KSamplerAdvanced", "inputs": {"add_noise": "enable", "noise_seed": 77, "steps": 30, "cfg": 6, "sampler_name": "euler", "scheduler": "karras"}}
	}`)
	if m == nil || m.Seed == nil || *m.Seed != 77 || m.Sampler != "euler/karras" {
		t.Errorf("got %+v, want seed 77 and euler/karras", m)
	}
}
