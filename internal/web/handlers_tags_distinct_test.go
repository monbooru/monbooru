package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/galleryio"
)

func TestBatchDistinct_MarksThePairAndTheFiltersFollow(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	artist := mustTag(t, srv.tagSvc(), "kirin", galleryio.LookupCategoryID(cx.DB, "artist"))
	character := mustTag(t, srv.tagSvc(), "kirin", galleryio.LookupCategoryID(cx.DB, "character"))
	fox := mustTag(t, srv.tagSvc(), "fox", cx.GeneralCategoryID)
	mustTag(t, srv.tagSvc(), "fox", galleryio.LookupCategoryID(cx.DB, "species"))

	w := postHTMXForm(t, srv, "/tags/batch-distinct", url.Values{"ids": {fmt.Sprintf("%d,%d,%d", artist.ID, character.ID, fox.ID)}})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Marked 1 pair distinct. Skipped 1 tag with no same-name tag selected.")
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}

	conflicts := do(srv, "GET", "/tags?conflicts=1").Body.String()
	wantNotIn(t, conflicts, `>kirin</a>`)
	if got := strings.Count(conflicts, `>fox</a>`); got != 2 {
		t.Errorf("conflicts view lists %d fox rows, want 2", got)
	}
	wantIn(t, conflicts, `id="btn-batch-distinct"`)

	distinct := do(srv, "GET", "/tags?conflicts=distinct").Body.String()
	if got := strings.Count(distinct, `>kirin</a>`); got != 2 {
		t.Errorf("distinct view lists %d kirin rows, want 2", got)
	}
	wantIn(t, distinct, `title="Marked distinct from character:kirin">distinct</span>`)
	wantIn(t, distinct, fmt.Sprintf(`data-distinct="%d"`, character.ID))
	wantIn(t, distinct, `id="btn-batch-undistinct"`)

	plain := do(srv, "GET", "/tags?q=kirin").Body.String()
	wantNotIn(t, plain, `>distinct</span>`)
	wantNotIn(t, plain, `id="btn-batch-distinct"`)

	w = postHTMXForm(t, srv, "/tags/batch-distinct", url.Values{"ids": {fmt.Sprint(artist.ID)}, "mode": {"remove"}})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Unmarked 1 distinct pair.")
	wantIn(t, do(srv, "GET", "/tags?conflicts=1").Body.String(), `>kirin</a>`)
}

func TestBatchDistinct_RefusesASelectionWithNothingToMark(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := mustTag(t, srv.tagSvc(), "saber", cx.GeneralCategoryID)
	c := mustTag(t, srv.tagSvc(), "saber", galleryio.LookupCategoryID(cx.DB, "character"))

	w := postHTMXForm(t, srv, "/tags/batch-distinct", url.Values{"ids": {fmt.Sprint(a.ID)}})
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "Select two or more tags that share a name.")

	ids := url.Values{"ids": {fmt.Sprintf("%d,%d", a.ID, c.ID)}}
	mustCode(t, postHTMXForm(t, srv, "/tags/batch-distinct", ids), http.StatusNoContent)
	w = postHTMXForm(t, srv, "/tags/batch-distinct", ids)
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "already marked distinct")

	w = postHTMXForm(t, srv, "/tags/batch-distinct", url.Values{"ids": {"999999"}, "mode": {"remove"}})
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "None of the selected tags is marked distinct.")
}

func TestTagDetail_SameNameBlockMarksAndUnmarks(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := mustTag(t, srv.tagSvc(), "yoshida", galleryio.LookupCategoryID(cx.DB, "artist"))
	c := mustTag(t, srv.tagSvc(), "yoshida", galleryio.LookupCategoryID(cx.DB, "character"))
	g := mustTag(t, srv.tagSvc(), "yoshida", cx.GeneralCategoryID)
	lone := mustTag(t, srv.tagSvc(), "lonely", cx.GeneralCategoryID)
	if _, _, err := srv.tagSvc().MarkDistinct([]int64{a.ID, c.ID}); err != nil {
		t.Fatal(err)
	}

	body := do(srv, "GET", fmt.Sprintf("/tags/%d", a.ID)).Body.String()
	wantIn(t, body, `id="tag-same-name"`)
	wantIn(t, body, fmt.Sprintf(`hx-delete="/tags/%d/distinct/%d"`, a.ID, c.ID))
	wantIn(t, body, fmt.Sprintf(`hx-vals='{"ids": "%d,%d"}'`, a.ID, g.ID))
	wantIn(t, body, `openDetailAlias('general:yoshida')`)
	wantNotIn(t, do(srv, "GET", fmt.Sprintf("/tags/%d", lone.ID)).Body.String(), `id="tag-same-name"`)

	w := do(srv, "DELETE", fmt.Sprintf("/tags/%d/distinct/%d", a.ID, c.ID), [2]string{"HX-Request", "true"}, [2]string{"X-CSRF-Token", srv.sessions.CSRFToken("anon")})
	mustCode(t, w, http.StatusNoContent)
	if n, err := srv.tagSvc().DistinctCount(); err != nil || n != 0 {
		t.Errorf("DistinctCount = %d, %v after the unmark; want 0", n, err)
	}
}

func TestTagCategoryConflicts_CountsTheMarkedTagsApart(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := mustTag(t, srv.tagSvc(), "ruby", cx.GeneralCategoryID)
	c := mustTag(t, srv.tagSvc(), "ruby", galleryio.LookupCategoryID(cx.DB, "character"))
	if _, _, err := srv.tagSvc().MarkDistinct([]int64{a.ID, c.ID}); err != nil {
		t.Fatal(err)
	}

	body := postForm(t, srv, "/settings/maintenance/tag-conflicts", url.Values{}).Body.String()
	wantIn(t, body, "No tags share a name across categories.")
	wantIn(t, body, `<strong>2</strong> tags are <a href="/tags?conflicts=distinct">marked distinct</a> and not counted.`)

	mustTag(t, srv.tagSvc(), "mercy", cx.GeneralCategoryID)
	mustTag(t, srv.tagSvc(), "mercy", galleryio.LookupCategoryID(cx.DB, "character"))
	body = postForm(t, srv, "/settings/maintenance/tag-conflicts", url.Values{}).Body.String()
	wantIn(t, body, "<strong>2</strong> tags share a name")
	wantIn(t, body, `<strong>2</strong> more are <a href="/tags?conflicts=distinct">marked distinct</a>`)
}
