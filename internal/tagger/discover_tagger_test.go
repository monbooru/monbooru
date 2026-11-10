//go:build tagger

package tagger

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestEnabledTaggers_FiltersUnavailable(t *testing.T) {
	stubRuntimeLibrary(t)
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "ok", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "broken", []string{"model.onnx"})

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := EnabledTaggers(cfg)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Errorf("EnabledTaggers = %+v, want only `ok`", got)
	}
}

func TestDiscoverTaggers_MissingRuntimeLibrary(t *testing.T) {
	t.Setenv("ORT_LIB_PATH", filepath.Join(t.TempDir(), "absent.so"))
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "ok", []string{"model.onnx", "tags.csv"})

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := DiscoverTaggers(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Available {
		t.Error("a complete folder is still unusable without the library")
	}
	if !strings.Contains(got[0].Reason, "absent.so") {
		t.Errorf("reason = %q, want the missing library named", got[0].Reason)
	}
	if len(EnabledTaggers(cfg)) != 0 {
		t.Error("a tagger that cannot run must not be handed to a job")
	}
	if _, present, _ := GalleryTaggers(cfg, "default"); !present {
		t.Error("present should stay true: one is set up, it just cannot run")
	}
}
