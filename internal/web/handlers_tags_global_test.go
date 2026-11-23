package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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
	postForm(t, srv, "/tags/global", url.Values{"ids": {strconv.FormatInt(id, 10)}})
	settleJobs(t, srv)

	addRemovableGallery(t, srv, "third", filepath.Join(t.TempDir(), "third"))
	settleJobs(t, srv)
	if !isGlobal(t, srv.get("third"), "animation") {
		t.Error("the added gallery did not take the global tag")
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

	page := getPage(t, srv, "/tags")
	if !strings.Contains(page, `id="btn-batch-global"`) || strings.Contains(page, `>global</span>`) {
		t.Error("before: the page lacks the button or already shows a global tag")
	}
	postForm(t, srv, "/tags/global", form)
	settleJobs(t, srv)
	page = getPage(t, srv, "/tags?scope=global")
	if n := strings.Count(page, `>global</span>`); n != 2 {
		t.Errorf("scope=global shows %d global rows, want 2", n)
	}
	if page := getPage(t, srv, "/tags?scope=local"); strings.Contains(page, `>global</span>`) {
		t.Error("scope=local shows a global tag")
	}
	detail := getPage(t, srv, "/tags/"+strconv.FormatInt(id, 10))
	for _, want := range []string{`id="btn-detail-local"`, "global, shared with second"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the tag page lacks %q", want)
		}
	}
}

func TestASingleGalleryShowsNoGlobalControls(t *testing.T) {
	srv := newTestServer(t)
	id := tagNamed(t, srv.active(), "animation")
	for _, path := range []string{"/tags", "/tags/" + strconv.FormatInt(id, 10)} {
		page := getPage(t, srv, path)
		for _, absent := range []string{`id="btn-batch-global"`, `id="btn-detail-global"`, `id="make-global-dialog"`, `>Scope<`} {
			if strings.Contains(page, absent) {
				t.Errorf("%s shows %q with one gallery", path, absent)
			}
		}
	}
}

func getPage(t *testing.T, srv *Server, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, w.Code)
	}
	return w.Body.String()
}
