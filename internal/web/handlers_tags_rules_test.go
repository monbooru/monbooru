package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/models"
)

func rulesStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"version":"v-stub"}`))
		case "/api/v1/rules":
			var asked struct {
				Names []struct{ Tag, Category string } `json:"names"`
			}
			_ = json.NewDecoder(r.Body).Decode(&asked)
			if r.Method != http.MethodPost || len(asked.Names) == 0 || asked.Names[0].Tag != "watermark" || asked.Names[0].Category != "general" {
				t.Errorf("rules asked %s for %+v", r.Method, asked)
			}
			_, _ = w.Write([]byte(`{"results":[` + body + `]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

const rulesAnswer = `{"category_kept":true,"sites":[
 {"site":"danbooru","kind":"booru","rule":"drop","kept":false},
 {"site":"e621","kind":"booru","rule":"none","renamed_from":["wm_text"],"kept":true},
 {"site":"gelbooru","kind":"booru","rule":"none","mapped_from":["meta"],"kept":true},
 {"site":"konachan","kind":"booru","rule":"none","kept":true},
 {"site":"rule34","kind":"booru","rule":"none","kept":false},
 {"site":"sankaku","kind":"booru","rule":"none","blacklist":"site","blacklist_line":"watermark","kept":true},
 {"site":"yandere","kind":"booru","rule":"none","kept":true},
 {"site":"zerochan","kind":"booru","builtin":true,"rule":"none","blacklist":"global","blacklist_line":"watermark rating:explicit","kept":true},
 {"site":"ptr","kind":"lookup","rule":"move","target":"meta:watermark","kept":false}]}`

func TestTagRulesPanel_DrawsEveryLane(t *testing.T) {
	stub := rulesStub(t, rulesAnswer)
	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "tok"
	srv.cfg.Server.MonloaderURL = stub.URL
	cx := srv.active()
	tag := mustTag(t, cx.TagSvc, "watermark", cx.GeneralCategoryID)
	text := mustTag(t, cx.TagSvc, "text", cx.GeneralCategoryID)
	if _, err := cx.TagSvc.AddImplication(tag.ID, text.ID); err != nil {
		t.Fatal(err)
	}
	logo := mustTag(t, cx.TagSvc, "logo", cx.GeneralCategoryID)
	if _, err := cx.TagSvc.AddImplication(logo.ID, tag.ID); err != nil {
		t.Fatal(err)
	}

	w := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		"monloader live", "set in monloader", "set in monbooru", "1 mappings", "2 blacklist", "3 categories kept", "4-6 gallery rules · default",
		">danbooru<", "tag rule: ", ">drop<", ">e621<", ">wm_text<", "> → here<",
		"category map: meta → ", ">sankaku<", ">skip<", "sankaku&#39;s own list: post skipped",
		">rule34<", ">off<", "post sent without it",
		">zerochan<", ">built-in<", "only with rating:explicit", `class="tp-swarn"`, ">?<",
		"2 other sites", ">ptr<", "tag rule → ", "meta:watermark", `class="tp-sdim" stroke-dasharray="3 3"`,
		">you<", "pages, batch, API, import",
		"4 aliases", "none  ", "[add alias]", "5 rejected", "[reject]", "6 implications", "1 tag implies it", "[add]",
		"through an implication", " · 1 tag that adds this one", ">logo<", fmt.Sprintf(`href="/tags/%d"`, logo.ID), "no image yet",
		"the image", "0 images", "also gets ", ">text<",
		`href="` + stub.URL + `/settings?site=danbooru&amp;tab=mapping&amp;q=watermark"`,
		`href="` + stub.URL + `/settings#rules"`, `href="` + stub.URL + `/settings?site=sankaku&amp;tab=mapping"`,
		`href="#implications-editor"`, `href="#tag-aliases"`, `href="#tag-implied-by"`,
	} {
		wantIn(t, body, want)
	}
	for _, want := range []string{
		`<div class="tag-path-list">`, `<h4>as watermark`, `<div class="tag-path-row"><span><span class="tl-ml">danbooru</span>`,
		`<span class="tl-err"> ×</span>`, `<span class="tl-warn"> ?</span>`, `tag-path-row-note tl-warn">only with rating:explicit`,
		`<p><a href="#tag-aliases" onclick="document.getElementById('alias-add-input').focus();return false"><span class="tl-fg">4 aliases: </span>`,
	} {
		wantIn(t, body, want)
	}
	shared := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(stub.URL) + `/settings#rules" target="_blank" rel="noopener"><rect x="` + fmt.Sprint(tpColumns(true, 0).x3) + `" y="[0-9.]+" width="` + fmt.Sprint(tp3W) + `" height="62"`)
	if !shared.MatchString(body) {
		t.Error("the step-3 box two lanes share does not open monloader's Rules")
	}
	wantIn(t, body, `<a href="#tag-aliases" onclick="document.getElementById('alias-add-input').focus();return false"><text`)
	wantIn(t, body, `<a href="#tag-implied-by" onclick="document.getElementById('implied-by-add-input').focus();return false"><text`)
	wantNotIn(t, body, ">konachan<")
	wantNotIn(t, body, "Configure → mappings")
}

