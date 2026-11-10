package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHealthAddr(t *testing.T) {
	const envKey = "MONBOORU_SERVER_BIND_ADDRESS"

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\nbind_address = \"127.0.0.1:9999\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		env    string
		config string
		want   string
	}{
		{"env wins over config", "127.0.0.1:8080", cfgPath, "127.0.0.1:8080"},
		{"env wildcard rewritten to loopback", "0.0.0.0:8080", "", "127.0.0.1:8080"},
		{"env ipv6 wildcard rewritten", "[::]:8080", "", "127.0.0.1:8080"},
		{"config fallback when env empty", "", cfgPath, "127.0.0.1:9999"},
		{"default when env and config empty", "", "", "127.0.0.1:8455"},
		{"missing config falls back to default", "", filepath.Join(dir, "nope.toml"), "127.0.0.1:8455"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.env)
			if got := resolveHealthAddr(tc.config); got != tc.want {
				t.Errorf("resolveHealthAddr(%q) with env %q = %q, want %q", tc.config, tc.env, got, tc.want)
			}
		})
	}
}
