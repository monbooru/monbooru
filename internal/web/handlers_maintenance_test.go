package web

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/models"
)

func TestRebuildThumbsJob_KeepsRefusalsOutOfTheTally(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "good.png", 10, 10)

	var full bytes.Buffer
	if err := png.Encode(&full, image.NewRGBA(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	cx := srv.active()
	undecodable := filepath.Join(cx.GalleryPath, "broken.png")

	mustWriteFile(t, undecodable, full.Bytes()[:33], 0o644)
	if _, _, err := gallery.Ingest(cx.Handle, undecodable, ""); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if err := srv.startRebuildThumbsJob(cx); err != nil {
		t.Fatalf("startRebuildThumbsJob: %v", err)
	}
	drainJob(t, srv)

	got := srv.jobs.Get().Summary
	if !strings.HasSuffix(got, "rebuilt 1 thumbnail, 1 failed.") {
		t.Errorf("summary = %q, want the refused row counted as failed", got)
	}
}

func TestRebuildThumbsJob_FillsAStillsMissingSize(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	src := filepath.Join(cx.GalleryPath, "pic.avif")
	generateStill(t, src, 64, 48)
	img, _, err := gallery.Ingest(cx.Handle, src, "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	mustExec(t, cx.DB.Write, `UPDATE images SET width = NULL, height = NULL WHERE id = ?`, img.ID)

	if err := srv.startRebuildThumbsJob(cx); err != nil {
		t.Fatalf("startRebuildThumbsJob: %v", err)
	}
	drainJob(t, srv)

	var w, h int
	if err := cx.DB.Read.QueryRow(`SELECT width, height FROM images WHERE id = ?`, img.ID).Scan(&w, &h); err != nil {
		t.Fatal(err)
	}
	if w != 64 || h != 48 {
		t.Errorf("size = %dx%d, want 64x48", w, h)
	}
}

func sidewaysJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			img.Set(x, y, color.RGBA{0, 0, 255, 255})
			if x < 10 && y < 10 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	exif := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	app1 := append([]byte{0xff, 0xe1, 0, byte(len(exif) + 2)}, exif...)
	return slices.Concat(buf.Bytes()[:2], app1, buf.Bytes()[2:])
}

func unturnThumbnail(t *testing.T, database *db.DB, thumbnailsPath, src string, id int64) int64 {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, gallery.ThumbnailPath(thumbnailsPath, id), buf.Bytes(), 0o644)
	h, err := gallery.RecomputeAndStorePhash(context.Background(), database, id, thumbnailsPath)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func thumbnailSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func storedPhash(t *testing.T, database *db.DB, id int64) int64 {
	t.Helper()
	var h int64
	if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRebuildThumbsJob_RecomputesThePhash(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	src := filepath.Join(cx.GalleryPath, "phone.jpg")
	mustWriteFile(t, src, sidewaysJPEG(t), 0o644)
	img, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, src, "")
	if old := unturnThumbnail(t, cx.DB, cx.ThumbnailsPath, src, img.ID); old == *img.Phash {
		t.Fatal("the fixture hashes the same turned and unturned")
	}

	if err := srv.startRebuildThumbsJob(cx); err != nil {
		t.Fatalf("startRebuildThumbsJob: %v", err)
	}
	drainJob(t, srv)

	if got := storedPhash(t, cx.DB, img.ID); got != *img.Phash {
		t.Errorf("phash = %x after the rebuild, want the turned thumbnail's %x", got, *img.Phash)
	}
}

func TestStartWatchers_TurnsAnUpgradedLibrarysThumbnails(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data")
	g := config.Gallery{
		Name:           "default",
		GalleryPath:    filepath.Join(dir, "gallery"),
		DBPath:         filepath.Join(dataPath, "default", "monbooru.db"),
		ThumbnailsPath: filepath.Join(dataPath, "default", "thumbnails"),
	}
	mustMkdirAll(t, g.GalleryPath, 0o755)
	mustMkdirAll(t, filepath.Dir(g.DBPath), 0o755)
	mustWriteFile(t, g.DBPath, mustBootstrappedDB(t), 0o644)
	database, err := db.Open(g.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(g.GalleryPath, "phone.jpg")
	mustWriteFile(t, src, sidewaysJPEG(t), 0o644)
	img, _ := mustIngest(t, database, g.GalleryPath, g.ThumbnailsPath, src, "")
	if old := unturnThumbnail(t, database, g.ThumbnailsPath, src, img.ID); old == *img.Phash {
		t.Fatal("the fixture hashes the same turned and unturned")
	}
	mustExec(t, database.Write, `PRAGMA user_version = 16`)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Paths.DataPath = dataPath
		c.Galleries = []config.Gallery{g}
		c.DefaultGallery = g.Name
	})
	srv.StartWatchers()
	deadline := time.Now().Add(30 * time.Second)
	for st := srv.jobs.Get(); st == nil || st.Running; st = srv.jobs.Get() {
		if time.Now().After(deadline) {
			t.Fatal("no turning job finished within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := srv.jobs.Get().Summary; got != "Turned 1 thumbnail by its EXIF orientation." {
		t.Errorf("summary = %q", got)
	}
	if w, h := thumbnailSize(t, gallery.ThumbnailPath(g.ThumbnailsPath, img.ID)); w != 20 || h != 40 {
		t.Errorf("thumbnail %dx%d, want it turned to 20x40", w, h)
	}
	if got := storedPhash(t, srv.active().DB, img.ID); got != *img.Phash {
		t.Errorf("phash = %x, want a fresh ingest's %x", got, *img.Phash)
	}
}

func TestTagCategoryConflicts(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	tagSvc := srv.tagSvc()
	generalID := galleryio.LookupCategoryID(cx.DB, "general")
	characterID := galleryio.LookupCategoryID(cx.DB, "character")

	for _, tc := range []struct {
		name string
		cat  int64
	}{{"nintendo", generalID}, {"nintendo", characterID}, {"solo", generalID}} {
		if _, err := tagSvc.GetOrCreateTag(tc.name, tc.cat); err != nil {
			t.Fatalf("GetOrCreateTag(%q): %v", tc.name, err)
		}
	}

	form := url.Values{}
	w := postForm(t, srv, "/settings/maintenance/tag-conflicts", form)

	mustCode(t, w, http.StatusOK)
	body := w.Body.String()

	wantIn(t, body, "<strong>2</strong> tags share a name")
	wantIn(t, body, `href="/tags?conflicts=1"`)

	listW := do(srv, "GET", "/tags?conflicts=1")
	listBody := listW.Body.String()
	if got := strings.Count(listBody, `>nintendo</a>`); got != 2 {
		t.Errorf("conflicts filter should list both nintendo rows, got %d", got)
	}
	wantNotIn(t, listBody, `>solo</a>`)
}

func TestTagCategoryConflicts_None(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{}
	w := postForm(t, srv, "/settings/maintenance/tag-conflicts", form)

	mustCode(t, w, http.StatusOK)
	if body := w.Body.String(); !strings.Contains(body, "No tags share a name") {
		t.Errorf("expected the empty-state message; got %s", body)
	}
}

func TestComputeHashesJob_RunsBothPasses(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "a.png", 10, 10)
	cx := srv.active()
	mustExec(t, cx.DB.Write, `UPDATE images SET md5 = '', phash = NULL`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/maintenance/compute-hashes", nil)
	srv.computeHashesPost(rec, req)
	mustCode(t, rec, http.StatusOK)
	drainJob(t, srv)

	if got := srv.jobs.Get().Summary; !strings.Contains(got, "1 perceptual hash of") || !strings.Contains(got, "1 md5 digest of") {
		t.Errorf("summary = %q, want both tallies", got)
	}
	var sum string
	var phash *int64
	if err := cx.DB.Read.QueryRow(`SELECT md5, phash FROM images`).Scan(&sum, &phash); err != nil {
		t.Fatal(err)
	}
	if len(sum) != 32 {
		t.Errorf("md5 = %q, want a 32-hex digest", sum)
	}
	if phash == nil {
		t.Error("phash still NULL, want the first pass to have filled it")
	}
}

func TestComputeHashesJob_DropsCachedSearches(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "a.png", 10, 10)
	mustExec(t, srv.db().Write, `UPDATE images SET md5 = '', phash = NULL`)
	for _, q := range []string{"-md5:", "-phash:"} {
		if got := galleryResultCount(t, srv, q); got != "1" {
			t.Fatalf("%s = %s before the backfill, want 1", q, got)
		}
	}

	mustCode(t, postForm(t, srv, "/settings/maintenance/compute-hashes", url.Values{}), http.StatusOK)
	awaitJobsDrain(t, srv)

	for _, q := range []string{"-md5:", "-phash:"} {
		if got := galleryResultCount(t, srv, q); got != "0" {
			t.Errorf("%s = %s after the backfill, want 0", q, got)
		}
	}
}

func TestEmptyFoldersScan_ListsOnlyTheEmptyOnes(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	for _, d := range []string{"leftover", "holder/.stfolder"} {
		mustMkdirAll(t, filepath.Join(cx.GalleryPath, filepath.FromSlash(d)), 0o755)
	}
	seedImage(t, srv, "kept.png", 10, 10)

	w := postForm(t, srv, "/settings/maintenance/empty-folders", url.Values{})
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.Contains(body, `value="leftover"`) {
		t.Errorf("scan did not list the empty folder: %s", body)
	}
	if strings.Contains(body, "holder") {
		t.Errorf("a folder holding a dot-directory was listed: %s", body)
	}
}

func TestEmptyFoldersRemove_TakesOnlyWhatWasPosted(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	for _, d := range []string{"go", "stay"} {
		mustMkdirAll(t, filepath.Join(cx.GalleryPath, d), 0o755)
	}

	w := postForm(t, srv, "/settings/maintenance/empty-folders/remove", url.Values{"folder": {"go"}})
	mustCode(t, w, http.StatusOK)
	if got := w.Body.String(); !strings.Contains(got, "Removed 1 empty folder.") {
		t.Errorf("flash = %q, want the removed tally", got)
	}
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "go")); !os.IsNotExist(err) {
		t.Error("the posted folder is still there")
	}
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "stay")); err != nil {
		t.Errorf("an unposted folder was removed: %v", err)
	}
}

