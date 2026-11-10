package peer

import (
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestPeerCallbackURL(t *testing.T) {
	cases := []struct{ advertised, source, want string }{
		{"http://localhost:8081", "10.0.0.5", "http://10.0.0.5:8081"},
		{"http://monloader:8081", "172.18.0.4", "http://172.18.0.4:8081"},
		{"https://localhost", "10.0.0.5", "https://10.0.0.5"},
		{"http://localhost:8081", "fe80::1", "http://[fe80::1]:8081"},
		{"https://localhost", "fe80::1", "https://[fe80::1]"},
		{"http://localhost:8081", "", "http://localhost:8081"},
		{"", "10.0.0.5", ""},
	}
	for _, c := range cases {
		if got := CallbackURL(c.advertised, c.source); got != c.want {
			t.Errorf("CallbackURL(%q, %q) = %q, want %q", c.advertised, c.source, got, c.want)
		}
	}
}

func pairedConfig(app string) *config.Config {
	tok, _ := apitoken.New(app+" (paired)", config.AllScopes)
	tok.Paired = app
	c := &config.Config{}
	c.Auth.Tokens = []apitoken.Token{tok}
	return c
}

func TestGrantResumesAPausedLink(t *testing.T) {
	c := &config.Config{}
	c.Monloader.Paused = true
	tok, _ := apitoken.New(contract.AppMonloader+" (paired)", config.AllScopes)
	tok.Paired = contract.AppMonloader

	Grant(c, Req{App: contract.AppMonloader, PeerToken: "peer-secret"}, tok)
	if c.Monloader.Paused {
		t.Error("a fresh pairing left the monloader link paused")
	}
}

func TestRevokeKeepsAPIURL(t *testing.T) {
	c := pairedConfig(contract.AppMonloader)
	c.Monloader.APIURL = "http://10.3.2.10:18081"
	c.Monloader.APIToken = "peer-secret"

	Revoke(c, contract.AppMonloader)
	if c.Auth.Tokens.Paired(contract.AppMonloader) != nil || c.Monloader.APIToken != "" {
		t.Error("remove should drop the pairing token and credential")
	}
	if c.Monloader.APIURL != "http://10.3.2.10:18081" {
		t.Errorf("remove dropped the configured api_url: %q", c.Monloader.APIURL)
	}
}

func TestRevokeDropsAPluginsBlock(t *testing.T) {
	c := pairedConfig("monedit")
	c.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek"}}

	Revoke(c, "monedit")
	if c.Auth.Tokens.Paired("monedit") != nil || len(c.Plugins) != 0 {
		t.Errorf("remove left state behind: tokens=%+v plugins=%+v", c.Auth.Tokens, c.Plugins)
	}
}

func TestRevokeKeepsAPluginBlocksOperatorLines(t *testing.T) {
	c := &config.Config{}
	c.Plugins = []config.PluginConfig{{
		Name: "monedit", PeerToken: "sek", Version: "0.3.1", Enabled: true,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay}},
	}}

	Revoke(c, "monedit")
	p := c.FindPlugin("monedit")
	if p == nil {
		t.Fatal("a block carrying the operator's enable must survive the unpair")
	}
	if p.PeerToken != "" || p.Version != "" || len(p.Buttons) != 0 {
		t.Errorf("paired halves not cleared: %+v", *p)
	}
	if !p.Enabled {
		t.Error("the enable was cleared with the pairing")
	}
}
