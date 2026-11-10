package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSettingsSaveRacesNoRender(t *testing.T) {
	srv := newTestServer(t)

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 40 {
			body := "page_size=40&thumbnail_fit=square&max_file_size_mb=100&watch_enabled=on&default_upload_folder="
			if i%2 == 0 {
				body = "page_size=12&thumbnail_fit=natural&max_file_size_mb=50&default_upload_folder=inbox"
			}
			r := httptest.NewRequest("POST", "/settings/general", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			srv.settingsGeneralPost(httptest.NewRecorder(), r)
		}
	})
	wg.Go(func() {
		for range 40 {
			srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}
	})
	wg.Go(func() {
		for range 40 {
			srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/settings", nil))
		}
	})
	wg.Wait()
	if got := srv.cfgSnapshot().UI.PageSize; got != 12 && got != 40 {
		t.Errorf("page_size = %d, want one of the two written values", got)
	}
}

func TestAFailedConfigSaveUndoesTheEdit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	var cfgDir string
	srv := newTestServerAt(t, false, nil, func(dir string) string {
		cfgDir = filepath.Join(dir, "cfg")
		mustMkdirAll(t, cfgDir, 0o755)
		return filepath.Join(cfgDir, "monbooru.toml")
	})
	addRemovableGallery(t, srv, "b", filepath.Join(t.TempDir(), "b"))
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/settings/schedule", url.Values{"time": {"03:33"}, "mode": {"at_time"}, "sync_gallery": {"on"}}},
		{"/settings/general", url.Values{"page_size": {"12"}, "thumbnail_fit": {"natural"}, "rename_on_ingest": {"on"}}},
		{"/settings/general/ignore", url.Values{"ignore": {"skipped/"}}},
		{"/settings/tagger/wd14/enable", url.Values{}},
		{"/settings/plugins/theme", url.Values{"theme": {"dark"}}},
		{"/settings/auth/tokens", url.Values{"name": {"fresh"}}},
		{"/settings/galleries/b/default", url.Values{}},
		{"/settings/relations", url.Values{"default_distance": {"6"}, "default_session_order": {"random"}}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			before := srv.cfgSnapshot()
			w := postHTMXForm(t, srv, tc.path, tc.form)
			wantIn(t, w.Body.String()+w.Header().Get("HX-Trigger"), "ermission denied")
			mustCode(t, w, http.StatusOK)
			if after := srv.cfgSnapshot(); !reflect.DeepEqual(after, before) {
				t.Errorf("the edit stayed live after the failed save")
			}
		})
	}
}

func TestConfigWritesGoThroughWithConfig(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "withConfig" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Lock" {
					return true
				}
				if mu, ok := sel.X.(*ast.SelectorExpr); ok && mu.Sel.Name == "cfgMu" {
					t.Errorf("%s: %s takes cfgMu's write lock: edit the config through withConfig", fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}
