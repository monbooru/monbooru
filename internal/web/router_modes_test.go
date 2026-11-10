package web

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
)

var wantWriteRoutes = []string{
	"GET /i/{sha}",
	"POST /internal/gallery/switch",
	"POST /settings/galleries",
	"POST /settings/galleries/{name}/default",
	"POST /settings/galleries/{name}/delete",
	"POST /settings/galleries/{name}/import",
	"POST /settings/galleries/{name}/rename",
	"POST /setup",
}

var wantFreeRoutes = []string{
	"GET /manifest.json",
	"GET /settings/galleries/{name}/export",
	"GET /static/",
	"GET /static/light/",
	"GET /static/shared/",
	"GET /theme.css",
	"GET /theme.favicon",
	"GET /theme.logo",
	"GET /theme.mascot/{face}",
	"GET /thumbnails/{gallery}/{file}",

	"DELETE /settings/auth/tokens/{id}",
	"GET /favicon.ico",
	"GET /health",
	"GET /internal/browse",
	"GET /internal/desktop-power",
	"GET /login",
	"GET /settings/auth/tokens/{id}/privileges",
	"POST /internal/open-folder",
	"POST /login",
	"POST /logout",
	"POST /settings/auth/password",
	"POST /settings/auth/remove-password",
	"POST /settings/auth/tokens",
	"POST /settings/auth/tokens/{id}/privileges",
	"POST /settings/desktop",
	"POST /settings/monloader",
	"POST /settings/quit",
	"POST /settings/restart",
	"POST /settings/schedule",
	"POST /settings/schedule/run",
	"POST /settings/tagger",
	"POST /settings/tagger/{name}/config",
	"POST /settings/tagger/{name}/delete",
	"POST /settings/tagger/{name}/disable",
	"POST /settings/tagger/{name}/enable",
	"POST /settings/tagger/{name}/reset",
	"GET /api/v1/pair/status",
	"GET /internal/plugins/pairing",
	"POST /api/v1/pair/remove",
	"POST /api/v1/pair/request",

	"GET " + pluginMountPrefix + "{name}/",
	"POST " + pluginMountPrefix + "{name}/",
	"GET /images/{id}/ptr-contrib-dialog",
	"GET /images/{id}/ptr-contrib-panel",
	"GET /internal/monloader-status",
	"GET /tags/{id}/ptr-contrib-dialog",
	"GET /tags/{id}/ptr-contrib-panel",
	"GET /tags/{id}/rules",
	"GET /tags/{id}/ptr-lookup-preview",
	"GET /tags/{id}/ptr-lookup-search",
	"POST /images/{id}/lookup",
	"POST /images/{id}/ptr-contrib",
	"POST /images/{id}/replace",
	"POST /images/{id}/sources/fetch",
	"POST /internal/batch-download",
	"POST /internal/monloader/disconnect",
	"POST /internal/monloader/reconnect",
	"POST /internal/plugin/relay",
	"POST /upload",
	"POST /api/v1/images",
	"POST /api/v1/images/{id}/file",
	"POST /settings/plugins/pair/{id}/approve",
	"POST /settings/plugins/pair/{id}/deny",
	"POST /settings/plugins/theme",
	"POST /settings/plugins/{name}/pause",
	"POST /settings/plugins/{name}/remove",
	"POST /settings/plugins/{name}/start",
	"POST /settings/plugins/{name}/stop",
	"POST /tags/{id}/ptr-contrib",
}

func TestRouteModes(t *testing.T) {
	srv := newTestServer(t)
	rt := srv.newRoutes()
	srv.registerRoutes(rt)

	got := map[routeMode][]string{}
	for pattern, mode := range rt.modes {
		got[mode] = append(got[mode], pattern)
	}
	for _, c := range []struct {
		mode routeMode
		name string
		want []string
	}{
		{modeWrite, "gallery-write", wantWriteRoutes},
		{modeFree, "gallery-free", wantFreeRoutes},
	} {
		have, want := slices.Sorted(slices.Values(got[c.mode])), slices.Sorted(slices.Values(c.want))
		if !slices.Equal(have, want) {
			t.Errorf("%s routes:\n got %q\nwant %q", c.name, have, want)
		}
	}
	if len(got[modeRead]) == 0 {
		t.Error("no route registered gallery-read")
	}
}

