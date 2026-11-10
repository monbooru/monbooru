package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestFullPageRoutes_RenderTheNotFoundPage(t *testing.T) {
	srv := newTestServer(t)
	for _, target := range []string{
		"/tags/99999",
		"/tags/abc",
		"/images/99999/relations",
		"/relations/browse?kind=nosuchkind",
		"/setup",
		"/plugins/nosuchplugin/",
	} {
		w := do(srv, "GET", target)
		wantCode(t, w, http.StatusNotFound)
		if !strings.Contains(w.Body.String(), `id="nav"`) {
			t.Errorf("%s: expected the full layout, got: %s", target, snippet(w.Body.String(), "404"))
		}
	}
}
