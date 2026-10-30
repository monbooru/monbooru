// Package config loads, validates and atomically saves monbooru.toml. It
// takes no lock: callers guard a shared Config themselves.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/pairing"
)

type Config struct {
	DefaultGallery string `toml:"default_gallery"`
	// SetupDone gates the desktop first-run wizard; a file without the
	// key loads as true, so an upgrade skips it.
	SetupDone bool            `toml:"setup_done"`
	Galleries []Gallery       `toml:"galleries,omitempty"`
	Server    ServerConfig    `toml:"server"`
	Monloader MonloaderConfig `toml:"monloader"`
	Paths     PathsConfig     `toml:"paths"`
	Gallery   GalleryConfig   `toml:"gallery"`
	Tagger    TaggerConfig    `toml:"tagger"`
	Auth      AuthConfig      `toml:"auth"`
	UI        UIConfig        `toml:"ui"`
	Log       LogConfig       `toml:"log"`
	Schedule  ScheduleConfig  `toml:"schedule"`
	Relations RelationsConfig `toml:"relations"`
	Desktop   DesktopConfig   `toml:"desktop"`
	// Omitted when empty: TOML refuses a hand-added [[plugin]] block once
	// `plugin = []` is written.
	Plugins []PluginConfig `toml:"plugin,omitempty"`

	// Unexported so the encoder never writes them.
	portableBase string
	envPins      map[string]envPin
}

type envPin struct {
	variable string
	restore  func(*Config)
}

// EnvPinned names the environment variable that overrode key (a dotted TOML
// key) at load, or "" when the file's value stands.
func (cfg *Config) EnvPinned(key string) string { return cfg.envPins[key].variable }

// 0.85 keeps the tag-pair queue proportional to the library; below 0.7 it
// offers more pairs than anyone can decide.
const (
	DefaultTagPairThreshold = 0.85
	MinTagPairThreshold     = 0.7
)

const (
	MaxPhashDistance           = 12
	DefaultConfidenceThreshold = 0.4
)

func ClampTagPairThreshold(v float64) float64 {
	switch {
	case v < MinTagPairThreshold:
		return MinTagPairThreshold
	case v > 1:
		return 1
	}
	return v
}

type RelationsConfig struct {
	DefaultDistance     int     `toml:"default_distance"`
	DefaultSessionOrder string  `toml:"default_session_order"`
	IncrementalOnIngest bool    `toml:"incremental_on_ingest"`
	TagPairs            bool    `toml:"tag_pairs"`
	TagPairThreshold    float64 `toml:"tag_pair_threshold"`
}

type ServerConfig struct {
	BindAddress string `toml:"bind_address"`
	BaseURL     string `toml:"base_url"`
	// Allowing "*" relies on the API's bearer auth being cookie-free.
	CORSOrigins []string `toml:"cors_origins,omitempty"`
	BooruName   string   `toml:"name"`
	Theme       string   `toml:"theme,omitempty"`
	ThemeColor  string   `toml:"theme_color"`
	// The browser-facing address; blank renders no link, since
	// [monloader].api_url may be one a browser cannot resolve.
	MonloaderURL string `toml:"monloader_url"`
}

type MonloaderConfig struct {
	APIURL   string `toml:"api_url"`
	APIToken string `toml:"api_token,omitempty"`
	Paused   bool   `toml:"paused,omitempty"`
}

// PluginConfig never names a program: a plugin monbooru launches is a
// folder under <configdir>/plugins/ carrying its own manifest.
type PluginConfig struct {
	Name      string `toml:"name"`
	Version   string `toml:"version,omitempty"`
	APIURL    string `toml:"api_url,omitempty"`
	PeerToken string `toml:"peer_token,omitempty"`
	Paused    bool   `toml:"paused,omitempty"`
	// Boot-start switch for a plugin found under the plugins folder.
	Enabled bool           `toml:"enabled,omitempty"`
	Buttons []PluginButton `toml:"button,omitempty"`
}

type PluginButton struct {
	Slot  string `toml:"slot" json:"slot"`
	Label string `toml:"label" json:"label"`
	Mode  string `toml:"mode" json:"mode"`
	Path  string `toml:"path,omitempty" json:"path,omitempty"`
	Media string `toml:"media,omitempty" json:"media,omitempty"`
}

func (b PluginButton) AppliesTo(fileType string) bool {
	if b.Media == "" {
		return true
	}
	kind := models.MediaKind(fileType)
	return slices.ContainsFunc(strings.Split(b.Media, ","), func(v string) bool { return strings.TrimSpace(v) == kind })
}

const (
	SlotDetailActions = "detail-actions"
	SlotBatchBar      = "batch-bar"
	ModeOpen          = "open"
	ModeRelay         = "relay"
)

const (
	MaxPluginLabel      = 24
	MaxPluginVersion    = 32
	maxPluginSlotButton = 4
)

