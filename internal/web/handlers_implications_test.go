package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func drainJob(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st := srv.jobs.Get()
		if !st.Running && st.FinishedAt != nil {
			return
		}
		if !st.Running && st.JobType == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish within 60s")
}

func TestAddImplicationPost_DeclaresAndDispatches(t *testing.T) {
	srv := newTestServer(t)

	idA := seedImage(t, srv, "a.png", 11, 11)
	idB := seedImage(t, srv, "b.png", 12, 12)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), idA, parent.ID, false, nil)
	mustAddTag(t, srv.tagSvc(), idB, parent.ID, false, nil)

	form := url.Values{
		"implied_id": {"japanese"},
	}
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications", form)
	mustCode(t, w, http.StatusNoContent)
	drainJob(t, srv)

	var impliedTagID int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, "japanese").Scan(&impliedTagID); err != nil {
		t.Fatalf("look up implied tag: %v", err)
	}
	n := countRows(t, srv.db(), "image_tags WHERE tag_id = ? AND is_implied = 1", impliedTagID)
	if n != 2 {
		t.Errorf("implied rows = %d, want 2", n)
	}
}

func TestImplicationRoutes_RefuseWhileAJobHoldsTheLane(t *testing.T) {
	srv := newTestServer(t)
	idA := seedImage(t, srv, "a.png", 11, 11)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	implied := mustTag(t, srv.tagSvc(), "japanese", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), idA, parent.ID, false, nil)
	mustAddImply(t, srv.tagSvc(), parent.ID, implied.ID)
	edgeCount := countRows(t, srv.db(), "tag_implications")

	mustStartJob(t, srv.jobs, models.JobTypeDelete)
	t.Cleanup(func() { srv.jobs.Complete("done") })

	parentPath := "/tags/" + strconv.FormatInt(parent.ID, 10)
	for _, tc := range []struct {
		what   string
		method string
		path   string
		form   url.Values
	}{
		{"declare", "POST", parentPath + "/implications", url.Values{"implied_id": {"outdoors"}}},
		{"declare the inverse", "POST", parentPath + "/implied-by", url.Values{"parent_id": {"landscape"}}},
		{"remove", "DELETE", parentPath + "/implications/" + strconv.FormatInt(implied.ID, 10), url.Values{}},
		{"remove a group", "DELETE", parentPath + "/implications/group", url.Values{"origin": {""}}},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Errorf("%s while a job runs = %d, want 409: %s", tc.what, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "A job is already running.") {
			t.Errorf("%s refusal body = %q", tc.what, w.Body.String())
		}
	}
	if got := countRows(t, srv.db(), "tag_implications"); got != edgeCount {
		t.Errorf("tag_implications = %d, want %d: a refused route wrote an edge anyway", got, edgeCount)
	}
}

func TestAddImplicationPost_FansOutEveryTokenItDeclared(t *testing.T) {
	srv := newTestServer(t)
	idA := seedImage(t, srv, "a.png", 11, 11)
	idB := seedImage(t, srv, "b.png", 12, 12)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), idA, parent.ID, false, nil)
	mustAddTag(t, srv.tagSvc(), idB, parent.ID, false, nil)

	w := postForm(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications",
		url.Values{"implied_id": {"japanese animation drawing"}})
	mustCode(t, w, http.StatusNoContent)
	drainJob(t, srv)

	for _, name := range []string{"japanese", "animation", "drawing"} {
		var tagID int64
		if err := srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&tagID); err != nil {
			t.Fatalf("look up %q: %v", name, err)
		}
		if n := countRows(t, srv.db(), "image_tags WHERE tag_id = ? AND is_implied = 1", tagID); n != 2 {
			t.Errorf("%q landed on %d image(s), want 2", name, n)
		}
	}
}

