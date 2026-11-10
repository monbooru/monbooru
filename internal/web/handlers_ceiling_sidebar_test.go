package web

import (
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestRelationsHub_PhashMissingHonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	seedRatedPair(t, srv)

	mustExec(t, srv.db().Write, `UPDATE images SET phash = NULL`)

	body := mustGetBody(t, srv, "/relations")
	if !strings.Contains(body, "<strong>2</strong>") || !strings.Contains(body, "without a perceptual hash") {
		t.Errorf("no-ceiling: expected 2 image(s) without a perceptual hash; body slice: %s", phashMissingSlice(body))
	}

	w := do(srv, "GET", "/relations", [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
	mustCode(t, w, http.StatusOK)
	body = w.Body.String()
	if !strings.Contains(body, "<strong>1</strong>") || !strings.Contains(body, "without a perceptual hash") {
		t.Errorf("ceiling=sensitive: expected 1 image without a perceptual hash; body slice: %s", phashMissingSlice(body))
	}
}

func phashMissingSlice(body string) string {
	return sliceAround(body, "without a perceptual hash", 200)
}

func sliceAround(body, needle string, span int) string {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return "(needle not found: " + needle + ")"
	}
	return body[max(idx-100, 0):min(idx+span, len(body))]
}

func TestGallery_HiddenByCeilingIndicator(t *testing.T) {
	srv := newTestServer(t)
	seedRatedPair(t, srv)

	body := mustGetBody(t, srv, "/")
	if !strings.Contains(body, `<span id="footer-hidden" class="footer-hidden"></span>`) {
		t.Errorf("no-ceiling: footer cell should be empty; got: %s", footerHiddenSlice(body))
	}
	if !strings.Contains(body, `class="result-count">2</span>`) {
		t.Errorf("no-ceiling: expected 2 matches; got: %s", statusSlice(body))
	}

	w := do(srv, "GET", "/", [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
	body = w.Body.String()
	if !strings.Contains(body, `class="result-count">1</span>`) {
		t.Errorf("ceiling=sensitive: expected 1 match; got: %s", statusSlice(body))
	}
	if !strings.Contains(body, `id="footer-hidden"`) || !strings.Contains(body, `1 hidden<span class="footer-wide"> image in the current search`) {
		t.Errorf("ceiling=sensitive: expected footer hidden indicator with 1; got: %s", footerHiddenSlice(body))
	}
}

func TestGallery_HiddenByCeilingIndicator_FilteredQuery(t *testing.T) {
	raiseHiddenIndicatorBudget(t)
	srv := newTestServer(t)
	cx := srv.active()
	safe, explicit := seedRatedPair(t, srv)

	generalCat := cx.GeneralCategoryID
	tag := mustTag(t, cx.TagSvc, "shared", generalCat)
	mustAddTag(t, cx.TagSvc, safe, tag.ID, false, nil)
	mustAddTag(t, cx.TagSvc, explicit, tag.ID, false, nil)
	cx.InvalidateCaches()

	w := do(srv, "GET", "/?q=shared", [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.Contains(body, `class="result-count">1</span>`) {
		t.Errorf("expected 1 match (general only); got: %s", statusSlice(body))
	}
	if strings.Contains(footerHiddenSlice(body), "hidden<") {
		t.Errorf("the render should leave the hidden count to the fragment; got: %s", footerHiddenSlice(body))
	}
	frag := countFragment(t, srv, body, [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
	if !strings.Contains(frag, `id="footer-hidden"`) || !strings.Contains(frag, `1 hidden<span class="footer-wide"> image in the current search`) {
		t.Errorf("expected 1 hidden footer indicator; got: %s", frag)
	}
	if strings.Contains(frag, `id="result-matches"`) {
		t.Errorf("a known total should not be swapped again; got: %s", frag)
	}
}

func TestGallery_HiddenByCeilingIndicator_KeptUntilTheCachesDrop(t *testing.T) {
	raiseHiddenIndicatorBudget(t)
	srv := newTestServer(t)
	cx := srv.active()
	safe, explicit := seedRatedPair(t, srv)
	tag := mustTag(t, cx.TagSvc, "shared", cx.GeneralCategoryID)
	mustAddTag(t, cx.TagSvc, safe, tag.ID, false, nil)
	mustAddTag(t, cx.TagSvc, explicit, tag.ID, false, nil)
	cx.InvalidateCaches()
	hidden := func() string {
		t.Helper()
		cookie := [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"}
		w := do(srv, "GET", "/?q=shared", cookie)
		mustCode(t, w, http.StatusOK)
		return countFragment(t, srv, w.Body.String(), cookie)
	}

	if got := hidden(); !strings.Contains(got, `1 hidden<span class="footer-wide"> image in the current search`) {
		t.Fatalf("expected 1 hidden; got: %s", got)
	}
	mustExec(t, srv.db().Write, `UPDATE images SET rating_rank = (SELECT rating_rank FROM images WHERE id = ?) WHERE id = ?`, explicit, safe)
	if got := hidden(); !strings.Contains(got, `1 hidden<span class="footer-wide"> image in the current search`) {
		t.Errorf("before the caches drop: expected the kept 1 hidden; got: %s", got)
	}
	cx.InvalidateCaches()
	if got := hidden(); !strings.Contains(got, `2 hidden<span class="footer-wide"> images in the current search`) {
		t.Errorf("after the caches drop: expected 2 hidden; got: %s", got)
	}
}

func TestGallery_HiddenByCeilingIndicator_BoundedTotal(t *testing.T) {
	raiseHiddenIndicatorBudget(t)
	srv := newTestServer(t)
	cx := srv.active()
	generalCat := cx.GeneralCategoryID
	tagA := mustTag(t, cx.TagSvc, "bound_a", generalCat)
	tagB := mustTag(t, cx.TagSvc, "bound_b", generalCat)
	for i, rating := range []string{"general", "general", "general", "explicit", "explicit"} {
		id := seedImage(t, srv, "bounded_"+strconv.Itoa(i)+".png", 10+i, 10)
		mustAddTag(t, cx.TagSvc, id, ratingTagIDWeb(t, cx.DB, rating), false, nil)
		mustAddTag(t, cx.TagSvc, id, tagA.ID, false, nil)
		mustAddTag(t, cx.TagSvc, id, tagB.ID, false, nil)
	}

	mustExec(t, srv.db().Write, `UPDATE tags SET usage_count = 50001 WHERE id IN (?, ?)`, tagA.ID, tagB.ID)
	cx.InvalidateCaches()

	cookie := [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"}
	w := do(srv, "GET", "/?q=bound_a+bound_b", cookie)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.Contains(body, `class="result-count">3</span>`) {
		t.Errorf("expected 3 matches; got: %s", statusSlice(body))
	}
	if frag := countFragment(t, srv, body, cookie); !strings.Contains(frag, `2 hidden<span class="footer-wide"> images in the current search`) {
		t.Errorf("expected 2 hidden; got: %s", frag)
	}
}

func TestGallery_ASlowCountLoadsAfterTheGrid(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 2
	for i := range 5 {
		seedImage(t, srv, "slow_"+strconv.Itoa(i)+".png", 10+i, 10)
	}

	w := do(srv, "GET", "/?q=width:>0&page=2")
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.Contains(body, `class="result-count">…</span> matches`) {
		t.Errorf("expected the pending count; got: %s", statusSlice(body))
	}
	if !strings.Contains(body, `Page 2 / …`) || strings.Contains(body, `Last »`) || !strings.Contains(body, `Next →`) {
		t.Errorf("expected a pager with Next and no Last; got: %s", sliceAround(body, `id="gallery-pager-top"`, 600))
	}
	frag := countFragment(t, srv, body)
	if !strings.Contains(frag, `class="result-count">5</span> matches`) {
		t.Errorf("expected the fragment to carry 5 matches; got: %s", frag)
	}
	if !strings.Contains(frag, `id="gallery-pager-top" hx-swap-oob="true"`) || !strings.Contains(frag, `Page 2 / 3`) || !strings.Contains(frag, `Last »`) {
		t.Errorf("expected both pagers with the total; got: %s", frag)
	}
}

var countURLRe = regexp.MustCompile(`hx-get="(/internal/search/count\?[^"]*)"`)

func countFragment(t *testing.T, srv *Server, galleryBody string, extra ...[2]string) string {
	t.Helper()
	m := countURLRe.FindStringSubmatch(galleryBody)
	if m == nil {
		t.Fatalf("no count fragment in the status bar: %s", statusSlice(galleryBody))
	}
	w := do(srv, "GET", html.UnescapeString(m[1]), extra...)
	mustCode(t, w, http.StatusOK)
	return w.Body.String()
}

func statusSlice(body string) string {
	return sliceAround(body, `id="gallery-status"`, 300)
}

func footerHiddenSlice(body string) string {
	return sliceAround(body, `id="footer-hidden"`, 300)
}

func ceilingGet(t *testing.T, srv *Server, path, ceiling string) string {
	t.Helper()
	var extra [][2]string
	if ceiling != "" {
		extra = append(extra, [2]string{"Cookie", "monbooru_rating_ceiling=" + ceiling})
	}
	w := do(srv, "GET", path, extra...)
	mustCode(t, w, http.StatusOK)
	return w.Body.String()
}

func TestRelationsHub_DeclaredCountersHonourCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	rel := cx.RelationsSvc

	dim := 7
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}
	tag := func(id int64, level string) {
		mustAddTag(t, cx.TagSvc, id, ratingTagIDWeb(t, cx.DB, level), false, nil)
	}

	dupSafeA, dupSafeB := mkImg("dup_safe_a.png"), mkImg("dup_safe_b.png")
	dupTaintA, dupTaintB := mkImg("dup_taint_a.png"), mkImg("dup_taint_b.png")
	tag(dupTaintB, "explicit")
	if err := rel.AddDuplicate(dupSafeA, dupSafeB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDuplicate(dupTaintA, dupTaintB); err != nil {
		t.Fatal(err)
	}

	altSafeA, altSafeB := mkImg("alt_safe_a.png"), mkImg("alt_safe_b.png")
	altTaintA, altTaintB := mkImg("alt_taint_a.png"), mkImg("alt_taint_b.png")
	tag(altTaintB, "explicit")
	if err := rel.AddAlternate(altSafeA, altSafeB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddAlternate(altTaintA, altTaintB); err != nil {
		t.Fatal(err)
	}

	verSafeP, verSafeC := mkImg("ver_safe_p.png"), mkImg("ver_safe_c.png")
	verTaintP, verTaintC := mkImg("ver_taint_p.png"), mkImg("ver_taint_c.png")
	tag(verTaintC, "explicit")
	if err := rel.AddVersionEdge(verSafeP, verSafeC); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddVersionEdge(verTaintP, verTaintC); err != nil {
		t.Fatal(err)
	}

	derSafeS, derSafeD := mkImg("der_safe_s.png"), mkImg("der_safe_d.png")
	derTaintS, derTaintD := mkImg("der_taint_s.png"), mkImg("der_taint_d.png")
	tag(derTaintD, "explicit")
	if err := rel.AddDerivativeEdge(derSafeS, derSafeD); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDerivativeEdge(derTaintS, derTaintD); err != nil {
		t.Fatal(err)
	}

	nrSafeA, nrSafeB := mkImg("nr_safe_a.png"), mkImg("nr_safe_b.png")
	nrTaintA, nrTaintB := mkImg("nr_taint_a.png"), mkImg("nr_taint_b.png")
	tag(nrTaintB, "explicit")
	if err := rel.AddNotRelated(nrSafeA, nrSafeB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddNotRelated(nrTaintA, nrTaintB); err != nil {
		t.Fatal(err)
	}

	body := ceilingGet(t, srv, "/relations", "")
	for _, want := range []string{
		`<strong>2</strong> same-image group`,
		`<strong>2</strong> variant group`,
		`<strong>2</strong> revision chain`,
		`<strong>2</strong> based-on tree`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no-ceiling: expected %q on /relations; slice: %s", want, sliceAround(body, "Browse relations", 600))
		}
	}

	body = ceilingGet(t, srv, "/relations", "sensitive")
	for _, want := range []string{
		`<strong>1</strong> same-image group`,
		`<strong>1</strong> variant group`,
		`<strong>1</strong> revision chain`,
		`<strong>1</strong> based-on tree`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ceiling=sensitive: expected %q on /relations; slice: %s", want, sliceAround(body, "Browse relations", 600))
		}
	}

	browse := ceilingGet(t, srv, "/relations/browse?kind=not_related", "sensitive")
	if !strings.Contains(browse, `Not related <span class="field-hint">(1)`) {
		t.Errorf("ceiling=sensitive: expected Not related (1) tab label; slice: %s", sliceAround(browse, "Not related", 200))
	}
	browse = ceilingGet(t, srv, "/relations/browse?kind=not_related", "")
	if !strings.Contains(browse, `Not related <span class="field-hint">(2)`) {
		t.Errorf("no-ceiling: expected Not related (2) tab label; slice: %s", sliceAround(browse, "Not related", 200))
	}
}

func TestRelationsHub_ChainTreeCountsMatchBrowseCards(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	rel := cx.RelationsSvc

	dim := 11
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}
	tag := func(id int64, level string) {
		mustAddTag(t, cx.TagSvc, id, ratingTagIDWeb(t, cx.DB, level), false, nil)
	}

	chainA, chainB, chainC := mkImg("chain_a.png"), mkImg("chain_b.png"), mkImg("chain_c.png")
	if err := rel.AddVersionEdge(chainA, chainB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddVersionEdge(chainB, chainC); err != nil {
		t.Fatal(err)
	}

	treeSrc, treeD1, treeD2 := mkImg("tree_src.png"), mkImg("tree_d1.png"), mkImg("tree_d2.png")
	if err := rel.AddDerivativeEdge(treeSrc, treeD1); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDerivativeEdge(treeSrc, treeD2); err != nil {
		t.Fatal(err)
	}

	body := ceilingGet(t, srv, "/relations", "")
	if !strings.Contains(body, `<strong>1</strong> revision chain`) {
		t.Errorf("no-ceiling: expected 1 revision chain; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	if !strings.Contains(body, `<strong>1</strong> based-on tree`) {
		t.Errorf("no-ceiling: expected 1 based-on tree; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	browse := ceilingGet(t, srv, "/relations/browse?kind=version", "")
	if !strings.Contains(browse, `Revision history <span class="field-hint">(1)`) {
		t.Errorf("no-ceiling: expected Revision history (1) tab; slice: %s", sliceAround(browse, "Revision history", 200))
	}
	browse = ceilingGet(t, srv, "/relations/browse?kind=derivative", "")
	if !strings.Contains(browse, `Based on <span class="field-hint">(1)`) {
		t.Errorf("no-ceiling: expected Based on (1) tab; slice: %s", sliceAround(browse, "Based on", 200))
	}

	tag(chainB, "explicit")
	tag(treeD1, "explicit")

	body = ceilingGet(t, srv, "/relations", "sensitive")
	if !strings.Contains(body, `<strong>0</strong> revision chain`) {
		t.Errorf("ceiling=sensitive: expected 0 revision chain; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	if !strings.Contains(body, `<strong>0</strong> based-on tree`) {
		t.Errorf("ceiling=sensitive: expected 0 based-on tree; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	browse = ceilingGet(t, srv, "/relations/browse?kind=version", "sensitive")
	if !strings.Contains(browse, `Revision history <span class="field-hint">(0)`) {
		t.Errorf("ceiling=sensitive: expected Revision history (0) tab; slice: %s", sliceAround(browse, "Revision history", 200))
	}
	browse = ceilingGet(t, srv, "/relations/browse?kind=derivative", "sensitive")
	if !strings.Contains(browse, `Based on <span class="field-hint">(0)`) {
		t.Errorf("ceiling=sensitive: expected Based on (0) tab; slice: %s", sliceAround(browse, "Based on", 200))
	}
}

func TestSameCollection_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)
	self := seedImage(t, srv, "self.png", 12, 12)
	for _, id := range []int64{self, safe, explicit} {
		mustAddCollection(t, srv.db(), id, "Saga", nil)
	}
	explicitTile := fmt.Sprintf(`/thumbnails/%s/%d.jpg`, srv.activeGallery(), explicit)
	safeTile := fmt.Sprintf(`/thumbnails/%s/%d.jpg`, srv.activeGallery(), safe)
	for _, path := range []string{
		fmt.Sprintf("/internal/images/%d/related-entries", self),
		fmt.Sprintf("/images/%d/relations", self),
	} {
		w := do(srv, "GET", path)
		mustCode(t, w, http.StatusOK)
		wantIn(t, w.Body.String(), explicitTile)

		w = do(srv, "GET", path, [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
		mustCode(t, w, http.StatusOK)
		body := w.Body.String()
		wantNotIn(t, body, explicitTile)
		wantIn(t, body, safeTile)
	}
}
