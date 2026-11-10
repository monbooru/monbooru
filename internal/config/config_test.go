package config

import (
	"bytes"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

const baseTOML = `
[[galleries]]
name = "default"
gallery_path = "/gallery"

[paths]
data_path = "/data"
`

func writeConfig(t *testing.T, toml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monbooru.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatalf("Load %s: %v", path, err)
	}
	return cfg
}

func TestLoadFromValidTOML(t *testing.T) {

	content := `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery"

[server]
bind_address = "0.0.0.0:9090"
base_url = "http://example.com"
monloader_url = "http://localhost:8081"

[monloader]
api_url = "http://monloader:8081"

[paths]
data_path  = "/my/data"
model_path = "/my/models"

[gallery]
watch_enabled    = false
max_file_size_mb = 100

[[tagger.taggers]]
name = "wd-swinv2"
enabled = true
confidence_threshold = 0.50

[auth]
enable_password       = true
password_hash         = "$2a$10$test"
session_lifetime_days = 14
api_token             = "mysecret"

[ui]
page_size = 60

[log]
level = "debug"
`
	path := writeConfig(t, content)

	cfg := mustLoad(t, path)

	if cfg.Server.BindAddress != "0.0.0.0:9090" {
		t.Errorf("BindAddress = %q", cfg.Server.BindAddress)
	}
	if cfg.Server.MonloaderURL != "http://localhost:8081" {
		t.Errorf("MonloaderURL = %q", cfg.Server.MonloaderURL)
	}
	if cfg.Monloader.APIURL != "http://monloader:8081" {
		t.Errorf("Monloader.APIURL = %q", cfg.Monloader.APIURL)
	}
	if len(cfg.Galleries) != 1 || cfg.Galleries[0].GalleryPath != "/my/gallery" {
		t.Errorf("Galleries = %+v", cfg.Galleries)
	}
	if cfg.Galleries[0].DBPath != "/my/data/default/monbooru.db" {
		t.Errorf("DBPath = %q", cfg.Galleries[0].DBPath)
	}
	if cfg.Galleries[0].ThumbnailsPath != "/my/data/default/thumbnails" {
		t.Errorf("ThumbnailsPath = %q", cfg.Galleries[0].ThumbnailsPath)
	}
	if cfg.Paths.DataPath != "/my/data" {
		t.Errorf("DataPath = %q", cfg.Paths.DataPath)
	}
	if cfg.Gallery.MaxFileSizeMB != 100 {
		t.Errorf("MaxFileSizeMB = %d", cfg.Gallery.MaxFileSizeMB)
	}
	if len(cfg.Tagger.Taggers) != 1 || cfg.Tagger.Taggers[0].ConfidenceThreshold != 0.50 {
		t.Errorf("Taggers = %+v", cfg.Tagger.Taggers)
	}
	if !cfg.Auth.EnablePassword {
		t.Errorf("EnablePassword should be true")
	}
	if cfg.UI.PageSize != 60 {
		t.Errorf("PageSize = %d", cfg.UI.PageSize)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q", cfg.Log.Level)
	}
}

func TestDefaultLogLevel(t *testing.T) {
	if got := Default().Log.Level; got != "warn" {
		t.Errorf("default Log.Level = %q, want %q", got, "warn")
	}
}

func TestMissingTOMLCreatesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	cfg := mustLoad(t, path)
	if cfg.Server.BindAddress != "127.0.0.1:8455" {
		t.Errorf("default BindAddress = %q", cfg.Server.BindAddress)
	}
	if cfg.UI.PageSize != 40 {
		t.Errorf("default PageSize = %d", cfg.UI.PageSize)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("default config file was not created")
	}
}

func TestInvalidBindAddress(t *testing.T) {
	path := writeConfig(t, `
[server]
bind_address = "notavalidaddress"
`)
	if _, err := LoadWithDefaults(path, nil); err == nil {
		t.Errorf("expected error for invalid bind address")
	}
}

func TestSessionLifetimeDaysClampsZero(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[auth]
session_lifetime_days = 0
`)
	cfg := mustLoad(t, path)
	if cfg.Auth.SessionLifetimeDays != 7 {
		t.Errorf("SessionLifetimeDays = %d, want 7 (clamped)", cfg.Auth.SessionLifetimeDays)
	}
}

func TestPageSizeClampsAboveMax(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[ui]
page_size = 50000
`)
	cfg := mustLoad(t, path)
	if cfg.UI.PageSize != MaxPageSize {
		t.Errorf("PageSize = %d, want %d (clamped)", cfg.UI.PageSize, MaxPageSize)
	}
}

