package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/tags"
)

func deleteWithCSRF(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	return deleteCSRF(t, srv, target, [2]string{"HX-Request", "true"})
}

func deleteCSRF(t *testing.T, srv *Server, target string, extra ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", target, nil)
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestRemoveCategoryTagsFromImage_TakesOnlyThatCategory(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "cat_strip.png", 10, 10)
	cats, err := tags.CategoryIDsByName(srv.db().Read)
	if err != nil {
		t.Fatal(err)
	}
	character := mustTag(t, srv.tagSvc(), "hakurei_reimu", cats["character"])
	artist := mustTag(t, srv.tagSvc(), "zun", cats["artist"])
	general := mustTag(t, srv.tagSvc(), "1girl", srv.active().GeneralCategoryID)
	for _, tag := range []int64{character.ID, artist.ID, general.ID} {
		mustAddTag(t, srv.tagSvc(), id, tag, false, nil)
	}

	w := deleteWithCSRF(t, srv, fmt.Sprintf("/images/%d/category-tags?cat=character", id))
	mustCode(t, w, http.StatusOK)

	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, character.ID); n != 0 {
		t.Error("the character tag stayed on the image")
	}
	for _, tag := range []int64{artist.ID, general.ID} {
		if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, tag); n != 1 {
			t.Errorf("tag %d left with another category's rows", tag)
		}
	}
}

func TestRemoveTagsDialog_OffersUserTagsOnlyForOwnedRows(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "implied_only.png", 10, 10)
	gen := srv.active().GeneralCategoryID
	parent := mustTag(t, srv.tagSvc(), "sourced_parent", gen)
	child := mustTag(t, srv.tagSvc(), "implied_child", gen)
	mustAddImply(t, srv.tagSvc(), parent.ID, child.ID)

	if err := srv.tagSvc().AddTagsToImageFromTagger(id, []int64{parent.ID}, false, "danbooru"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", id, child.ID); n != 1 {
		t.Fatal("test premise broken: the child is not implied")
	}
	page := fmt.Sprintf("/images/%d", id)

	if strings.Contains(do(srv, "GET", page).Body.String(), "All tags you added") {
		t.Error("All tags you added is offered with no tag of the user's on the image")
	}
	mustAddTag(t, srv.tagSvc(), id, mustTag(t, srv.tagSvc(), "typed_by_hand", gen).ID, false, nil)
	wantIn(t, do(srv, "GET", page).Body.String(), "All tags you added")
}

func TestDropSourceContribution_KeepsACoVouchedTag(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "source_strip.png", 10, 10)
	solo := mustTag(t, srv.tagSvc(), "solo_claim", srv.active().GeneralCategoryID)
	shared := mustTag(t, srv.tagSvc(), "shared_claim", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), id, solo.ID, false, nil)
	mustAddTag(t, srv.tagSvc(), id, shared.ID, false, nil)
	for _, row := range [][2]any{{solo.ID, "danbooru"}, {shared.ID, "danbooru"}, {shared.ID, "gelbooru"}} {
		mustExec(t, srv.db().Write,
			`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source) VALUES (?, ?, ?)`,
			id, row[0], row[1])
	}

	mustExec(t, srv.db().Write,
		`DELETE FROM image_tag_sources WHERE image_id = ? AND source = 'user'`, id)

	w := deleteWithCSRF(t, srv, fmt.Sprintf("/images/%d/source-contribution?source=danbooru", id))
	mustCode(t, w, http.StatusOK)

	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, solo.ID); n != 0 {
		t.Error("the tag whose only source withdrew stayed on the image")
	}
	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, shared.ID); n != 1 {
		t.Error("a tag another source still vouches for left the image")
	}
	if n := countRows(t, srv.db(), "image_tag_sources WHERE image_id = ? AND source = 'danbooru'", id); n != 0 {
		t.Error("the withdrawing source kept its ledger rows")
	}
	if n := countRows(t, srv.db(), "image_tag_sources WHERE image_id = ? AND source = 'gelbooru'", id); n != 1 {
		t.Error("the withdrawal took another source's ledger row")
	}
}
