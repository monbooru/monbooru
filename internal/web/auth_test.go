package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"golang.org/x/crypto/bcrypt"
)

func setTestPassword(t *testing.T, srv *Server, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv.cfgMu.Lock()
	srv.cfg.Auth.EnablePassword = true
	srv.cfg.Auth.PasswordHash = string(hash)
	srv.cfg.Auth.SessionLifetimeDays = 1
	srv.cfgMu.Unlock()
}

func TestLoginPost_AcceptsCorrectPassword(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")

	w := postLogin(t, srv, "s3cret", "10.0.0.10:443")

	mustCode(t, w, http.StatusSeeOther)
	var sess string
	for _, c := range w.Result().Cookies() {
		if c.Name == "monbooru_session" {
			sess = c.Value
		}
	}
	if sess == "" {
		t.Fatal("login did not set monbooru_session cookie")
	}
	wantNotIn(t, w.Header().Get("Set-Cookie"), "Secure")
	if !srv.sessions.Valid(sess) {
		t.Error("session store should contain the issued session ID")
	}
}

func TestLoginPost_BehindALoopbackTLSProxyTheCookieIsSecure(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")
	w := postLogin(t, srv, "s3cret", "127.0.0.1:1111", [2]string{"X-Forwarded-Proto", "https"})

	mustCode(t, w, http.StatusSeeOther)
	c := w.Header().Get("Set-Cookie")
	wantIn(t, c, "monbooru_session=")
	wantIn(t, c, "Secure")
}

func TestLoginPost_RejectsWrongPasswordAndRecordsFailure(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")

	w := postLogin(t, srv, "WRONG", "10.0.0.11:443")

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Invalid password")
	if srv.loginRL.Wait("10.0.0.11") == 0 {
		t.Error("a bad login left its IP free to try again at once")
	}
}

func TestLoginPost_RateLimitedAfterRepeatedFailures(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")

	for range 3 {
		srv.loginRL.Fail("10.0.0.12")
	}

	w := postLogin(t, srv, "s3cret", "10.0.0.12:443")

	wantIn(t, w.Body.String(), "Wait 4 seconds before trying again.")
	for _, c := range w.Result().Cookies() {
		if c.Name == "monbooru_session" {
			t.Error("rate-limited login must not issue a session cookie")
		}
	}
}

func TestLoginPost_OneFailureNamesTheSecondLeft(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")
	srv.loginRL.Fail("10.0.0.13")

	w := postLogin(t, srv, "s3cret", "10.0.0.13:443")

	wantIn(t, w.Body.String(), "Wait 1 second before trying again.")
	wantNotIn(t, w.Body.String(), "Too many")
}

func TestLoginPost_DisabledAuthRedirectsHome(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"password": {"anything"}}
	w := postForm(t, srv, "/login", form)

	wantCode(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

func TestLogoutPost_ClearsSession(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")
	h := srv.Handler()
	sessID := srv.sessions.Create(1)

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken(sessID))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	wantCode(t, w, http.StatusSeeOther)
	if srv.sessions.Valid(sessID) {
		t.Error("session should be gone after logout")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "monbooru_session" || cookies[0].MaxAge >= 0 ||
		!cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Errorf("logout cookies = %+v, want one expired HttpOnly SameSite=Lax monbooru_session", cookies)
	}
}

func TestSettingsPasswordPost_RejectsWrongCurrent(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "initial")

	sessID := srv.sessions.Create(1)

	w := postFormAs(t, srv, sessID, "/settings/auth/password", url.Values{
		"current_password": {"WRONG"},
		"new_password":     {"replacement"},
	})

	wantIn(t, w.Body.String(), "Current password is incorrect")
	if err := bcrypt.CompareHashAndPassword([]byte(srv.cfg.Auth.PasswordHash), []byte("initial")); err != nil {
		t.Error("password hash should still verify against 'initial' after a rejected change")
	}
}

