package tagger

import (
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestSeedTaggerInstance_NoCatalog(t *testing.T) {
	got := SeedTaggerInstance("custom", true, nil)
	if got.Name != "custom" || !got.Enabled {
		t.Errorf("name/enabled wrong: %+v", got)
	}
	if got.ConfidenceThreshold != config.DefaultConfidenceThreshold {
		t.Errorf("threshold = %v, want package default %v", got.ConfidenceThreshold, config.DefaultConfidenceThreshold)
	}
	if got.CategoryThresholds != nil {
		t.Errorf("CategoryThresholds = %v, want nil", got.CategoryThresholds)
	}
}

func TestSeedTaggerInstance_CatalogDefaults(t *testing.T) {
	cat := &CatalogEntry{
		Name:              "wd-swinv2",
		DefaultThreshold:  0.40,
		DefaultThresholds: map[string]float64{"character": 0.85},
	}
	got := SeedTaggerInstance("wd-swinv2", true, cat)
	if got.ConfidenceThreshold != 0.40 {
		t.Errorf("ConfidenceThreshold = %v, want 0.40", got.ConfidenceThreshold)
	}
	if got.CategoryThresholds["character"] != 0.85 {
		t.Errorf("character override = %v, want 0.85", got.CategoryThresholds["character"])
	}
}

func TestSeedTaggerInstance_PartialCatalog(t *testing.T) {
	cat := &CatalogEntry{Name: "joytag", DefaultThreshold: 0.40}
	got := SeedTaggerInstance("joytag", true, cat)
	if got.ConfidenceThreshold != 0.40 {
		t.Errorf("ConfidenceThreshold = %v, want 0.40", got.ConfidenceThreshold)
	}
	if got.CategoryThresholds != nil {
		t.Errorf("CategoryThresholds = %v, want nil for catalog without per-cat map", got.CategoryThresholds)
	}
}

func TestCatalogDefaults_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	cat := LoadCatalog(tmp)
	byName := map[string]CatalogEntry{}
	for _, e := range cat {
		byName[e.Name] = e
	}
	wd := byName["wd-swinv2"]
	if wd.DefaultThreshold != 0.35 {
		t.Errorf("wd-swinv2 DefaultThreshold = %v, want 0.35", wd.DefaultThreshold)
	}
	if wd.DefaultThresholds["character"] != 0.50 {
		t.Errorf("wd-swinv2 character = %v, want 0.50", wd.DefaultThresholds["character"])
	}
	joy := byName["joytag"]
	if joy.DefaultThreshold != 0.40 {
		t.Errorf("joytag DefaultThreshold = %v, want 0.40", joy.DefaultThreshold)
	}
	if len(joy.DefaultThresholds) != 0 {
		t.Errorf("joytag DefaultThresholds = %v, want empty (single_general profile)", joy.DefaultThresholds)
	}
	eva := byName["animetimm-eva02"]
	if eva.DefaultThreshold != 0.40 {
		t.Errorf("animetimm-eva02 DefaultThreshold = %v, want 0.40", eva.DefaultThreshold)
	}
	if eva.DefaultThresholds["character"] != 0.61 {
		t.Errorf("animetimm-eva02 character = %v, want 0.61", eva.DefaultThresholds["character"])
	}
}
