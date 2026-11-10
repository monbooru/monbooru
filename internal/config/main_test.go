package config

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "MONBOORU_") {
			os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
