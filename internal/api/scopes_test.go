package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
)

func scopedRequest(t *testing.T, scopes []string, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := newBareEnv(t, nil, func(cfg *config.Config, g *library.Gallery) {
		cfg.Auth.Tokens = []apitoken.Token{{
			ID: "scoped", Name: "scoped", TokenHash: apitoken.Hash("scoped-token"), Scopes: scopes,
		}}
	})
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer scoped-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestBearerAuthEnforcesEachScopeArm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
		method string
		target string
		body   string
		want   int
	}{
		{"read token reads", []string{config.ScopeRead}, "GET", "/api/v1/tags", "", http.StatusOK},
		{"read token cannot write", []string{config.ScopeRead}, "POST", "/api/v1/tags", `{"name":"nope"}`, http.StatusForbidden},
		{"read token cannot patch", []string{config.ScopeRead}, "PATCH", "/api/v1/tags/1", `{"name":"nope"}`, http.StatusForbidden},
		{"read token cannot delete", []string{config.ScopeRead}, "DELETE", "/api/v1/tags/1", "", http.StatusForbidden},
		{"write token writes", []string{config.ScopeRead, config.ScopeWrite}, "POST", "/api/v1/tags", `{"name":"yes"}`, http.StatusCreated},
		{"write token cannot delete", []string{config.ScopeRead, config.ScopeWrite}, "DELETE", "/api/v1/tags/1", "", http.StatusForbidden},
	} {
		w := scopedRequest(t, tc.scopes, tc.method, tc.target, tc.body)
		wantCode(t, w, tc.want)
		if tc.want == http.StatusForbidden && !strings.Contains(w.Body.String(), "insufficient_scope") {
			t.Errorf("%s: body = %s, want insufficient_scope", tc.name, w.Body.String())
		}
	}
}