func TestAddImplicationPost_ABareNameTakesTheOneTagHoldingIt(t *testing.T) {
	srv := newTestServer(t)
	copyrightID, ok, err := tags.CategoryIDByName(srv.db(), "copyright")
	if err != nil || !ok {
		t.Fatalf("copyright category: %v %v", ok, err)
	}
	parent := mustTag(t, srv.tagSvc(), "saber", srv.active().GeneralCategoryID)
	fate := mustTag(t, srv.tagSvc(), "fate", copyrightID)

	mustCode(t, postForm(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications",
		url.Values{"implied_id": {"fate"}}), http.StatusNoContent)
	drainJob(t, srv)

	if n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?", parent.ID, fate.ID); n != 1 {
		t.Errorf("edge to copyright:fate = %d, want 1", n)
	}
	if n := countRows(t, srv.db(), "tags WHERE name = 'fate'"); n != 1 {
		t.Errorf("tags named fate = %d, want only the copyright one", n)
	}

	mustTag(t, srv.tagSvc(), "fate", srv.active().GeneralCategoryID)
	other := mustTag(t, srv.tagSvc(), "archer", srv.active().GeneralCategoryID)
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(other.ID, 10)+"/implications", url.Values{"implied_id": {"fate"}})
	wantIn(t, w.Body.String(), "exists in more than one category")
	drainJob(t, srv)
	if n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ?", other.ID); n != 0 {
		t.Errorf("an ambiguous name declared %d edge(s), want 0", n)
	}
}

func TestAddImplicationPost_SummaryCountsEachImageOnce(t *testing.T) {
	srv := newTestServer(t)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), seedImage(t, srv, "a.png", 11, 11), parent.ID, false, nil)
	mustAddTag(t, srv.tagSvc(), seedImage(t, srv, "b.png", 12, 12), parent.ID, false, nil)

	mustCode(t, postForm(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications",
		url.Values{"implied_id": {"japanese animation drawing"}}), http.StatusNoContent)
	drainJob(t, srv)

	if got := srv.jobs.Get().Summary; got != "Implication applied to 2 images." {
		t.Errorf("summary = %q, want each image counted once", got)
	}
}

func TestAddImpliedByPost_DeclaresInverseEdge(t *testing.T) {
	srv := newTestServer(t)
	idA := seedImage(t, srv, "a.png", 11, 11)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	implied := mustTag(t, srv.tagSvc(), "japanese", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), idA, parent.ID, false, nil)

	form := url.Values{"parent_id": {"anime"}}
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(implied.ID, 10)+"/implied-by", form)
	mustCode(t, w, http.StatusSeeOther)
	drainJob(t, srv)

	n := countRows(t, srv.db(), "tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?", parent.ID, implied.ID)
	if n != 1 {
		t.Errorf("edge count = %d, want 1", n)
	}
	if n := countRows(t, srv.db(), "image_tags WHERE tag_id = ? AND is_implied = 1", implied.ID); n != 1 {
		t.Errorf("implied rows = %d, want 1", n)
	}

	w = postForm(t, srv, "/tags/"+strconv.FormatInt(implied.ID, 10)+"/implied-by", form)
	wantIn(t, w.Body.String(), "Already declared; re-applying it.")
	drainJob(t, srv)
}

func TestAddImplicationPost_RedeclareReappliesTheEdge(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "a.png", 11, 11)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	implied := mustTag(t, srv.tagSvc(), "japanese", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), id, parent.ID, false, nil)
	mustAddImply(t, srv.tagSvc(), parent.ID, implied.ID)

	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", id, implied.ID); n != 0 {
		t.Fatalf("implied rows before = %d, want 0", n)
	}

	w := postForm(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications", url.Values{"implied_id": {"japanese"}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Already declared; re-applying it.")
	drainJob(t, srv)

	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1", id, implied.ID); n != 1 {
		t.Errorf("implied rows after = %d, want 1", n)
	}
}

