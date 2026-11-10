package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/shared/apitoken"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/pairing"
)

func TestPairingHandshake(t *testing.T) {
	srv := newTestServer(t)

	var cacheControl string
	status := func(id string) (int, map[string]string) {
		rr := do(srv, "GET", "/api/v1/pair/status?id="+id)
		cacheControl = rr.Header().Get("Cache-Control")
		var out map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	body := `{"app":"monloader","url":"http://ml:8081","requested_scopes":["read","write"],"peer_token":"ml-secret"}`
	code, out := pairOffer(t, srv, body)
	if code != http.StatusOK || out["request_id"] == "" {
		t.Fatalf("request: code=%d out=%v", code, out)
	}
	id := out["request_id"]

	if _, st := status(id); st["status"] != "pending" {
		t.Fatalf("status before approval = %v, want pending", st)
	}

	if !srv.pairs.SetState(id, pairing.Approved) {
		t.Fatal("setState approved failed")
	}

	code, st := status(id)
	if code != http.StatusOK || st["status"] != "approved" || len(st["token"]) != 32 {
		t.Fatalf("claim: code=%d st=%v", code, st)
	}
	if cacheControl != "no-store" {
		t.Errorf("the token answer has Cache-Control %q, want no-store", cacheControl)
	}
	if !srv.pairedWith(monloader.App) {
		t.Error("no paired token after claim")
	}
	if srv.cfg.Monloader.APIToken != "ml-secret" {
		t.Errorf("reverse token not stored: %+v", srv.cfg.Monloader)
	}

	if got := srv.ml.Base(); got != "http://192.0.2.1:8081" {
		t.Errorf("api base not derived from request source: %q", got)
	}
	tok := srv.cfg.Auth.Tokens.ByHash(apitoken.Hash(st["token"]))
	if tok == nil || !tok.HasScope(config.ScopeWrite) || tok.HasScope(config.ScopeDelete) {
		t.Errorf("issued token scopes wrong: %+v", tok)
	}

	if _, st2 := status(id); st2["token"] != "" {
		t.Errorf("token re-delivered on second poll: %v", st2)
	}

	code, again := pairOffer(t, srv, `{"app":"monloader","url":"http://ml:8081","requested_scopes":["read","write"],"peer_token":"ml-second"}`)
	if code != http.StatusOK || again["request_id"] == "" {
		t.Fatalf("re-pair request: code=%d out=%v", code, again)
	}
	pending := srv.pairs.ListPending()
	if len(pending) != 1 || !pending[0].Repair {
		t.Fatalf("pending = %+v, want one request marked as a re-pair", pending)
	}
	if !srv.pairs.SetState(again["request_id"], pairing.Approved) {
		t.Fatal("setState approved failed")
	}
	code, st = status(again["request_id"])
	if code != http.StatusOK || len(st["token"]) != 32 {
		t.Fatalf("re-pair claim: code=%d st=%v", code, st)
	}
	var paired int
	for _, t := range srv.cfg.Auth.Tokens {
		if t.Paired == "monloader" {
			paired++
		}
	}
	if paired != 1 {
		t.Errorf("paired tokens = %d, want the re-pair to have replaced the old one", paired)
	}
	if srv.cfg.Auth.Tokens.ByHash(apitoken.Hash(st["token"])) == nil {
		t.Error("the re-paired secret is not the token that survived")
	}
	if srv.cfg.Monloader.APIToken != "ml-second" {
		t.Errorf("reverse token not replaced: %+v", srv.cfg.Monloader)
	}
}

func TestPairingRefusesALaterPluginContract(t *testing.T) {
	srv := newTestServer(t)
	for body, want := range map[string]int{
		`{"app":"simple-edit","url":"http://p:9","contract":99}`: http.StatusConflict,
		`{"app":"simple-edit","url":"http://p:9","contract":1}`:  http.StatusOK,
		`{"app":"simple-edit","url":"http://p:9"}`:               http.StatusOK,
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body)))
		if rr.Code != want {
			t.Errorf("%s: code %d, want %d (%s)", body, rr.Code, want, rr.Body.String())
		}
		if want == http.StatusConflict && !strings.Contains(rr.Body.String(), `"contract_unsupported"`) {
			t.Errorf("refusal body = %s, want contract_unsupported", rr.Body.String())
		}
	}
}

