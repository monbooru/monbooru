package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/shared/pairing"
)

func pairOffer(t *testing.T, srv *Server, body string) (int, map[string]string) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body)))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func pairStatus(t *testing.T, srv *Server, id string) (int, map[string]string) {
	t.Helper()
	rr := do(srv, "GET", "/api/v1/pair/status?id="+id)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func pairApprove(srv *Server, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/settings/plugins/pair/"+id+"/approve", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.pluginPairApprove(w, req)
	return w
}

func TestPluginPairingWritesItsOwnBlock(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Monloader.APIToken = "monloader-secret"

	code, out := pairOffer(t, srv, `{"app":"monedit","url":"http://edit:7301","requested_scopes":["read","write"],
		"peer_token":"edit-secret","version":"0.3.1",
		"buttons":[{"slot":"detail-actions","label":"crop","mode":"open","path":"/edit?image={image_id}"}]}`)
	if code != http.StatusOK {
		t.Fatalf("request: code=%d out=%v", code, out)
	}
	if !srv.pairs.SetState(out["request_id"], pairing.Approved) {
		t.Fatal("approve failed")
	}
	pairStatus(t, srv, out["request_id"])

	p := srv.cfg.FindPlugin("monedit")
	if p == nil {
		t.Fatalf("no [[plugin]] block after claim: %+v", srv.cfg.Plugins)
	}
	if p.PeerToken != "edit-secret" || p.Version != "0.3.1" || len(p.Buttons) != 1 {
		t.Errorf("block not filled from the offer: %+v", *p)
	}

	if srv.cfg.Monloader.APIToken != "monloader-secret" {
		t.Errorf("plugin pairing clobbered monloader's credential: %q", srv.cfg.Monloader.APIToken)
	}
	if !srv.pairedWith("monedit") {
		t.Error("no paired token for the plugin")
	}
}

func TestPluginPairingRefusesInvalidOffers(t *testing.T) {
	cases := []struct{ name, body string }{
		{"reserved name", `{"app":"monbooru","url":"http://x"}`},
		{"bad charset", `{"app":"mon edit","url":"http://x"}`},
		{"unknown slot", `{"app":"a","url":"http://x","buttons":[{"slot":"sidebar","label":"x","mode":"relay"}]}`},
		{"unknown mode", `{"app":"a","url":"http://x","buttons":[{"slot":"batch-bar","label":"x","mode":"push"}]}`},
		{"open off detail", `{"app":"a","url":"http://x","buttons":[{"slot":"batch-bar","label":"x","mode":"open","path":"/e"}]}`},
		{"empty label", `{"app":"a","url":"http://x","buttons":[{"slot":"batch-bar","label":"","mode":"relay"}]}`},
		{"long label", `{"app":"a","url":"http://x","buttons":[{"slot":"batch-bar","label":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":"relay"}]}`},
		{"relative path", `{"app":"a","url":"http://x","buttons":[{"slot":"batch-bar","label":"x","mode":"relay","path":"rotate"}]}`},
		{"unknown variable", `{"app":"a","url":"http://x","buttons":[{"slot":"detail-actions","label":"x","mode":"open","path":"/e?t={token}"}]}`},
		{"unknown media", `{"app":"a","url":"http://x","buttons":[{"slot":"batch-bar","label":"x","mode":"relay","media":"image,video"}]}`},
		{"long version", `{"app":"a","url":"http://x","version":"` + strings.Repeat("v", 33) + `"}`},
	}
	for _, c := range cases {
		srv := newTestServer(t)
		if code, out := pairOffer(t, srv, c.body); code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%v)", c.name, code, out)
		}
	}
}

func TestPluginPairingCountsLabelAndVersionInCharacters(t *testing.T) {
	offer := func(label, version string) string {
		return `{"app":"a","url":"http://x","version":"` + version + `","buttons":[{"slot":"batch-bar","label":"` + label + `","mode":"relay"}]}`
	}
	kanji := func(n int) string { return strings.Repeat("回", n) }
	if code, out := pairOffer(t, newTestServer(t), offer(kanji(24), kanji(32))); code != http.StatusOK {
		t.Errorf("a 24-character label and a 32-character version: code = %d (%v)", code, out)
	}
	if code, _ := pairOffer(t, newTestServer(t), offer(kanji(25), "1.0")); code != http.StatusBadRequest {
		t.Errorf("a 25-character label: code = %d, want 400", code)
	}
	if code, _ := pairOffer(t, newTestServer(t), offer("x", kanji(33))); code != http.StatusBadRequest {
		t.Errorf("a 33-character version: code = %d, want 400", code)
	}
}

func TestPluginPairingRefusesFifthButtonInASlot(t *testing.T) {
	srv := newTestServer(t)
	var buttons []string
	for range 5 {
		buttons = append(buttons, `{"slot":"batch-bar","label":"x","mode":"relay"}`)
	}
	body := `{"app":"a","url":"http://x","buttons":[` + strings.Join(buttons, ",") + `]}`
	if code, out := pairOffer(t, srv, body); code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 (%v)", code, out)
	}
}