var hxRules = [2]string{"HX-Request", "true"}

func TestTagRulesPanel_RedirectsAPlainGetAndRefusesAStalePage(t *testing.T) {
	srv := newTestServer(t)
	tag := mustTag(t, srv.active().TagSvc, "plain_get", srv.active().GeneralCategoryID)
	w := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID))
	mustCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != fmt.Sprintf("/tags/%d#tag-path", tag.ID) {
		t.Errorf("Location = %q", loc)
	}
	stale := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules, [2]string{"X-Monbooru-Gallery", "another"})
	mustCode(t, stale, http.StatusConflict)
}

func TestTagRulesPanel_UnpairedAndRejectedAndAlias(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	tag := mustTag(t, cx.TagSvc, "watermark", cx.GeneralCategoryID)
	alias, err := cx.TagSvc.CreateAlias("wm", cx.GeneralCategoryID, tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.RejectTag(tag.ID); err != nil {
		t.Fatal(err)
	}

	w := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{"monloader not paired", ">monloader<", "not paired, no site lanes", `href="/settings#plugins" target="_blank" rel="noopener"`, "[un-reject]", "yes · 0 dropped", "btn-detail-unreject", "1 name → here"} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, "set in monloader")
	for _, gone := range []string{"[add alias]", "alias-add-input", "implied-by-add-input", "[add]"} {
		wantNotIn(t, body, gone)
	}

	plain := mustTag(t, cx.TagSvc, "solo", cx.GeneralCategoryID)
	twin, err := cx.TagSvc.CreateCategory("pose", "#aabbcc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cx.TagSvc.RejectTagName("solo", twin.ID); err != nil {
		t.Fatal(err)
	}
	wantNotIn(t, do(srv, "GET", fmt.Sprintf("/tags/%d/rules", plain.ID), hxRules).Body.String(), "[alias it]")

	w = do(srv, "GET", fmt.Sprintf("/tags/%d/rules", alias.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	if w.Body.Len() != 0 {
		t.Errorf("an alias row drew a path: %q", w.Body.String())
	}
	mustCode(t, do(srv, "GET", "/tags/99999/rules", hxRules), http.StatusNotFound)
}

func TestTagRulesPanel_NamesAMonloaderTooOldForRules(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/rules" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`{"version":"v-stub"}`))
	}))
	t.Cleanup(stub.Close)
	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "tok"
	tag := mustTag(t, srv.active().TagSvc, "old_peer", srv.active().GeneralCategoryID)

	body := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules).Body.String()
	wantIn(t, body, "monloader too old")
	wantIn(t, body, "monloader v1.14.0 or newer shows site lanes")
	wantNotIn(t, body, "unreachable")
}

