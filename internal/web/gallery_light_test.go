package web

import (
	"bytes"
	"image/color"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
)

func TestImportGalleryLightZip_WipesAndRebuilds(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.get("stock")

	for i, name := range []string{"a.png", "b.png"} {
		bytesPng := makePNGBytes(t, 16, 16, byte(i*30), byte(i*30+10), byte(i*30+20))
		p := filepath.Join(cx.GalleryPath, name)
		mustWriteFile(t, p, bytesPng, 0o644)
		img, _, err := galleryIngest(srv, "stock", p)
		if err != nil {
			t.Fatal(err)
		}
		tag := mustTag(t, cx.TagSvc, "orig_tag", 1)
		mustAddTag(t, cx.TagSvc, img.ID, tag.ID, false, nil)
	}

	exportBuf := mustExport(t, srv, "stock", "light")

	unpinStock(t, srv)
	strayPath := filepath.Join(cx.GalleryPath, "stray.txt")
	mustWriteFile(t, strayPath, []byte("should be wiped"), 0o644)

	w := postReplaceImport(t, srv, "stock", "stock-light.zip", exportBuf.Bytes())

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-ok")

	if _, err := os.Stat(strayPath); !os.IsNotExist(err) {
		t.Errorf("stray.txt should have been wiped, stat err: %v", err)
	}

	cx2 := srv.get("stock")
	n := countRows(t, cx2.DB, "images")
	if n != 2 {
		t.Errorf("after light import: %d images, want 2", n)
	}
	if orig := countRows(t, cx2.DB, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE t.name = 'orig_tag'"); orig != 2 {
		t.Errorf("orig_tag rows back = %d, want 2", orig)
	}
}

func TestImportLightJSON_ReplaceRebuildsFromOnDiskFiles(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.get("stock")

	keepBytes := makePNGBytes(t, 16, 16, 11, 22, 33)
	keepPath := filepath.Join(cx.GalleryPath, "keep.png")
	mustWriteFile(t, keepPath, keepBytes, 0o644)
	keepImg, _, err := galleryIngest(srv, "stock", keepPath)
	if err != nil {
		t.Fatal(err)
	}
	tag := mustTag(t, cx.TagSvc, "kept_tag", 1)
	mustAddTag(t, cx.TagSvc, keepImg.ID, tag.ID, false, nil)

	manifest := galleryio.LightManifest{
		Version: galleryio.LightManifestVersion,
		Images: []galleryio.LightManifestImage{
			{SHA256: keepImg.SHA256, Path: "keep.png", Tags: []string{"kept_tag"}},
			{SHA256: "deadbeef", Path: "missing/from/disk.png", Tags: []string{"phantom_tag"}},
		},
	}
	manifestJSON := mustMarshal(t, manifest)

	unpinStock(t, srv)

	w := postReplaceImport(t, srv, "stock", "stock-light.json", manifestJSON)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-ok")

	cx2 := srv.get("stock")
	n := countRows(t, cx2.DB, "images")
	if n != 2 {
		t.Errorf("after light-json replace: %d images, want 2 (kept + missing)", n)
	}
	missing := countRows(t, cx2.DB, "images WHERE is_missing = 1")
	if missing != 1 {
		t.Errorf("expected 1 row flagged is_missing=1, got %d", missing)
	}
	tagNames, err := db.QueryStrings(cx2.DB.Read, `
		SELECT t.name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		ORDER BY t.name`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"kept_tag": false, "phantom_tag": false}
	for _, n := range tagNames {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("expected tag %q after light-json replace, got %v", k, tagNames)
		}
	}
}

func TestMergeGalleryExcludesTheGalleryMutations(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedMergeTarget(t, srv, "stock")

	manifest := galleryio.LightManifest{Version: galleryio.LightManifestVersion}
	body := mustMarshal(t, manifest)
	gate := &gateReader{r: bytes.NewReader(body), opened: make(chan struct{}), wait: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, mergeErr := srv.mergeGallery("stock", "json", gate)
		done <- mergeErr
	}()

	<-gate.opened
	renameErr := srv.renameGallery("stock", "renamed")
	close(gate.wait)
	if err := <-done; err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	if renameErr == nil {
		t.Error("the rename went through while the merge was writing")
	}
}