func TestPairingApproveAndRemove(t *testing.T) {

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer peer.Close()

	srv := newTestServer(t)
	h := srv.Handler()

	rr := httptest.NewRecorder()
	preq := httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monloader","url":"`+peer.URL+`","requested_scopes":["read","write"],"peer_token":"sek"}`))

	preq.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rr, preq)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	pairApprove(srv, id)

	_, st := pairStatus(t, srv, id)
	if len(st["token"]) != 32 || !srv.pairedWith(monloader.App) {
		t.Fatalf("approve+claim failed: %v", st)
	}

	rmr := httptest.NewRecorder()
	srv.pluginPairRemove(rmr, pluginRemoveRequest(monloader.App))
	if srv.pairedWith(monloader.App) || srv.cfg.Monloader.APIToken != "" {
		t.Errorf("remove did not tear down: paired=%v creds=%+v", srv.pairedWith(monloader.App), srv.cfg.Monloader)
	}
	if !strings.Contains(rmr.Body.String(), `id="auth-tokens"`) || !strings.Contains(rmr.Body.String(), "hx-swap-oob") {
		t.Errorf("remove should OOB-refresh the token list, got %q", rmr.Body.String())
	}

	wantIn(t, rmr.Body.String(), "could not reach monloader")
}

func TestPairingCardShowsTheScopesTheTokenCarries(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer peer.Close()
	srv := newTestServer(t)
	h := srv.Handler()

	rr := httptest.NewRecorder()
	preq := httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monedit","url":"`+peer.URL+`","requested_scopes":["admin"],"version":"0.1.0"}`))
	preq.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rr, preq)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	card := httptest.NewRecorder()
	srv.pluginPairingFragment(card, httptest.NewRequest("GET", "/internal/plugins/pairing", nil))
	wantIn(t, card.Body.String(), "<td data-label=\"Scopes\">read write</td>")
	wantNotIn(t, card.Body.String(), "admin")

	pairApprove(srv, id)
	pairStatus(t, srv, id)
	tok := srv.cfg.Auth.Tokens.Paired("monedit")
	if tok == nil || !slices.Equal(tok.Scopes, []string{config.ScopeRead, config.ScopeWrite}) {
		t.Errorf("paired token = %+v, want read and write, as the card showed", tok)
	}
}

func TestPairingCardSaysAReadOnlyOpenButtonActsWithTheSessionUnlessManaged(t *testing.T) {
	open := config.PluginButton{Slot: config.SlotDetailActions, Label: "edit", Mode: config.ModeOpen, Path: "/edit"}
	relay := config.PluginButton{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay, Path: "/rotate"}
	cases := []struct {
		name    string
		scopes  []string
		buttons []config.PluginButton
		managed bool
		source  string
		want    bool
	}{
		{"read only, open button", []string{config.ScopeRead}, []config.PluginButton{relay, open}, false, "192.168.1.20", true},
		{"read and delete, open button", []string{config.ScopeRead, config.ScopeDelete}, []config.PluginButton{open}, false, "192.168.1.20", true},
		{"write, open button", []string{config.ScopeRead, config.ScopeWrite}, []config.PluginButton{open}, false, "192.168.1.20", false},
		{"read only, relay only", []string{config.ScopeRead}, []config.PluginButton{relay}, false, "192.168.1.20", false},
		{"managed plugin from this host, open button", []string{config.ScopeRead}, []config.PluginButton{open}, true, "127.0.0.1", false},
		{"a managed plugin's name from another host, open button", []string{config.ScopeRead}, []config.PluginButton{open}, true, "192.168.1.20", true},
		{"self-managed from this host, open button", []string{config.ScopeRead}, []config.PluginButton{open}, false, "127.0.0.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			if tc.managed {
				installPluginFolder(t, srv, "monedit", "command = \"./run.sh\"\n", "")
			}
			if _, ok := srv.pairs.Create(peer.Req{Request: pairing.Request{App: "monedit", Source: tc.source, Scopes: tc.scopes}, Buttons: tc.buttons}); !ok {
				t.Fatal("pending request not queued")
			}
			card := httptest.NewRecorder()
			srv.pluginPairingFragment(card, httptest.NewRequest("GET", "/internal/plugins/pairing", nil))
			if got := strings.Contains(card.Body.String(), "run with your session"); got != tc.want {
				t.Errorf("session line shown = %v, want %v:\n%s", got, tc.want, card.Body.String())
			}
		})
	}
}