func TestEmptyFolders_OutcomeGoesToTheSectionFlash(t *testing.T) {
	srv := newTestServer(t)
	mustMkdirAll(t, filepath.Join(srv.active().GalleryPath, "listed"), 0o755)

	listed := postForm(t, srv, "/settings/maintenance/empty-folders", url.Values{}).Body.String()
	wantIn(t, listed, `<div id="flash-empty-folders" hx-swap-oob="true">`)
	wantIn(t, listed, `value="listed"`)
	wantNotIn(t, listed, `class="flash`)

	removed := postForm(t, srv, "/settings/maintenance/empty-folders/remove", url.Values{"folder": {"listed"}}).Body.String()
	wantIn(t, removed, `<div id="flash-empty-folders" hx-swap-oob="true"></div>`)
	wantIn(t, removed, "Removed 1 empty folder.")

	none := postForm(t, srv, "/settings/maintenance/empty-folders", url.Values{}).Body.String()
	wantIn(t, none, `<div id="flash-empty-folders" hx-swap-oob="true"></div>`)
	wantIn(t, none, "No empty folders.")
}

func TestEmptyFoldersRemove_CountsOneThatFilledUpAgain(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	mustMkdirAll(t, filepath.Join(cx.GalleryPath, "refilled"), 0o755)
	mustWriteFile(t, filepath.Join(cx.GalleryPath, "refilled", "landed.png"), []byte("x"), 0o644)

	w := postForm(t, srv, "/settings/maintenance/empty-folders/remove", url.Values{"folder": {"refilled"}})
	mustCode(t, w, http.StatusOK)
	if got := w.Body.String(); !strings.Contains(got, "1 no longer empty") {
		t.Errorf("flash = %q, want the skipped folder counted", got)
	}
}

