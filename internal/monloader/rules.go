package monloader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

// ErrNoRules is a monloader that serves no rules call: one released before
// v1.14.0.
var ErrNoRules = errors.New("monloader serves no tag rules")

// TagRulesFor answers in the order of names.
func (l *Link) TagRulesFor(ctx context.Context, names []contract.TagName) ([]contract.TagRules, error) {
	resp, err := l.client.Post(ctx, "/api/v1/rules", contract.TagRulesRequest{Names: names})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRules
	}
	if resp.StatusCode != http.StatusOK {
		return nil, refused(resp)
	}
	var out contract.TagRulesBatch
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Results, err
}
