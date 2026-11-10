package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestSidebarCollapseRendersFromCookie(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		name      string
		cookie    string
		collapsed bool
	}{
		{"no cookie", "", false},
		{"collapsed", "collapsed", true},
		{"unknown value", "hidden", false},
	}

	for _, path := range []string{"/", "/tags"} {
		for _, tc := range cases {
			req := httptest.NewRequest("GET", path, nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "monbooru_sidebar", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s (%s): expected 200, got %d", path, tc.name, w.Code)
			}
			got := strings.Contains(w.Body.String(), `<div id="main-layout" class="sidebar-collapsed">`)
			if got != tc.collapsed {
				t.Errorf("GET %s (%s): collapsed=%v, want %v", path, tc.name, got, tc.collapsed)
			}
		}
	}
}

func TestSidebarCollapsedSearchShipsPlaceholder(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	id := seedImage(t, srv, "one.png", 10, 10)
	tag := mustTag(t, cx.TagSvc, "sidebartag", cx.GeneralCategoryID)
	mustAddTag(t, cx.TagSvc, id, tag.ID, false, nil)
	cx.InvalidateCaches()

	gridSearch := func(collapsed bool) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("HX-Request", "true")
		req.Header.Set("HX-Target", "gallery-grid")
		if collapsed {
			req.AddCookie(&http.Cookie{Name: "monbooru_sidebar", Value: "collapsed"})
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("gallery search (collapsed=%v): expected 200, got %d", collapsed, w.Code)
		}
		return w.Body.String()
	}

	if body := gridSearch(false); !strings.Contains(body, "tag-list-sidebar") {
		t.Error("shown: the search response carried no sidebar content")
	}
	body := gridSearch(true)
	wantNotIn(t, body, "tag-list-sidebar")
	wantIn(t, body, `hx-trigger="sidebar-shown from:body"`)

	if !strings.Contains(body, `hx-get="/internal/sidebar?ids=`+strconv.FormatInt(id, 10)) {
		t.Errorf("collapsed: placeholder does not carry this page's ids; body slice: %s",
			sliceAround(body, "sidebar-inner", 260))
	}
}
