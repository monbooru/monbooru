package web

import (
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMarkedWalkerDeleteOne_MissingImage404s(t *testing.T) {
	srv := newTestServer(t)
	w := postForm(t, srv, "/relations/duplicates/marked/delete-one", url.Values{"image_id": {"999999"}})
	mustCode(t, w, http.StatusNotFound)
	wantIn(t, w.Header().Get("HX-Trigger"), `"action-flash":{"kind":"err","text":"Image #999999 is not a marked duplicate."}`)
}

func TestSha256WalkerRemoveOne_RefusalsReachTheFlashHeader(t *testing.T) {
	srv := newTestServer(t)
	for form, want := range map[string]string{"x": "Invalid path id.", "999999": "Not a non-canonical path."} {
		w := postHTMXForm(t, srv, "/relations/duplicates/sha256/remove-one", url.Values{"path_id": {form}})
		wantIn(t, w.Header().Get("HX-Trigger"), `"text":"`+want+`"`)
	}
}

func TestSha256Walker_PagesALongList(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "canonical.png", 10, 10)

	extra := duplicatesWalkerPageSize + 20
	for i := range extra {
		mustExec(t, srv.db().Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, fmt.Sprintf("/copies/dup-%03d.png", i))
	}

	body := func(path string) string {
		w := do(srv, "GET", path)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		return w.Body.String()
	}

	first := body("/relations/duplicates/sha256")
	if got := strings.Count(first, `name="path_id"`); got != duplicatesWalkerPageSize {
		t.Errorf("page 1 rendered %d rows, want %d", got, duplicatesWalkerPageSize)
	}
	wantIn(t, first, "Page 1 / 2")

	second := body("/relations/duplicates/sha256?page=2")
	if got := strings.Count(second, `name="path_id"`); got != extra-duplicatesWalkerPageSize {
		t.Errorf("page 2 rendered %d rows, want %d", got, extra-duplicatesWalkerPageSize)
	}

	if clamped := body("/relations/duplicates/sha256?page=99"); !strings.Contains(clamped, "Page 2 / 2") {
		t.Error("a past-the-end page did not clamp onto the last")
	}
}

func seedAliasCopy(t *testing.T, srv *Server, id int64, name string) (int64, string) {
	t.Helper()
	cx := srv.active()
	path := filepath.Join(cx.GalleryPath, name)
	original, err := os.ReadFile(canonicalPathOf(t, srv, id))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, original, 0o644)
	pathID, _ := mustExec(t, srv.db().Write,
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, path).LastInsertId()
	return pathID, path
}

