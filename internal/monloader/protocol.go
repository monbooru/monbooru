package monloader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// monloaderRoutes is every monloader route this package calls, as
// monloader's OpenAPI spells it; the contract tests hold each call to it
// and it to monloader's document.
var monloaderRoutes = []struct{ Method, Path string }{
	{http.MethodGet, "/api/v1/queue"},
	{http.MethodGet, "/api/v1/queue/{id}"},
	{http.MethodGet, "/api/v1/ptr/status"},
	{http.MethodPost, "/api/v1/metadata"},
	{http.MethodPost, "/api/v1/replace"},
	{http.MethodPost, "/api/v1/lookup"},
	{http.MethodPost, "/api/v1/galleries/rename"},
	{http.MethodPost, "/api/v1/ptr/lookup"},
	{http.MethodPost, "/api/v1/ptr/tags"},
	{http.MethodGet, "/api/v1/ptr/tags/search"},
	{http.MethodPost, "/api/v1/ptr/contrib"},
	{http.MethodPost, "/api/v1/ptr/contrib/preview"},
	{http.MethodPost, "/api/v1/ptr/contrib/pair-preview"},
	{http.MethodPost, "/api/v1/pair/remove"},
	{http.MethodGet, "/health"},
}

// A 409 on a PTR call: the cached capability was stale; the link itself
// is fine.
var ErrPTRUnavailable = errors.New("the PTR lookup is unavailable on monloader")

type conflict struct{ code string }

func (c conflict) Error() string { return ErrPTRUnavailable.Error() }
func (c conflict) Unwrap() error { return ErrPTRUnavailable }

type ContribRefused string

func (e ContribRefused) Error() string { return string(e) }

// It ends the phase: every later row would get the same answer.
var errBudgetSpent = errors.New("monloader's daily lookup budget is spent")

// No per-image fallback: one queued job per image is what the batch
// endpoint exists to stop.
var errPTRBatchUnsupported = errors.New("monloader is too old for batch PTR lookup")

// A 404 from a monloader too old to search; the dialog falls back to its
// plain input.
var ErrNoSearch = errors.New("monloader does not serve the spelling search")

var ErrSearchUnbounded = errors.New("a substring search needs a category")

// monbooru only enqueues, keeping its single-egress model: every fetch
// runs on monloader.
func (l *Link) enqueue(ctx context.Context, path string, payload any, onConflict error) (int64, error) {
	resp, err := l.client.Post(ctx, path, payload)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if onConflict != nil && resp.StatusCode == http.StatusConflict {
		if setupRequired(resp) {
			return 0, setupRefusal(resp)
		}
		return 0, onConflict
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, errBudgetSpent
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return 0, refused(resp)
	}
	var out contract.JobAccepted
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.JobID, nil
}

func (l *Link) EnqueueMetadataFetch(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := l.enqueue(ctx, "/api/v1/metadata",
		contract.ImageJobRequest{ImageID: imageID, Gallery: gallery, URL: url}, nil)
	return err
}

func (l *Link) EnqueueReplace(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := l.enqueue(ctx, "/api/v1/replace",
		contract.ImageJobRequest{ImageID: imageID, Gallery: gallery, URL: url}, nil)
	return err
}

func (l *Link) enqueueHashLookup(ctx context.Context, imageID int64, gallery, backend, md5, sha256 string, background, budgeted bool) (int64, error) {
	return l.enqueue(ctx, "/api/v1/lookup", contract.LookupRequest{
		ImageID: imageID, Gallery: gallery, Backend: backend, MD5: md5, SHA256: sha256,
		Background: background, Budgeted: budgeted,
	}, ErrPTRUnavailable)
}

func (l *Link) GalleryRenamed(ctx context.Context, oldName, newName string) error {
	if !l.cfg().Paired {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := l.client.Post(ctx, "/api/v1/galleries/rename", contract.GalleryRename{From: oldName, To: newName})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return refused(resp)
	}
	return nil
}

// Teardown runs remove, which drops the pairing and its token, then tells
// monloader with the address and token read before it.
func (l *Link) Teardown(remove func()) error {
	base, token := l.addr(), l.cfg().Token
	remove()
	err := peer.NotifyTeardown(App, base, token)
	if err != nil {
		logx.Errorf("pairing: could not notify monloader of teardown: %v", err)
	}
	return err
}

// A hundred hashes read from a cold full-size index took nearly 30 s.
const ptrBatchTimeout = 2 * time.Minute

func (l *Link) ptrBatchLookup(ctx context.Context, gallery string, scheduled bool, images []contract.PTRLookupImage) (map[string][]string, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, ptrBatchTimeout)
	defer cancel()
	resp, err := l.client.PostSlow(ctx, "/api/v1/ptr/lookup", contract.PTRLookupRequest{
		Images: images, Gallery: gallery, Scheduled: scheduled,
	})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		if setupRequired(resp) {
			return nil, 0, setupRefusal(resp)
		}
		return nil, 0, ErrPTRUnavailable
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, 0, errPTRBatchUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, refused(resp)
	}
	var out contract.PTRLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, err
	}
	return out.Results, out.Index, nil
}

func (l *Link) ptrIndexCursor(ctx context.Context) (uint64, error) {
	resp, err := l.client.Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, refused(resp)
	}
	var out ptrStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Progress.UpdateIndex, nil
}

