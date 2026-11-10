package monloader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	peerpkg "github.com/monbooru/monbooru/internal/peer"
)

func TestDoRefusesAnUnconfiguredLinkBeforeAnyIO(t *testing.T) {
	reached := false
	peer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer peer.Close()

	for _, tc := range []struct {
		name  string
		base  string
		token string
	}{
		{"paused link", "", "tok"},
		{"no token", peer.URL, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{Base: func() string { return tc.base }, Token: func() string { return tc.token }}
			if _, err := c.Do(context.Background(), http.MethodGet, "/api/status", nil); !errors.Is(err, errUnconfigured) {
				t.Errorf("err = %v, want errUnconfigured", err)
			}
		})
	}
	if reached {
		t.Error("an unconfigured call reached the peer")
	}
}

func TestDoSendsTheBearerAndTrimsTheBase(t *testing.T) {
	var gotPath, gotAuth, gotType string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
	}))
	defer peer.Close()

	c := &Client{Base: func() string { return peer.URL + "/" }, Token: func() string { return "sekrit" }}
	resp, err := c.Do(context.Background(), http.MethodGet, "/api/status", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if gotPath != "/api/status" {
		t.Errorf("path = %q, want /api/status (the trailing slash on the base is dropped)", gotPath)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("authorization = %q, want the bearer token", gotAuth)
	}
	if gotType != "" {
		t.Errorf("content type = %q, want none on a bodyless call", gotType)
	}
}

func TestPostSendsJSON(t *testing.T) {
	var gotType string
	var body map[string]any
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}))
	defer peer.Close()

	c := &Client{Base: func() string { return peer.URL }, Token: func() string { return "tok" }}
	resp, err := c.Post(context.Background(), "/api/lookup", map[string]any{"md5": "abc"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	_ = resp.Body.Close()
	if gotType != "application/json" {
		t.Errorf("content type = %q, want application/json", gotType)
	}
	if body["md5"] != "abc" {
		t.Errorf("body = %v, want the payload", body)
	}
}

func TestDoRereadsTheTokenEveryCall(t *testing.T) {
	var seen []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer peer.Close()

	token := "first"
	c := &Client{Base: func() string { return peer.URL }, Token: func() string { return token }}
	for _, want := range []string{"first", "second"} {
		token = want
		resp, err := c.Do(context.Background(), http.MethodGet, "/api/status", nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Errorf("authorization headers = %v, want the token re-read each call", seen)
	}
}

func TestDoHonoursACancelledContext(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer peer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{Base: func() string { return peer.URL }, Token: func() string { return "tok" }}
	if _, err := c.Do(ctx, http.MethodGet, "/api/status", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestPTRBatchDeadlineStaysUnderItsClientBackstop(t *testing.T) {
	if peerpkg.SlowClient.Timeout <= ptrBatchTimeout {
		t.Errorf("SlowClient backstop %s would cut a %s batch short", peerpkg.SlowClient.Timeout, ptrBatchTimeout)
	}
	if peerpkg.Client.Timeout >= ptrBatchTimeout {
		t.Errorf("a batch deadline under Client's %s backstop needs no slow client", peerpkg.Client.Timeout)
	}
}
