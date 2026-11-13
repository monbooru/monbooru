package monloader

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func stubLink(t *testing.T, h http.HandlerFunc) *Link {
	t.Helper()
	stub := httptest.NewServer(h)
	t.Cleanup(stub.Close)
	return NewLink(func() LinkConfig { return LinkConfig{APIURL: stub.URL, Token: "peer-secret", Paired: true} }, jobs.NewManager())
}

func TestEnqueueMetadataFetch(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
	})

	unconfigured := NewLink(func() LinkConfig { return LinkConfig{} }, nil)
	if err := unconfigured.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1"); err == nil {
		t.Error("unconfigured enqueue should fail")
	}

	if err := l.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if gotPath != "/api/v1/metadata" || gotAuth != "Bearer peer-secret" {
		t.Errorf("request = %s auth=%q, want /api/v1/metadata with the bearer", gotPath, gotAuth)
	}
	for _, want := range []string{`"image_id":7`, `"gallery":"default"`, `"url":"https://d/1"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body %q lacks %q", gotBody, want)
		}
	}

	refuse := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	err := refuse.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1")

	if !peer.IsStatusError(err) || !strings.Contains(err.Error(), App) {
		t.Errorf("400 refusal = %v, want a status error naming monloader", err)
	}
	gone := NewLink(func() LinkConfig { return LinkConfig{APIURL: "http://127.0.0.1:0", Token: "peer-secret"} }, nil)
	if err := gone.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1"); err == nil || peer.IsStatusError(err) {
		t.Errorf("transport failure = %v, want a non-status error", err)
	}
}

func TestEnqueueMapsMonloadersRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		call    func(l *Link) error
		want    error
		wantRef bool
	}{
		{"replace answered", http.StatusAccepted, func(l *Link) error {
			return l.EnqueueReplace(t.Context(), 7, "default", "https://d/1")
		}, nil, false},
		{"replace 409 is a refusal", http.StatusConflict, func(l *Link) error {
			return l.EnqueueReplace(t.Context(), 7, "default", "https://d/1")
		}, nil, true},
		{"replace 429 spends the budget", http.StatusTooManyRequests, func(l *Link) error {
			return l.EnqueueReplace(t.Context(), 7, "default", "https://d/1")
		}, errBudgetSpent, false},
		{"hash lookup 409 is the PTR", http.StatusConflict, func(l *Link) error {
			_, err := l.enqueueHashLookup(t.Context(), 7, "default", "ptr", "", "sha", true, false)
			return err
		}, ErrPTRUnavailable, false},
		{"hash lookup 429 spends the budget", http.StatusTooManyRequests, func(l *Link) error {
			_, err := l.enqueueHashLookup(t.Context(), 7, "default", "booru", "md5", "sha", true, true)
			return err
		}, errBudgetSpent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				w.WriteHeader(tc.status)
			})
			err := tc.call(l)
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("err = %v, want %v", err, tc.want)
			case tc.wantRef && !peer.IsStatusError(err):
				t.Errorf("err = %v, want a status error", err)
			case tc.want == nil && !tc.wantRef && err != nil:
				t.Errorf("err = %v, want none", err)
			}
			if !strings.HasPrefix(path, "/api/v1/") {
				t.Errorf("path = %q", path)
			}
		})
	}
}

func TestEnqueueHashLookupReturnsTheJob(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":42}`))
	})
	if id, err := l.enqueueHashLookup(t.Context(), 7, "default", "booru", "md5", "sha", false, false); err != nil || id != 42 {
		t.Errorf("id = %d err = %v, want job 42", id, err)
	}
}

func TestPTRBatchLookupMapsMonloadersAnswers(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusConflict, ErrPTRUnavailable},
		{http.StatusNotFound, errPTRBatchUnsupported},
	} {
		l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })
		if _, err := l.ptrBatchLookup(t.Context(), "default", false, []contract.PTRLookupImage{{ImageID: 1, SHA256: "a"}}); !errors.Is(err, tc.want) {
			t.Errorf("%d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, err := l.ptrBatchLookup(t.Context(), "default", false, nil); !peer.IsStatusError(err) {
		t.Errorf("500: err = %v, want a status error", err)
	}
}

func TestPTRBatchLookupReadsA405AsTooOld(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) })
	if _, err := l.ptrBatchLookup(t.Context(), "default", false, []contract.PTRLookupImage{{ImageID: 1, SHA256: "a"}}); !errors.Is(err, errPTRBatchUnsupported) {
		t.Errorf("err = %v, want %v", err, errPTRBatchUnsupported)
	}
}

func TestPTRSpellingSearchReadsA405AsNoSearch(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		if _, _, err := l.PTRSpellingSearch(t.Context(), "blue", "", 10); !errors.Is(err, ErrNoSearch) {
			t.Errorf("%d: err = %v, want %v", status, err, ErrNoSearch)
		}
	}
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, _, err := l.PTRSpellingSearch(t.Context(), "blue", "", 10); !peer.IsStatusError(err) {
		t.Errorf("500: err = %v, want a status error", err)
	}
}

