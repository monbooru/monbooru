package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/tags"
)

func twoGalleries(t *testing.T) (*Server, *library.Gallery, *library.Gallery) {
	t.Helper()
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "second", filepath.Join(t.TempDir(), "second"))
	return srv, srv.get(srv.activeGallery()), srv.get("second")
}

func settleJobs(t *testing.T, srv *Server) {
	t.Helper()
	for range 3 {
		time.Sleep(20 * time.Millisecond)
		if !srv.jobs.WaitIdle(10 * time.Second) {
			t.Fatal("a job is still running")
		}
	}
}

func tagNamed(t *testing.T, g *library.Gallery, name string) int64 {
	t.Helper()
	tag, err := g.TagSvc.GetOrCreateTag(name, g.GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	return tag.ID
}

func aliasNames(t *testing.T, g *library.Gallery, of string) []string {
	t.Helper()
	id := tagNamed(t, g, of)
	byCanonical, err := g.TagSvc.AliasesForTagIDs([]int64{id})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range byCanonical[id] {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	return names
}

func isGlobal(t *testing.T, g *library.Gallery, name string) bool {
	t.Helper()
	tag, err := g.TagSvc.GetTag(tagNamed(t, g, name))
	if err != nil {
		t.Fatal(err)
	}
	return tag.Global
}

func TestMakeGlobalThenAnEditReachesTheOtherGallery(t *testing.T) {
	srv, first, second := twoGalleries(t)
	id := tagNamed(t, first, "animation")
	if _, err := first.TagSvc.CreateAlias("anime", first.GeneralCategoryID, id); err != nil {
		t.Fatal(err)
	}

	w := postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10)}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("make global = %d %s", w.Code, w.Body)
	}
	settleJobs(t, srv)
	if !isGlobal(t, first, "animation") || !isGlobal(t, second, "animation") {
		t.Fatal("the tag is not global in both galleries")
	}
	if got := aliasNames(t, second, "animation"); !slices.Equal(got, []string{"anime"}) {
		t.Fatalf("second gallery aliases = %v", got)
	}

	w = postForm(t, srv, "/tags/"+strconv.FormatInt(id, 10)+"/aliases", url.Values{"name": {"animated"}})
	if w.Code >= 400 {
		t.Fatalf("add alias = %d %s", w.Code, w.Body)
	}
	settleJobs(t, srv)
	if got := aliasNames(t, second, "animation"); !slices.Equal(got, []string{"animated", "anime"}) {
		t.Errorf("after an alias added in the first gallery, the second has %v", got)
	}

	w = sendForm(t, srv, "DELETE", "/tags/"+strconv.FormatInt(id, 10), url.Values{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("deleting a global tag = %d, want 400", w.Code)
	}
	if err := first.TagSvc.RejectTag(id); err != tags.ErrTagGlobal {
		t.Errorf("rejecting a global tag = %v, want ErrTagGlobal", err)
	}

	w = postForm(t, srv, "/tags/"+strconv.FormatInt(id, 10)+"/local", url.Values{}, [2]string{"HX-Request", "true"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("make local = %d %s", w.Code, w.Body)
	}
	settleJobs(t, srv)
	if isGlobal(t, first, "animation") || isGlobal(t, second, "animation") {
		t.Error("the tag is still global after make local")
	}
	if got := aliasNames(t, second, "animation"); len(got) != 2 {
		t.Errorf("make local took aliases away: %v", got)
	}
}

func TestAnAddedGalleryStartsWithTheGlobalTags(t *testing.T) {
	srv, first, _ := twoGalleries(t)
	id := tagNamed(t, first, "animation")
	export := mustExport(t, srv, first.Name, "json")
	postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10)}})
	settleJobs(t, srv)

	third := filepath.Join(t.TempDir(), "third")
	mustMkdirAll(t, third, 0o755)
	wantIn(t, postAddGallery(t, srv, "third", third), "Gallery third added and now active.")
	if got := srv.activeGallery(); got != "third" {
		t.Errorf("active gallery = %s, want third", got)
	}
	settleJobs(t, srv)
	if !isGlobal(t, srv.get("third"), "animation") {
		t.Error("the added gallery did not take the global tag")
	}

	fourth := filepath.Join(t.TempDir(), "fourth")
	mustMkdirAll(t, fourth, 0o755)
	body, ct := buildMultipart(t, map[string]string{"name": "fourth", "gallery_path": fourth}, "import_file", "export.json", export.Bytes())
	wantIn(t, postMultipart(t, srv, "/settings/galleries", body, ct).Body.String(), "Gallery fourth added and imported.")
	settleJobs(t, srv)
	if !isGlobal(t, srv.get("fourth"), "animation") {
		t.Error("the gallery added with an import did not take the global tag")
	}
}

func TestAStartPullsIntoAGalleryWithNoGlobalTag(t *testing.T) {
	srv, first, _ := twoGalleries(t)
	id := tagNamed(t, first, "animation")
	postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10)}})
	settleJobs(t, srv)
	third := filepath.Join(t.TempDir(), "third")
	mustMkdirAll(t, third, 0o755)
	if err := srv.set.Add("third", third); err != nil {
		t.Fatal(err)
	}

	srv.StartWatchers()
	settleJobs(t, srv)
	if !isGlobal(t, srv.get("third"), "animation") {
		t.Error("the start did not pull the global tags into the gallery that had none")
	}
}