func TestTagRulesPanel_DrawsTheAliasBand(t *testing.T) {
	answers := map[string]string{
		"miku":      `{"category_kept":true,"sites":[{"site":"danbooru","kind":"booru","rule":"drop","kept":false},{"site":"e621","kind":"booru","rule":"none","kept":true},{"site":"ptr","kind":"lookup","rule":"none","kept":true}]}`,
		"hatsune":   `{"category_kept":true,"sites":[{"site":"danbooru","kind":"booru","rule":"none","kept":true}]}`,
		"miku_hat":  `{"category_kept":true,"sites":[{"site":"danbooru","kind":"booru","rule":"none","kept":true}]}`,
		"hatsune_m": `{"category_kept":true,"sites":[{"site":"danbooru","kind":"booru","rule":"none","kept":true}]}`,
	}
	var asked []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/rules" {
			_, _ = w.Write([]byte(`{"version":"v-stub"}`))
			return
		}
		var body struct {
			Names []struct{ Tag, Category string } `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var out []string
		for _, n := range body.Names {
			asked = append(asked, n.Category+":"+n.Tag)
			out = append(out, answers[n.Tag])
		}
		_, _ = fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(out, ","))
	}))
	t.Cleanup(stub.Close)
	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "tok"
	cx := srv.active()
	charID := galleryio.LookupCategoryID(cx.DB, "character")
	tag := mustTag(t, cx.TagSvc, "hatsune_m", charID)
	for _, a := range []string{"miku", "hatsune"} {
		if _, err := cx.TagSvc.CreateAlias(a, charID, tag.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cx.TagSvc.CreateAlias("miku_hat", cx.GeneralCategoryID, tag.ID); err != nil {
		t.Fatal(err)
	}
	other := mustTag(t, cx.TagSvc, "miku", cx.GeneralCategoryID)

	w := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if len(asked) != 4 || asked[0] != "character:hatsune_m" {
		t.Errorf("asked = %v, want the tag then its three spellings in one call", asked)
	}
	for _, want := range []string{
		"as an alias", " · 3 other names", ">miku<", ">: <", "tag rule: ", ">drop<", "2 other sites",
		"any source", ">hatsune<", ">general:miku_hat<", "3 names → here",
		"general:miku", " exists  ", "[alias it]", fmt.Sprintf(`data-alias-into="%d"`, other.ID),
	} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, ", all character")

	long := strings.Repeat("hatsune_miku_long_", 5)
	if _, err := cx.TagSvc.CreateAlias(long, charID, tag.ID); err != nil {
		t.Fatal(err)
	}
	body = do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules).Body.String()
	wantIn(t, body, "<title>"+long)
}

func TestBlacklistConditionIsTheRestOfTheLine(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"watermark", ""},
		{"*mark", ""},
		{"watermark rating:explicit", "rating:explicit"},
		{"1boy general:watermark -solo", "1boy -solo"},
		{"*miku* 1boy", "*miku* 1boy"},
	} {
		if got := blacklistCondition(c.line, "watermark"); got != c.want {
			t.Errorf("blacklistCondition(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestTagRulesPanel_TaggerThresholdAndOffLanes(t *testing.T) {
	srv, modelPath := newModelServer(t,
		config.TaggerInstance{Name: "alpha", Enabled: true, ConfidenceThreshold: 0.35, CategoryThresholds: map[string]float64{"general": 0.6}, TagsFile: "tags.csv"},
		config.TaggerInstance{Name: "beta", Enabled: true, ConfidenceThreshold: 0.35, TagsFile: "tags.csv", Galleries: []string{}},
		config.TaggerInstance{Name: "gamma", Enabled: false, ConfidenceThreshold: 0.35, TagsFile: "tags.csv"},
		config.TaggerInstance{Name: "delta", Enabled: true, ConfidenceThreshold: 0.35, TagsFile: "tags.csv"},
	)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		installTagger(t, modelPath, name, "tag_id,name,category\n1,watermark,0\n")
	}
	installTagger(t, modelPath, "delta", "tag_id,name,category\n1,other,0\n")
	cx := srv.active()
	tag := mustTag(t, cx.TagSvc, "watermark", cx.GeneralCategoryID)

	w := do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		"1 tagger", ">1 mappings<", ">3 categories kept<", "Configure → mappings", "Configure → thresholds",
		">alpha<", "≥ 0.60", `href="/settings?tagger=alpha&amp;panel=thresholds" target="_blank" rel="noopener"`,
		">beta<", "off for this gallery ", `href="/settings?tagger=beta&amp;panel=galleries"`,
		">gamma<", "turned off ", `href="/settings#tagger" target="_blank" rel="noopener"`,
	} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, ">delta<")
}

func TestTagPathSVG_GalleryFrameStaysInsideTheDrawing(t *testing.T) {
	frame := regexp.MustCompile(`<rect x="([0-9.]+)" y="[0-9.]+" width="([0-9.]+)" height="[0-9.]+" fill="none" class="tp-slink" stroke-dasharray="2 3"`)
	tag := &models.Tag{Name: "watermark", CategoryName: "general"}
	for _, ml := range []string{"live", "unpaired"} {
		for _, avail := range []float64{0, 1700} {
			svg := string(tagPathSVG(tag, tagPath{Monloader: ml, GalleryName: "default", Category: "general"}, avail))
			m := frame.FindStringSubmatch(svg)
			if m == nil {
				t.Fatalf("%s: no gallery frame in %s", ml, svg)
			}
			x, _ := strconv.ParseFloat(m[1], 64)
			w, _ := strconv.ParseFloat(m[2], 64)
			if right, edge := x+w, tpColumns(ml == "live", avail).w; right > edge {
				t.Errorf("%s at %g: the frame ends at %g, past the drawing's %g", ml, avail, right, edge)
			}
		}
	}
}

func TestTpColumns_TakeTheWidthGiven(t *testing.T) {
	for _, ml := range []bool{true, false} {
		narrow := tpColumns(ml, 0)
		if narrow.w1 != tp1Min || narrow.gw != tpGMin {
			t.Errorf("ml=%v: the narrowest drawing has step 1 at %g and the gallery at %g", ml, narrow.w1, narrow.gw)
		}
		if got := tpColumns(ml, 600).w; got != narrow.w {
			t.Errorf("ml=%v: a narrow page gets %g, want the narrowest drawing %g", ml, got, narrow.w)
		}
		for _, avail := range []float64{1500, 1777.5} {
			c := tpColumns(ml, avail)
			if c.w != math.Floor(avail) {
				t.Errorf("ml=%v: %g wide gives a drawing of %g", ml, avail, c.w)
			}
			if c.w1 <= tp1Min || c.gw <= tpGMin {
				t.Errorf("ml=%v at %g: step 1 %g and the gallery %g did not grow", ml, avail, c.w1, c.gw)
			}
		}
		if got := tpColumns(ml, 5000).w; got != tpMaxW {
			t.Errorf("ml=%v: a very wide page gets %g, want %d", ml, got, tpMaxW)
		}
	}
}

func TestFitParts_CountsAWideGlyphAsAnEm(t *testing.T) {
	parts, title := fitParts([]pathPart{part("初音ミク", "fg")}, 5*12*tpCharW, 12)
	if title != "初音ミク" || len(parts) != 1 || parts[0].Text != "初音…" {
		t.Errorf("got %+v, %q", parts, title)
	}
	if lines := wrapParts([]pathPart{part("初音ミク", "fg"), part(", ", "fg"), part("初音未来", "fg")}, 12*12*tpCharW, 12); len(lines) != 2 {
		t.Errorf("two names of 6.7 cells each share a line of 12: %+v", lines)
	}
}

func TestWrapParts_KeepsTheCommaWithItsName(t *testing.T) {
	parts := []pathPart{part("aaaa", "fg"), part(", ", "fg"), part("bbbb", "fg"), part(", ", "fg"), part("cccc", "fg")}
	lines := wrapParts(parts, 9*12*tpCharW, 12)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines of 9 cells, got %+v", lines)
	}
	for _, ln := range lines[:2] {
		if got := ln[len(ln)-1].Text; got != "," {
			t.Errorf("a line ends on %q, want the bare comma", got)
		}
	}
	if parts[1].Text != ", " {
		t.Error("wrapping changed the run it was given")
	}
}

func TestSpans_PinTheFittedLength(t *testing.T) {
	var b svgWriter
	b.spans(0, 0, []pathPart{part("ab", "fg"), colored("初音", "#fff")}, 12, 100)
	wantIn(t, b.String(), `textLength="38.4" lengthAdjust="spacing"`)
	b.Reset()
	b.spans(0, 0, []pathPart{part("ab", "fg")}, 12, 0)
	wantNotIn(t, b.String(), "textLength")
}

func TestTagRulesPanel_DrawsToThePageWidth(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	tag := mustTag(t, cx.TagSvc, "watermark", cx.GeneralCategoryID)

	w := do(srv, "GET", fmt.Sprintf("/tags/%d/rules?w=1630", tag.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `<svg viewBox="0 0 1600 `)
	w = do(srv, "GET", fmt.Sprintf("/tags/%d/rules", tag.ID), hxRules)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`<svg viewBox="0 0 %g `, tpColumns(false, 0).w))
}