func TestGalleryFreeRoutesAnswerWhileASwitchWaits(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.sessions.CSRFToken("anon")
	for _, tc := range []struct {
		method, path, body string
	}{
		{"GET", "/health", ""},
		{"POST", "/settings/schedule", "time=02:00&mode=at_time"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			srv.set.RLock()
			locked := make(chan struct{})
			go func() {
				srv.set.Lock()
				close(locked)
				srv.set.Unlock()
			}()

			time.Sleep(50 * time.Millisecond)
			answered := make(chan int, 1)
			go func() {
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("X-CSRF-Token", csrf)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				answered <- w.Code
			}()
			select {
			case code := <-answered:
				if code != http.StatusOK {
					t.Errorf("%s = %d, want 200", tc.path, code)
				}
			case <-time.After(time.Second):
				t.Errorf("%s waited behind the pending switch", tc.path)
			}
			srv.set.RUnlock()
			<-locked
		})
	}
}

func TestGalleryExportPinsTheGalleryInsteadOfTheLock(t *testing.T) {
	srv := newMultiGalleryServer(t)
	upload := mustExport(t, srv, "default", "json").String()
	sw := &stalledWriter{header: http.Header{}, started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Handler().ServeHTTP(sw, httptest.NewRequest("GET", "/settings/galleries/stock/export?format=json", nil))
	}()
	select {
	case <-sw.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the export never started streaming")
	}

	if !srv.set.TryLock() {
		close(sw.release)
		<-done
		t.Fatal("the export holds the gallery lock while it streams")
	}
	srv.set.Unlock()
	for _, tc := range []struct {
		what string
		call func() error
	}{
		{"remove", func() error { _, err := srv.removeGallery("stock", false); return err }},
		{"rename", func() error { return srv.renameGallery("stock", "other") }},
		{"repoint", func() error { return srv.repointGallery("stock", t.TempDir()) }},
		{"import over", func() error { _, err := srv.importGallery("stock", "json", strings.NewReader(upload)); return err }},
	} {
		if err := tc.call(); err == nil || !strings.Contains(err.Error(), "is being exported") {
			t.Errorf("%s mid-export = %v, want the export named", tc.what, err)
		}
	}

	close(sw.release)
	<-done
	if _, err := srv.removeGallery("stock", false); err != nil {
		t.Errorf("remove once the export ended: %v", err)
	}
}

func TestWritesFromAPageOfAnotherGalleryAreRefused(t *testing.T) {
	srv := newMultiGalleryServer(t)
	rt := srv.newRoutes()
	srv.registerRoutes(rt)
	checkedFree := []string{
		"POST /images/{id}/lookup", "POST /images/{id}/replace", "POST /images/{id}/sources/fetch",
		"POST /internal/batch-download", "POST /internal/plugin/relay",
	}
	wildcard := regexp.MustCompile(`\{[^}]*\}`)
	for pattern, mode := range rt.modes {
		method, path, _ := strings.Cut(pattern, " ")
		if method == http.MethodGet || strings.HasPrefix(path, "/api/") {
			continue
		}
		if mode != modeRead && !slices.Contains(checkedFree, pattern) {
			continue
		}
		body := url.Values{"url": {"http://example.com/post/1"}}.Encode()
		req := httptest.NewRequest(method, wildcard.ReplaceAllString(path, "1"), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
		req.Header.Set("X-Monbooru-Gallery", "stock")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusConflict || w.Header().Get("X-Monbooru-Gallery") != "default" {
			t.Errorf("%s from a page of gallery stock: %d %q", pattern, w.Code, w.Body.String())
		}
	}
}

func TestUploadsReadTheirBodyOutsideTheGalleryLock(t *testing.T) {
	srv := newTestServer(t)
	setTokens(srv, []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash("bearer-1"), Scopes: config.AllScopes}})
	for _, tc := range []struct{ path, field string }{{"/upload", "files"}, {"/api/v1/images", "file"}} {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		req := httptest.NewRequest("POST", tc.path, pr)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
		req.Header.Set("Authorization", "Bearer bearer-1")
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		}()
		part, err := mw.CreateFormFile(tc.field, "slow.png")
		if err != nil {
			t.Fatal(err)
		}

		if _, err := part.Write([]byte("the first bytes")); err != nil {
			t.Fatal(err)
		}
		if srv.set.TryLock() {
			srv.set.Unlock()
		} else {
			t.Errorf("%s holds the gallery lock while its body streams", tc.path)
		}
		_ = mw.Close()
		_ = pw.Close()
		<-done
	}
}
