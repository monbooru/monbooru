package web

import (
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestSettingsGalleryDefaultPost_PersistsTheKey(t *testing.T) {
	srv := newMultiGalleryServer(t)
	if got := srv.cfgSnapshot().DefaultGallery; got != "default" {
		t.Fatalf("test premise broken: default_gallery starts at %q", got)
	}
	wasActive := srv.set.ActiveName()

	w := postForm(t, srv, "/settings/galleries/stock/default", url.Values{})
	mustCode(t, w, http.StatusOK)

	if got := srv.cfgSnapshot().DefaultGallery; got != "stock" {
		t.Errorf("default_gallery = %q, want stock", got)
	}
	if got := srv.set.ActiveName(); got != wasActive {
		t.Errorf("active gallery moved to %q; setting a default does not switch", got)
	}

	reloaded, err := config.LoadWithDefaults(srv.configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DefaultGallery != "stock" {
		t.Errorf("reloaded default_gallery = %q, want stock", reloaded.DefaultGallery)
	}
	toml, err := os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantIn(t, string(toml), `default_gallery = "stock"`)
}

func TestSettingsGalleryDefaultPost_RefusesAnUnknownGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)

	w := postForm(t, srv, "/settings/galleries/nope/default", url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "unknown gallery")
	wantIn(t, w.Body.String(), "nope")
	if got := srv.cfgSnapshot().DefaultGallery; got != "default" {
		t.Errorf("default_gallery = %q, want it unchanged", got)
	}
}