func TestMonloaderLightPauseIsReversible(t *testing.T) {
	srv := newTestServer(t)
	tok, _ := apitoken.New("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	tok.PeerURL = "http://mon.example:7777"
	setTokens(srv, []apitoken.Token{tok})
	srv.cfg.Monloader.APIToken = "sek"

	pr := httptest.NewRecorder()
	srv.monloaderLightDisconnect(pr, httptest.NewRequest("POST", "/internal/monloader/disconnect", nil))
	if !srv.pairedWith(monloader.App) || srv.cfg.Monloader.APIToken != "sek" {
		t.Fatalf("pause dropped the pairing: paired=%v creds=%+v", srv.pairedWith(monloader.App), srv.cfg.Monloader)
	}
	if !srv.cfg.Monloader.Paused || srv.ml.Base() != "" {
		t.Errorf("pause did not suspend connectivity: paused=%v base=%q", srv.cfg.Monloader.Paused, srv.ml.Base())
	}
	if body := pr.Body.String(); !strings.Contains(body, "/internal/monloader/reconnect") || !strings.Contains(body, "paused") {
		t.Errorf("paused light should offer resume, got %q", body)
	}

	rr := httptest.NewRecorder()
	srv.monloaderLightReconnect(rr, httptest.NewRequest("POST", "/internal/monloader/reconnect", nil))
	if srv.cfg.Monloader.Paused || srv.ml.Base() != "http://mon.example:7777" {
		t.Errorf("resume did not restore connectivity: paused=%v base=%q", srv.cfg.Monloader.Paused, srv.ml.Base())
	}
}

func TestMonloaderAPIURLEditableWhenPaired(t *testing.T) {
	readonly := func(body string) bool {
		i := strings.Index(body, `id="monloader_api_url"`)
		if i < 0 {
			t.Fatal("api url field not found")
		}
		tag := body[i:]
		if e := strings.IndexByte(tag, '>'); e >= 0 {
			tag = tag[:e]
		}
		return strings.Contains(tag, "readonly")
	}

	srv := newTestServer(t)
	rr := do(srv, "GET", "/settings")
	if readonly(rr.Body.String()) {
		t.Error("api url should be editable when not paired")
	}

	pairPeer(srv, "monloader")
	rr2 := do(srv, "GET", "/settings")
	if readonly(rr2.Body.String()) {
		t.Error("api url override should stay editable when paired")
	}
}

func TestPairingFragmentRefreshesRowsOnTransition(t *testing.T) {
	srv := newTestServer(t)
	pairPeer(srv, "monloader")

	rr := httptest.NewRecorder()
	srv.pluginPairingFragment(rr, httptest.NewRequest("GET", "/internal/plugins/pairing?paired=0", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `id="auth-tokens"`) || !strings.Contains(body, `id="plugin-rows"`) {
		t.Errorf("transition should OOB-refresh the rows and the token list, got %q", body)
	}

	rr2 := httptest.NewRecorder()
	srv.pluginPairingFragment(rr2, httptest.NewRequest("GET", "/internal/plugins/pairing?paired=1", nil))
	wantNotIn(t, rr2.Body.String(), "hx-swap-oob")
}

func TestPairingPreservesConfiguredAPIURL(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Monloader.APIURL = peer.URL

	_, out := pairOffer(t, srv, `{"app":"monloader","url":"http://localhost:8081","requested_scopes":["read","write"],"peer_token":"sek"}`)
	id := out["request_id"]

	pairApprove(srv, id)
	pairStatus(t, srv, id)

	if !srv.pairedWith(monloader.App) {
		t.Fatal("pairing did not complete")
	}
	if srv.cfg.Monloader.APIURL != peer.URL {
		t.Errorf("pairing overwrote the configured api_url: %q", srv.cfg.Monloader.APIURL)
	}
}

func TestPairingApproveBlockedWhenUnreachable(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	rr := httptest.NewRecorder()
	preq := httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monloader","url":"http://127.0.0.1:1","requested_scopes":["read","write"],"peer_token":"sek"}`))
	preq.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rr, preq)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	arr := pairApprove(srv, id)
	wantIn(t, arr.Body.String(), "unreachable")

	if st, _ := srv.pairs.Get(id); st.State != pairing.Pending {
		t.Errorf("request should stay pending, got %q", st.State)
	}
	pairStatus(t, srv, id)
	if srv.pairedWith(monloader.App) {
		t.Error("unreachable monloader must not become paired")
	}
}

func TestPairingRemoveNotifiesPeer(t *testing.T) {
	var gotPath, gotAuth string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
	}))
	defer peer.Close()

	srv := newTestServer(t)
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = peer.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	rmr := httptest.NewRecorder()
	srv.pluginPairRemove(rmr, pluginRemoveRequest(monloader.App))

	if gotPath != "/api/v1/pair/remove" || gotAuth != "Bearer peer-secret" {
		t.Errorf("peer not notified: path=%q auth=%q", gotPath, gotAuth)
	}
	if srv.pairedWith(monloader.App) {
		t.Error("local pairing not removed")
	}
	wantNotIn(t, rmr.Body.String(), "could not reach")
}