func TestPluginApprovalProbesThePluginNotMonloader(t *testing.T) {
	var probed []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = append(probed, r.Host)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	srv.cfg.Monloader.APIURL = "http://127.0.0.1:1"
	id, _ := srv.pairs.Create(peer.Req{Request: pairing.Request{App: "monedit", Source: "127.0.0.1"}, URL: stub.URL})

	pairApprove(srv, id)

	if len(probed) == 0 {
		t.Fatalf("the plugin was never probed; monloader's dead address was used instead")
	}
	if st, _ := srv.pairs.Get(id); st.State != pairing.Approved {
		t.Errorf("state = %q, want approved", st.State)
	}
}

func TestAnApprovedMonloaderAwaitsItsTokenOnItsRow(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer stub.Close()
	srv := newTestServer(t)
	srv.cfg.Monloader.APIURL = stub.URL
	id, _ := srv.pairs.Create(peer.Req{Request: pairing.Request{App: "monloader", Source: "127.0.0.1"}, URL: stub.URL})
	const awaiting = "Approved. Waiting for monloader to finish."

	body := pairApprove(srv, id).Body.String()
	wantIn(t, body, `<div id="plugin-rows" hx-swap-oob="true">`)
	wantIn(t, body, awaiting)
	wantNotIn(t, body, "Not paired yet")

	settings := mustGetBody(t, srv, "/settings")
	wantIn(t, settings, awaiting)

	pairStatus(t, srv, id)
	settings = mustGetBody(t, srv, "/settings")
	wantNotIn(t, settings, awaiting)
}

func pluginFormRequest(target, body string) *http.Request {
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func relay(srv *Server, form string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.pluginRelay(rr, pluginFormRequest("/internal/plugin/relay", form))
	return rr
}

func TestPluginSlotRendersUnreachablePeersInert(t *testing.T) {
	srv := newTestServer(t)
	button := func(slot, label string) config.PluginButton {
		return config.PluginButton{Slot: slot, Label: label, Mode: config.ModeRelay}
	}
	srv.cfg.Plugins = []config.PluginConfig{
		{Name: "zeta", PeerToken: "sek", APIURL: "http://zeta:1", Buttons: []config.PluginButton{button(config.SlotBatchBar, "z")}},
		{Name: "alpha", PeerToken: "sek", APIURL: "http://alpha:1", Buttons: []config.PluginButton{button(config.SlotBatchBar, "a")}},
		{Name: "paused", PeerToken: "sek", APIURL: "http://p:1", Paused: true, Buttons: []config.PluginButton{button(config.SlotBatchBar, "p")}},
		{Name: "dead", PeerToken: "sek", APIURL: "http://d:1", Buttons: []config.PluginButton{button(config.SlotBatchBar, "d")}},
		{Name: "nowhere", PeerToken: "sek", Buttons: []config.PluginButton{button(config.SlotBatchBar, "n")}},
		{Name: "elsewhere", PeerToken: "sek", APIURL: "http://e:1", Buttons: []config.PluginButton{button(config.SlotDetailActions, "e")}},
	}
	srv.peers.MarkDown("dead")

	view := srv.pluginSlot(httptest.NewRequest("GET", "/", nil), config.SlotBatchBar, 0, "")
	off := map[string]bool{}
	var got []string
	for _, g := range view.Groups {
		got = append(got, g.Peer)
		off[g.Peer] = g.Buttons[0].Off
	}
	want := []string{"alpha", "dead", "paused", "zeta"}
	if !slices.Equal(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
	for peer, wantOff := range map[string]bool{"alpha": false, "zeta": false, "dead": true, "paused": true} {
		if off[peer] != wantOff {
			t.Errorf("%s button Off = %v, want %v", peer, off[peer], wantOff)
		}
	}
}

func TestPluginSlotSubstitutesOpenTargets(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{
		{Name: "monedit", PeerToken: "sek", APIURL: "http://edit:7301", Buttons: []config.PluginButton{
			{Slot: config.SlotDetailActions, Label: "crop", Mode: config.ModeOpen, Path: "/edit?image={image_id}&back={back_url}"},
		}},
	}
	r := httptest.NewRequest("GET", "/images/613?back_q=blue", nil)
	r.Host = "nas:8080"
	view := srv.pluginSlot(r, config.SlotDetailActions, 613, models.FileTypePNG)
	if len(view.Groups) != 1 {
		t.Fatalf("groups = %+v", view.Groups)
	}
	want := "/plugins/monedit/edit?image=613&back=" + url.QueryEscape("http://nas:8080/images/613?back_q=blue")
	if got := view.Groups[0].Buttons[0]; got.Href != want {
		t.Errorf("button = %+v, want href %q", got, want)
	}
	if !view.AnyOpen() {
		t.Error("an open button must bring the pop-in with it")
	}
}

func TestPluginSlotHonoursTheDeclaredMedia(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: "http://edit:1", Buttons: []config.PluginButton{
		{Slot: config.SlotDetailActions, Label: "rotate", Mode: config.ModeRelay, Media: "image"},
		{Slot: config.SlotDetailActions, Label: "read", Mode: config.ModeRelay, Media: "archive,animated"},
		{Slot: config.SlotDetailActions, Label: "anything", Mode: config.ModeRelay},
	}}}

	for _, tc := range []struct {
		fileType string
		want     []string
	}{
		{models.FileTypePNG, []string{"rotate", "anything"}},
		{models.FileTypeCBZ, []string{"read", "anything"}},
		{models.FileTypeMP4, []string{"read", "anything"}},
	} {
		view := srv.pluginSlot(httptest.NewRequest("GET", "/", nil), config.SlotDetailActions, 1, tc.fileType)
		var got []string
		for _, b := range view.Groups[0].Buttons {
			got = append(got, b.Label)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s buttons = %v, want %v", tc.fileType, got, tc.want)
		}
	}

	srv.cfg.Plugins[0].Buttons = []config.PluginButton{
		{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay, Media: "image"},
	}
	if view := srv.pluginSlot(httptest.NewRequest("GET", "/", nil), config.SlotBatchBar, 0, ""); !view.Any() {
		t.Error("the batch bar dropped a button it has no medium to judge")
	}
}