func TestEnvOverrideSessionLifetimeRevalidates(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[auth]
session_lifetime_days = 30
`)

	t.Setenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", "0")
	cfg := mustLoad(t, path)
	if cfg.Auth.SessionLifetimeDays != 7 {
		t.Errorf("env=0 SessionLifetimeDays = %d, want 7 (clamped after override)", cfg.Auth.SessionLifetimeDays)
	}

	t.Setenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", "notanint")
	cfg = mustLoad(t, path)
	if cfg.Auth.SessionLifetimeDays != 30 {
		t.Errorf("unparseable env SessionLifetimeDays = %d, want 30 (kept)", cfg.Auth.SessionLifetimeDays)
	}
}

func TestThumbnailFitDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"absent defaults to natural", "", "natural"},
		{"bogus snaps to natural", `thumbnail_fit = "circle"`, "natural"},
		{"square preserved", `thumbnail_fit = "square"`, "square"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toml := "\n[[galleries]]\nname = \"default\"\ngallery_path = \"/gallery\"\n\n[paths]\ndata_path = \"/data\"\n\n[ui]\n" + c.line + "\n"
			path := writeConfig(t, toml)
			cfg := mustLoad(t, path)
			if cfg.UI.ThumbnailFit != c.want {
				t.Errorf("ThumbnailFit = %q, want %q", cfg.UI.ThumbnailFit, c.want)
			}
		})
	}
}

func TestPasswordHashRejectsNonBcrypt(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[auth]
enable_password = true
password_hash   = "not-a-bcrypt-hash"
`)
	if _, err := LoadWithDefaults(path, nil); err == nil || !strings.Contains(err.Error(), "bcrypt") {
		t.Errorf("expected bcrypt-shape error, got %v", err)
	}
}

func TestDefault_AllFields(t *testing.T) {
	cfg := Default()
	if cfg.Server.BindAddress == "" {
		t.Error("default BindAddress empty")
	}
	if cfg.Paths.DataPath == "" {
		t.Error("default DataPath empty")
	}
	if cfg.Gallery.MaxFileSizeMB <= 0 {
		t.Error("default MaxFileSizeMB <= 0")
	}
	if cfg.UI.PageSize <= 0 {
		t.Error("default PageSize <= 0")
	}
	if len(cfg.Galleries) != 1 || cfg.Galleries[0].Name != "default" {
		t.Errorf("default Galleries = %+v", cfg.Galleries)
	}
	if cfg.DefaultGallery != "default" {
		t.Errorf("default DefaultGallery = %q", cfg.DefaultGallery)
	}
}

func TestLoadMultipleGalleries(t *testing.T) {
	content := `
default_gallery = "stock"

[[galleries]]
name = "default"
gallery_path = "/gallery"

[[galleries]]
name = "stock"
gallery_path = "/gallery2"

[server]
bind_address = "127.0.0.1:8080"

[paths]
data_path  = "/data"
model_path = "/models"
`
	path := writeConfig(t, content)
	cfg := mustLoad(t, path)
	if len(cfg.Galleries) != 2 {
		t.Fatalf("Galleries = %+v", cfg.Galleries)
	}
	if cfg.DefaultGallery != "stock" {
		t.Errorf("DefaultGallery = %q", cfg.DefaultGallery)
	}
	stock := cfg.FindGallery("stock")
	if stock == nil || stock.DBPath != "/data/stock/monbooru.db" {
		t.Errorf("stock DBPath = %q", stock)
	}
}

func TestDefaultGalleryFallback(t *testing.T) {
	path := writeConfig(t, `
default_gallery = "missing"

[[galleries]]
name = "only"
gallery_path = "/gallery"
`)
	cfg := mustLoad(t, path)
	if cfg.DefaultGallery != "only" {
		t.Errorf("DefaultGallery should fall back to only gallery, got %q", cfg.DefaultGallery)
	}
}

func TestTaggerGalleriesNilVsEmptyRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		applies bool
	}{
		{name: "all-nil", input: nil, applies: true},
		{name: "none", input: []string{}, applies: false},
		{name: "named", input: []string{"default"}, applies: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "monbooru.toml")
			cfg := Default()
			cfg.Tagger.Taggers = []TaggerInstance{{Name: "wd-swinv2", Enabled: true, ConfidenceThreshold: 0.4, Galleries: c.input}}
			if err := Save(cfg, path); err != nil {
				t.Fatalf("Save: %v", err)
			}
			loaded := mustLoad(t, path)
			if len(loaded.Tagger.Taggers) != 1 {
				t.Fatalf("len(Taggers) = %d", len(loaded.Tagger.Taggers))
			}
			got := loaded.Tagger.Taggers[0].Galleries
			if (c.input == nil) != (got == nil) {
				t.Errorf("nil-ness drift: input nil=%t, got nil=%t (%v)", c.input == nil, got == nil, got)
			}
			if loaded.Tagger.Taggers[0].AppliesToGallery("default") != c.applies {
				t.Errorf("AppliesToGallery(default) = %t, want %t", !c.applies, c.applies)
			}
		})
	}
}

func TestGalleryRefsFollowARenameAndARemoval(t *testing.T) {
	cfg := Default()
	cfg.Tagger.Taggers = []TaggerInstance{
		{Name: "every"},
		{Name: "two", Galleries: []string{"anime", "stock"}},
		{Name: "one", Galleries: []string{"anime"}},
		{Name: "stale", Galleries: []string{"anime", "art"}},
	}
	cfg.Schedule.Galleries = map[string][]string{ActionSyncGallery: {"anime", "stock"}, ActionLookupPTR: {"anime"}}
	old, oldLists := cfg.Tagger.Taggers[1].Galleries, cfg.Schedule.Galleries
	check := func(step string, want [][]string, wantLists map[string][]string) {
		t.Helper()
		for i, w := range want {
			got := cfg.Tagger.Taggers[i].Galleries
			if !slices.Equal(got, w) || (got == nil) != (w == nil) {
				t.Errorf("%s: tagger %q galleries = %#v, want %#v", step, cfg.Tagger.Taggers[i].Name, got, w)
			}
		}
		for action, w := range wantLists {
			if got := cfg.Schedule.Galleries[action]; !slices.Equal(got, w) || got == nil {
				t.Errorf("%s: schedule %s galleries = %#v, want %#v", step, action, got, w)
			}
		}
	}

	cfg.RenameGalleryRefs("anime", "art")
	check("rename", [][]string{nil, {"art", "stock"}, {"art"}, {"art"}},
		map[string][]string{ActionSyncGallery: {"art", "stock"}, ActionLookupPTR: {"art"}})
	if !slices.Equal(old, []string{"anime", "stock"}) || !slices.Equal(oldLists[ActionLookupPTR], []string{"anime"}) {
		t.Errorf("the rename wrote through the lists a snapshot holds: %q, %q", old, oldLists)
	}

	cfg.DropGalleryRefs("art")
	check("removal", [][]string{nil, {"stock"}, {}, {}},
		map[string][]string{ActionSyncGallery: {"stock"}, ActionLookupPTR: {}})
}

func TestScheduleRunsOnFollowsTheSwitchAndTheList(t *testing.T) {
	sc := ScheduleConfig{
		SyncGallery:   true,
		RemoveOrphans: true,
		LookupBooru:   true,
		Galleries: map[string][]string{
			ActionRemoveOrphans: {"anime"},
			ActionLookupBooru:   {},
			ActionLookupPTR:     {"anime"},
		},
	}
	for _, c := range []struct {
		action, gallery string
		want            bool
	}{
		{ActionSyncGallery, "anime", true},
		{ActionSyncGallery, "added-tomorrow", true},
		{ActionRemoveOrphans, "anime", true},
		{ActionRemoveOrphans, "photos", false},
		{ActionLookupBooru, "anime", false},
		{ActionLookupPTR, "anime", false},
		{ActionFindRelationPairs, "anime", false},
		{"vacuum", "anime", false},
	} {
		if got := sc.RunsOn(c.action, c.gallery); got != c.want {
			t.Errorf("RunsOn(%q, %q) = %v, want %v", c.action, c.gallery, got, c.want)
		}
	}
}

