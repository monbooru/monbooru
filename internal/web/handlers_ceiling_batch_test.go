package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func awaitJobsDrain(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background job never drained")
}

func seedRatedPair(t *testing.T, srv *Server) (safe, explicit int64) {
	t.Helper()
	cx := srv.active()
	safe = seedImage(t, srv, "safe.png", 10, 10)
	explicit = seedImage(t, srv, "explicit.png", 11, 11)
	mustAddTag(t, cx.TagSvc, safe, ratingTagIDWeb(t, cx.DB, "general"), false, nil)
	mustAddTag(t, cx.TagSvc, explicit, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil)
	cx.InvalidateCaches()
	return safe, explicit
}

func postCeilingRequest(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, srv, path, form, [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
}

func TestDeleteSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	w := postCeilingRequest(t, srv, "/internal/delete-search", url.Values{"q": {""}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	safeAlive := countRows(t, srv.db(), "images WHERE id = ?", safe)
	explicitAlive := countRows(t, srv.db(), "images WHERE id = ?", explicit)
	if safeAlive != 0 {
		t.Errorf("safe image %d should have been deleted; still present", safe)
	}
	if explicitAlive != 1 {
		t.Errorf("explicit image %d should have survived under ceiling=sensitive; got %d rows", explicit, explicitAlive)
	}
}

func TestBatchTag_ScopeSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	w := postCeilingRequest(t, srv, "/internal/batch-tag", url.Values{
		"scope": {"search"}, "q": {""}, "op": {"add"}, "tags": {"mark_under_ceiling"},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	carriesMark := func(id int64) bool {
		return countRows(t, srv.db(), "image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.name = 'mark_under_ceiling'", id) == 1
	}
	if !carriesMark(safe) {
		t.Errorf("safe image %d should have gained the mark", safe)
	}
	if carriesMark(explicit) {
		t.Errorf("explicit image %d should NOT have gained the mark under ceiling=sensitive", explicit)
	}
}

func TestBatchMove_ScopeSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	target := "moved"
	w := postCeilingRequest(t, srv, "/internal/batch-place", url.Values{
		"scope": {"search"}, "q": {""}, "folder": {target},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	folderOf := func(id int64) string {
		var p string
		_ = srv.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&p)
		return p
	}
	if got := folderOf(safe); got != target {
		t.Errorf("safe image %d folder = %q, want %q", safe, got, target)
	}
	if got := folderOf(explicit); got == target {
		t.Errorf("explicit image %d should not have moved under ceiling=sensitive; folder = %q", explicit, got)
	}
}

func TestMarkedWalkerDeleteAll_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	rel := cx.RelationsSvc

	dim := 7
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}

	safeOrig, safeDup := mkImg("safe_orig.png"), mkImg("safe_dup.png")
	taintOrig, taintDup := mkImg("taint_orig.png"), mkImg("taint_dup.png")
	mustAddTag(t, cx.TagSvc, taintDup, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil)
	if err := rel.AddDuplicate(safeOrig, safeDup); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDuplicate(taintOrig, taintDup); err != nil {
		t.Fatal(err)
	}

	w := postCeilingRequest(t, srv, "/relations/duplicates/marked/delete-all", url.Values{})
	mustCode(t, w, http.StatusOK)
	awaitJobsDrain(t, srv)

	alive := func(id int64) int {
		return countRows(t, srv.db(), "images WHERE id = ?", id)
	}
	if alive(safeDup) != 0 {
		t.Errorf("safe duplicate %d should have been deleted", safeDup)
	}
	if alive(taintDup) != 1 {
		t.Errorf("explicit duplicate %d should have survived (ceiling=sensitive hides it from the walker)", taintDup)
	}
	if alive(safeOrig) != 1 || alive(taintOrig) != 1 {
		t.Errorf("originals must always survive: safeOrig=%d taintOrig=%d", alive(safeOrig), alive(taintOrig))
	}
}

func TestFileDuplicatesRemoveAll_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	dim := 9
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}

	safe := mkImg("safe.png")
	explicit := mkImg("explicit.png")
	mustAddTag(t, cx.TagSvc, explicit, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil)
	addAlias := func(imgID int64, name string) int64 {
		t.Helper()
		fullPath := filepath.Join(cx.GalleryPath, name)
		id, _ := mustExec(t, srv.db().Write,
			`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, imgID, fullPath).LastInsertId()
		return id
	}
	safeAlias := addAlias(safe, "safe_alias.png")
	explicitAlias := addAlias(explicit, "explicit_alias.png")

	w := postCeilingRequest(t, srv, "/relations/file-duplicates/remove", url.Values{"all": {"true"}})
	mustCode(t, w, http.StatusOK)
	awaitJobsDrain(t, srv)

	pathAlive := func(id int64) int {
		return countRows(t, srv.db(), "image_paths WHERE id = ?", id)
	}
	if pathAlive(safeAlias) != 0 {
		t.Errorf("safe alias %d should have been deleted", safeAlias)
	}
	if pathAlive(explicitAlias) != 1 {
		t.Errorf("explicit alias %d should have survived under ceiling=sensitive", explicitAlias)
	}
}

func TestSha256Walker_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	safe := seedImage(t, srv, "safe.png", 9, 9)
	explicit := seedImage(t, srv, "explicit.png", 10, 10)
	mustAddTag(t, cx.TagSvc, explicit, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil)
	for _, a := range []struct {
		id   int64
		name string
	}{{safe, "safe_alias.png"}, {explicit, "explicit_alias.png"}} {
		mustExec(t, srv.db().Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, a.id, filepath.Join(cx.GalleryPath, a.name))
	}

	w := do(srv, "GET", "/relations/duplicates/sha256", [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "safe_alias.png")
	wantNotIn(t, body, "explicit_alias.png")
}

func TestBatchInbox_ScopeSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)
	for _, id := range []int64{safe, explicit} {
		var v int
		_ = srv.db().Read.QueryRow(`SELECT is_inbox FROM images WHERE id = ?`, id).Scan(&v)
		if v != 1 {
			t.Fatalf("seedImage %d expected is_inbox=1, got %d", id, v)
		}
	}

	w := postCeilingRequest(t, srv, "/internal/batch-inbox", url.Values{
		"scope": {"search"}, "q": {""},
	})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	if got := srv.jobs.Get().Summary; got != "Toggled inbox state for 1 image." {
		t.Errorf("summary = %q", got)
	}

	inboxOf := func(id int64) int {
		var v int
		_ = srv.db().Read.QueryRow(`SELECT is_inbox FROM images WHERE id = ?`, id).Scan(&v)
		return v
	}
	if got := inboxOf(safe); got != 0 {
		t.Errorf("safe image %d should have been toggled out of inbox; is_inbox=%d", safe, got)
	}
	if got := inboxOf(explicit); got != 1 {
		t.Errorf("explicit image %d should have stayed in inbox under ceiling=sensitive; is_inbox=%d", explicit, got)
	}
}

func TestSearchIDs_ListsTheMatchesUnderTheCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	ids := func(req *http.Request) []int64 {
		t.Helper()
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var out struct {
			IDs       []int64 `json:"ids"`
			Truncated bool    `json:"truncated"`
		}
		mustUnmarshal(t, w.Body.Bytes(), &out)
		if out.Truncated {
			t.Fatalf("body %q: truncated on a two-row fixture", w.Body.String())
		}
		return out.IDs
	}

	got := ids(httptest.NewRequest("GET", "/internal/search/ids?q=", nil))
	if !slices.Contains(got, safe) || !slices.Contains(got, explicit) {
		t.Fatalf("ids = %v, want both %d and %d with no ceiling", got, safe, explicit)
	}

	capped := httptest.NewRequest("GET", "/internal/search/ids?q=", nil)
	capped.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	got = ids(capped)
	if !slices.Contains(got, safe) {
		t.Errorf("ids = %v, want the general row kept", got)
	}
	if slices.Contains(got, explicit) {
		t.Errorf("ids = %v, want the explicit row hidden by the ceiling", got)
	}
}

func TestSearchIDs_StopsAtTheCapAndSaysSo(t *testing.T) {
	srv := newTestServer(t)
	seedRatedPair(t, srv)
	old := searchIDsCap
	searchIDsCap = 1
	t.Cleanup(func() { searchIDsCap = old })

	w := do(srv, "GET", "/internal/search/ids?q=")
	var out struct {
		IDs       []int64 `json:"ids"`
		Truncated bool    `json:"truncated"`
	}
	mustUnmarshal(t, w.Body.Bytes(), &out)
	if len(out.IDs) != 1 || !out.Truncated {
		t.Fatalf("ids = %v truncated = %v, want one id and truncated", out.IDs, out.Truncated)
	}
}