func TestPluginSlotDropsAPeerWithNothingLeft(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: "http://edit:1", Buttons: []config.PluginButton{
		{Slot: config.SlotDetailActions, Label: "crop", Mode: config.ModeOpen, Path: "/crop", Media: "image"},
	}}}

	view := srv.pluginSlot(httptest.NewRequest("GET", "/", nil), config.SlotDetailActions, 1, models.FileTypeWEBM)
	if view.Any() {
		t.Errorf("groups = %+v, want none", view.Groups)
	}
}

func TestPluginRelayNarrowsTheScopeToTheButtonsMedia(t *testing.T) {
	var gotBody string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"ok":true,"message":"rotated"}`))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay, Media: "image"}}}}
	png := seedContribImage(t, srv, "shot", models.FileTypePNG)
	clip := seedContribImage(t, srv, "clip", models.FileTypeMP4)

	relay(srv, fmt.Sprintf("plugin=monedit&button=0&ids=%d&ids=%d", png, clip))
	if want := fmt.Sprintf(`"image_ids":[%d]`, png); !strings.Contains(gotBody, want) {
		t.Errorf("payload %q, want only %s", gotBody, want)
	}
	wantIn(t, gotBody, `"gallery":"default"`)

	gotBody = ""
	rr := relay(srv, fmt.Sprintf("plugin=monedit&button=0&ids=%d", clip))
	if gotBody != "" {
		t.Errorf("peer saw %q, want nothing", gotBody)
	}
	if trigger := rr.Header().Get("HX-Trigger"); !strings.Contains(trigger, "nothing selected that monedit handles") {
		t.Errorf("flash = %q", trigger)
	}
}

func TestPluginMountServesThePeersPages(t *testing.T) {
	var gotPath, gotAuth, gotBase string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.RequestURI(), r.Header.Get("Authorization")
		gotBase = r.Header.Get("X-Monbooru-Plugin-Base")
		if r.URL.Path == "/save" {
			http.Redirect(w, r, "/crop?done=1", http.StatusSeeOther)
			return
		}
		_, _ = w.Write([]byte("<h1>crop</h1>"))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "edit-secret", APIURL: peer.URL}}

	rr := do(srv, "GET", "/plugins/monedit/crop?image=613")
	if rr.Code != http.StatusOK || rr.Body.String() != "<h1>crop</h1>" {
		t.Fatalf("mount = %d %q", rr.Code, rr.Body.String())
	}
	if gotPath != "/crop?image=613" || gotAuth != "Bearer edit-secret" || gotBase != "/plugins/monedit" {
		t.Errorf("peer saw path=%q auth=%q base=%q", gotPath, gotAuth, gotBase)
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/plugins/monedit/save", nil))
	if got := rr.Header().Get("Location"); got != "/plugins/monedit/crop?done=1" {
		t.Errorf("redirect = %q", got)
	}

	rr = do(srv, "GET", "/plugins/nobody/crop")
	wantCode(t, rr, http.StatusNotFound)

	srv.cfg.Plugins = []config.PluginConfig{{Name: "gone", PeerToken: "sek", APIURL: "http://127.0.0.1:1"}}
	rr = do(srv, "GET", "/plugins/gone/crop")
	wantCode(t, rr, http.StatusBadGateway)
	if pr := srv.peers.ProbeSeed("gone"); pr.Conn != "down" {
		t.Errorf("probe after a dead mount = %+v", pr)
	}
}

func TestPluginMountRefusesACrossOriginWriteAndForwardsNoCookie(t *testing.T) {
	var hits int
	var gotCookie string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotCookie = r.Header.Get("Cookie")
	}))
	defer peer.Close()
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "edit-secret", APIURL: peer.URL}}

	for _, tc := range []struct {
		header, value string
		code          int
	}{
		{"Origin", "http://evil.example", http.StatusForbidden},
		{"Sec-Fetch-Site", "cross-site", http.StatusForbidden},
		{"Origin", "http://example.com", http.StatusOK},
	} {
		before := hits
		req := httptest.NewRequest("POST", "/plugins/monedit/crop", nil)
		req.Header.Set(tc.header, tc.value)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != tc.code || (hits > before) != (tc.code == http.StatusOK) {
			t.Errorf("%s: %s: code %d, peer hit %v", tc.header, tc.value, rr.Code, hits > before)
		}
	}

	req := httptest.NewRequest("GET", "/plugins/monedit/crop", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: "operator"})
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if gotCookie != "" {
		t.Errorf("the plugin received Cookie %q", gotCookie)
	}
}

func TestPluginRelayScopesASelectionPastTheParameterCaps(t *testing.T) {
	t.Parallel()
	var gotBody string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"ok":true,"message":"done"}`))
	}))
	defer peer.Close()
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "edit-secret", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "crop", Mode: config.ModeRelay, Path: "/crop", Media: "image"}}}}
	const n = 40001
	mustExec(t, srv.db().Write, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		INSERT INTO images (id, sha256, canonical_path, file_type, file_size, ingested_at)
		SELECT i, 'sha' || i, '/v/' || i, CASE WHEN i = 1 THEN 'png' ELSE 'mp4' END, 1, datetime('now') FROM seq`, n)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}

	rr := relay(srv, "plugin=monedit&button=0&ids="+strings.Join(ids, ","))

	mustCode(t, rr, http.StatusNoContent)
	wantIn(t, gotBody, `"image_ids":[1]`)
}

func TestPluginRelayCarriesTheScopeAndFlashesTheAnswer(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(body)
		_, _ = w.Write([]byte(`{"ok":true,"message":"rotated 2 images","refresh":true}`))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "edit-secret", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate 90", Mode: config.ModeRelay, Path: "/rotate"}}}}

	rr := relay(srv, "plugin=monedit&button=0&ids=7&ids=9")

	mustCode(t, rr, http.StatusNoContent)
	if gotPath != "/rotate" || gotAuth != "Bearer edit-secret" {
		t.Errorf("peer call: path=%q auth=%q", gotPath, gotAuth)
	}
	for _, want := range []string{`"payload":1`, `"slot":"batch-bar"`, `"button":"rotate 90"`, `"image_ids":[7,9]`, `"gallery":"default"`} {
		wantIn(t, gotBody, want)
	}
	wantIn(t, rr.Header().Get("HX-Trigger"), "rotated 2 images")
	if rr.Header().Get("HX-Refresh") != "true" {
		t.Error("refresh:true should ride HX-Refresh so an in-place edit shows")
	}
}

func TestPluginRelayReportsARefusalAndAnUnreachablePeer(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"message":"cannot rotate a video"}`))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{
		{Name: "monedit", PeerToken: "sek", APIURL: peer.URL,
			Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay}}},
		{Name: "gone", PeerToken: "sek", APIURL: "http://127.0.0.1:1",
			Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay}}},
	}

	rr := relay(srv, "plugin=monedit&button=0&ids=7")
	trigger := rr.Header().Get("HX-Trigger")
	if !strings.Contains(trigger, "cannot rotate a video") || !strings.Contains(trigger, `"kind":"err"`) {
		t.Errorf("a refusal should flash in the error style, got %q", trigger)
	}
	if rr.Header().Get("HX-Refresh") != "" {
		t.Error("a refusal must not refresh the page")
	}

	rr2 := relay(srv, "plugin=gone&button=0&ids=7")
	wantIn(t, rr2.Header().Get("HX-Trigger"), "did not answer")

	if srv.peers.ProbeSeed("gone").Conn != "down" {
		t.Error("a failed relay should mark the peer down")
	}
}

