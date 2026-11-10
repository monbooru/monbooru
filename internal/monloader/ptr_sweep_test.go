package monloader

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

func TestResolvePTRSpelling(t *testing.T) {
	graph := map[string]contract.PTRTagInfo{
		"blond_hair":  {Known: true, Ideal: "yellow_hair"},
		"yellow_hair": {Known: true, Ideal: "yellow_hair", Aliases: []string{"blond_hair"}},
	}

	graph["blonde_hair"] = contract.PTRTagInfo{Known: true, Ideal: "blonde_hair", Implications: []string{"hair"}}
	if got, _, ok := resolvePTRSpelling(graph, "blonde_hair", []string{"blond_hair"}); !ok || got != "blonde_hair" {
		t.Errorf("own name known: got %q ok=%v", got, ok)
	}

	graph["blonde_hair"] = contract.PTRTagInfo{Known: true, Ideal: "blonde_hair"}
	if got, _, ok := resolvePTRSpelling(graph, "blonde_hair", []string{"blond_hair"}); !ok || got != "blond_hair" {
		t.Errorf("orphan own name shadowed the alias: got %q ok=%v", got, ok)
	}

	if got, _, ok := resolvePTRSpelling(graph, "blonde_hair", []string{"golden_hair"}); !ok || got != "blonde_hair" {
		t.Errorf("bare own name: got %q ok=%v", got, ok)
	}
	delete(graph, "blonde_hair")

	if got, _, ok := resolvePTRSpelling(graph, "blonde_hair", []string{"blond_hair", "yellow_hair"}); !ok || got != "yellow_hair" {
		t.Errorf("ideal-preferred alias: got %q ok=%v", got, ok)
	}
	if got, _, ok := resolvePTRSpelling(graph, "blonde_hair", []string{"blond_hair"}); !ok || got != "blond_hair" {
		t.Errorf("first known alias: got %q ok=%v", got, ok)
	}
	if _, _, ok := resolvePTRSpelling(graph, "blonde_hair", []string{"golden_hair"}); ok {
		t.Error("nothing known must not resolve")
	}
}

type sweepStub struct {
	graph    map[string]contract.PTRTagInfo
	calls    [][]string
	conflict int
	hold     func(r *http.Request)
}