func TestLookupBackendsNeedThePairingAndThePhase(t *testing.T) {
	cfg := Default()
	cfg.Schedule.LookupPTR, cfg.Schedule.LookupBooru = true, true
	cfg.Schedule.Galleries = map[string][]string{ActionLookupBooru: {"anime"}}
	if got := cfg.LookupBackends("anime"); got != nil {
		t.Errorf("unpaired: %v, want none", got)
	}
	cfg.Auth.Tokens = apitoken.List{{Name: "plugin", Paired: "other"}}
	if cfg.MonloaderPaired() {
		t.Error("another app's pairing reads as monloader's")
	}
	cfg.Auth.Tokens = append(cfg.Auth.Tokens, apitoken.Token{Name: "monloader", Paired: contract.AppMonloader})
	if !cfg.MonloaderPaired() {
		t.Fatal("monloader's pairing is not seen")
	}
	if got, want := cfg.LookupBackends("anime"), []string{contract.LookupPTR, contract.LookupBooru}; !slices.Equal(got, want) {
		t.Errorf("anime: %v, want %v", got, want)
	}
	if got, want := cfg.LookupBackends("photos"), []string{contract.LookupPTR}; !slices.Equal(got, want) {
		t.Errorf("photos, off the booru list: %v, want %v", got, want)
	}
	cfg.Schedule.LookupPTR = false
	if got := cfg.LookupBackends("photos"); got != nil {
		t.Errorf("both phases off for photos: %v, want none", got)
	}
}

func TestScheduleGalleriesRoundTrip(t *testing.T) {

	if Default().Schedule.Galleries != nil {
		t.Fatal("Default() seeds schedule.galleries")
	}
	path := filepath.Join(t.TempDir(), "monbooru.toml")
	cfg := Default()
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "[schedule.galleries]") {
		t.Errorf("no list is set, yet the file carries the table:\n%s", raw)
	}

	cfg.Schedule.Galleries = map[string][]string{ActionLookupBooru: {"default"}, ActionLookupPTR: {}}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	got := mustLoad(t, path).Schedule.Galleries
	if len(got) != 2 || !slices.Equal(got[ActionLookupBooru], []string{"default"}) {
		t.Errorf("galleries = %#v", got)
	}
	if none, ok := got[ActionLookupPTR]; !ok || none == nil || len(none) != 0 {
		t.Errorf("an empty list came back as %#v (present %v)", none, ok)
	}
}

func TestScheduleGalleriesDropAKeyThatIsNoAction(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[schedule.galleries]
lookup_boru = ["default"]
lookup_ptr = ["default"]
`)
	got := mustLoad(t, path).Schedule.Galleries
	if len(got) != 1 || !slices.Equal(got[ActionLookupPTR], []string{"default"}) {
		t.Errorf("galleries = %#v, want lookup_ptr alone", got)
	}
}

func TestLoadRejectsDuplicateGalleryNames(t *testing.T) {
	path := writeConfig(t, `
[[galleries]]
name = "a"
gallery_path = "/gallery1"

[[galleries]]
name = "a"
gallery_path = "/gallery2"
`)
	if _, err := LoadWithDefaults(path, nil); err == nil {
		t.Error("expected error for duplicate gallery name")
	}
}

func TestDerivePaths(t *testing.T) {
	cfg := Default()
	db, thumbs := cfg.DerivePaths("stock")
	if db != "/data/stock/monbooru.db" {
		t.Errorf("DerivePaths db = %q", db)
	}
	if thumbs != "/data/stock/thumbnails" {
		t.Errorf("DerivePaths thumbs = %q", thumbs)
	}
}

func TestValidateGalleryName(t *testing.T) {
	t.Parallel()
	valid := []string{"default", "a", "stock_1", "StockSet-2"}
	invalid := []string{"", "with space", "weird/slash", "dot.name", "accénté"}
	for _, n := range valid {
		t.Run("valid/"+n, func(t *testing.T) {
			t.Parallel()
			if err := ValidateGalleryName(n); err != nil {
				t.Errorf("ValidateGalleryName(%q) = %v, want nil", n, err)
			}
		})
	}
	for _, n := range invalid {
		t.Run("invalid/"+n, func(t *testing.T) {
			t.Parallel()
			if err := ValidateGalleryName(n); err == nil {
				t.Errorf("ValidateGalleryName(%q) = nil, want error", n)
			}
		})
	}
}

func TestSave_ErrorOnBadDir(t *testing.T) {

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(Default(), filepath.Join(file, "deep", "monbooru.toml")); err == nil {
		t.Error("expected error saving under a path whose parent is a file")
	}
}

func TestCORSOriginsNormaliseAndDropTheUnusable(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[server]
cors_origins = [
  "http://localhost:5173/",
  "  https://tags.example  ",
  "*",
  "moz-extension://abcdef",
  "192.168.1.50:5173",
  "https://tags.example/app",
  "",
]
`)
	cfg := mustLoad(t, path)
	want := []string{"http://localhost:5173", "https://tags.example", "*", "moz-extension://abcdef"}
	if !slices.Equal(cfg.Server.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %q, want %q", cfg.Server.CORSOrigins, want)
	}
}

