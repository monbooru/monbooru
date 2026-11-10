package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func seedOrphanedImplied(t *testing.T, srv *Server) (imageID int64, parent, implied string) {
	t.Helper()
	parent, implied = "cloud", "sky"
	imageID = seedImage(t, srv, "orphan.png", 10, 10)

	svc := srv.tagSvc()
	parentTag := mustTag(t, svc, parent, srv.active().GeneralCategoryID)
	impliedTag := mustTag(t, svc, implied, srv.active().GeneralCategoryID)
	mustAddImply(t, svc, parentTag.ID, impliedTag.ID)
	mustAddTag(t, svc, imageID, parentTag.ID, false, nil)

	mustExec(t, srv.db().Write, `DELETE FROM tag_implications`)

	if _, err := svc.RemoveTagFromImage(imageID, parentTag.ID); err != nil {
		t.Fatal(err)
	}
	return imageID, parent, implied
}

func TestTagSidebarListsAnOrphanedImpliedRow(t *testing.T) {
	srv := newTestServer(t)
	imageID, _, implied := seedOrphanedImplied(t, srv)

	for _, mode := range []string{tagModeCategory, tagModeSource} {
		sb := sidebarOf(t, srv, imageID, mode, impliedNested)
		var found *tagSidebarRow
		for _, section := range sb.Sections {
			for i, row := range section.Rows {
				if row.TagName == implied {
					found = &section.Rows[i]
				}
			}
		}
		if found == nil {
			t.Fatalf("%s view: the orphaned implied row is not listed at all", mode)
		}
		if !found.Removable() {
			t.Errorf("%s view: the orphaned implied row carries no remove button", mode)
		}
	}
}

func TestEveryTagRowLinksToItsTagPage(t *testing.T) {
	srv := newTestServer(t)
	svc := srv.tagSvc()
	imageID := seedImage(t, srv, "wiki.png", 10, 10)
	parent := mustTag(t, svc, "wiki_parent", srv.active().GeneralCategoryID)
	implied := mustTag(t, svc, "wiki_implied", srv.active().GeneralCategoryID)
	mustAddImply(t, svc, parent.ID, implied.ID)
	mustAddTag(t, svc, imageID, parent.ID, false, nil)

	id := strconv.FormatInt(imageID, 10)
	for _, page := range []string{"/images/" + id, "/internal/sidebar?ids=" + id} {
		body := do(srv, "GET", page).Body.String()
		for _, tagID := range []int64{parent.ID, implied.ID} {
			wantIn(t, body, `<a href="/tags/`+strconv.FormatInt(tagID, 10)+`" class="tag-wiki"`)
		}
	}
}

func TestDetailPageRendersAnOrphanedImpliedRow(t *testing.T) {
	srv := newTestServer(t)
	imageID, _, implied := seedOrphanedImplied(t, srv)

	for _, mode := range []string{tagModeCategory, tagModeSource} {
		req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(imageID, 10), nil)
		req.AddCookie(&http.Cookie{Name: tagModeCookieName, Value: mode})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s view: status = %d", mode, w.Code)
		}
		if !strings.Contains(w.Body.String(), `data-name="`+implied+`"`) {
			t.Errorf("%s view: %q is missing from the page", mode, implied)
		}
	}
}

