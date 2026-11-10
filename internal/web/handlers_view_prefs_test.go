package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPageSize_OnlyHonoursOfferedSizes(t *testing.T) {
	srv := newTestServer(t)

	for _, tc := range []struct {
		cookie string
		want   int
	}{
		{"20", 20},
		{"500", 500},
		{"999", srv.cfg.UI.PageSize},
		{"", srv.cfg.UI.PageSize},
		{"not-a-number", srv.cfg.UI.PageSize},
		{"-20", srv.cfg.UI.PageSize},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if tc.cookie != "" {
			req.AddCookie(&http.Cookie{Name: pageSizeCookieName, Value: tc.cookie})
		}
		if got := srv.pageSize(req); got != tc.want {
			t.Errorf("cookie %q: page size = %d, want %d", tc.cookie, got, tc.want)
		}
	}
}

func TestViewPrefsPost_RefusesAnUnofferedSize(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/internal/view-prefs", url.Values{"page_size": {"60"}})
	mustCode(t, w, http.StatusNoContent)
	if c := w.Header().Get("Set-Cookie"); !strings.Contains(c, pageSizeCookieName+"=60") {
		t.Errorf("accepted size: Set-Cookie = %q", c)
	}

	w = postForm(t, srv, "/internal/view-prefs", url.Values{"page_size": {"999"}})
	mustCode(t, w, http.StatusBadRequest)
	if c := w.Header().Get("Set-Cookie"); c != "" {
		t.Errorf("refused size still set a cookie: %q", c)
	}
}

func TestThumbSize_FallsBackToTheMiddleStep(t *testing.T) {
	for _, tc := range []struct{ cookie, want string }{
		{"s", "s"}, {"l", "l"}, {"m", "m"}, {"", "m"}, {"xl", "m"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if tc.cookie != "" {
			req.AddCookie(&http.Cookie{Name: thumbSizeCookieName, Value: tc.cookie})
		}
		if got := thumbSize(req); got != tc.want {
			t.Errorf("cookie %q: thumb size = %q, want %q", tc.cookie, got, tc.want)
		}
	}
}

func TestViewPrefsPost_RefusesAnUnknownThumbSize(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/internal/view-prefs", url.Values{"thumb": {"l"}})
	mustCode(t, w, http.StatusNoContent)
	if c := w.Header().Get("Set-Cookie"); !strings.Contains(c, thumbSizeCookieName+"=l") {
		t.Errorf("accepted step: Set-Cookie = %q", c)
	}

	w = postForm(t, srv, "/internal/view-prefs", url.Values{"thumb": {"xl"}})
	mustCode(t, w, http.StatusBadRequest)
}

func TestPageSizeChoices_DefaultSitsAmongTheSizesInOrder(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 24

	plain := httptest.NewRequest("GET", "/", nil)
	got := srv.pageSizeChoices(plain)
	if len(got) != len(PageSizeOptions)+1 {
		t.Fatalf("choices = %v, want the offered set plus a default row", got)
	}
	if got[1].Value != "default" || got[1].Label != "24" || got[1].Title != "Default, set in Settings" || !got[1].Selected {
		t.Errorf("second button = %+v, want the configured default, selected, between 20 and 40", got[1])
	}

	override := httptest.NewRequest("GET", "/", nil)
	override.AddCookie(&http.Cookie{Name: pageSizeCookieName, Value: "100"})
	got = srv.pageSizeChoices(override)
	if got[1].Selected {
		t.Error("default row still selected under an override")
	}
	if !slices.ContainsFunc(got, func(c PageSizeChoice) bool { return c.Value == "100" && c.Selected }) {
		t.Errorf("choices = %v, want 100 selected", got)
	}
}

func TestPageSizeChoices_DefaultStandsInForAnEqualSize(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 40

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: pageSizeCookieName, Value: "40"})
	got := srv.pageSizeChoices(r)
	if len(got) != len(PageSizeOptions) {
		t.Fatalf("choices = %v, want 40 offered once", got)
	}
	if got[1].Value != "default" || got[1].Label != "40" || !got[1].Selected {
		t.Errorf("second button = %+v, want the default in 40's place, selected by the equal override", got[1])
	}
}

func TestViewPrefsPost_DefaultClearsTheOverride(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/internal/view-prefs", url.Values{"page_size": {"default"}})
	mustCode(t, w, http.StatusNoContent)
	c := w.Header().Get("Set-Cookie")
	if !strings.Contains(c, pageSizeCookieName+"=;") || !strings.Contains(c, "Max-Age=0") {
		t.Errorf("Set-Cookie = %q, want the override expired", c)
	}
}

func TestPrefCookieDeletesStayHttpOnlyAndLax(t *testing.T) {
	for name, write := range map[string]func(http.ResponseWriter, *http.Request){
		ratingCeilingCookieName: func(w http.ResponseWriter, r *http.Request) { writeRatingCookie(w, r, "") },
		tagModeCookieName:       func(w http.ResponseWriter, r *http.Request) { tagModePref.write(w, r, "") },
		impliedModeCookieName:   func(w http.ResponseWriter, r *http.Request) { impliedModePref.write(w, r, "") },
	} {
		w := httptest.NewRecorder()
		write(w, httptest.NewRequest("POST", "/", nil))
		c := w.Header().Get("Set-Cookie")
		for _, want := range []string{name + "=;", "Max-Age=0", "HttpOnly", "SameSite=Lax"} {
			if !strings.Contains(c, want) {
				t.Errorf("Set-Cookie = %q, want %q", c, want)
			}
		}
		wantNotIn(t, c, "Secure")

		w = httptest.NewRecorder()
		write(w, httptest.NewRequest("POST", "https://monbooru.example/", nil))
		wantIn(t, w.Header().Get("Set-Cookie"), "Secure")
	}
}

