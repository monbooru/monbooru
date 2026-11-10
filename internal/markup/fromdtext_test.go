package markup

import (
	"strings"
	"testing"
	"time"
)

func TestFromDText(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"artist commentary",
			"[b]Title[/b]\n\"pixiv\":[https://www.pixiv.net/artworks/1] - [[blue eyes]] again",
			"[b]Title[/b]\n[url=https://www.pixiv.net/artworks/1]pixiv[/url] - [tag=blue_eyes]blue eyes[/tag] again"},

		{"shared marks survive", "[b]a[/b][i]b[/i][u]c[/u][s]d[/s][code]e[/code][tn]f[/tn]",
			"[b]a[/b][i]b[/i][u]c[/u][s]d[/s][code]e[/code][tn]f[/tn]"},
		{"wrappers keep their text", "[quote]said[/quote] and [spoiler]hidden[/spoiler]",
			"said and hidden"},
		{"wrapper with an argument", "[expand=More]body[/expand]", "body"},
		{"br is a newline", "a[br]b", "a\nb"},
		{"header prefix goes", "h4. A heading\nbody", "A heading\nbody"},
		{"header anchor goes with it", "h2#intro. Intro", "Intro"},
		{"a header mid-line is text", "see h4. there", "see h4. there"},

		{"bracketed link", `"label":[https://example.com/a]`, "[url=https://example.com/a]label[/url]"},
		{"bare-value link", `"label":https://example.com/a done`,
			"[url=https://example.com/a]label[/url] done"},
		{"trailing punctuation is not the link", `see "it":https://example.com/a.`,
			"see [url=https://example.com/a]it[/url]."},
		{"relative link keeps its label", `"post":[/posts/123]`, "post"},
		{"quoted phrase before a colon is text", `he said "hi": nice`, `he said "hi": nice`},
		{"unterminated quote is text", `a "b`, `a "b`},

		{"bare url links", "look at https://example.com/x now",
			"look at [url=https://example.com/x]https://example.com/x[/url] now"},
		{"a url glued to a word is text", "xhttps://example.com/x", "xhttps://example.com/x"},
		{"angle url links", "<https://example.com/a(b)>",
			"[url=https://example.com/a(b)]https://example.com/a(b)[/url]"},

		{"wiki link with a label", "[[blue eyes|those eyes]]", "[tag=blue_eyes]those eyes[/tag]"},
		{"tag search", "{{solo}}", "[tag=solo]solo[/tag]"},
		{"multi-term search keeps its text", "{{1girl solo}}", "1girl solo"},
		{"marks inside a label", `"[b]bold[/b] label":[https://example.com]`,
			"[url=https://example.com][b]bold[/b] label[/url]"},
		{"a link inside a link keeps its label", `"[[solo]] here":[https://example.com]`,
			"[url=https://example.com]solo here[/url]"},

		{"unknown construct is text", "[color2]x[/color2]", "[color2]x[/color2]"},
		{"this side's own link survives a second pass",
			"[url=https://example.com]a[/url]", "[url=https://example.com]a[/url]"},
		{"unclosed bracket is text", "[b unclosed", "[b unclosed"},
		{"crlf folds", "a\r\nb", "a\nb"},
		{"plain text", "just text", "just text"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromDText(tc.src); got != tc.want {
				t.Errorf("FromDText(%q)\n got %q\nwant %q", tc.src, got, tc.want)
			}
		})
	}
}

func TestFromDTextScalesWithLength(t *testing.T) {
	src := strings.Repeat("Some Mixed Case prose about https://example.com/a and more of it. ", 4000)
	if len(src) < 256*1024 {
		t.Fatalf("input is %d bytes, want at least 256 KiB", len(src))
	}
	done := make(chan string, 1)
	go func() { done <- FromDText(src) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "[url=https://example.com/a]") {
			t.Error("converted body carries no link")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("conversion did not finish in 5s")
	}
}

func TestFromDTextRoundTripsThroughParse(t *testing.T) {
	src := "[b]t[/b] \"pixiv\":[https://www.pixiv.net/artworks/1] [[blue eyes]] {{solo}}"
	doc := Parse(FromDText(src))
	res := Resolver{Tags: map[string]TagRef{
		"blue_eyes": {Href: "/?q=blue_eyes", Known: true},
		"solo":      {Href: "/?q=solo", Known: true},
	}}
	got := string(doc.Render(res))
	for _, want := range []string{
		"<strong>t</strong>",
		`href="https://www.pixiv.net/artworks/1"`,
		`href="/?q=blue_eyes"`,
		`href="/?q=solo"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render %q missing %q", got, want)
		}
	}
}