func canonicalPathOf(t *testing.T, srv *Server, id int64) string {
	t.Helper()
	var p string
	if err := srv.db().Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSha256WalkerRemoveOne_DeletesTheDuplicateFile(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	canonical := canonicalPathOf(t, srv, id)
	pathID, aliasPath := seedAliasCopy(t, srv, id, "copy.png")

	w := postForm(t, srv, "/relations/duplicates/sha256/remove-one",
		url.Values{"path_id": {strconv.FormatInt(pathID, 10)}})
	mustCode(t, w, http.StatusSeeOther)

	if _, err := os.Stat(aliasPath); !os.IsNotExist(err) {
		t.Error("the duplicate file is still on disk")
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("the canonical file went with the duplicate: %v", err)
	}
	rows := countRows(t, srv.db(), "image_paths WHERE id = ?", pathID)
	if rows != 0 {
		t.Error("the alias row outlived its file")
	}
}

func TestSha256WalkerRemoveOne_KeepsTheCanonicalEntryReachedThroughALink(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	canonical := canonicalPathOf(t, srv, id)
	cx := srv.active()
	if err := os.Symlink(cx.GalleryPath, filepath.Join(cx.GalleryPath, "pictures")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasPath := filepath.Join(cx.GalleryPath, "pictures", filepath.Base(canonical))
	pathID, _ := mustExec(t, srv.db().Write,
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, aliasPath).LastInsertId()

	w := postForm(t, srv, "/relations/duplicates/sha256/remove-one",
		url.Values{"path_id": {strconv.FormatInt(pathID, 10)}})
	mustCode(t, w, http.StatusSeeOther)

	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("removing the alias unlinked the canonical file: %v", err)
	}
	if rows := countRows(t, srv.db(), "image_paths WHERE id = ?", pathID); rows != 0 {
		t.Error("the phantom alias row survived")
	}
}

func TestSha256WalkerRemoveOne_RemovesAHardLinkedDuplicate(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	canonical := canonicalPathOf(t, srv, id)
	cx := srv.active()
	aliasPath := filepath.Join(cx.GalleryPath, "hardlink.png")
	if err := os.Link(canonical, aliasPath); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	pathID, _ := mustExec(t, srv.db().Write,
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, aliasPath).LastInsertId()

	w := postForm(t, srv, "/relations/duplicates/sha256/remove-one",
		url.Values{"path_id": {strconv.FormatInt(pathID, 10)}})
	mustCode(t, w, http.StatusSeeOther)

	if _, err := os.Stat(aliasPath); !os.IsNotExist(err) {
		t.Error("the hard-linked duplicate is still on disk")
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("the canonical file went with it: %v", err)
	}
}

func TestPromoteCanonical_SwapsTheCanonicalPath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "original.png", 10, 10)
	wasCanonical := canonicalPathOf(t, srv, id)
	_, aliasPath := seedAliasCopy(t, srv, id, "preferred.png")

	w := postForm(t, srv, fmt.Sprintf("/images/%d/canonical-path", id), url.Values{"path": {aliasPath}})
	mustCode(t, w, http.StatusSeeOther)

	if got := canonicalPathOf(t, srv, id); got != aliasPath {
		t.Errorf("canonical_path = %q, want the promoted %q", got, aliasPath)
	}
	canonicalCount := countRows(t, srv.db(), "image_paths WHERE image_id = ? AND is_canonical = 1", id)
	if canonicalCount != 1 {
		t.Errorf("%d canonical paths after the promotion, want exactly 1", canonicalCount)
	}
	for _, p := range []string{wasCanonical, aliasPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("promotion removed %q: %v", p, err)
		}
	}
}

func TestPromoteCanonical_FollowsTheFolderPath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "flat.png", 10, 10)
	cx := srv.active()
	mustMkdirAll(t, filepath.Join(cx.GalleryPath, "nested"), 0o755)
	_, aliasPath := seedAliasCopy(t, srv, id, filepath.Join("nested", "deep.png"))

	w := postForm(t, srv, fmt.Sprintf("/images/%d/canonical-path", id), url.Values{"path": {aliasPath}})
	mustCode(t, w, http.StatusSeeOther)

	if got := canonicalPathOf(t, srv, id); got != aliasPath {
		t.Errorf("canonical_path = %q, want %q", got, aliasPath)
	}
	var folder string
	if err := srv.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	if folder != "nested" {
		t.Errorf("folder_path = %q, want nested", folder)
	}
}

var duplicateRemovals = []struct {
	name          string
	removed, kept int
	remove        func(t *testing.T, srv *Server, id, pathID int64) *httptest.ResponseRecorder
}{
	{"detail panel", http.StatusOK, http.StatusNoContent, func(t *testing.T, srv *Server, id, pathID int64) *httptest.ResponseRecorder {
		w := deleteWithCSRF(t, srv, fmt.Sprintf("/images/%d/aliases/%d", id, pathID))
		return w
	}},
	{"walker row", http.StatusSeeOther, http.StatusConflict, func(t *testing.T, srv *Server, _, pathID int64) *httptest.ResponseRecorder {
		return postForm(t, srv, "/relations/duplicates/sha256/remove-one",
			url.Values{"path_id": {strconv.FormatInt(pathID, 10)}})
	}},
	{"remove all", http.StatusOK, http.StatusOK, func(t *testing.T, srv *Server, _, _ int64) *httptest.ResponseRecorder {
		w := postForm(t, srv, "/relations/file-duplicates/remove", url.Values{"all": {"true"}})
		awaitJobsDrain(t, srv)
		return w
	}},
}