const (
	PluginVarImageID = "{image_id}"
	PluginVarGallery = "{gallery}"
	PluginVarBackURL = "{back_url}"
)

var PluginVars = []string{PluginVarImageID, PluginVarGallery, PluginVarBackURL}

var pluginVarRe = regexp.MustCompile(`\{[^}]*\}`)

var reservedPluginNames = []string{"monbooru", "api", "default"}

func ValidatePluginName(name string) error {
	if !pairing.ValidApp(name) {
		return fmt.Errorf("plugin name %q must match [A-Za-z0-9_-]{1,32}", name)
	}
	if slices.Contains(reservedPluginNames, strings.ToLower(name)) {
		return fmt.Errorf("plugin name %q is reserved", name)
	}
	return nil
}

func ValidatePluginButton(b PluginButton) error {
	if b.Slot != SlotDetailActions && b.Slot != SlotBatchBar {
		return fmt.Errorf("unknown slot %q", b.Slot)
	}
	if b.Mode != ModeOpen && b.Mode != ModeRelay {
		return fmt.Errorf("unknown mode %q", b.Mode)
	}
	// A batch-bar click carries a scope, which a page cannot receive.
	if b.Mode == ModeOpen && b.Slot != SlotDetailActions {
		return fmt.Errorf("open mode is only valid on %s", SlotDetailActions)
	}
	label := strings.TrimSpace(b.Label)
	if label == "" || utf8.RuneCountInString(label) > MaxPluginLabel {
		return fmt.Errorf("label must be 1-%d characters", MaxPluginLabel)
	}
	if b.Media != "" {
		for _, v := range strings.Split(b.Media, ",") {
			if kind := strings.TrimSpace(v); !slices.Contains(models.MediaKinds, kind) {
				return fmt.Errorf("unknown media %q", kind)
			}
		}
	}
	// A relay with no path posts to the peer base.
	if b.Path == "" && b.Mode == ModeOpen {
		return fmt.Errorf("open mode needs a path")
	}
	if b.Path != "" && !strings.HasPrefix(b.Path, "/") {
		return fmt.Errorf("path %q must start with /", b.Path)
	}
	for _, v := range pluginVarRe.FindAllString(b.Path, -1) {
		if !slices.Contains(PluginVars, v) {
			return fmt.Errorf("unknown substitution variable %s", v)
		}
	}
	return nil
}

func ValidatePluginButtons(buttons []PluginButton) error {
	perSlot := map[string]int{}
	for _, b := range buttons {
		if err := ValidatePluginButton(b); err != nil {
			return err
		}
		perSlot[b.Slot]++
		if perSlot[b.Slot] > maxPluginSlotButton {
			return fmt.Errorf("at most %d buttons per slot", maxPluginSlotButton)
		}
	}
	return nil
}

func (cfg *Config) FindPlugin(name string) *PluginConfig {
	i := slices.IndexFunc(cfg.Plugins, func(p PluginConfig) bool { return p.Name == name })
	if i < 0 {
		return nil
	}
	return &cfg.Plugins[i]
}

type PathsConfig struct {
	DataPath  string `toml:"data_path"`
	ModelPath string `toml:"model_path"`
}

type Gallery struct {
	Name           string `toml:"name"`
	GalleryPath    string `toml:"gallery_path"`
	DBPath         string `toml:"-"`
	ThumbnailsPath string `toml:"-"`
}

type GalleryConfig struct {
	WatchEnabled        bool   `toml:"watch_enabled"`
	MaxFileSizeMB       int    `toml:"max_file_size_mb"`
	DefaultUploadFolder string `toml:"default_upload_folder"`
	DefaultUploadName   string `toml:"default_upload_name"`
	RenameOnIngest      bool   `toml:"rename_on_ingest"`
	AutoMetaTags        bool   `toml:"auto_meta_tags"`
	// No omitempty: a saved `ignore = []` must not reload as the default list.
	Ignore []string `toml:"ignore"`
}

type TaggerConfig struct {
	// Read only to migrate old configs, and never written back.
	UseCUDA           bool   `toml:"use_cuda,omitempty"`
	ExecutionProvider string `toml:"execution_provider"`
	Parallel          int    `toml:"parallel"`
	// 0 disables the cache: each run loads the model fresh.
	IdleReleaseAfterMinutes int                  `toml:"idle_release_after_minutes"`
	Aggregation             TaggerAggregationCfg `toml:"aggregation"`
	Taggers                 []TaggerInstance     `toml:"taggers"`
}

// ExecutionProviders is every tagger.execution_provider value, in the
// order Settings lists them, with the label it shows.
var ExecutionProviders = []struct{ Name, Label string }{
	{"cpu", "CPU"},
	{"cuda", "CUDA"},
	{"directml", "DirectML"},
	{"tensorrt", "TensorRT"},
	{"openvino", "OpenVINO"},
	{"coreml", "CoreML"},
	{"coremlv2", "CoreML V2"},
}

