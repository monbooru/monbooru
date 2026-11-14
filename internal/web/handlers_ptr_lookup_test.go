package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/tags"
)

func stubPTRGraph(t *testing.T, srv *Server, results map[string]any, asked *[]string) {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ptr/tags" {
			t.Errorf("ptr lookup hit %q, want /api/v1/ptr/tags", r.URL.Path)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad ptr/tags body: %v", err)
		}
		*asked = append(*asked, body.Tags...)
		out := map[string]any{}
		for _, name := range body.Tags {
			if res, ok := results[name]; ok {
				out[name] = res
			} else {
				out[name] = map[string]any{"known": false}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
	}))
	t.Cleanup(stub.Close)
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"
	srv.ml.Cache.Store(monloader.Status{Conn: peer.ConnOK, PTR: true})
}

func postPTRLookup(t *testing.T, srv *Server, path string, form url.Values) {
	t.Helper()
	w := postForm(t, srv, path, form)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%s: %d, %s", path, w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)
}

func TestPTRLookupTag_MergeKeepsTheLookedUpSpelling(t *testing.T) {
	srv := newTestServer(t)
	imgID := seedImage(t, srv, "ptrmerge.png", 6, 6)
	generalID := srv.active().GeneralCategoryID
	own := mustTag(t, srv.tagSvc(), "samus_aran_(metroid)", generalID)
	if _, err := srv.tagSvc().AddTagsToOneImage(imgID, []int64{own.ID}, ""); err != nil {
		t.Fatal(err)
	}

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"samus_aran": map[string]any{
			"known":        true,
			"ideal":        "samus_aran",
			"aliases":      []string{"samus"},
			"implications": []string{"metroid"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(own.ID, 10)+"/ptr-lookup",
		url.Values{"as": {"samus_aran"}, "merge": {"1"}})

	var isAlias int64
	var canonical sql.NullInt64
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias, canonical_tag_id FROM tags WHERE id = ?`, own.ID).Scan(&isAlias, &canonical); err != nil {
		t.Fatal(err)
	}
	if isAlias != 1 {
		t.Fatalf("the operator's spelling is still canonical")
	}
	target, err := srv.tagSvc().GetTag(canonical.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "samus_aran" {
		t.Fatalf("merged into %q, want samus_aran", target.Name)
	}
	if target.UsageCount != 1 {
		t.Errorf("target usage = %d, want 1", target.UsageCount)
	}

	if !slices.Contains(asked, "samus_aran") {
		t.Errorf("the sweep asked %v, want it to cover samus_aran", asked)
	}
	aliasOfTarget := countRows(t, srv.db(), "tags WHERE name = 'samus' AND is_alias = 1 AND canonical_tag_id = ?", target.ID)
	if aliasOfTarget != 1 {
		t.Errorf("the cluster did not land on the surviving tag")
	}
}

func TestPTRLookupTag_MergeIntoItselfFailsBeforeTheSweep(t *testing.T) {
	srv := newTestServer(t)
	own := mustTag(t, srv.tagSvc(), "samus_aran", srv.active().GeneralCategoryID)
	var asked []string
	stubPTRGraph(t, srv, nil, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(own.ID, 10)+"/ptr-lookup",
		url.Values{"as": {"samus_aran"}, "merge": {"1"}})

	if got := srv.jobs.Get().Error; got != tags.ErrMergeIntoSelf.Error() {
		t.Errorf("job error = %q, want %q", got, tags.ErrMergeIntoSelf.Error())
	}
	if len(asked) != 0 {
		t.Errorf("the sweep asked %v after a refused merge", asked)
	}
	if n := countRows(t, srv.db(), "tags WHERE id = ? AND is_alias = 0", own.ID); n != 1 {
		t.Error("the refused merge touched the tag")
	}
}

func TestPTRLookupTag_MergeNeedsASpelling(t *testing.T) {
	srv := newTestServer(t)
	tag := mustTag(t, srv.tagSvc(), "lone_tag", srv.active().GeneralCategoryID)
	srv.ml.Cache.Store(monloader.Status{Conn: peer.ConnOK, PTR: true})
	form := url.Values{"merge": {"1"}}
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-lookup", form)
	mustCode(t, w, http.StatusBadRequest)
}

func TestPTRLookupTag_RefusesAnAlias(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	canon := mustTag(t, srv.tagSvc(), "canon", generalID)
	spelled := mustCreateAlias(t, srv.tagSvc(), "spelled", generalID, canon.ID)
	var asked []string
	stubPTRGraph(t, srv, nil, &asked)

	w := postForm(t, srv, "/tags/"+strconv.FormatInt(spelled.ID, 10)+"/ptr-lookup",
		url.Values{"as": {"other"}, "merge": {"1"}})
	mustCode(t, w, http.StatusNotFound)
	awaitJobsDrain(t, srv)

	if n := countRows(t, srv.db(), "tags WHERE id = ? AND canonical_tag_id = ?", spelled.ID, canon.ID); n != 1 {
		t.Error("the refused pull repointed the alias")
	}
	if len(asked) != 0 {
		t.Errorf("the refused pull asked %v", asked)
	}
}

func TestPTRLookupTag_RefusesWhileThePTRIsNotReady(t *testing.T) {
	srv := newTestServer(t)
	tag := mustTag(t, srv.tagSvc(), "lone_tag", srv.active().GeneralCategoryID)
	var asked []string
	stubPTRGraph(t, srv, nil, &asked)
	srv.ml.Cache.Store(monloader.Status{Conn: peer.ConnOK})

	w := postForm(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-lookup", url.Values{})
	mustCode(t, w, http.StatusConflict)
	if len(asked) != 0 {
		t.Errorf("the refused pull asked %v", asked)
	}
}

func TestPTRLookupTag_AppliesAliasesAndImplications(t *testing.T) {
	srv := newTestServer(t)
	imgID := seedImage(t, srv, "ptr1.png", 6, 6)
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "1girl", generalID)
	if _, err := srv.tagSvc().AddTagsToOneImage(imgID, []int64{parent.ID}, ""); err != nil {
		t.Fatal(err)
	}

	taken := mustTag(t, srv.tagSvc(), "solo_girl", generalID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"1girl": map[string]any{
			"known":        true,
			"ideal":        "1girl",
			"aliases":      []string{"1girls", "solo_girl"},
			"implications": []string{"girl"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})

	var isAlias, canonical int64
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias, canonical_tag_id FROM tags WHERE name = '1girls'`).Scan(&isAlias, &canonical); err != nil {
		t.Fatalf("alias 1girls not created: %v", err)
	}
	if isAlias != 1 || canonical != parent.ID {
		t.Errorf("1girls = (alias=%d, canonical=%d), want alias of %d", isAlias, canonical, parent.ID)
	}
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias FROM tags WHERE id = ?`, taken.ID).Scan(&isAlias); err != nil {
		t.Fatal(err)
	}
	if isAlias != 0 {
		t.Error("the existing solo_girl tag must not be repointed into an alias")
	}
	n := countRows(t, srv.db(), "tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id WHERE ti.parent_tag_id = ? AND t.name = 'girl'", parent.ID)
	if n != 1 {
		t.Errorf("implication 1girl -> girl edges = %d, want 1", n)
	}
	n = countRows(t, srv.db(), `image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'girl' AND it.is_implied = 1`, imgID)
	if n != 1 {
		t.Errorf("implied girl rows on the image = %d, want 1 (fan-out missing)", n)
	}
}

func TestPTRLookupTag_FlagsDroppedRelationsStale(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "1girl", generalID)
	full := map[string]any{
		"known":        true,
		"ideal":        "1girl",
		"aliases":      []string{"1girls", "one_girl"},
		"implications": []string{"girl", "female"},
	}
	var asked []string
	results := map[string]any{"1girl": full}
	stubPTRGraph(t, srv, results, &asked)
	lookupPath := "/tags/" + strconv.FormatInt(parent.ID, 10) + "/ptr-lookup"

	aliasStale := func(name string) int {
		t.Helper()
		var s int
		if err := srv.db().Read.QueryRow(`SELECT stale FROM tags WHERE name = ?`, name).Scan(&s); err != nil {
			t.Fatalf("alias %s: %v", name, err)
		}
		return s
	}
	implStale := func(name string) int {
		t.Helper()
		var s int
		if err := srv.db().Read.QueryRow(
			`SELECT ti.stale FROM tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id
			 WHERE ti.parent_tag_id = ? AND t.name = ?`, parent.ID, name).Scan(&s); err != nil {
			t.Fatalf("implication %s: %v", name, err)
		}
		return s
	}

	postPTRLookup(t, srv, lookupPath, url.Values{})
	if aliasStale("one_girl") != 0 || implStale("female") != 0 {
		t.Fatal("fresh pull must not flag anything")
	}

	results["1girl"] = map[string]any{
		"known": true, "ideal": "1girl",
		"aliases": []string{"1girls"}, "implications": []string{"girl"},
	}
	postPTRLookup(t, srv, lookupPath, url.Values{})
	if aliasStale("one_girl") != 1 || aliasStale("1girls") != 0 {
		t.Errorf("alias stale = one_girl:%d 1girls:%d, want 1 and 0", aliasStale("one_girl"), aliasStale("1girls"))
	}
	if implStale("female") != 1 || implStale("girl") != 0 {
		t.Errorf("implication stale = female:%d girl:%d, want 1 and 0", implStale("female"), implStale("girl"))
	}

	results["1girl"] = full
	postPTRLookup(t, srv, lookupPath, url.Values{})
	if aliasStale("one_girl") != 0 || implStale("female") != 0 {
		t.Error("re-listed relations should shed their stale flag")
	}

	delete(results, "1girl")
	postPTRLookup(t, srv, lookupPath, url.Values{})
	for _, name := range []string{"1girls", "one_girl"} {
		if aliasStale(name) != 1 {
			t.Errorf("alias %s stale = %d after the PTR forgot the tag, want 1", name, aliasStale(name))
		}
	}
	for _, name := range []string{"girl", "female"} {
		if implStale(name) != 1 {
			t.Errorf("implication %s stale = %d after the PTR forgot the tag, want 1", name, implStale(name))
		}
	}
}

func TestPTRLookupTag_ReportsNonRepresentableSpellings(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "1girl", generalID)
	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"1girl": map[string]any{
			"known":   true,
			"ideal":   "1girl",
			"aliases": []string{"1girls", "bad*wolf"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})

	wantIn(t, srv.jobs.Get().Summary, "1 spelling not representable")
	var n int
	if err := srv.db().Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE name = '1girls'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("representable alias 1girls not created (n=%d, err=%v)", n, err)
	}
}

func TestPTRLookupTag_UnavailableStopsWithoutFailing(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "1girl", generalID)

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"ptr_unavailable"}`))
	}))
	defer stub.Close()
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"
	srv.ml.Cache.Store(monloader.Status{Conn: peer.ConnOK, PTR: true})

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-lookup", url.Values{})

	state := srv.jobs.Get()
	if state.Error != "" {
		t.Fatalf("job failed with %q, want a completed stop", state.Error)
	}
	wantIn(t, state.Summary, "unavailable")
}