func TestAFailedPullWaitsForTheNextChange(t *testing.T) {
	srv, first, _ := twoGalleries(t)
	id := tagNamed(t, first, "animation")
	postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10) + "," + strconv.FormatInt(tagNamed(t, first, "still"), 10)}})
	settleJobs(t, srv)
	if !isGlobal(t, first, "still") {
		t.Fatal("still is not global")
	}
	thirdPath := filepath.Join(t.TempDir(), "third")
	mustMkdirAll(t, thirdPath, 0o755)
	if err := srv.set.Add("third", thirdPath); err != nil {
		t.Fatal(err)
	}
	third := srv.get("third")
	mustExec(t, third.DB.Write, `ALTER TABLE global_tag_applying RENAME TO applying_away`)
	srv.queueGlobalPull("third")
	settleJobs(t, srv)
	if st := srv.jobs.Get(); st == nil || st.Error == "" {
		t.Fatalf("the pull did not fail: %+v", st)
	}

	base := srv.jobs.Finished()
	for range 2 {
		postForm(t, srv, "/internal/job/dismiss", url.Values{})
		settleJobs(t, srv)
	}
	if n := srv.jobs.Finished() - base; n != 0 {
		t.Errorf("%d jobs ran on writes that changed no global tag", n)
	}

	mustExec(t, third.DB.Write, `ALTER TABLE applying_away RENAME TO global_tag_applying`)
	if _, err := first.TagSvc.CreateAlias("anime", first.GeneralCategoryID, id); err != nil {
		t.Fatal(err)
	}
	postForm(t, srv, "/internal/job/dismiss", url.Values{})
	settleJobs(t, srv)
	if !isGlobal(t, third, "still") {
		t.Error("the next change did not retry the pull")
	}
}

func TestMakeGlobalNeedsASecondGallery(t *testing.T) {
	srv := newTestServer(t)
	id := tagNamed(t, srv.active(), "animation")
	w := postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10)}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("make global with one gallery = %d, want 400", w.Code)
	}
}

func TestTheCheckSaysWhatEachGalleryGetsAndTheListingShowsTheMark(t *testing.T) {
	srv, first, second := twoGalleries(t)
	id := tagNamed(t, first, "animation")
	if _, err := first.TagSvc.CreateAlias("anime", first.GeneralCategoryID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := first.TagSvc.AddImplication(id, tagNamed(t, first, "video")); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"ids": {strconv.FormatInt(id, 10)}}

	body := postForm(t, srv, "/tags/global/check", form).Body.String()
	for _, want := range []string{"<strong>second</strong> gets 2 tags, 1 alias and 1 implication.", "Comes along, being implied: video."} {
		if !strings.Contains(body, want) {
			t.Errorf("check lacks %q in:\n%s", want, body)
		}
	}
	if isGlobal(t, first, "animation") || len(aliasNames(t, second, "animation")) != 0 {
		t.Fatal("the check wrote something")
	}

	page := mustGetBody(t, srv, "/tags")
	if !strings.Contains(page, `id="btn-batch-global"`) || strings.Contains(page, `>global</span>`) {
		t.Error("before: the page lacks the button or already shows a global tag")
	}
	postForm(t, srv, "/tags/global", form)
	settleJobs(t, srv)
	page = mustGetBody(t, srv, "/tags?scope=global")
	if n := strings.Count(page, `>global</span>`); n != 2 {
		t.Errorf("scope=global shows %d global rows, want 2", n)
	}
	if page := mustGetBody(t, srv, "/tags?scope=local"); strings.Contains(page, `>global</span>`) {
		t.Error("scope=local shows a global tag")
	}
	detail := mustGetBody(t, srv, "/tags/"+strconv.FormatInt(id, 10))
	for _, want := range []string{`id="btn-detail-local"`, "global, shared with second"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the tag page lacks %q", want)
		}
	}
}

func TestTheTagsPagerKeepsTheScope(t *testing.T) {
	srv, first, _ := twoGalleries(t)
	for i := range tagListingPageSize + 1 {
		tagNamed(t, first, "tag"+strconv.Itoa(i))
	}
	next := regexp.MustCompile(`href="([^"]*page=2)"`).FindStringSubmatch(mustGetBody(t, srv, "/tags?scope=local"))
	if next == nil {
		t.Fatal("no link to page 2")
	}
	if !strings.Contains(next[1], "scope=local&amp;") {
		t.Errorf("the pager drops the scope: %s", next[1])
	}
}

func TestASingleGalleryShowsNoGlobalControls(t *testing.T) {
	srv := newTestServer(t)
	id := tagNamed(t, srv.active(), "animation")
	for _, path := range []string{"/tags", "/tags/" + strconv.FormatInt(id, 10)} {
		page := mustGetBody(t, srv, path)
		for _, absent := range []string{`id="btn-batch-global"`, `id="btn-detail-global"`, `id="make-global-dialog"`, `>Scope<`} {
			if strings.Contains(page, absent) {
				t.Errorf("%s shows %q with one gallery", path, absent)
			}
		}
	}
}

func TestARefusedTagIsNamedWithoutStartingAJobByItself(t *testing.T) {
	srv, first, second := twoGalleries(t)
	id := tagNamed(t, first, "kitty")
	postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10)}})
	settleJobs(t, srv)
	tagNamed(t, second, "cat")
	if err := first.TagSvc.RenameTag(id, "cat"); err != nil {
		t.Fatal(err)
	}
	postForm(t, srv, "/internal/job/dismiss", url.Values{})
	settleJobs(t, srv)
	if st := srv.jobs.Get(); st == nil || !strings.Contains(st.Summary, "1 left out: cat in second") {
		t.Fatalf("job = %+v, want the rename named as left out", st)
	}

	base := srv.jobs.Finished()
	postForm(t, srv, "/internal/job/dismiss", url.Values{})
	settleJobs(t, srv)
	if n := srv.jobs.Finished() - base; n != 0 {
		t.Errorf("%d jobs ran with only the refused record waiting", n)
	}
}
