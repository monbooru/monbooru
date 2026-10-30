package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

// The connection a probe leaves a peer in; "" is a cold cache.
const (
	ConnOK       = webui.ConnOK
	ConnDown     = webui.ConnDown
	ConnRejected = webui.ConnRejected
	ConnPaused   = webui.ConnPaused
)

// A backstop for callers with an unbounded context: it must stay above every
// per-call deadline, or it cuts off a send the peer may still commit. A
// redirect is not followed, since it could point at any host.
var Client = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: noRedirect,
}

// SlowClient backs calls whose own deadline outlasts Client's backstop.
var SlowClient = &http.Client{
	Timeout:       5 * time.Minute,
	CheckRedirect: noRedirect,
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// A refused request, not a sign the peer is down, so a batch skips the
// row. It carries the peer's name: a plugin's refusal must not read as
// monloader's.
type StatusError struct{ Peer, Status, Code string }

func (e StatusError) Error() string {
	if e.Code != "" {
		return e.Peer + " returned " + e.Status + " (" + e.Code + ")"
	}
	return e.Peer + " returned " + e.Status
}

func IsStatusError(err error) bool {
	var se StatusError
	return errors.As(err, &se)
}

// APIVersion moves on any change to /api/v1 a paired peer would trip
// over: a path or field gone, a status code or a vocabulary word changed.
// MinPeerAPI is the oldest peer API this side still drives.
const (
	APIVersion = 3
	MinPeerAPI = 1
)

// A 200 with any body passes; every field is optional.
func Health(ctx context.Context, base string) (contract.Health, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return contract.Health{}, false
	}
	resp, err := Client.Do(req)
	if err != nil {
		return contract.Health{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return contract.Health{}, false
	}
	var h contract.Health
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h, true
}

func Reachable(ctx context.Context, base string) bool {
	if strings.TrimRight(base, "/") == "" {
		return false
	}
	_, up := Health(ctx, base)
	return up
}

func NotifyTeardown(app, baseURL, token string) error {
	base := strings.TrimRight(baseURL, "/")
	if base == "" || token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/pair/remove", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Any 2xx: the teardown is idempotent, so a 204 has done what was asked.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return StatusError{Peer: app, Status: resp.Status}
	}
	return nil
}