func TestSettingsPasswordPost_ChecksAHashLeftWithAuthOff(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "initial")
	srv.cfg.Auth.EnablePassword = false

	wantIn(t, do(srv, "GET", "/settings").Body.String(), `name="current_password"`)
	for _, current := range []string{"", "WRONG"} {
		form := url.Values{"current_password": {current}, "new_password": {"taken"}}
		wantIn(t, postForm(t, srv, "/settings/auth/password", form).Body.String(), "Current password is incorrect")
	}
	if srv.cfg.Auth.EnablePassword || bcrypt.CompareHashAndPassword([]byte(srv.cfg.Auth.PasswordHash), []byte("initial")) != nil {
		t.Fatal("a change without the current password took the password over")
	}
	form := url.Values{"current_password": {"initial"}, "new_password": {"replacement"}}
	postForm(t, srv, "/settings/auth/password", form)
	if bcrypt.CompareHashAndPassword([]byte(srv.cfg.Auth.PasswordHash), []byte("replacement")) != nil {
		t.Error("the right current password did not change it")
	}
}

func TestSettingsPasswordPost_EndsOtherSessions(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "initial")
	mine := srv.sessions.Create(1)
	other := srv.sessions.Create(1)

	postFormAs(t, srv, mine, "/settings/auth/password", url.Values{"current_password": {"initial"}, "new_password": {"replacement"}})

	if srv.sessions.Valid(other) {
		t.Error("another session outlived the password change")
	}
	if !srv.sessions.Valid(mine) {
		t.Error("the session that changed the password was logged out")
	}
}

func TestSettingsPasswordPost_SetsPassword(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{
		"new_password": {"freshpass"},
	}
	postForm(t, srv, "/settings/auth/password", form)

	if !srv.cfg.Auth.EnablePassword {
		t.Error("EnablePassword should be true after setting a password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(srv.cfg.Auth.PasswordHash), []byte("freshpass")); err != nil {
		t.Errorf("password hash should verify against 'freshpass', got err %v", err)
	}
}

func TestSettingFirstPasswordKeepsTheSetterLoggedIn(t *testing.T) {
	srv := newTestServer(t)

	w := postHTMXForm(t, srv, "/settings/auth/password", url.Values{"new_password": {"freshpass"}})

	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("the page is not reloaded onto the new session: %v", w.Header())
	}
	var sess *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "monbooru_session" {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie handed to the setter")
	}
	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(sess)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	wantCode(t, rr, http.StatusOK)
}

func TestSettingsPasswordPost_RejectsEmptyNewPassword(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"new_password": {""}}
	w := postForm(t, srv, "/settings/auth/password", form)

	wantIn(t, w.Body.String(), "New password required")
	if srv.cfg.Auth.EnablePassword {
		t.Error("empty new_password must not enable auth")
	}
}

func TestSettingsRemovePasswordPost_RequiresCorrectCurrent(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "original")
	sessID := srv.sessions.Create(1)

	w := postFormAs(t, srv, sessID, "/settings/auth/remove-password", url.Values{"current_password": {"WRONG"}})

	wantIn(t, w.Body.String(), "Current password is incorrect")
	if !srv.cfg.Auth.EnablePassword {
		t.Error("auth must remain enabled after a rejected remove")
	}

	postFormAs(t, srv, sessID, "/settings/auth/remove-password", url.Values{"current_password": {"original"}})

	if srv.cfg.Auth.EnablePassword {
		t.Error("auth should be disabled after successful remove")
	}
	if srv.cfg.Auth.PasswordHash != "" {
		t.Error("PasswordHash should be cleared after remove")
	}
}

func TestSessionMiddleware_PassesAuthenticatedRequest(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")

	sessID := srv.sessions.Create(1)
	w := do(srv, "GET", "/settings", [2]string{"Cookie", "monbooru_session=" + sessID})

	wantCode(t, w, http.StatusOK)
}

