package web

import (
	"html"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/webui"
	webFS "github.com/monbooru/monbooru/web"
)

type chordNode struct {
	url       string
	selectors []string
	children  map[string]*chordNode
}

func parseChordMap(t *testing.T, js string) *chordNode {
	t.Helper()
	start := strings.Index(js, "var chordMap = {")
	if start < 0 {
		t.Fatal("main.js holds no chordMap")
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(js[start+len("var chordMap = "):], "")
	pos := 0
	skip := func() {
		for pos < len(src) && strings.ContainsRune(" \t\r\n,", rune(src[pos])) {
			pos++
		}
	}
	str := func() string {
		end := strings.IndexByte(src[pos+1:], src[pos])
		s := src[pos+1 : pos+1+end]
		pos += end + 2
		return s
	}
	var value func() *chordNode
	value = func() *chordNode {
		skip()
		switch src[pos] {
		case '\'', '"':
			return &chordNode{url: str()}
		case '[':
			pos++
			n := &chordNode{selectors: []string{}}
			for skip(); src[pos] != ']'; skip() {
				n.selectors = append(n.selectors, str())
			}
			pos++
			return n
		case '{':
			pos++
			n := &chordNode{children: map[string]*chordNode{}}
			for skip(); src[pos] != '}'; skip() {
				colon := strings.IndexByte(src[pos:], ':')
				key := strings.Trim(strings.TrimSpace(src[pos:pos+colon]), `'"`)
				pos += colon + 1
				n.children[key] = value()
			}
			pos++
			return n
		}
		t.Fatalf("chordMap: unexpected %q", src[pos:min(pos+20, len(src))])
		return nil
	}
	return value()
}

func TestShortcutOverlayKeysAreBound(t *testing.T) {
	layout, err := fs.ReadFile(webFS.FS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	overlay := string(layout)
	overlay = overlay[strings.Index(overlay, `<dialog id="shortcuts-help">`):]
	overlay = overlay[:strings.Index(overlay, "</dialog>")]

	mainJS, err := fs.ReadFile(webFS.FS, "static/main.js")
	if err != nil {
		t.Fatal(err)
	}
	baseJS, err := fs.ReadFile(webui.Static, "base.js")
	if err != nil {
		t.Fatal(err)
	}
	var scripts strings.Builder
	scripts.Write(baseJS)
	scripts.Write(mainJS)
	for _, glob := range []string{"templates/*.html", "templates/partials/*.html"} {
		files, _ := fs.Glob(webFS.FS, glob)
		for _, f := range files {
			b, _ := fs.ReadFile(webFS.FS, f)
			for _, m := range regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(string(b), -1) {
				scripts.WriteString(m[1])
			}
		}
	}
	src := scripts.String()

	chords := parseChordMap(t, string(mainJS))
	digitTargets := map[string]string{}
	for _, m := range regexp.MustCompile(`node === chordMap\.(\w+)\) return document\.querySelectorAll\('([^']+)'\)`).FindAllStringSubmatch(string(mainJS), -1) {
		digitTargets[m[1]] = m[2]
	}
	stepKeys := map[string]bool{}
	for _, m := range regexp.MustCompile(`\{([^{}]*)\}\[\w+\.key\]`).FindAllStringSubmatch(src, -1) {
		for _, k := range regexp.MustCompile(`(\w+)\s*:\s*\[`).FindAllStringSubmatch(m[1], -1) {
			stepKeys[k[1]] = true
		}
	}
	var digitRanges [][2]byte
	for _, m := range regexp.MustCompile(`\.key >= '(\d)' && \w+\.key <= '(\d)'|/\^\[(\d)-(\d)\]\$/\.test\(\w+\.key\)`).FindAllStringSubmatch(src, -1) {
		lo, hi := m[1]+m[3], m[2]+m[4]
		digitRanges = append(digitRanges, [2]byte{lo[0], hi[0]})
	}
	inRange := func(lo, hi byte) bool {
		return slices.ContainsFunc(digitRanges, func(r [2]byte) bool { return r[0] <= lo && hi <= r[1] })
	}
	bound := func(key string) bool {
		lit := regexp.QuoteMeta(strings.ReplaceAll(key, `\`, `\\`))
		if regexp.MustCompile(`\.key\s*===\s*['"]` + lit + `['"]|case\s+['"]` + lit + `['"]\s*:`).MatchString(src) {
			return true
		}
		if stepKeys[key] {
			return true
		}
		if key == "1-9" {
			return inRange('1', '9')
		}
		return len(key) == 1 && key[0] >= '0' && key[0] <= '9' && inRange(key[0], key[0])
	}

	named := map[string]string{
		"Esc": "Escape", "Space": " ", "Enter": "Enter", "Delete": "Delete", "Backspace": "Backspace",
		"Home": "Home", "End": "End", "↑": "ArrowUp", "↓": "ArrowDown", "←": "ArrowLeft", "→": "ArrowRight",
	}
	pointer := map[string]bool{
		"Drag": true, "click": true, "Double click": true, "Click outside": true, "Wheel": true,
		"Alt": true, "Ctrl": true, "Shift": true,
	}
	rows := regexp.MustCompile(`(?s)<li([^>]*)>(.*?)</li>`).FindAllStringSubmatch(overlay, -1)
	if len(rows) < 50 {
		t.Fatalf("read %d overlay rows; the overlay markup moved", len(rows))
	}
	for _, row := range rows {
		when := ""
		if m := regexp.MustCompile(`data-when="([^"]*)"`).FindStringSubmatch(row[1]); m != nil {
			when = html.UnescapeString(m[1])
		}
		var keys []string
		for _, m := range regexp.MustCompile(`<kbd>([^<]+)</kbd>`).FindAllStringSubmatch(row[2], -1) {
			keys = append(keys, html.UnescapeString(m[1]))
		}
		label := strings.Join(keys, " ")

		node, path := chords, []string{}
		for _, k := range keys {
			next := node.children[k]
			if next == nil {
				break
			}
			node, path = next, append(path, k)
		}
		switch {
		case len(path) == len(keys) && node.children == nil:
			if when != "" && !slices.Contains(node.selectors, when) {
				t.Errorf("overlay row %q: data-when %q is not one of the chord's selectors %q", label, when, node.selectors)
			}
			continue
		case len(path) > 0 && len(path) == len(keys)-1 && keys[len(keys)-1] == "1-9" && digitTargets[strings.Join(path, ".")] != "":
			if want := digitTargets[strings.Join(path, ".")]; when != want {
				t.Errorf("overlay row %q: data-when %q, want the chord's digit targets %q", label, when, want)
			}
			continue
		case len(path) > 0 && len(keys) > 1:
			t.Errorf("overlay row %q starts a chord but is no chordMap path", label)
			continue
		}

		for _, k := range keys {
			if pointer[k] {
				continue
			}
			if n, ok := named[k]; ok {
				k = n
			}
			if !bound(k) {
				t.Errorf("overlay row %q lists %q, which no script binds", label, k)
			}
		}
		for sel := range strings.SplitSeq(when, ",") {
			needle := strings.TrimSpace(sel)
			if regexp.MustCompile(`^#[\w-]+$`).MatchString(needle) {
				needle = needle[1:]
			}
			if when != "" && !strings.Contains(src, needle) {
				t.Errorf("overlay row %q: data-when %q is read by no script", label, sel)
			}
		}
	}
}
