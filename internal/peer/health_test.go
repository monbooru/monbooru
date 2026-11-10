package peer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestPeerTeardownErrorNamesThePeer(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer peer.Close()

	err := NotifyTeardown("simple-edit", peer.URL, "peer-secret")
	if err == nil || !strings.Contains(err.Error(), "simple-edit") {
		t.Errorf("teardown error = %v, want it to name simple-edit", err)
	}
	if err != nil && strings.Contains(err.Error(), contract.AppMonloader) {
		t.Errorf("teardown error = %v, want no mention of the companion", err)
	}

	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer quiet.Close()
	if err := NotifyTeardown("simple-edit", quiet.URL, "peer-secret"); err != nil {
		t.Errorf("teardown answered 204 reported %v, want no error", err)
	}
}

func TestHealthRefusesARedirect(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, up := Health(t.Context(), srv.URL); up || followed {
		t.Errorf("redirected health = up %v, followed %v; want down and no second request", up, followed)
	}
}

func TestHealthAPIMismatchAgainstThisSide(t *testing.T) {
	for _, c := range []struct {
		h        contract.Health
		mismatch bool
	}{
		{contract.Health{}, false},
		{contract.Health{API: APIVersion, MinPeerAPI: MinPeerAPI}, false},
		{contract.Health{API: APIVersion + 1, MinPeerAPI: APIVersion + 1}, true},
		{contract.Health{API: APIVersion + 1, MinPeerAPI: APIVersion}, false},
	} {
		if got := c.h.APIMismatch(contract.AppMonbooru, APIVersion, MinPeerAPI) != ""; got != c.mismatch {
			t.Errorf("%+v: mismatch %v, want %v", c.h, got, c.mismatch)
		}
	}
}