func sweepEnv(t *testing.T, stub *sweepStub, onChange func()) (*Link, *tags.Service) {
	t.Helper()
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tags []string `json:"tags"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		stub.calls = append(stub.calls, req.Tags)
		if stub.hold != nil {
			stub.hold(r)
		}
		if len(stub.calls) == stub.conflict {
			w.WriteHeader(http.StatusConflict)
			return
		}
		results := map[string]contract.PTRTagInfo{}
		for _, name := range req.Tags {
			if info, ok := stub.graph[name]; ok {
				results[name] = info
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	cx := testGallery(t)
	if err := l.jobs.Start("ptr-lookup"); err != nil {
		t.Fatal(err)
	}
	return l, tags.New(cx.DB, onChange)
}

func sweepTag(t *testing.T, svc *tags.Service, form string) int64 {
	t.Helper()
	catID, bare, ok := svc.ParseForm(form)
	if !ok {
		t.Fatalf("parse %q", form)
	}
	tag, err := svc.GetOrCreateTag(bare, catID)
	if err != nil {
		t.Fatal(err)
	}
	return tag.ID
}

func canonicalOf(t *testing.T, svc *tags.Service, form string) string {
	t.Helper()
	catID, bare, _ := svc.ParseForm(form)
	tag, err := svc.FindTag(bare, catID)
	if err != nil {
		return ""
	}
	return tag.Name
}

func impliedNames(t *testing.T, svc *tags.Service, id int64) (names []string, stale []bool) {
	t.Helper()
	rows, err := svc.ListImplications(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ParentID == id {
			names, stale = append(names, r.ImpliedName), append(stale, r.Stale)
		}
	}
	return names, stale
}

func TestPTRTagSweep_FollowsTheTagsItCreated(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{
		"cat": {Known: true, Ideal: "feline", Aliases: []string{"kitty"}, Implications: []string{"animal"}},
	}}
	l, svc := sweepEnv(t, stub, nil)
	cat := sweepTag(t, svc, "cat")

	l.PTRTagSweep(svc, []int64{cat}, "")

	if !slices.EqualFunc(stub.calls, [][]string{{"cat"}, {"animal"}}, slices.Equal) {
		t.Errorf("lookups = %v, want cat, then the animal it created", stub.calls)
	}
	for _, alias := range []string{"feline", "kitty"} {
		if got := canonicalOf(t, svc, alias); got != "cat" {
			t.Errorf("%s resolves to %q, want cat", alias, got)
		}
	}
	rows, _ := svc.ListImplications(cat)
	if len(rows) != 1 || rows[0].ImpliedName != "animal" || rows[0].Origin != "ptr" {
		t.Errorf("implications = %+v, want cat -> animal from the PTR", rows)
	}
	want := "PTR: added 2 aliases and 1 implication across 2 tags; 1 unknown to the PTR."
	if st := l.jobs.Get(); st.Summary != want || st.Error != "" {
		t.Errorf("summary = %q error = %q, want %q", st.Summary, st.Error, want)
	}
}

func TestPTRTagSweep_AnOrphanOwnNameResolvesThroughAConnectedAlias(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{
		"great_sword": {Known: true, Ideal: "great_sword"},
		"katanas":     {Known: true, Ideal: "katana", Implications: []string{"sword"}},
	}}
	l, svc := sweepEnv(t, stub, nil)
	greatSword := sweepTag(t, svc, "great_sword")
	general, _, _ := svc.ParseForm("katanas")
	if _, err := svc.CreateAliasFrom("katanas", general, greatSword, "ptr"); err != nil {
		t.Fatal(err)
	}

	l.PTRTagSweep(svc, []int64{greatSword}, "")

	if names, stale := impliedNames(t, svc, greatSword); !slices.Equal(names, []string{"sword"}) || stale[0] {
		t.Errorf("implied = %v stale = %v, want great_sword -> sword from the alias's cluster", names, stale)
	}
	if st := l.jobs.Get(); strings.Contains(st.Summary, "no longer on the PTR") {
		t.Errorf("summary = %q, want the alias that answered kept current", st.Summary)
	}
}

func TestPTRTagSweep_StopsWhenThePTRGoesAwayKeepingWhatApplied(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{
		"cat": {Known: true, Implications: []string{"animal"}},
	}, conflict: 2}
	l, svc := sweepEnv(t, stub, nil)
	cat := sweepTag(t, svc, "cat")

	l.PTRTagSweep(svc, []int64{cat}, "")

	st := l.jobs.Get()
	if !strings.HasPrefix(st.Summary, "PTR lookup stopped at 1/2: the PTR became unavailable") || st.Error != "" {
		t.Errorf("summary = %q error = %q, want a degraded stop", st.Summary, st.Error)
	}
	if names, _ := impliedNames(t, svc, cat); !slices.Equal(names, []string{"animal"}) {
		t.Errorf("implied = %v, want the cat -> animal edge kept", names)
	}
}

func TestPTRTagSweep_LooksUpAsTheGivenSpelling(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{"felis": {Known: true, Ideal: "felis"}}}
	l, svc := sweepEnv(t, stub, nil)
	cat := sweepTag(t, svc, "cat")

	l.PTRTagSweep(svc, []int64{cat}, "felis")

	if !slices.EqualFunc(stub.calls, [][]string{{"felis"}}, slices.Equal) {
		t.Errorf("lookups = %v, want felis only", stub.calls)
	}
	if got := canonicalOf(t, svc, "felis"); got != "cat" {
		t.Errorf("felis resolves to %q, want cat", got)
	}
	if st := l.jobs.Get(); !strings.HasSuffix(st.Summary, ", looked up as felis.") {
		t.Errorf("summary = %q, want it to name the spelling", st.Summary)
	}
}

func TestPTRTagSweep_RetiresWhatThePTRNoLongerCarries(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{
		"cat": {Known: true, Implications: []string{"animal"}},
	}}
	l, svc := sweepEnv(t, stub, nil)
	cat := sweepTag(t, svc, "cat")
	l.PTRTagSweep(svc, []int64{cat}, "")

	stub.graph["cat"] = contract.PTRTagInfo{Known: true}
	if err := l.jobs.Start("ptr-lookup"); err != nil {
		t.Fatal(err)
	}
	l.PTRTagSweep(svc, []int64{cat}, "")

	if names, stale := impliedNames(t, svc, cat); !slices.Equal(names, []string{"animal"}) || !stale[0] {
		t.Errorf("implied = %v stale = %v, want cat -> animal kept as stale", names, stale)
	}
	if st := l.jobs.Get(); !strings.Contains(st.Summary, "; 1 relation no longer on the PTR") {
		t.Errorf("summary = %q, want the retired edge counted", st.Summary)
	}
}

func TestPTRTagSweep_KeepsAnImpliedByEdgeTheChildSpellsThroughAnotherTag(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{
		"gun":     {Known: true, Ideal: "firearm", ImpliedBy: []string{"rifle"}},
		"firearm": {Known: true, Ideal: "firearm", ImpliedBy: []string{"rifle"}},
		"rifle":   {Known: true, Ideal: "rifle", Implications: []string{"firearm"}},
	}}
	l, svc := sweepEnv(t, stub, nil)
	gun := sweepTag(t, svc, "gun")
	sweepTag(t, svc, "firearm")
	rifle := sweepTag(t, svc, "rifle")

	l.PTRTagSweep(svc, []int64{gun, rifle}, "")

	names, stale := impliedNames(t, svc, rifle)
	if !slices.Equal(names, []string{"firearm", "gun"}) || slices.Contains(stale, true) {
		t.Errorf("rifle implies %v stale %v, want firearm and gun, both current", names, stale)
	}
	if st := l.jobs.Get(); strings.Contains(st.Summary, "no longer on the PTR") {
		t.Errorf("summary = %q, want nothing retired", st.Summary)
	}

	stub.graph["gun"] = contract.PTRTagInfo{Known: true, Ideal: "firearm"}
	if err := l.jobs.Start("ptr-lookup"); err != nil {
		t.Fatal(err)
	}
	l.PTRTagSweep(svc, []int64{rifle}, "")
	if names, stale := impliedNames(t, svc, rifle); !slices.Equal(names, []string{"firearm", "gun"}) || !slices.Equal(stale, []bool{false, true}) {
		t.Errorf("rifle implies %v stale %v, want gun retired once neither answer lists it", names, stale)
	}
}

func TestPTRTagSweep_CancelledStopsBeforeTheFollowUpRound(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{
		"cat": {Known: true, Implications: []string{"animal"}},
	}}
	var l *Link
	armed := false
	l, svc := sweepEnv(t, stub, func() {
		if armed {
			l.jobs.Cancel()
		}
	})
	cat := sweepTag(t, svc, "cat")
	armed = true
	l.PTRTagSweep(svc, []int64{cat}, "")

	if len(stub.calls) != 1 {
		t.Errorf("lookups = %v, want the follow-up round skipped", stub.calls)
	}
	if st := l.jobs.Get(); st.Summary != "PTR lookup cancelled (1/2 processed)" || st.Error != "" {
		t.Errorf("summary = %q error = %q", st.Summary, st.Error)
	}
}

func TestApplyPTRTagInfo_KeepsTheOperatorsCatalog(t *testing.T) {
	_, svc := sweepEnv(t, &sweepStub{}, nil)
	cat := sweepTag(t, svc, "cat")
	sweepTag(t, svc, "kitty")
	lion := sweepTag(t, svc, "lion")
	general, _, _ := svc.ParseForm("big_cat")
	if _, err := svc.CreateAlias("big_cat", general, lion); err != nil {
		t.Fatal(err)
	}

	info := contract.PTRTagInfo{Known: true, Aliases: []string{"kitty", "a*b"}, Implications: []string{"big_cat"}}
	aliases, implications, dropped, aliased, _ := applyPTRTagInfo(svc, cat, info, nil, map[int64]struct{}{}, map[int64]struct{}{}, true)

	if aliases != 0 || implications != 0 || dropped != 1 || aliased != 1 {
		t.Errorf("aliases %d implications %d dropped %d aliased %d, want 0 0 1 1", aliases, implications, dropped, aliased)
	}
	if got := canonicalOf(t, svc, "kitty"); got != "kitty" {
		t.Errorf("kitty resolves to %q, want the operator's own tag", got)
	}
	if names, _ := impliedNames(t, svc, cat); len(names) != 0 {
		t.Errorf("implied = %v, want no edge through an alias", names)
	}
}

func TestPTRMergeSweep_AliasesTheOldTagAndSweepsTheTarget(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{"cat": {Known: true, Ideal: "cat"}}}
	l, svc := sweepEnv(t, stub, nil)
	kitty := sweepTag(t, svc, "kitty")

	l.PTRMergeSweep(svc, kitty, "cat")

	if got := canonicalOf(t, svc, "kitty"); got != "cat" {
		t.Errorf("kitty resolves to %q, want cat", got)
	}
	if !slices.EqualFunc(stub.calls, [][]string{{"cat"}, {"kitty"}}, slices.Equal) {
		t.Errorf("lookups = %v, want the target swept, then its orphan answer retried through its alias", stub.calls)
	}
	if st := l.jobs.Get(); st.Error != "" || !strings.HasPrefix(st.Summary, "PTR: ") {
		t.Errorf("summary = %q error = %q", st.Summary, st.Error)
	}
}

func TestPTRTagSweep_ACancelDuringARequestReportsCancelled(t *testing.T) {
	stub := &sweepStub{graph: map[string]contract.PTRTagInfo{"cat": {Known: true}}}
	l, svc := sweepEnv(t, stub, nil)
	stub.hold = func(r *http.Request) {
		l.jobs.Cancel()
		<-r.Context().Done()
	}
	cat := sweepTag(t, svc, "cat")

	l.PTRTagSweep(svc, []int64{cat}, "")

	if st := l.jobs.Get(); st.Summary != "PTR lookup cancelled (0/1 processed)" || st.Error != "" {
		t.Errorf("summary = %q error = %q, want a cancel", st.Summary, st.Error)
	}
}