func TestIndexWorkflowsPost_DropsCachedSearches(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "wf.png", 10, 10)
	mustExec(t, srv.db().Write, `INSERT INTO comfyui_metadata (image_id, raw_workflow) VALUES (?, ?)`, id, `{"1": {"class_type": "KSampler", "inputs": {}}}`)
	index := func() {
		t.Helper()
		mustCode(t, postForm(t, srv, "/settings/maintenance/index-workflows", url.Values{}), http.StatusOK)
		awaitJobsDrain(t, srv)
	}
	index()
	if got := galleryResultCount(t, srv, "comfyui:KSampler"); got != "1" {
		t.Fatalf("comfyui:KSampler = %s, want 1", got)
	}
	mustExec(t, srv.db().Write, `UPDATE comfyui_metadata SET raw_workflow = ? WHERE image_id = ?`, `{"1": {"class_type": "Other", "inputs": {}}}`, id)

	index()

	if got := galleryResultCount(t, srv, "comfyui:KSampler"); got != "0" {
		t.Errorf("comfyui:KSampler = %s after the re-index, want 0", got)
	}
}

func TestUnusedTags_DeletesWhatNoImageCarriesAndKeepsTheCuratedOnRequest(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	g := cx.GeneralCategoryID
	svc := srv.tagSvc()
	used := mustTag(t, svc, "unused_probe_used", g)
	onMissing := mustTag(t, svc, "unused_probe_on_missing", g)
	plain := mustTag(t, svc, "unused_probe_plain", g)
	canon := mustTag(t, svc, "unused_probe_canon", g)
	parent := mustTag(t, svc, "unused_probe_parent", g)
	child := mustTag(t, svc, "unused_probe_child", g)
	noted := mustTag(t, svc, "unused_probe_noted", g)
	shown := seedImage(t, srv, "unused_shown.png", 8, 8)
	gone := seedImage(t, srv, "unused_gone.png", 9, 9)
	mustAddTag(t, svc, shown, used.ID, false, nil)
	mustAddTag(t, svc, gone, onMissing.ID, false, nil)
	mustExec(t, srv.db().Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)
	mustCreateAlias(t, svc, "unused_probe_alias", g, canon.ID)
	mustAddImply(t, svc, parent.ID, child.ID)
	if err := svc.SetTagNote(noted.ID, "keep me", nil); err != nil {
		t.Fatal(err)
	}

	w := do(srv, "GET", "/settings/maintenance/unused-tags", [2]string{"HX-Request", "true"})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Delete 1 unused tag?")
	wantIn(t, w.Body.String(), "4 more unused tags carry aliases, implications or a note and are kept.")
	wantIn(t, w.Body.String(), `<p class="unused-tags-all dlg-title" hidden>Delete 5 unused tags?`)

	mustStartJob(t, srv.jobs, models.JobTypeTag)
	w = postForm(t, srv, "/settings/maintenance/unused-tags", url.Values{})
	mustCode(t, w, http.StatusConflict)
	wantIn(t, w.Body.String(), "A job is already running.")
	srv.jobs.Complete("")

	w = postForm(t, srv, "/settings/maintenance/unused-tags", url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Deleting 1 unused tag.")
	awaitJobsDrain(t, srv)
	if n := countRows(t, srv.db(), "tags WHERE id = ?", plain.ID); n != 0 {
		t.Errorf("the plain unused tag survived")
	}
	if n := countRows(t, srv.db(), "tags WHERE name LIKE 'unused_probe_%'"); n != 7 {
		t.Errorf("tags left after the plain run = %d, want 7 (the alias included)", n)
	}
	w = do(srv, "GET", "/settings/maintenance/unused-tags", [2]string{"HX-Request", "true"})
	wantIn(t, w.Body.String(), "Nothing to delete unless the kept tags go too.")
	wantIn(t, w.Body.String(), "Delete 4 unused tags?")
	wantIn(t, w.Body.String(), " 4 unused tags carry aliases")

	w = postForm(t, srv, "/settings/maintenance/unused-tags", url.Values{"include_kept": {"1"}})
	wantIn(t, w.Body.String(), "Deleting 4 unused tags.")
	awaitJobsDrain(t, srv)
	if n := countRows(t, srv.db(), "tags WHERE name LIKE 'unused_probe_%'"); n != 2 {
		t.Errorf("tags left = %d, want the used one and the one on a missing image", n)
	}
	if n := countRows(t, srv.db(), "tags WHERE category_id = ?", categoryIDByName(t, srv, "rating")); n != 4 {
		t.Errorf("rating tags = %d, want the four kept", n)
	}

	w = postForm(t, srv, "/settings/maintenance/unused-tags", url.Values{})
	wantIn(t, w.Body.String(), "No unused tags to delete.")
}

func TestMaintenanceDialogs_RedirectAPlainGETToTheMaintenanceSection(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/settings/maintenance/apply-destination", "/settings/maintenance/unused-tags", "/settings/maintenance/reset-tags"} {
		w := do(srv, "GET", path)
		mustCode(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != "/settings#maintenance" {
			t.Errorf("GET %s redirects to %q, want /settings#maintenance", path, loc)
		}
	}
}

func TestResetTags_TakesTheTypedGalleryNameAndKeepsOnlyTheRatings(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	svc := srv.tagSvc()
	rating := categoryIDByName(t, srv, "rating")
	character := categoryIDByName(t, srv, "character")
	rapi := mustTag(t, svc, "rapi", character)
	android := mustTag(t, svc, "android", cx.GeneralCategoryID)
	explicit, err := svc.FindTag("explicit", rating)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateAlias(t, svc, "nsfw", cx.GeneralCategoryID, explicit.ID)
	mustCreateAlias(t, svc, "rapi_alt", character, rapi.ID)
	mustAddImply(t, svc, rapi.ID, android.ID)
	img := seedImage(t, srv, "reset.png", 8, 8)
	mustAddTag(t, svc, img, rapi.ID, false, nil)
	mustAddTag(t, svc, img, explicit.ID, false, nil)
	mustExec(t, srv.db().Write, `INSERT INTO tags (name, category_id) VALUES ('stray', ?)`, rating)

	plain := countRows(t, srv.db(), "tags WHERE is_alias = 0 AND category_id != ?", rating) + 1
	w := do(srv, "GET", "/settings/maintenance/reset-tags", [2]string{"HX-Request", "true"})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), fmt.Sprintf("Deletes %d tags, 2 aliases and 1 implication", plain))
	wantIn(t, w.Body.String(), "Type <strong>"+cx.Name+"</strong> to confirm")

	w = postForm(t, srv, "/settings/maintenance/reset-tags", url.Values{"confirm_name": {cx.Name + "x"}})
	wantIn(t, w.Body.String(), "type-to-confirm name does not match")
	if n := countRows(t, srv.db(), "tags WHERE id = ?", rapi.ID); n != 1 {
		t.Fatalf("a mistyped name deleted tags")
	}

	w = postForm(t, srv, "/settings/maintenance/reset-tags", url.Values{"confirm_name": {cx.Name}})
	wantIn(t, w.Body.String(), fmt.Sprintf("Resetting the tags: deleting %d tags.", plain))
	awaitJobsDrain(t, srv)
	if st := srv.jobs.Get(); st == nil || !strings.Contains(st.Summary, fmt.Sprintf("deleted %d tags", plain)) {
		t.Errorf("job = %+v, want a summary counting the %d tags the flash named", st, plain)
	}
	if n := countRows(t, srv.db(), "tags WHERE name = 'stray'"); n != 0 {
		t.Errorf("a non-canonical rating-category tag survived the reset")
	}
	if n := countRows(t, srv.db(), "tags"); n != 4 {
		t.Errorf("tags left = %d, want the four ratings", n)
	}
	if n := countRows(t, srv.db(), "tags WHERE category_id = ? AND is_alias = 0", rating); n != 4 {
		t.Errorf("rating tags = %d, want 4", n)
	}
	if n := countRows(t, srv.db(), "tag_implications"); n != 0 {
		t.Errorf("implications left = %d", n)
	}
	if n := countRows(t, srv.db(), "image_tags WHERE image_id = ?", img); n != 1 {
		t.Errorf("image rows left = %d, want the rating alone", n)
	}
	if n := countRows(t, srv.db(), "tag_categories WHERE id = ?", character); n != 1 {
		t.Errorf("the reset dropped a category")
	}
}