func TestPTRLookupTag_CategoryQualifiedTokens(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "1girl", generalID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"1girl": map[string]any{
			"known":        true,
			"ideal":        "1girl",
			"aliases":      []string{"character:foo"},
			"implications": []string{"copyright:bar"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})

	charID := galleryio.LookupCategoryID(srv.db(), "character")
	var isAlias, canonical int64
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias, canonical_tag_id FROM tags WHERE name = 'foo' AND category_id = ?`, charID,
	).Scan(&isAlias, &canonical); err != nil {
		t.Fatalf("alias foo not created in the character category: %v", err)
	}
	if isAlias != 1 || canonical != parent.ID {
		t.Errorf("foo = (alias=%d, canonical=%d), want a character-category alias of %d", isAlias, canonical, parent.ID)
	}

	copyID := galleryio.LookupCategoryID(srv.db(), "copyright")
	n := countRows(t, srv.db(), "tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id WHERE ti.parent_tag_id = ? AND t.name = 'bar' AND t.category_id = ?", parent.ID, copyID)
	if n != 1 {
		t.Errorf("implication 1girl -> copyright:bar edges = %d, want 1", n)
	}
}

func TestSplitCategoryTag(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	charID := galleryio.LookupCategoryID(cx.DB, "character")
	cases := []struct {
		in       string
		wantCat  int64
		wantBare string
		wantOK   bool
	}{
		{"character:foo", charID, "foo", true},
		{"nier:automata", cx.GeneralCategoryID, "nier:automata", true},
		{"1girl", cx.GeneralCategoryID, "1girl", true},
		{"", 0, "", false},
	}
	for _, tc := range cases {
		cat, bare, ok := srv.tagSvc().ParseForm(tc.in)
		if cat != tc.wantCat || bare != tc.wantBare || ok != tc.wantOK {
			t.Errorf("splitCategoryTag(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tc.in, cat, bare, ok, tc.wantCat, tc.wantBare, tc.wantOK)
		}
	}
}

func TestPTRLookupTag_SweepsCreatedImpliedTags(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "1girl", generalID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"1girl": map[string]any{
			"known":        true,
			"ideal":        "1girl",
			"aliases":      []string{"1girls"},
			"implications": []string{"girl"},
		},
		"girl": map[string]any{
			"known":   true,
			"ideal":   "girl",
			"aliases": []string{"female_child"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})
	if st := srv.jobs.Get(); !strings.Contains(st.Summary, "added 2 aliases and 1 implication") {
		t.Errorf("first run summary = %q, want the implied tag's alias counted in the same run", st.Summary)
	}
	var isAlias int
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias FROM tags WHERE name = 'female_child'`).Scan(&isAlias); err != nil || isAlias != 1 {
		t.Fatalf("alias of the created implied tag after one run: is_alias=%d, err=%v", isAlias, err)
	}

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})
	if st := srv.jobs.Get(); !strings.Contains(st.Summary, "added 0 aliases and 0 implications") {
		t.Errorf("re-run summary = %q, want nothing new", st.Summary)
	}
}