func TestPTRSpellingSearchReadsOnlyNamespaceRequiredAsUnbounded(t *testing.T) {
	for code, unbounded := range map[string]bool{contract.CodeNamespaceRequired: true, contract.CodeInvalidRequest: false, "": false} {
		l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
			contract.WriteError(w, http.StatusBadRequest, code, "refused")
		})
		_, _, err := l.PTRSpellingSearch(t.Context(), "blue", contract.SearchContains, 10)
		if errors.Is(err, ErrSearchUnbounded) != unbounded || !unbounded && !peer.IsStatusError(err) {
			t.Errorf("400 %q: err = %v, want unbounded %v or else a status error", code, err, unbounded)
		}
	}
}

func TestPostJSONRefusalIsAStatusError(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	_, err := l.PTRTagLookup(t.Context(), []string{"blue"})
	if !peer.IsStatusError(err) || err.Error() != "monloader returned 500 Internal Server Error" {
		t.Errorf("err = %v, want a status error naming the status", err)
	}
}

func TestBatchLookupNamesAPTRRefusal(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	cx := testGallery(t)
	seedPhaseImage(t, cx, 1, strings.Repeat("a", 64))
	if err := l.jobs.Start("lookup"); err != nil {
		t.Fatal(err)
	}

	l.BatchLookup(t.Context(), cx, BatchLookupOpts{Mode: "hash", PTR: true}, []int64{1})
	if st := l.jobs.Get(); !strings.HasPrefix(st.Error, "PTR lookup failed: ") {
		t.Errorf("job error = %q, want a PTR lookup failure, not an unreachable monloader", st.Error)
	}
}

func TestProbeReadsTheThreeLinkStates(t *testing.T) {
	queue := http.StatusOK
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"version":"v2"}`))
		case "/api/v1/queue":
			w.WriteHeader(queue)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if st := l.Probe(t.Context()); st.Conn != peer.ConnOK || st.Version != "v2" {
		t.Errorf("answering link = %+v, want ok at v2", st)
	}
	queue = http.StatusUnauthorized
	if st := l.Probe(t.Context()); st.Conn != peer.ConnRejected {
		t.Errorf("revoked token = %+v, want rejected", st)
	}
	down := NewLink(func() LinkConfig { return LinkConfig{APIURL: "http://127.0.0.1:1", Token: "t", Paired: true} }, nil)
	if st := down.Probe(t.Context()); st.Conn != peer.ConnDown {
		t.Errorf("unreachable link = %+v, want down", st)
	}
	paused := NewLink(func() LinkConfig { return LinkConfig{APIURL: "http://127.0.0.1:1", Paused: true, Paired: true} }, nil)
	if st := paused.Probe(t.Context()); st.Conn != "" || paused.Usable() {
		t.Errorf("paused link = %+v usable %v, want unprobed and unusable", st, paused.Usable())
	}
}

func TestPreviewContribSendsAnEmptyTagListForNone(t *testing.T) {
	var body map[string]json.RawMessage
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"to_add":[],"ptr_only":[]}`))
	})

	if _, err := l.previewContrib(t.Context(), "abc", nil, nil); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got := string(body["tags"]); got != "[]" {
		t.Errorf(`"tags" = %s, want []`, got)
	}
}

func TestPanelsChangesWhenOnlyTheBanChanges(t *testing.T) {
	st := Status{PTR: true, Contrib: true}
	banned := st
	banned.ContribBanned = true
	if st.Panels() == banned.Panels() {
		t.Errorf("panels key %q ignores the ban", st.Panels())
	}
}

func TestTeardownNotifiesAPausedPeer(t *testing.T) {
	var gotPath, gotAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stub.Close)
	l := NewLink(func() LinkConfig {
		return LinkConfig{APIURL: stub.URL, Token: "peer-secret", Paired: true, Paused: true}
	}, nil)

	if err := l.Teardown(func() error { return nil }); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if gotPath != "/api/v1/pair/remove" || gotAuth != "Bearer peer-secret" {
		t.Errorf("notice = %q with %q, want the pair removal with the bearer", gotPath, gotAuth)
	}
	if l.Base() != "" {
		t.Errorf("base = %q, want none while paused", l.Base())
	}
}

func TestProbeReadsAMonloaderWithNoTokensAsRejected(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"version":"v2"}`))
		case "/api/v1/queue":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"api_disabled"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if st := l.Probe(t.Context()); st.Conn != peer.ConnRejected {
		t.Errorf("probe = %+v, want rejected", st)
	}
}

func TestAMonloaderAwaitingItsSetupReadsAsRejected(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"version":"v2"}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"monloader has not been set up yet","code":"setup_required"}`))
	})
	if st := l.Probe(t.Context()); st.Conn != peer.ConnRejected {
		t.Errorf("probe = %+v, want rejected", st)
	}
	_, err := l.enqueueHashLookup(t.Context(), 7, "default", "booru", "md5", "sha", false, false)
	_, batchErr := l.ptrBatchLookup(t.Context(), "default", false, nil)
	_, tagsErr := l.PTRTagLookup(t.Context(), []string{"a"})
	_, _, searchErr := l.PTRSpellingSearch(t.Context(), "a", "", 5)
	for call, err := range map[string]error{"lookup": err, "batch PTR lookup": batchErr, "PTR tags": tagsErr, "spelling search": searchErr} {
		if errors.Is(err, ErrPTRUnavailable) || !peer.IsStatusError(err) || !strings.Contains(err.Error(), "setup_required") {
			t.Errorf("%s err = %v, want the setup_required refusal, not the PTR", call, err)
		}
	}
}