func TestAddTagAliasPost_CreatesPointingAlias(t *testing.T) {
	srv := newTestServer(t)
	canon := mustTag(t, srv.tagSvc(), "blonde_hair", srv.active().GeneralCategoryID)

	form := url.Values{"name": {"blond_hair yellow_hair"}}
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(canon.ID, 10)+"/aliases", form)
	mustCode(t, w, http.StatusSeeOther)
	n := countRows(t, srv.db(), "tags WHERE is_alias = 1 AND canonical_tag_id = ?", canon.ID)
	if n != 2 {
		t.Errorf("alias rows = %d, want 2", n)
	}

	taken := mustTag(t, srv.tagSvc(), "taken", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), seedImage(t, srv, "t.png", 11, 11), taken.ID, false, nil)
	w = postForm(t, srv, "/tags/"+strconv.FormatInt(canon.ID, 10)+"/aliases", url.Values{"name": {"taken"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "flash-err") {
		t.Errorf("collision: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAddImplicationPost_CycleRejected(t *testing.T) {
	srv := newTestServer(t)
	parent := mustTag(t, srv.tagSvc(), "p", srv.active().GeneralCategoryID)
	child := mustTag(t, srv.tagSvc(), "c", srv.active().GeneralCategoryID)
	mustAddImply(t, srv.tagSvc(), parent.ID, child.ID)

	form := url.Values{"implied_id": {"p"}}
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(child.ID, 10)+"/implications", form)
	body := w.Body.String()
	wantIn(t, body, "cycle")
}

func TestImplicationRoutes_NothingToPropagateKeepsThePreviousJobState(t *testing.T) {
	srv := newTestServer(t)
	parent := mustTag(t, srv.tagSvc(), "p", srv.active().GeneralCategoryID)
	child := mustTag(t, srv.tagSvc(), "c", srv.active().GeneralCategoryID)
	mustAddImply(t, srv.tagSvc(), parent.ID, child.ID)
	mustStartJob(t, srv.jobs, models.JobTypeDelete)
	srv.jobs.Fail("boom")
	finished := srv.jobs.Finished()

	wantIn(t, postForm(t, srv, "/tags/"+strconv.FormatInt(child.ID, 10)+"/implications", url.Values{"implied_id": {"p"}}).Body.String(), "cycle")
	req := httptest.NewRequest("DELETE", "/tags/"+strconv.FormatInt(child.ID, 10)+"/implications/group", strings.NewReader(url.Values{"origin": {""}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	wantIn(t, w.Body.String()+w.Header().Get("HX-Trigger"), "0 implications removed.")
	gone := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d/implications/%d", child.ID, parent.ID))
	mustCode(t, gone, http.StatusBadRequest)

	if st := srv.jobs.Get(); st == nil || st.Error != "boom" {
		t.Errorf("job = %+v, want the earlier failure still shown", st)
	}
	if got := srv.jobs.Finished(); got != finished {
		t.Errorf("Finished() = %d, want %d: a request with nothing to propagate ended a job", got, finished)
	}
}

func TestDeleteImplicationPost_RemovesAndPropagates(t *testing.T) {
	srv := newTestServer(t)
	idA := seedImage(t, srv, "a.png", 10, 10)
	parent := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), idA, parent.ID, false, nil)

	addForm := url.Values{"implied_id": {"japanese"}}
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications", addForm)
	mustCode(t, w, http.StatusNoContent)
	drainJob(t, srv)

	var impliedTagID int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, "japanese").Scan(&impliedTagID); err != nil {
		t.Fatal(err)
	}
	delW := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d/implications/%d", parent.ID, impliedTagID))
	mustCode(t, delW, http.StatusNoContent)
	drainJob(t, srv)

	n := countRows(t, srv.db(), "image_tags WHERE tag_id = ? AND is_implied = 1", impliedTagID)
	if n != 0 {
		t.Errorf("implied rows after remove = %d, want 0", n)
	}
}

func TestDeleteImplication_SweepsTheWholeChain(t *testing.T) {
	srv := newTestServer(t)
	img := seedImage(t, srv, "chain.png", 10, 10)
	cat := srv.active().GeneralCategoryID
	mk := func(name string) int64 { return getOrCreateTagID(t, srv, name, cat) }

	leaf, side, hub, root := mk("ch_leaf"), mk("ch_side"), mk("ch_hub"), mk("ch_root")
	for _, edge := range [][2]int64{{root, hub}, {hub, leaf}, {hub, side}, {side, leaf}} {
		mustAddImply(t, srv.tagSvc(), edge[0], edge[1])
	}
	mustAddTag(t, srv.tagSvc(), img, root, false, nil)

	w := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d/implications/%d", root, hub))
	mustCode(t, w, http.StatusNoContent)
	drainJob(t, srv)

	for _, id := range []int64{hub, side, leaf} {
		n := countRows(t, srv.db(), "image_tags WHERE image_id = ? AND tag_id = ?", img, id)
		if n != 0 {
			t.Errorf("tag %d still on the image after its last edge went", id)
		}
	}
}

func TestDeleteImplication_SkipsMissingImagesInTheUsageDrop(t *testing.T) {
	srv := newTestServer(t)
	cat := srv.active().GeneralCategoryID
	mk := func(name string) int64 { return getOrCreateTagID(t, srv, name, cat) }
	leaf, hub, root := mk("md_leaf"), mk("md_hub"), mk("md_root")
	for _, edge := range [][2]int64{{root, hub}, {hub, leaf}} {
		mustAddImply(t, srv.tagSvc(), edge[0], edge[1])
	}

	other := seedImage(t, srv, "md_other.png", 10, 10)
	mustAddTag(t, srv.tagSvc(), other, leaf, false, nil)
	present := seedImage(t, srv, "md_present.png", 11, 11)
	missing := seedImage(t, srv, "md_missing.png", 12, 12)
	for _, id := range []int64{present, missing} {
		mustAddTag(t, srv.tagSvc(), id, root, false, nil)
	}
	if _, err := gallery.SetMissing(srv.db(), missing, true); err != nil {
		t.Fatal(err)
	}

	w := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d/implications/%d", root, hub))
	mustCode(t, w, http.StatusNoContent)
	drainJob(t, srv)

	var got int
	if err := srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, leaf).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("leaf usage_count = %d, want 1 (only the present image's row was counted)", got)
	}
}