func TestCORSOriginsEnvOverride(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[server]
cors_origins = ["https://from-file.example"]
`)

	t.Setenv("MONBOORU_SERVER_CORS_ORIGINS", "http://localhost:5173, https://tags.example/ ,nope")
	cfg := mustLoad(t, path)
	want := []string{"http://localhost:5173", "https://tags.example"}
	if !slices.Equal(cfg.Server.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %q, want %q", cfg.Server.CORSOrigins, want)
	}
}

func TestValidate_EmptyBindAddress(t *testing.T) {
	path := writeConfig(t, "[server]\nbind_address = \"\"")
	if _, err := LoadWithDefaults(path, nil); err == nil {
		t.Error("expected error for empty bind_address")
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	path := writeConfig(t, "not valid toml ][[[")
	if _, err := LoadWithDefaults(path, nil); err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestSave_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skip("cannot change dir permissions")
	}
	defer func() { _ = os.Chmod(dir, 0755) }()
	if err := Save(Default(), filepath.Join(dir, "monbooru.toml")); err == nil {
		if os.Getuid() == 0 {
			t.Skip("running as root, chmod has no effect")
		}
		t.Error("expected error saving to read-only directory")
	}
}

func TestSaveAtomicTempInSameDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	cfg := Default()
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	cfg2 := mustLoad(t, path)
	if cfg2.Server.BindAddress != cfg.Server.BindAddress {
		t.Errorf("round-trip BindAddress mismatch")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".monbooru.toml.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestExecutionProviderMigration(t *testing.T) {
	load := func(t *testing.T, taggerSection string) *Config {
		t.Helper()
		path := writeConfig(t, baseTOML+taggerSection)
		cfg := mustLoad(t, path)
		return cfg
	}

	if got := load(t, "").Tagger.ExecutionProvider; got != "cpu" {
		t.Errorf("no tagger section: provider = %q, want cpu", got)
	}
	if got := load(t, "[tagger]\nuse_cuda = true\n").Tagger.ExecutionProvider; got != "cuda" {
		t.Errorf("use_cuda=true: provider = %q, want cuda", got)
	}
	if got := load(t, "[tagger]\nuse_cuda = true\nexecution_provider = \"openvino\"\n").Tagger.ExecutionProvider; got != "openvino" {
		t.Errorf("explicit provider beside use_cuda: provider = %q, want openvino", got)
	}

	t.Setenv("MONBOORU_TAGGER_USE_CUDA", "true")
	if got := load(t, "").Tagger.ExecutionProvider; got != "cuda" {
		t.Errorf("legacy env alias: provider = %q, want cuda", got)
	}
	t.Setenv("MONBOORU_TAGGER_EXECUTION_PROVIDER", "cpu")
	if got := load(t, "").Tagger.ExecutionProvider; got != "cpu" {
		t.Errorf("provider env beside legacy env: provider = %q, want cpu", got)
	}
}

func TestExecutionProviderFallsBackOnUnknown(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[tagger]
execution_provider = "quantum"
`)
	if got := mustLoad(t, path).Tagger.ExecutionProvider; got != "cpu" {
		t.Errorf("execution_provider = quantum loaded as %q, want the default cpu", got)
	}

	path = writeConfig(t, baseTOML+`
[tagger]
execution_provider = "cuda"
`)
	t.Setenv("MONBOORU_TAGGER_EXECUTION_PROVIDER", "quantum")
	cfg := mustLoad(t, path)
	if got := cfg.Tagger.ExecutionProvider; got != "cuda" {
		t.Errorf("env execution_provider = quantum loaded as %q, want the file's cuda", got)
	}
	if cfg.EnvPinned("tagger.execution_provider") != "" {
		t.Error("an ignored env provider still pins the key")
	}
}