func TestSessionMiddleware_RedirectsUnauthenticated(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")

	for target, want := range map[string]string{
		"/":                             "/login",
		"/settings":                     "/login?next=%2Fsettings",
		"/images/416?back_q=cat&page=2": "/login?next=%2Fimages%2F416%3Fback_q%3Dcat%26page%3D2",
	} {
		w := do(srv, "GET", target)
		wantCode(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != want {
			t.Errorf("GET %s: Location = %q, want %q", target, loc, want)
		}
	}
}

func TestLoginPost_ReturnsToTheRequestedPageOnly(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")

	for next, want := range map[string]string{
		"/images/416?back_q=cat": "/images/416?back_q=cat",
		"":                       "/",
		"//evil.example":         "/",
		`/\evil.example`:         "/",
		"https://evil.example/":  "/",
		"/\t/evil.example":       "/",
		"javascript:alert(1)":    "/",
	} {
		form := url.Values{"password": {"s3cret"}, "next": {next}}
		w := postForm(t, srv, "/login", form)
		mustCode(t, w, http.StatusSeeOther)
		if loc := w.Header().Get("Location"); loc != want {
			t.Errorf("next=%q: Location = %q, want %q", next, loc, want)
		}
	}
}

func TestSessionMiddleware_HTMXGets401WithoutARedirect(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")
	h := srv.Handler()

	for _, method := range []string{"GET", "POST"} {
		req := httptest.NewRequest(method, "/internal/job/status", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		wantCode(t, w, http.StatusUnauthorized)
		if got := w.Header().Get("HX-Redirect"); got != "" {
			t.Errorf("%s: HX-Redirect = %q; a request the page makes on its own must not navigate it", method, got)
		}
	}
}

func TestCSRFMiddleware_AcceptsHeaderToken(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/settings/general", strings.NewReader("page_size=25"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	wantCode(t, w, http.StatusOK)
}

func TestCSRFMiddleware_AcceptsFormFieldToken(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	form := url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "page_size": {"25"}}
	req := httptest.NewRequest("POST", "/settings/general", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	wantCode(t, w, http.StatusOK)
}

func TestCSRFMiddleware_RejectsMissingToken(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/settings/general", strings.NewReader("page_size=25"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	wantCode(t, w, http.StatusForbidden)
}

func TestCSRFMiddleware_RejectsForeignToken(t *testing.T) {
	srvA := newTestServer(t)
	srvB := newTestServer(t)

	req := httptest.NewRequest("POST", "/settings/general", strings.NewReader("page_size=25"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srvB.sessions.CSRFToken("anon"))
	w := httptest.NewRecorder()
	srvA.Handler().ServeHTTP(w, req)

	wantCode(t, w, http.StatusForbidden)
}

func TestCSRFMiddleware_AStaleTokenPassesOnlyFromThisOrigin(t *testing.T) {
	srvA := newTestServer(t)
	srvB := newTestServer(t)
	for _, tc := range []struct {
		header, value string
		want          bool
	}{
		{"Origin", "http://example.com", true},
		{"Sec-Fetch-Site", "same-origin", true},
		{"Origin", "http://evil.example", false},
		{"Sec-Fetch-Site", "cross-site", false},
		{"", "", false},
	} {
		req := httptest.NewRequest("POST", "/settings/general", strings.NewReader("page_size=25"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srvB.sessions.CSRFToken("anon"))
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		srvA.Handler().ServeHTTP(w, req)
		if got := w.Code != http.StatusForbidden; got != tc.want {
			t.Errorf("%s: %q with a stale token: status %d, passed=%v want %v", tc.header, tc.value, w.Code, got, tc.want)
		}
	}
}

func TestCSRFMiddleware_APIRoutesBypass(t *testing.T) {
	srv := newTestServer(t)
	setTokens(srv, []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash("bearer-1"), Scopes: config.AllScopes}})
	h := srv.Handler()

	req := httptest.NewRequest("DELETE", "/api/v1/images/1", nil)
	req.Header.Set("Authorization", "Bearer bearer-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Errorf("API routes must bypass CSRF, got 403")
	}
}

func TestSameOriginReferer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		host string
		ref  string
		want string
	}{
		{"no_referer", "host.local", "", "/"},
		{"same_host", "host.local", "http://host.local/foo", "http://host.local/foo"},
		{"different_host", "host.local", "http://other.local/foo", "/"},
		{"javascript_scheme", "host.local", "javascript:alert(1)", "/"},
		{"data_scheme", "host.local", "data:text/html,<script>alert(1)</script>", "/"},
		{"relative", "host.local", "/some/path", "/some/path"},
		{"protocol_relative", "host.local", "//evil.com/", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/internal/sync", nil)
			req.Host = tc.host
			if tc.ref != "" {
				req.Header.Set("Referer", tc.ref)
			}
			if got := sameOriginReferer(req); got != tc.want {
				t.Errorf("sameOriginReferer(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func revokeToken(srv *Server, id string) string {
	req := httptest.NewRequest("DELETE", "/settings/auth/tokens/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.settingsTokenRevoke(w, req)
	return w.Body.String()
}

func TestRevokingAPluginTokenRemovesItsPairing(t *testing.T) {
	var teardown string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/remove" {
			teardown = r.Header.Get("Authorization")
		}
	}))
	defer peer.Close()
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Plugins = []config.PluginConfig{{Name: "relay", PeerToken: "sek", APIURL: peer.URL}}
	})
	tok := pairPeer(srv, "relay")

	body := revokeToken(srv, tok.ID)

	if srv.pairedWith("relay") {
		t.Error("the token outlived its revoke")
	}
	if teardown != "Bearer sek" {
		t.Errorf("the peer was called back with %q", teardown)
	}
	wantIn(t, body, "relay pairing removed")
	wantIn(t, body, `id="plugin-rows"`)
}

