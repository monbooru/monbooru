package tagger

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeLabel_PreservesColon(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":3", ":3"},
		{"nier:automata", "nier:automata"},
		{"rating:general", "rating:general"},
	}
	for _, c := range cases {
		got, ok := sanitizeLabel(c.in, 0)
		if got != c.want || !ok {
			t.Errorf("sanitizeLabel(%q) = (%q, %v), want (%q, true)", c.in, got, ok, c.want)
		}
	}
}

func TestSanitizeLabel_PlaceholderForAllPunct(t *testing.T) {
	got, ok := sanitizeLabel(":::", 7)
	if got != "_unsupported_7" || ok {
		t.Errorf("sanitizeLabel(%q, 7) = (%q, %v), want (_unsupported_7, false)", ":::", got, ok)
	}
}

func TestSanitizeLabel_KeepsPunctuation(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"jack-o' challenge", "jack-o'_challenge"},
		{"fate/grand order", "fate/grand_order"},
		{"r&b", "r&b"},
		{"DECO*27", "deco_27"},
		{`"quoted"`, "quoted"},
	}
	for _, c := range cases {
		got, ok := sanitizeLabel(c.in, 0)
		if got != c.want || !ok {
			t.Errorf("sanitizeLabel(%q) = (%q, %v), want (%q, true)", c.in, got, ok, c.want)
		}
	}
}

func TestSanitizeLabel_Unicode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"café", "café"},
		{"東方Project", "東方project"},
		{"Kaenbyou Rin", "kaenbyou_rin"},
	}
	for _, c := range cases {
		got, ok := sanitizeLabel(c.in, 0)
		if got != c.want || !ok {
			t.Errorf("sanitizeLabel(%q) = (%q, %v), want (%q, true)", c.in, got, ok, c.want)
		}
	}
	long := strings.Repeat("あ", 201)
	got, ok := sanitizeLabel(long, 0)
	if !ok || utf8.RuneCountInString(got) != 200 || !utf8.ValidString(got) {
		t.Errorf("sanitizeLabel(201 runes) = (%d runes, valid=%v, ok=%v), want 200 valid runes",
			utf8.RuneCountInString(got), utf8.ValidString(got), ok)
	}
}

func TestSanitizeLabel_AllowsEmoticonLabels(t *testing.T) {
	cases := []string{"??", ">_<", "^_^", "<3", "=3", "=w=", "nani?"}
	for _, in := range cases {
		got, ok := sanitizeLabel(in, 0)
		if got != in || !ok {
			t.Errorf("sanitizeLabel(%q) = (%q, %v), want (%q, true)", in, got, ok, in)
		}
	}
}