func newServerWithGalleryUnderData(t *testing.T, name string) (*Server, string) {
	t.Helper()
	var folder string
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		folder = filepath.Join(cfg.Paths.DataPath, name)
		mustMkdirAll(t, folder, 0o755)
		cfg.Galleries = append(cfg.Galleries, config.Gallery{
			Name:           name,
			GalleryPath:    folder,
			DBPath:         filepath.Join(folder, "monbooru.db"),
			ThumbnailsPath: filepath.Join(folder, "thumbnails"),
		})
	})
	return srv, folder
}

func unpinStock(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.setDefault("stock"); err != nil {
		t.Fatal(err)
	}
	if err := srv.switchGallery("default"); err != nil {
		t.Fatal(err)
	}
	if err := srv.setDefault("default"); err != nil {
		t.Fatal(err)
	}
}

func postReplaceImport(t *testing.T, srv *Server, gallery, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.sessions.CSRFToken("anon"),
		"mode":         "replace",
		"confirm_name": gallery,
	}, "file", filename, data)
	return postMultipart(t, srv, "/settings/galleries/"+gallery+"/import", body, ct)
}

func TestRepointGalleryKeepsTheGalleryWhenTheReopenFails(t *testing.T) {
	srv := newTestServerWith(t, false, nil)
	cx := srv.active()
	elsewhere := t.TempDir()

	if err := os.RemoveAll(cx.ThumbnailsPath); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, cx.ThumbnailsPath, []byte("not a directory"), 0o644)

	if err := srv.repointGallery(srv.defaultGallery(), elsewhere); err == nil {
		t.Fatal("RepointGallery should have refused the reopen")
	}
	after := srv.active()
	if after == nil {
		t.Fatal("the gallery is gone from the context map")
	}
	if err := after.DB.Read.Ping(); err != nil {
		t.Errorf("the gallery is left on closed handles: %v", err)
	}
	if after.GalleryPath == elsewhere {
		t.Error("the refused path was adopted anyway")
	}
}

func TestRenameGalleryRestoresTheGalleryWhenTheMoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Gallery.WatchEnabled = true })
	name := srv.defaultGallery()
	before := srv.get(name)
	if before == nil {
		t.Fatal("no gallery to rename")
	}
	dataRoot := filepath.Dir(filepath.Dir(before.DBPath))

	if err := os.Chmod(dataRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataRoot, 0o755) })

	if err := srv.renameGallery(name, "renamed"); err == nil {
		t.Fatal("RenameGallery should have refused the move")
	}
	after := srv.get(name)
	if after == nil {
		t.Fatal("the gallery is gone from the context map")
	}
	if err := after.DB.Read.Ping(); err != nil {
		t.Errorf("the gallery is left on closed handles: %v", err)
	}
	if !after.WatcherRunning() {
		t.Error("the gallery came back without its watcher")
	}
	if !after.ReclaimerRunning() {
		t.Error("the gallery came back without its manga reclaimer")
	}
}

