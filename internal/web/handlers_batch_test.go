package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestBatchDelete_TakesASelectionPastTheFormParameterCap(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "victim.png", 10, 10)

	ids := make([]string, 0, 40001)
	ids = append(ids, strconv.FormatInt(id, 10))
	for n := int64(1); n <= 40000; n++ {
		ids = append(ids, strconv.FormatInt(id+n, 10))
	}
	w := postForm(t, srv, "/internal/batch-delete", url.Values{"ids": {strings.Join(ids, ",")}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-delete with %d ids = %d, want 202: %s", len(ids), w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)
	if rows := countRows(t, srv.db(), "images WHERE id = ?", id); rows != 0 {
		t.Error("the one real id in the list was not deleted")
	}
}

func TestRunBulkDeleteClearsDuplicateGroupsInScope(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	var ids []int64
	for _, name := range []string{"dup_a", "dup_b", "loner"} {
		p := filepath.Join(cx.GalleryPath, name+".png")
		mustWriteFile(t, p, []byte(name), 0o644)
		var id int64
		if err := cx.DB.Write.QueryRow(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
			 VALUES (?, ?, 'png', 5, datetime('now')) RETURNING id`, "sha-"+name, p,
		).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		ids = append(ids, id)
	}
	if err := cx.RelationsSvc.AddDuplicate(ids[0], ids[1]); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}

	srv.runBulkDelete(ids)

	remaining := countRows(t, cx.DB, "images")
	if remaining != 0 {
		t.Errorf("images remaining = %d, want 0", remaining)
	}
	groups := countRows(t, cx.DB, "dup_groups")
	if groups != 0 {
		t.Errorf("dup_groups remaining = %d, want 0", groups)
	}
}

func TestDeleteRemovesTheRowsOtherCopies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch bool
	}{{"single", false}, {"batch", true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			cx := srv.active()

			canonical := filepath.Join(cx.GalleryPath, "curated.png")
			copy1 := filepath.Join(cx.GalleryPath, "sub", "same-bytes.png")
			mustMkdirAll(t, filepath.Dir(copy1), 0o755)
			for _, p := range []string{canonical, copy1} {
				mustWriteFile(t, p, []byte("bytes"), 0o644)
			}
			id, _ := mustExec(t, cx.DB.Write,
				`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
				 VALUES (?, ?, 'png', 5, datetime('now'))`, bytesSHA, canonical).LastInsertId()
			for _, p := range []struct {
				path      string
				canonical int
			}{{canonical, 1}, {copy1, 0}} {
				mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, ?)`, id, p.path, p.canonical)
			}

			if tc.batch {
				srv.runBulkDelete([]int64{id})
			} else if _, err := cx.DeleteImages(context.Background(), []int64{id}, library.DeleteOptions{}, nil); err != nil {
				t.Fatalf("DeleteImages: %v", err)
			}

			for _, p := range []string{canonical, copy1} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s survived the delete, stat err = %v", p, err)
				}
			}
		})
	}
}

const bytesSHA = "277089d91c0bdf4f2e6862ba7e4a07605119431f5d13f726dd352b06f1b206a9"

func TestRunBulkDeleteRefusesFileOutsideGalleryRoot(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()

	outside := filepath.Join(t.TempDir(), "outside.png")
	mustWriteFile(t, outside, []byte("bytes"), 0o644)
	inside := filepath.Join(cx.GalleryPath, "inside.png")
	mustWriteFile(t, inside, []byte("bytes"), 0o644)

	var outID, inID int64
	for _, tc := range []struct {
		path string
		dst  *int64
		sha  string
	}{{outside, &outID, "sha-out"}, {inside, &inID, bytesSHA}} {
		*tc.dst, _ = mustExec(t, cx.DB.Write,
			`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
			 VALUES (?, ?, 'png', 5, datetime('now'))`, tc.sha, tc.path).LastInsertId()
	}

	srv.runBulkDelete([]int64{outID, inID})

	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside the gallery root was removed: %v", err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Errorf("file inside the gallery root should be gone, stat err = %v", err)
	}
	remaining := countRows(t, cx.DB, "images")
	if remaining != 0 {
		t.Errorf("both rows should be deleted regardless of the unlink gate, got %d", remaining)
	}
}