type ptrStatus struct {
	Enabled  bool `json:"enabled"`
	Progress struct {
		UpdateIndex uint64 `json:"update_index"`
	} `json:"progress"`
	State     string `json:"state"`
	Queryable bool   `json:"queryable"`
	Contrib   *struct {
		Account bool `json:"account"`
		Banned  bool `json:"banned"`
		Failed  int  `json:"failed"`
	} `json:"contrib"`
}

type queueJob struct {
	Status  string `json:"status"`
	Backend string `json:"backend"`
	Items   []struct {
		Outcome   string `json:"outcome"`
		ErrorCode string `json:"error_code"`
	} `json:"items"`
}

// false leaves the row in flight. An empty result is inconclusive: moving
// the ladder on it would walk an image to "nothing found" while monloader
// is down.
func (l *Link) jobOutcome(ctx context.Context, jobID int64) (string, bool) {
	if jobID == 0 {
		return "", true // a monloader that reports no job id
	}
	resp, err := l.client.Do(ctx, http.MethodGet, "/api/v1/queue/"+strconv.FormatInt(jobID, 10), nil)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Aged out of the finished ring, or never existed.
		return "", true
	}
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var job queueJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return "", false
	}
	switch job.Status {
	case contract.JobQueued, contract.JobRunning:
		return "", false
	case contract.JobCanceled, contract.JobInterrupted:
		return "", true
	}
	for _, it := range job.Items {
		if it.Outcome == contract.OutcomeEnriched {
			// A job over both backends cannot say which one matched; that
			// one's enrich already concluded its own row.
			if len(lookup.Backends(job.Backend)) > 1 {
				return "", true
			}
			return lookup.ResultHit, true
		}
		if result, _ := Outcome(it.ErrorCode); result == lookup.ResultMiss {
			return result, true
		}
	}
	return lookup.ResultError, true
}

func postJSON[T any](l *Link, ctx context.Context, path string, payload any) (*T, error) {
	resp, err := l.client.Post(ctx, path, payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		var refusal contract.Error
		_ = json.NewDecoder(resp.Body).Decode(&refusal)
		if refusal.Code == contract.CodeSetupRequired {
			return nil, setupRefusal(resp)
		}
		return nil, conflict{refusal.Code}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, refused(resp)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// The caller keeps a call's names under monloader's request limit.
func (l *Link) PTRTagLookup(ctx context.Context, names []string) (map[string]contract.PTRTagInfo, error) {
	out, err := postJSON[contract.PTRTagsResponse](l, ctx, "/api/v1/ptr/tags", contract.PTRTagsRequest{Tags: names})
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

func (l *Link) PTRSpellingSearch(ctx context.Context, q, mode string, limit int) (clusters []contract.PTRCluster, truncated bool, err error) {
	v := url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}}
	if mode != "" {
		v.Set("mode", mode)
	}
	resp, err := l.client.Do(ctx, http.MethodGet, "/api/v1/ptr/tags/search?"+v.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, false, ErrNoSearch
	case http.StatusBadRequest:
		if e := refused(resp); e.Code != contract.CodeNamespaceRequired {
			return nil, false, e
		}
		return nil, false, ErrSearchUnbounded
	case http.StatusConflict:
		if setupRequired(resp) {
			return nil, false, setupRefusal(resp)
		}
		return nil, false, ErrPTRUnavailable
	default:
		return nil, false, refused(resp)
	}
	var out contract.PTRSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return out.Clusters, out.Truncated, nil
}

type ContribPreviewToAdd struct {
	contract.ContribToAdd
	Color string `json:"-"`
	// Sent is the spelling the row goes up under; Tag is the one the
	// operator sees.
	Sent string `json:"-"`
}

func (t ContribPreviewToAdd) PTRDiffers() bool { return strings.ReplaceAll(t.Tag, "_", " ") != t.PTR }

type ContribPreviewPTROnly struct {
	contract.ContribPTROnly
	Color string `json:"-"`
}

type ContribPreview struct {
	Provisional bool                    `json:"provisional"`
	ToAdd       []ContribPreviewToAdd   `json:"to_add"`
	PTROnly     []ContribPreviewPTROnly `json:"ptr_only"`
}

func (l *Link) previewContrib(ctx context.Context, sha256 string, tags, implied []string) (*ContribPreview, error) {
	// monloader refuses a null list; an image with nothing to send sends [].
	if tags == nil {
		tags = []string{}
	}
	return postJSON[ContribPreview](l, ctx, "/api/v1/ptr/contrib/preview",
		contract.ContribPreviewRequest{SHA256: sha256, Tags: tags, Implied: implied})
}

func (l *Link) SendContrib(ctx context.Context, origin string, items []contract.ContribItem) (*contract.ContribResponse, error) {
	commit := true
	out, err := postJSON[contract.ContribResponse](l, ctx, "/api/v1/ptr/contrib",
		contract.ContribRequest{Commit: &commit, Origin: origin, Items: items})
	var c conflict
	if errors.As(err, &c) {
		if reason, ok := contribRefusals[c.code]; ok {
			return nil, ContribRefused(reason)
		}
	}
	return out, err
}

func (l *Link) previewPair(ctx context.Context, kind, a, b string) (*contract.PairPreviewResponse, error) {
	return postJSON[contract.PairPreviewResponse](l, ctx, "/api/v1/ptr/contrib/pair-preview",
		contract.PairPreviewRequest{Kind: kind, A: a, B: b})
}