var ValidExecutionProviders = func() []string {
	names := make([]string, len(ExecutionProviders))
	for i, p := range ExecutionProviders {
		names[i] = p.Name
	}
	return names
}()

func IsValidExecutionProvider(v string) bool { return slices.Contains(ValidExecutionProviders, v) }

func ExecutionProviderLabel(name string) string {
	for _, p := range ExecutionProviders {
		if p.Name == name {
			return p.Label
		}
	}
	return name
}

type TaggerAggregationCfg struct {
	MinHitFraction float64 `toml:"min_hit_fraction"`
}

type TaggerInstance struct {
	Name                string             `toml:"name"`
	Enabled             bool               `toml:"enabled"`
	ConfidenceThreshold float64            `toml:"confidence_threshold"`
	ModelFile           string             `toml:"model_file"`
	TagsFile            string             `toml:"tags_file"`
	CategoryThresholds  map[string]float64 `toml:"category_thresholds,omitempty"`
	PerCategoryTopK     map[string]int     `toml:"per_category_top_k,omitempty"`
	DisabledCategories  []string           `toml:"disabled_categories,omitempty"`
	// Nil means every gallery and an empty list none; omitempty would
	// write neither, and both would reload as nil.
	Galleries []string `toml:"galleries"`
}

func (t TaggerInstance) AppliesToGallery(name string) bool {
	if t.Galleries == nil {
		return true
	}
	return slices.Contains(t.Galleries, name)
}

type AuthConfig struct {
	EnablePassword      bool          `toml:"enable_password"`
	PasswordHash        string        `toml:"password_hash"`
	SessionLifetimeDays int           `toml:"session_lifetime_days"`
	Tokens              apitoken.List `toml:"tokens,omitempty"`
}

const (
	ScopeRead   = contract.ScopeRead
	ScopeWrite  = contract.ScopeWrite
	ScopeDelete = "delete"
)

var AllScopes = []string{ScopeRead, ScopeWrite, ScopeDelete}

type UIConfig struct {
	PageSize     int    `toml:"page_size"`
	ThumbnailFit string `toml:"thumbnail_fit"`
}

// DesktopConfig holds only the tray switch: start-at-login and the menu
// entry live where the platform reads them.
type DesktopConfig struct {
	Tray bool `toml:"tray"`
}

type LogConfig struct {
	Level string `toml:"level"`
}

// Off overrides every per-action flag without clearing them.
const (
	ScheduleAtTime        = "at_time"
	ScheduleAtTimeCatchup = "at_time_catchup"
	ScheduleOnStart       = "on_start"
	ScheduleOff           = "off"
)

var ScheduleModes = []string{ScheduleAtTime, ScheduleAtTimeCatchup, ScheduleOnStart, ScheduleOff}

func IsValidScheduleMode(v string) bool { return slices.Contains(ScheduleModes, v) }

// Spelled as their [schedule] keys, which also key [schedule.galleries].
const (
	ActionSyncGallery       = "sync_gallery"
	ActionRemoveOrphans     = "remove_orphans"
	ActionRunAutoTaggers    = "run_auto_taggers"
	ActionFindRelationPairs = "find_relation_pairs"
	ActionLookupPTR         = "lookup_ptr"
	ActionLookupBooru       = "lookup_booru"
)

var ScheduleActions = []string{
	ActionSyncGallery, ActionRemoveOrphans, ActionRunAutoTaggers,
	ActionFindRelationPairs, ActionLookupPTR, ActionLookupBooru,
}

type ScheduleConfig struct {
	Time              string `toml:"time"`
	Mode              string `toml:"mode"`
	SyncGallery       bool   `toml:"sync_gallery"`
	RemoveOrphans     bool   `toml:"remove_orphans"`
	RunAutoTaggers    bool   `toml:"run_auto_taggers"`
	FindRelationPairs bool   `toml:"find_relation_pairs"`
	LookupPTR         bool   `toml:"lookup_ptr"`
	LookupBooru       bool   `toml:"lookup_booru"`
	// Writers replace the map and its lists rather than write through
	// them: a config copy shares them.
	Galleries map[string][]string `toml:"galleries,omitempty"`
}

func (sc ScheduleConfig) EffectiveMode() string {
	if IsValidScheduleMode(sc.Mode) {
		return sc.Mode
	}
	return ScheduleAtTime
}

func (sc ScheduleConfig) Enabled(action string) bool {
	switch action {
	case ActionSyncGallery:
		return sc.SyncGallery
	case ActionRemoveOrphans:
		return sc.RemoveOrphans
	case ActionRunAutoTaggers:
		return sc.RunAutoTaggers
	case ActionFindRelationPairs:
		return sc.FindRelationPairs
	case ActionLookupPTR:
		return sc.LookupPTR
	case ActionLookupBooru:
		return sc.LookupBooru
	}
	return false
}

