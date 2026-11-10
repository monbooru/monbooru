package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

func idCSV(ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func downloadedZip(t *testing.T, w *httptest.ResponseRecorder) *zip.Reader {
	t.Helper()
	mustCode(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", ct)
	}
	body := w.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

func zipNames(zr *zip.Reader) []string {
	names := make([]string, len(zr.File))
	for i, f := range zr.File {
		names[i] = f.Name
	}
	return names
}

func downloadFilename(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" {
		t.Fatalf("Content-Disposition = %q (%v), want an attachment", w.Header().Get("Content-Disposition"), err)
	}
	return params["filename"]
}

func TestBatchDownload_ZipsTheSelectionInPickOrder(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	seedImage(t, srv, "b.png", 11, 11)
	c := seedImage(t, srv, "c.png", 12, 12)

	zr := downloadedZip(t, postForm(t, srv, "/internal/batch-download", url.Values{
		"scope": {"selection"}, "ids": {idCSV(c, a)},
	}))
	if got := zipNames(zr); !slices.Equal(got, []string{"c.png", "a.png"}) {
		t.Fatalf("entries = %v, want [c.png a.png]", got)
	}
	for _, f := range zr.File {
		want, err := os.ReadFile(filepath.Join(srv.active().GalleryPath, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, want) || f.Method != zip.Store {
			t.Errorf("%s: stored=%v, same bytes=%v", f.Name, f.Method == zip.Store, bytes.Equal(got, want))
		}
	}
}

func TestBatchDownload_SearchWalksTheOrderOnScreen(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	zeta := seedImage(t, srv, "zeta.png", 10, 10)
	alpha := seedImage(t, srv, "alpha.png", 11, 11)
	for i, id := range []int64{alpha, zeta} {
		pos := i + 1
		mustAddCollection(t, cx.DB, id, "Trip", &pos)
	}
	cx.InvalidateCaches()

	w := postForm(t, srv, "/internal/batch-download", url.Values{
		"scope": {"search"}, "q": {`collection:"Trip"`}, "sort": {"order"}, "order": {"asc"},
	})
	if got := zipNames(downloadedZip(t, w)); !slices.Equal(got, []string{"alpha.png", "zeta.png"}) {
		t.Errorf("entries = %v, want reading order", got)
	}
	if name := downloadFilename(t, w); !regexp.MustCompile(`^\d{8}-\d{6}-Trip\.zip$`).MatchString(name) {
		t.Errorf("zip name = %q, want the time then the pinned collection", name)
	}
}

func TestBatchDownload_HonoursTheRatingCeiling(t *testing.T) {
	srv := newTestServer(t)
	seedRatedPair(t, srv)

	w := postCeilingRequest(t, srv, "/internal/batch-download", url.Values{"scope": {"search"}, "q": {""}})
	if got := zipNames(downloadedZip(t, w)); !slices.Equal(got, []string{"safe.png"}) {
		t.Errorf("entries = %v, want only the image under the ceiling", got)
	}
}

func TestBatchDownload_LeavesWhatItCannotReadOut(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	gone := seedImage(t, srv, "gone.png", 11, 11)
	outside := seedImage(t, srv, "outside.png", 12, 12)
	mustExec(t, srv.db().Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)
	mustExec(t, srv.db().Write, `UPDATE images SET canonical_path = '/elsewhere/outside.png' WHERE id = ?`, outside)
	form := url.Values{"scope": {"selection"}, "ids": {idCSV(a, gone, outside)}}

	count := postForm(t, srv, "/internal/batch-download/count", form)
	mustCode(t, count, http.StatusOK)
	var got map[string]any
	mustUnmarshal(t, count.Body.Bytes(), &got)
	if got["files"] != 1.0 || got["missing"] != 2.0 {
		t.Errorf("count = %v, want 1 file and 2 missing", got)
	}
	if names := zipNames(downloadedZip(t, postForm(t, srv, "/internal/batch-download", form))); !slices.Equal(names, []string{"a.png"}) {
		t.Errorf("entries = %v, want [a.png]", names)
	}
}

func TestBatchDownload_Refusals(t *testing.T) {
	srv := newTestServer(t)
	gone := seedImage(t, srv, "gone.png", 11, 11)
	mustExec(t, srv.db().Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, gone)

	w := postForm(t, srv, "/internal/batch-download", url.Values{"scope": {"selection"}, "ids": {idCSV(gone)}})
	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "Nothing to download")

	degraded := newTestServerWith(t, true, nil)
	w = postForm(t, degraded, "/internal/batch-download", url.Values{"scope": {"selection"}, "ids": {"1"}})
	wantCode(t, w, http.StatusServiceUnavailable)
	wantIn(t, w.Body.String(), "gallery path is unreadable")
}

func TestBatchDownload_NamesTheZipAfterTheListing(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	for q, label := range map[string]string{`collection:"Trip"`: "Trip", "": "default"} {
		w := postForm(t, srv, "/internal/batch-download", url.Values{
			"scope": {"selection"}, "ids": {idCSV(a)}, "q": {q},
		})
		mustCode(t, w, http.StatusOK)
		if got := downloadFilename(t, w); !regexp.MustCompile(`^\d{8}-\d{6}-` + label + `\.zip$`).MatchString(got) {
			t.Errorf("q %q saves as %q, want the time then %s", q, got, label)
		}
	}
}

func TestBatchDownload_CarriesTagsWhenAsked(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	mustAddTag(t, cx.TagSvc, a, mustTag(t, cx.TagSvc, "solo", 1).ID, false, nil)

	zr := downloadedZip(t, postForm(t, srv, "/internal/batch-download", url.Values{
		"scope": {"selection"}, "ids": {idCSV(a, b)}, "tags": {"1"},
	}))
	if got := zipNames(zr); !slices.Equal(got, []string{"tags.json", "a.png", "b.png"}) {
		t.Fatalf("entries = %v, want tags.json then the files", got)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	var mf galleryio.LightManifest
	err = json.NewDecoder(rc).Decode(&mf)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	tagsByPath := map[string][]string{}
	for _, img := range mf.Images {
		tagsByPath[img.Path] = img.Tags
	}
	if !slices.Contains(tagsByPath["a.png"], "solo") {
		t.Errorf("manifest = %+v, want a.png tagged solo", mf.Images)
	}
	if _, ok := tagsByPath["b.png"]; !ok {
		t.Errorf("manifest = %+v, want b.png listed", mf.Images)
	}
}

func TestBatchDownloadCount_PricesTheScope(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	var bytesWant int64
	if err := srv.db().Read.QueryRow(`SELECT SUM(file_size) FROM images`).Scan(&bytesWant); err != nil {
		t.Fatal(err)
	}

	w := postForm(t, srv, "/internal/batch-download/count", url.Values{
		"scope": {"selection"}, "ids": {idCSV(a, b)},
	})
	mustCode(t, w, http.StatusOK)
	var got struct {
		Files, Missing int
		Bytes          int64
		Size           string
	}
	mustUnmarshal(t, w.Body.Bytes(), &got)
	if got.Files != 2 || got.Missing != 0 || got.Bytes != bytesWant || got.Size != webui.HumanBytes(bytesWant) {
		t.Errorf("count = %+v, want 2 files, %d bytes", got, bytesWant)
	}
}

type stalledWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stalledWriter) Header() http.Header { return s.header }
func (s *stalledWriter) WriteHeader(int)     {}
func (s *stalledWriter) Write(p []byte) (int, error) {
	s.once.Do(func() {
		close(s.started)
		<-s.release
	})
	return len(p), nil
}

func TestBatchDownload_StreamsWithoutTheGalleryLock(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	csrf := srv.sessions.CSRFToken("anon")
	form := url.Values{"scope": {"selection"}, "ids": {idCSV(a)}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/internal/batch-download", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	sw := &stalledWriter{header: http.Header{}, started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Handler().ServeHTTP(sw, req)
	}()
	select {
	case <-sw.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the download never started streaming")
	}

	if srv.set.TryLock() {
		srv.set.Unlock()
	} else {
		t.Error("the download holds the gallery lock while it streams")
	}
	wantIn(t, do(srv, "GET", "/internal/job/status").Body.String(), "job-download")

	close(sw.release)
	<-done
	wantNotIn(t, do(srv, "GET", "/internal/job/status").Body.String(), "job-download")
}
