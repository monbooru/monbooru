package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestSettingsTaggerPost_SavesTheAcceptedFormAndRefusesABadProvider(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/settings/tagger", url.Values{
		"execution_provider":         {"cpu"},
		"parallel":                   {"3"},
		"idle_release_after_minutes": {"7"},
	})
	mustCode(t, w, http.StatusOK)
	cfg := srv.cfgSnapshot()
	if cfg.Tagger.ExecutionProvider != "cpu" {
		t.Errorf("execution_provider = %q, want cpu", cfg.Tagger.ExecutionProvider)
	}
	if cfg.Tagger.Parallel != 3 {
		t.Errorf("parallel = %d, want 3", cfg.Tagger.Parallel)
	}
	if cfg.Tagger.IdleReleaseAfterMinutes != 7 {
		t.Errorf("idle_release_after_minutes = %d, want 7", cfg.Tagger.IdleReleaseAfterMinutes)
	}
	toml, err := os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantIn(t, string(toml), "parallel = 3")

	w = postForm(t, srv, "/settings/tagger", url.Values{
		"execution_provider": {"quantum"},
		"parallel":           {"9"},
	})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Invalid execution provider")
	if got := srv.cfgSnapshot().Tagger.Parallel; got != 3 {
		t.Errorf("parallel = %d, want the refused form to have changed nothing", got)
	}
}

func TestSettingsTaggerDeletePost_RemovesTheFolderAndTheEntry(t *testing.T) {
	srv, modelPath := newModelServer(t,
		config.TaggerInstance{Name: "wd-swinv2", Enabled: false, ModelFile: "model.onnx", TagsFile: "tags.csv"},
		config.TaggerInstance{Name: "joytag", Enabled: false},
	)
	dir := filepath.Join(modelPath, "wd-swinv2")
	mustMkdirAll(t, filepath.Join(dir, "nested"), 0o755)
	mustWriteFile(t, filepath.Join(dir, "model.onnx"), []byte("weights"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "nested", "tags.csv"), []byte("a,b"), 0o644)
	keep := filepath.Join(modelPath, "joytag")
	mustMkdirAll(t, keep, 0o755)

	w := postForm(t, srv, "/settings/tagger/wd-swinv2/delete", url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Header().Get("HX-Trigger"), "Tagger wd-swinv2 deleted.")
	if w.Header().Get("HX-Refresh") != "true" {
		t.Error("the delete no longer refreshes the page")
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the model folder survived, stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the delete took a neighbouring tagger's folder: %v", err)
	}
	names := taggerNames(srv)
	if len(names) != 1 || names[0] != "joytag" {
		t.Errorf("configured taggers = %v, want [joytag]", names)
	}
	toml, err := os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(toml), "wd-swinv2") {
		t.Error("the entry is still in the TOML")
	}
}

func TestSettingsTaggerMappingPost_RefusesANonLevelRatingTarget(t *testing.T) {
	srv, modelPath := newModelServer(t)
	mustMkdirAll(t, filepath.Join(modelPath, "camie"), 0o755)
	dispatch := filepath.Join(modelPath, "camie", "dispatch.json")
	rule := func(name string) *httptest.ResponseRecorder {
		return postForm(t, srv, "/settings/tagger/camie/mapping", url.Values{
			"rule_source": {"rating_safe"}, "rule_category": {"rating"}, "rule_name": {name},
		})
	}

	w := rule("safe")
	wantIn(t, w.Body.String(), "general, sensitive, questionable or explicit, not safe")
	if _, err := os.Stat(dispatch); !os.IsNotExist(err) {
		t.Errorf("the refused rule was saved: %v", err)
	}

	rule("general")
	saved, err := os.ReadFile(dispatch)
	if err != nil || !strings.Contains(string(saved), `"rating_safe"`) {
		t.Errorf("dispatch.json = %q (%v), want the level rule saved", saved, err)
	}
}

func TestSettingsTaggerMappingPost_RefusesOverABrokenDispatchFile(t *testing.T) {
	srv, modelPath := newModelServer(t)
	dir := installTagger(t, modelPath, "wd-swinv2", "tag_id,name,category\n1,engrish,0\n")
	broken := `{"version":1,"rules":[{"source":"engrish","category":""},]}`
	dispatch := filepath.Join(dir, "dispatch.json")
	mustWriteFile(t, dispatch, []byte(broken), 0o644)

	w := postForm(t, srv, "/settings/tagger/wd-swinv2/mapping", url.Values{
		"rule_source": {"monochrome"}, "rule_category": {"meta"},
	})
	wantIn(t, w.Body.String(), "Could not save dispatch.json:")
	if got, _ := os.ReadFile(dispatch); string(got) != broken {
		t.Errorf("dispatch.json = %q, want it untouched", got)
	}

	_, body := getBody(t, srv, "/settings/tagger/wd-swinv2/config")
	wantIn(t, body, "dispatch.json cannot be read")
}