func TestRenameGalleryIsUndoneWhenTheConfigCannotBeSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	var cfgDir string
	srv := newTestServerAt(t, false, func(c *config.Config) { c.Gallery.WatchEnabled = true }, func(dir string) string {
		cfgDir = filepath.Join(dir, "cfg")
		mustMkdirAll(t, cfgDir, 0o755)
		return filepath.Join(cfgDir, "monbooru.toml")
	})
	name := srv.defaultGallery()
	oldDB := srv.get(name).DBPath
	mustExec(t, srv.db().Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at) VALUES ('kept-sha', 'kept.png', 'png', 10, datetime('now'))`)
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	err := srv.renameGallery(name, "renamed")

	if err == nil || !strings.Contains(err.Error(), "persist gallery rename") {
		t.Fatalf("err = %v, want the failed save", err)
	}
	if srv.get("renamed") != nil {
		t.Error("the new name went live without a config naming it")
	}
	cx := srv.get(name)
	if cx == nil {
		t.Fatal("the gallery is gone under its old name")
	}
	if cx.DBPath != oldDB || srv.activeGallery() != name {
		t.Errorf("db %q active %q, want %q and %q", cx.DBPath, srv.activeGallery(), oldDB, name)
	}
	if n := countRows(t, cx.DB, "images WHERE sha256 = 'kept-sha'"); n != 1 {
		t.Error("the gallery reopened without its rows")
	}
	if !cx.WatcherRunning() {
		t.Error("the gallery came back without its watcher")
	}
	srv.cfgMu.RLock()
	cfgName, def := srv.cfg.Galleries[0].Name, srv.cfg.DefaultGallery
	srv.cfgMu.RUnlock()
	if cfgName != name || def != name {
		t.Errorf("config names %q with default %q, want %q", cfgName, def, name)
	}
}

func TestRemoveGallery_AFailedConfigSaveKeepsTheGalleryAndItsData(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	var cfgDir string
	srv := newTestServerAt(t, false, nil, func(dir string) string {
		cfgDir = filepath.Join(dir, "cfg")
		mustMkdirAll(t, cfgDir, 0o755)
		return filepath.Join(cfgDir, "monbooru.toml")
	})
	galleryPath := filepath.Join(t.TempDir(), "doomed")
	addRemovableGallery(t, srv, "doomed", galleryPath)
	image := filepath.Join(galleryPath, "held.png")
	mustWriteFile(t, image, tinyPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 0o644)
	dbPath := srv.get("doomed").DBPath
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	_, err := srv.removeGallery("doomed", true)

	if err == nil || !strings.Contains(err.Error(), "persist gallery removal") {
		t.Fatalf("err = %v, want the failed save", err)
	}
	srv.cfgMu.RLock()
	inConfig := srv.cfg.FindGallery("doomed") != nil
	srv.cfgMu.RUnlock()
	if !slices.Contains(srv.set.Names(), "doomed") || !inConfig {
		t.Fatal("the gallery left the set or the config")
	}
	for _, p := range []string{dbPath, image} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s went with a removal the config never recorded: %v", p, err)
		}
	}
	if err := srv.get("doomed").DB.Read.Ping(); err != nil {
		t.Errorf("the kept gallery was closed: %v", err)
	}
}

func TestAPIResolver_TreatsAClosedGalleryAsAbsent(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "other", filepath.Join(t.TempDir(), "other"))
	if _, ok := srv.apiResolver("other"); !ok {
		t.Fatal("an open gallery did not resolve")
	}
	srv.get("other").Close()
	if _, ok := srv.apiResolver("other"); ok {
		t.Error("a closed gallery resolved, so the request would run on its closed database")
	}
}

func TestAClosedGalleryIsAbsentToItsReaders(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "other", filepath.Join(t.TempDir(), "other"))
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", SyncGallery: true}
	srv.cfgMu.Unlock()
	srv.get("other").Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/settings/galleries/other/export?format=db")
	if err != nil {
		t.Errorf("the export broke off mid-stream: %v", err)
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("export of a closed gallery: status %d, want 404", resp.StatusCode)
		}
	}
	if slices.ContainsFunc(srv.allContexts(), func(cx *library.Gallery) bool { return cx.Name == "other" }) {
		t.Error("the every-gallery readers still see the closed gallery")
	}
	srv.sched.RunNow()
	if info := srv.sched.Status().LastInfo; strings.Contains(info, "other") {
		t.Errorf("the scheduled run worked on the closed gallery: %s", info)
	}
}

func addRemovableGallery(t *testing.T, srv *Server, name, galleryPath string) {
	t.Helper()
	mustMkdirAll(t, galleryPath, 0o755)
	if err := srv.addGallery(name, galleryPath); err != nil {
		t.Fatalf("AddGallery: %v", err)
	}
}

func TestRemoveGalleryTakesTheSourceFolderOnlyWhenAsked(t *testing.T) {
	srv := newTestServer(t)
	root := t.TempDir()

	for _, tc := range []struct {
		name         string
		removeFolder bool
	}{
		{"kept", false},
		{"wiped", true},
	} {
		galleryPath := filepath.Join(root, tc.name)
		addRemovableGallery(t, srv, tc.name, galleryPath)
		image := filepath.Join(galleryPath, "held.png")
		mustWriteFile(t, image, tinyPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 0o644)
		dataDir := filepath.Dir(srv.get(tc.name).DBPath)

		if _, err := srv.removeGallery(tc.name, tc.removeFolder); err != nil {
			t.Fatalf("RemoveGallery(%q, %v): %v", tc.name, tc.removeFolder, err)
		}

		if srv.get(tc.name) != nil {
			t.Errorf("%s: the gallery is still in the context map", tc.name)
		}
		if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
			t.Errorf("%s: data dir %q survived the removal", tc.name, dataDir)
		}
		_, err := os.Stat(image)
		if tc.removeFolder && !os.IsNotExist(err) {
			t.Errorf("%s: remove_folder left the source image behind", tc.name)
		}
		if !tc.removeFolder && err != nil {
			t.Errorf("%s: the source image went with a removal that did not ask for it: %v", tc.name, err)
		}
	}
}

func TestRemoveGalleryLeavesWhatItsDataDirHoldsForOthers(t *testing.T) {
	var dataPath string
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		dataPath = cfg.Paths.DataPath
		for _, g := range []struct{ name, folder string }{
			{"own", filepath.Join(dataPath, "own")},
			{"host", filepath.Join(filepath.Dir(dataPath), "host")},
			{"guest", filepath.Join(dataPath, "host", "pics")},
		} {
			mustMkdirAll(t, g.folder, 0o755)
			cfg.Galleries = append(cfg.Galleries, config.Gallery{
				Name:           g.name,
				GalleryPath:    g.folder,
				DBPath:         filepath.Join(dataPath, g.name, "monbooru.db"),
				ThumbnailsPath: filepath.Join(dataPath, g.name, "thumbnails"),
			})
		}
	})
	ownImage := filepath.Join(dataPath, "own", "held.png")
	guestImage := filepath.Join(dataPath, "host", "pics", "held.png")
	mustWriteFile(t, ownImage, tinyPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 0o644)
	mustWriteFile(t, guestImage, tinyPNG(t, 4, 4, color.RGBA{4, 5, 6, 255}), 0o644)

	for _, name := range []string{"own", "host"} {
		if _, err := srv.removeGallery(name, false); err != nil {
			t.Fatalf("RemoveGallery(%q): %v", name, err)
		}
	}
	for _, image := range []string{ownImage, guestImage} {
		if _, err := os.Stat(image); err != nil {
			t.Errorf("%s went with a removal that kept the folder: %v", image, err)
		}
	}
	for _, name := range []string{"own", "host"} {
		for _, own := range []string{"monbooru.db", "thumbnails"} {
			if _, err := os.Stat(filepath.Join(dataPath, name, own)); !os.IsNotExist(err) {
				t.Errorf("%s: %s outlived the removal (err=%v)", name, own, err)
			}
		}
	}
}

func TestRemoveGalleryRefusesToFollowASymlink(t *testing.T) {
	srv := newTestServer(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	mustMkdirAll(t, real, 0o755)
	bystander := filepath.Join(real, "bystander.png")
	mustWriteFile(t, bystander, tinyPNG(t, 4, 4, color.RGBA{4, 5, 6, 255}), 0o644)
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if err := srv.addGallery("linked", link); err != nil {
		t.Fatalf("AddGallery: %v", err)
	}

	if _, err := srv.removeGallery("linked", true); err != nil {
		t.Fatalf("RemoveGallery: %v", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("the symlink was followed and its target wiped: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) && err != nil {
		t.Errorf("lstat link: %v", err)
	}
}

func TestRenameGalleryMovesTheDataDirAndRewritesTheConfig(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "before", filepath.Join(t.TempDir(), "before"))
	oldDir := filepath.Dir(srv.get("before").DBPath)

	w := postForm(t, srv, "/settings/galleries/before/rename", url.Values{"new_name": {"after"}})
	mustCode(t, w, http.StatusOK)
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}

	if srv.get("before") != nil {
		t.Error("the old name still resolves")
	}
	renamed := srv.get("after")
	if renamed == nil {
		t.Fatal("the renamed gallery is gone from the context map")
	}
	if err := renamed.DB.Read.Ping(); err != nil {
		t.Errorf("the renamed gallery is left on closed handles: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("data dir %q was left behind under the old name", oldDir)
	}
	newDir := filepath.Dir(renamed.DBPath)
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("data dir %q is not at the new name: %v", newDir, err)
	}
	toml, err := os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toml), `name = "after"`) || strings.Contains(string(toml), `name = "before"`) {
		t.Error("the config on disk still carries the old name")
	}
}

func TestTaggerGalleriesFollowARenameAndARemoval(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "before", filepath.Join(t.TempDir(), "before"))
	srv.cfgMu.Lock()
	srv.cfg.Tagger.Taggers = []config.TaggerInstance{{Name: "wd", Enabled: true, Galleries: []string{"before"}}}
	srv.cfgMu.Unlock()
	saved := func() []string {
		t.Helper()
		cfg, err := config.LoadWithDefaults(srv.configPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Tagger.Taggers[0].Galleries
	}

	mustCode(t, postForm(t, srv, "/settings/galleries/before/rename", url.Values{"new_name": {"after"}}), http.StatusOK)
	if got := saved(); !slices.Equal(got, []string{"after"}) {
		t.Errorf("tagger galleries after the rename = %q, want [after]", got)
	}

	mustCode(t, postForm(t, srv, "/settings/galleries/after/delete", url.Values{"confirm_name": {"after"}}), http.StatusOK)
	if got := saved(); got == nil || len(got) != 0 {
		t.Errorf("tagger galleries after the removal = %#v, want an empty list", got)
	}
}

func TestGalleryRenameTellsThePairedMonloader(t *testing.T) {
	srv := newTestServer(t)
	renames := make(chan string, 2)
	var refuse atomic.Bool
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/galleries/rename" || refuse.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		renames <- string(body)
		_, _ = w.Write([]byte(`{"changed":1}`))
	})
	addRemovableGallery(t, srv, "anime", filepath.Join(t.TempDir(), "anime"))

	w := postForm(t, srv, "/settings/galleries/anime/rename", url.Values{"new_name": {"art"}})
	mustCode(t, w, http.StatusOK)
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true once monloader was told", w.Header().Get("HX-Refresh"))
	}
	select {
	case body := <-renames:
		wantIn(t, body, `"from":"anime"`)
		wantIn(t, body, `"to":"art"`)
	default:
		t.Fatal("monloader was not told about the rename")
	}

	refuse.Store(true)
	w = postForm(t, srv, "/settings/galleries/art/rename", url.Values{"new_name": {"pics"}})
	wantIn(t, w.Body.String(), "monloader could not be told, so its default gallery and site targets still name art")
	if srv.get("pics") == nil {
		t.Error("the rename did not stand once monloader refused the notice")
	}
}

func TestGalleryMutations_ReleaseTheJobLane(t *testing.T) {
	srv := newMultiGalleryServer(t)
	for _, tc := range []struct {
		what string
		call func() error
	}{
		{"switch to the active gallery", func() error { return srv.switchGallery("stock") }},
		{"switch to an unknown gallery", func() error { return srv.switchGallery("nope") }},
		{"add a name already taken", func() error { return srv.addGallery("stock", t.TempDir()) }},
		{"remove an unknown gallery", func() error { _, err := srv.removeGallery("nope", false); return err }},
		{"rename an unknown gallery", func() error { return srv.renameGallery("nope", "other") }},
		{"repoint an unknown gallery", func() error { return srv.repointGallery("nope", t.TempDir()) }},
	} {
		_ = tc.call()
		if srv.jobs.IsRunning() {
			t.Fatalf("%s left the job lane held", tc.what)
		}
	}
}

func TestANestedGalleryIsFencedOffTheOneHoldingIt(t *testing.T) {
	srv := newTestServer(t)
	inner := filepath.Join(srv.get("default").GalleryPath, ".H")
	addRemovableGallery(t, srv, "H", inner)
	probe := filepath.Join(inner, "a.png")
	if !srv.get("default").Boundary().Excludes(probe) {
		t.Fatal("the outer gallery still owns the nested gallery's folder")
	}
	if srv.get("H").Boundary().Excludes(probe) {
		t.Fatal("the nested gallery is fenced off its own folder")
	}

	if _, err := srv.removeGallery("H", false); err != nil {
		t.Fatalf("RemoveGallery: %v", err)
	}
	if srv.get("default").Boundary().Excludes(probe) {
		t.Error("the folder stayed fenced after its gallery went")
	}
}

func TestAGalleryHoldingTheDataFolderLeavesItOut(t *testing.T) {
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		cfg.Paths.DataPath = filepath.Join(cfg.Galleries[0].GalleryPath, ".monbooru")
		cfg.Galleries[0].DBPath = filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db")
		cfg.Galleries[0].ThumbnailsPath = filepath.Join(cfg.Paths.DataPath, "default", "thumbnails")
	})
	cx := srv.get("default")
	if !cx.Boundary().Excludes(filepath.Join(cx.ThumbnailsPath, "1.jpg")) {
		t.Error("the gallery indexes its own thumbnails")
	}
}

func TestAGalleryUnderTheDataFolderLeavesOnlyItsThumbnailsOut(t *testing.T) {
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		cfg.Galleries[0].GalleryPath = filepath.Join(cfg.Paths.DataPath, "default")
		mustMkdirAll(t, cfg.Galleries[0].GalleryPath, 0o755)
	})
	cx := srv.get("default")
	b := cx.Boundary()
	if b.Excludes(filepath.Join(cx.GalleryPath, "photo.png")) {
		t.Error("data_path fenced off the gallery mounted under it")
	}
	if !b.Excludes(filepath.Join(cx.ThumbnailsPath, "1.jpg")) {
		t.Error("the gallery indexes its own thumbnails")
	}
}

func TestRemoveGalleryWithItsFolderKeepsANestedGallery(t *testing.T) {
	srv := newTestServer(t)
	root := filepath.Join(t.TempDir(), "outer")
	addRemovableGallery(t, srv, "outer", root)
	inner := filepath.Join(root, ".H")
	addRemovableGallery(t, srv, "H", inner)
	own := filepath.Join(root, "own.png")
	theirs := filepath.Join(inner, "in.png")
	mustWriteFile(t, own, tinyPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 0o644)
	mustWriteFile(t, theirs, tinyPNG(t, 4, 4, color.RGBA{4, 5, 6, 255}), 0o644)

	if _, err := srv.removeGallery("outer", true); err != nil {
		t.Fatalf("RemoveGallery: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("the nested gallery's file went with the outer folder: %v", err)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("the gallery's own file stayed (err=%v)", err)
	}
}

func TestAddGalleryRefusesAFolderAnotherGalleryUses(t *testing.T) {
	srv := newTestServer(t)
	root := srv.get("default").GalleryPath
	if err := srv.addGallery("twin", root); err == nil || !strings.Contains(err.Error(), "is already the folder of gallery default") {
		t.Errorf("same folder = %v, want a refusal naming gallery default", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if err := srv.addGallery("linked", link); err == nil || !strings.Contains(err.Error(), "is already the folder of gallery default") {
		t.Errorf("through a link = %v, want a refusal naming gallery default", err)
	}
}

func TestAddGalleryWithImportDoesNotSpoolThroughTheTempDir(t *testing.T) {
	srv := newTestServer(t)
	mustExec(t, srv.db().Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at) VALUES ('seed-sha', 'seed.png', 'png', 10, datetime('now'))`)
	export := mustExport(t, srv, srv.activeGallery(), "json")

	export.Write(bytes.Repeat([]byte(" "), 33<<20))
	galleryPath := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	body, ct := buildMultipart(t, map[string]string{"name": "fresh", "gallery_path": galleryPath}, "import_file", "export.json", export.Bytes())
	w := postMultipart(t, srv, "/settings/galleries", body, ct)

	wantIn(t, w.Body.String(), "Gallery fresh added and imported.")
	if cx := srv.get("fresh"); cx == nil {
		t.Fatal("the gallery was not added")
	} else if n := countRows(t, cx.DB, "images"); n != 1 {
		t.Errorf("images = %d, want the imported 1", n)
	}
}

