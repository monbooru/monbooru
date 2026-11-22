package registry_test

import (
	"io/fs"
	"path"
	"testing"

	"github.com/monbooru/monbooru/internal/registry"
)

func TestBuiltinsAreEmbedded(t *testing.T) {
	for _, file := range []string{"theme.css", "logo.png", "favicon.png", "mascot-hello.png", "mascot-lost.png", "mascot-sleep.png", "mascot-panic.png", "mascot-sparkle-head.png"} {
		if _, err := fs.ReadFile(registry.FS, path.Join(registry.LightThemeDir, file)); err != nil {
			t.Errorf("light theme %s: %v", file, err)
		}
	}
	for _, dir := range []string{registry.LegacyDarkThemeDir, registry.LegacyLightThemeDir} {
		for _, file := range []string{"theme.css", "logo.png", "favicon.png"} {
			if _, err := fs.ReadFile(registry.FS, path.Join(dir, file)); err != nil {
				t.Errorf("%s %s: %v", dir, file, err)
			}
		}
	}
}
