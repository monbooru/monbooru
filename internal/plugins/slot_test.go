package plugins

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestSlotOffersEachPeersButtonsInNameOrder(t *testing.T) {
	c := &config.Config{Plugins: []config.PluginConfig{
		{Name: "zed", APIURL: "http://zed:1", Buttons: []config.PluginButton{
			{Slot: config.SlotDetailActions, Label: "archives", Mode: config.ModeRelay, Media: "archive"},
			{Slot: config.SlotDetailActions, Label: "view", Mode: config.ModeOpen, Path: "/v?id={image_id}&g={gallery}"},
			{Slot: config.SlotBatchBar, Label: "elsewhere", Mode: config.ModeRelay},
		}},
		{Name: "alpha", APIURL: "http://alpha:1", Paused: true, Buttons: []config.PluginButton{
			{Slot: config.SlotDetailActions, Label: "edit", Mode: config.ModeRelay},
		}},
		{Name: "gone", Buttons: []config.PluginButton{{Slot: config.SlotDetailActions, Label: "x", Mode: config.ModeRelay}}},
	}}
	peers := NewPeers(func() string { return "" }, make(chan struct{}))
	peers.MarkDown("zed")

	got := peers.Slot(c, config.SlotDetailActions, 7, "png", "my pics", "", func(name string) string { return "/plugins/" + name })
	want := []SlotPeer{
		{Name: "alpha", Off: true, OffState: "paused", Buttons: []SlotButton{{Label: "edit", Mode: config.ModeRelay}}},
		{Name: "zed", Off: true, OffState: "not responding", Buttons: []SlotButton{
			{Label: "view", Mode: config.ModeOpen, Index: 1, Href: "/plugins/zed/v?id=7&g=my+pics"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slot = %+v, want %+v", got, want)
	}
}

func TestScopeKeepsTheMediaItsButtonNames(t *testing.T) {
	types := func([]int64) (map[int64]string, error) { return map[int64]string{1: "png", 2: "cbz"}, nil }
	if got := Scope(config.PluginButton{Media: "archive"}, []int64{1, 2, 404}, types); !slices.Equal(got, []int64{2}) {
		t.Errorf("archive scope = %v, want [2]", got)
	}
	if got := Scope(config.PluginButton{}, []int64{1, 2, 404}, nil); !slices.Equal(got, []int64{1, 2, 404}) {
		t.Errorf("unscoped = %v, want every id", got)
	}
	failed := func([]int64) (map[int64]string, error) {
		return map[int64]string{2: "cbz"}, errors.New("read cut short")
	}
	if got := Scope(config.PluginButton{Media: "archive"}, []int64{1, 2}, failed); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("scope after a failed read = %v, want every id kept", got)
	}
}

func TestSubstituteVarsReplacesEveryDeclaredVariable(t *testing.T) {
	for _, v := range config.PluginVars {
		if got := SubstituteVars("/x"+v, 7, "pics", "http://b/"); got == "/x" || strings.Contains(got, v) {
			t.Errorf("%s substitutes to %q, want a value", v, got)
		}
	}
	if got := SubstituteVars("/v?id={image_id}&g={gallery}&back={back_url}", 7, "my pics", "http://b/?q=a b"); got != "/v?id=7&g=my+pics&back=http%3A%2F%2Fb%2F%3Fq%3Da+b" {
		t.Errorf("substituted = %q", got)
	}
}