func TestDuplicateRemovalsKeepACopyOverwrittenSince(t *testing.T) {
	for _, tc := range duplicateRemovals {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			id := seedImage(t, srv, "keeper.png", 10, 10)
			pathID, copyPath := seedAliasCopy(t, srv, id, "copy.png")

			mustWriteFile(t, copyPath, tinyPNG(t, 7, 7, color.RGBA{9, 9, 9, 255}), 0o644)

			mustCode(t, tc.remove(t, srv, id, pathID), tc.kept)
			if _, err := os.Stat(copyPath); err != nil {
				t.Errorf("the file written over the copy was deleted: %v", err)
			}
			if n := countRows(t, srv.db(), "image_paths WHERE id = ?", pathID); n != 1 {
				t.Error("the kept copy lost its row")
			}
		})
	}
}

func TestDuplicateRemovalsKeepTheOnlyCopyWhenTheCanonicalIsGone(t *testing.T) {
	for _, tc := range duplicateRemovals {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			id := seedImage(t, srv, "keeper.png", 10, 10)
			pathID, copyPath := seedAliasCopy(t, srv, id, "copy.png")
			if err := os.Remove(canonicalPathOf(t, srv, id)); err != nil {
				t.Fatal(err)
			}

			mustCode(t, tc.remove(t, srv, id, pathID), tc.kept)
			if _, err := os.Stat(copyPath); err != nil {
				t.Errorf("the only copy left was deleted: %v", err)
			}
			if n := countRows(t, srv.db(), "image_paths WHERE id = ?", pathID); n != 1 {
				t.Error("the only copy left lost its row")
			}
		})
	}
}

func TestDuplicateRemovalsLeaveACopyInAnotherGallerysFolder(t *testing.T) {
	for _, tc := range duplicateRemovals {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			id := seedImage(t, srv, "keeper.png", 10, 10)
			inner := filepath.Join(srv.active().GalleryPath, ".H")
			mustMkdirAll(t, inner, 0o755)
			pathID, copyPath := seedAliasCopy(t, srv, id, filepath.Join(".H", "copy.png"))
			addRemovableGallery(t, srv, "H", inner)

			mustCode(t, tc.remove(t, srv, id, pathID), tc.removed)
			if _, err := os.Stat(copyPath); err != nil {
				t.Errorf("gallery H's file went with the alias: %v", err)
			}
			if n := countRows(t, srv.db(), "image_paths WHERE id = ?", pathID); n != 0 {
				t.Error("the alias row outlived the removal")
			}
		})
	}
}

func TestSha256WalkerAndRemoveAllLeaveOutACopyInAnotherGallerysFolder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	inner := filepath.Join(srv.active().GalleryPath, ".H")
	mustMkdirAll(t, inner, 0o755)
	seedAliasCopy(t, srv, id, filepath.Join(".H", "theirs.png"))
	seedAliasCopy(t, srv, id, "ours.png")
	addRemovableGallery(t, srv, "H", inner)

	walker := do(srv, "GET", "/relations/duplicates/sha256")
	mustCode(t, walker, http.StatusOK)
	wantNotIn(t, walker.Body.String(), "theirs.png")
	wantIn(t, walker.Body.String(), "ours.png")
	if got := strings.Count(walker.Body.String(), `name="path_id"`); got != 1 {
		t.Errorf("walker rendered %d rows, want only the gallery's own copy", got)
	}

	mustCode(t, postForm(t, srv, "/relations/file-duplicates/remove", url.Values{"all": {"true"}}), http.StatusOK)
	awaitJobsDrain(t, srv)
	st := srv.jobs.Get()
	wantIn(t, st.Summary+st.Message, "Removed 1 duplicate path.")
	if _, err := os.Stat(filepath.Join(inner, "theirs.png")); err != nil {
		t.Errorf("gallery H's file went with the alias: %v", err)
	}
}

func TestRemoveAllDuplicateFiles_CountsOnlyTheFilesItRemoved(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	_, removed := seedAliasCopy(t, srv, id, "copy.png")
	_, overwritten := seedAliasCopy(t, srv, id, "swapped.png")
	mustWriteFile(t, overwritten, tinyPNG(t, 7, 7, color.RGBA{9, 9, 9, 255}), 0o644)

	mustCode(t, postForm(t, srv, "/relations/file-duplicates/remove", url.Values{"all": {"true"}}), http.StatusOK)
	awaitJobsDrain(t, srv)

	st := srv.jobs.Get()
	wantIn(t, st.Summary+st.Message, "Removed 1 duplicate path; kept 1 changed since indexing.")
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Errorf("the true copy is still on disk (%v)", err)
	}
	if _, err := os.Stat(overwritten); err != nil {
		t.Errorf("the overwritten copy was deleted: %v", err)
	}
}

