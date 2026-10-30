// Package monloader is the client for the paired monloader instance.
package monloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// App is the peer's name in pairing records and error text.
const App = contract.AppMonloader

// errUnconfigured is returned before any I/O when no link is set up.
var errUnconfigured = errors.New("monloader is not configured")

// Client reads Base and Token on every call so a re-pair or a pause lands
// without rebuilding it. Base must return "" while the link is paused.
type Client struct {
	Base  func() string
	Token func() string
}

func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return c.do(ctx, peer.Client, method, path, body)
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(c.Base(), "/")
	token := c.Token()
	if base == "" || token == "" {
		return nil, errUnconfigured
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxResponseBytes)
	return resp, nil
}

const maxResponseBytes = 16 << 20

// A monloader whose first-run setup is not done answers every /api/v1 call
// 409 setup_required, which is no PTR or job refusal.
func setupRequired(resp *http.Response) bool {
	if resp.StatusCode != http.StatusConflict {
		return false
	}
	var e contract.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return e.Code == contract.CodeSetupRequired
}

func setupRefusal(resp *http.Response) error {
	return peer.StatusError{Peer: App, Status: resp.Status, Code: contract.CodeSetupRequired}
}

func refused(resp *http.Response) peer.StatusError {
	var e contract.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return peer.StatusError{Peer: App, Status: resp.Status, Code: e.Code}
}

func (c *Client) Post(ctx context.Context, path string, payload any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	return c.Do(ctx, http.MethodPost, path, body)
}

// PostSlow must be given a deadline: only peer.SlowClient's backstop
// bounds it otherwise.
func (c *Client) PostSlow(ctx context.Context, path string, payload any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	return c.do(ctx, peer.SlowClient, http.MethodPost, path, body)
}
