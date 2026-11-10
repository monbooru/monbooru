package web

import (
	"strings"
	"testing"
)

func TestElideHash(t *testing.T) {
	elide, ok := templateFuncs()["elideHash"].(func(string) string)
	if !ok {
		t.Fatalf("elideHash = %T, want func(string) string", templateFuncs()["elideHash"])
	}
	sha := strings.Repeat("ab", 32)
	cases := []struct{ in, want string }{
		{"", ""},
		{"deadbeef", "deadbeef"},
		{strings.Repeat("c", 19), strings.Repeat("c", 19)},
		{strings.Repeat("c", 20), strings.Repeat("c", 8) + "..." + strings.Repeat("c", 8)},
		{sha, "abababab...abababab"},
	}
	for _, c := range cases {
		if got := elide(c.in); got != c.want {
			t.Errorf("elideHash(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := elide(sha); len(got) != 19 {
		t.Errorf("elideHash length = %d, want 19", len(got))
	}
}
