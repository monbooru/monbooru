package monloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func seedLookupImage(t *testing.T, cx *library.Gallery, id int64) {
	t.Helper()
	mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, '/g/a.png', 'png', 1)`, id, strings.Repeat("a", 63)+string(rune('0'+id)))
}

func TestReconcileLookupsResolvesEachJobState(t *testing.T) {
	for _, tc := range []struct {
		name         string
		job          string
		wantResult   string
		wantAttempts int
		wantInFlight bool
	}{
		{"succeeded and enriched", `{"status":"succeeded","items":[{"outcome":"enriched"}]}`, lookup.ResultHit, 0, false},
		{"failed with no match", `{"status":"failed","items":[{"outcome":"failed","error_code":"hash_not_found"}]}`, lookup.ResultMiss, 1, false},
		{"failed", `{"status":"failed","items":[{"error_code":"blocked"}]}`, "", 0, false},
		{"canceled", `{"status":"canceled","items":[]}`, "", 0, false},
		{"still running", `{"status":"running","items":[]}`, "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/v1/queue/") {
					_, _ = w.Write([]byte(tc.job))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			})
			cx := testGallery(t)
			seedLookupImage(t, cx, 1)
			stale := time.Now().Add(-lookupGrace - time.Hour)
			if err := lookup.Enqueued(cx.DB, 1, []string{contract.LookupBooru}, 42, stale); err != nil {
				t.Fatal(err)
			}

			l.reconcileLookups(context.Background(), cx)

			rows, err := lookup.ForImage(cx.DB, 1)
			if err != nil {
				t.Fatal(err)
			}
			row := rows[contract.LookupBooru]
			if row.LastResult != tc.wantResult {
				t.Errorf("last_result = %q, want %q", row.LastResult, tc.wantResult)
			}
			if row.Attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", row.Attempts, tc.wantAttempts)
			}
			if inFlight := !row.QueuedAt.IsZero(); inFlight != tc.wantInFlight {
				t.Errorf("in flight = %v, want %v", inFlight, tc.wantInFlight)
			}
		})
	}
}

func TestReconcileLookupsLeavesAHitOverBothBackendsInconclusive(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/queue/42" {
			_, _ = w.Write([]byte(`{"status":"succeeded","backend":"all","items":[{"outcome":"enriched"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	cx := testGallery(t)
	seedLookupImage(t, cx, 1)
	stale := time.Now().Add(-lookupGrace - time.Hour)
	if err := lookup.Enqueued(cx.DB, 1, lookup.Backends("all"), 42, stale); err != nil {
		t.Fatal(err)
	}
	if err := lookup.RecordInFlight(cx.DB, 1, contract.LookupBooru, lookup.ResultHit, stale); err != nil {
		t.Fatal(err)
	}

	resolved, inconclusive := l.reconcileLookups(context.Background(), cx)
	if resolved != 1 || inconclusive != 1 {
		t.Fatalf("resolved = %d inconclusive = %d, want 1 and 1", resolved, inconclusive)
	}
	rows, _ := lookup.ForImage(cx.DB, 1)
	if row := rows[contract.LookupPTR]; row.LastResult != "" || !row.QueuedAt.IsZero() {
		t.Errorf("ptr row = %+v, want it out of flight with no result: the online walk made the hit", row)
	}
	if row := rows[contract.LookupBooru]; row.LastResult != lookup.ResultHit {
		t.Errorf("booru row = %+v, want its hit kept", row)
	}
}

func TestReconcileLookupsTreatsAMissingJobAsInconclusive(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	cx := testGallery(t)
	seedLookupImage(t, cx, 1)
	now := time.Now()
	if err := lookup.Record(cx.DB, 1, contract.LookupBooru, lookup.ResultMiss, 0, now); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Enqueued(cx.DB, 1, []string{contract.LookupBooru}, 42, now.Add(-lookupGrace-time.Hour)); err != nil {
		t.Fatal(err)
	}

	resolved, inconclusive := l.reconcileLookups(context.Background(), cx)
	if resolved != 1 || inconclusive != 1 {
		t.Fatalf("resolved = %d inconclusive = %d, want 1 and 1", resolved, inconclusive)
	}
	rows, _ := lookup.ForImage(cx.DB, 1)
	row := rows[contract.LookupBooru]
	if row.Attempts != 1 || row.LastResult != lookup.ResultMiss {
		t.Errorf("row = %+v, want the one recorded miss untouched", row)
	}
	if !row.QueuedAt.IsZero() || row.NextDueAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("row = %+v, want it out of flight and due now", row)
	}
}

func TestReconcileLookupsRespectsTheGrace(t *testing.T) {
	asked := false
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		asked = true
		w.WriteHeader(http.StatusNotFound)
	})
	cx := testGallery(t)
	seedLookupImage(t, cx, 1)
	if err := lookup.Enqueued(cx.DB, 1, []string{contract.LookupBooru}, 42, time.Now()); err != nil {
		t.Fatal(err)
	}
	if resolved, _ := l.reconcileLookups(context.Background(), cx); resolved != 0 || asked {
		t.Errorf("resolved = %d asked = %v, want a fresh attempt left alone", resolved, asked)
	}
}

func TestReconcileAllLookupsResolvesStaleRowsWhileTheLinkIsUp(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/queue/42" {
			_, _ = w.Write([]byte(`{"status":"succeeded","items":[{"outcome":"enriched"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(stub.Close)
	var paused atomic.Bool
	l := NewLink(func() LinkConfig {
		return LinkConfig{APIURL: stub.URL, Token: "peer-secret", Paired: true, Paused: paused.Load()}
	}, nil)
	cx := testGallery(t)
	seedLookupImage(t, cx, 1)
	if err := lookup.Enqueued(cx.DB, 1, []string{contract.LookupBooru}, 42, time.Now().Add(-lookupGrace-time.Hour)); err != nil {
		t.Fatal(err)
	}

	paused.Store(true)
	l.ReconcileAll([]*library.Gallery{cx})
	rows, _ := lookup.ForImage(cx.DB, 1)
	if row := rows[contract.LookupBooru]; row.QueuedAt.IsZero() {
		t.Fatalf("row = %+v, want it left in flight while the link is paused", row)
	}

	paused.Store(false)
	l.ReconcileAll([]*library.Gallery{cx})
	rows, _ = lookup.ForImage(cx.DB, 1)
	if row := rows[contract.LookupBooru]; !row.QueuedAt.IsZero() || row.LastResult != lookup.ResultHit {
		t.Errorf("row = %+v, want the enriched job recorded as a hit", row)
	}
}