func TestScheduleTimeFallsBackOnInvalid(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[schedule]
time = "25:61"
`)
	if got := mustLoad(t, path).Schedule.Time; got != "01:00" {
		t.Errorf("schedule.time = 25:61 loaded as %q, want the default 01:00", got)
	}
}

func TestLoadCleansGalleryPath(t *testing.T) {
	path := writeConfig(t, `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery/sub/../"

[paths]
data_path = "/my/data"
`)
	cfg := mustLoad(t, path)
	if got := cfg.Galleries[0].GalleryPath; got != "/my/gallery" {
		t.Errorf("GalleryPath = %q, want %q", got, "/my/gallery")
	}
}

func TestSaveOmitsEmptyArraysOfTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monbooru.toml")
	cfg := Default()
	cfg.Plugins = []PluginConfig{{Name: "monedit", PeerToken: "sek"}}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	cfg.Plugins = cfg.Plugins[:0]
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(raw), "plugin = []") {
		t.Error("an emptied plugin list was written as an inline array")
	}

	appended, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appended.WriteString("\n[[plugin]]\nname = \"monedit\"\n"); err != nil {
		t.Fatal(err)
	}
	_ = appended.Close()
	loaded := mustLoad(t, path)
	if loaded.FindPlugin("monedit") == nil {
		t.Errorf("the block was dropped: %+v", loaded.Plugins)
	}

	noGalleries := Default()
	noGalleries.Galleries = nil
	bare := filepath.Join(t.TempDir(), "bare.toml")
	if err := Save(noGalleries, bare); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(bare); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(raw), "galleries = []") {
		t.Error("an emptied gallery list was written as an inline array")
	}
}

func TestPluginBlockRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	cfg := Default()
	cfg.Plugins = []PluginConfig{{
		Name: "monedit", Version: "0.3.1", PeerToken: "sek", Enabled: true,
		Buttons: []PluginButton{
			{Slot: SlotDetailActions, Label: "crop", Mode: ModeOpen, Path: "/edit?image={image_id}&back={back_url}"},
			{Slot: SlotBatchBar, Label: "rotate 90", Mode: ModeRelay},
		},
	}}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	loaded := mustLoad(t, path)
	p := loaded.FindPlugin("monedit")
	if p == nil {
		t.Fatalf("plugin block lost: %+v", loaded.Plugins)
	}
	if p.PeerToken != "sek" || !p.Enabled || len(p.Buttons) != 2 {
		t.Errorf("block did not round-trip: %+v", *p)
	}
	if p.Buttons[0].Path != "/edit?image={image_id}&back={back_url}" {
		t.Errorf("button path = %q", p.Buttons[0].Path)
	}
}

func TestLoadDropsMalformedPluginBlocks(t *testing.T) {
	path := writeConfig(t, `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/gallery"

[paths]
data_path = "/data"

[[plugin]]
name = "mon edit"

[[plugin]]
name = "monedit"
peer_token = "sek"

  [[plugin.button]]
  slot = "detail-actions"
  label = "crop"
  mode = "open"
  path = "/edit?image={image_id}"

  [[plugin.button]]
  slot = "sidebar"
  label = "nope"
  mode = "open"
  path = "/x"
`)
	cfg := mustLoad(t, path)
	if len(cfg.Plugins) != 1 || cfg.Plugins[0].Name != "monedit" {
		t.Fatalf("blocks = %+v, want only monedit", cfg.Plugins)
	}
	if len(cfg.Plugins[0].Buttons) != 1 || cfg.Plugins[0].Buttons[0].Label != "crop" {
		t.Errorf("buttons = %+v, want only the valid one", cfg.Plugins[0].Buttons)
	}
}

func TestTrayDefaultsOnForAnExistingConfig(t *testing.T) {
	content := `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery"
`
	path := writeConfig(t, content)
	cfg := mustLoad(t, path)
	if !cfg.Desktop.Tray {
		t.Error("desktop.tray should read as true for a file that predates the key")
	}
}

func TestTrayHonoursAnExplicitFalse(t *testing.T) {
	content := `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery"

[desktop]
tray = false
`
	path := writeConfig(t, content)
	cfg := mustLoad(t, path)
	if cfg.Desktop.Tray {
		t.Error("desktop.tray = false was ignored")
	}
}

func TestSetupDoneDefaultsTrueForAnExistingConfig(t *testing.T) {
	content := `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery"
`
	path := writeConfig(t, content)
	cfg := mustLoad(t, path)
	if !cfg.SetupDone {
		t.Error("setup_done should read as true for a file that predates the key")
	}
}

func TestSetupDoneHonoursAnExplicitFalse(t *testing.T) {
	content := `
default_gallery = "default"
setup_done = false

[[galleries]]
name = "default"
gallery_path = "/my/gallery"
`
	path := writeConfig(t, content)
	cfg := mustLoad(t, path)
	if cfg.SetupDone {
		t.Error("an explicit setup_done = false must survive the load")
	}
}

func TestFreshConfigStartsUnsetUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monbooru.toml")
	cfg := mustLoad(t, path)
	if cfg.SetupDone {
		t.Error("a config monbooru just created should start at setup_done = false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "setup_done = false") {
		t.Errorf("setup_done not written to the fresh config:\n%s", data)
	}
}

func TestLoadWithDefaultsSeedsOnlyAFreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	seed := func(c *Config) {
		c.Paths.DataPath = "/seeded/data"
		c.Server.BindAddress = "127.0.0.1:8455"
	}
	cfg, err := LoadWithDefaults(path, seed)
	if err != nil {
		t.Fatalf("LoadWithDefaults: %v", err)
	}
	if cfg.Paths.DataPath != "/seeded/data" || cfg.Server.BindAddress != "127.0.0.1:8455" {
		t.Fatalf("seed not applied: data=%q bind=%q", cfg.Paths.DataPath, cfg.Server.BindAddress)
	}

	again, err := LoadWithDefaults(path, func(c *Config) { c.Paths.DataPath = "/other" })
	if err != nil {
		t.Fatalf("LoadWithDefaults: %v", err)
	}
	if again.Paths.DataPath != "/seeded/data" {
		t.Errorf("data_path = %q, want the stored /seeded/data", again.Paths.DataPath)
	}
}

func TestIgnoreDefaultsAndAnEmptyListRoundTrips(t *testing.T) {
	if got := mustLoad(t, writeConfig(t, baseTOML)).Gallery.Ignore; !slices.Equal(got, defaultIgnore) {
		t.Errorf("absent ignore = %q, want the default list", got)
	}

	path := writeConfig(t, baseTOML+"\n[gallery]\nignore = []\n")
	cfg := mustLoad(t, path)
	if len(cfg.Gallery.Ignore) != 0 {
		t.Fatalf("ignore = [] loaded as %q", cfg.Gallery.Ignore)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, path).Gallery.Ignore; len(got) != 0 {
		t.Errorf("an empty list came back from Save as %q", got)
	}
}

func TestIgnoreDropsWhatCouldNeverServe(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[gallery]
ignore = ["@eaDir", " drafts/ ", "[abc", "*", "*/?", "@EADIR", "", "/wip", "2024/*/raw"]
`)
	want := []string{"@eaDir", "drafts", "/wip", "2024/*/raw"}
	if got := mustLoad(t, path).Gallery.Ignore; !slices.Equal(got, want) {
		t.Errorf("ignore = %q, want %q", got, want)
	}
}

