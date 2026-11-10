package plugins

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
)

func installFolder(t *testing.T, dir, name, manifest string) string {
	t.Helper()
	folder := filepath.Join(dir, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "plugin.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return folder
}

func TestAPairedBlockTakesItsFoldersLaunchLine(t *testing.T) {
	dir := t.TempDir()
	folder := installFolder(t, dir, "dropped", "command = \"./run.sh\"\n")

	p, ok := FindEffective(EffectiveList([]config.PluginConfig{{Name: "dropped", PeerToken: "sek"}}, dir), "dropped")
	if !ok || !p.Installed || p.Launch.Command != filepath.Join(folder, "run.sh") || p.PeerToken != "sek" {
		t.Errorf("effective = %+v, ok %v", p, ok)
	}
}

func TestDiscoverySkipsWhatItCannotRun(t *testing.T) {
	dir := t.TempDir()
	installFolder(t, dir, "no-command", "args = [\"main.py\"]\n")
	installFolder(t, dir, "monloader", "command = \"./run.sh\"\n")

	list := EffectiveList(nil, dir)
	for _, name := range []string{"no-command", "monloader"} {
		if p, ok := FindEffective(list, name); !ok || p.Installed || p.Launch.Command != "" || p.Refused == "" {
			t.Errorf("%s = %+v (found %v), want a refused entry with nothing to launch", name, p, ok)
		}
	}
}

func TestBasePrefersTheOperatorOverride(t *testing.T) {
	tok, _ := apitoken.New("monedit (paired)", config.AllScopes)
	tok.Paired, tok.PeerURL = "monedit", "http://192.0.2.9:7301"
	c := &config.Config{}
	c.Auth.Tokens = []apitoken.Token{tok}

	p := config.PluginConfig{Name: "monedit", PeerToken: "sek"}
	if got := Base(c, p); got != "http://192.0.2.9:7301" {
		t.Errorf("pairing-derived base = %q", got)
	}
	p.APIURL = "http://nas:7301/"
	if got := Base(c, p); got != "http://nas:7301" {
		t.Errorf("override base = %q", got)
	}
	p.Paused = true
	if got := Base(c, p); got != "" {
		t.Errorf("paused base = %q, want empty", got)
	}
}

func TestCallbackURLPrefersTheListener(t *testing.T) {
	for _, tc := range []struct{ bind, want string }{
		{"0.0.0.0:8080", "http://127.0.0.1:8080"},
		{"127.0.0.1:9999", "http://127.0.0.1:9999"},
		{"[::]:8080", "http://127.0.0.1:8080"},
		{"192.168.1.5:8080", "http://192.168.1.5:8080"},
		{"", "https://booru.example.com"},
	} {
		if got := CallbackURL(tc.bind, "https://booru.example.com"); got != tc.want {
			t.Errorf("bind %q -> %q, want %q", tc.bind, got, tc.want)
		}
	}
}

func TestRefreshMarksAnUnreachablePeerDown(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"version":"0.3.1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()

	peers := NewPeers(func() string { return "" }, make(chan struct{}))
	peers.Refresh(t.Context(), &config.Config{Plugins: []config.PluginConfig{
		{Name: "up", PeerToken: "sek", APIURL: up.URL},
		{Name: "down", PeerToken: "sek", APIURL: "http://127.0.0.1:1"},
	}})

	if pr := peers.ProbeSeed("up"); pr.Conn != "ok" || pr.Version != "0.3.1" {
		t.Errorf("reachable peer probe = %+v", pr)
	}
	if pr := peers.ProbeSeed("down"); pr.Conn != "down" {
		t.Errorf("unreachable peer probe = %+v", pr)
	}
}