func TestDeleteSearch_HonoursComfyUITerm(t *testing.T) {
	srv := newTestServer(t)
	doomed := seedImage(t, srv, "comfy_doomed.png", 8, 8)
	spared := seedImage(t, srv, "comfy_spared.png", 9, 9)
	seedWorkflow(t, srv, doomed, `{"1": {"class_type": "IPAdapterAdvanced", "inputs": {}}}`)
	seedWorkflow(t, srv, spared, `{"1": {"class_type": "KSampler", "inputs": {}}}`)

	w := postForm(t, srv, "/internal/delete-search",
		url.Values{"q": {"comfyui:IPAdapterAdvanced"}})
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)

	left, err := db.QueryIDs(srv.db().Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != spared {
		t.Fatalf("images left = %v, want [%d]", left, spared)
	}
}

func seedWorkflow(t *testing.T, srv *Server, imageID int64, raw string) {
	t.Helper()
	mustExec(t, srv.db().Write, `INSERT INTO comfyui_metadata (image_id, raw_workflow) VALUES (?, ?)`, imageID, raw)
	if _, err := gallery.BackfillComfyTerms(context.Background(), srv.db(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSuggest_ComfyUITerms(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "sug.png", 8, 8)
	seedWorkflow(t, srv, id, `{
		"1": {"class_type": "IPAdapterAdvanced", "inputs": {"weight_type": "linear"}},
		"2": {"class_type": "LoraLoader", "inputs": {"lora_name": "elong_style.safetensors"}}}`)

	for _, tc := range []struct {
		q          string
		want, deny string
	}{
		{"comfyui:ipa", `data-tag-name="comfyui:&#34;IPAdapterAdvanced&#34;"`, "node="},
		{"comfyui:lora_name=", `data-tag-name="comfyui:&#34;lora_name=elong_style.safetensors&#34;"`, ""},
		{"comfyui:weight_type=li", `data-tag-name="comfyui:&#34;weight_type=linear&#34;"`, ""},
	} {
		w := do(srv, "GET", "/internal/search/suggest?q="+url.QueryEscape(tc.q))
		mustCode(t, w, http.StatusOK)
		wantIn(t, w.Body.String(), tc.want)
		if tc.deny != "" && strings.Contains(w.Body.String(), tc.deny) {
			t.Errorf("%q: suggestion still carries %q", tc.q, tc.deny)
		}
	}

	w := do(srv, "GET", "/internal/search/suggest?q="+url.QueryEscape("comfyui:i"))
	mustCode(t, w, http.StatusNoContent)
}

func stubBatchLookup(t *testing.T, srv *Server, ptrStatus int, hits map[string][]string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ImageID int64  `json:"image_id"`
			Backend string `json:"backend"`
			URL     string `json:"url"`
			Images  []struct {
				ImageID int64  `json:"image_id"`
				SHA256  string `json:"sha256"`
			} `json:"images"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/ptr/lookup":
			ids := make([]string, len(req.Images))
			results := map[string][]string{}
			for i, img := range req.Images {
				ids[i] = strconv.FormatInt(img.ImageID, 10)
				if tags, ok := hits[img.SHA256]; ok {
					results[img.SHA256] = tags
				}
			}
			calls = append(calls, "ptr "+strings.Join(ids, ","))
			if ptrStatus != http.StatusOK {
				w.WriteHeader(ptrStatus)
				return
			}
			out, _ := json.Marshal(map[string]any{"index": 1, "results": results})
			_, _ = w.Write(out)
		case "/api/v1/lookup":
			calls = append(calls, fmt.Sprintf("lookup %d %s", req.ImageID, req.Backend))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job_id":7}`))
		case "/api/v1/metadata":
			calls = append(calls, fmt.Sprintf("metadata %d %s", req.ImageID, req.URL))
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func seedLookupCandidate(t *testing.T, srv *Server, id int64, sha string) {
	t.Helper()
	mustExec(t, srv.db().Write, `INSERT INTO images (id, sha256, md5, canonical_path, file_type, file_size) VALUES (?, ?, ?, ?, 'png', 1)`,
		id, sha, sha[:32], "/g/"+sha+".png")
}

func runBatchLookupForm(t *testing.T, srv *Server, form url.Values) (summary, failure string) {
	t.Helper()
	w := postForm(t, srv, "/internal/batch-lookup", form)
	mustCode(t, w, http.StatusAccepted)
	awaitJobsDrain(t, srv)
	st := srv.jobs.Get()
	return st.Summary, st.Error
}

func TestBatchLookup_PTRFirstThenOnline(t *testing.T) {
	srv := newTestServer(t)
	hit := strings.Repeat("a", 64)
	calls := stubBatchLookup(t, srv, http.StatusOK, map[string][]string{hit: {"blue_eyes"}})
	seedLookupCandidate(t, srv, 1, hit)
	seedLookupCandidate(t, srv, 2, strings.Repeat("b", 64))

	summary, failure := runBatchLookupForm(t, srv, url.Values{
		"scope": {"selection"}, "ids": {"1,2"}, "mode": {"hash"}, "ptr": {"1"}, "booru": {"1"},
	})
	if failure != "" {
		t.Fatalf("job failed: %s", failure)
	}
	if want := []string{"ptr 1,2", "lookup 1 booru", "lookup 2 booru"}; !slices.Equal(calls(), want) {
		t.Errorf("calls = %q, want %q", calls(), want)
	}
	if want := "Looked up 2 images: 1 matched in the PTR, 2 queued on monloader."; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
	rows, _ := lookup.ForImage(srv.active().DB, 2)
	if rows[contract.LookupBooru].QueuedAt.IsZero() {
		t.Errorf("image 2 booru row = %+v, want the enqueue recorded in flight", rows[contract.LookupBooru])
	}
}

func TestBatchLookup_UnsourcedOnlySkipsSourcedImages(t *testing.T) {
	srv := newTestServer(t)
	calls := stubBatchLookup(t, srv, http.StatusOK, nil)
	for id := int64(1); id <= 3; id++ {
		seedLookupCandidate(t, srv, id, strings.Repeat(strconv.FormatInt(id, 10), 64))
	}
	mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (1, 'danbooru', '1', 'https://danbooru.donmai.us/posts/1')`)

	mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (3, 'ptr', '', '')`)

	summary, failure := runBatchLookupForm(t, srv, url.Values{
		"scope": {"selection"}, "ids": {"1,2,3"}, "mode": {"hash"}, "booru": {"1"}, "unsourced": {"1"},
	})
	if failure != "" {
		t.Fatalf("job failed: %s", failure)
	}
	if want := []string{"lookup 2 booru", "lookup 3 booru"}; !slices.Equal(calls(), want) {
		t.Errorf("calls = %q, want %q", calls(), want)
	}
	if want := "Looked up 2 images: 2 queued on monloader."; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func TestBatchLookup_PTRUnavailableEndsThePTRLegOnly(t *testing.T) {
	srv := newTestServer(t)
	calls := stubBatchLookup(t, srv, http.StatusConflict, nil)
	seedLookupCandidate(t, srv, 1, strings.Repeat("c", 64))

	summary, failure := runBatchLookupForm(t, srv, url.Values{
		"scope": {"selection"}, "ids": {"1"}, "mode": {"hash"}, "ptr": {"1"}, "booru": {"1"},
	})
	if failure != "" {
		t.Fatalf("job failed: %s", failure)
	}
	if want := []string{"ptr 1", "lookup 1 booru"}; !slices.Equal(calls(), want) {
		t.Errorf("calls = %q, want %q", calls(), want)
	}
	if want := "Looked up 1 image: the PTR is unavailable on monloader, 1 queued on monloader."; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func TestBatchLookup_ARevokedTokenCountsAsRefusedNotSkipped(t *testing.T) {
	srv := newTestServer(t)
	pairStubMonloader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	seedLookupCandidate(t, srv, 1, strings.Repeat("f", 64))
	seedLookupCandidate(t, srv, 2, strings.Repeat("9", 64))

	summary, failure := runBatchLookupForm(t, srv, url.Values{
		"scope": {"selection"}, "ids": {"1,2"}, "mode": {"hash"}, "booru": {"1"},
	})
	if failure != "" {
		t.Fatalf("job failed: %s", failure)
	}
	if want := "Looked up 2 images: 0 queued on monloader, 2 refused by monloader."; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func TestBatchLookup_RefreshQueuesEachSourceOnce(t *testing.T) {
	srv := newTestServer(t)
	calls := stubBatchLookup(t, srv, http.StatusOK, nil)
	seedLookupCandidate(t, srv, 1, strings.Repeat("d", 64))
	seedLookupCandidate(t, srv, 2, strings.Repeat("e", 64))
	post := "https://danbooru.donmai.us/posts/9"
	mustExec(t, srv.db().Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (1, 'danbooru', '9', ?), (1, 'gelbooru', '9', ?), (1, 'ptr', '', '')`, post, " "+post+" ")

	summary, failure := runBatchLookupForm(t, srv, url.Values{
		"scope": {"selection"}, "ids": {"1,2"}, "mode": {"refresh"},
	})
	if failure != "" {
		t.Fatalf("job failed: %s", failure)
	}
	if want := []string{"metadata 1 " + post, "lookup 1 ptr"}; !slices.Equal(calls(), want) {
		t.Errorf("calls = %q, want %q", calls(), want)
	}
	if want := "Queued 2 source fetches on monloader; skipped 1 without a fetchable source."; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}
