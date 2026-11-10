package monloader

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func testGallery(t *testing.T) *library.Gallery {
	t.Helper()
	dir := t.TempDir()
	cx, err := library.Open(config.Gallery{Name: "default", GalleryPath: dir, ThumbnailsPath: filepath.Join(dir, "thumbs"), DBPath: filepath.Join(dir, "g.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cx.Close)
	return cx
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := ex.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func countRows(t *testing.T, database *db.DB, from string) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM ` + from).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type ptrStubHandler struct {
	index     uint64
	hits      map[string][]string
	withheld  map[string][]string
	heldCats  []string
	batches   [][]string
	ids       []int64
	scheduled []bool
}

func (p *ptrStubHandler) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/ptr/status":
		_, _ = w.Write([]byte(`{"enabled":true,"state":"ready","progress":{"update_index":` +
			strconv.FormatUint(p.index, 10) + `}}`))
	case "/api/v1/ptr/lookup":
		var req contract.PTRLookupRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		batch := make([]string, len(req.Images))
		results := map[string][]string{}
		for i, img := range req.Images {
			batch[i] = img.SHA256
			p.ids = append(p.ids, img.ImageID)
			if tags, ok := p.hits[img.SHA256]; ok {
				results[img.SHA256] = tags
			}
		}
		p.batches = append(p.batches, batch)
		p.scheduled = append(p.scheduled, req.Scheduled)
		out, _ := json.Marshal(contract.PTRLookupResponse{Index: p.index, Results: results, WithheldTags: p.withheld, WithheldCategories: p.heldCats})
		_, _ = w.Write(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func seedPhaseImage(t *testing.T, cx *library.Gallery, id int64, sha string) {
	t.Helper()
	mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 1)`, id, sha, "/g/img"+sha+".png")
}

func TestPTRLookupPhaseAppliesAndRecords(t *testing.T) {
	hitSHA := strings.Repeat("a", 64)
	missSHA := strings.Repeat("b", 64)
	stub := &ptrStubHandler{index: 4812, hits: map[string][]string{hitSHA: {"blue_eyes", "artist:someone"}}}

	l := stubLink(t, stub.handle)
	cx := testGallery(t)
	seedPhaseImage(t, cx, 1, hitSHA)
	seedPhaseImage(t, cx, 2, missSHA)

	checked, matched, err := l.runPTRLookupPhase(context.Background(), cx)
	if err != nil {
		t.Fatalf("phase: %v", err)
	}
	if checked != 2 || matched != 1 {
		t.Fatalf("checked = %d matched = %d, want 2 and 1", checked, matched)
	}
	if !slices.Equal(stub.ids, []int64{1, 2}) || !slices.Equal(stub.scheduled, []bool{true}) {
		t.Errorf("sent ids = %v scheduled = %v, want both images on the scheduled lane", stub.ids, stub.scheduled)
	}

	tagCount := countRows(t, cx.DB, "image_tags WHERE image_id = 1")
	if tagCount != 2 {
		t.Errorf("image 1 has %d tags, want the 2 the repository answered", tagCount)
	}
	var site, url string
	if err := cx.DB.Read.QueryRow(`SELECT site, url FROM image_sources WHERE image_id = 1`).Scan(&site, &url); err != nil {
		t.Fatalf("no ptr origin written: %v", err)
	}
	if site != "ptr" || url != "" {
		t.Errorf("origin = %q %q, want a url-less ptr row", site, url)
	}

	rows, _ := lookup.ForImage(cx.DB, 1)
	if got := rows[contract.LookupPTR]; got.LastResult != lookup.ResultHit {
		t.Errorf("image 1 row = %+v, want a hit", got)
	}
	rows, _ = lookup.ForImage(cx.DB, 2)
	miss := rows[contract.LookupPTR]
	if miss.LastResult != lookup.ResultMiss || miss.Attempts != 1 || miss.NextDueAt.IsZero() {
		t.Errorf("image 2 row = %+v, want one miss with a next try scheduled", miss)
	}

	checked, _, err = l.runPTRLookupPhase(context.Background(), cx)
	if err != nil || checked != 0 {
		t.Errorf("second run checked = %d err = %v, want nothing due", checked, err)
	}
}

func TestBatchPTRLookupLeavesWhatTheAnswerHeldBackUnflagged(t *testing.T) {
	sha := strings.Repeat("a", 64)
	stub := &ptrStubHandler{index: 1, hits: map[string][]string{sha: {"blue_eyes", "species:cat", "artist:someone"}}}
	l := stubLink(t, stub.handle)
	cx := testGallery(t)
	seedPhaseImage(t, cx, 1, sha)
	if _, err := l.batchPTRLookup(t.Context(), cx, []int64{1}); err != nil {
		t.Fatal(err)
	}

	stub.hits[sha] = []string{"blue_eyes"}
	stub.withheld = map[string][]string{sha: {"species:cat"}}
	stub.heldCats = []string{"artist"}
	if _, err := l.batchPTRLookup(t.Context(), cx, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, cx.DB, "image_tags WHERE image_id = 1 AND stale = 1"); n != 0 {
		t.Errorf("%d tags flagged stale, want none: the answer held them back", n)
	}

	stub.withheld, stub.heldCats = nil, nil
	if _, err := l.batchPTRLookup(t.Context(), cx, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, cx.DB, "image_tags WHERE image_id = 1 AND stale = 1"); n != 2 {
		t.Errorf("%d tags flagged stale, want the 2 an answer stopped listing", n)
	}
}

func TestPTRLookupPhaseReportsTheDueCountAsTotal(t *testing.T) {
	stub := &ptrStubHandler{index: 1}
	l := stubLink(t, stub.handle)
	cx := testGallery(t)
	for id := int64(1); id <= 150; id++ {
		seedPhaseImage(t, cx, id, fmt.Sprintf("%064d", id))
	}
	if err := l.jobs.Start("ptr-lookup"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := l.runPTRLookupPhase(context.Background(), cx); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if st := l.jobs.Get(); st.Processed != 150 || st.Total != 150 {
		t.Errorf("progress = %d/%d, want 150/150", st.Processed, st.Total)
	}
}

func TestOnlineLookupPhaseReportsTheDueCountAsTotal(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":7}`))
	})
	cx := testGallery(t)
	dir := t.TempDir()
	for id := int64(1); id <= 3; id++ {
		path := dir + "/" + strconv.FormatInt(id, 10) + ".png"
		mustWriteFile(t, path, []byte("bytes"+strconv.FormatInt(id, 10)))
		mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 5)`, id, strings.Repeat("d", 63)+strconv.FormatInt(id, 10), path)
	}
	if err := l.jobs.Start("online-lookup"); err != nil {
		t.Fatal(err)
	}

	if _, err := l.runOnlineLookupPhase(context.Background(), cx); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if st := l.jobs.Get(); st.Processed != 3 || st.Total != 3 {
		t.Errorf("progress = %d/%d, want 3/3", st.Processed, st.Total)
	}
}

func TestOnlineLookupPhaseOrderAndBudget(t *testing.T) {
	var posted []map[string]any
	budget := 2
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/lookup" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if len(posted) >= budget {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"budget_exhausted"}`))
			return
		}
		posted = append(posted, req)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":7}`))
	})
	cx := testGallery(t)

	dir := t.TempDir()
	for id := int64(1); id <= 3; id++ {
		path := dir + "/" + strconv.FormatInt(id, 10) + ".png"
		mustWriteFile(t, path, []byte("bytes"+strconv.FormatInt(id, 10)))
		mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 5)`, id, strings.Repeat("d", 63)+strconv.FormatInt(id, 10), path)
	}
	now := time.Now()
	if err := lookup.Record(cx.DB, 1, contract.LookupBooru, lookup.ResultMiss, 0, now.Add(-2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Record(cx.DB, 2, contract.LookupBooru, lookup.ResultMiss, 0, now.Add(-9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	mustExec(t, cx.DB.Write, `UPDATE image_lookups SET next_due_at = CASE image_id WHEN 1 THEN '2026-01-02T00:00:00Z' ELSE '2026-01-01T00:00:00Z' END`)

	res, err := l.runOnlineLookupPhase(context.Background(), cx)
	if err != nil {
		t.Fatalf("phase: %v", err)
	}
	if res.queued != 2 || !res.budgetSpent {
		t.Fatalf("res = %+v, want 2 queued and the budget spent", res)
	}
	if res.stillDue != 1 {
		t.Errorf("still due = %d, want the one image the budget did not reach", res.stillDue)
	}
	if len(posted) != 2 {
		t.Fatalf("posted %d lookups, want 2", len(posted))
	}
	if posted[0]["image_id"] != float64(3) || posted[1]["image_id"] != float64(2) {
		t.Errorf("order = %v %v, want image 3 then image 2", posted[0]["image_id"], posted[1]["image_id"])
	}
	for i, p := range posted {
		if p["background"] != true || p["budgeted"] != true {
			t.Errorf("post %d = %v, want the background lane and the budget", i, p)
		}
	}
	rows, _ := lookup.ForImage(cx.DB, 3)
	if got := rows[contract.LookupBooru]; got.QueuedAt.IsZero() {
		t.Errorf("row = %+v, want the enqueue recorded in flight", got)
	}
}

func TestOnlineLookupPhaseEndsOnAPageItCannotRecord(t *testing.T) {
	l := stubLink(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":7}`))
	})
	cx := testGallery(t)
	for id := int64(1); id <= 3; id++ {
		mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 5)`, id, strings.Repeat("e", 63)+strconv.FormatInt(id, 10), "/gone/"+strconv.FormatInt(id, 10)+".png")
	}

	done := make(chan onlineLookupResult, 1)
	go func() {
		res, err := l.runOnlineLookupPhase(context.Background(), cx)
		if err != nil {
			t.Errorf("phase: %v", err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.queued != 0 || res.skipped != 3 {
			t.Errorf("res = %+v, want the three unreadable rows skipped and nothing queued", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the phase never returned: a page it cannot record is being retried forever")
	}
}

func TestOnlineLookupPhaseReachesRowsBehindAPageItCannotRecord(t *testing.T) {
	var posted []float64
	l := stubLink(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ImageID float64 `json:"image_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		posted = append(posted, req.ImageID)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":7}`))
	})
	cx := testGallery(t)
	for id := int64(1); id <= onlineLookupChunk; id++ {
		mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 5)`, id, strings.Repeat("e", 60)+strconv.FormatInt(id+1000, 10), "/gone/"+strconv.FormatInt(id, 10)+".png")
	}
	readable := filepath.Join(t.TempDir(), "here.png")
	mustWriteFile(t, readable, []byte("bytes"))
	mustExec(t, cx.DB.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 5)`, onlineLookupChunk+1, strings.Repeat("f", 64), readable)

	res, err := l.runOnlineLookupPhase(context.Background(), cx)
	if err != nil {
		t.Fatalf("phase: %v", err)
	}
	if res.queued != 1 || res.skipped != onlineLookupChunk || !slices.Equal(posted, []float64{onlineLookupChunk + 1}) {
		t.Errorf("res = %+v posted = %v, want the readable row behind a page of unreadable ones queued", res, posted)
	}
}

func TestBatchPTRLookupSendsTheBulkLane(t *testing.T) {
	hitSHA := strings.Repeat("d", 64)
	stub := &ptrStubHandler{index: 4812, hits: map[string][]string{hitSHA: {"blue_eyes"}}}
	l := stubLink(t, stub.handle)
	cx := testGallery(t)
	seedPhaseImage(t, cx, 1, hitSHA)

	matched, err := l.batchPTRLookup(context.Background(), cx, []int64{1})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if matched != 1 {
		t.Errorf("matched = %d, want the seeded hit", matched)
	}
	if !slices.Equal(stub.ids, []int64{1}) || !slices.Equal(stub.scheduled, []bool{false}) {
		t.Errorf("sent ids = %v scheduled = %v, want image 1 off the scheduled lane", stub.ids, stub.scheduled)
	}
}

func TestPTRLookupPhaseGatesOnTheCursor(t *testing.T) {
	sha := strings.Repeat("c", 64)
	stub := &ptrStubHandler{index: 100}
	l := stubLink(t, stub.handle)
	cx := testGallery(t)
	seedPhaseImage(t, cx, 1, sha)

	if _, _, err := l.runPTRLookupPhase(context.Background(), cx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	mustExec(t, cx.DB.Write, `UPDATE image_lookups SET next_due_at = ? WHERE image_id = 1`, time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z"))
	before := len(stub.batches)
	if _, _, err := l.runPTRLookupPhase(context.Background(), cx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(stub.batches) != before {
		t.Errorf("asked %d batches, want none against an unmoved index", len(stub.batches)-before)
	}

	stub.index = 200
	if _, _, err := l.runPTRLookupPhase(context.Background(), cx); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(stub.batches) != before+1 {
		t.Errorf("asked %d batches after the index moved, want 1", len(stub.batches)-before)
	}
}