func TestPluginRelayANon2xxAnswerKeepsThePeerUp(t *testing.T) {
	body := ""
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay}}}}

	for _, c := range []struct{ answer, want string }{
		{"", "monedit returned 500 Internal Server Error"},
		{"not json", "monedit returned an unreadable answer"},
	} {
		body = c.answer
		rr := relay(srv, "plugin=monedit&button=0&ids=7")
		trigger := rr.Header().Get("HX-Trigger")
		if !strings.Contains(trigger, c.want) || !strings.Contains(trigger, `"kind":"err"`) {
			t.Errorf("flash = %q, want %q in the error style", trigger, c.want)
		}
		if pr := srv.peers.ProbeSeed("monedit"); pr.Conn == "down" {
			t.Errorf("a peer that answered %q was marked down", c.want)
		}
	}
}

func TestPluginRelayAnswersARedirectAsAnUpstreamError(t *testing.T) {
	followed := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			_, _ = w.Write([]byte(`{"ok":true,"message":"followed"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay, Path: "/rotate"}}}}

	rr := relay(srv, "plugin=monedit&button=0&ids=7")
	mustCode(t, rr, http.StatusNoContent)
	if loc := rr.Header().Get("Location"); loc != "" {
		t.Errorf("the relay passed the peer's Location %q to the browser", loc)
	}
	trigger := rr.Header().Get("HX-Trigger")
	if !strings.Contains(trigger, "monedit returned 302 Found") || !strings.Contains(trigger, `"kind":"err"`) {
		t.Errorf("flash = %q, want the redirect reported in the error style", trigger)
	}
	if followed {
		t.Error("the relay followed the peer's redirect")
	}
	if pr := srv.peers.ProbeSeed("monedit"); pr.Conn == "down" {
		t.Error("a peer that answered with a redirect was marked down")
	}
}

func heldPeer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	got := make(chan struct{}, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(peer.Close)
	return peer, got
}

func TestPluginRelayABrowserAbortKeepsThePeerUp(t *testing.T) {
	peer, got := heldPeer(t)
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "rotate", Mode: config.ModeRelay}}}}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.pluginRelay(httptest.NewRecorder(), pluginFormRequest("/internal/plugin/relay", "plugin=monedit&button=0&ids=7").WithContext(ctx))
	}()
	<-got
	cancel()
	<-done
	if pr := srv.peers.ProbeSeed("monedit"); pr.Conn == "down" {
		t.Error("an aborted click marked a healthy peer down")
	}
}

func TestPluginMountABrowserAbortKeepsThePeerUp(t *testing.T) {
	peer, got := heldPeer(t)
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: peer.URL}}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/plugins/monedit/crop", nil).WithContext(ctx))
	}()
	<-got
	cancel()
	<-done
	if pr := srv.peers.ProbeSeed("monedit"); pr.Conn == "down" {
		t.Error("an aborted page load marked a healthy peer down")
	}
}

func TestPluginRelayRefusesWhatItCannotRoute(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: "http://edit:1",
		Buttons: []config.PluginButton{
			{Slot: config.SlotDetailActions, Label: "crop", Mode: config.ModeOpen, Path: "/e"},
			{Slot: config.SlotBatchBar, Label: "send", Mode: config.ModeRelay, Path: "/r"},
		}}}

	cases := []struct {
		name, body, want string
	}{
		{"unknown peer", "plugin=nope&button=0&ids=7", "plugin nope is not available"},
		{"out of range", "plugin=monedit&button=4&ids=7", "unknown plugin button"},
		{"open button", "plugin=monedit&button=0&ids=7", "unknown plugin button"},
		{"empty scope", "plugin=monedit&button=1", "no images selected"},
	}
	for _, c := range cases {
		rr := relay(srv, c.body)
		if rr.Code != http.StatusNoContent {
			t.Errorf("%s: code = %d, want %d", c.name, rr.Code, http.StatusNoContent)
		}
		if got := rr.Header().Get("HX-Trigger"); !strings.Contains(got, c.want) {
			t.Errorf("%s: flash = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPluginRelayReportsAScopeItNarrowed(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req plugins.RelayRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = fmt.Fprintf(w, `{"ok":true,"message":"handled %d"}`, len(req.ImageIDs))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "sek", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "send", Mode: config.ModeRelay, Media: "image"}}}}
	for id, ft := range map[int64]string{1: "png", 2: "png", 3: "cbz"} {
		mustExec(t, srv.db().Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, ?, 1)`, id, strings.Repeat("f", 63)+strconv.FormatInt(id, 10), "/g/"+strconv.FormatInt(id, 10), ft)
	}

	rr := relay(srv, "plugin=monedit&button=0&ids=1&ids=2&ids=3")
	flash := rr.Header().Get("HX-Trigger")
	wantIn(t, flash, "handled 2")
	wantIn(t, flash, "2 of 3 sent; 1 not handled by monedit")

	rr = relay(srv, "plugin=monedit&button=0&ids=1&ids=2")
	if flash := rr.Header().Get("HX-Trigger"); strings.Contains(flash, "sent;") {
		t.Errorf("flash = %q, want no scope note on a full scope", flash)
	}
}