func TestRemoveImplicationGroup_ScopesToOneOrigin(t *testing.T) {
	srv := newTestServer(t)
	img := seedImage(t, srv, "grp.png", 10, 10)
	catID := srv.active().GeneralCategoryID
	mk := func(name string) int64 {
		t.Helper()
		tag := mustTag(t, srv.tagSvc(), name, catID)
		return tag.ID
	}
	subject := mk("subject")
	mustAddTag(t, srv.tagSvc(), img, subject, false, nil)
	ptrA, ptrB, userChild := mk("ptr_a"), mk("ptr_b"), mk("user_child")
	for _, edge := range [][2]int64{{subject, ptrA}, {subject, ptrB}} {
		if _, err := srv.tagSvc().AddImplicationFrom(edge[0], edge[1], "ptr"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.tagSvc().AddImplicationFrom(subject, userChild, "user"); err != nil {
		t.Fatal(err)
	}
	parentA, parentB := mk("parent_a"), mk("parent_b")
	for _, p := range []int64{parentA, parentB} {
		if _, err := srv.tagSvc().AddImplicationFrom(p, subject, "ptr"); err != nil {
			t.Fatal(err)
		}
	}
	mustAddTag(t, srv.tagSvc(), img, subject, false, nil)

	del := func(path string) {
		t.Helper()
		w := deleteCSRF(t, srv, path)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: code=%d body=%s", path, w.Code, w.Body.String())
		}
		trigger := w.Header().Get("HX-Trigger")
		wantIn(t, trigger, "2 implications removed.")
		wantIn(t, trigger, "tag-relations-changed")
		drainJob(t, srv)
	}
	del(fmt.Sprintf("/tags/%d/implications/group?origin=ptr&stale=0", subject))

	implied, err := srv.tagSvc().ListImplications(subject)
	if err != nil {
		t.Fatal(err)
	}
	if len(implied) != 1 || implied[0].ImpliedID != userChild {
		t.Fatalf("implications after the ptr group delete = %+v, want only user_child", implied)
	}
	for _, id := range []int64{ptrA, ptrB} {
		n := countRows(t, srv.db(), "image_tags WHERE tag_id = ? AND is_implied = 1", id)
		if n != 0 {
			t.Errorf("implied rows left for tag %d = %d, want 0 (sweep ran for every edge)", id, n)
		}
	}

	del(fmt.Sprintf("/tags/%d/implied-by/group?origin=ptr&stale=0", subject))
	inbound, err := srv.tagSvc().ImpliedBy(subject)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbound) != 0 {
		t.Errorf("implied-by after the group delete = %+v, want none", inbound)
	}
}