func postAddGallery(t *testing.T, srv *Server, name, galleryPath string) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mustWriteField(t, mw, "name", name)
	mustWriteField(t, mw, "gallery_path", galleryPath)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/settings/galleries", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Body.String()
}

func TestGalleryFoldersStayOutOfWhatMonbooruWritesTo(t *testing.T) {
	var dataPath, modelPath string
	srv := newTestServerWith(t, false, func(cfg *config.Config) {
		dataPath = cfg.Paths.DataPath
		modelPath = filepath.Join(dataPath, "models")
		cfg.Paths.ModelPath = modelPath
	})
	elsewhere := filepath.Join(filepath.Dir(dataPath), "elsewhere")
	for _, dir := range []string{
		filepath.Join(dataPath, "pics"), filepath.Join(dataPath, "default", "sub"),
		filepath.Join(modelPath, "sub"), filepath.Join(dataPath, "mounted"), elsewhere,
	} {
		mustMkdirAll(t, dir, 0o755)
	}

	for _, tc := range []struct{ name, folder string }{
		{"top", dataPath},
		{"pics", filepath.Join(dataPath, "pics")},
		{"inside", filepath.Join(dataPath, "default", "sub")},
		{"model", filepath.Join(modelPath, "sub")},
	} {
		if err := srv.addGallery(tc.name, tc.folder); err == nil {
			t.Errorf("add %s at %s was accepted", tc.name, tc.folder)
		}
	}
	if err := srv.repointGallery("default", filepath.Join(dataPath, "default", "sub")); err == nil {
		t.Error("a repoint into the gallery's own data folder was accepted")
	}

	if err := srv.addGallery("photos", filepath.Join(dataPath, "mounted")); err != nil {
		t.Fatalf("a gallery mounted under the data folder was refused: %v", err)
	}
	for _, name := range []string{"mounted", "models"} {
		if err := srv.addGallery(name, elsewhere); err == nil {
			t.Errorf("a gallery named %s was accepted, whose data folder holds another folder", name)
		}
	}
}