func TestRemovingAPairingTellsNoPeerWhenTheSaveFails(t *testing.T) {
	for _, app := range []string{"relay", monloader.App} {
		for _, route := range []string{"revoke", "remove"} {
			var told atomic.Bool
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/pair/remove" {
					told.Store(true)
				}
			}))
			srv := newTestServerWith(t, false, func(c *config.Config) {
				c.Plugins = []config.PluginConfig{{Name: "relay", PeerToken: "sek", APIURL: stub.URL}}
				c.Monloader.APIURL, c.Monloader.APIToken = stub.URL, "sek"
			})
			tok := pairPeer(srv, app)
			notADir := filepath.Join(t.TempDir(), "file")
			mustWriteFile(t, notADir, nil, 0o644)
			srv.configPath = filepath.Join(notADir, "monbooru.toml")

			var body string
			if route == "revoke" {
				body = revokeToken(srv, tok.ID)
			} else {
				w := httptest.NewRecorder()
				srv.pluginPairRemove(w, pluginRemoveRequest(app))
				body = w.Body.String()
			}
			stub.Close()

			if !srv.pairedWith(app) {
				t.Errorf("%s %s: the pairing went with an unsaved change", route, app)
			}
			if told.Load() {
				t.Errorf("%s %s: the peer was told of an unsaved change", route, app)
			}
			if !strings.Contains(body, "Could not save: ") || strings.Contains(body, "flash-ok") {
				t.Errorf("%s %s: body = %s, want the save error and no ok flash", route, app, body)
			}
		}
	}
}

func TestRevokingAPeerWithNoPeerToken(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Plugins = []config.PluginConfig{{Name: "script"}}
	})
	tok := pairPeer(srv, "script")

	body := revokeToken(srv, tok.ID)

	if srv.pairedWith("script") {
		t.Errorf("the token outlived its revoke: %s", body)
	}
	wantIn(t, body, "script pairing removed")
}

func TestRevokingTheMonloaderTokenWarnsWhenUnreachable(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) {
		c.Monloader.APIURL = "http://127.0.0.1:1"
		c.Monloader.APIToken = "sek"
	})
	tok := pairPeer(srv, monloader.App)

	body := revokeToken(srv, tok.ID)

	if srv.pairedWith(monloader.App) || srv.cfg.Monloader.APIToken != "" {
		t.Errorf("the monloader pairing outlived its revoke: %+v", srv.cfg.Monloader)
	}
	wantIn(t, body, "could not reach monloader")
}