func TestSettingsTaggerMappingPost_RefreshesTheExportAndTheRow(t *testing.T) {
	srv, modelPath := newModelServer(t)
	installTagger(t, modelPath, "wd-swinv2", "tag_id,name,category\n1,engrish,0\n2,monochrome,0\n")
	oob := func(body, id string) string {
		t.Helper()
		i := strings.Index(body, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("no %s in the reply:\n%s", id, body)
		}
		start := strings.LastIndex(body[:i], "<")
		tag := body[start : start+strings.Index(body[start:], ">")]
		wantIn(t, tag, `hx-swap-oob="true"`)
		return body[start:]
	}

	w := postForm(t, srv, "/settings/tagger/wd-swinv2/mapping", url.Values{
		"rule_source": {"monochrome"}, "rule_category": {"meta"},
	})
	body := w.Body.String()
	export := oob(body, "tc-export-wd-swinv2")
	wantIn(t, export, "1 changes vs stock")
	wantIn(t, export[strings.Index(export, `id="tc-export-json-wd-swinv2"`):], "monochrome")
	wantIn(t, oob(body, "tagger-config-summary-wd-swinv2"), ", 1 rule<")
	wantIn(t, oob(body, "tagger-reset-wd-swinv2"), ">Reset</button>")

	w = postForm(t, srv, "/settings/tagger/wd-swinv2/mapping", url.Values{
		"rule_source": {"monochrome"}, "rule_reset": {"1"},
	})
	body = w.Body.String()
	wantIn(t, oob(body, "tc-export-wd-swinv2"), "0 changes vs stock")
	wantNotIn(t, oob(body, "tagger-config-summary-wd-swinv2"), "rule")
	wantNotIn(t, oob(body, "tagger-reset-wd-swinv2"), "Reset")
}

func TestSettingsShowsAMalformedTaggerFolderWithoutActions(t *testing.T) {
	srv, modelPath := newModelServer(t)
	installTagger(t, modelPath, "wd-v1.4-moat", "tag_id,name,category\n1,engrish,0\n")

	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, "folder name must match [A-Za-z0-9_-]&#43;; rename the folder")
	for _, action := range []string{"/settings/tagger/wd-v1.4-moat/", `id="tagger-config-wd-v1.4-moat"`} {
		wantNotIn(t, body, action)
	}
}

func taggerEnabled(srv *Server, name string) bool {
	srv.cfgMu.Lock()
	defer srv.cfgMu.Unlock()
	for _, t := range srv.cfg.Tagger.Taggers {
		if t.Name == name {
			return t.Enabled
		}
	}
	return false
}

func TestSettingsKeepsTaggersEnabledWhileTheModelPathIsMissing(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Paths.ModelPath = filepath.Join(t.TempDir(), "unmounted")
		c.Tagger.Taggers = []config.TaggerInstance{{Name: "wd-swinv2", Enabled: true}}
	})
	getBody(t, srv, "/settings")
	if !taggerEnabled(srv, "wd-swinv2") {
		t.Error("a missing model_path disabled the tagger")
	}
}

func TestSettingsDisablesATaggerWhoseFilesAreGone(t *testing.T) {
	srv, modelPath := newModelServer(t, config.TaggerInstance{Name: "wd-swinv2", Enabled: true}, config.TaggerInstance{Name: "joytag", Enabled: true})
	installTagger(t, modelPath, "joytag", "")
	getBody(t, srv, "/settings")
	if taggerEnabled(srv, "wd-swinv2") || taggerEnabled(srv, "joytag") {
		t.Error("a tagger whose folder or label file is gone stayed enabled")
	}
}

