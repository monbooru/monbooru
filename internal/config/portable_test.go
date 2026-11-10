package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func portableDir(t *testing.T, marker string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestPortableEmptyMarkerSeedsBesideTheProgram(t *testing.T) {
	dir, path := portableDir(t, "\n  \n")

	cfg, err := LoadPortable(path, nil)
	if err != nil {
		t.Fatalf("LoadPortable: %v", err)
	}
	if want := filepath.Join(dir, "data"); cfg.Paths.DataPath != want {
		t.Errorf("DataPath = %q, want %q", cfg.Paths.DataPath, want)
	}
	if want := filepath.Join(dir, "data", "models"); cfg.Paths.ModelPath != want {
		t.Errorf("ModelPath = %q, want %q", cfg.Paths.ModelPath, want)
	}
	if want := filepath.Join(dir, "gallery"); cfg.Galleries[0].GalleryPath != want {
		t.Errorf("GalleryPath = %q, want %q", cfg.Galleries[0].GalleryPath, want)
	}
	if cfg.SetupDone {
		t.Error("SetupDone = true, want false on a seeded marker")
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data_path = "data"`, `gallery_path = "gallery"`} {
		if !strings.Contains(string(written), want) {
			t.Errorf("seeded config missing %s:\n%s", want, written)
		}
	}
}

func TestPortableFileTakesTheLayoutForWhatItOmits(t *testing.T) {
	dir, path := portableDir(t, "[server]\nbind_address = \"127.0.0.1:9000\"\n")

	cfg, err := LoadPortable(path, nil)
	if err != nil {
		t.Fatalf("LoadPortable: %v", err)
	}
	if cfg.Server.BindAddress != "127.0.0.1:9000" {
		t.Errorf("BindAddress = %q, want the one the file names", cfg.Server.BindAddress)
	}
	if want := filepath.Join(dir, "data"); cfg.Paths.DataPath != want {
		t.Errorf("DataPath = %q, want %q (not the container mount)", cfg.Paths.DataPath, want)
	}
	if want := filepath.Join(dir, "gallery"); cfg.Galleries[0].GalleryPath != want {
		t.Errorf("GalleryPath = %q, want %q", cfg.Galleries[0].GalleryPath, want)
	}
	if !cfg.SetupDone {
		t.Error("SetupDone = false, want true for a config already on disk")
	}
}

func TestPortableResolvesTheRelativePathsItNames(t *testing.T) {
	dir, path := portableDir(t, `
[[galleries]]
name = "default"
gallery_path = "pics"

[paths]
data_path = "state"
model_path = "state/models"
`)

	cfg, err := LoadPortable(path, nil)
	if err != nil {
		t.Fatalf("LoadPortable: %v", err)
	}
	if want := filepath.Join(dir, "state"); cfg.Paths.DataPath != want {
		t.Errorf("DataPath = %q, want %q", cfg.Paths.DataPath, want)
	}
	if want := filepath.Join(dir, "pics"); cfg.Galleries[0].GalleryPath != want {
		t.Errorf("GalleryPath = %q, want %q", cfg.Galleries[0].GalleryPath, want)
	}
	if want := filepath.Join(dir, "state", "default", "monbooru.db"); cfg.Galleries[0].DBPath != want {
		t.Errorf("DBPath = %q, want %q", cfg.Galleries[0].DBPath, want)
	}
}

func TestPortableSaveKeepsThePathsUnderTheFolderRelative(t *testing.T) {
	dir, path := portableDir(t, "")

	cfg, err := LoadPortable(path, nil)
	if err != nil {
		t.Fatalf("LoadPortable: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "archive")
	cfg.Galleries = append(cfg.Galleries, Gallery{Name: "stock", GalleryPath: outside})
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data_path = "data"`, `gallery_path = "gallery"`, `gallery_path = "` + outside + `"`} {
		if !strings.Contains(string(written), want) {
			t.Errorf("saved config missing %s:\n%s", want, written)
		}
	}
	if want := filepath.Join(dir, "data"); cfg.Paths.DataPath != want {
		t.Errorf("Save rewrote the live config: DataPath = %q, want %q", cfg.Paths.DataPath, want)
	}

	again, err := LoadPortable(path, nil)
	if err != nil {
		t.Fatalf("LoadPortable again: %v", err)
	}
	if again.Paths.DataPath != cfg.Paths.DataPath {
		t.Errorf("round trip DataPath = %q, want %q", again.Paths.DataPath, cfg.Paths.DataPath)
	}
	if again.Galleries[1].GalleryPath != outside {
		t.Errorf("round trip stock gallery = %q, want %q", again.Galleries[1].GalleryPath, outside)
	}
}

func TestEmptyConfigIsStillAnErrorWithoutPortable(t *testing.T) {
	_, path := portableDir(t, "\n")
	if _, err := LoadWithDefaults(path, nil); err == nil {
		t.Error("LoadWithDefaults on an empty file = nil, want an error")
	}
}

func TestNonPortableLoadMakesRelativePathsAbsolute(t *testing.T) {
	dir, path := portableDir(t, `
[[galleries]]
name = "default"
gallery_path = "pics"

[paths]
data_path = "state"
model_path = "state/models"
`)
	t.Chdir(dir)
	cfg, err := LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatalf("LoadWithDefaults: %v", err)
	}
	for _, c := range []struct{ what, got, want string }{
		{"GalleryPath", cfg.Galleries[0].GalleryPath, filepath.Join(dir, "pics")},
		{"DataPath", cfg.Paths.DataPath, filepath.Join(dir, "state")},
		{"ModelPath", cfg.Paths.ModelPath, filepath.Join(dir, "state", "models")},
		{"DBPath", cfg.Galleries[0].DBPath, filepath.Join(dir, "state", "default", "monbooru.db")},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
		}
	}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	again, err := LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Galleries[0].GalleryPath != cfg.Galleries[0].GalleryPath || again.Paths.DataPath != cfg.Paths.DataPath {
		t.Errorf("saved and loaded from another folder: %q and %q", again.Galleries[0].GalleryPath, again.Paths.DataPath)
	}
}

func TestARelativeDataPathFromTheEnvironmentDerivesAbsolutePaths(t *testing.T) {
	dir, path := portableDir(t, `
[[galleries]]
name = "default"
gallery_path = "/srv/pics"
`)
	t.Chdir(dir)
	t.Setenv("MONBOORU_PATHS_DATA_PATH", "state")
	cfg, err := LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatalf("LoadWithDefaults: %v", err)
	}
	if want := filepath.Join(dir, "state", "default", "monbooru.db"); cfg.Galleries[0].DBPath != want {
		t.Errorf("DBPath = %q, want %q", cfg.Galleries[0].DBPath, want)
	}
}