func (sc ScheduleConfig) RunsOn(action, gallery string) bool {
	if !sc.Enabled(action) {
		return false
	}
	names, listed := sc.Galleries[action]
	return !listed || slices.Contains(names, gallery)
}

func (cfg *Config) MonloaderPaired() bool {
	return cfg.Auth.Tokens.Paired(contract.AppMonloader) != nil
}

func (cfg *Config) LookupBackends(gallery string) []string {
	if !cfg.MonloaderPaired() {
		return nil
	}
	var out []string
	if cfg.Schedule.RunsOn(ActionLookupPTR, gallery) {
		out = append(out, contract.LookupPTR)
	}
	if cfg.Schedule.RunsOn(ActionLookupBooru, gallery) {
		out = append(out, contract.LookupBooru)
	}
	return out
}

var defaultIgnore = []string{
	"@eaDir", "#recycle", "#snapshot", "@Recycle", ".@__thumb", "$RECYCLE.BIN",
	".Trash-*", ".Trashes", ".zfs", ".snapshots", ".stversions", ".thumbnails", "._*",
}

func Default() *Config {
	return &Config{
		DefaultGallery: "default",
		Galleries: []Gallery{{
			Name:        "default",
			GalleryPath: "/gallery",
		}},
		Server: ServerConfig{
			BindAddress: "127.0.0.1:" + contract.PortMonbooru,
			BaseURL:     "http://localhost:" + contract.PortMonbooru,
		},
		Paths: PathsConfig{
			DataPath:  "/data",
			ModelPath: "/models",
		},
		Gallery: GalleryConfig{
			WatchEnabled:  true,
			MaxFileSizeMB: 2048,
			AutoMetaTags:  true,
			Ignore:        slices.Clone(defaultIgnore),
		},
		Tagger: TaggerConfig{
			ExecutionProvider:       defaultExecutionProvider,
			Parallel:                defaultTaggerParallel,
			IdleReleaseAfterMinutes: defaultIdleReleaseMinutes,
			Aggregation:             TaggerAggregationCfg{MinHitFraction: defaultMinHitFraction},
		},
		Auth: AuthConfig{
			SessionLifetimeDays: defaultSessionLifetimeDays,
		},
		UI: UIConfig{
			PageSize:     defaultPageSize,
			ThumbnailFit: defaultThumbnailFit,
		},
		Log: LogConfig{
			Level: "warn",
		},
		Schedule: ScheduleConfig{
			Time:              defaultScheduleTime,
			Mode:              ScheduleAtTime,
			SyncGallery:       true,
			RemoveOrphans:     true,
			RunAutoTaggers:    false,
			FindRelationPairs: false,
		},
		Desktop: DesktopConfig{
			Tray: true,
		},
		Relations: RelationsConfig{
			DefaultDistance:     defaultRelationsDistance,
			DefaultSessionOrder: "smallest_distance_first",
			IncrementalOnIngest: true,
			TagPairs:            true,
			TagPairThreshold:    DefaultTagPairThreshold,
		},
	}
}

var scheduleTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func ValidateScheduleTime(v string) error {
	if !scheduleTimeRe.MatchString(v) {
		return fmt.Errorf("schedule.time %q must be HH:MM (00:00-23:59)", v)
	}
	return nil
}

// LoadWithDefaults applies seed only to a config it creates; one already on
// disk loads as it stands, so a container's volume layout is never rewritten.
func LoadWithDefaults(path string, seed func(*Config)) (*Config, error) {
	return load(path, "", seed)
}

// LoadPortable resolves relative paths against the config's folder, and
// Save writes them back relative, so the folder can move.
func LoadPortable(path string, seed func(*Config)) (*Config, error) {
	return load(path, filepath.Dir(path), seed)
}