func TestPTRLookupTag_PullsImpliedBy(t *testing.T) {
	srv := newTestServer(t)
	imgID := seedImage(t, srv, "impliedby.png", 7, 7)
	generalID := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "hatsune_miku_(append)", generalID)
	if _, err := srv.tagSvc().AddTagsToOneImage(imgID, []int64{parent.ID}, ""); err != nil {
		t.Fatal(err)
	}
	child := mustTag(t, srv.tagSvc(), "hatsune_miku", generalID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"hatsune_miku": map[string]any{
			"known":      true,
			"ideal":      "hatsune_miku",
			"implied_by": []string{"hatsune_miku_(append)"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(child.ID, 10)+"/ptr-lookup", url.Values{})

	n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?", parent.ID, child.ID)
	if n != 1 {
		t.Errorf("implied-by edge parent -> child = %d, want 1", n)
	}
	n = countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", imgID, child.ID)
	if n != 1 {
		t.Errorf("child fanned onto the parent's image = %d, want 1", n)
	}
}

func TestPTRLookupTag_SkipsRelationsOnLocalAliases(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	futanari := mustTag(t, srv.tagSvc(), "futanari", generalID)
	solo := mustTag(t, srv.tagSvc(), "solo", generalID)
	mustCreateAlias(t, srv.tagSvc(), "solo_futanari", generalID, futanari.ID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"solo": map[string]any{
			"known":      true,
			"ideal":      "solo",
			"implied_by": []string{"solo_futanari"},
		},
		"futanari": map[string]any{
			"known":        true,
			"ideal":        "futanari",
			"implications": []string{"solo_futanari"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(solo.ID, 10)+"/ptr-lookup", url.Values{})
	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(futanari.ID, 10)+"/ptr-lookup", url.Values{})

	n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?", futanari.ID, futanari.ID)
	if n != 0 {
		t.Errorf("edges touching the alias canonical = %d, want 0: the PTR declared none of them", n)
	}
}

func TestPTRLookupTag_ImpliedByParentNotSwept(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	child := mustTag(t, srv.tagSvc(), "solo", generalID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"solo": map[string]any{
			"known":      true,
			"ideal":      "solo",
			"implied_by": []string{"solo_futanari"},
		},
		"solo_futanari": map[string]any{
			"known":        true,
			"ideal":        "solo_futanari",
			"aliases":      []string{"futanari_solo"},
			"implications": []string{"futanari", "solo"},
			"implied_by":   []string{"solo_dickgirl"},
		},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(child.ID, 10)+"/ptr-lookup", url.Values{})

	n := countRows(t, srv.db(), "tag_implications ti JOIN tags p ON p.id = ti.parent_tag_id WHERE ti.implied_tag_id = ? AND p.name = 'solo_futanari'", child.ID)
	if n != 1 {
		t.Errorf("implied-by edge solo_futanari -> solo = %d, want 1", n)
	}

	for _, leaked := range []string{"futanari", "futanari_solo", "solo_dickgirl"} {
		n = countRows(t, srv.db(), "tags WHERE name = ?", leaked)
		if n != 0 {
			t.Errorf("pulling solo cascaded in %q (%d rows); an implied-by parent must not be swept", leaked, n)
		}
	}
}

func TestPTRLookupSearch_SkipsAliasAndRatingRows(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	canonical := mustTag(t, srv.tagSvc(), "blonde_hair", generalID)
	mustCreateAlias(t, srv.tagSvc(), "blond_hair", generalID, canonical.ID)

	var asked []string
	stubPTRGraph(t, srv, map[string]any{}, &asked)

	postPTRLookup(t, srv, "/tags/ptr-lookup-search", url.Values{"show_zero": {"1"}})

	found := map[string]int{}
	for i, name := range asked {
		if _, seen := found[name]; !seen {
			found[name] = i
		}
	}
	if _, ok := found["blonde_hair"]; !ok {
		t.Errorf("canonical tag missing from the sweep: %v", asked)
	}

	if i, ok := found["blond_hair"]; !ok || i <= found["blonde_hair"] {
		t.Errorf("alias should be asked only as a fallback after its canonical: %v", asked)
	}
	for _, rating := range []string{"rating:general", "rating:explicit", "general", "explicit"} {
		if _, ok := found[rating]; ok {
			t.Errorf("rating row %q must be skipped", rating)
		}
	}
}

func TestPTRLookupTag_ResolvesThroughAlias(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "samus_aran_(metroid)", generalID)
	if _, err := srv.tagSvc().CreateAliasFrom("samus_aran", generalID, tag.ID, "ptr"); err != nil {
		t.Fatal(err)
	}

	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"samus_aran": map[string]any{
			"known":        true,
			"ideal":        "samus_aran",
			"aliases":      []string{"samus"},
			"implications": []string{"metroid"},
		},
		"metroid": map[string]any{"known": true, "ideal": "metroid"},
	}, &asked)

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-lookup", url.Values{})

	var canonical, stale int64
	if err := srv.db().Read.QueryRow(
		`SELECT canonical_tag_id FROM tags WHERE name = 'samus' AND is_alias = 1`).Scan(&canonical); err != nil {
		t.Fatalf("alias samus not created through the fallback: %v", err)
	}
	if canonical != tag.ID {
		t.Errorf("samus points at %d, want %d", canonical, tag.ID)
	}
	if err := srv.db().Read.QueryRow(`SELECT stale FROM tags WHERE name = 'samus_aran'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Error("the answering alias must not be flagged stale by its own answer")
	}
	n := countRows(t, srv.db(), "tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id WHERE ti.parent_tag_id = ? AND t.name = 'metroid'", tag.ID)
	if n != 1 {
		t.Errorf("implication through the alias = %d, want 1", n)
	}
	wantIn(t, srv.jobs.Get().Summary, "added 1 alias and 1 implication")
	wantNotIn(t, srv.jobs.Get().Summary, "unknown to the PTR")
}

func TestPTRLookupTag_AdoptsIdeal(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "1girl", generalID)
	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"1girl": map[string]any{"known": true, "ideal": "1_female"},
	}, &asked)

	path := "/tags/" + strconv.FormatInt(tag.ID, 10) + "/ptr-lookup"
	postPTRLookup(t, srv, path, url.Values{})
	postPTRLookup(t, srv, path, url.Values{})

	var canonical, stale int64
	var origin string
	if err := srv.db().Read.QueryRow(
		`SELECT canonical_tag_id, origin, stale FROM tags WHERE name = '1_female' AND is_alias = 1`).Scan(&canonical, &origin, &stale); err != nil {
		t.Fatalf("ideal not adopted as an alias: %v", err)
	}
	if canonical != tag.ID || origin != "ptr" || stale != 0 {
		t.Errorf("1_female = (canonical %d, origin %q, stale %d), want (%d, ptr, 0)", canonical, origin, stale, tag.ID)
	}
}

func TestPTRLookupTag_As(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "samus_aran_(metroid)", generalID)
	var asked []string
	stubPTRGraph(t, srv, map[string]any{
		"samus_aran": map[string]any{"known": true, "ideal": "samus_aran", "aliases": []string{"samus"}},
	}, &asked)
	path := "/tags/" + strconv.FormatInt(tag.ID, 10) + "/ptr-lookup"

	w := postForm(t, srv, path, url.Values{"as": {`"*"`}})
	mustCode(t, w, http.StatusBadRequest)

	postPTRLookup(t, srv, path, url.Values{"as": {" Samus_Aran "}})
	if strings.Join(asked, " ") != "samus_aran" {
		t.Errorf("asked %v, want the looked-up spelling alone", asked)
	}
	for _, name := range []string{"samus_aran", "samus"} {
		var canonical int64
		if err := srv.db().Read.QueryRow(
			`SELECT canonical_tag_id FROM tags WHERE name = ? AND is_alias = 1`, name).Scan(&canonical); err != nil {
			t.Fatalf("alias %s not adopted: %v", name, err)
		}
		if canonical != tag.ID {
			t.Errorf("%s points at %d, want %d", name, canonical, tag.ID)
		}
	}
	if got := srv.jobs.Get().Summary; !strings.Contains(got, "added 2 aliases") || !strings.Contains(got, "looked up as samus_aran") {
		t.Errorf("summary = %q", got)
	}
}
