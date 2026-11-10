package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func patchCategoryColor(t *testing.T, srv *Server, id int64, color string) *httptest.ResponseRecorder {
	t.Helper()
	return sendForm(t, srv, "PATCH", "/settings/categories/"+strconv.FormatInt(id, 10), url.Values{"color": {color}}, [2]string{"HX-Request", "true"})
}

func categoryIDByName(t *testing.T, srv *Server, name string) int64 {
	t.Helper()
	cats, err := srv.tagSvc().ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("category %q not found", name)
	return 0
}

func galleryResultCount(t *testing.T, srv *Server, q string) string {
	t.Helper()
	body := do(srv, "GET", "/?q="+url.QueryEscape(q)).Body.String()
	m := regexp.MustCompile(`id="result-count" class="result-count">([^<]*)<`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no result count for %q", q)
	}
	return m[1]
}

func TestDeleteCategoryDelete_DropsTheCachedTagCount(t *testing.T) {
	srv := newTestServer(t)
	cat, err := srv.tagSvc().CreateCategory("doomed", "#123456")
	if err != nil {
		t.Fatal(err)
	}
	mustTag(t, srv.tagSvc(), "doomed_tag", cat.ID)
	before, _ := srv.active().TagCount()

	w := deleteCategory(t, srv, cat.ID, "action=delete_all")

	if w.Code >= 400 {
		t.Fatalf("delete answered %d: %s", w.Code, w.Body.String())
	}
	if after, _ := srv.active().TagCount(); after != before-1 {
		t.Errorf("tag count = %d, want %d once the category's tag is gone", after, before-1)
	}
}

func TestRenameCategoryPost_DropsCachedSearches(t *testing.T) {
	srv := newTestServer(t)
	cat, err := srv.tagSvc().CreateCategory("oldcat", "#123456")
	if err != nil {
		t.Fatal(err)
	}
	tag := mustTag(t, srv.tagSvc(), "oc_tag", cat.ID)
	mustAddTag(t, srv.tagSvc(), seedImage(t, srv, "oc.png", 10, 10), tag.ID, false, nil)
	if got := galleryResultCount(t, srv, "cat:oldcat"); got != "1" {
		t.Fatalf("cat:oldcat = %s before the rename, want 1", got)
	}

	postForm(t, srv, "/tags/categories/"+strconv.FormatInt(cat.ID, 10)+"/rename", url.Values{"name": {"newcat"}})

	if got := galleryResultCount(t, srv, "cat:oldcat"); got != "0" {
		t.Errorf("cat:oldcat = %s after the rename, want 0", got)
	}
}

func TestUpdateCategoryPatchAnswersWithTheRow(t *testing.T) {
	srv := newTestServer(t)
	id := categoryIDByName(t, srv, "artist")

	w := patchCategoryColor(t, srv, id, "#123456")
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), `<tr id="cat-row-`) {
		t.Errorf("body should open on the row so htmx parses it in a table context; got %s", body)
	}
	wantIn(t, body, `value="#123456"`)
	wantIn(t, body, "btn-cat-reset")

	back := patchCategoryColor(t, srv, id, "#cc0000")
	wantNotIn(t, back.Body.String(), "btn-cat-reset")
}

func TestUpdateCategoryPatchRefusesAnInvalidColor(t *testing.T) {
	srv := newTestServer(t)
	id := categoryIDByName(t, srv, "artist")

	w := patchCategoryColor(t, srv, id, "red")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `value="#cc0000"`)
	if trigger := w.Header().Get("HX-Trigger"); !strings.Contains(trigger, "invalid category color") {
		t.Errorf("HX-Trigger = %q, want the refusal on the flash channel", trigger)
	}
}

