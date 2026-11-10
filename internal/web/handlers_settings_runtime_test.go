//go:build tagger

package web

import (
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
