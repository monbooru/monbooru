package tagger

import (
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/worker"
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
	cat := &worker.CatalogEntry{
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
	cat := &worker.CatalogEntry{Name: "joytag", DefaultThreshold: 0.40}
	got := SeedTaggerInstance("joytag", true, cat)
	if got.ConfidenceThreshold != 0.40 {
		t.Errorf("ConfidenceThreshold = %v, want 0.40", got.ConfidenceThreshold)
	}
	if got.CategoryThresholds != nil {
		t.Errorf("CategoryThresholds = %v, want nil for catalog without per-cat map", got.CategoryThresholds)
	}
}