func TestRemoveTagAliasesGroup_ScopesToOneOrigin(t *testing.T) {
	srv := newTestServer(t)
	catID := srv.active().GeneralCategoryID
	canon := mustTag(t, srv.tagSvc(), "canon", catID)
	for _, name := range []string{"ptr_alias_a", "ptr_alias_b"} {
		if _, err := srv.tagSvc().CreateAliasFrom(name, catID, canon.ID, "ptr"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.tagSvc().CreateAliasFrom("user_alias", catID, canon.ID, "user"); err != nil {
		t.Fatal(err)
	}

	w := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d/aliases/group?origin=ptr&stale=0", canon.ID))
	mustCode(t, w, http.StatusNoContent)

	byCanonical, err := srv.tagSvc().AliasesForTagIDs([]int64{canon.ID})
	if err != nil {
		t.Fatal(err)
	}
	left := byCanonical[canon.ID]
	if len(left) != 1 || left[0].Name != "user_alias" {
		t.Fatalf("aliases after the ptr group delete = %+v, want only user_alias", left)
	}
}

func TestRunImplicationEdges_ChunkedAcrossBoundary(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("seeds 600+ images; skipping in short mode")
	}
	srv := newTestServer(t)
	parent := mustTag(t, srv.tagSvc(), "parent", srv.active().GeneralCategoryID)
	const carriers = 600

	for i := 0; i < carriers; i++ {
		id := insertRawImage(t, srv, fmt.Sprintf("img%03d.png", i), fmt.Sprintf("%064x", i+1))
		mustAddTag(t, srv.tagSvc(), id, parent.ID, false, nil)
	}
	implied := mustTag(t, srv.tagSvc(), "implied", srv.active().GeneralCategoryID)
	mustAddImply(t, srv.tagSvc(), parent.ID, implied.ID)
	mustStartJob(t, srv.jobs, models.JobTypeTag)
	go srv.runImplicationEdges([]models.Implication{{ParentID: parent.ID, ImpliedID: implied.ID}}, "add")
	drainJob(t, srv)

	rowCount := countRows(t, srv.db(), "image_tags WHERE tag_id = ? AND is_implied = 1", implied.ID)
	if rowCount != carriers {
		t.Errorf("implied rows = %d, want %d", rowCount, carriers)
	}

	var usage int
	if err := srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, implied.ID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != carriers {
		t.Errorf("usage_count = %d, want %d", usage, carriers)
	}

	st := srv.jobs.Get()
	summary := st.Summary
	if summary == "" {
		summary = st.Message
	}
	wantIn(t, summary, fmt.Sprintf("%d image", carriers))
	if st.JobType != models.JobTypeTag {
		t.Errorf("job type = %q", st.JobType)
	}
}

var implSummaryRe = regexp.MustCompile(`^applying implication cancelled \((\d+)/(\d+)\)$`)

func TestRunImplicationEdges_CancelledMidRun(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds many images to span chunk boundaries; skipping in short mode")
	}
	srv := newTestServer(t)
	parent := mustTag(t, srv.tagSvc(), "parent", srv.active().GeneralCategoryID)

	const carriers = 600
	for i := 0; i < carriers; i++ {
		id := insertRawImage(t, srv, fmt.Sprintf("c%05d.png", i), fmt.Sprintf("%064x", i+1))
		mustAddTag(t, srv.tagSvc(), id, parent.ID, false, nil)
	}
	implied := mustTag(t, srv.tagSvc(), "implied", srv.active().GeneralCategoryID)
	mustAddImply(t, srv.tagSvc(), parent.ID, implied.ID)

	mustStartJob(t, srv.jobs, models.JobTypeTag)

	hold, err := srv.db().Write.Begin()
	if err != nil {
		t.Fatalf("hold the write connection: %v", err)
	}
	done := make(chan struct{})
	go func() {
		srv.runImplicationEdges([]models.Implication{{ParentID: parent.ID, ImpliedID: implied.ID}}, "add")
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := srv.jobs.Get()
		if st.Total == carriers && strings.Contains(st.Message, "applying implication") {
			break
		}
		if !st.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chunk loop did not start within 5s")
		}
		runtime.Gosched()
	}
	srv.jobs.Cancel()

	if err := hold.Rollback(); err != nil {
		t.Fatalf("release the write connection: %v", err)
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("worker did not finish within 60s of cancel")
	}

	st := srv.jobs.Get()
	summary := st.Summary
	if summary == "" {
		summary = st.Message
	}
	m := implSummaryRe.FindStringSubmatch(summary)
	if m == nil {
		t.Fatalf("cancel summary = %q, want shape %q", summary, implSummaryRe.String())
	}
	if st.Error != "" {
		t.Errorf("cancelled run must Complete with a summary, not Fail: error=%q", st.Error)
	}
	processed, _ := strconv.Atoi(m[1])
	total, _ := strconv.Atoi(m[2])
	if total != carriers {
		t.Errorf("summary total = %d, want %d", total, carriers)
	}
	if processed < 0 || processed >= carriers {
		t.Errorf("cancelled processed = %d, want a partial count in [0,%d)", processed, carriers)
	}
	if processed%chunkSizeImplication != 0 {
		t.Errorf("cancelled processed = %d, want a 500-row chunk boundary", processed)
	}
}

