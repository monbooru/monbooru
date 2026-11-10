package searchkw

import (
	"slices"
	"testing"
)

func TestValueKnown(t *testing.T) {
	cases := []struct {
		key, val string
		want     bool
	}{
		{"type", "image", true},
		{"type", "animated", true},
		{"type", "video", false},
		{"type", "IMAGE", true},
		{"mime", "png", true},
		{"mime", "avif", true},
		{"mime", "jxl", true},
		{"mime", "zzzz", false},
		{"fav", "true", true},
		{"fav", "maybe", false},
		{"fav", "yes", true},
		{"inbox", "0", true},
		{"missing", "off", true},
		{"rating", "explicit", true},
		{"rating", "spicy", false},
		{"ai", "sd", true},
		{"ai", "dalle", false},
		{"relation", "duplicate", true},
		{"relation", "bogus", false},

		{"type", "image,archive", true},
		{"type", "image,video", false},
		{"ai", "a1111,comfyui", true},
		{"fav", "true,false", false},
		{"rating", "general,explicit", false},
		{"relation", "duplicate,version", false},
		{"mime", "image/png", true},
		{"mime", "video/mp4", true},
		{"mime", "image/png,video/mp4", true},

		{"width", ">=1920", true},
		{"size", "10MB", true},
		{"date", "2024-01-01", true},
		{"tagcount", "5..20", true},
		{"name", "vacation", true},
		{"folder", "trips/2024", true},
		{"source", "some booru", true},
		{"tagged", "ptr", true},
		{"autotagged", "wd14", true},
		{"via", "extract", true},
		{"via", "monloader", true},

		{"type", "", true},
	}
	for _, c := range cases {
		if got := ValueKnown(c.key, c.val); got != c.want {
			t.Errorf("ValueKnown(%q, %q) = %v, want %v", c.key, c.val, got, c.want)
		}
	}
}

func TestTheKeywordTablesAgree(t *testing.T) {
	for key := range Expansions {
		if !IsKeyword(key) {
			t.Errorf("Expansions row %q is no keyword", key)
		}
	}
	for _, keys := range []map[string]bool{ordinalKeys, rangeKeys} {
		for key := range keys {
			if _, ok := Expansions[key]; !ok {
				t.Errorf("hint key %q has no Expansions row", key)
			}
		}
	}
	for key, descs := range ExpansionDescriptions {
		for val := range descs {
			if !slices.Contains(Expansions[key], val) {
				t.Errorf("ExpansionDescriptions[%q] describes %q, which Expansions does not offer", key, val)
			}
		}
	}
}