func TestPluginRelayCapsThePeerMessage(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"message":"` + strings.Repeat("é", 400) + `"}`))
	}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "loud", PeerToken: "sek", APIURL: peer.URL,
		Buttons: []config.PluginButton{{Slot: config.SlotBatchBar, Label: "shout", Mode: config.ModeRelay}}}}

	rr := relay(srv, "plugin=loud&button=0&ids=7")
	var trigger map[string]map[string]string
	mustUnmarshal(t, []byte(rr.Header().Get("HX-Trigger")), &trigger)
	if n := strings.Count(trigger["action-flash"]["text"], "é"); n != plugins.MessageMax {
		t.Errorf("flashed %d runes, want %d", n, plugins.MessageMax)
	}
}

func managedScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.sh")
	mustWriteFile(t, path, []byte("#!/bin/sh\n"+body), 0o755)
	return path
}

func waitForManaged(t *testing.T, srv *Server, name, want string) {
	t.Helper()
	for range 100 {
		if srv.peers.State(name) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s state = %q, want %q", name, srv.peers.State(name), want)
}

func TestManagedPluginRunsAndStops(t *testing.T) {
	script := managedScript(t, "echo hello from the plugin\ncat > /dev/null\n")
	srv := newTestServer(t)
	p := config.PluginConfig{Name: "monedit", PeerToken: "sek"}
	srv.cfg.Plugins = []config.PluginConfig{p}

	srv.peers.Start(plugins.Launch{Name: p.Name, Command: script})
	waitForManaged(t, srv, "monedit", "running")

	srv.peers.Stop("monedit")
	waitForManaged(t, srv, "monedit", "stopped")

	time.Sleep(100 * time.Millisecond)
	if got := srv.peers.State("monedit"); got != "stopped" {
		t.Errorf("state after stop = %q", got)
	}
}

func TestStoppingAManagedPluginTakesItsSurfacesDown(t *testing.T) {
	srv := newTestServer(t)
	installPluginFolder(t, srv, "monedit", "command = \"./run.sh\"\n", "cat > /dev/null\n")
	p := config.PluginConfig{Name: "monedit", PeerToken: "sek", APIURL: "http://127.0.0.1:1", Enabled: true}
	srv.cfg.Plugins = []config.PluginConfig{p}
	srv.peers.SetProbe("monedit", plugins.Probe{Conn: "ok", CheckedAt: time.Now()})

	stop := pluginFormRequest("/settings/plugins/monedit/stop", "")
	stop.SetPathValue("name", "monedit")
	srv.pluginStop(httptest.NewRecorder(), stop)
	if pr := srv.peers.ProbeSeed("monedit"); pr.Conn != "down" {
		t.Errorf("probe after stop = %+v, want down", pr)
	}
	if srv.peers.Usable(p) {
		t.Error("a stopped plugin still renders its buttons")
	}

	start := pluginFormRequest("/settings/plugins/monedit/start", "")
	start.SetPathValue("name", "monedit")
	srv.pluginStart(httptest.NewRecorder(), start)
	defer srv.peers.Stop("monedit")
	waitForManaged(t, srv, "monedit", "running")
	if pr := srv.peers.ProbeSeed("monedit"); pr.Conn != "" {
		t.Errorf("probe after start = %+v, want a cold cache", pr)
	}
	if !srv.peers.Usable(p) {
		t.Error("a started plugin still hides its buttons")
	}
}

func TestRemovingAPluginDropsItsPendingOffer(t *testing.T) {
	var notified string
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/remove" {
			mu.Lock()
			notified = r.Header.Get("Authorization")
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stub.Close()
	srv := newTestServer(t)
	installPluginFolder(t, srv, "monedit", "command = \"./run.sh\"\n", "cat > /dev/null\n")
	pairPeer(srv, "monedit")
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "edit-secret", APIURL: stub.URL, Enabled: true, Paused: true}}
	srv.peers.StartManaged(srv.effectivePlugins())
	defer srv.peers.Stop("monedit")
	waitForManaged(t, srv, "monedit", "running")
	if _, ok := srv.pairs.Create(peer.Req{Request: pairing.Request{App: "monedit"}, URL: stub.URL}); !ok {
		t.Fatal("offer refused")
	}

	w := postForm(t, srv, "/settings/plugins/monedit/remove", url.Values{})
	mustCode(t, w, http.StatusOK)

	waitForManaged(t, srv, "monedit", "stopped")
	if pending := srv.pairs.ListPending(); len(pending) != 0 {
		t.Errorf("pending = %+v, want the departing plugin's offer dropped", pending)
	}
	if pr := srv.peers.ProbeSeed("monedit"); pr.Conn != "down" {
		t.Errorf("probe = %+v, want down", pr)
	}
	if srv.pairedWith("monedit") {
		t.Error("the token survived the removal")
	}
	if p := srv.cfgSnapshot().FindPlugin("monedit"); p == nil || p.Enabled || p.PeerToken != "" {
		t.Errorf("block = %+v, want it kept disabled and unpaired", p)
	}
	mu.Lock()
	defer mu.Unlock()
	if notified != "Bearer edit-secret" {
		t.Errorf("notify carried %q, want a paused plugin told with its token", notified)
	}
}

func TestPluginPauseKeepsTheTokenAndStopsTheButtons(t *testing.T) {
	srv := newTestServer(t)
	pairPeer(srv, "monedit")
	srv.cfg.Plugins = []config.PluginConfig{{Name: "monedit", PeerToken: "edit-secret", APIURL: "http://edit:7301"}}
	srv.peers.SetProbe("monedit", plugins.Probe{Conn: "ok", CheckedAt: time.Now()})

	mustCode(t, postForm(t, srv, "/settings/plugins/monedit/pause", url.Values{"paused": {"1"}}), http.StatusOK)
	p := *srv.cfgSnapshot().FindPlugin("monedit")
	if !p.Paused || p.PeerToken != "edit-secret" || !srv.pairedWith("monedit") {
		t.Fatalf("paused block = %+v paired %v, want paused with both tokens kept", p, srv.pairedWith("monedit"))
	}
	if srv.peers.Usable(p) || srv.pluginBase(p) != "" {
		t.Error("a paused plugin still offers its buttons or an address to call")
	}
	if row, _ := pluginRowNamed(srv, "monedit"); row.Conn != "paused" {
		t.Errorf("row conn = %q, want paused", row.Conn)
	}

	mustCode(t, postForm(t, srv, "/settings/plugins/monedit/pause", url.Values{"paused": {"0"}}), http.StatusOK)
	if p := *srv.cfgSnapshot().FindPlugin("monedit"); p.Paused || !srv.peers.Usable(p) {
		t.Errorf("resumed block = %+v, want its buttons back", p)
	}

	mustCode(t, postForm(t, srv, "/settings/plugins/monloader/pause", url.Values{"paused": {"1"}}), http.StatusOK)
	if !srv.cfgSnapshot().Monloader.Paused {
		t.Error("the companion's row did not pause the link")
	}
}

func TestManagedPluginSurvivesAnOversizedLogLine(t *testing.T) {
	script := managedScript(t, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x\n", plugins.LogLineMax+1)+
		"echo\nyes 'the tail the drain has to keep reading' | head -n 20000\nexit 1\n")
	srv := newTestServer(t)
	p := config.PluginConfig{Name: "loud", PeerToken: "sek"}
	srv.cfg.Plugins = []config.PluginConfig{p}

	srv.peers.Start(plugins.Launch{Name: p.Name, Command: script})
	t.Cleanup(func() { srv.peers.Stop("loud") })
	waitForManaged(t, srv, "loud", "restarting (1)")
}

func TestDisableDuringBackoffIsNotUndoneByAStaleStopChannel(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	script := managedScript(t, "echo run >> "+marker+"\nexit 1\n")
	srv := newTestServer(t)
	p := config.PluginConfig{Name: "flaky", PeerToken: "sek"}
	srv.cfg.Plugins = []config.PluginConfig{p}
	e := plugins.Launch{Name: p.Name, Command: script}

	srv.peers.Start(e)
	waitForManaged(t, srv, "flaky", "restarting (1)")

	time.Sleep(100 * time.Millisecond)
	srv.peers.Start(e)
	srv.peers.Stop("flaky")

	time.Sleep(plugins.BackoffMin + 500*time.Millisecond)
	runs, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(runs), "run"); got != 1 {
		t.Errorf("the script ran %d times, want only the one before the stop", got)
	}
	if got := srv.peers.State("flaky"); got != "stopped" {
		t.Errorf("state = %q, want stopped", got)
	}
}

func TestManagedPluginRestartsAfterACrash(t *testing.T) {
	script := managedScript(t, "exit 1\n")
	srv := newTestServer(t)
	p := config.PluginConfig{Name: "flaky", PeerToken: "sek"}
	srv.cfg.Plugins = []config.PluginConfig{p}

	srv.peers.Start(plugins.Launch{Name: p.Name, Command: script})
	waitForManaged(t, srv, "flaky", "restarting (1)")
	srv.peers.Stop("flaky")
}

func TestPairingCannotMakeAPluginRunnable(t *testing.T) {
	srv := newTestServer(t)

	code, out := pairOffer(t, srv, `{"app":"monedit","url":"http://edit:1","peer_token":"sek","command":"/bin/sh"}`)
	if code != http.StatusOK {
		t.Fatalf("request: %d %v", code, out)
	}
	if !srv.pairs.SetState(out["request_id"], pairing.Approved) {
		t.Fatal("approve failed")
	}
	pairStatus(t, srv, out["request_id"])

	p, ok := srv.effective("monedit")
	if !ok {
		t.Fatal("the claim wrote no block")
	}
	if p.Launch.Command != "" || p.Installed {
		t.Errorf("the wire produced something to launch: %+v", p)
	}
}

func installPluginFolder(t *testing.T, srv *Server, name, manifest, script string) string {
	t.Helper()
	dir := filepath.Join(srv.pluginsDir(), name)
	mustMkdirAll(t, dir, 0o755)
	mustWriteFile(t, filepath.Join(dir, "plugin.toml"), []byte(manifest), 0o644)
	if script != "" {
		mustWriteFile(t, filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"+script), 0o755)
	}
	return dir
}

func TestDiscoveredPluginStaysStoppedUntilEnabled(t *testing.T) {
	srv := newTestServer(t)
	dir := installPluginFolder(t, srv, "dropped", "command = \"./run.sh\"\n",
		"pwd > where.txt\necho \"$MONBOORU_URL\" > url.txt\ncat > /dev/null\n")

	srv.peers.StartManaged(srv.effectivePlugins())
	time.Sleep(50 * time.Millisecond)
	if got := srv.peers.State("dropped"); got != "stopped" {
		t.Fatalf("boot started a plugin nobody enabled: state %q", got)
	}
	row, found := pluginRowNamed(srv, "dropped")
	if !found || !row.Installed || row.Enabled || row.RunState != "stopped" || !strings.Contains(row.Command, dir) {
		t.Fatalf("discovered row = %+v, found %v", row, found)
	}

	srv.cfg.Plugins = []config.PluginConfig{{Name: "dropped", Enabled: true}}
	srv.peers.StartManaged(srv.effectivePlugins())
	waitForManaged(t, srv, "dropped", "running")
	defer srv.peers.Stop("dropped")

	where := waitForFileContent(t, filepath.Join(dir, "where.txt"))
	if where != dir {
		t.Errorf("plugin cwd = %q, want %q", where, dir)
	}
	want := "http://" + srv.cfg.Server.BindAddress
	if got := waitForFileContent(t, filepath.Join(dir, "url.txt")); got != want {
		t.Errorf("MONBOORU_URL = %q, want %q", got, want)
	}
}

func pluginRowNamed(srv *Server, name string) (pluginRowView, bool) {
	for _, row := range srv.pluginRows() {
		if row.Name == name {
			return row, true
		}
	}
	return pluginRowView{}, false
}

func waitForFileContent(t *testing.T, path string) string {
	t.Helper()
	for range 100 {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was never written", path)
	return ""
}

func TestABootDoesNotRestoreADisabledPluginsButtons(t *testing.T) {
	srv := newTestServer(t)
	installPluginFolder(t, srv, "dropped", "command = \"./run.sh\"\n", "cat > /dev/null\n")
	p := config.PluginConfig{Name: "dropped", PeerToken: "sek", APIURL: "http://127.0.0.1:1",
		Buttons: []config.PluginButton{{Slot: config.SlotDetailActions, Label: "edit", Mode: config.ModeRelay}}}
	srv.cfg.Plugins = []config.PluginConfig{p}

	srv.peers.StartManaged(srv.effectivePlugins())
	if srv.peers.Usable(p) {
		t.Error("a disabled plugin still offers buttons that can only fail")
	}

	srv.cfg.Plugins[0].Enabled = true
	srv.peers.ClearProbe("dropped")
	srv.peers.StartManaged(srv.effectivePlugins())
	defer srv.peers.Stop("dropped")
	if !srv.peers.Usable(srv.cfg.Plugins[0]) {
		t.Error("an enabled plugin came up with its buttons already down")
	}
}

func TestStartPersistsEnabledForDiscovered(t *testing.T) {
	srv := newTestServer(t)
	installPluginFolder(t, srv, "dropped", "command = \"./run.sh\"\n", "cat > /dev/null\n")

	if p, ok := srv.effective("dropped"); !ok || !p.Installed || p.Enabled {
		t.Fatalf("effective = %+v, ok %v", p, ok)
	}
	srv.setPluginEnabled("dropped", true)
	if blk := srv.cfg.FindPlugin("dropped"); blk == nil || !blk.Enabled {
		t.Fatalf("enable did not land on a block: %+v", srv.cfg.Plugins)
	}
	srv.setPluginEnabled("dropped", false)
	if blk := srv.cfg.FindPlugin("dropped"); blk == nil || blk.Enabled {
		t.Errorf("disable did not clear the block: %+v", srv.cfg.Plugins)
	}
}

func TestDiscoveredCommandIsAbsoluteUnderARelativeConfigPath(t *testing.T) {
	srv := newTestServerAt(t, false, nil, func(dir string) string {
		t.Chdir(dir)
		return "monbooru.toml"
	})

	installPluginFolder(t, srv, "relcmd", "command = \"./run.sh\"\n", "exit 0\n")
	p, ok := srv.effective("relcmd")
	if !ok {
		t.Fatal("the dropped folder was not discovered")
	}
	if !filepath.IsAbs(p.Launch.Command) {
		t.Errorf("command = %q, want an absolute path", p.Launch.Command)
	}
	if _, err := os.Stat(p.Launch.Command); err != nil {
		t.Errorf("command does not resolve: %v", err)
	}
}

func TestPluginRowsSkipResidueBlocks(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Plugins = []config.PluginConfig{{Name: "gone", Enabled: true}}

	if _, found := pluginRowNamed(srv, "gone"); found {
		t.Error("a block with nothing to show got a row")
	}
}

func TestPluginRowsShowARefusedFolder(t *testing.T) {
	srv := newTestServer(t)
	installPluginFolder(t, srv, "futureplug", fmt.Sprintf("command = \"./run.sh\"\ncontract = %d\n", plugins.Contract+1), "cat > /dev/null\n")

	row, found := pluginRowNamed(srv, "futureplug")
	if !found || row.Installed || row.Command != "" || row.SelfManaged {
		t.Fatalf("refused row = %+v, found %v", row, found)
	}
	rr := httptest.NewRecorder()
	srv.renderPluginRows(rr, httptest.NewRequest("GET", "/settings", nil), false)
	body := rr.Body.String()
	want := fmt.Sprintf("refused - it needs plugin contract %d, this monbooru speaks %d", plugins.Contract+1, plugins.Contract)
	if !strings.Contains(body, want) {
		t.Errorf("rows do not carry %q:\n%s", want, body)
	}
	if strings.Contains(body, "/settings/plugins/futureplug/start") {
		t.Error("a refused folder offers Enable")
	}
}

func TestPluginScopeKeepsTheMediaItsButtonNames(t *testing.T) {
	srv := newTestServer(t)
	still := seedImage(t, srv, "still.png", 8, 8)
	archive := seedImage(t, srv, "archive.png", 9, 9)
	mustExec(t, srv.db().Write, `UPDATE images SET file_type = 'cbz' WHERE id = ?`, archive)
	ids := []int64{still, archive, 404}

	if got := srv.scopeForButton(config.PluginButton{Media: "archive"}, slices.Clone(ids)); !slices.Equal(got, []int64{archive}) {
		t.Errorf("archive scope = %v, want [%d]", got, archive)
	}
	if got := srv.scopeForButton(config.PluginButton{}, slices.Clone(ids)); !slices.Equal(got, ids) {
		t.Errorf("unscoped = %v, want every id", got)
	}
}
