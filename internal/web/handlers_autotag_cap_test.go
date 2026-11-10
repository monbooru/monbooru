//go:build tagger

package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
)

func stageDummyTagger(t *testing.T, srv *Server) {
	t.Helper()
	modelRoot := filepath.Join(t.TempDir(), "models")
	installTagger(t, modelRoot, "wd14", "tag_id,name,category\n0,sample,0\n")

	lib := filepath.Join(modelRoot, "libonnxruntime.so")
	mustWriteFile(t, lib, nil, 0o644)
	t.Setenv("ORT_LIB_PATH", lib)
	srv.cfg.Paths.ModelPath = modelRoot
}

func bulkSeedVisibleImages(t *testing.T, srv *Server, n int) {
	t.Helper()
	_, err := srv.db().Write.Exec(`
		WITH RECURSIVE seq(n) AS (
			SELECT 1
			UNION ALL
			SELECT n + 1 FROM seq WHERE n < ?
		)
		INSERT INTO images (sha256, canonical_path, file_type, file_size)
		SELECT printf('cap-%08d', n), printf('/cap/%08d.png', n), 'png', 1024
		FROM seq`, n)
	if err != nil {
		t.Fatalf("bulk seed %d images: %v", n, err)
	}
}

func TestAutotagTrigger_SearchScopeHasNoCap(t *testing.T) {
	srv := newTestServer(t)
	enableDummyTagger(t, srv)
	bulkSeedVisibleImages(t, srv, 50001)

	form := url.Values{"scope": {"search"}, "q": {""}}
	w := postHTMXForm(t, srv, "/internal/autotag", form)
	mustCode(t, w, http.StatusAccepted)
	wantIn(t, w.Header().Get("HX-Trigger"), "Auto-tagger started for 50001 images.")
	srv.jobs.Cancel()
	awaitJobsDrain(t, srv)
}