func TestPairedPeerAddressFollowsThePeer(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	_, out := pairOffer(t, srv, `{"app":"monloader","url":"http://localhost:8081","requested_scopes":["read","write"],"peer_token":"sek"}`)
	if !srv.pairs.SetState(out["request_id"], pairing.Approved) {
		t.Fatal("setState approved failed")
	}
	_, st := pairStatus(t, srv, out["request_id"])
	if got := srv.ml.Base(); got != "http://192.0.2.1:8081" {
		t.Fatalf("api base after pairing = %q", got)
	}

	call := func(secret, remote string) {
		req := httptest.NewRequest("GET", "/api/v1/", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.RemoteAddr = remote
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	call(st["token"], "198.51.100.7:40000")
	if got := srv.ml.Base(); got != "http://198.51.100.7:8081" {
		t.Errorf("peer address did not follow the peer: %q", got)
	}

	opTok, opSecret := apitoken.New("script", config.AllScopes)
	setTokens(srv, append(srv.cfg.Auth.Tokens, opTok))
	call(opSecret, "203.0.113.9:40000")
	if got := srv.ml.Base(); got != "http://198.51.100.7:8081" {
		t.Errorf("an unpaired token moved the peer: %q", got)
	}
}

func pluginRemoveRequest(name string) *http.Request {
	r := httptest.NewRequest("POST", "/settings/plugins/"+name+"/remove", nil)
	r.SetPathValue("name", name)
	return r
}

func TestPairingAndHealthCarryCORSHeaders(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Server.CORSOrigins = []string{"http://tool.example"}
	h := srv.Handler()

	do := func(method, target, origin string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, strings.NewReader(`{"app":"probe","url":"http://p","peer_token":"x"}`))
		req.Header.Set("Origin", origin)
		h.ServeHTTP(rr, req)
		return rr
	}

	for _, c := range []struct{ method, target string }{
		{"POST", "/api/v1/pair/request"},
		{"GET", "/api/v1/pair/status?id=none"},
		{"POST", "/api/v1/pair/remove"},
		{"GET", "/health"},
	} {
		rr := do(c.method, c.target, "http://tool.example")
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://tool.example" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want the listed origin", c.target, got)
		}
		if got := rr.Header().Get("Vary"); got != "Origin" {
			t.Errorf("%s: Vary = %q, want Origin", c.target, got)
		}
	}

	if rr := do("POST", "/api/v1/pair/request", "http://evil.example"); rr.Code != http.StatusForbidden {
		t.Errorf("unlisted origin on pair/request = %d, want %d", rr.Code, http.StatusForbidden)
	}
	rr := do("GET", "/health", "http://evil.example")
	if rr.Code != http.StatusOK {
		t.Errorf("unlisted origin on /health = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("/health echoed an unlisted origin: %q", got)
	}
}

func TestJobStatusSwapsThePairNoticeOnlyWhenTheRequestsChange(t *testing.T) {
	srv := newTestServer(t)
	wantNotIn(t, do(srv, "GET", "/internal/job/status").Body.String(), `id="pair-notice"`)

	if code, _ := pairOffer(t, srv, `{"app":"monedit","url":"http://me:9","requested_scopes":["read"],"version":"0.4.0"}`); code != http.StatusOK {
		t.Fatalf("offer = %d", code)
	}
	key := srv.pairNotice().Key()
	body := do(srv, "GET", "/internal/job/status").Body.String()
	wantIn(t, body, `<a id="pair-notice" class="pair-notice" href="/settings#plugins" hx-swap-oob="true"`)
	wantIn(t, body, "[! monedit wants to pair]")
	wantIn(t, body, `"pair":"`+key+`"`)

	wantNotIn(t, do(srv, "GET", "/internal/job/status?pair="+url.QueryEscape(key)).Body.String(), `id="pair-notice"`)
}

func TestPageNavCarriesThePairNotice(t *testing.T) {
	srv := newTestServer(t)
	wantIn(t, do(srv, "GET", "/").Body.String(), `<a id="pair-notice" class="pair-notice" href="/settings#plugins" hidden></a>`)

	pairOffer(t, srv, `{"app":"monedit","url":"http://me:9","requested_scopes":["read"]}`)
	page := do(srv, "GET", "/settings").Body.String()
	wantIn(t, page, `aria-label="monedit wants to pair"`)
	wantIn(t, page, `<span id="settings-pair-count" class="nav-count"> (1)</span>`)
	wantIn(t, page, "<td data-label=\"Expires\">5 min</td>")
}

func TestPairingCardPollSwapsTheNoticeOnlyOnAChange(t *testing.T) {
	srv := newTestServer(t)
	pairOffer(t, srv, `{"app":"monedit","url":"http://me:9","requested_scopes":["read"]}`)
	key := srv.pairNotice().Key()

	rr := httptest.NewRecorder()
	srv.pluginPairingFragment(rr, httptest.NewRequest("GET", "/internal/plugins/pairing?pair=", nil))
	wantIn(t, rr.Body.String(), `<span id="settings-pair-count" class="nav-count" hx-swap-oob="true"> (1)</span>`)
	wantIn(t, rr.Body.String(), `id="pair-notice"`)

	same := httptest.NewRecorder()
	srv.pluginPairingFragment(same, httptest.NewRequest("GET", "/internal/plugins/pairing?pair="+url.QueryEscape(key), nil))
	wantNotIn(t, same.Body.String(), "hx-swap-oob")
}

func TestDenyClearsThePairNoticeAtOnce(t *testing.T) {
	srv := newTestServer(t)
	_, out := pairOffer(t, srv, `{"app":"monedit","url":"http://me:9","requested_scopes":["read"]}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/settings/plugins/pair/"+out["request_id"]+"/deny", nil)
	req.SetPathValue("id", out["request_id"])
	srv.pluginPairDeny(rr, req)
	wantIn(t, rr.Body.String(), `<a id="pair-notice" class="pair-notice" href="/settings#plugins" hx-swap-oob="true" hidden></a>`)
	wantIn(t, rr.Body.String(), `<span id="settings-pair-count" class="nav-count" hx-swap-oob="true"></span>`)
}
