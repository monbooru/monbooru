package webui

import (
	"html/template"
	"testing"
)

func TestCategoryColor(t *testing.T) {
	cases := []struct {
		in   string
		want template.CSS
	}{
		{"#3D90E3", "var(--cat-3d90e3,#3D90E3)"},
		{"#abc", "var(--cat-abc,#abc)"},
		{"", "#888888"},
		{"red", "#888888"},
		{"#12345", "#888888"},
		{"#000000;background:url(x)", "#888888"},
	}
	for _, c := range cases {
		if got := CategoryColor(c.in); got != c.want {
			t.Errorf("CategoryColor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