func TestRemoveDuplicateFiles_RefusesAPostWithoutAll(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	pathID, copyPath := seedAliasCopy(t, srv, id, "copy.png")

	w := postForm(t, srv, "/relations/file-duplicates/remove", url.Values{"path_id": {strconv.FormatInt(pathID, 10)}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-err")
	awaitJobsDrain(t, srv)
	if _, err := os.Stat(copyPath); err != nil {
		t.Errorf("a post naming a path id removed it: %v", err)
	}
}

func TestSha256WalkerRemoveOne_NamesTheCopyItKept(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keeper.png", 10, 10)
	pathID, copyPath := seedAliasCopy(t, srv, id, "copy.png")
	mustWriteFile(t, copyPath, tinyPNG(t, 7, 7, color.RGBA{9, 9, 9, 255}), 0o644)

	w := postForm(t, srv, "/relations/duplicates/sha256/remove-one",
		url.Values{"path_id": {strconv.FormatInt(pathID, 10)}}, [2]string{"HX-Request", "true"})

	mustCode(t, w, http.StatusNoContent)
	trigger := w.Header().Get("HX-Trigger")
	wantIn(t, trigger, "action-flash")
	wantIn(t, trigger, "copy.png")
	wantIn(t, trigger, `"err"`)
}

func TestWalkerRowDelete_ReloadsThePageItCameFrom(t *testing.T) {
	cases := []struct {
		route, flash string
		post         func(t *testing.T, srv *Server) url.Values
	}{
		{"sha256/remove-one", "copy.png.", func(t *testing.T, srv *Server) url.Values {
			id := seedImage(t, srv, "keeper.png", 10, 10)
			pathID, _ := seedAliasCopy(t, srv, id, "copy.png")
			return url.Values{"path_id": {strconv.FormatInt(pathID, 10)}}
		}},
		{"marked/delete-one", "Deleted image #", func(t *testing.T, srv *Server) url.Values {
			_, dup := seedMarkedPair(t, srv, "pair", 10)
			return url.Values{"image_id": {strconv.FormatInt(dup, 10)}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			srv := newTestServer(t)
			w := postForm(t, srv, "/relations/duplicates/"+tc.route, tc.post(t, srv), [2]string{"HX-Request", "true"})

			mustCode(t, w, http.StatusNoContent)
			if got := w.Header().Get("HX-Redirect"); got != "" {
				t.Errorf("HX-Redirect = %q, want the page reloaded where it was", got)
			}
			if got := w.Header().Get("HX-Refresh"); got != "true" {
				t.Errorf("HX-Refresh = %q, want true", got)
			}
			trigger := w.Header().Get("HX-Trigger")
			wantIn(t, trigger, "Deleted ")
			wantIn(t, trigger, tc.flash)
			wantIn(t, trigger, `"ok"`)
		})
	}
}

func TestCopyTagsToOriginalPost_DropsTheUntaggedCount(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	if err := cx.RelationsSvc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
	var gid, original int64
	if err := srv.db().Read.QueryRow(`SELECT id, original_image_id FROM dup_groups`).Scan(&gid, &original); err != nil {
		t.Fatal(err)
	}
	dup := a
	if original == a {
		dup = b
	}
	mustAddTag(t, srv.tagSvc(), dup, mustTag(t, srv.tagSvc(), "copied_tag", cx.GeneralCategoryID).ID, false, nil)
	if got := galleryResultCount(t, srv, "tagged:false"); got != "1" {
		t.Fatalf("tagged:false = %s before the copy, want the original", got)
	}

	mustCode(t, postForm(t, srv, "/relations/dup-group/"+strconv.FormatInt(gid, 10)+"/copy-tags", url.Values{}), http.StatusOK)

	if got := galleryResultCount(t, srv, "tagged:false"); got != "0" {
		t.Errorf("tagged:false = %s after the copy, want 0", got)
	}
}

func TestMarkedWalkerDeleteOne_RefusesAnImageInNoGroup(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "loner.png", 10, 10)
	path := canonicalPathOf(t, srv, id)

	w := postForm(t, srv, "/relations/duplicates/marked/delete-one", url.Values{"image_id": {strconv.FormatInt(id, 10)}})
	mustCode(t, w, http.StatusNotFound)
	wantIn(t, w.Body.String(), "is not a marked duplicate")
	if n := countRows(t, srv.db(), "images WHERE id = ?", id); n != 1 {
		t.Error("an image in no group was deleted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("its file was removed: %v", err)
	}
}

func seedMarkedPair(t *testing.T, srv *Server, prefix string, size int) (original, dup int64) {
	t.Helper()
	a := seedImage(t, srv, prefix+"_a.png", size, size)
	b := seedImage(t, srv, prefix+"_b.png", size+1, size+1)
	if err := srv.active().RelationsSvc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
	if err := srv.db().Read.QueryRow(
		`SELECT g.original_image_id FROM dup_group_members m JOIN dup_groups g ON g.id = m.group_id WHERE m.image_id = ?`, a,
	).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if original == a {
		return a, b
	}
	return b, a
}

var originalGoneWays = []struct {
	name string
	lose func(t *testing.T, srv *Server, original int64)
}{
	{"flagged missing", func(t *testing.T, srv *Server, original int64) {
		mustExec(t, srv.db().Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, original)
	}},
	{"file gone", func(t *testing.T, srv *Server, original int64) {
		if err := os.Remove(canonicalPathOf(t, srv, original)); err != nil {
			t.Fatal(err)
		}
	}},
}

func TestMarkedWalkerDeleteOne_RefusesWhenTheOriginalIsMissing(t *testing.T) {
	for _, tc := range originalGoneWays {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			original, dup := seedMarkedPair(t, srv, "pair", 10)
			dupPath := canonicalPathOf(t, srv, dup)
			tc.lose(t, srv, original)

			w := postForm(t, srv, "/relations/duplicates/marked/delete-one", url.Values{"image_id": {strconv.FormatInt(dup, 10)}})
			mustCode(t, w, http.StatusConflict)
			wantIn(t, w.Body.String(), fmt.Sprintf("original #%d is missing, so it was kept.", original))
			if n := countRows(t, srv.db(), "images WHERE id = ?", dup); n != 1 {
				t.Error("the member was deleted")
			}
			if _, err := os.Stat(dupPath); err != nil {
				t.Errorf("the member's file was removed: %v", err)
			}
		})
	}
}

func TestMarkedWalker_MarksAMissingOriginalAndOffersNoDelete(t *testing.T) {
	srv := newTestServer(t)
	original, dup := seedMarkedPair(t, srv, "gone", 10)
	_, kept := seedMarkedPair(t, srv, "here", 20)
	if err := os.Remove(canonicalPathOf(t, srv, original)); err != nil {
		t.Fatal(err)
	}

	body := mustGetBody(t, srv, "/relations/duplicates/marked")
	wantIn(t, body, "(missing)")
	wantNotIn(t, body, fmt.Sprintf(`name="image_id" value="%d"`, dup))
	wantIn(t, body, fmt.Sprintf(`name="image_id" value="%d"`, kept))
}

func TestMarkedWalker_NoCopyTagsWhenOnlyImpliedRowsDiffer(t *testing.T) {
	srv := newTestServer(t)
	_, dup := seedMarkedPair(t, srv, "pair", 10)
	implied := mustTag(t, srv.tagSvc(), "implied_only", srv.active().GeneralCategoryID)
	mustExec(t, srv.db().Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?, ?, 1)`, dup, implied.ID)

	w := do(srv, "GET", "/relations/duplicates/marked")
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf(`name="image_id" value="%d"`, dup))
	wantNotIn(t, w.Body.String(), `data-group-id="`)
}

func TestCopyTagsPreview_ListsWhatTheCopyAdds(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	_, dup := seedMarkedPair(t, srv, "pair", 10)
	mustAddTag(t, srv.tagSvc(), dup, mustTag(t, srv.tagSvc(), "copied_tag", cx.GeneralCategoryID).ID, false, nil)
	implied := mustTag(t, srv.tagSvc(), "implied_only", cx.GeneralCategoryID)
	mustExec(t, srv.db().Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?, ?, 1)`, dup, implied.ID)
	mustExec(t, srv.db().Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, dup, ratingTagIDWeb(t, cx.DB, "explicit"))
	var gid int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM dup_groups`).Scan(&gid); err != nil {
		t.Fatal(err)
	}

	w := do(srv, "GET", fmt.Sprintf("/relations/dup-group/%d/copy-tags/preview", gid))
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "This <strong>1</strong> tag will be copied")
	wantIn(t, body, "copied_tag")
	wantNotIn(t, body, "implied_only")
	wantNotIn(t, body, "explicit")
}