func TestNormalizeIgnoreNamesTheEntryItRefuses(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"[abc", `"[abc" is not a valid pattern`},
		{"*", `"*" would leave out every file`},
		{"/**/", `"/**" would leave out every file`},
	} {
		if _, err := NormalizeIgnore([]string{"ok", c.in}); err == nil || err.Error() != c.want {
			t.Errorf("NormalizeIgnore(%q) = %v, want %q", c.in, err, c.want)
		}
	}
}

func TestValidateNormalisesTaggerAndRelationsValues(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[relations]
default_distance = 20

[tagger]
parallel = 0
idle_release_after_minutes = -1

[tagger.aggregation]
min_hit_fraction = 2.0

[[tagger.taggers]]
name = "wd"
confidence_threshold = 1.5

[tagger.taggers.category_thresholds]
general = 2.0
character = 0.7
`)
	cfg := mustLoad(t, path)
	if cfg.Relations.DefaultDistance != 4 {
		t.Errorf("default_distance = %d, want 4", cfg.Relations.DefaultDistance)
	}
	if cfg.Tagger.Parallel != 4 {
		t.Errorf("parallel = %d, want 4", cfg.Tagger.Parallel)
	}
	if cfg.Tagger.IdleReleaseAfterMinutes != 15 {
		t.Errorf("idle_release_after_minutes = %d, want 15", cfg.Tagger.IdleReleaseAfterMinutes)
	}
	if cfg.Tagger.Aggregation.MinHitFraction != 0.05 {
		t.Errorf("min_hit_fraction = %v, want 0.05", cfg.Tagger.Aggregation.MinHitFraction)
	}
	tg := cfg.Tagger.Taggers[0]
	if tg.ConfidenceThreshold != 0.4 {
		t.Errorf("confidence_threshold = %v, want 0.4", tg.ConfidenceThreshold)
	}
	if want := map[string]float64{"character": 0.7}; !maps.Equal(tg.CategoryThresholds, want) {
		t.Errorf("category_thresholds = %v, want %v", tg.CategoryThresholds, want)
	}
}

func TestSaveLeavesEnvironmentValuesOutOfTheFile(t *testing.T) {
	path := writeConfig(t, `
[[galleries]]
name = "default"
gallery_path = "/gallery"

[server]
bind_address = "127.0.0.1:8455"

[paths]
data_path = "/data"

[gallery]
max_file_size_mb = 100

[monloader]
api_token = "file-token"
`)
	t.Setenv("MONBOORU_SERVER_BIND_ADDRESS", "0.0.0.0:9000")
	t.Setenv("MONBOORU_GALLERY_MAX_FILE_SIZE_MB", "300")
	t.Setenv("MONBOORU_MONLOADER_API_TOKEN", "env-token")
	cfg := mustLoad(t, path)
	if cfg.Server.BindAddress != "0.0.0.0:9000" || cfg.Gallery.MaxFileSizeMB != 300 || cfg.Monloader.APIToken != "env-token" {
		t.Fatalf("loaded %q %d %q, want the environment's values", cfg.Server.BindAddress, cfg.Gallery.MaxFileSizeMB, cfg.Monloader.APIToken)
	}
	if got := cfg.EnvPinned("gallery.max_file_size_mb"); got != "MONBOORU_GALLERY_MAX_FILE_SIZE_MB" {
		t.Errorf("EnvPinned(max_file_size_mb) = %q", got)
	}
	if got := cfg.EnvPinned("ui.page_size"); got != "" {
		t.Errorf("EnvPinned(page_size) = %q, want empty", got)
	}
	cfg.UI.PageSize = 60
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.BindAddress != "0.0.0.0:9000" {
		t.Errorf("Save changed the running bind address to %q", cfg.Server.BindAddress)
	}

	for _, k := range []string{"MONBOORU_SERVER_BIND_ADDRESS", "MONBOORU_GALLERY_MAX_FILE_SIZE_MB", "MONBOORU_MONLOADER_API_TOKEN"} {
		t.Setenv(k, "")
	}
	file := mustLoad(t, path)
	if file.Server.BindAddress != "127.0.0.1:8455" || file.Gallery.MaxFileSizeMB != 100 || file.Monloader.APIToken != "file-token" {
		t.Errorf("file holds %q %d %q, want its own values", file.Server.BindAddress, file.Gallery.MaxFileSizeMB, file.Monloader.APIToken)
	}
	if file.UI.PageSize != 60 {
		t.Errorf("file page_size = %d, want the saved 60", file.UI.PageSize)
	}
}

func TestLoadTakesAPasswordHashFromTheEnvironment(t *testing.T) {
	path := writeConfig(t, baseTOML+`
[auth]
enable_password = true
`)
	hash := "$2a$10$abcdefghijklmnopqrstuuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0"
	t.Setenv("MONBOORU_AUTH_PASSWORD_HASH", hash)
	cfg, err := LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.PasswordHash != hash {
		t.Errorf("password_hash = %q, want the environment's", cfg.Auth.PasswordHash)
	}
}

func TestLoadWarnsOnAnUnknownKey(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	saved := filepath.Join(t.TempDir(), "monbooru.toml")
	if err := Save(Default(), saved); err != nil {
		t.Fatal(err)
	}
	mustLoad(t, saved)
	if strings.Contains(buf.String(), "unknown key") {
		t.Errorf("a saved config warned: %s", buf.String())
	}

	buf.Reset()
	cfg := mustLoad(t, writeConfig(t, "[[galleries]]\nname = \"default\"\ngallery_path = \"/g\"\n\n[server]\nbind_adress = \"0.0.0.0:1\"\n\n[sever]\nbase_url = \"x\"\n"))
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "server.bind_adress") || !strings.Contains(out, "sever.base_url") {
		t.Errorf("logged %q, want a warning naming server.bind_adress and sever.base_url", out)
	}
	if cfg.Server.BindAddress != Default().Server.BindAddress {
		t.Errorf("BindAddress = %q, want the default", cfg.Server.BindAddress)
	}
}
