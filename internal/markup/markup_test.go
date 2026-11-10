package markup

import (
	"strings"
	"testing"
	"time"
)

func testResolver() Resolver {
	return Resolver{
		Tags: map[string]TagRef{
			"hulk":  {Href: "/?q=hulk", Color: "var(--cat-00aa00,#00aa00)", Known: true},
			"ghost": {Href: "/?q=ghost"},
		},
		Images: map[int64]bool{12: true},
		Links:  map[string]int64{"https://danbooru.example/posts/5": 7},
	}
}

func TestRender(t *testing.T) {
	cases := []renderCase{
		{"plain", "plain text", "plain text"},
		{"bold", "[b]bold[/b]", "<strong>bold</strong>"},
		{"nested marks", "[i][u]x[/u][/i]", `<em><span class="mk-u">x</span></em>`},
		{"struck", "[s]x[/s]", "<s>x</s>"},
		{"code", "[code]x[/code]", "<code>x</code>"},
		{"translator note", "[tn]x[/tn]", `<span class="mk-tn">x</span>`},
		{"newlines survive", "a\nb", "a\nb"},

		{"known tag", "[tag=hulk]Hulk[/tag]",
			`<a class="mk-tag" href="/?q=hulk" style="color:var(--cat-00aa00,#00aa00)">Hulk</a>`},
		{"short tag form", "[tag]hulk[/tag]",
			`<a class="mk-tag" href="/?q=hulk" style="color:var(--cat-00aa00,#00aa00)">hulk</a>`},
		{"unknown tag", "[tag=ghost]Ghost[/tag]",
			`<a class="mk-tag mk-unknown" href="/?q=ghost" title="no such tag in this gallery">Ghost</a>`},
		{"unresolved tag falls back to its label", "[tag=nowhere]Nowhere[/tag]", "Nowhere"},

		{"external link", "[url=https://x.example/a]link[/url]",
			`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="https://x.example/a">link</a>`},
		{"link the gallery holds", "[url=https://danbooru.example/posts/5]post[/url]",
			`<a class="mk-local" href="/images/7" title="https://danbooru.example/posts/5">post</a>`},
		{"empty label falls back to the target", "[url=https://x.example/a][/url]",
			`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="https://x.example/a">https://x.example/a</a>`},

		{"image here", "[image:12]", `<a class="mk-local" href="/images/12">#12</a>`},
		{"image gone", "[image:99]", "#99"},

		{"marks inside a link", "[url=https://x.example/a][b]bold[/b][/url]",
			`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="https://x.example/a"><strong>bold</strong></a>`},
	}
	runRenderCases(t, cases)
}

func TestRenderDegradesToText(t *testing.T) {
	cases := []renderCase{
		{"unclosed", "[b]unclosed", "[b]unclosed"},
		{"unknown name", "[x]y[/x]", "[x]y[/x]"},
		{"uppercase", "[B]x[/B]", "[B]x[/B]"},
		{"space in the head", "[b ]x[/b ]", "[b ]x[/b ]"},
		{"empty head", "[]x", "[]x"},
		{"prose in brackets", "see [fig 1] below", "see [fig 1] below"},
		{"mark with a value", "[b=1]x[/b]", "[b=1]x[/b]"},
		{"bad scheme", "[url=javascript:alert(1)]x[/url]", "[url=javascript:alert(1)]x[/url]"},
		{"no scheme", "[url=/local]x[/url]", "[url=/local]x[/url]"},
		{"non-numeric image", "[image:abc]", "[image:abc]"},
		{"zero image", "[image:0]", "[image:0]"},
		{"empty tag reference", "[tag][/tag]", "[tag][/tag]"},
		{"truncated mid-construct", "[b]bold[/b] and [tag=hu", "<strong>bold</strong> and [tag=hu"},
		{"nested link", "[url=https://a.example]outer [url=https://b.example]inner[/url][/url]",
			`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="https://a.example">outer [url=https://b.example]inner</a>[/url]`},
		{"past the depth cap", "[b][i][u][s][code]x[/code][/s][/u][/i][/b]",
			`<strong><em><span class="mk-u"><s>[code]x[/code]</s></span></em></strong>`},
	}
	runRenderCases(t, cases)
}

func TestRenderEscapesEverythingFromTheBody(t *testing.T) {
	cases := []renderCase{
		{"script tag", `hello <script>alert(1)</script>`,
			"hello &lt;script&gt;alert(1)&lt;/script&gt;"},
		{"markup inside a mark", "[b]<img src=x onerror=alert(1)>[/b]",
			"<strong>&lt;img src=x onerror=alert(1)&gt;</strong>"},
		{"quote in a link target", `[url=https://x.example/"onmouseover="a]x[/url]`,
			`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="https://x.example/&#34;onmouseover=&#34;a">x</a>`},
		{"quote in a link label", `[url=https://x.example/a]"><b>[/url]`,
			`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="https://x.example/a">&#34;&gt;&lt;b&gt;</a>`},
		{"ampersand", "a & b", "a &amp; b"},
	}
	runRenderCases(t, cases)
}

func TestText(t *testing.T) {
	cases := []struct{ src, want string }{
		{"plain", "plain"},
		{"[b]bold[/b] rest", "bold rest"},
		{"[tag=hulk]Hulk[/tag]", "Hulk"},
		{"[tag]hulk[/tag]", "hulk"},
		{"[url=https://x.example/a]link[/url]", "link"},
		{"[image:12]", "#12"},
		{"[tn][tag=hulk]Hulk[/tag][/tn]", "Hulk"},
	}
	for _, c := range cases {
		if got := Parse(c.src).Text(); got != c.want {
			t.Fatalf("Text(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestCollect(t *testing.T) {
	refs := NewRefs()
	Parse("[b][tag=hulk]Hulk[/tag][/b] [image:12] [url=https://x.example/a]a[/url]").Collect(refs)
	Parse("[tag=hulk]again[/tag] [image:13]").Collect(refs)

	if len(refs.Tags) != 1 || !refs.Tags["hulk"] {
		t.Fatalf("tags = %v", refs.Tags)
	}
	if len(refs.Images) != 2 || !refs.Images[12] || !refs.Images[13] {
		t.Fatalf("images = %v", refs.Images)
	}
	if len(refs.URLs) != 1 || !refs.URLs["https://x.example/a"] {
		t.Fatalf("urls = %v", refs.URLs)
	}
}

func TestPathologicalBodiesStayCheap(t *testing.T) {
	cases := map[string]string{
		"unclosed opens": strings.Repeat("[b]", 3333),
		"unclosed tags":  strings.Repeat("[tag=x]", 1428),
		"deep nesting":   strings.Repeat("[b]", 2500) + strings.Repeat("[/b]", 2500),
		"bracket soup":   strings.Repeat("[", 10000),
		"unclosed urls":  strings.Repeat("[url=https://x.example/a]", 400),
	}
	for name, src := range cases {
		start := time.Now()
		d := Parse(src)
		_ = d.Render(Resolver{})
		_ = d.Text()
		if el := time.Since(start); el > 200*time.Millisecond {
			t.Errorf("%s: %v for %d bytes", name, el, len(src))
		}
	}
}

type renderCase struct{ name, src, want string }

func runRenderCases(t *testing.T, cases []renderCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(Parse(c.src).Render(testResolver())); got != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}