func TestPairedTokenPrivilegesLocked(t *testing.T) {
	srv := newTestServer(t)
	tok := pairPeer(srv, "monloader")

	form := url.Values{"scope": {config.ScopeRead}}
	req := httptest.NewRequest("POST", "/settings/auth/tokens/"+tok.ID+"/privileges", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", tok.ID)
	w := httptest.NewRecorder()
	srv.settingsTokenPrivilegesPost(w, req)

	if got := strings.Join(srv.cfg.Auth.Tokens[0].Scopes, " "); got != strings.Join(config.AllScopes, " ") {
		t.Fatalf("paired token scopes changed to %q; want them unchanged (manage via pairing)", got)
	}
	wantIn(t, w.Body.String(), "pairing")
}

func wantBooruURL(srv *Server, tok apitoken.Token) string {
	return "http://example.com" + api.BooruPath(srv.cfg.Galleries[0].Name, api.BooruKey(tok.TokenHash, srv.cfg.Galleries[0].Name))
}

// newBooruEndpointToken is a token as creating the endpoint makes it.
func newBooruEndpointToken() apitoken.Token {
	tok, _ := apitoken.New(booruTokenName, []string{config.ScopeRead})
	tok.Class = api.BooruClass
	return tok
}

// shareBooruGalleries turns every gallery on, so its address shows.
func shareBooruGalleries(srv *Server) {
	srv.cfgMu.Lock()
	for i := range srv.cfg.Galleries {
		srv.cfg.Galleries[i].Booru = true
	}
	srv.cfgMu.Unlock()
}

func TestBooruEndpointInSettings(t *testing.T) {
	tok := newBooruEndpointToken()
	cases := []struct {
		name         string
		tokens       []apitoken.Token
		want, absent []string
	}{
		{"no endpoint", nil,
			[]string{"[+ create danbooru compatible endpoint]"},
			[]string{"Remove endpoint"}},
		{"an endpoint", []apitoken.Token{tok},
			[]string{"Remove endpoint", "the Danbooru compatible addresses are disabled", `status-badge status-ok">danbooru</span>`, booruRevokeNote},
			[]string{"[+ create danbooru compatible endpoint]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			setTokens(srv, tc.tokens)
			shareBooruGalleries(srv)
			body := do(srv, "GET", "/settings").Body.String()
			for _, want := range tc.want {
				wantIn(t, body, want)
			}
			for _, absent := range tc.absent {
				if strings.Contains(body, absent) {
					t.Errorf("the page shows %s", absent)
				}
			}
			if got := strings.Contains(body, wantBooruURL(srv, tok)); got != (len(tc.tokens) > 0) {
				t.Errorf("address shown = %v", got)
			}
		})
	}

	// Without an endpoint there is no gallery to turn on.
	srv := newTestServer(t)
	body := postHTMXForm(t, srv, "/settings/auth/booru-gallery", url.Values{"gallery": {"default"}, "on": {"1"}}).Body.String()
	wantIn(t, body, "create the Danbooru compatible endpoint first")
	if srv.cfg.Galleries[0].Booru {
		t.Error("a gallery turned on without an endpoint")
	}
}

func TestCreatingTheBooruEndpoint(t *testing.T) {
	srv := newTestServer(t)
	body := postHTMXForm(t, srv, "/settings/auth/booru-endpoint", url.Values{}).Body.String()
	wantIn(t, body, "endpoint created")
	tok := srv.cfg.Auth.Tokens.ByClass(api.BooruClass)
	if tok == nil || len(tok.Scopes) != 1 || !tok.HasScope(config.ScopeRead) {
		t.Fatalf("the endpoint's token = %+v, want one locked to read", tok)
	}
	// Both blocks redraw: the token shows in API Tokens, the block offers
	// its galleries, none of them on yet.
	wantIn(t, body, `id="auth-tokens" hx-swap-oob="true"`)
	wantIn(t, body, `id="booru-endpoints" hx-swap-oob="true"`)
	wantIn(t, body, "Remove endpoint")
	if srv.cfg.Galleries[0].Booru {
		t.Error("a new endpoint shares a gallery unasked")
	}

	body = postHTMXForm(t, srv, "/settings/auth/booru-endpoint", url.Values{}).Body.String()
	wantIn(t, body, "already exists")
	if n := len(srv.cfg.Auth.Tokens); n != 1 {
		t.Errorf("%d tokens after creating twice, want 1", n)
	}
}

func TestRemovingTheBooruEndpoint(t *testing.T) {
	cases := []struct {
		name, flash string
		remove      func(*testing.T, *Server, apitoken.Token) string
	}{
		{"its button", "endpoint removed", func(t *testing.T, s *Server, _ apitoken.Token) string {
			return removeBooruEndpoint(t, s)
		}},
		{"revoking its token", "Token revoked", func(_ *testing.T, s *Server, tok apitoken.Token) string { return revokeToken(s, tok.ID) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			tok := newBooruEndpointToken()
			reader, _ := apitoken.New("reader", []string{config.ScopeRead})
			setTokens(srv, []apitoken.Token{reader, tok})
			shareBooruGalleries(srv)
			body := tc.remove(t, srv, tok)
			wantIn(t, body, tc.flash)
			wantIn(t, body, "[+ create danbooru compatible endpoint]")
			if srv.cfg.Auth.Tokens.ByClass(api.BooruClass) != nil || srv.cfg.Auth.Tokens.ByID(reader.ID) == nil {
				t.Errorf("tokens left: %+v, want only reader", srv.cfg.Auth.Tokens)
			}
			// A new endpoint starts with none shared.
			if srv.cfg.Galleries[0].Booru {
				t.Error("the gallery still answers")
			}
		})
	}

	wantIn(t, removeBooruEndpoint(t, newTestServer(t)), "There is no Danbooru compatible endpoint")
}

func removeBooruEndpoint(t *testing.T, srv *Server) string {
	return sendForm(t, srv, "DELETE", "/settings/auth/booru-endpoint", url.Values{}, [2]string{"HX-Request", "true"}).Body.String()
}

func TestBooruEndpointTokenPrivilegesLocked(t *testing.T) {
	srv := newTestServer(t)
	tok := newBooruEndpointToken()
	setTokens(srv, []apitoken.Token{tok})
	req := httptest.NewRequest("GET", "/settings/auth/tokens/"+tok.ID+"/privileges", nil)
	req.SetPathValue("id", tok.ID)
	w := httptest.NewRecorder()
	srv.settingsTokenPrivilegesGet(w, req)
	wantIn(t, w.Body.String(), "Managed by the Danbooru compatible endpoint.")

	body := setTokenScopes(t, srv, tok.ID, config.ScopeRead, config.ScopeWrite)
	wantIn(t, body, "managed by the Danbooru compatible endpoint")
	if got := srv.cfg.Auth.Tokens.ByID(tok.ID).Scopes; len(got) != 1 {
		t.Errorf("scopes = %v, want read only", got)
	}
}

func setTokenScopes(t *testing.T, srv *Server, id string, scopes ...string) string {
	return postHTMXForm(t, srv, "/settings/auth/tokens/"+id+"/privileges", url.Values{"scope": scopes}).Body.String()
}

func TestBooruAddressesAPhoneCannotReach(t *testing.T) {
	tok := newBooruEndpointToken()
	cases := []struct {
		name, bind, host, want string
	}{
		{"reachable", "0.0.0.0:8455", "192.0.2.1:8455", ""},
		{"listening on this computer only", "127.0.0.1:8455", "localhost:8455", "only answers this computer"},
		{"a page opened on localhost", "0.0.0.0:8455", "localhost:8455", "use localhost"},
		{"a page opened on 127.0.0.1", "0.0.0.0:8455", "127.0.0.1:8455", "use localhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWith(t, false, func(c *config.Config) { c.Server.BindAddress = tc.bind })
			setTokens(srv, []apitoken.Token{tok})
			shareBooruGalleries(srv)
			req := httptest.NewRequest("GET", "/settings", nil)
			req.Host = tc.host
			got := srv.booruEndpoints(req).Unreachable
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("warning %q, want one with %q", got, tc.want)
			}
		})
	}
}

