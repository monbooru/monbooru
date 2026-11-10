package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/pairing"
)

func TestPluginPairDeny_MintsNothingAndDropsThePending(t *testing.T) {
	srv := newTestServer(t)
	code, out := pairOffer(t, srv, `{"app":"monedit","url":"http://edit:7301","requested_scopes":["read"],
		"peer_token":"edit-secret","version":"0.3.1"}`)
	if code != http.StatusOK {
		t.Fatalf("request: code=%d out=%v", code, out)
	}
	id := out["request_id"]

	w := postForm(t, srv, "/settings/plugins/pair/"+id+"/deny", url.Values{})
	mustCode(t, w, http.StatusOK)

	req, ok := srv.pairs.Get(id)
	if !ok {
		t.Fatal("the request vanished instead of being marked denied")
	}
	if req.State != pairing.Denied {
		t.Errorf("state = %q, want %q", req.State, pairing.Denied)
	}
	if srv.pairedWith("monedit") {
		t.Error("a denied pairing minted a token")
	}
	if p := srv.cfgSnapshot().FindPlugin("monedit"); p != nil {
		t.Errorf("a denied pairing wrote a [[plugin]] block: %+v", *p)
	}

	do(srv, "GET", "/api/v1/pair/status?id="+id)
	if srv.pairedWith("monedit") {
		t.Error("polling status after a deny completed the pairing")
	}
}

func TestTeardownPluginPairing_DropsBothHalves(t *testing.T) {
	srv := newTestServer(t)
	var notified string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/pair/remove" {
			notified = r.Header.Get("Authorization")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(stub.Close)
	code, out := pairOffer(t, srv, `{"app":"monedit","url":"`+stub.URL+`","requested_scopes":["read"],
		"peer_token":"edit-secret","version":"0.3.1"}`)
	if code != http.StatusOK {
		t.Fatalf("request: code=%d out=%v", code, out)
	}
	if !srv.pairs.SetState(out["request_id"], pairing.Approved) {
		t.Fatal("approve failed")
	}
	do(srv, "GET", "/api/v1/pair/status?id="+out["request_id"])
	if !srv.pairedWith("monedit") {
		t.Fatal("test premise broken: the claim did not pair")
	}

	if err := srv.withConfig(func(c *config.Config) error {
		c.FindPlugin("monedit").APIURL = stub.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := srv.teardownPluginPairing("monedit"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if notified != "Bearer edit-secret" {
		t.Errorf("peer notify carried %q, want the peer token so it can drop its own copy", notified)
	}
	if srv.pairedWith("monedit") {
		t.Error("the local token survived the teardown")
	}
	if p := srv.cfgSnapshot().FindPlugin("monedit"); p != nil && p.PeerToken != "" {
		t.Errorf("the peer token survived the teardown: %+v", *p)
	}
}