func TestCopySources_FromTheMarkedWalker(t *testing.T) {
	srv := newTestServer(t)
	_, dup := seedMarkedPair(t, srv, "pair", 10)
	mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'site_b', '7', 'https://b.example/7')`, dup)
	var gid int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM dup_groups`).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	if got := galleryResultCount(t, srv, "source:site_b"); got != "1" {
		t.Fatalf("source:site_b = %s before the copy, want the duplicate", got)
	}

	preview := fmt.Sprintf("/relations/dup-group/%d/copy-sources/preview", gid)
	wantIn(t, mustGetBody(t, srv, "/relations/duplicates/marked"), preview)
	body := mustGetBody(t, srv, preview)
	wantIn(t, body, "This <strong>1</strong> source will be copied")
	wantIn(t, body, "b.example")

	w := postForm(t, srv, fmt.Sprintf("/relations/dup-group/%d/copy-sources", gid), url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Copied 1 source to the original.")
	if got := galleryResultCount(t, srv, "source:site_b"); got != "2" {
		t.Errorf("source:site_b = %s after the copy, want both images", got)
	}
	wantNotIn(t, mustGetBody(t, srv, "/relations/duplicates/marked"), preview)
}

func TestMarkedWalkerDeleteAll_KeepsAGroupWhoseOriginalIsMissing(t *testing.T) {
	for _, tc := range originalGoneWays {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			lostOriginal, keptDup := seedMarkedPair(t, srv, "gone", 10)
			_, removedDup := seedMarkedPair(t, srv, "here", 20)
			keptPath := canonicalPathOf(t, srv, keptDup)
			tc.lose(t, srv, lostOriginal)

			mustCode(t, postForm(t, srv, "/relations/duplicates/marked/delete-all", url.Values{}), http.StatusOK)
			awaitJobsDrain(t, srv)

			st := srv.jobs.Get()
			wantIn(t, st.Summary+st.Message, "Removed 1 marked duplicate; kept 1 whose original is missing.")
			if n := countRows(t, srv.db(), "images WHERE id = ?", keptDup); n != 1 {
				t.Error("the member whose original is missing was deleted")
			}
			if _, err := os.Stat(keptPath); err != nil {
				t.Errorf("its file was removed: %v", err)
			}
			if n := countRows(t, srv.db(), "images WHERE id = ?", removedDup); n != 0 {
				t.Error("the member whose original is present was kept")
			}
		})
	}
}

func writeOverWithMore(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, append(body, "written over since"...), 0o644)
}

func TestMarkedWalkerDeletes_SayHowManyChangedFilesTheyKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, srv *Server, dup int64) string
	}{
		{"delete-one", func(t *testing.T, srv *Server, dup int64) string {
			w := postForm(t, srv, "/relations/duplicates/marked/delete-one",
				url.Values{"image_id": {strconv.FormatInt(dup, 10)}}, [2]string{"HX-Request", "true"})
			return w.Header().Get("HX-Trigger")
		}},
		{"delete-all", func(t *testing.T, srv *Server, _ int64) string {
			mustCode(t, postForm(t, srv, "/relations/duplicates/marked/delete-all", url.Values{}), http.StatusOK)
			awaitJobsDrain(t, srv)
			st := srv.jobs.Get()
			return st.Summary + st.Message
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			_, dup := seedMarkedPair(t, srv, "pair", 10)
			dupPath := canonicalPathOf(t, srv, dup)
			writeOverWithMore(t, dupPath)

			wantIn(t, tc.run(t, srv, dup), "kept 1 file changed since indexing")
			if _, err := os.Stat(dupPath); err != nil {
				t.Errorf("the file written over since was removed: %v", err)
			}
		})
	}
}