func TestTurningABooruGalleryOn(t *testing.T) {
	tok := newBooruEndpointToken()
	cases := []struct {
		name, gallery, on string
		before, want      bool
		flash             string
	}{
		{"on", "default", "1", false, true, "answers at its Danbooru compatible address"},
		{"off", "default", "", true, false, "no longer answers"},
		{"not an unknown gallery", "no_such", "1", false, false, "Could not save"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			setTokens(srv, []apitoken.Token{tok})
			srv.cfg.Galleries[0].Booru = tc.before
			form := url.Values{"gallery": {tc.gallery}, "on": {tc.on}}
			body := postHTMXForm(t, srv, "/settings/auth/booru-gallery", form).Body.String()
			wantIn(t, body, tc.flash)
			if got := srv.cfg.Galleries[0].Booru; got != tc.want {
				t.Errorf("default turned on = %v, want %v", got, tc.want)
			}
			if tc.flash == "Could not save" {
				return
			}
			wantIn(t, body, `id="booru-endpoints" hx-swap-oob="true"`)
			if got := strings.Contains(body, wantBooruURL(srv, tok)); got != tc.want {
				t.Errorf("address shown = %v, want %v", got, tc.want)
			}
		})
	}
}

// The layer checks its own key, so a login in the web UI does not turn
// the apps away.
func TestBooruLayerAnswersPastTheLogin(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")
	tok := newBooruEndpointToken()
	setTokens(srv, []apitoken.Token{tok})
	shareBooruGalleries(srv)
	path := api.BooruPath(srv.cfg.Galleries[0].Name, api.BooruKey(tok.TokenHash, srv.cfg.Galleries[0].Name))
	cases := []struct {
		name, path string
		code       int
	}{
		{"its address", path + "/posts.json", http.StatusOK},
		{"a wrong key", api.BooruPath(srv.cfg.Galleries[0].Name, strings.Repeat("0", 32)) + "/posts.json", http.StatusUnauthorized},
		{"the web UI still", "/", http.StatusSeeOther},
	}
	for _, tc := range cases {
		if w := do(srv, "GET", tc.path); w.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.code)
		}
	}
}