func load(path, base string, seed func(*Config)) (*Config, error) {
	cfg := Default()
	cfg.portableBase = base
	if base != "" {
		portableDefaults(cfg)
	}

	fresh, err := absent(path, base != "")
	if err != nil {
		return nil, err
	}
	if fresh {
		if seed != nil {
			seed(cfg)
		}
		if writeErr := Save(cfg, path); writeErr != nil {
			return nil, fmt.Errorf("creating default config: %w", writeErr)
		}
	} else {
		cfg.Galleries = nil
		cfg.DefaultGallery = ""
		// Cleared so the use_cuda migration tells an omitted key from "cpu".
		cfg.Tagger.ExecutionProvider = ""
		cfg.SetupDone = true
		md, err := toml.DecodeFile(path, cfg)
		if err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
		if keys := md.Undecoded(); len(keys) > 0 {
			logx.Warnf("config: ignoring unknown %s in %s: %v", models.Plural(len(keys), "key", "keys"), path, keys)
		}
		if base != "" && len(cfg.Galleries) == 0 {
			cfg.Galleries = Default().Galleries
			cfg.Galleries[0].GalleryPath = portableGalleryDir
		}
	}
	if base != "" {
		resolveIn(cfg, base)
	}

	migrateTaggerProvider(cfg)
	applyEnvOverrides(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	fillDerivedPaths(cfg)
	return cfg, nil
}

// Relative on purpose: a moved folder or a new drive letter must not
// break them.
const (
	portableDataDir    = "data"
	portableGalleryDir = "gallery"
)

func portableDefaults(cfg *Config) {
	cfg.Paths.DataPath = portableDataDir
	cfg.Paths.ModelPath = filepath.Join(portableDataDir, "models")
	cfg.Galleries[0].GalleryPath = portableGalleryDir
}

// An empty file counts only for a portable install, whose archives ship
// one as their marker; elsewhere it stays an error rather than being
// overwritten with defaults.
func absent(path string, portable bool) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading config file: %w", err)
	}
	return portable && len(strings.TrimSpace(string(b))) == 0, nil
}

func resolveIn(cfg *Config, base string) {
	cfg.Paths.DataPath = fsx.AbsIn(cfg.Paths.DataPath, base)
	cfg.Paths.ModelPath = fsx.AbsIn(cfg.Paths.ModelPath, base)
	for i := range cfg.Galleries {
		cfg.Galleries[i].GalleryPath = fsx.AbsIn(cfg.Galleries[i].GalleryPath, base)
	}
}

// A use_cuda = true config with no execution_provider migrates to "cuda".
func migrateTaggerProvider(cfg *Config) {
	if cfg.Tagger.ExecutionProvider == "" {
		if cfg.Tagger.UseCUDA {
			cfg.Tagger.ExecutionProvider = "cuda"
		} else {
			cfg.Tagger.ExecutionProvider = "cpu"
		}
	}
	cfg.Tagger.UseCUDA = false
}

func Save(cfg *Config, path string) error {
	out := cfg
	if len(cfg.envPins) > 0 {
		out = fileForm(cfg)
	}
	if cfg.portableBase != "" {
		out = portableForm(out)
	}
	return fsx.WriteDurable(path, func(f *os.File) error {
		if err := toml.NewEncoder(f).Encode(out); err != nil {
			return fmt.Errorf("encoding config: %w", err)
		}
		return nil
	})
}

// A copy with the file's own values under the keys the environment set.
func fileForm(cfg *Config) *Config {
	out := *cfg
	for _, p := range cfg.envPins {
		p.restore(&out)
	}
	return &out
}

// Works on a copy, galleries included: the running config keeps its
// absolute paths.
func portableForm(cfg *Config) *Config {
	out := *cfg
	out.Paths.DataPath = fsx.RelIn(cfg.Paths.DataPath, cfg.portableBase)
	out.Paths.ModelPath = fsx.RelIn(cfg.Paths.ModelPath, cfg.portableBase)
	out.Galleries = slices.Clone(cfg.Galleries)
	for i := range out.Galleries {
		out.Galleries[i].GalleryPath = fsx.RelIn(out.Galleries[i].GalleryPath, cfg.portableBase)
	}
	return &out
}

func (cfg *Config) FindGallery(name string) *Gallery {
	i := slices.IndexFunc(cfg.Galleries, func(g Gallery) bool { return g.Name == name })
	if i < 0 {
		return nil
	}
	return &cfg.Galleries[i]
}

func (cfg *Config) RenameGalleryRefs(oldName, newName string) {
	rename := func(names []string) []string { return renameIn(names, oldName, newName) }
	for i := range cfg.Tagger.Taggers {
		cfg.Tagger.Taggers[i].Galleries = rename(cfg.Tagger.Taggers[i].Galleries)
	}
	cfg.Schedule.Galleries = editLists(cfg.Schedule.Galleries, rename)
}

// DropGalleryRefs leaves an emptied list empty, not nil, which would mean
// every gallery.
func (cfg *Config) DropGalleryRefs(name string) {
	drop := func(names []string) []string { return dropFrom(names, name) }
	for i := range cfg.Tagger.Taggers {
		cfg.Tagger.Taggers[i].Galleries = drop(cfg.Tagger.Taggers[i].Galleries)
	}
	cfg.Schedule.Galleries = editLists(cfg.Schedule.Galleries, drop)
}

func editLists(lists map[string][]string, edit func([]string) []string) map[string][]string {
	if lists == nil {
		return nil
	}
	out := make(map[string][]string, len(lists))
	for key, names := range lists {
		out[key] = edit(names)
	}
	return out
}