func TestTagSidebarPrefs_QueryOverridesAndStoresTheCookie(t *testing.T) {
	srv := newTestServer(t)
	svc := srv.tagSvc()
	imageID := seedImage(t, srv, "prefs.png", 10, 10)
	parent := mustTag(t, svc, "pref_parent", srv.active().GeneralCategoryID)
	implied := mustTag(t, svc, "pref_implied", srv.active().GeneralCategoryID)
	mustAddImply(t, svc, parent.ID, implied.ID)
	mustAddTag(t, svc, imageID, parent.ID, false, nil)
	id := strconv.FormatInt(imageID, 10)

	for _, tc := range []struct {
		name, cookie, value, query, current, setCookie string
	}{
		{"tag mode default", "", "", "", "[category]", ""},
		{"tag mode cookie", tagModeCookieName, "source", "", "[source]", ""},
		{"tag mode unknown cookie", tagModeCookieName, "bogus", "", "[category]", ""},
		{"tag mode query", "", "", "tagmode=source", "[source]", tagModeCookieName + "=source"},
		{"tag mode query back to default", tagModeCookieName, "source", "tagmode=category", "[category]", tagModeCookieName + "=;"},
		{"tag mode unknown query", tagModeCookieName, "source", "tagmode=bogus", "[category]", tagModeCookieName + "=;"},
		{"implied default", "", "", "", "[nested]", ""},
		{"implied cookie", impliedModeCookieName, "own", "", "[own category]", ""},
		{"implied unknown cookie", impliedModeCookieName, "bogus", "", "[nested]", ""},
		{"implied query", "", "", "implied=own", "[own category]", impliedModeCookieName + "=own"},
		{"implied query back to default", impliedModeCookieName, "own", "implied=nested", "[nested]", impliedModeCookieName + "=;"},
		{"implied unknown query", impliedModeCookieName, "own", "implied=bogus", "[nested]", impliedModeCookieName + "=;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/images/"+id+"/tags?"+tc.query, nil)
			req.Header.Set("HX-Request", "true")
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: tc.cookie, Value: tc.value})
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			mustCode(t, w, http.StatusOK)
			wantIn(t, w.Body.String(), `<span class="tag-mode-current">`+tc.current+`</span>`)
			cookies := strings.Join(w.Header().Values("Set-Cookie"), "\n")
			if tc.setCookie == "" {
				wantNotIn(t, cookies, tagModeCookieName)
				wantNotIn(t, cookies, impliedModeCookieName)
			} else {
				wantIn(t, cookies, tc.setCookie)
			}
		})
	}

	for _, tc := range []struct{ cookie, value, current string }{
		{tagModeCookieName, "source", "[source]"},
		{impliedModeCookieName, "own", "[own category]"},
	} {
		req := httptest.NewRequest("GET", "/images/"+id, nil)
		req.AddCookie(&http.Cookie{Name: tc.cookie, Value: tc.value})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		mustCode(t, w, http.StatusOK)
		wantIn(t, w.Body.String(), `<span class="tag-mode-current">`+tc.current+`</span>`)
	}
}

func TestViewPrefCookieReads(t *testing.T) {
	read := func(name, value string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if name != "" {
			r.AddCookie(&http.Cookie{Name: name, Value: value})
		}
		return r
	}
	for _, tc := range []struct {
		name, cookie, value string
		got                 func(*http.Request) any
		want                any
	}{
		{"ceiling absent", "", "", func(r *http.Request) any { return readRatingCookie(r) }, ""},
		{"ceiling set", ratingCeilingCookieName, "sensitive", func(r *http.Request) any { return readRatingCookie(r) }, "sensitive"},
		{"ceiling unknown", ratingCeilingCookieName, "bogus", func(r *http.Request) any { return readRatingCookie(r) }, ""},
		{"sidebar absent", "", "", func(r *http.Request) any { return sidebarCollapsed(r) }, false},
		{"sidebar collapsed", sidebarCookieName, "collapsed", func(r *http.Request) any { return sidebarCollapsed(r) }, true},
		{"sidebar other", sidebarCookieName, "open", func(r *http.Request) any { return sidebarCollapsed(r) }, false},
		{"thumbs absent", "", "", func(r *http.Request) any { return thumbSize(r) }, "m"},
		{"thumbs large", thumbSizeCookieName, "l", func(r *http.Request) any { return thumbSize(r) }, "l"},
		{"thumbs unknown", thumbSizeCookieName, "xl", func(r *http.Request) any { return thumbSize(r) }, "m"},
		{"page size absent", "", "", func(r *http.Request) any { return pageSizeOverride(r) }, 0},
		{"page size offered", pageSizeCookieName, "100", func(r *http.Request) any { return pageSizeOverride(r) }, 100},
		{"page size not offered", pageSizeCookieName, "7", func(r *http.Request) any { return pageSizeOverride(r) }, 0},
		{"page size empty", pageSizeCookieName, "", func(r *http.Request) any { return pageSizeOverride(r) }, 0},
	} {
		if got := tc.got(read(tc.cookie, tc.value)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