const chunkSizeImplication = 500

func TestImplicationsDialogHandler_NonHTMXRedirects(t *testing.T) {
	srv := newTestServer(t)
	tag := mustTag(t, srv.tagSvc(), "anime", srv.active().GeneralCategoryID)
	path := fmt.Sprintf("/tags/%d/implications", tag.ID)

	w := do(srv, "GET", path)
	mustCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != fmt.Sprintf("/tags/%d", tag.ID) {
		t.Errorf("redirect Location = %q, want /tags/%d", loc, tag.ID)
	}

	hxW := do(srv, "GET", path, [2]string{"HX-Request", "true"})
	mustCode(t, hxW, http.StatusOK)
	wantIn(t, hxW.Body.String(), "No implications declared.")
}

func getOrCreateTagID(t *testing.T, srv *Server, name string, categoryID int64) int64 {
	t.Helper()
	tag := mustTag(t, srv.tagSvc(), name, categoryID)
	return tag.ID
}

func TestRemoveImplicationButtons_RefusalsLandInTheTagFlash(t *testing.T) {
	srv := newTestServer(t)
	cat := srv.active().GeneralCategoryID
	mk := func(name string) int64 { return getOrCreateTagID(t, srv, name, cat) }
	above, subject, a, b, stray := mk("rf_above"), mk("rf_subject"), mk("rf_a"), mk("rf_b"), mk("rf_stray")
	for _, edge := range [][2]int64{{above, subject}, {subject, a}, {subject, b}} {
		mustAddImply(t, srv.tagSvc(), edge[0], edge[1])
	}
	mustAddImply(t, srv.tagSvc(), stray, subject)

	button := regexp.MustCompile(`hx-delete="/tags/[^"]*/(implications|implied-by)[^"]*"(\s*hx-target="#tag-flash")?`)
	for _, path := range []string{fmt.Sprintf("/tags/%d", subject), fmt.Sprintf("/tags/%d/implications", subject)} {
		page := do(srv, "GET", path, [2]string{"HX-Request", "true"})
		mustCode(t, page, http.StatusOK)
		found := button.FindAllStringSubmatch(page.Body.String(), -1)
		if len(found) < 2 {
			t.Fatalf("%s: %d remove buttons, want at least 2", path, len(found))
		}
		for _, m := range found {
			if m[2] == "" {
				t.Errorf("%s: %s has no #tag-flash target, so a 409 replaces the button", path, m[0])
			}
		}
	}

	w := deleteCSRF(t, srv, fmt.Sprintf("/tags/%d/implications/%d", subject, stray), [2]string{"HX-Request", "true"})
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Header().Get("HX-Trigger"), `"action-flash":{"kind":"err"`)
}