// renameIn and dropFrom return a new list: a config snapshot may still be
// reading the old one.
func renameIn(names []string, oldName, newName string) []string {
	if !slices.Contains(names, oldName) {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == oldName {
			n = newName
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func dropFrom(names []string, name string) []string {
	if !slices.Contains(names, name) {
		return names
	}
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == name })
}

func (cfg *Config) DerivePaths(name string) (dbPath, thumbnailsPath string) {
	dir := filepath.Join(cfg.Paths.DataPath, name)
	return filepath.Join(dir, "monbooru.db"), filepath.Join(dir, "thumbnails")
}

var folderNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateFolderName guards a name that becomes a folder a removal deletes.
func ValidateFolderName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name must not be empty", kind)
	}
	if !folderNameRe.MatchString(name) {
		return fmt.Errorf("%s name %q must match [A-Za-z0-9_-]+", kind, name)
	}
	return nil
}

func ValidateGalleryName(name string) error { return ValidateFolderName("gallery", name) }

func fillDerivedPaths(cfg *Config) {
	for i := range cfg.Galleries {
		db, th := cfg.DerivePaths(cfg.Galleries[i].Name)
		cfg.Galleries[i].DBPath = db
		cfg.Galleries[i].ThumbnailsPath = th
	}
}

func envOverride[T any](cfg *Config, key string, field func(*Config) *T, parse func(string) (T, error)) {
	variable := "MONBOORU_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	raw := os.Getenv(variable)
	if raw == "" {
		return
	}
	v, err := parse(raw)
	if err != nil {
		logx.Warnf("config: ignoring %s=%q: %v", variable, raw, err)
		return
	}
	pinEnv(cfg, key, variable, field, v)
}

func pinEnv[T any](cfg *Config, key, variable string, field func(*Config) *T, v T) {
	file := *field(cfg)
	*field(cfg) = v
	if cfg.envPins == nil {
		cfg.envPins = map[string]envPin{}
	}
	cfg.envPins[key] = envPin{variable: variable, restore: func(c *Config) { *field(c) = file }}
}

func parseString(v string) (string, error) { return v, nil }

func parseList(v string) ([]string, error) { return strings.Split(v, ","), nil }

func parseProvider(v string) (string, error) {
	if !IsValidExecutionProvider(v) {
		return "", fmt.Errorf("must be one of %v", ValidExecutionProviders)
	}
	return v, nil
}

func applyEnvOverrides(cfg *Config) {
	envOverride(cfg, "server.bind_address", func(c *Config) *string { return &c.Server.BindAddress }, parseString)
	envOverride(cfg, "server.base_url", func(c *Config) *string { return &c.Server.BaseURL }, parseString)
	envOverride(cfg, "server.cors_origins", func(c *Config) *[]string { return &c.Server.CORSOrigins }, parseList)
	envOverride(cfg, "server.monloader_url", func(c *Config) *string { return &c.Server.MonloaderURL }, parseString)
	envOverride(cfg, "paths.data_path", func(c *Config) *string { return &c.Paths.DataPath }, parseString)
	envOverride(cfg, "paths.model_path", func(c *Config) *string { return &c.Paths.ModelPath }, parseString)
	envOverride(cfg, "gallery.watch_enabled", func(c *Config) *bool { return &c.Gallery.WatchEnabled }, strconv.ParseBool)
	envOverride(cfg, "gallery.max_file_size_mb", func(c *Config) *int { return &c.Gallery.MaxFileSizeMB }, strconv.Atoi)
	envOverride(cfg, "gallery.auto_meta_tags", func(c *Config) *bool { return &c.Gallery.AutoMetaTags }, strconv.ParseBool)
	provider := func(c *Config) *string { return &c.Tagger.ExecutionProvider }
	envOverride(cfg, "tagger.execution_provider", provider, parseProvider)
	// Deployments that still export the old MONBOORU_TAGGER_USE_CUDA keep
	// the GPU.
	if raw := os.Getenv("MONBOORU_TAGGER_USE_CUDA"); raw != "" && os.Getenv("MONBOORU_TAGGER_EXECUTION_PROVIDER") == "" {
		if useCUDA, err := strconv.ParseBool(raw); err != nil {
			logx.Warnf("config: ignoring MONBOORU_TAGGER_USE_CUDA=%q: %v", raw, err)
		} else if useCUDA {
			pinEnv(cfg, "tagger.execution_provider", "MONBOORU_TAGGER_USE_CUDA", provider, "cuda")
		}
	}
	envOverride(cfg, "auth.enable_password", func(c *Config) *bool { return &c.Auth.EnablePassword }, strconv.ParseBool)
	envOverride(cfg, "auth.password_hash", func(c *Config) *string { return &c.Auth.PasswordHash }, parseString)
	envOverride(cfg, "auth.session_lifetime_days", func(c *Config) *int { return &c.Auth.SessionLifetimeDays }, strconv.Atoi)
	envOverride(cfg, "monloader.api_url", func(c *Config) *string { return &c.Monloader.APIURL }, parseString)
	envOverride(cfg, "monloader.api_token", func(c *Config) *string { return &c.Monloader.APIToken }, parseString)
	envOverride(cfg, "log.level", func(c *Config) *string { return &c.Log.Level }, parseString)
}

