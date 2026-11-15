//go:build tagger

package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestSettingsKeepsTaggersEnabledWithoutTheRuntime(t *testing.T) {
	srv, modelPath := newModelServer(t, config.TaggerInstance{Name: "wd14", Enabled: true})
	installTagger(t, modelPath, "wd14", "tag_id,name,category\n0,sample,0\n")
	t.Setenv("ORT_LIB_PATH", filepath.Join(t.TempDir(), "absent.so"))

	getBody(t, srv, "/settings")
	if !taggerEnabled(srv, "wd14") {
		t.Error("a missing runtime library disabled the tagger")
	}
}

func TestSettingsShowsAMalformedTaggerFolderWithoutActions(t *testing.T) {
	srv, modelPath := newModelServer(t)
	installTagger(t, modelPath, "wd-v1.4-moat", "tag_id,name,category\n1,engrish,0\n")

	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, "folder name must match [A-Za-z0-9_-]&#43;; rename the folder")
	for _, action := range []string{"/settings/tagger/wd-v1.4-moat/", `id="tagger-config-wd-v1.4-moat"`} {
		wantNotIn(t, body, action)
	}
}

func TestSettingsOffersTheFilesACopiedRegistryFolderLacks(t *testing.T) {
	srv, modelPath := newModelServer(t)
	src := filepath.Join("..", "registry", "plugins", "taggers", "camie-v2")
	dir := filepath.Join(modelPath, "camie-v2")
	mustMkdirAll(t, dir, 0o755)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(dir, e.Name()), data, 0o644)
	}

	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, `data-tagger-name="camie-v2"`)
	wantIn(t, body, "Anime tagger with artist/copyright recognition")
	wantIn(t, body, "camie-tagger-v2.onnx")
	wantNotIn(t, body, "taggers/camie-v2/profile_default.json")

	mustWriteFile(t, filepath.Join(dir, "camie-tagger-v2.onnx"), []byte("weights"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "camie-tagger-v2-metadata.json"), []byte(`{}`), 0o644)
	_, body = getBody(t, srv, "/settings")
	wantNotIn(t, body, `data-tagger-name="camie-v2"`)
}