func TestProbeKeepsTheAPIMismatchWhenTheTokenIsRejected(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"version":"v9","api":9,"min_peer_api":9}`))
		case "/api/v1/queue":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if st := l.Probe(t.Context()); st.Conn != peer.ConnRejected || st.Version != "v9" || st.APIMismatch == "" {
		t.Errorf("probe = %+v, want rejected at v9 with the api mismatch kept", st)
	}
}

func TestSendContribNamesTheRefusal(t *testing.T) {
	for code, want := range map[string]string{
		"ptr_account_required": "a contribution account in monloader is required to contribute",
		"ptr_banned":           "the contribution account is banned",
		"ptr_syncing":          "the Public Tag Repository is still syncing",
	} {
		l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"` + code + `","error":"refused"}`))
		})
		var refused ContribRefused
		if _, err := l.SendContrib(t.Context(), "image 1", nil); !errors.As(err, &refused) || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", code, err, want)
		}
	}
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusConflict) })
	if _, err := l.SendContrib(t.Context(), "image 1", nil); !errors.Is(err, ErrPTRUnavailable) {
		t.Errorf("a bare 409: err = %v, want %v", err, ErrPTRUnavailable)
	}
}

func TestBatchLookupACancelDuringARequestReportsCancelled(t *testing.T) {
	for _, opts := range []BatchLookupOpts{{Mode: "hash", Booru: true}, {Mode: "hash", PTR: true}} {
		var l *Link
		l = stubLink(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			l.jobs.Cancel()
			<-r.Context().Done()
		})
		cx := testGallery(t)
		seedPhaseImage(t, cx, 1, strings.Repeat("a", 64))
		mustExec(t, cx.DB.Write, `UPDATE images SET md5 = ? WHERE id = 1`, strings.Repeat("b", 32))
		if err := l.jobs.Start("lookup"); err != nil {
			t.Fatal(err)
		}

		l.BatchLookup(l.jobs.Context(), cx, opts, []int64{1})
		if st := l.jobs.Get(); st.Summary != "Lookup cancelled after queueing 0." || st.Error != "" {
			t.Errorf("%+v: summary = %q error = %q, want a cancel", opts, st.Summary, st.Error)
		}
	}
}

func TestTheClientRefusesARedirectAndKeepsTheRefusalsCode(t *testing.T) {
	followed := false
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	if _, err := l.PTRTagLookup(t.Context(), []string{"a"}); !peer.IsStatusError(err) || followed {
		t.Errorf("redirect = %v, followed %v, want a status error and no second request", err, followed)
	}

	coded := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(contract.Error{Error: "bad", Code: "invalid_request"})
	})
	var se peer.StatusError
	if _, err := coded.PTRTagLookup(t.Context(), []string{"a"}); !errors.As(err, &se) || se.Code != "invalid_request" {
		t.Errorf("400 = %v, want its code kept", err)
	}
}

func TestTheClientCapsAnAnswer(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":{"x":"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxResponseBytes))
		_, _ = w.Write([]byte(`"}}`))
	})
	if _, err := l.PTRTagLookup(t.Context(), []string{"a"}); err == nil {
		t.Error("an answer over the cap decoded")
	}
}

func TestTagRulesForAsksForEveryNameInOneCall(t *testing.T) {
	var asked tagRulesRequest
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/rules" {
			t.Errorf("%s %s, want POST /api/v1/rules", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&asked)
		_, _ = io.WriteString(w, `{"results":[{"category_kept":true,"sites":[{"site":"danbooru","rule":"drop"}]},{"category_kept":false,"sites":[]}]}`)
	})
	got, err := l.TagRulesFor(t.Context(), []TagName{{Tag: "miku", Category: "character"}, {Tag: "absurdres", Category: "meta"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(asked.Names) != 2 || asked.Names[1].Category != "meta" {
		t.Errorf("asked = %+v, want both names with their categories", asked)
	}
	if len(got) != 2 || got[0].Sites[0].Rule != "drop" || got[1].CategoryKept {
		t.Errorf("got = %+v, want the two answers in order", got)
	}
}

func TestTagRulesForNamesAMonloaderWithoutTheCall(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotFound} {
		l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("%s %s, want only the POST", r.Method, r.URL)
			}
			w.WriteHeader(status)
		})
		if _, err := l.TagRulesFor(t.Context(), []TagName{{Tag: "miku", Category: "character"}}); !errors.Is(err, ErrNoRules) {
			t.Errorf("%d: err = %v, want ErrNoRules", status, err)
		}
	}
}