func TestHealth_AnswersWithPasswordAuthOn(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")

	w := do(srv, "GET", "/health")
	if w.Code != http.StatusOK {
		t.Fatalf("/health expected 200, got %d (Location %q)", w.Code, w.Header().Get("Location"))
	}
	var body struct {
		App, Status string
		API         int
		MinPeerAPI  int `json:"min_peer_api"`
	}
	mustUnmarshal(t, w.Body.Bytes(), &body)
	if body.App != "monbooru" || body.Status != "ok" || body.API != peer.APIVersion || body.MinPeerAPI != peer.MinPeerAPI {
		t.Errorf("/health body = %+v, want app monbooru / status ok and the api pair", body)
	}

	w = do(srv, "GET", "/")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("gated page = %d %q, want 303 /login", w.Code, w.Header().Get("Location"))
	}
}

func postFormAs(t *testing.T, srv *Server, sessID, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	csrf := srv.sessions.CSRFToken(sessID)
	form.Set("_csrf", csrf)
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func postLogin(t *testing.T, srv *Server, password, remoteAddr string, extra ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"_csrf": {srv.sessions.CSRFToken("anon")}, "password": {password}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.sessions.CSRFToken("anon"))
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestLoopbackBindRefusesAReboundHost(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Server.BaseURL = "http://booru.lan:8455" })
	for host, want := range map[string]int{
		"evil.example:8455": http.StatusMisdirectedRequest,
		"127.0.0.1:8455":    http.StatusOK,
		"localhost:8455":    http.StatusOK,
		"[::1]:8455":        http.StatusOK,
		"booru.lan:8455":    http.StatusOK,
	} {
		req := httptest.NewRequest("GET", "/health", nil)
		req.Host = host
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Host %s: status = %d, want %d", host, w.Code, want)
		}
	}
}

func TestNetworkBindRefusesOnlyAPublicName(t *testing.T) {
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Server.BindAddress = "0.0.0.0:8455" })
	for _, tc := range []struct {
		host, path string
		want       int
	}{
		{"evil.example:8455", "/settings", http.StatusMisdirectedRequest},
		{"evil.example:8455", "/health", http.StatusOK},
		{"evil.example:8455", "/api/v1/openapi.json", http.StatusOK},
		{"nas.local:8455", "/health", http.StatusOK},
		{"nas:8455", "/health", http.StatusOK},
		{"192.168.1.5:8455", "/health", http.StatusOK},
		{"box.tailnet.ts.net:8455", "/health", http.StatusOK},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Host = tc.host
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("Host %s %s: status = %d, want %d", tc.host, tc.path, w.Code, tc.want)
		}
	}
}

func TestACreatedTokenResetsItsFormThroughAnEvent(t *testing.T) {
	srv := newTestServer(t)
	w := postHTMXForm(t, srv, "/settings/auth/tokens", url.Values{"name": {"ci"}})
	if trig := w.Header().Get("HX-Trigger"); trig != "token-created" {
		t.Errorf("HX-Trigger = %q, want token-created", trig)
	}
	wantIn(t, w.Body.String(), "Token generated")
	wantNotIn(t, w.Body.String(), "<script")
}