func TestRenameGalleryRefusesWhileAGalleryFolderSitsInItsDataDir(t *testing.T) {
	srv, folder := newServerWithGalleryUnderData(t, "own")
	image := filepath.Join(folder, "held.png")
	mustWriteFile(t, image, tinyPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 0o644)

	if err := srv.renameGallery("own", "renamed"); err == nil {
		t.Fatal("the rename was accepted")
	}
	if _, err := os.Stat(image); err != nil {
		t.Errorf("the gallery's image moved: %v", err)
	}
	if srv.get("own") == nil {
		t.Error("the gallery is gone after a refused rename")
	}
}

func TestAddGalleryNamesHowItNests(t *testing.T) {
	srv := newTestServer(t)
	root := srv.get("default").GalleryPath
	inner := filepath.Join(root, "sub", ".H")
	mustMkdirAll(t, inner, 0o755)
	p := filepath.Join(inner, "in.png")
	mustWriteFile(t, p, tinyPNG(t, 4, 4, color.RGBA{7, 8, 9, 255}), 0o644)
	if _, _, err := galleryIngest(srv, "default", p); err != nil {
		t.Fatal(err)
	}

	body := postAddGallery(t, srv, "H", inner)
	wantIn(t, body, "It sits inside gallery default, which leaves this folder to it from now on: default&#39;s next sync marks the 1 image it holds there missing.")

	body = postAddGallery(t, srv, "all", filepath.Dir(root))
	wantIn(t, body, "It leaves out the folder of gallery default.")
}

