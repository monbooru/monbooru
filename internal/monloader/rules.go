package monloader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type TagRule struct {
	Site          string   `json:"site"`
	Kind          string   `json:"kind"`
	Builtin       bool     `json:"builtin"`
	Rule          string   `json:"rule"`
	Target        string   `json:"target"`
	Blacklist     string   `json:"blacklist"`
	BlacklistLine string   `json:"blacklist_line"`
	MappedFrom    []string `json:"mapped_from"`
	RenamedFrom   []string `json:"renamed_from"`
	Kept          bool     `json:"kept"`
}

type TagRules struct {
	CategoryKept bool      `json:"category_kept"`
	Sites        []TagRule `json:"sites"`
}

type TagName struct {
	Tag      string `json:"tag"`
	Category string `json:"category"`
}

type tagRulesRequest struct {
	Names []TagName `json:"names"`
}

type tagRulesBatch struct {
	Results []TagRules `json:"results"`
}

// ErrNoRules is a monloader that serves no rules call: one released before
// v1.14.0.
var ErrNoRules = errors.New("monloader serves no tag rules")

// TagRulesFor answers in the order of names.
func (l *Link) TagRulesFor(ctx context.Context, names []TagName) ([]TagRules, error) {
	resp, err := l.client.Post(ctx, "/api/v1/rules", tagRulesRequest{Names: names})
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
	var out tagRulesBatch
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Results, err
}