func TestSettingsAutoEnablesAndDisablesInOneVisit(t *testing.T) {
	srv, modelPath := newModelServer(t, config.TaggerInstance{Name: "wd-swinv2", Enabled: true})
	installTagger(t, modelPath, "fresh", "tag_id,name,category,count\n0,red,0,1\n")

	lib := filepath.Join(t.TempDir(), "libonnxruntime.so")
	mustWriteFile(t, lib, nil, 0o644)
	t.Setenv("ORT_LIB_PATH", lib)

	getBody(t, srv, "/settings")

	if taggerEnabled(srv, "wd-swinv2") || !taggerEnabled(srv, "fresh") {
		t.Errorf("taggers = %+v, want wd-swinv2 disabled and fresh enabled", srv.cfgSnapshot().Tagger.Taggers)
	}
	saved, err := config.LoadWithDefaults(srv.configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, ti := range saved.Tagger.Taggers {
		onDisk = append(onDisk, fmt.Sprintf("%s=%t", ti.Name, ti.Enabled))
	}
	if got := strings.Join(onDisk, " "); got != "wd-swinv2=false fresh=true" {
		t.Errorf("saved taggers = %q", got)
	}
}

func TestSettingsTaggerDeletePost_RefusesAnEnabledTagger(t *testing.T) {
	srv, modelPath := newModelServer(t, config.TaggerInstance{Name: "wd-swinv2", Enabled: true})
	dir := installTagger(t, modelPath, "wd-swinv2", "")

	w := postForm(t, srv, "/settings/tagger/wd-swinv2/delete", url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Disable tagger wd-swinv2 before deleting it.")

	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err != nil {
		t.Errorf("the refused delete wiped the folder anyway: %v", err)
	}
	if names := taggerNames(srv); len(names) != 1 {
		t.Errorf("configured taggers = %v, want the entry kept", names)
	}
}

func TestSettingsTaggerDeletePost_WaitsForARunningJob(t *testing.T) {
	srv, modelPath := newModelServer(t, config.TaggerInstance{Name: "wd-swinv2"})
	dir := installTagger(t, modelPath, "wd-swinv2", "")
	held, err := srv.jobs.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	w := postForm(t, srv, "/settings/tagger/wd-swinv2/delete", url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "A job is running")
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err != nil {
		t.Errorf("the delete wiped the folder under a running job: %v", err)
	}
	if names := taggerNames(srv); len(names) != 1 {
		t.Errorf("configured taggers = %v, want the entry kept", names)
	}

	w = postForm(t, srv, "/settings/tagger", url.Values{"execution_provider": {"cuda"}})
	wantIn(t, w.Body.String(), "A job is running")
	if got := srv.executionProvider(); got != "cpu" {
		t.Errorf("provider = %q under a running job, want cpu kept", got)
	}
}

func taggerNames(srv *Server) []string {
	var out []string
	for _, t := range srv.cfgSnapshot().Tagger.Taggers {
		out = append(out, t.Name)
	}
	return out
}

func TestSettingsOffersTheFilesACopiedRegistryFolderLacks(t *testing.T) {
	srv, modelPath := newModelServer(t)
	src := filepath.Join("..", "registry", "plugins", "taggers", "camie-v2")
	dir := filepath.Join(modelPath, "camie-v2")
	mustMkdirAll(t, dir, 0o755)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(dir, e.Name()), data, 0o644)
	}

	_, body := getBody(t, srv, "/settings")
	wantIn(t, body, `data-tagger-name="camie-v2"`)
	wantIn(t, body, "Anime tagger with artist/copyright recognition")
	wantIn(t, body, "camie-tagger-v2.onnx")
	wantNotIn(t, body, "taggers/camie-v2/profile_default.json")

	mustWriteFile(t, filepath.Join(dir, "camie-tagger-v2.onnx"), []byte("weights"), 0o644)
	mustWriteFile(t, filepath.Join(dir, "camie-tagger-v2-metadata.json"), []byte(`{}`), 0o644)
	_, body = getBody(t, srv, "/settings")
	wantNotIn(t, body, `data-tagger-name="camie-v2"`)
}

func newModelServer(t *testing.T, taggers ...config.TaggerInstance) (*Server, string) {
	t.Helper()
	var modelPath string
	srv := newTestServerWith(t, false, func(c *config.Config) {
		modelPath = filepath.Join(t.TempDir(), "models")
		c.Paths.ModelPath = modelPath
		if len(taggers) > 0 {
			c.Tagger.Taggers = taggers
		}
	})
	return srv, modelPath
}

func installTagger(t *testing.T, modelPath, name, csv string) string {
	t.Helper()
	dir := filepath.Join(modelPath, name)
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "model.onnx"), []byte("weights"), 0o644)
	if csv != "" {
		mustWriteFile(t, filepath.Join(dir, "tags.csv"), []byte(csv), 0o644)
	}
	return dir
}
