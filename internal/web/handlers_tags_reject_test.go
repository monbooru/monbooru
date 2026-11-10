package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/tags"
)

func TestRejectTagsPost_RejectsNamesAndTheListingShowsThem(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := cx.GeneralCategoryID
	mustTag(t, cx.TagSvc, "watermark", general)

	w := postHTMXForm(t, srv, "/tags/reject", url.Values{"name": {"watermark lowres"}, "category_id": {fmt.Sprint(general)}})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Rejected 2 names: watermark, lowres.")
	if w.Header().Get("HX-Refresh") != "true" {
		t.Error("expected an HX-Refresh")
	}

	body := do(srv, "GET", "/tags?type=rejected").Body.String()
	wantIn(t, body, "rejected-badge")
	wantIn(t, body, ">watermark</a>")
	wantIn(t, body, ">lowres</a>")
	wantIn(t, body, "Un-reject")
	wantIn(t, body, `Rejected <span class="count-badge">2</span>`)
	wantNotIn(t, do(srv, "GET", "/tags").Body.String(), ">watermark</a>")

	w = postHTMXForm(t, srv, "/tags/new", url.Values{"name": {"watermark"}, "category_id": {fmt.Sprint(general)}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-err")
	wantIn(t, w.Body.String(), tags.ErrTagRejected.Error())
}

func TestRejectTagsPost_RefusesWhatCannotBeRejected(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := cx.GeneralCategoryID
	target := mustTag(t, cx.TagSvc, "target", general)
	if _, err := cx.TagSvc.CreateAlias("spelling", general, target.ID); err != nil {
		t.Fatal(err)
	}
	w := postHTMXForm(t, srv, "/tags/reject", url.Values{"name": {"spelling"}, "category_id": {fmt.Sprint(general)}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-err")
	wantIn(t, w.Body.String(), "spelling: an alias follows its target")
}

func TestRejectAndUnrejectTagPost(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := cx.GeneralCategoryID
	tag := mustTag(t, cx.TagSvc, "watermark", general)

	w := postForm(t, srv, fmt.Sprintf("/tags/%d/reject", tag.ID), url.Values{})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "Rejected watermark")
	if got, _ := cx.TagSvc.GetTag(tag.ID); !got.Rejected {
		t.Fatal("tag not rejected")
	}
	page := do(srv, "GET", fmt.Sprintf("/tags/%d", tag.ID)).Body.String()
	wantIn(t, page, "rejected-badge")
	wantIn(t, page, `id="btn-detail-unreject"`)
	wantNotIn(t, page, `id="btn-detail-rename"`)

	w = postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/unreject", tag.ID), url.Values{})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "watermark is a tag again.")
	if got, _ := cx.TagSvc.GetTag(tag.ID); got.Rejected {
		t.Fatal("tag still rejected")
	}

	w = postForm(t, srv, "/tags/99999/reject", url.Values{})
	mustCode(t, w, http.StatusBadRequest)
}

func TestDeleteTagsSearchPost_WithRejectKeepsTheNamesOut(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := cx.GeneralCategoryID
	a := mustTag(t, cx.TagSvc, "aaa", general)
	b := mustTag(t, cx.TagSvc, "bbb", general)

	w := postForm(t, srv, "/tags/delete-search", url.Values{"ids": {fmt.Sprintf("%d,%d", a.ID, b.ID)}, "reject": {"1"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	for _, id := range []int64{a.ID, b.ID} {
		got, err := cx.TagSvc.GetTag(id)
		if err != nil || !got.Rejected {
			t.Errorf("tag %d = %+v, %v, want kept and rejected", id, got, err)
		}
	}

	w = postForm(t, srv, "/tags/reject-search", url.Values{"q": {"a"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
}

func TestAddTagToImage_ARejectedNameIsRefusedWithAnUnrejectLink(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	rejected, err := cx.TagSvc.RejectTagName("watermark", cx.GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	id := seedImage(t, srv, "rejected_box.png", 10, 10)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/tags", id), url.Values{"tag": {"watermark kept"}})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "watermark is rejected in this gallery")
	wantIn(t, body, fmt.Sprintf(`hx-post="/tags/%d/unreject"`, rejected.ID))
	wantIn(t, body, "added: kept")
	wantNotIn(t, body, "rejected: watermark")
	if got, _ := cx.TagSvc.GetTag(rejected.ID); got.RejectedDrops != 0 {
		t.Errorf("the add box counted a drop: %d", got.RejectedDrops)
	}
}

func TestDeleteAndRejectRefusalsAnswerAFetchInPlainText(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	general := cx.GeneralCategoryID
	target := mustTag(t, cx.TagSvc, "target_tag", general)
	alias, err := cx.TagSvc.CreateAlias("alias_tag", general, target.ID)
	if err != nil {
		t.Fatal(err)
	}

	gone := deleteCSRF(t, srv, "/tags/999999")
	mustCode(t, gone, http.StatusBadRequest)
	if got := gone.Body.String(); got != "Tag not found.\n" {
		t.Errorf("a fetch's refused delete = %q, want the plain message", got)
	}
	hx := deleteCSRF(t, srv, "/tags/999999", [2]string{"HX-Request", "true"})
	wantIn(t, hx.Header().Get("HX-Trigger"), "Tag not found.")

	refused := postForm(t, srv, fmt.Sprintf("/tags/%d/reject", alias.ID), url.Values{})
	mustCode(t, refused, http.StatusBadRequest)
	if body := refused.Body.String(); strings.Contains(body, "<div") || !strings.Contains(body, "alias") {
		t.Errorf("a fetch's refused reject = %q, want the plain alias message", body)
	}
}