func TestCategoryCountHandler_CountsTheTagsInTheCategory(t *testing.T) {
	srv := newTestServer(t)
	general := categoryIDByName(t, srv, "general")
	character := categoryIDByName(t, srv, "character")
	mustTag(t, srv.tagSvc(), "1girl", general)
	mustTag(t, srv.tagSvc(), "outdoors", general)
	mustTag(t, srv.tagSvc(), "hakurei_reimu", character)

	for _, tc := range []struct {
		id   int64
		want int
	}{
		{general, 2},
		{character, 1},
		{categoryIDByName(t, srv, "artist"), 0},
	} {
		w := do(srv, "GET", "/tags/categories/"+strconv.FormatInt(tc.id, 10)+"/count")
		mustCode(t, w, http.StatusOK)
		var got struct{ Count int }
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("category %d: %v in %s", tc.id, err, w.Body.String())
		}
		if got.Count != tc.want {
			t.Errorf("category %d count = %d, want %d", tc.id, got.Count, tc.want)
		}
	}
}

func TestCategoryCountHandler_UnknownCategoryIs404(t *testing.T) {
	srv := newTestServer(t)
	mustCode(t, do(srv, "GET", "/tags/categories/987654/count"), http.StatusNotFound)
}

func TestCategoryCountHandler_CountsTheAliases(t *testing.T) {
	srv := newTestServer(t)
	cat, err := srv.tagSvc().CreateCategory("aliases_only", "#123456")
	if err != nil {
		t.Fatal(err)
	}
	canon := mustTag(t, srv.tagSvc(), "alias_target", srv.active().GeneralCategoryID)
	mustCreateAlias(t, srv.tagSvc(), "held_alias", cat.ID, canon.ID)
	w := do(srv, "GET", "/tags/categories/"+strconv.FormatInt(cat.ID, 10)+"/count")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"aliases":1`)
}

func deleteCategory(t *testing.T, srv *Server, id int64, query string) *httptest.ResponseRecorder {
	t.Helper()
	return deleteCSRF(t, srv, "/tags/categories/"+strconv.FormatInt(id, 10)+"?"+query)
}

func TestDeleteCategory_RefusesAMalformedTarget(t *testing.T) {
	srv := newTestServer(t)
	cat, err := srv.tagSvc().CreateCategory("malformed_target", "#123456")
	if err != nil {
		t.Fatal(err)
	}
	tag := mustTag(t, srv.tagSvc(), "stays_put", cat.ID)

	mustCode(t, deleteCategory(t, srv, cat.ID, "action=move&target_id=abc"), http.StatusBadRequest)
	got, err := srv.tagSvc().GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CategoryID != cat.ID {
		t.Errorf("tag moved to category %d on a malformed target, want it left in %d", got.CategoryID, cat.ID)
	}
}

func TestDeleteCategory_StatusFollowsTheRefusal(t *testing.T) {
	srv := newTestServer(t)
	general := srv.active().GeneralCategoryID
	cat, err := srv.tagSvc().CreateCategory("clashing", "#123456")
	if err != nil {
		t.Fatal(err)
	}
	mustTag(t, srv.tagSvc(), "clash", cat.ID)
	mustTag(t, srv.tagSvc(), "clash", general)

	for _, c := range []struct {
		id    int64
		query string
		want  int
	}{
		{987654, "action=delete_all", http.StatusNotFound},
		{general, "action=delete_all", http.StatusBadRequest},
		{cat.ID, "action=move&target_id=" + strconv.FormatInt(general, 10), http.StatusConflict},
		{cat.ID, "action=move&target_id=" + strconv.FormatInt(cat.ID, 10), http.StatusBadRequest},
	} {
		if w := deleteCategory(t, srv, c.id, c.query); w.Code != c.want {
			t.Errorf("category %d %s: %d %s, want %d", c.id, c.query, w.Code, strings.TrimSpace(w.Body.String()), c.want)
		}
	}
}

func TestCategoryCreateAndRenameAnswerAStatusPerCause(t *testing.T) {
	srv := newTestServer(t)
	mustCode(t, postForm(t, srv, "/tags/categories", url.Values{"name": {"oc"}}), http.StatusSeeOther)
	id := strconv.FormatInt(categoryIDByName(t, srv, "oc"), 10)

	mustCode(t, postForm(t, srv, "/tags/categories", url.Values{"name": {"a b"}}), http.StatusBadRequest)
	mustCode(t, postForm(t, srv, "/tags/categories", url.Values{"name": {"artist"}}), http.StatusConflict)
	mustCode(t, postForm(t, srv, "/tags/categories/"+id+"/rename", url.Values{"name": {"artist"}}), http.StatusConflict)
	mustCode(t, postForm(t, srv, "/tags/categories/987654/rename", url.Values{"name": {"other"}}), http.StatusNotFound)

	mustExec(t, srv.db().Write, `CREATE TRIGGER no_cat_insert BEFORE INSERT ON tag_categories BEGIN SELECT RAISE(ABORT, 'disk gone'); END`)
	mustExec(t, srv.db().Write, `CREATE TRIGGER no_cat_update BEFORE UPDATE ON tag_categories BEGIN SELECT RAISE(ABORT, 'disk gone'); END`)
	mustCode(t, postForm(t, srv, "/tags/categories", url.Values{"name": {"fresh"}}), http.StatusInternalServerError)
	mustCode(t, postForm(t, srv, "/tags/categories/"+id+"/rename", url.Values{"name": {"other"}}), http.StatusInternalServerError)
}

func categoryOrderForm(ids ...int64) url.Values {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return url.Values{"ids": {strings.Join(parts, ",")}}
}

func TestReorderCategoriesPost_AnswersAStatusPerCause(t *testing.T) {
	srv := newTestServer(t)
	cats, err := srv.tagSvc().ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(cats))
	for i, c := range cats {
		ids[len(ids)-1-i] = c.ID
	}

	mustCode(t, postForm(t, srv, "/tags/categories/order", categoryOrderForm(ids...)), http.StatusNoContent)
	mustCode(t, postForm(t, srv, "/tags/categories/order", url.Values{"ids": {"1,x"}}), http.StatusBadRequest)
	stale := postForm(t, srv, "/tags/categories/order", categoryOrderForm(ids[1:]...))
	mustCode(t, stale, http.StatusConflict)
	wantIn(t, stale.Body.String(), "reload")

	body := do(srv, "GET", "/categories").Body.String()
	if first, last := strings.Index(body, `id="cat-row-`+strconv.FormatInt(ids[0], 10)+`"`),
		strings.Index(body, `id="cat-row-`+strconv.FormatInt(ids[len(ids)-1], 10)+`"`); first < 0 || first > last {
		t.Errorf("the Categories page does not list the saved order")
	}
}

func TestTagSidebarsFollowTheCategoryOrder(t *testing.T) {
	srv := newTestServer(t)
	svc := srv.tagSvc()
	general, artist := categoryIDByName(t, srv, "general"), categoryIDByName(t, srv, "artist")
	img := seedImage(t, srv, "order.png", 10, 10)
	mustAddTag(t, svc, img, mustTag(t, svc, "solo", general).ID, false, nil)
	mustAddTag(t, svc, img, mustTag(t, svc, "someone", artist).ID, false, nil)

	artistFirst := func(page string) bool {
		t.Helper()
		body := do(srv, "GET", page).Body.String()
		a, g := strings.Index(body, `data-cat="artist"`), strings.Index(body, `data-cat="general"`)
		if a < 0 || g < 0 {
			t.Fatalf("%s lists no artist or general group", page)
		}
		return a < g
	}
	detail := "/images/" + strconv.FormatInt(img, 10)
	sidebar := "/internal/sidebar?ids=" + strconv.FormatInt(img, 10)
	if !artistFirst(detail) || !artistFirst(sidebar) {
		t.Fatal("the shipped order lists artist before general")
	}

	order, err := svc.DefaultCategoryOrder()
	if err != nil {
		t.Fatal(err)
	}
	order = append([]int64{general}, slices.DeleteFunc(order, func(id int64) bool { return id == general })...)
	mustCode(t, postForm(t, srv, "/tags/categories/order", categoryOrderForm(order...)), http.StatusNoContent)

	if artistFirst(detail) {
		t.Error("the image page still lists artist before general")
	}
	if artistFirst(sidebar) {
		t.Error("the gallery sidebar still lists artist before general")
	}
}

func TestCategorySelectsPreselectGeneralWhateverTheOrder(t *testing.T) {
	srv := newTestServer(t)
	general := strconv.FormatInt(categoryIDByName(t, srv, "general"), 10)
	body := do(srv, "GET", "/tags").Body.String()
	selected := regexp.MustCompile(`<option value="(\d+)" selected>`).FindAllStringSubmatch(body, -1)
	if len(selected) == 0 {
		t.Fatal("the /tags page preselects no category")
	}
	for _, m := range selected {
		if m[1] != general {
			t.Errorf("a category select preselects %s, want general (%s)", m[1], general)
		}
	}
}

func TestCategoryCleanup_RemovesTheImplicationsDeclaredThereAndSweeps(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	character := categoryIDByName(t, srv, "character")
	rapi := mustTag(t, srv.tagSvc(), "rapi", character)
	android := mustTag(t, srv.tagSvc(), "android", cx.GeneralCategoryID)
	gunner := mustTag(t, srv.tagSvc(), "gunner", cx.GeneralCategoryID)
	marian := mustTag(t, srv.tagSvc(), "marian", character)
	img := seedImage(t, srv, "cleanup.png", 8, 8)
	mustAddImply(t, srv.tagSvc(), rapi.ID, android.ID)
	mustAddImply(t, srv.tagSvc(), gunner.ID, marian.ID)
	mustAddTag(t, srv.tagSvc(), img, rapi.ID, false, nil)

	w := do(srv, "GET", "/tags/categories/"+strconv.FormatInt(character, 10)+"/count")
	wantIn(t, w.Body.String(), `"implications":1`)

	mustStartJob(t, srv.jobs, models.JobTypeTag)
	mustCode(t, postForm(t, srv, "/tags/categories/"+strconv.FormatInt(character, 10)+"/remove-implications", url.Values{}), http.StatusConflict)
	srv.jobs.Complete("")

	w = postForm(t, srv, "/tags/categories/"+strconv.FormatInt(character, 10)+"/remove-implications", url.Values{})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "1 implication removed.")
	wantNotIn(t, w.Header().Get("HX-Trigger"), "tag-relations-changed")
	awaitJobsDrain(t, srv)
	if n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ?", rapi.ID); n != 0 {
		t.Errorf("edges declared on character tags = %d, want 0", n)
	}
	if n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ?", gunner.ID); n != 1 {
		t.Errorf("an edge declared on a general tag was removed")
	}
	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", img, android.ID); n != 0 {
		t.Errorf("the implied row outlived its edge")
	}
}

func TestCategoryCleanup_NothingToRemoveKeepsThePreviousJobState(t *testing.T) {
	srv := newTestServer(t)
	character := categoryIDByName(t, srv, "character")
	mustStartJob(t, srv.jobs, models.JobTypeDelete)
	srv.jobs.Fail("boom")
	finished := srv.jobs.Finished()

	w := postForm(t, srv, "/tags/categories/"+strconv.FormatInt(character, 10)+"/remove-implications", url.Values{})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Trigger"), "0 implications removed.")

	if st := srv.jobs.Get(); st == nil || st.Error != "boom" {
		t.Errorf("job = %+v, want the earlier failure still shown", st)
	}
	if got := srv.jobs.Finished(); got != finished {
		t.Errorf("Finished() = %d, want %d: a request with nothing to remove ended a job", got, finished)
	}
}