func TestRemoveTagFromImage_NothingRemovedStaysSilent(t *testing.T) {
	srv := newTestServer(t)
	imageID := seedImage(t, srv, "silent.png", 10, 10)
	absent := mustTag(t, srv.tagSvc(), "not_here", srv.active().GeneralCategoryID)

	id := strconv.FormatInt(imageID, 10)
	for _, path := range []string{
		"/images/" + id + "/tags/" + strconv.FormatInt(absent.ID, 10),
		"/images/" + id + "/tags/987654",
		"/images/" + id + "/user-tags",
	} {
		w := deleteCSRF(t, srv, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if got := w.Header().Get("HX-Trigger"); got != "" {
			t.Errorf("%s: HX-Trigger = %q, want none", path, got)
		}
		if strings.Contains(w.Body.String(), "removed") {
			t.Errorf("%s: flashes a removal that took nothing", path)
		}
	}
}

func seedSharedImplied(t *testing.T, srv *Server, androidCat int64) int64 {
	t.Helper()
	svc := srv.tagSvc()
	character := categoryIDByName(t, srv, "character")
	anis := mustTag(t, svc, "anis", character)
	rapi := mustTag(t, svc, "rapi", character)
	android := mustTag(t, svc, "android", androidCat)
	machine := mustTag(t, svc, "machine", srv.active().GeneralCategoryID)
	mustAddImply(t, svc, anis.ID, android.ID)
	mustAddImply(t, svc, rapi.ID, android.ID)
	mustAddImply(t, svc, android.ID, machine.ID)
	imageID := seedImage(t, srv, "shared.png", 10, 10)
	mustAddTag(t, svc, imageID, anis.ID, false, nil)
	mustAddTag(t, svc, imageID, rapi.ID, false, nil)
	return imageID
}

func sidebarOf(t *testing.T, srv *Server, imageID int64, mode, implied string) tagSidebar {
	t.Helper()
	_, imageTags, err := srv.tagSvc().GetImageTags(imageID)
	if err != nil {
		t.Fatal(err)
	}
	return srv.buildTagSidebar(imageID, mode, implied, imageTags)
}

func TestTagSidebarNestsASharedImpliedTagUnderEveryParent(t *testing.T) {
	srv := newTestServer(t)
	imageID := seedSharedImplied(t, srv, srv.active().GeneralCategoryID)
	var got []string
	for _, section := range sidebarOf(t, srv, imageID, tagModeCategory, impliedNested).Sections {
		if section.Name != "character" {
			continue
		}
		for _, row := range section.Rows {
			got = append(got, strings.Repeat(">", row.Depth)+row.TagName)
		}
	}
	want := "anis >android >>machine rapi >android >>machine"
	if strings.Join(got, " ") != want {
		t.Errorf("character section = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestTagSidebarBySourceNestsASharedImpliedTagUnderEveryParent(t *testing.T) {
	srv := newTestServer(t)
	imageID := seedSharedImplied(t, srv, srv.active().GeneralCategoryID)
	var got []string
	for _, section := range sidebarOf(t, srv, imageID, tagModeSource, impliedNested).Sections {
		if section.Name != "user" {
			continue
		}
		for _, row := range section.Rows {
			got = append(got, strings.Repeat(">", row.Depth)+row.TagName)
		}
	}
	want := "anis >android >>machine rapi >android >>machine"
	if strings.Join(got, " ") != want {
		t.Errorf("user section = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestTagSidebarOwnCategoryListsImpliedTagsWithTheirParents(t *testing.T) {
	srv := newTestServer(t)
	imageID := seedSharedImplied(t, srv, categoryIDByName(t, srv, "species"))
	sb := sidebarOf(t, srv, imageID, tagModeCategory, impliedOwn)
	if !sb.HasImplied {
		t.Error("HasImplied = false, want the switch offered")
	}
	got := map[string]string{}
	for _, section := range sb.Sections {
		for _, row := range section.Rows {
			if row.Depth != 0 {
				t.Errorf("%s nests at depth %d in its own category", row.TagName, row.Depth)
			}
			got[section.Name+":"+row.TagName] = row.ImpliedBy
		}
		if section.Name == "species" && section.DeleteURL != "" {
			t.Error("a section of implied rows offers a category remove")
		}
	}
	for key, want := range map[string]string{"character:anis": "", "character:rapi": "", "species:android": "anis, rapi", "general:machine": "android"} {
		if by, ok := got[key]; !ok || by != want {
			t.Errorf("%s implied by %q (listed %v), want %q", key, by, ok, want)
		}
	}

	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(imageID, 10)+"/tags?implied=own", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Header().Get("Set-Cookie"), impliedModeCookieName+"=own")
	wantIn(t, w.Body.String(), `<span class="tag-mode-current">[own category]</span>`)
	wantIn(t, w.Body.String(), `title="implied by anis, rapi">← anis, rapi</span>`)
}
