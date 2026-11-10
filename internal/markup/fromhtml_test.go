package markup

import (
	"net/url"
	"strings"
	"testing"
)

func TestFromHTML(t *testing.T) {
	base, err := url.Parse("https://danbooru.example/posts/11856883")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"character label",
			"<b><a style=\"color:var(--character-tag-color)\" href=\"/posts?tags=lapis_lazuli_(steven_universe)\">Lapis Lazuli</a></b>\r\n" +
				"<tn><a href=\"/posts?tags=steven_universe\">Steven Universe</a></tn>",
			"[b][tag=lapis_lazuli_(steven_universe)]Lapis Lazuli[/tag][/b]\n" +
				"[tn][tag=steven_universe]Steven Universe[/tag][/tn]"},
		{"off-site link beside a tag link",
			"<b><a href=\"/posts?tags=hulk\">Hulk</a></b>\r\n" +
				"<tn><a href=\"/posts?tags=the_incredible_hulk_(series)\">The Incredible Hulk</a>\r\n" +
				"<a href=\"https://www.youtube.com/watch?v=9n9QOKY-0d0\">Shampoo Prank</a></tn>",
			"[b][tag=hulk]Hulk[/tag][/b]\n" +
				"[tn][tag=the_incredible_hulk_(series)]The Incredible Hulk[/tag]\n" +
				"[url=https://www.youtube.com/watch?v=9n9QOKY-0d0]Shampoo Prank[/url][/tn]"},

		{"plain text", "just text", "just text"},
		{"marks", "<strong>a</strong><em>b</em><del>c</del><ins>d</ins><tt>e</tt>",
			"[b]a[/b][i]b[/i][s]c[/s][u]d[/u][code]e[/code]"},
		{"line break", "a<br>b", "a\nb"},
		{"blocks", "<div>x</div><div>y</div>", "x\ny"},
		{"list", "<ul><li>one<li>two</ul>", "- one\n- two"},
		{"unknown element keeps its text", "<span style=\"color:red\">kept</span>", "kept"},
		{"script is dropped whole", "<script>alert(1)</script>hello", "hello"},
		{"script closes in any case", "<script>a</p></ScRiPt>hello", "hello"},
		{"style is dropped whole", "<style>a{}</style>hello", "hello"},
		{"comment is dropped", "a<!-- note -->b", "ab"},
		{"unclosed element still balances", "<b>x", "[b]x[/b]"},
		{"crossed elements still balance", "<b><i></b></i>", "[b][i][/i][/b]"},
		{"a close with nothing open is ignored", "<b>x</i></a>y", "[b]xy[/b]"},

		{"bad scheme keeps only the label", `<a href="javascript:alert(1)">click</a>`, "click"},
		{"empty href keeps only the label", `<a href="">click</a>`, "click"},
		{"a multi-tag search stays a link", `<a href="/posts?tags=a+b">two</a>`,
			"[url=https://danbooru.example/posts?tags=a+b]two[/url]"},
		{"nested links keep the outer one", `<a href="https://a.example/1">out<a href="https://b.example/2">in</a></a>`,
			"[url=https://a.example/1]outin[/url]"},
		{"entities decode once", "&lt;b&gt;text &amp; more", "<b>text & more"},
		{"unquoted href", `<a href=https://x.example/a>x</a>`, "[url=https://x.example/a]x[/url]"},
		{"bare angle bracket is text", "2 < 3", "2 < 3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromHTML(c.src, base); got != c.want {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestFromHTMLWithoutBase(t *testing.T) {
	if got := FromHTML(`<a href="/posts?tags=hulk">Hulk</a>`, nil); got != "Hulk" {
		t.Fatalf("got %q", got)
	}
	if got := FromHTML(`<a href="https://x.example/a">a</a>`, nil); got != "[url=https://x.example/a]a[/url]" {
		t.Fatalf("got %q", got)
	}
}

func TestFromHTMLRoundTripStaysInert(t *testing.T) {
	base, _ := url.Parse("https://danbooru.example/posts/1")
	src := `<a href="https://x.example/a]injected[/url][b]bold">x</a><b>[tag=hulk]</b>`
	got := string(Parse(FromHTML(src, base)).Render(testResolver()))
	want := "x<strong>[tag=hulk]</strong>"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestFromHTML_UnterminatedTagEndsTheConversion(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`a <b>bold</b> <a href="https://x.example/`, "a [b]bold[/b]"},
		{"kept<i>tail", "kept[i]tail[/i]"},
		{"kept<i" + strings.Repeat("<a", 3), "kept"},
		{"<script>never closed", ""},
	} {
		if got := FromHTML(c.src, nil); got != c.want {
			t.Errorf("FromHTML(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func BenchmarkFromHTML_Unterminated(b *testing.B) {
	src := strings.Repeat("<a", 8000)
	for b.Loop() {
		FromHTML(src, nil)
	}
}

func BenchmarkFromHTML_Scripts(b *testing.B) {
	src := strings.Repeat("<script></script>", 1000)
	for b.Loop() {
		FromHTML(src, nil)
	}
}

func BenchmarkFromHTML_UnmatchedCloses(b *testing.B) {
	src := strings.Repeat("<b>", 2666) + strings.Repeat("</i>", 2000)
	for b.Loop() {
		FromHTML(src, nil)
	}
}
