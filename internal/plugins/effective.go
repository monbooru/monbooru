package plugins

import (
	"os"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// The launch line stays off PluginConfig and out of monbooru.toml, so a
// web-writable exec line cannot exist.
type Effective struct {
	config.PluginConfig
	Launch    Launch
	Installed bool
	Refused   string
}

// Seeds nothing: an example here would be executable code.
func EnsureDir(dir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warnf("plugins: could not create %s: %v", dir, err)
	}
}

func reserveName(name string) (string, bool) {
	if name == contract.AppMonloader {
		return "the name belongs to the companion", true
	}
	if err := config.ValidatePluginName(name); err != nil {
		return err.Error(), true
	}
	return "", false
}

func EffectiveList(blocks []config.PluginConfig, dir string) []Effective {
	out := make([]Effective, 0, len(blocks))
	byName := make(map[string]int, len(blocks))
	for _, p := range blocks {
		byName[p.Name] = len(out)
		out = append(out, Effective{PluginConfig: p})
	}
	found, refused := Discover(dir, reserveName)
	for _, ins := range found {
		i, ok := byName[ins.Name]
		if !ok {
			out = append(out, Effective{
				PluginConfig: config.PluginConfig{Name: ins.Name},
				Launch:       ins,
				Installed:    true,
			})
			continue
		}
		out[i].Launch, out[i].Installed = ins, true
	}
	for _, r := range refused {
		i, ok := byName[r.Name]
		if !ok {
			out = append(out, Effective{PluginConfig: config.PluginConfig{Name: r.Name}, Refused: r.Reason})
			continue
		}
		out[i].Refused = r.Reason
	}
	return out
}

func FindEffective(list []Effective, name string) (Effective, bool) {
	i := slices.IndexFunc(list, func(p Effective) bool { return p.Name == name })
	if i < 0 {
		return Effective{}, false
	}
	return list[i], true
}

func (e Effective) CommandLine() string {
	return strings.TrimSpace(e.Launch.Command + " " + strings.Join(e.Launch.Args, " "))
}

func SetEnabled(c *config.Config, name string, enabled bool) {
	if p := c.FindPlugin(name); p != nil {
		p.Enabled = enabled
	} else if enabled {
		c.Plugins = append(c.Plugins, config.PluginConfig{Name: name, Enabled: true})
	}
}
