package web

import (
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/jobs"
)

func TestScheduleTableShowsOnceThereIsAChoice(t *testing.T) {
	srv := newTestServer(t)
	settings := func() string {
		t.Helper()
		w := do(srv, "GET", "/settings")
		mustCode(t, w, http.StatusOK)
		return w.Body.String()
	}
	if strings.Contains(settings(), `name="scope_action"`) {
		t.Error("a lone gallery with no list drew the table")
	}

	srv.cfgMu.Lock()
	srv.cfg.Schedule.Galleries = map[string][]string{config.ActionSyncGallery: {}}
	srv.cfgMu.Unlock()
	wantIn(t, settings(), `name="every_sync_gallery"`)

	srv.cfgMu.Lock()
	srv.cfg.Schedule.Galleries = nil
	srv.cfgMu.Unlock()
	addRemovableGallery(t, srv, "photos", filepath.Join(t.TempDir(), "photos"))
	body := settings()
	wantIn(t, body, `aria-label="Sync gallery: photos"`)

	wantIn(t, body, `title="no enabled auto-tagger runs on this gallery"`)
	if strings.Contains(body, `name="every_lookup_ptr"`) {
		t.Error("the lookup columns drew while monloader is unpaired")
	}

	pairStubMonloader(t, srv, (&ptrStubHandler{}).handle)
	wantIn(t, settings(), `name="every_lookup_ptr"`)
}

func TestScheduleSaveReadsTheGalleryTable(t *testing.T) {
	srv := newTestServer(t)
	addRemovableGallery(t, srv, "photos", filepath.Join(t.TempDir(), "photos"))
	srv.cfgMu.Lock()
	srv.cfg.Schedule.Galleries = map[string][]string{config.ActionLookupPTR: {"photos"}, config.ActionSyncGallery: {"photos"}}
	srv.cfgMu.Unlock()

	mustCode(t, postForm(t, srv, "/settings/schedule", url.Values{
		"time":                   {"02:00"},
		"sync_gallery":           {"on"},
		"scope_action":           {"sync_gallery", "remove_orphans", "run_auto_taggers", "find_relation_pairs", "vacuum"},
		"every_sync_gallery":     {"on"},
		"on_sync_gallery":        {"photos"},
		"on_remove_orphans":      {"photos", "ghost", "photos"},
		"every_run_auto_taggers": {"on"},
		"on_vacuum":              {"default"},
	}), http.StatusOK)
	want := map[string][]string{
		config.ActionRemoveOrphans:     {"photos"},
		config.ActionFindRelationPairs: {},
		config.ActionLookupPTR:         {"photos"},
	}
	got := srv.cfgSnapshot().Schedule
	if !maps.EqualFunc(got.Galleries, want, slices.Equal) {
		t.Errorf("galleries = %#v, want %#v", got.Galleries, want)
	}
	if got.Time != "02:00" || !got.SyncGallery {
		t.Errorf("schedule = %+v, want the switches and time saved beside the table", got)
	}

	saved, err := config.LoadWithDefaults(srv.configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(saved.Schedule.Galleries, want, slices.Equal) {
		t.Errorf("saved galleries = %#v, want %#v", saved.Schedule.Galleries, want)
	}

	mustCode(t, postForm(t, srv, "/settings/schedule", url.Values{"time": {"02:00"}}), http.StatusOK)
	if got := srv.cfgSnapshot().Schedule.Galleries; !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("a save without the table changed the lists to %#v", got)
	}
}

func TestScheduleSaveKeepsTheLookupsItDidNotDraw(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule.LookupPTR, srv.cfg.Schedule.LookupBooru = true, true
	srv.cfgMu.Unlock()

	mustCode(t, postForm(t, srv, "/settings/schedule", url.Values{"time": {"03:00"}}), http.StatusOK)
	if got := srv.cfgSnapshot().Schedule; !got.LookupPTR || !got.LookupBooru {
		t.Errorf("a save without the switches turned them off: %+v", got)
	}

	mustCode(t, postForm(t, srv, "/settings/schedule", url.Values{
		"time": {"03:00"}, "lookup_switches": {"1"}, "lookup_booru": {"on"},
	}), http.StatusOK)
	if got := srv.cfgSnapshot().Schedule; got.LookupPTR || !got.LookupBooru {
		t.Errorf("a save with the switches drawn: ptr=%v booru=%v, want false true", got.LookupPTR, got.LookupBooru)
	}
}

func TestGeneralSaveKeepsAnEnvPinnedKey(t *testing.T) {
	srv, path := newEnvPinnedServer(t, "MONBOORU_GALLERY_MAX_FILE_SIZE_MB", "300", func(file *config.Config) {
		file.Gallery.MaxFileSizeMB = 100
	})

	page := do(srv, "GET", "/settings").Body.String()
	wantIn(t, page, `value=this.value" disabled>`)
	wantIn(t, page, "Set by MONBOORU_GALLERY_MAX_FILE_SIZE_MB in the environment.")

	w := postForm(t, srv, "/settings/general", url.Values{"page_size": {"25"}})
	mustCode(t, w, http.StatusOK)
	if got := srv.cfgSnapshot().Gallery.MaxFileSizeMB; got != 300 {
		t.Errorf("running max_file_size_mb = %d, want the environment's 300", got)
	}
	t.Setenv("MONBOORU_GALLERY_MAX_FILE_SIZE_MB", "")
	saved, err := config.LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Gallery.MaxFileSizeMB != 100 || saved.UI.PageSize != 25 {
		t.Errorf("file holds max_file_size_mb=%d page_size=%d, want 100 and 25", saved.Gallery.MaxFileSizeMB, saved.UI.PageSize)
	}
}

func TestPluginsRepoURL_SitsBesideTheRepository(t *testing.T) {
	for repo, want := range map[string]string{
		"https://github.com/monbooru/monbooru":  "https://github.com/monbooru/monbooru-plugins",
		"https://github.com/monbooru/monbooru/": "https://github.com/monbooru/monbooru-plugins",
	} {
		if got := pluginsRepoURL(repo); got != want {
			t.Errorf("pluginsRepoURL(%q) = %q, want %q", repo, got, want)
		}
	}
}

func newEnvPinnedServer(t *testing.T, env, value string, edit func(*config.Config)) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	file := config.Default()
	file.Paths.DataPath = filepath.Join(dir, "data")
	file.Galleries[0].GalleryPath = filepath.Join(dir, "gallery")
	mustMkdirAll(t, file.Galleries[0].GalleryPath, 0o755)
	file.Gallery.WatchEnabled = false
	file.Server.BaseURL = "http://example.com"
	edit(file)
	if err := config.Save(file, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, value)
	cfg, err := config.LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, path, jobs.NewManager(), Desktop{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, path
}
