package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/pairing"
)

func healthPeer(t *testing.T) *httptest.Server {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func offerFrom(t *testing.T, srv *Server, remote, body string, headers ...[2]string) (string, pairing.State) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body))
	req.RemoteAddr = remote
	for _, h := range headers {
		if h[0] == "Host" {
			req.Host = h[1]
			continue
		}
		req.Header.Set(h[0], h[1])
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != http.StatusOK || out["status"] != "pending" {
		t.Fatalf("offer answered %d %v, want 200 and pending as ever", rr.Code, out)
	}
	got, _ := srv.pairs.Get(out["request_id"])
	return out["request_id"], got.State
}

func TestPairWindowApprovesMonloaderAndAWorkerOnArrival(t *testing.T) {
	for _, app := range []string{"monloader", "monworker"} {
		t.Run(app, func(t *testing.T) {
			p := healthPeer(t)
			srv := newTestServer(t)
			srv.win.open(pairWindowLength)

			id, st := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"`+app+`","url":"`+p.URL+`","requested_scopes":["read","write"],"peer_token":"sek","version":"1.15.0"}`)
			if st != pairing.Approved {
				t.Fatalf("state = %q, want approved on arrival", st)
			}
			if len(srv.pairs.ListPending()) != 0 {
				t.Error("an approved offer still shows as a card")
			}
			if len(srv.win.list()) != 0 {
				t.Error("the trace lists a pairing before its token was claimed")
			}
			if _, out := pairStatus(t, srv, id); len(out["token"]) != 32 || !srv.pairedWith(app) {
				t.Fatalf("claim = %v, want the token minted on the claim", out)
			}
			made := srv.win.list()
			if len(made) != 1 || made[0].App != app || made[0].Source != "127.0.0.1" || made[0].Version != "1.15.0" {
				t.Errorf("trace = %+v, want one %s pairing from 127.0.0.1", made, app)
			}
		})
	}
}

func TestPairWindowLeavesOffersOnACard(t *testing.T) {
	cases := []struct {
		name, app, remote string
		headers           [][2]string
		prepare           func(*Server)
	}{
		{name: "a plugin", app: "monedit", remote: "127.0.0.1:50000"},
		{name: "the window closed", app: "monloader", remote: "127.0.0.1:50000", prepare: func(s *Server) { s.win.close() }},
		{name: "the window expired", app: "monloader", remote: "127.0.0.1:50000", prepare: func(s *Server) { s.win.open(-time.Second) }},
		{name: "a public source", app: "monloader", remote: "203.0.113.5:50000"},
		{name: "a forwarded hop", app: "monloader", remote: "127.0.0.1:50000", headers: [][2]string{{"X-Forwarded-For", "192.168.1.9"}}},
		{name: "a Forwarded header", app: "monloader", remote: "192.168.1.9:50000", headers: [][2]string{{"Forwarded", "for=192.168.1.9"}}},
		{name: "an X-Real-IP header", app: "monloader", remote: "192.168.1.9:50000", headers: [][2]string{{"X-Real-IP", "192.168.1.9"}}},
		{name: "a monloader already paired", app: "monloader", remote: "127.0.0.1:50000", prepare: func(s *Server) { pairPeer(s, "monloader") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthPeer(t)
			srv := newTestServer(t)
			srv.win.open(pairWindowLength)

			srv.cfg.Monloader.APIURL = p.URL
			srv.cfg.Plugins = append(srv.cfg.Plugins, config.PluginConfig{Name: "monedit", APIURL: p.URL})
			if tc.prepare != nil {
				tc.prepare(srv)
			}
			_, st := offerFrom(t, srv, tc.remote, `{"app":"`+tc.app+`","url":"`+p.URL+`","peer_token":"sek"}`, tc.headers...)
			if st != pairing.Pending {
				t.Errorf("state = %q, want pending on a card", st)
			}
		})
	}
}

func TestPairWindowWaitsForAClickWhenThePeerIsUnreachable(t *testing.T) {
	srv := newTestServer(t)
	srv.win.open(pairWindowLength)
	if _, st := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monloader","url":"http://127.0.0.1:1","peer_token":"sek"}`); st != pairing.Pending {
		t.Errorf("state = %q, want pending", st)
	}
}

func TestPairWindowButtonOpensAndClosesIt(t *testing.T) {
	srv := newTestServer(t)
	wantIn(t, do(srv, "GET", "/settings").Body.String(), "Accept pairings for 10 minutes")

	body := postForm(t, srv, "/settings/plugins/pair/window", url.Values{"open": {"1"}}).Body.String()
	if srv.win.closesAt().IsZero() {
		t.Fatal("the button did not open the window")
	}
	wantIn(t, body, `<span class="status-badge status-warn">open</span>`)
	wantIn(t, body, "Closes in 10:00")
	wantNotIn(t, body, "Accept pairings")

	body = postForm(t, srv, "/settings/plugins/pair/window", url.Values{"open": {"0"}}).Body.String()
	if !srv.win.closesAt().IsZero() {
		t.Fatal("Close left the window open")
	}
	wantIn(t, body, "Approves monloader and workers without a click.")
}

func TestPairedWithoutAClickIsListedWithRevoke(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	srv.win.open(pairWindowLength)
	id, _ := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monloader","url":"`+p.URL+`","requested_scopes":["read","write"],"peer_token":"sek","version":"1.15.0"}`)
	pairStatus(t, srv, id)

	page := do(srv, "GET", "/settings").Body.String()
	wantIn(t, page, "<th>Paired without a click</th>")
	wantIn(t, page, `<td data-label="Source">127.0.0.1</td>`)
	wantIn(t, page, `<td data-label="Scopes">read write</td>`)
	wantIn(t, page, `hx-post="/settings/plugins/monloader/remove"`)
	wantIn(t, page, ">Revoke</button>")

	rr := httptest.NewRecorder()
	srv.pluginPairRemove(rr, pluginRemoveRequest("monloader"))
	if srv.pairedWith("monloader") || len(srv.win.list()) != 0 {
		t.Fatalf("revoke left paired=%v trace=%+v", srv.pairedWith("monloader"), srv.win.list())
	}

	wantIn(t, rr.Body.String(), `id="plugin-pairing" hx-get=`)
	wantNotIn(t, rr.Body.String(), "Paired without a click")
}

func TestAClickedPairingLeavesTheTrace(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	srv.win.open(pairWindowLength)
	id, _ := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monloader","url":"`+p.URL+`","peer_token":"sek"}`)
	pairStatus(t, srv, id)

	srv.win.close()
	id, st := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monloader","url":"`+p.URL+`","peer_token":"sek2"}`)
	if st != pairing.Pending {
		t.Fatalf("re-pair state = %q, want a card", st)
	}
	pairApprove(srv, id)
	pairStatus(t, srv, id)
	if len(srv.win.list()) != 0 {
		t.Errorf("trace = %+v, want the clicked re-pair to replace it", srv.win.list())
	}
}

func TestNavSaysAppsPairedWithoutAClickUntilSettingsIsOpened(t *testing.T) {
	p := healthPeer(t)
	srv := newTestServer(t)
	srv.win.open(pairWindowLength)
	wantIn(t, do(srv, "GET", "/").Body.String(), `<a id="auto-pair-notice" class="pair-notice" href="/settings#plugins" hidden></a>`)

	id, _ := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monloader","url":"`+p.URL+`","peer_token":"sek"}`)
	pairStatus(t, srv, id)
	poll := do(srv, "GET", "/internal/job/status").Body.String()
	wantIn(t, poll, `<a id="auto-pair-notice" class="pair-notice" href="/settings#plugins" hx-swap-oob="true"`)
	wantIn(t, poll, "[! 1 app paired without a click]")
	key := srv.pairNotice().Key() + srv.autoPairNotice().Key()
	wantNotIn(t, do(srv, "GET", "/internal/job/status?pair="+url.QueryEscape(key)).Body.String(), `id="auto-pair-notice"`)

	id, _ = offerFrom(t, srv, "127.0.0.1:50000", `{"app":"monworker","url":"`+p.URL+`","peer_token":"w"}`)
	pairStatus(t, srv, id)
	wantIn(t, do(srv, "GET", "/").Body.String(), "[! 2 apps paired without a click]")

	page := do(srv, "GET", "/settings").Body.String()
	wantIn(t, page, `<a id="auto-pair-notice" class="pair-notice" href="/settings#plugins" hidden></a>`)
	wantIn(t, page, "Paired without a click")
	wantIn(t, do(srv, "GET", "/").Body.String(), `<a id="auto-pair-notice" class="pair-notice" href="/settings#plugins" hidden></a>`)
}

func newInstallServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Paths.DataPath = filepath.Join(dir, "data")
	cfg.Galleries[0].GalleryPath = filepath.Join(dir, "gallery")
	mustMkdirAll(t, cfg.Galleries[0].GalleryPath, 0o755)
	cfg.Galleries[0].DBPath = filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db")
	cfg.Galleries[0].ThumbnailsPath = filepath.Join(cfg.Paths.DataPath, "default", "thumbnails")
	cfg.Gallery.WatchEnabled = false
	if mutate != nil {
		mutate(cfg)
	}
	srv, err := NewServer(cfg, filepath.Join(dir, "monbooru.toml"), jobs.NewManager(), Desktop{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestPairWindowOpensByItselfOnANewInstall(t *testing.T) {
	if newInstallServer(t, nil).win.closesAt().IsZero() {
		t.Error("a new install opened no window")
	}
	withPassword := newInstallServer(t, func(c *config.Config) { c.Auth.PasswordHash = "$2a$10$abcdefghijklmnopqrstuv" })
	if !withPassword.win.closesAt().IsZero() {
		t.Error("a config that arrives with a password opened the window")
	}
	paired := newInstallServer(t, func(c *config.Config) {
		tok, _ := apitoken.New("monloader (paired)", config.AllScopes)
		tok.Paired = "monloader"
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
	})
	if !paired.win.closesAt().IsZero() {
		t.Error("a config that arrives paired opened the window")
	}
	if !newTestServer(t).win.closesAt().IsZero() {
		t.Error("a library that already existed opened the window")
	}
}

func TestDesktopApprovesAnOfferFromThisMachineAtAnyTime(t *testing.T) {
	local := [2]string{"Host", "127.0.0.1:8455"}
	cases := []struct {
		name    string
		app     string
		desktop bool
		repair  bool
		headers [][2]string
		want    pairing.State
	}{
		{"monloader", "monloader", true, false, [][2]string{local}, pairing.Approved},
		{"a worker", "monworker", true, false, [][2]string{local}, pairing.Approved},
		{"a re-pair of monloader", "monloader", true, true, [][2]string{local}, pairing.Approved},
		{"a plugin", "monedit", true, false, [][2]string{local}, pairing.Pending},
		{"a server", "monloader", false, false, [][2]string{local}, pairing.Pending},
		{"a proxied hop", "monloader", true, false, [][2]string{local, {"Via", "1.1 proxy"}}, pairing.Pending},
		{"a LAN Host", "monloader", true, false, [][2]string{{"Host", "192.168.1.5:8455"}}, pairing.Pending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthPeer(t)
			srv := newTestServerWith(t, false, func(c *config.Config) {
				c.SetupDone = true

				c.Server.BaseURL = "http://192.168.1.5:8455"
			})
			srv.desktop = tc.desktop
			if tc.repair {
				pairPeer(srv, tc.app)
			}
			id, st := offerFrom(t, srv, "127.0.0.1:50000", `{"app":"`+tc.app+`","url":"`+p.URL+`","peer_token":"sek"}`, tc.headers...)
			if st != tc.want {
				t.Fatalf("state = %q, want %q", st, tc.want)
			}
			if tc.want != pairing.Approved {
				return
			}
			claim := httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil)
			claim.Host = "127.0.0.1:8455"
			srv.Handler().ServeHTTP(httptest.NewRecorder(), claim)
			if made := srv.win.list(); len(made) != 1 || made[0].App != tc.app {
				t.Errorf("trace = %+v, want the %s pairing listed", made, tc.app)
			}
		})
	}
}