// MaxPageSize stays well under SQLite's 32766-variable limit: a page binds
// one SQL variable per row.
const MaxPageSize = 1000

const (
	defaultScheduleTime        = "01:00"
	defaultExecutionProvider   = "cpu"
	defaultPageSize            = 40
	defaultThumbnailFit        = "natural"
	defaultSessionLifetimeDays = 7
	defaultTaggerParallel      = 4
	defaultIdleReleaseMinutes  = 15
	defaultMinHitFraction      = 0.05
	defaultRelationsDistance   = 4
)

func validate(cfg *Config) error {
	if cfg.Server.BindAddress == "" {
		return fmt.Errorf("server.bind_address must not be empty")
	}
	if !strings.Contains(cfg.Server.BindAddress, ":") {
		return fmt.Errorf("server.bind_address %q is not a valid host:port", cfg.Server.BindAddress)
	}
	if cfg.Auth.EnablePassword {
		h := strings.TrimSpace(cfg.Auth.PasswordHash)
		// An empty hash would let the password-update handler skip its
		// current-password check.
		if h == "" {
			return fmt.Errorf("auth.enable_password is true but auth.password_hash is empty - " +
				"run `monbooru -hash-password 'your-password'` and paste the result into monbooru.toml")
		}
		if !strings.HasPrefix(h, "$2a$") && !strings.HasPrefix(h, "$2b$") && !strings.HasPrefix(h, "$2y$") {
			return fmt.Errorf("auth.password_hash does not look like a bcrypt hash - " +
				"run `monbooru -hash-password 'your-password'` and paste the result into monbooru.toml")
		}
	}
	if len(cfg.Galleries) == 0 {
		return fmt.Errorf("at least one gallery must be configured")
	}
	if cfg.Paths.DataPath == "" {
		return fmt.Errorf("paths.data_path must not be empty")
	}
	// Absolute from load on, as the next save writes them: a later start
	// from another folder opens the same paths.
	var err error
	if cfg.Paths.DataPath, err = filepath.Abs(cfg.Paths.DataPath); err != nil {
		return fmt.Errorf("paths.data_path: %w", err)
	}
	if cfg.Paths.ModelPath != "" {
		if cfg.Paths.ModelPath, err = filepath.Abs(cfg.Paths.ModelPath); err != nil {
			return fmt.Errorf("paths.model_path: %w", err)
		}
	}
	seen := map[string]bool{}
	for i := range cfg.Galleries {
		g := &cfg.Galleries[i]
		if err := ValidateGalleryName(g.Name); err != nil {
			return fmt.Errorf("invalid gallery: %w", err)
		}
		if seen[g.Name] {
			return fmt.Errorf("duplicate gallery name %q", g.Name)
		}
		seen[g.Name] = true
		if g.GalleryPath == "" {
			return fmt.Errorf("gallery %q has an empty gallery_path", g.Name)
		}
		// Downstream compares against paths the filesystem returns,
		// absolute, cleaned and native; "C:/pics" would never match on
		// Windows.
		if g.GalleryPath, err = filepath.Abs(g.GalleryPath); err != nil {
			return fmt.Errorf("gallery %q: %w", g.Name, err)
		}
	}
	if cfg.FindGallery(cfg.DefaultGallery) == nil {
		cfg.DefaultGallery = cfg.Galleries[0].Name
	}
	if cfg.Schedule.Time == "" {
		cfg.Schedule.Time = defaultScheduleTime
	} else if err := ValidateScheduleTime(cfg.Schedule.Time); err != nil {
		logx.Warnf("config: %v; using %s", err, defaultScheduleTime)
		cfg.Schedule.Time = defaultScheduleTime
	}
	cfg.Schedule.Mode = cfg.Schedule.EffectiveMode()
	if cfg.Tagger.ExecutionProvider == "" {
		cfg.Tagger.ExecutionProvider = defaultExecutionProvider
	} else if !IsValidExecutionProvider(cfg.Tagger.ExecutionProvider) {
		logx.Warnf("config: tagger.execution_provider %q must be one of %v; using %s", cfg.Tagger.ExecutionProvider, ValidExecutionProviders, defaultExecutionProvider)
		cfg.Tagger.ExecutionProvider = defaultExecutionProvider
	}
	// The API divides by it, so zero would panic.
	if cfg.UI.PageSize <= 0 {
		cfg.UI.PageSize = defaultPageSize
	} else if cfg.UI.PageSize > MaxPageSize {
		cfg.UI.PageSize = MaxPageSize
	}
	if cfg.UI.ThumbnailFit != "square" {
		cfg.UI.ThumbnailFit = defaultThumbnailFit
	}
	// Zero would reach net/http as MaxAge 0, which means a session cookie.
	if cfg.Auth.SessionLifetimeDays <= 0 {
		cfg.Auth.SessionLifetimeDays = defaultSessionLifetimeDays
	}
	if cfg.Relations.DefaultDistance < 0 || cfg.Relations.DefaultDistance > MaxPhashDistance {
		cfg.Relations.DefaultDistance = defaultRelationsDistance
	}
	if cfg.Tagger.Parallel < 1 {
		cfg.Tagger.Parallel = defaultTaggerParallel
	}
	if cfg.Tagger.IdleReleaseAfterMinutes < 0 {
		cfg.Tagger.IdleReleaseAfterMinutes = defaultIdleReleaseMinutes
	}
	if !isFraction(cfg.Tagger.Aggregation.MinHitFraction) {
		cfg.Tagger.Aggregation.MinHitFraction = defaultMinHitFraction
	}
	normalizeTaggerThresholds(cfg)
	dropInvalidPlugins(cfg)
	dropInvalidCORSOrigins(cfg)
	dropInvalidIgnorePatterns(cfg)
	dropInvalidScheduleGalleries(cfg)
	return nil
}

