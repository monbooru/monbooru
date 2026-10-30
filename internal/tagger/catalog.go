package tagger

import (
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/registry"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

const (
	catalogSchemaVersion = 1

	modelManifest = "model.json"
	stockProfile  = "profile_default.json"
	stockDispatch = "dispatch_default.json"
)

// Monbooru never fetches these URLs itself: Settings only renders the
// commands, so the app makes no outbound request.
type CatalogEntry struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Files       []CatalogFile `json:"files"`
	// Gated entries need a Hugging Face login; the snippets expect
	// HF_TOKEN in the shell.
	Gated             bool               `json:"gated,omitempty"`
	DefaultThreshold  float64            `json:"default_threshold,omitempty"`
	DefaultThresholds map[string]float64 `json:"default_thresholds,omitempty"`
	DefaultTopK       map[string]int     `json:"default_top_k,omitempty"`
}

type CatalogFile struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
}

type modelDoc struct {
	Version int `json:"version"`
	CatalogEntry
}

func Builtin(name string) bool { return slices.Contains(registry.Taggers, name) }

func stockFile(modelPath, taggerName, file string) ([]byte, string, error) {
	if Builtin(taggerName) {
		data, err := registry.FS.ReadFile(path.Join(registry.TaggersDir, taggerName, file))
		return data, "embedded " + taggerName, err
	}
	p := filepath.Join(modelPath, taggerName, file)
	data, err := os.ReadFile(p)
	return data, p, err
}

func loadModelDoc(modelPath, taggerName string) (CatalogEntry, bool) {
	data, source, err := stockFile(modelPath, taggerName, modelManifest)
	if err != nil {
		return CatalogEntry{}, false
	}
	var doc modelDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		logx.Warnf("tagger: %s: parse failed: %v", source, err)
		return CatalogEntry{}, false
	}
	if doc.Version != catalogSchemaVersion {
		logx.Warnf("tagger: %s: schema version %d unsupported (want %d)", source, doc.Version, catalogSchemaVersion)
		return CatalogEntry{}, false
	}
	doc.Name = taggerName
	return doc.CatalogEntry, true
}

func LoadCatalog(modelPath string) []CatalogEntry {
	var out []CatalogEntry
	for _, name := range registry.Taggers {
		if e, ok := loadModelDoc(modelPath, name); ok {
			out = append(out, e)
		}
	}
	if dirs, err := os.ReadDir(modelPath); err == nil {
		for _, d := range dirs {
			name := d.Name()
			if Builtin(name) || ValidateTaggerName(name) != nil {
				continue
			}
			if e, ok := loadModelDoc(modelPath, name); ok {
				out = append(out, e)
			}
		}
	}
	return out
}

func (c CatalogEntry) Missing(dir string) CatalogEntry {
	out := c
	out.Files = nil
	for _, f := range c.Files {
		if !regularFile(filepath.Join(dir, f.Filename)) && !regularFile(filepath.Join(dir, downloadName(f.URL))) {
			out.Files = append(out.Files, f)
		}
	}
	return out
}

func downloadName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

func regularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// HostCommand's paths are relative to the model path. $HF_TOKEN sits in
// double quotes so the shell running the snippet expands it.
func (c CatalogEntry) HostCommand() string {
	auth := ""
	if c.Gated {
		auth = `-H "Authorization: Bearer $HF_TOKEN" `
	}
	steps := []string{"mkdir -p " + shellSingleQuote(c.Name)}
	for _, f := range c.Files {
		steps = append(steps, "curl -L "+auth+"-o "+shellSingleQuote(c.Name+"/"+f.Filename)+" "+shellSingleQuote(f.URL))
	}
	return strings.Join(steps, " && \\\n")
}

func shellSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
