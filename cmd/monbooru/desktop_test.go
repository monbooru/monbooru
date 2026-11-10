package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/desktop"
)

func TestDesktopSeedReplacesTheContainerPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the seed asserts POSIX home layout")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	layout, err := desktop.Resolve(appName, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cfg := config.Default()
	desktopSeed(layout)(cfg)

	if cfg.Paths.DataPath != layout.DataDir {
		t.Errorf("data_path = %q, want %q", cfg.Paths.DataPath, layout.DataDir)
	}
	if want := filepath.Join(layout.DataDir, "models"); cfg.Paths.ModelPath != want {
		t.Errorf("model_path = %q, want %q", cfg.Paths.ModelPath, want)
	}
	want := filepath.Join(home, "Pictures", appName)
	if cfg.Galleries[0].GalleryPath != want {
		t.Fatalf("gallery_path = %q, want %q", cfg.Galleries[0].GalleryPath, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("gallery folder not created: %v", err)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
}

func TestHostSeedPutsEverythingBesideTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	hostSeed(filepath.Join(dir, "monbooru.toml"))(cfg)

	if want := filepath.Join(dir, "data"); cfg.Paths.DataPath != want {
		t.Errorf("data_path = %q, want %q", cfg.Paths.DataPath, want)
	}
	if want := filepath.Join(dir, "data", "models"); cfg.Paths.ModelPath != want {
		t.Errorf("model_path = %q, want %q", cfg.Paths.ModelPath, want)
	}
	want := filepath.Join(dir, "gallery")
	if cfg.Galleries[0].GalleryPath != want {
		t.Fatalf("gallery_path = %q, want %q", cfg.Galleries[0].GalleryPath, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("gallery folder not created: %v", err)
	}
}

func TestHostSeedResolvesARelativeConfigPath(t *testing.T) {
	cfg := config.Default()
	hostSeed("monbooru.toml")(cfg)
	t.Cleanup(func() { _ = os.RemoveAll("gallery") })

	if !filepath.IsAbs(cfg.Paths.DataPath) {
		t.Errorf("data_path = %q, want an absolute path", cfg.Paths.DataPath)
	}
}

func TestInContainerReadsTheImagesOwnVariable(t *testing.T) {
	t.Setenv("MONBOORU_CONTAINER", "")
	if inContainer() {
		t.Error("inContainer = true with the variable unset")
	}
	t.Setenv("MONBOORU_CONTAINER", "1")
	if !inContainer() {
		t.Error("inContainer = false with the variable set")
	}
}

func TestDesktopSeedLeavesThePortableLayoutAlone(t *testing.T) {
	dir := t.TempDir()
	layout := desktop.Layout{
		ConfigPath: filepath.Join(dir, "monbooru.toml"),
		ConfigDir:  dir,
		DataDir:    filepath.Join(dir, "data"),
		Portable:   true,
	}
	cfg := config.Default()
	before := cfg.Paths.DataPath
	desktopSeed(layout)(cfg)

	if cfg.Paths.DataPath != before {
		t.Errorf("data_path = %q, want the config package's own %q", cfg.Paths.DataPath, before)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
	if cfg.Schedule.Mode != config.ScheduleAtTimeCatchup {
		t.Errorf("schedule.mode = %q, want the catchup mode", cfg.Schedule.Mode)
	}
}

func TestPortableGalleryCreatesTheFolderTheArchivePromises(t *testing.T) {
	dir := t.TempDir()
	layout := desktop.Layout{ConfigDir: dir, Portable: true}
	cfg := config.Default()
	cfg.Galleries[0].GalleryPath = filepath.Join(dir, "gallery")
	portableGallery(layout, cfg)

	if fi, err := os.Stat(cfg.Galleries[0].GalleryPath); err != nil || !fi.IsDir() {
		t.Errorf("gallery folder not created: %v", err)
	}
}

func TestPortableGalleryLeavesAPathElsewhereAlone(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "on-another-disk")
	layout := desktop.Layout{ConfigDir: dir, Portable: true}
	cfg := config.Default()
	cfg.Galleries[0].GalleryPath = elsewhere
	portableGallery(layout, cfg)

	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Errorf("created %s, want it left absent", elsewhere)
	}
}

func TestSeedGalleryDirFallsBackToTheDataDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on POSIX directory permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))

	layout := desktop.Layout{DataDir: filepath.Join(root, "data")}
	got := seedGalleryDir(layout)
	if want := filepath.Join(layout.DataDir, "gallery"); got != want {
		t.Fatalf("seedGalleryDir = %q, want %q", got, want)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Errorf("fallback folder not created: %v", err)
	}
}