// Written as a range test so NaN fails it too.
func isFraction(v float64) bool { return v >= 0 && v <= 1 }

func normalizeTaggerThresholds(cfg *Config) {
	for i := range cfg.Tagger.Taggers {
		t := &cfg.Tagger.Taggers[i]
		if !isFraction(t.ConfidenceThreshold) {
			logx.Warnf("config: tagger %q confidence_threshold %v is not between 0 and 1, using %v", t.Name, t.ConfidenceThreshold, DefaultConfidenceThreshold)
			t.ConfidenceThreshold = DefaultConfidenceThreshold
		}
		for cat, v := range t.CategoryThresholds {
			if !isFraction(v) {
				logx.Warnf("config: dropping tagger %q category_thresholds entry %q: %v is not between 0 and 1", t.Name, cat, v)
				delete(t.CategoryThresholds, cat)
			}
		}
	}
}

func dropInvalidScheduleGalleries(cfg *Config) {
	for action := range cfg.Schedule.Galleries {
		if !slices.Contains(ScheduleActions, action) {
			logx.Warnf("config: dropping schedule.galleries entry %q: not a schedule action", action)
			delete(cfg.Schedule.Galleries, action)
		}
	}
}

// Browsers send an origin with no path and no trailing slash, so anything
// else could never match.
func dropInvalidCORSOrigins(cfg *Config) {
	for i, o := range cfg.Server.CORSOrigins {
		cfg.Server.CORSOrigins[i] = strings.TrimRight(strings.TrimSpace(o), "/")
	}
	cfg.Server.CORSOrigins = slices.DeleteFunc(cfg.Server.CORSOrigins, func(o string) bool {
		if o == "*" || isOrigin(o) {
			return false
		}
		logx.Warnf("config: dropping server.cors_origins entry %q - want a bare scheme://host[:port]", o)
		return true
	})
}

func dropInvalidIgnorePatterns(cfg *Config) {
	valid := make([]string, 0, len(cfg.Gallery.Ignore))
	for _, p := range cfg.Gallery.Ignore {
		if _, err := normalizeIgnorePattern(p); err != nil {
			logx.Warnf("config: dropping gallery.ignore entry: %v", err)
			continue
		}
		valid = append(valid, p)
	}
	cfg.Gallery.Ignore, _ = NormalizeIgnore(valid)
}

func NormalizeIgnore(lines []string) ([]string, error) {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		p, err := normalizeIgnorePattern(l)
		if err != nil {
			return nil, err
		}
		if p != "" && !slices.ContainsFunc(out, func(k string) bool { return strings.EqualFold(k, p) }) {
			out = append(out, p)
		}
	}
	return out, nil
}

func normalizeIgnorePattern(p string) (string, error) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	if _, err := path.Match(p, ""); err != nil {
		return "", fmt.Errorf("%q is not a valid pattern", p)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if strings.Trim(seg, "*?") != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q would leave out every file", p)
}

// Any scheme, so a browser extension's own origin qualifies.
func isOrigin(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != "" &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

func dropInvalidPlugins(cfg *Config) {
	cfg.Plugins = slices.DeleteFunc(cfg.Plugins, func(p PluginConfig) bool {
		if err := ValidatePluginName(p.Name); err != nil {
			logx.Warnf("config: dropping a [[plugin]] block: %v", err)
			return true
		}
		return false
	})
	for i := range cfg.Plugins {
		p := &cfg.Plugins[i]
		p.Buttons = slices.DeleteFunc(p.Buttons, func(b PluginButton) bool {
			if err := ValidatePluginButton(b); err != nil {
				logx.Warnf("config: dropping a button on plugin %q: %v", p.Name, err)
				return true
			}
			return false
		})
	}
}