func TestGalleryRowsAndTheDeleteNameWhatAGalleryLeavesOut(t *testing.T) {
	srv := newTestServer(t)
	root := filepath.Join(t.TempDir(), "outer")
	addRemovableGallery(t, srv, "outer", root)
	addRemovableGallery(t, srv, "H", filepath.Join(root, ".H"))
	kept := filepath.Join(root, ".H", "kept.png")
	mustWriteFile(t, kept, tinyPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 0o644)

	var leaves []string
	for _, row := range srv.galleryRowsWithSnapshot(srv.activeGallery(), 0, 0) {
		if row.Name == "outer" {
			leaves = row.LeavesOut
		}
	}
	if want := []string{".H (gallery H)"}; !slices.Equal(leaves, want) {
		t.Errorf("outer leaves out %q, want %q", leaves, want)
	}

	w := postForm(t, srv, "/settings/galleries/outer/delete", url.Values{"confirm_name": {"outer"}, "remove_folder": {"on"}})
	wantIn(t, w.Body.String(), "Deleted outer. Kept what it leaves out: .H.")
	if w.Header().Get("HX-Refresh") != "" {
		t.Error("a delete that kept folders refreshed before saying so")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("gallery H's file went with outer: %v", err)
	}
}

func TestDeleteNamesAnotherGallerysFolderFirstAndOnlyAFewEntries(t *testing.T) {
	srv := newTestServer(t)
	root := filepath.Join(t.TempDir(), "outer")
	addRemovableGallery(t, srv, "outer", root)
	for _, sub := range []string{"a", "b", "c", "d", "e", "f"} {
		mustMkdirAll(t, filepath.Join(root, sub), 0o755)
		mustWriteFile(t, filepath.Join(root, sub, "._shot.png"), []byte("sidecar"), 0o644)
	}
	addRemovableGallery(t, srv, "zed", filepath.Join(root, "zed"))

	w := postForm(t, srv, "/settings/galleries/outer/delete", url.Values{"confirm_name": {"outer"}, "remove_folder": {"on"}})
	wantIn(t, w.Body.String(), "Deleted outer. Kept what it leaves out: zed, a/._shot.png, b/._shot.png, c/._shot.png, d/._shot.png (and 2 more).")
}
