package api

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

func fixedResolver(g *library.Gallery) ResolverFunc {
	return func(name string) (*library.Gallery, bool) {
		if name == "" || name == g.Name {
			return g, true
		}
		return nil, false
	}
}

func testGallery(name string, database *db.DB) *library.Gallery {
	g, err := library.New(gallery.Handle{Name: name, DB: database})
	if err != nil {
		panic(err)
	}
	return g
}

type testEnv struct {
	handler    *Handler
	mux        http.Handler
	database   *db.DB
	cfg        *config.Config
	galleryDir string
	thumbDir   string
	bounds     *atomic.Pointer[gallery.Boundary]
	gallery    *library.Gallery
}

func newBareEnv(t *testing.T, cfgMu *sync.RWMutex, mutate func(*config.Config, *library.Gallery)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	newBareHandler(t, cfgMu, mutate).Mount(mux)
	return mux
}

func newBareHandler(t *testing.T, cfgMu *sync.RWMutex, mutate func(*config.Config, *library.Gallery)) *Handler {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfg := config.Default()
	g := testGallery(cfg.DefaultGallery, database)
	if mutate != nil {
		mutate(cfg, g)
	}
	if cfgMu == nil {
		cfgMu = new(sync.RWMutex)
	}
	return New(testSnapshot(cfg, cfgMu), jobs.NewManager(), fixedResolver(g), "v-test")
}

func testSnapshot(cfg *config.Config, mu *sync.RWMutex) func() *config.Config {
	return func() *config.Config {
		mu.RLock()
		defer mu.RUnlock()
		c := *cfg
		c.Galleries = slices.Clone(c.Galleries)
		c.Plugins = slices.Clone(c.Plugins)
		c.Auth.Tokens = slices.Clone(c.Auth.Tokens)
		c.Tagger.Taggers = slices.Clone(c.Tagger.Taggers)
		return &c
	}
}

func (e *testEnv) do(method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func (e *testEnv) writePNG(t *testing.T, name string, w, h int) string {
	t.Helper()
	path := filepath.Join(e.galleryDir, name)
	f := mustCreate(t, path)
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func doJSON(mux http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func mustTag(t *testing.T, svc *tags.Service, name string, categoryID int64) *models.Tag {
	t.Helper()
	tag, err := svc.GetOrCreateTag(name, categoryID)
	if err != nil {
		t.Fatalf("GetOrCreateTag(%q): %v", name, err)
	}
	return tag
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("expected %d, got %d: %s", want, w.Code, w.Body.String())
	}
}

func mustCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("expected %d, got %d: %s", want, w.Code, w.Body.String())
	}
}

func wantIn(t *testing.T, haystack, want string) {
	t.Helper()
	if !strings.Contains(haystack, want) {
		t.Errorf("missing %q in %q", want, haystack)
	}
}

func wantNotIn(t *testing.T, haystack, unwanted string) {
	t.Helper()
	if strings.Contains(haystack, unwanted) {
		t.Errorf("unexpected %q in %q", unwanted, haystack)
	}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { gallery.WaitPageThumbs(10 * time.Second) })

	galleryDir := filepath.Join(dir, "gallery")
	thumbDir := filepath.Join(dir, "thumbs")
	mustMkdirAll(t, galleryDir, 0755)
	mustMkdirAll(t, thumbDir, 0755)

	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfg := config.Default()
	cfg.Galleries[0].GalleryPath = galleryDir
	cfg.Galleries[0].DBPath = filepath.Join(dir, "test.db")
	cfg.Galleries[0].ThumbnailsPath = thumbDir
	cfg.Gallery.MaxFileSizeMB = 100
	cfg.Auth.Tokens = []apitoken.Token{{ID: "test", Name: "test", TokenHash: apitoken.Hash(testAPIToken), Scopes: config.AllScopes}}

	bounds := new(atomic.Pointer[gallery.Boundary])
	g := testGallery(cfg.DefaultGallery, database)
	g.GalleryPath, g.ThumbnailsPath, g.Bounds = galleryDir, thumbDir, bounds
	h := New(testSnapshot(cfg, new(sync.RWMutex)), jobs.NewManager(), fixedResolver(g), "v-test")
	raw := http.NewServeMux()
	h.Mount(raw)
	mux := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+testAPIToken)
		}
		raw.ServeHTTP(w, r)
	}))

	return &testEnv{handler: h, mux: mux, database: database, cfg: cfg,
		galleryDir: galleryDir, thumbDir: thumbDir, bounds: bounds, gallery: g}
}

const testAPIToken = "test-api-token"

func (e *testEnv) createTestImage(t *testing.T, name string, w, h int) int64 {
	t.Helper()
	path := e.writePNG(t, name, w, h)
	record, _ := mustIngest(t, e.database, e.galleryDir, e.thumbDir, path, "")
	return record.ID
}

func newTestMux(t *testing.T) http.Handler {
	return newTestEnv(t).mux
}

func TestResolveGallery_PicksTheNamedGallery(t *testing.T) {
	open := func() *db.DB {
		database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
		if err := db.Bootstrap(database); err != nil {
			t.Fatal(err)
		}
		return database
	}
	first, second := testGallery("default", open()), testGallery("second", open())
	mustExec(t, second.DB.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES ('second-sha', '/second/a.png', 'png', 1)`)
	named := ResolverFunc(func(name string) (*library.Gallery, bool) {
		switch name {
		case "", "default":
			return first, true
		case "second":
			return second, true
		}
		return nil, false
	})
	serve := func(resolver ResolverFunc, target, header string) *httptest.ResponseRecorder {
		cfg := config.Default()
		cfg.Auth.Tokens = []apitoken.Token{{ID: "test", Name: "test", TokenHash: apitoken.Hash(testAPIToken), Scopes: config.AllScopes}}
		mux := http.NewServeMux()
		New(testSnapshot(cfg, new(sync.RWMutex)), jobs.NewManager(), resolver, "v-test").Mount(mux)
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIToken)
		if header != "" {
			req.Header.Set("X-Monbooru-Gallery", header)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	for _, tc := range []struct {
		name, target, header string
		total                int
	}{
		{"the query names it", "/api/v1/images/search?q=&gallery=second", "", 1},
		{"the header names it", "/api/v1/images/search?q=", "second", 1},
		{"the query wins over the header", "/api/v1/images/search?q=&gallery=default", "second", 0},
		{"no name is the default", "/api/v1/images/search?q=", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(named, tc.target, tc.header)
			mustCode(t, w, http.StatusOK)
			var page struct {
				Total int `json:"total"`
			}
			mustDecode(t, w, &page)
			if page.Total != tc.total {
				t.Errorf("total = %d, want %d", page.Total, tc.total)
			}
		})
	}

	w := serve(named, "/api/v1/images/search?q=&gallery=nope", "")
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), `"invalid_gallery"`)

	none := ResolverFunc(func(string) (*library.Gallery, bool) { return nil, false })
	w = serve(none, "/api/v1/images/search?q=", "")
	mustCode(t, w, http.StatusServiceUnavailable)
	wantIn(t, w.Body.String(), `"api_disabled"`)
}

func TestOpenAPIJSON(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/openapi.json")

	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "openapi")
	wantIn(t, w.Body.String(), "409 setup_required")
}

func TestGetImageNotFound(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/images/99999")

	wantCode(t, w, http.StatusNotFound)
}

func TestUnknownAPIPathAnswersJSONForEveryMethod(t *testing.T) {
	env := newTestEnv(t)
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		for _, target := range []string{"/api/v1/nope", "/api/v1/galleries"} {
			if method == "GET" && target == "/api/v1/galleries" {
				continue
			}
			w := env.do(method, target)
			if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json" {
				t.Errorf("%s %s: %d %q %s", method, target, w.Code, w.Header().Get("Content-Type"), w.Body.String())
				continue
			}
			wantIn(t, w.Body.String(), `"code":"not_found"`)
			wantIn(t, w.Body.String(), "endpoint not found: "+method+" "+target)
		}
	}
}

func TestSearchImagesReturnsEnvelope(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/images/search?q=")

	wantCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, key := range []string{"page", "limit", "total", "results"} {
		wantIn(t, body, key)
	}
}

func TestSearch_AHugePageIsPastTheEnd(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "huge_page.png", 12, 12)

	for _, path := range []string{"/api/v1/images/search", "/api/v1/tags"} {
		for _, page := range []int{math.MaxInt/2 + 1, math.MaxInt} {
			rec := env.do("GET", fmt.Sprintf("%s?page=%d", path, page))
			mustCode(t, rec, http.StatusOK)
			var resp struct {
				Results []json.RawMessage `json:"results"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Results) != 0 {
				t.Errorf("%s page %d served %d row(s), want none", path, page, len(resp.Results))
			}
		}
	}
}

func TestSearchImages_PopulatesNote(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "search_note.png", 12, 12)
	mustExec(t, env.database.Write, `UPDATE images SET note = 'remember this' WHERE id = ?`, id)

	rec := env.do("GET", "/api/v1/images/search")
	mustCode(t, rec, http.StatusOK)
	var resp struct {
		Results []struct {
			ID   int64  `json:"id"`
			Note string `json:"note"`
		} `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range resp.Results {
		if r.ID == id {
			found = true
			if r.Note != "remember this" {
				t.Errorf("note in search result = %q, want %q", r.Note, "remember this")
			}
		}
	}
	if !found {
		t.Fatal("seeded image missing from search results")
	}
}

func TestGetImage_PopulatesOriginalSource(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "orig_src.png", 12, 12)
	mustExec(t, env.database.Write, `UPDATE images SET original_source = 'https://example.test/art/1' WHERE id = ?`, id)

	rec := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	mustCode(t, rec, http.StatusOK)
	var resp struct {
		OriginalSource string `json:"original_source"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OriginalSource != "https://example.test/art/1" {
		t.Errorf("original_source = %q, want the seeded URL", resp.OriginalSource)
	}
}

func TestImageTags_ShapeMatchesTheImageResponse(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_shape.png", 8, 8)

	listTags := func() (int, string) {
		w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/tags", id))
		return w.Code, strings.TrimSpace(w.Body.String())
	}

	mustExec(t, env.database.Write, `DELETE FROM image_tags WHERE image_id = ?`, id)
	if code, body := listTags(); code != http.StatusOK || body != "[]" {
		t.Fatalf("no tags yet: got %d %q, want 200 []", code, body)
	}

	body, _ := json.Marshal(map[string]any{"tags": []string{"artist:someone", "plain"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	mustCode(t, w, http.StatusOK)

	code, listed := listTags()
	if code != http.StatusOK {
		t.Fatalf("list after add: %d", code)
	}
	var fromList []map[string]any
	if err := json.Unmarshal([]byte(listed), &fromList); err != nil {
		t.Fatalf("list is not a tag array: %v (%s)", err, listed)
	}
	var fromAdd []map[string]any
	mustDecode(t, w, &fromAdd)
	if len(fromList) != 2 || len(fromAdd) != 2 {
		t.Fatalf("want 2 tags from both, got add=%d list=%d", len(fromAdd), len(fromList))
	}

	imgRec := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	var img map[string]any
	if err := json.NewDecoder(imgRec.Body).Decode(&img); err != nil {
		t.Fatalf("decode image: %v", err)
	}
	imgTags, _ := img["tags"].([]any)
	if len(imgTags) != len(fromList) {
		t.Fatalf("image response carries %d tags, the tag endpoint %d", len(imgTags), len(fromList))
	}
	first, _ := imgTags[0].(map[string]any)
	for key := range first {
		if _, ok := fromList[0][key]; !ok {
			t.Errorf("tag endpoint entry is missing %q the image response carries", key)
		}
	}
}

func TestSearchImages_PopulatesTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "search_tags.png", 12, 12)

	body, _ := json.Marshal(map[string]any{"tags": []string{"red", "blue"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	mustCode(t, w, http.StatusOK)

	rec := env.do("GET", "/api/v1/images/search")
	mustCode(t, rec, http.StatusOK)
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	results, _ := resp["results"].([]any)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	tags, _ := results[0].(map[string]any)["tags"].([]any)
	if len(tags) < 2 {
		t.Errorf("expected >= 2 tags on the search result, got %d (full response: %s)", len(tags), rec.Body.String())
	}
}

func TestListTagsReturnsEnvelope(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/tags")

	wantCode(t, w, http.StatusOK)
	body := w.Body.String()
	for _, key := range []string{"page", "limit", "total", "results"} {
		wantIn(t, body, key)
	}
}

func TestAPIDisabledWhenNoToken(t *testing.T) {
	mux := newBareEnv(t, nil, func(cfg *config.Config, g *library.Gallery) {
		cfg.Auth.Tokens = nil
	})

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	wantCode(t, w, http.StatusServiceUnavailable)
	wantIn(t, w.Body.String(), "api_disabled")
}

func TestBearerAuth(t *testing.T) {
	mux := newBareEnv(t, nil, func(cfg *config.Config, g *library.Gallery) {
		cfg.Auth.Tokens = []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash("secret-token"), Scopes: config.AllScopes}}
	})
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"RejectsInvalidToken", "wrong-token", http.StatusUnauthorized},
		{"AcceptsValidToken", "secret-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/tags", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			wantCode(t, w, tc.want)
		})
	}
}

func TestBearerAuthEnforcesScopes(t *testing.T) {
	mux := newBareEnv(t, nil, func(cfg *config.Config, g *library.Gallery) {
		cfg.Auth.Tokens = []apitoken.Token{{ID: "ro", Name: "ro", TokenHash: apitoken.Hash("ro-token"), Scopes: []string{config.ScopeRead}}}
	})

	get := httptest.NewRequest("GET", "/api/v1/tags", nil)
	get.Header.Set("Authorization", "Bearer ro-token")
	gw := httptest.NewRecorder()
	mux.ServeHTTP(gw, get)
	wantCode(t, gw, http.StatusOK)

	del := httptest.NewRequest("DELETE", "/api/v1/images/1", nil)
	del.Header.Set("Authorization", "Bearer ro-token")
	dw := httptest.NewRecorder()
	mux.ServeHTTP(dw, del)
	wantCode(t, dw, http.StatusForbidden)
	wantIn(t, dw.Body.String(), "insufficient_scope")
}

func TestBearerAuthTokenReadRaceFree(t *testing.T) {
	tok, secret := apitoken.New("primary", config.AllScopes)
	var mu sync.RWMutex

	var cfg *config.Config
	mux := newBareEnv(t, &mu, func(c *config.Config, g *library.Gallery) {
		c.Auth.Tokens = []apitoken.Token{tok}
		cfg = c
	})

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 300; i++ {
			extra, _ := apitoken.New(fmt.Sprintf("x%d", i), config.AllScopes)
			mu.Lock()
			cfg.Auth.Tokens = append(cfg.Auth.Tokens, extra)
			if len(cfg.Auth.Tokens) > 4 {
				cfg.Auth.Tokens = cfg.Auth.Tokens[:1]
			}
			mu.Unlock()
		}
	})
	for r := 0; r < 4; r++ {
		wg.Go(func() {
			for i := 0; i < 300; i++ {
				req := httptest.NewRequest("GET", "/api/v1/tags", nil)
				req.Header.Set("Authorization", "Bearer "+secret)
				mux.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
	wg.Wait()
}

func TestSettingsChurnReadRaceFree(t *testing.T) {
	var mu sync.RWMutex
	var cfg *config.Config
	h := newBareHandler(t, &mu, func(c *config.Config, g *library.Gallery) {
		c.Tagger.Taggers = []config.TaggerInstance{{Name: "wd-swinv2", Enabled: true}}
		cfg = c
	})

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 2000; i++ {
			mu.Lock()
			cfg.UI.PageSize = 20 + i%40
			cfg.Gallery.MaxFileSizeMB = i % 8
			cfg.Tagger.Taggers[0].Enabled = i%2 == 0
			cfg.Tagger.Taggers[0].ConfidenceThreshold = float64(i%10) / 10
			mu.Unlock()
		}
	})
	wg.Go(func() {
		for i := 0; i < 2000; i++ {
			h.searchImages(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/images/search?q=", nil))
		}
	})
	wg.Wait()
}

func TestOpenAPIDocs(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/docs")

	wantCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "Monbooru API")
	wantIn(t, body, "/api/v1/openapi.json")
	wantIn(t, body, "/images/search")
	wantIn(t, body, `id="get-images-id"`)
	if strings.Contains(body, "unpkg.com") || strings.Contains(body, "cdn.") {
		t.Error("docs response should not load any external assets")
	}
}

func TestGetImage_ValidID(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "get_test.png", 10, 10)

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))

	wantCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["id"] == nil {
		t.Error("response missing 'id'")
	}
}

func TestGetImage_IncludesCollectionsArray(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "collections_api.png", 10, 10)
	if err := gallery.AddCollectionMembership(env.database, id, "Alpha", nil); err != nil {
		t.Fatal(err)
	}
	order := 4
	if err := gallery.AddCollectionMembership(env.database, id, "Beta", &order); err != nil {
		t.Fatal(err)
	}

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	mustCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)

	if resp["collection"] != "Alpha" {
		t.Errorf("collection = %v, want Alpha", resp["collection"])
	}
	cols, ok := resp["collections"].([]any)
	if !ok || len(cols) != 2 {
		t.Fatalf("collections = %v, want 2 entries", resp["collections"])
	}
	names := map[string]bool{}
	for _, c := range cols {
		names[c.(map[string]any)["name"].(string)] = true
	}
	if !names["Alpha"] || !names["Beta"] {
		t.Errorf("collections names = %v, want Alpha and Beta", names)
	}
}

func TestGetImage_IncludesTagSources(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_sources_api.png", 10, 10)
	svc := tags.New(env.database, nil)
	tag := mustTag(t, svc, "cute", 1)
	if err := svc.AddTagToImage(id, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncSourceTags(id, []int64{tag.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	mustCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)
	ts, ok := resp["tag_sources"].(map[string]any)
	if !ok {
		t.Fatalf("tag_sources = %v, want object", resp["tag_sources"])
	}
	srcs, ok := ts["cute"].([]any)
	if !ok || len(srcs) != 2 {
		t.Fatalf("tag_sources[cute] = %v, want 2 sources", ts["cute"])
	}
	got := map[string]bool{}
	for _, s := range srcs {
		got[s.(string)] = true
	}
	if !got["user"] || !got["danbooru"] {
		t.Errorf("tag_sources[cute] = %v, want user and danbooru", srcs)
	}
}

func TestGetImage_InvalidID(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/images/notanumber")
	wantCode(t, w, http.StatusBadRequest)
}

func TestCreateImage_JSONPath(t *testing.T) {
	env := newTestEnv(t)

	imgPath := env.writePNG(t, "new_api.png", 15, 15)

	body, _ := json.Marshal(map[string]any{
		"path": imgPath,
		"tags": []string{"test_tag"},
	})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)

	wantCode(t, w, http.StatusCreated)
}

func TestCreateImage_JSONPath_Duplicate(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "dup_api.png", 20, 20)

	var canonPath string
	if err := env.database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"path": canonPath})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)

	mustCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["alias_added"] != false {
		t.Errorf("alias_added = %v, want false for a re-push of the canonical path", resp["alias_added"])
	}
	if resp["image"] == nil {
		t.Errorf("expected image object in response, got %v", resp)
	}
}

func TestCreateImage_JSONPath_SecondCopyAddsAnAlias(t *testing.T) {
	env := newTestEnv(t)
	first := env.writePNG(t, "copy_a.png", 15, 15)
	createImageJSON(t, env, map[string]any{"path": first}, http.StatusCreated)
	second := filepath.Join(env.galleryDir, "copy_b.png")
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, second, data, 0o644)

	resp := createImageJSON(t, env, map[string]any{"path": second}, http.StatusOK)
	if resp["alias_added"] != true {
		t.Errorf("alias_added = %v, want true for a second copy at a new path", resp["alias_added"])
	}
	resp = createImageJSON(t, env, map[string]any{"path": second}, http.StatusOK)
	if resp["alias_added"] != false {
		t.Errorf("alias_added = %v, want false for a re-push of a recorded alias", resp["alias_added"])
	}
	if n := countRows(t, env.database, "image_paths WHERE path = ?", second); n != 1 {
		t.Errorf("image_paths rows for the copy = %d, want 1", n)
	}
}

func TestCreateImage_DuplicateMergesTagsAndSource(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "merge_api.png", 20, 20)
	var canonPath string
	if err := env.database.Read.QueryRow(`SELECT canonical_path FROM images LIMIT 1`).Scan(&canonPath); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"path":   canonPath,
		"source": "danbooru",
		"url":    "https://danbooru.donmai.us/posts/1",
		"tags":   []string{"1girl", "rating:explicit"},
	})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	mustCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)
	merge, _ := resp["merge"].(map[string]any)
	if merge == nil || merge["tags_added"].(float64) < 2 {
		t.Fatalf("expected merge summary with tags_added>=2, got %v", resp["merge"])
	}

	n := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE t.name IN ('1girl', 'explicit') AND it.tagger_name = 'danbooru'")
	if n < 2 {
		t.Errorf("expected 1girl + explicit attributed to danbooru, got %d", n)
	}
	var site, srcURL string
	if err := env.database.Read.QueryRow(`SELECT site, url FROM image_sources LIMIT 1`).Scan(&site, &srcURL); err != nil {
		t.Fatal(err)
	}
	if site != "danbooru" || srcURL == "" {
		t.Errorf("expected danbooru source recorded, got site=%q url=%q", site, srcURL)
	}
}

func TestCreateImage_ADuplicateKeepsItsIndexEntry(t *testing.T) {
	env := newTestEnv(t)
	tree := env.gallery.Source().Tree
	id := env.createTestImage(t, "indexed.png", 20, 20)
	var canonPath string
	var phash sql.NullInt64
	if err := env.database.Read.QueryRow(`SELECT canonical_path, phash FROM images WHERE id = ?`, id).Scan(&canonPath, &phash); err != nil || !phash.Valid {
		t.Fatalf("seed: %v (phash set: %t)", err, phash.Valid)
	}
	if err := tree.EnsureBuilt(env.database); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"path": canonPath})
	mustCode(t, doJSON(env.mux, "POST", "/api/v1/images", body), http.StatusOK)

	if ids, _ := tree.SearchWithinDistance(phash.Int64, 0); !slices.Contains(ids, id) {
		t.Errorf("image %d left the index after a duplicate create: %v", id, ids)
	}
}

func TestCreateImage_DuplicateMergesCommentaryAndOriginal(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "comment_api.png", 20, 20)
	var id int64
	var canonPath string
	if err := env.database.Read.QueryRow(`SELECT id, canonical_path FROM images LIMIT 1`).Scan(&id, &canonPath); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"path":       canonPath,
		"source":     "danbooru",
		"url":        "https://danbooru.donmai.us/posts/1",
		"commentary": "artist said hi",
		"original":   "https://pixiv.net/artworks/1",
	})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	mustCode(t, w, http.StatusOK)
	var stored, original string
	if err := env.database.Read.QueryRow(`SELECT commentary, original FROM image_sources WHERE site = 'danbooru'`).Scan(&stored, &original); err != nil {
		t.Fatal(err)
	}
	if stored != "artist said hi" {
		t.Errorf("stored commentary = %q, want the pushed body", stored)
	}
	if original != "https://pixiv.net/artworks/1" {
		t.Errorf("stored original = %q, want the pushed one", original)
	}
	w2 := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantIn(t, w2.Body.String(), "artist said hi")
}

func TestCreateImage_DuplicateMergesAnnotations(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "ann_api.png", 20, 20)
	var id int64
	var canonPath string
	if err := env.database.Read.QueryRow(`SELECT id, canonical_path FROM images LIMIT 1`).Scan(&id, &canonPath); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"path":   canonPath,
		"source": "danbooru",
		"notes":  []map[string]any{{"x": 5, "y": 6, "w": 7, "h": 8, "body": "translated line"}},
	})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	mustCode(t, w, http.StatusOK)
	n := countRows(t, env.database, "image_annotations WHERE site = 'danbooru' AND body = 'translated line'")
	if n != 1 {
		t.Errorf("expected 1 annotation stored, got %d", n)
	}
	w2 := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantIn(t, w2.Body.String(), "translated line")
}

func TestEnrichImage_AppliesTagsSentWithoutASource(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "bare.png", 13, 13)

	body, _ := json.Marshal(map[string]any{"tags": []string{"sunset"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	var resp struct {
		Merge struct {
			TagsAdded int `json:"tags_added"`
		} `json:"merge"`
	}
	mustDecode(t, w, &resp)
	if n := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.name = 'sunset'", id); n != 1 || resp.Merge.TagsAdded != 1 {
		t.Errorf("sunset rows = %d, tags_added = %d; want the tag applied and counted", n, resp.Merge.TagsAdded)
	}
}

func TestCreateImage_DuplicatePushAppliesSourcelessTags(t *testing.T) {
	env := newTestEnv(t)
	pic := pngFileBytes(t, 14, 14)
	first := env.postUpload(t, "first.png", pic, nil)
	mustCode(t, first, http.StatusCreated)
	var created struct {
		ID int64 `json:"id"`
	}
	mustDecode(t, first, &created)

	w := env.postUpload(t, "again.png", pic, map[string]string{"tags": `["blue_sky"]`})
	mustCode(t, w, http.StatusOK)
	var resp struct {
		Merge struct {
			TagsAdded int `json:"tags_added"`
		} `json:"merge"`
	}
	mustDecode(t, w, &resp)
	if n := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.name = 'blue_sky'", created.ID); n != 1 || resp.Merge.TagsAdded != 1 {
		t.Errorf("blue_sky rows = %d, tags_added = %d; want the tag applied and counted", n, resp.Merge.TagsAdded)
	}
}

func TestEnrichImage_AppliesTagsCommentaryOriginalNotes(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"tags":                  []string{"1girl", "artist:foo"},
		"source":                "danbooru",
		"url":                   "https://danbooru.donmai.us/posts/5",
		"commentary":            "artist said hi",
		"commentary_translated": "the artist said hi",
		"original":              "https://pixiv.net/artworks/9",
		"notes":                 []map[string]any{{"x": 10, "y": 20, "w": 30, "h": 40, "body": "translated"}},
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)

	nTags := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE t.name IN ('1girl', 'foo') AND it.tagger_name = 'danbooru'")
	if nTags < 2 {
		t.Errorf("expected 1girl + foo attributed to danbooru, got %d", nTags)
	}
	var commentary, translated, original string
	readSource := func() {
		t.Helper()
		if err := env.database.Read.QueryRow(
			`SELECT commentary, commentary_translated, original FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id,
		).Scan(&commentary, &translated, &original); err != nil {
			t.Fatal(err)
		}
	}
	readSource()
	if commentary != "artist said hi" {
		t.Errorf("commentary = %q, want 'artist said hi'", commentary)
	}
	if translated != "the artist said hi" {
		t.Errorf("commentary_translated = %q, want the pushed translation", translated)
	}
	if original != "https://pixiv.net/artworks/9" {
		t.Errorf("original = %q, want the pushed one", original)
	}
	nAnn := countRows(t, env.database, "image_annotations WHERE image_id = ? AND site = 'danbooru'", id)
	if nAnn != 1 {
		t.Errorf("annotations = %d, want 1", nAnn)
	}

	body, _ = json.Marshal(map[string]any{"source": "danbooru"})
	w = doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	readSource()
	if original != "https://pixiv.net/artworks/9" {
		t.Errorf("original after original-less enrich = %q, want kept", original)
	}
	if translated != "the artist said hi" {
		t.Errorf("translation after translation-less enrich = %q, want kept", translated)
	}
	w2 := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantIn(t, w2.Body.String(), `"commentary_translated":"the artist said hi"`)
}

func TestWithheldCategoryIsNotReadAsGoneFromThePost(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "withheld.png", 12, 12)
	var path string
	if err := env.database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		field string
		value string
	}{
		{"withheld_categories", "artist"},
		{"withheld_tags", "artist:painter"},
	} {
		fields := map[string]string{"source": "danbooru", "tags": `["hat"]`, w.field: `["` + w.value + `"]`}
		for _, tc := range []struct {
			name string
			send func() *httptest.ResponseRecorder
		}{
			{"enrich", func() *httptest.ResponseRecorder {
				body, _ := json.Marshal(map[string]any{"tags": []string{"hat"}, "source": "danbooru", w.field: []string{w.value}})
				return doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
			}},
			{"duplicate push", func() *httptest.ResponseRecorder { return env.postUpload(t, "withheld.png", data, fields) }},
			{"replace", func() *httptest.ResponseRecorder { return env.postReplace(t, id, "withheld.png", data, fields) }},
		} {
			t.Run(w.field+" "+tc.name, func(t *testing.T) {
				first, _ := json.Marshal(map[string]any{"tags": []string{"hat", "artist:painter"}, "source": "danbooru"})
				mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), first), http.StatusOK)

				w := tc.send()
				if w.Code != http.StatusOK {
					t.Fatalf("got %d: %s", w.Code, w.Body.String())
				}
				var resp struct {
					Merge struct {
						TagsRetired int `json:"tags_retired"`
					} `json:"merge"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if resp.Merge.TagsRetired != 0 {
					t.Errorf("tags_retired = %d, want 0", resp.Merge.TagsRetired)
				}
				var stale int
				if err := env.database.Read.QueryRow(
					`SELECT it.stale FROM image_tags it JOIN tags t ON t.id = it.tag_id
					 WHERE it.image_id = ? AND t.name = 'painter'`, id).Scan(&stale); err != nil {
					t.Fatal(err)
				}
				if stale != 0 {
					t.Error("the withheld category's tag went stale")
				}
			})
		}
	}
}

func TestClientTagsOutliveARefetchOfTheSource(t *testing.T) {
	env := newTestEnv(t)
	push := func(fields map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, _ := w.CreateFormFile("file", "client_tags.png")
		_, _ = fw.Write(paddedPNG(t, 2048))
		for k, v := range fields {
			_ = w.WriteField(k, v)
		}
		_ = w.Close()
		req := httptest.NewRequest("POST", "/api/v1/images", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		return rec
	}
	rec := push(map[string]string{"source": "danbooru", "via": "my_script", "tags": `["hat"]`, "client_tags": `["from_alice"]`})
	mustCode(t, rec, http.StatusCreated)

	var tagger string
	var stale int
	read := func() {
		t.Helper()
		if err := env.database.Read.QueryRow(`SELECT it.tagger_name, it.stale FROM image_tags it JOIN tags t ON t.id = it.tag_id WHERE t.name = 'from_alice'`).Scan(&tagger, &stale); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if tagger != "my_script" {
		t.Errorf("client tag attributed to %q, want via", tagger)
	}

	mustCode(t, push(map[string]string{"source": "danbooru", "tags": `["hat"]`}), http.StatusOK)
	read()
	if stale != 0 {
		t.Error("a refetch of the source flagged the client's tag stale")
	}

	mustCode(t, push(map[string]string{"source": "danbooru", "via": "my_script", "tags": `["hat"]`, "client_tags": `["second"]`}), http.StatusOK)
	var n int
	if err := env.database.Read.QueryRow(`SELECT COUNT(*) FROM image_tags it JOIN tags t ON t.id = it.tag_id WHERE t.name = 'second' AND it.tagger_name = 'my_script'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("client tag on a duplicate push: %d rows (err %v), want 1", n, err)
	}
}

func TestWithheldTagIsNeverCreated(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "withheld_new.png", 12, 12)
	body, _ := json.Marshal(map[string]any{"tags": []string{"hat"}, "source": "danbooru", "withheld_tags": []string{"artist:never_seen", ":"}})
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body), http.StatusOK)
	var n int
	if err := env.database.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE name = 'never_seen'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a withheld tag was created")
	}
}

func TestEnrichImage_FlagsDroppedSourceTagsStale(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_stale.png", 12, 12)

	enrich := func(tags []string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"tags": tags, "source": "danbooru"})
		w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
		mustCode(t, w, http.StatusOK)
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp
	}
	staleOf := func(name string) int {
		t.Helper()
		var s int
		if err := env.database.Read.QueryRow(
			`SELECT it.stale FROM image_tags it JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND t.name = ?`, id, name).Scan(&s); err != nil {
			t.Fatalf("tag %s: %v", name, err)
		}
		return s
	}

	enrich([]string{"1girl", "hat"})
	resp := enrich([]string{"1girl"})
	merge, _ := resp["merge"].(map[string]any)
	if merge == nil || merge["tags_retired"].(float64) != 1 {
		t.Fatalf("expected tags_retired=1, got %v", resp["merge"])
	}
	if staleOf("hat") != 1 || staleOf("1girl") != 0 {
		t.Errorf("stale = hat:%d 1girl:%d, want 1 and 0", staleOf("hat"), staleOf("1girl"))
	}

	enrich([]string{"1girl", "hat"})
	if staleOf("hat") != 0 {
		t.Error("hat should shed its stale flag once the source lists it again")
	}
}

func TestEnrichImage_ParentURLLinksDerivative(t *testing.T) {
	env := newTestEnv(t)
	parent := env.createTestImage(t, "parent.png", 12, 12)
	child := env.createTestImage(t, "child.png", 14, 14)

	env.enrichOK(t, parent, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/1"})
	env.enrichOK(t, child, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/2",
		"parent_url": "https://danbooru.donmai.us/posts/1"})

	var parentURL string
	if err := env.database.Read.QueryRow(
		`SELECT parent_url FROM image_sources WHERE image_id = ?`, child).Scan(&parentURL); err != nil {
		t.Fatal(err)
	}
	if parentURL != "https://danbooru.donmai.us/posts/1" {
		t.Errorf("parent_url = %q, want the declared parent", parentURL)
	}
	n := countRows(t, env.database, "derivative_edges WHERE source_image_id = ? AND derivative_image_id = ?", parent, child)
	if n != 1 {
		t.Errorf("derivative edges parent->child = %d, want 1", n)
	}

	env.enrichOK(t, child, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/2",
		"parent_url": "https://danbooru.donmai.us/posts/1"})
}

func TestEnrichImage_ParentArrivingLateLinksChildren(t *testing.T) {
	env := newTestEnv(t)
	child := env.createTestImage(t, "late_child.png", 12, 12)
	parent := env.createTestImage(t, "late_parent.png", 14, 14)

	env.enrichOK(t, child, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/20",
		"parent_url": "https://danbooru.donmai.us/posts/10"})
	n := countRows(t, env.database, "derivative_edges")
	if n != 0 {
		t.Fatalf("derivative edges before the parent landed = %d, want 0", n)
	}

	env.enrichOK(t, parent, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/10"})
	if err := env.database.Read.QueryRow(
		`SELECT COUNT(*) FROM derivative_edges WHERE source_image_id = ? AND derivative_image_id = ?`,
		parent, child).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("derivative edges parent->child = %d, want 1", n)
	}
}

func TestEnrichImage_ParentLinkLeavesADeclaredSourceAlone(t *testing.T) {
	env := newTestEnv(t)
	declared := env.createTestImage(t, "declared_source.png", 12, 12)
	parent := env.createTestImage(t, "booru_parent.png", 14, 14)
	child := env.createTestImage(t, "sourced_child.png", 16, 16)
	if err := relations.New(env.database, tags.New(env.database, nil), nil).AddDerivativeEdge(declared, child); err != nil {
		t.Fatalf("seed the operator's source: %v", err)
	}

	env.enrichOK(t, parent, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/31"})
	env.enrichOK(t, child, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/32",
		"parent_url": "https://danbooru.donmai.us/posts/31"})

	got, err := db.QueryIDs(env.database.Read,
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != declared {
		t.Fatalf("sources = %v, want just the declared %d", got, declared)
	}
}

func TestEnrichImage_ParentLinkSkipsConflictedPair(t *testing.T) {
	env := newTestEnv(t)
	parent := env.createTestImage(t, "conflict_parent.png", 12, 12)
	child := env.createTestImage(t, "conflict_child.png", 14, 14)

	if err := relations.New(env.database, tags.New(env.database, nil), nil).AddNotRelated(parent, child); err != nil {
		t.Fatal(err)
	}
	env.enrichOK(t, child, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/32",
		"parent_url": "https://danbooru.donmai.us/posts/31"})
	env.enrichOK(t, parent, map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/31"})
	n := countRows(t, env.database, "derivative_edges")
	if n != 0 {
		t.Errorf("derivative edges on a not-related pair = %d, want 0", n)
	}
}

func TestEnrichImage_PersistsSourceMD5(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_md5.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"source":     "danbooru",
		"url":        "https://d/1",
		"source_md5": "0123456789abcdef0123456789abcdef",
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	var md5 string
	if err := env.database.Read.QueryRow(
		`SELECT md5 FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&md5); err != nil {
		t.Fatal(err)
	}
	if md5 != "0123456789abcdef0123456789abcdef" {
		t.Errorf("stored md5 = %q, want the claimed one", md5)
	}

	body, _ = json.Marshal(map[string]any{"source": "danbooru"})
	w = doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	if err := env.database.Read.QueryRow(
		`SELECT md5 FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&md5); err != nil {
		t.Fatal(err)
	}
	if md5 != "0123456789abcdef0123456789abcdef" {
		t.Errorf("md5 after md5-less enrich = %q, want kept", md5)
	}
}

func TestFetchSummaryTaglessSourceOnly(t *testing.T) {
	if got := fetchSummary(gallery.MergeSummary{SourceAdded: true}, 0); got != "Recorded the source; no tags were fetched." {
		t.Errorf("tagless summary = %q", got)
	}
	if got := fetchSummary(gallery.MergeSummary{SourceAdded: true}, 2); got != "Fetched tags from the source." {
		t.Errorf("unchanged-tags summary = %q", got)
	}
}

func TestEnrichImage_URLLessUnverifiedApplies(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_ptr.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"tags":   []string{"1girl"},
		"source": "ptr",
		"url":    "",
		"verify": false,
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	nTags := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.name = '1girl' AND it.tagger_name = 'ptr'", id)
	if nTags != 1 {
		t.Errorf("1girl attributed to ptr = %d rows, want 1", nTags)
	}
	var srcURL string
	if err := env.database.Read.QueryRow(
		`SELECT url FROM image_sources WHERE image_id = ? AND site = 'ptr'`, id).Scan(&srcURL); err != nil {
		t.Fatal(err)
	}
	if srcURL != "" {
		t.Errorf("ptr origin url = %q, want empty", srcURL)
	}
}

func TestEnrichImage_BooruOutranksPTRPrimary(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_both.png", 12, 12)

	for _, payload := range []map[string]any{
		{"tags": []string{"1girl"}, "source": "ptr", "url": "", "verify": false},
		{"tags": []string{"1girl"}, "source": "danbooru", "url": "https://d/9", "verify": false},
	} {
		body, _ := json.Marshal(payload)
		w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
		mustCode(t, w, http.StatusOK)
	}

	var primary, primaryURL string
	if err := env.database.Read.QueryRow(
		`SELECT source, url FROM images WHERE id = ?`, id).Scan(&primary, &primaryURL); err != nil {
		t.Fatal(err)
	}
	if primary != "danbooru" || primaryURL != "https://d/9" {
		t.Errorf("primary mirror = (%q, %q), want the booru origin", primary, primaryURL)
	}
	n := countRows(t, env.database, "image_sources WHERE image_id = ?", id)
	if n != 2 {
		t.Errorf("origins recorded = %d, want both kept", n)
	}
}

func TestEnrichImage_SimilarityMatchSkipsHashCheck(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_sim.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"source": "sankaku", "url": "https://s/7", "similarity": 91.4,
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	var score float64
	if err := env.database.Read.QueryRow(
		`SELECT similarity FROM image_sources WHERE image_id = ? AND site = 'sankaku'`, id).Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score != 91.4 {
		t.Errorf("stored similarity = %v, want 91.4", score)
	}
	w = env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantIn(t, w.Body.String(), `"similarity":91.4`)

	body, _ = json.Marshal(map[string]any{
		"tags": []string{"1girl"}, "source": "sankaku", "url": "https://s/7",
		"source_md5": strings.Repeat("f", 32), "verify": true,
	})
	w = doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"verified":false`)
	if err := env.database.Read.QueryRow(
		`SELECT similarity FROM image_sources WHERE image_id = ? AND site = 'sankaku'`, id).Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score != 91.4 {
		t.Errorf("similarity after score-less refetch = %v, want kept", score)
	}

	body, _ = json.Marshal(map[string]any{
		"tags": []string{"1girl"}, "source": "danbooru", "url": "https://d/1",
		"source_md5": strings.Repeat("f", 32), "verify": true,
	})
	w = doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusConflict)
}

func TestEnrichImage_ReportsTagWarnings(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_warn.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"tags":   []string{"fine_tag", "rating:bogus"},
		"source": "danbooru",
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
	var resp struct {
		TagWarnings []string `json:"tag_warnings"`
	}
	mustDecode(t, w, &resp)
	if len(resp.TagWarnings) != 1 || !strings.Contains(resp.TagWarnings[0], "rating:bogus") {
		t.Errorf("tag_warnings = %v, want one entry for rating:bogus", resp.TagWarnings)
	}
}

func TestEnrichImage_HashMismatchMakesNoChanges(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_verify.png", 12, 12)
	body, _ := json.Marshal(map[string]any{
		"tags":       []string{"nope"},
		"source":     "danbooru",
		"source_md5": "00000000000000000000000000000000",
		"verify":     true,
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusConflict)
	n := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE t.name = 'nope'")
	if n != 0 {
		t.Errorf("a hash mismatch must not apply tags, got %d", n)
	}
}

func TestEnrichImage_VerifyTrimsTheSourceHash(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_trim.png", 12, 12)
	var path string
	if err := env.database.Read.QueryRow(
		`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	sum, err := gallery.Md5File(path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"tags":       []string{"trimmed"},
		"source":     "danbooru",
		"source_md5": " " + sum + "\n",
		"verify":     true,
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)
}

func TestEnrichImage_SecondPostSameSiteKeepsBothOrigins(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_twoposts.png", 12, 12)

	post := func(postID, url string, tags []string) {
		env.enrichOK(t, id, map[string]any{"tags": tags, "source": "danbooru", "post_id": postID, "url": url, "verify": false})
	}
	post("114", "https://danbooru.donmai.us/posts/114", []string{"alpha", "beta"})
	post("777", "https://danbooru.donmai.us/posts/777", []string{"gamma"})

	rows := countRows(t, env.database, "image_sources WHERE image_id = ? AND site = 'danbooru'", id)
	if rows != 2 {
		t.Fatalf("danbooru origin rows = %d, want 2", rows)
	}
	var firstURL string
	if err := env.database.Read.QueryRow(
		`SELECT url FROM image_sources WHERE image_id = ? AND site = 'danbooru' AND post_id = '114'`, id).Scan(&firstURL); err != nil {
		t.Fatal(err)
	}
	if firstURL != "https://danbooru.donmai.us/posts/114" {
		t.Errorf("first origin url = %q, want the post 114 url kept", firstURL)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		n := countRows(t, env.database, "image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.name = ?", id, name)
		if n != 1 {
			t.Errorf("tag %s rows = %d, want 1 (second post's merge must not prune the first's)", name, n)
		}
	}
}

func TestEnrichImage_RejectsInvalidURL(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_badurl.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"tags": []string{}, "source": "evil", "url": "javascript:alert(1)", "verify": false,
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusBadRequest)
	n := countRows(t, env.database, "image_sources WHERE image_id = ?", id)
	if n != 0 {
		t.Errorf("origin rows = %d, want none recorded", n)
	}
}

func TestEnrichImage_AdoptsLegacyOriginByURL(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_adopt.png", 12, 12)

	for _, postID := range []string{"", "114"} {
		env.enrichOK(t, id, map[string]any{
			"tags": []string{"alpha"}, "source": "danbooru", "post_id": postID,
			"url": "https://danbooru.donmai.us/posts/114", "verify": false,
		})
	}

	rows := countRows(t, env.database, "image_sources WHERE image_id = ? AND site = 'danbooru'", id)
	if rows != 1 {
		t.Fatalf("danbooru origin rows = %d, want the legacy row adopted, not twinned", rows)
	}
	var postID string
	if err := env.database.Read.QueryRow(
		`SELECT post_id FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&postID); err != nil {
		t.Fatal(err)
	}
	if postID != "114" {
		t.Errorf("adopted post_id = %q, want 114", postID)
	}
}

func TestFetchStatusReport_RecordsOutcome(t *testing.T) {
	var gotID int64
	var gotState, gotMsg string
	mux := http.NewServeMux()
	newBareHandler(t, nil, func(cfg *config.Config, _ *library.Gallery) {
		cfg.Auth.Tokens = []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash(testAPIToken), Scopes: config.AllScopes}}
	}).WithFetchStatus(func(_ string, id int64, state, msg string) { gotID, gotState, gotMsg = id, state, msg }).Mount(mux)

	body, _ := json.Marshal(map[string]any{"state": "unsupported_url", "message": "no extractor"})
	req := httptest.NewRequest("POST", "/api/v1/images/42/fetch-status", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	mustCode(t, w, http.StatusOK)
	if gotID != 42 || gotState != "unsupported_url" || gotMsg != "no extractor" {
		t.Errorf("recorded (%d,%q,%q), want (42,unsupported_url,no extractor)", gotID, gotState, gotMsg)
	}

	body, _ = json.Marshal(map[string]any{"message": "x"})
	req = httptest.NewRequest("POST", "/api/v1/images/42/fetch-status", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	wantCode(t, w, http.StatusBadRequest)
}

func TestFetchStatusConcludesOnlyTheBackendItNames(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "fetch_status_ptr.png", 10, 10)
	both := env.createTestImage(t, "fetch_status_all.png", 11, 11)
	now := time.Now()
	for _, img := range []int64{id, both} {
		if err := lookup.Enqueued(env.database, img, []string{contract.LookupBooru}, 11, now); err != nil {
			t.Fatal(err)
		}
		if err := lookup.Enqueued(env.database, img, []string{contract.LookupPTR}, 22, now); err != nil {
			t.Fatal(err)
		}
	}
	report := func(img int64, backend string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"state": "hash_not_found", "backend": backend})
		mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/fetch-status", img), body), http.StatusOK)
	}

	report(id, contract.LookupPTR)
	rows, _ := lookup.ForImage(env.database, id)
	if ptr := rows[contract.LookupPTR]; ptr.LastResult != lookup.ResultMiss || !ptr.QueuedAt.IsZero() {
		t.Errorf("ptr row = %+v, want the miss recorded", ptr)
	}
	if booru := rows[contract.LookupBooru]; booru.QueuedAt.IsZero() || booru.LastResult != "" {
		t.Errorf("booru row = %+v, want it still in flight", booru)
	}
	body, _ := json.Marshal(map[string]any{"source": "danbooru", "url": "https://danbooru.donmai.us/posts/1"})
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body), http.StatusOK)
	rows, _ = lookup.ForImage(env.database, id)
	if booru := rows[contract.LookupBooru]; booru.LastResult != lookup.ResultHit {
		t.Errorf("booru row = %+v, want the later hit recorded", booru)
	}

	report(both, "all")
	rows, _ = lookup.ForImage(env.database, both)
	for _, b := range []string{contract.LookupPTR, contract.LookupBooru} {
		if rows[b].LastResult != lookup.ResultMiss {
			t.Errorf("%s row = %+v, want an all job's miss on both", b, rows[b])
		}
	}
}

func TestFetchStatusWithoutABackendConcludesEveryInFlightRow(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "fetch_status_any.png", 10, 10)
	if err := lookup.Enqueued(env.database, id, []string{contract.LookupBooru, contract.LookupPTR}, 11, time.Now()); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"state": "hash_not_found"})
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/fetch-status", id), body), http.StatusOK)
	rows, _ := lookup.ForImage(env.database, id)
	for _, b := range []string{contract.LookupPTR, contract.LookupBooru} {
		if rows[b].LastResult != lookup.ResultMiss || !rows[b].QueuedAt.IsZero() {
			t.Errorf("%s row = %+v, want the miss recorded", b, rows[b])
		}
	}
}

func TestFetchStatusOfAJobOtherThanALookupConcludesNoLookup(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "fetch_status_kind.png", 10, 10)
	if err := lookup.Enqueued(env.database, id, []string{contract.LookupBooru, contract.LookupPTR}, 11, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"metadata", "replace"} {
		body, _ := json.Marshal(map[string]any{"state": "auth_required", "kind": kind})
		mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/fetch-status", id), body), http.StatusOK)
	}
	rows, _ := lookup.ForImage(env.database, id)
	for _, b := range []string{contract.LookupPTR, contract.LookupBooru} {
		if rows[b].QueuedAt.IsZero() || rows[b].LastResult != "" {
			t.Errorf("%s row = %+v, want it still in flight", b, rows[b])
		}
	}

	body, _ := json.Marshal(map[string]any{"state": "hash_not_found", "kind": "lookup", "backend": contract.LookupBooru})
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/fetch-status", id), body), http.StatusOK)
	rows, _ = lookup.ForImage(env.database, id)
	if booru := rows[contract.LookupBooru]; booru.LastResult != lookup.ResultMiss {
		t.Errorf("booru row = %+v, want the lookup's miss recorded", booru)
	}
}

func TestCreateImage_MissingPath(t *testing.T) {
	env := newTestEnv(t)
	body, _ := json.Marshal(map[string]any{"path": ""})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	wantCode(t, w, http.StatusBadRequest)
}

func TestCreateImage_JSONPath_OutsideGalleryRejected(t *testing.T) {
	env := newTestEnv(t)

	tmpDir := t.TempDir()
	imgPath := filepath.Join(tmpDir, "outside.png")
	mustWriteFile(t, imgPath, pngFileBytes(t, 8, 8), 0o644)

	body, _ := json.Marshal(map[string]any{"path": imgPath})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)

	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "inside the gallery")
}

func TestCreateImage_RefusesAFolderAnotherGalleryOwns(t *testing.T) {
	env := newTestEnv(t)
	inner := filepath.Join(env.galleryDir, ".H")
	mustMkdirAll(t, inner, 0o755)
	env.bounds.Store(gallery.NewBoundary(env.galleryDir, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))
	imgPath := filepath.Join(inner, "theirs.png")
	mustWriteFile(t, imgPath, pngFileBytes(t, 8, 8), 0o644)

	body, _ := json.Marshal(map[string]any{"path": imgPath})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "belongs to gallery H")

	body, _ = json.Marshal(map[string]any{"path": "theirs.png", "folder": ".H"})
	w = doJSON(env.mux, "POST", "/api/v1/images", body)
	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "belongs to gallery H")
}

func TestCreateImage_JSONPath_NonExistentReturns400(t *testing.T) {
	env := newTestEnv(t)
	ghost := filepath.Join(env.galleryDir, "does", "not", "exist.png")

	body, _ := json.Marshal(map[string]any{"path": ghost})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)

	wantCode(t, w, http.StatusBadRequest)
	wantNotIn(t, w.Body.String(), ghost)
}

func TestCreateImage_RejectsNonImageContent(t *testing.T) {
	env := newTestEnv(t)
	fakePath := filepath.Join(env.galleryDir, "fake.png")
	mustWriteFile(t, fakePath, []byte("not an image\n"), 0o644)

	body, _ := json.Marshal(map[string]any{"path": fakePath})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)

	wantCode(t, w, http.StatusUnsupportedMediaType)
}

func TestCreateImage_RefusesAnImageWithNoPixels(t *testing.T) {
	env := newTestEnv(t)
	for _, size := range []image.Rectangle{image.Rect(0, 0, 0, 0), image.Rect(0, 0, 10, 0)} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, image.NewRGBA(size), nil); err != nil {
			t.Fatal(err)
		}
		w := env.postUpload(t, "empty.jpg", buf.Bytes(), nil)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%v: %d %s, want 415", size, w.Code, w.Body.String())
		}
	}
}

func TestCreateImage_InvalidJSON(t *testing.T) {
	env := newTestEnv(t)
	w := doJSON(env.mux, "POST", "/api/v1/images", []byte("not json"))
	wantCode(t, w, http.StatusBadRequest)
}

func TestDeleteImage(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "del_test.png", 10, 10)

	w := env.do("DELETE", fmt.Sprintf("/api/v1/images/%d", id))

	wantCode(t, w, http.StatusNoContent)

	w2 := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantCode(t, w2, http.StatusNotFound)
}

func TestDeleteImage_SaysHowManyChangedFilesItKept(t *testing.T) {
	env := newTestEnv(t)
	imgPath := env.writePNG(t, "rewritten.png", 13, 13)
	record, _ := mustIngest(t, env.database, env.galleryDir, env.thumbDir, imgPath, "")
	body, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, imgPath, append(body, "written over since"...), 0o644)

	w := env.do("DELETE", fmt.Sprintf("/api/v1/images/%d", record.ID))

	mustCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["kept_changed_files"] != float64(1) {
		t.Errorf("kept_changed_files = %v, want 1", resp["kept_changed_files"])
	}
	if _, err := os.Stat(imgPath); err != nil {
		t.Errorf("the file written over since was removed: %v", err)
	}
}

func TestDeleteImage_NotFound(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("DELETE", "/api/v1/images/99999")
	wantCode(t, w, http.StatusNotFound)
}

func TestDeleteImage_InvalidID(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("DELETE", "/api/v1/images/bad")
	wantCode(t, w, http.StatusBadRequest)
}

func TestAddImageTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_add_test.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"red", "blue"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)

	wantCode(t, w, http.StatusOK)
	var tags []any
	mustDecode(t, w, &tags)
	if len(tags) < 2 {
		t.Errorf("expected >= 2 tags in response, got %d", len(tags))
	}
}

func TestCreateImage_JSONOriginRoundTrip(t *testing.T) {
	env := newTestEnv(t)

	path := env.writePNG(t, "ext_source.png", 8, 8)

	body, _ := json.Marshal(map[string]any{
		"path": path,
		"via":  "https://danbooru/12345",
	})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	mustCode(t, w, http.StatusCreated)
	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["origin"] != "https://danbooru/12345" {
		t.Errorf("origin = %v, want %q", resp["origin"], "https://danbooru/12345")
	}

	id := int64(resp["id"].(float64))
	gw := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	mustCode(t, gw, http.StatusOK)
	var got map[string]any
	if err := json.NewDecoder(gw.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["origin"] != "https://danbooru/12345" {
		t.Errorf("origin on GET = %v, want %q", got["origin"], "https://danbooru/12345")
	}
}

func TestAddImageTags_RejectsViaWithSelectorBreakers(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "via_bad.png", 10, 10)

	for _, bad := range []string{"foo bar", `with"quote`, "with]bracket"} {
		body, _ := json.Marshal(map[string]any{"tags": []string{"one"}, "via": bad})
		w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("via=%q expected 400, got %d: %s", bad, w.Code, w.Body.String())
		}
	}
}

func TestAddImageTags_AcceptsURLAndIdentifierVia(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "via_good.png", 10, 10)

	for _, ok := range []string{"my_app_v1.2", "https://danbooru/12345", "app-name", "user@host"} {
		body, _ := json.Marshal(map[string]any{"tags": []string{"tag_for_" + strings.ReplaceAll(ok, ":", "_")}, "via": ok})
		w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
		if w.Code != http.StatusOK {
			t.Errorf("via=%q expected 200, got %d: %s", ok, w.Code, w.Body.String())
		}
	}
}

func TestListImageTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_get.png", 10, 10)
	addBody, _ := json.Marshal(map[string]any{"tags": []string{"red", "blue"}})
	addW := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), addBody)
	mustCode(t, addW, http.StatusOK)

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/tags", id))
	mustCode(t, w, http.StatusOK)
	var tags []any
	mustDecode(t, w, &tags)
	if len(tags) < 2 {
		t.Errorf("got %d tags, want >= 2", len(tags))
	}
}

func TestListImageTags_NotFound(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/images/99999/tags")
	wantCode(t, w, http.StatusNotFound)
}

func TestAddImageTags_RejectsMissingTagsField(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_wrong_field.png", 10, 10)

	body := []byte(`{"add":["would_be_added"]}`)
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)

	wantCode(t, w, http.StatusBadRequest)
}

func TestRemoveImageTags_RejectsMissingTagsField(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_wrong_delete.png", 10, 10)

	body := []byte(`{"remove":["existing_tag"]}`)
	w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), body)

	wantCode(t, w, http.StatusBadRequest)
}

func TestAddImageTags_CarriesSource(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_source.png", 10, 10)

	body, _ := json.Marshal(map[string]any{
		"tags": []string{"from_app"},
		"via":  "my_app",
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	mustCode(t, w, http.StatusOK)

	var taggerName *string
	var isAuto int
	err := env.database.Read.QueryRow(`
		SELECT it.is_auto, it.tagger_name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id = ? AND t.name = 'from_app'`, id).Scan(&isAuto, &taggerName)
	if err != nil {
		t.Fatalf("scan image_tags: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0", isAuto)
	}
	if taggerName == nil || *taggerName != "my_app" {
		t.Errorf("tagger_name = %v, want %q", taggerName, "my_app")
	}
}

func TestAddImageTags_DefaultsSourceToAPI(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_no_via.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"no_via_tag"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	mustCode(t, w, http.StatusOK)

	var taggerName *string
	if err := env.database.Read.QueryRow(`
		SELECT it.tagger_name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id = ? AND t.name = 'no_via_tag'`, id).Scan(&taggerName); err != nil {
		t.Fatalf("scan image_tags: %v", err)
	}
	if taggerName == nil || *taggerName != "api" {
		t.Errorf("tagger_name = %v, want %q", taggerName, "api")
	}
}

func TestAddImageTags_InvalidID(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"red"}})
	w := doJSON(mux, "POST", "/api/v1/images/bad/tags", body)
	wantCode(t, w, http.StatusBadRequest)
}

func TestAddImageTags_InvalidBody(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_add_bad.png", 10, 10)
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), []byte("bad json"))
	wantCode(t, w, http.StatusBadRequest)
}

func TestAddImageTags_ImageNotFound(t *testing.T) {
	env := newTestEnv(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"never_added_red"}})
	w := doJSON(env.mux, "POST", "/api/v1/images/99999/tags", body)
	wantCode(t, w, http.StatusNotFound)
	n := countRows(t, env.database, "tags WHERE name = ?", "never_added_red")
	if n != 0 {
		t.Errorf("addImageTags on missing id created %d stray tag row(s)", n)
	}
}

func TestRemoveImageTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_rem_test.png", 10, 10)

	addBody, _ := json.Marshal(map[string]any{"tags": []string{"to_remove"}})
	doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), addBody)

	remBody, _ := json.Marshal(map[string]any{"tags": []string{"to_remove"}})
	w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), remBody)

	wantCode(t, w, http.StatusOK)
}

func TestRemoveImageTags_ParentWithItsImpliedChild(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "parent_child_rm.png", 10, 10)

	genID := categoryID(t, env, "general")
	svc := tags.New(env.database, nil)
	parent, _ := svc.GetOrCreateTag("rm_parent", genID)
	child, _ := svc.GetOrCreateTag("rm_child", genID)
	if _, err := svc.AddImplication(parent.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(id, parent.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	remBody, _ := json.Marshal(map[string]any{"tags": []string{"rm_child", "rm_parent"}})
	w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), remBody)
	mustCode(t, w, http.StatusOK)
	for _, name := range []string{"rm_parent", "rm_child"} {
		if n, _ := findImageTag(t, env, id, name); n != "" {
			t.Errorf("%s is still on the image", name)
		}
	}
}

func TestRemoveImageTags_InvalidID(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"red"}})
	w := doJSON(mux, "DELETE", "/api/v1/images/bad/tags", body)
	wantCode(t, w, http.StatusBadRequest)
}

func TestRemoveImageTags_InvalidBody(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "rem_bad.png", 10, 10)
	w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), []byte("not json"))
	wantCode(t, w, http.StatusBadRequest)
}

func TestRemoveImageTags_ImageNotFound(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"x"}})
	w := doJSON(mux, "DELETE", "/api/v1/images/99999/tags", body)
	wantCode(t, w, http.StatusNotFound)
}

func findImageTag(t *testing.T, env *testEnv, id int64, want string) (string, string) {
	t.Helper()
	var n, c string
	err := env.database.Read.QueryRow(
		`SELECT t.name, tc.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.name = ?`, id, want).Scan(&n, &c)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("query image tags: %v", err)
	}
	return n, c
}

func TestAddImageTags_ColonFallbackLiteral(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_fallback.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"nier:automata", ":3"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	mustCode(t, w, http.StatusOK)

	for _, name := range []string{"nier:automata", ":3"} {
		gotName, gotCat := findImageTag(t, env, id, name)
		if gotName != name {
			t.Errorf("tag %q not stored on image", name)
		}
		if gotCat != "general" {
			t.Errorf("tag %q category = %q, want general", name, gotCat)
		}
	}
}

func TestAddImageTags_CategoryPrefixStillSplits(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_split.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"artist:john_doe"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	mustCode(t, w, http.StatusOK)

	gotName, gotCat := findImageTag(t, env, id, "john_doe")
	if gotName != "john_doe" {
		t.Error("tag john_doe not stored on image")
	}
	if gotCat != "artist" {
		t.Errorf("category = %q, want artist", gotCat)
	}

	if n, _ := findImageTag(t, env, id, "artist:john_doe"); n != "" {
		t.Error("literal artist:john_doe must not be stored when artist is a real category")
	}
}

func TestRemoveImageTags_ColonFallbackLiteral(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_rm.png", 10, 10)

	addBody, _ := json.Marshal(map[string]any{"tags": []string{"nier:automata"}})
	doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), addBody)

	remBody, _ := json.Marshal(map[string]any{"tags": []string{"nier:automata"}})
	w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), remBody)
	mustCode(t, w, http.StatusOK)
	if n, _ := findImageTag(t, env, id, "nier:automata"); n != "" {
		t.Error("nier:automata should have been removed from the image")
	}
}

func TestRemoveImageTags_CategoryMissFallsThroughToLiteral(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_collide.png", 10, 10)

	genID := categoryID(t, env, "general")
	svc := tags.New(env.database, nil)
	tag, err := svc.GetOrCreateTag("artist:foo", genID)
	if err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := svc.AddTagToImage(id, tag.ID, false, nil); err != nil {
		t.Fatalf("attach tag: %v", err)
	}

	remBody, _ := json.Marshal(map[string]any{"tags": []string{"artist:foo"}})
	w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), remBody)
	mustCode(t, w, http.StatusOK)
	if n, _ := findImageTag(t, env, id, "artist:foo"); n != "" {
		t.Error("artist:foo should have been removed from the image after category-miss fall-through")
	}
}

func TestSearchImages_WithSort(t *testing.T) {
	env := newTestEnv(t)

	env.createTestImage(t, "sort1.png", 10, 10)
	env.createTestImage(t, "sort2.png", 30, 30)

	get := func(t *testing.T, sort, order string) []any {
		t.Helper()
		u := "/api/v1/images/search?sort=" + sort
		if order != "" {
			u += "&order=" + order
		}
		w := env.do("GET", u)
		if w.Code != http.StatusOK {
			t.Fatalf("sort=%s order=%s: expected 200, got %d: %s", sort, order, w.Code, w.Body.String())
		}
		var resp map[string]any
		mustDecode(t, w, &resp)
		r, _ := resp["results"].([]any)
		return r
	}

	t.Run("newest desc puts second upload first", func(t *testing.T) {
		got := get(t, "newest", "desc")
		if len(got) != 2 {
			t.Fatalf("expected 2 results, got %d", len(got))
		}
		first := got[0].(map[string]any)["canonical_path"].(string)
		if !strings.HasSuffix(first, "sort2.png") {
			t.Errorf("newest desc first = %q, want …/sort2.png", first)
		}
	})
	t.Run("filesize desc puts larger file first", func(t *testing.T) {
		got := get(t, "filesize", "desc")
		first := got[0].(map[string]any)["canonical_path"].(string)
		if !strings.HasSuffix(first, "sort2.png") {
			t.Errorf("filesize desc first = %q, want …/sort2.png", first)
		}
	})
	t.Run("random returns a 200 with both results", func(t *testing.T) {
		got := get(t, "random", "")
		if len(got) != 2 {
			t.Errorf("random expected 2 results, got %d", len(got))
		}
	})
}

func TestSearchImages_SimilaritySortRanksByOverlap(t *testing.T) {
	env := newTestEnv(t)
	tag := func(id int64, names ...string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"tags": names})
		mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body), http.StatusOK)
	}

	for i := range 20 {
		env.createTestImage(t, fmt.Sprintf("sim_filler_%02d.png", i), 20+i, 20+i)
	}
	seed := env.createTestImage(t, "sim_seed.png", 10, 10)
	near := env.createTestImage(t, "sim_near.png", 11, 11)
	far := env.createTestImage(t, "sim_far.png", 12, 12)
	tag(seed, "plaid_scarf", "ice_wings")
	tag(near, "plaid_scarf", "ice_wings")
	tag(far, "plaid_scarf", "knee_boots")

	firstID := func(sort string) int64 {
		t.Helper()
		w := env.do("GET", fmt.Sprintf("/api/v1/images/search?q=similar:%d&sort=%s", seed, sort))
		mustCode(t, w, http.StatusOK)
		var resp struct {
			Results []struct {
				ID int64 `json:"id"`
			} `json:"results"`
		}
		mustDecode(t, w, &resp)
		if len(resp.Results) != 2 {
			t.Fatalf("sort=%s: got %d results, want the two sharers", sort, len(resp.Results))
		}
		return resp.Results[0].ID
	}
	if got := firstID("newest"); got != far {
		t.Fatalf("newest first = %d, want the later upload %d", got, far)
	}
	if got := firstID("similarity"); got != near {
		t.Errorf("similarity first = %d, want the two-tag sharer %d", got, near)
	}
}

func TestSearchImages_RandomSeedStable(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 8; i++ {
		env.createTestImage(t, fmt.Sprintf("seed%d.png", i), 10+i, 10+i)
	}
	idsFor := func(t *testing.T) []float64 {
		t.Helper()
		w := env.do("GET", "/api/v1/images/search?sort=random&seed=42&limit=8")
		mustCode(t, w, http.StatusOK)
		var resp map[string]any
		mustDecode(t, w, &resp)
		raw, _ := resp["results"].([]any)
		out := make([]float64, len(raw))
		for i, item := range raw {
			out[i] = item.(map[string]any)["id"].(float64)
		}
		return out
	}
	first := idsFor(t)
	for i := 0; i < 2; i++ {
		again := idsFor(t)
		if len(again) != len(first) {
			t.Fatalf("run %d returned %d results, want %d", i+1, len(again), len(first))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("run %d order diverged at %d: got %v want %v", i+1, j, again, first)
			}
		}
	}
}

func TestSearchImages_WithPagination(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "pag1.png", 10, 10)

	w := env.do("GET", "/api/v1/images/search?page=1&limit=10")
	wantCode(t, w, http.StatusOK)
}

func TestSearchImages_LimitCapped(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/images/search?limit=9999")
	mustCode(t, w, http.StatusOK)
	var resp map[string]any
	mustDecode(t, w, &resp)
	raw, ok := resp["limit"]
	if !ok {
		t.Fatal("response missing 'limit'")
	}
	limit, ok := raw.(float64)
	if !ok {
		t.Fatalf("limit had type %T, want float64", raw)
	}
	if limit > 200 {
		t.Errorf("limit = %v, want <= 200 (spec §8.3 API max)", limit)
	}
}

func TestParsePage(t *testing.T) {
	for _, tc := range []struct {
		name, query         string
		maxLimit            int
		wantOffset, wantLim int
	}{
		{"page and limit", "?page=3&limit=20", 200, 40, 20},
		{"defaults", "", 200, 0, 40},
		{"a default over the cap is held to it", "", 30, 0, 30},
		{"limit capped", "?limit=9999", 100, 0, 100},
		{"page_size alias", "?page=2&page_size=50", 200, 50, 50},
		{"limit wins over page_size", "?limit=20&page_size=50", 200, 0, 20},
		{"invalid values fall back to the defaults", "?page=bad&limit=also_bad", 200, 0, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offset, limit := parsePage(httptest.NewRequest("GET", "/"+tc.query, nil), 40, tc.maxLimit)
			if offset != tc.wantOffset {
				t.Errorf("offset = %d, want %d", offset, tc.wantOffset)
			}
			if limit != tc.wantLim {
				t.Errorf("limit = %d, want %d", limit, tc.wantLim)
			}
		})
	}
}

func TestCORSRefusesBeforeTheTokenCheck(t *testing.T) {
	mux := newBareEnv(t, nil, func(cfg *config.Config, _ *library.Gallery) {
		cfg.Server.BaseURL = "https://myapp.example.com"
	})
	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	wantCode(t, w, http.StatusForbidden)
	wantIn(t, w.Body.String(), `"forbidden"`)
}

func corsRequest(t *testing.T, base, method, target, origin string, allowed ...string) *httptest.ResponseRecorder {
	t.Helper()
	mux := newBareEnv(t, nil, func(cfg *config.Config, _ *library.Gallery) {
		cfg.Server.BaseURL = base
		cfg.Server.CORSOrigins = allowed
		cfg.Auth.Tokens = []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash("tok"), Scopes: config.AllScopes}}
	})
	req := httptest.NewRequest(method, target, nil)
	req.Host = "192.168.1.5:8455"
	req.Header.Set("Authorization", "Bearer tok")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func wantHeader(t *testing.T, w *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := w.Header().Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func TestCORSAllowsTheAddressTheRequestArrivedOn(t *testing.T) {
	w := corsRequest(t, "http://localhost:8455", "GET", "/api/v1/tags", "http://192.168.1.5:8455")

	wantCode(t, w, http.StatusOK)
	wantHeader(t, w, "Access-Control-Allow-Origin", "http://192.168.1.5:8455")
	wantHeader(t, w, "Vary", "Origin")
}

func TestCORSEchoesTheAllowedOrigin(t *testing.T) {
	w := corsRequest(t, "https://myapp.example.com/", "GET", "/api/v1/tags", "https://myapp.example.com")

	wantCode(t, w, http.StatusOK)
	wantHeader(t, w, "Access-Control-Allow-Origin", "https://myapp.example.com")
}

func TestCORSVariesWithoutAnOrigin(t *testing.T) {
	w := corsRequest(t, "http://localhost:8455", "GET", "/api/v1/tags", "")

	wantCode(t, w, http.StatusOK)
	wantHeader(t, w, "Access-Control-Allow-Origin", "")
	wantHeader(t, w, "Vary", "Origin")
}

func TestCORSPreflightAnswersAllowedOrigin(t *testing.T) {
	w := corsRequest(t, "https://myapp.example.com", "OPTIONS", "/api/v1/images/search", "https://myapp.example.com")

	wantCode(t, w, http.StatusNoContent)
	wantHeader(t, w, "Access-Control-Allow-Origin", "https://myapp.example.com")
	wantHeader(t, w, "Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	wantHeader(t, w, "Access-Control-Allow-Headers", "Authorization, Content-Type, X-Monbooru-Gallery")
	wantHeader(t, w, "Access-Control-Max-Age", "600")
	wantHeader(t, w, "Vary", "Origin")
}

func TestCORSPreflightRejectsBadOrigin(t *testing.T) {
	w := corsRequest(t, "https://myapp.example.com", "OPTIONS", "/api/v1/images/search", "https://evil.example.com")

	wantCode(t, w, http.StatusForbidden)
	wantHeader(t, w, "Access-Control-Allow-Origin", "")
}

func TestCORSPreflightCoversEveryAPIPath(t *testing.T) {
	for _, target := range []string{"/api/v1/", "/api/v1/images", "/api/v1/images/42", "/api/v1/tags/merge", "/api/v1/openapi.json"} {
		w := corsRequest(t, "https://myapp.example.com", "OPTIONS", target, "https://myapp.example.com")
		if w.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s = %d, want %d", target, w.Code, http.StatusNoContent)
		}
	}
}

func TestCORSOnSpecAndDocs(t *testing.T) {
	for _, target := range []string{"/api/v1/openapi.json", "/api/v1/docs"} {
		w := corsRequest(t, "https://myapp.example.com", "GET", target, "https://myapp.example.com")
		wantCode(t, w, http.StatusOK)
		wantHeader(t, w, "Access-Control-Allow-Origin", "https://myapp.example.com")

		bad := corsRequest(t, "https://myapp.example.com", "GET", target, "https://evil.example.com")
		if bad.Code != http.StatusForbidden {
			t.Errorf("GET %s from a bad origin = %d, want %d", target, bad.Code, http.StatusForbidden)
		}
	}
}

func TestCORSExposesContentDisposition(t *testing.T) {
	w := corsRequest(t, "https://myapp.example.com", "GET", "/api/v1/tags", "https://myapp.example.com")

	wantHeader(t, w, "Access-Control-Expose-Headers", "Content-Disposition")
}

func TestCORSAllowsAConfiguredOrigin(t *testing.T) {
	const dev = "http://localhost:5173"
	w := corsRequest(t, "https://myapp.example.com", "GET", "/api/v1/tags", dev, "https://other.example", dev)

	wantCode(t, w, http.StatusOK)
	wantHeader(t, w, "Access-Control-Allow-Origin", dev)

	pre := corsRequest(t, "https://myapp.example.com", "OPTIONS", "/api/v1/tags", dev, dev)
	wantCode(t, pre, http.StatusNoContent)
	wantHeader(t, pre, "Access-Control-Allow-Origin", dev)
}

func TestCORSWildcardAllowsAnyOrigin(t *testing.T) {
	w := corsRequest(t, "https://myapp.example.com", "GET", "/api/v1/tags", "https://anything.example", "*")

	wantCode(t, w, http.StatusOK)
	wantHeader(t, w, "Access-Control-Allow-Origin", "https://anything.example")
}

func TestCORSUnlistedOriginStillRefused(t *testing.T) {
	w := corsRequest(t, "https://myapp.example.com", "GET", "/api/v1/tags", "https://evil.example", "http://localhost:5173")

	wantCode(t, w, http.StatusForbidden)
	wantHeader(t, w, "Access-Control-Allow-Origin", "")
}

func TestBearerAuth_MissingHeader(t *testing.T) {
	mux := newBareEnv(t, nil, func(cfg *config.Config, _ *library.Gallery) {
		cfg.Auth.Tokens = []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash("required-token"), Scopes: config.AllScopes}}
	})

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	wantCode(t, w, http.StatusUnauthorized)
}

func TestCreateImage_Multipart(t *testing.T) {
	env := newTestEnv(t)

	w := env.postUpload(t, "upload.png", pngFileBytes(t, 12, 12), map[string]string{"tags": `["multipart_tag"]`})

	wantCode(t, w, http.StatusCreated)
}

func TestCreateImage_Multipart_RefusesANameOnTheIgnoreList(t *testing.T) {
	env := newTestEnv(t)
	env.bounds.Store(gallery.NewBoundary(env.galleryDir, nil, []string{"._*"}))
	imgBytes := pngFileBytes(t, 12, 12)

	w := env.postUpload(t, "._shot.png", imgBytes, nil)

	mustCode(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "ignore list") {
		t.Errorf("body = %s, want the ignore list named", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.galleryDir, "._shot.png")); err == nil {
		t.Error("a file named on the ignore list was written into the gallery")
	}
}

func TestCreateImage_Multipart_FoldsANameAFilesystemRefuses(t *testing.T) {
	env := newTestEnv(t)
	imgBytes := pngFileBytes(t, 12, 12)

	w := env.postUpload(t, "a:b?.png", imgBytes, nil)

	mustCode(t, w, http.StatusCreated)
	if _, err := os.Stat(filepath.Join(env.galleryDir, "a_b_.png")); err != nil {
		t.Errorf("folded name missing: %v", err)
	}
}

func TestCreateImage_Multipart_BoundsALongName(t *testing.T) {
	env := newTestEnv(t)
	name := strings.Repeat("a", 300) + ".png"
	bounded := strings.Repeat("a", 180)

	mustCode(t, env.postUpload(t, name, pngFileBytes(t, 12, 12), nil), http.StatusCreated)
	mustCode(t, env.postUpload(t, name, pngFileBytes(t, 13, 13), nil), http.StatusCreated)

	for _, want := range []string{bounded + ".png", bounded + "_1.png"} {
		if _, err := os.Stat(filepath.Join(env.galleryDir, want)); err != nil {
			t.Errorf("%d-byte landing missing: %v", len(want), err)
		}
	}
}

func TestCreateImage_Multipart_NamesWhyTheWriteFailed(t *testing.T) {
	env := newTestEnv(t)
	if err := os.Chmod(env.galleryDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(env.galleryDir, 0o755) })
	if f, err := os.Create(filepath.Join(env.galleryDir, "probe")); err == nil {
		_ = f.Close()
		t.Skip("the gallery folder stays writable (root)")
	}

	w := env.postUpload(t, "upload.png", pngFileBytes(t, 12, 12), nil)

	mustCode(t, w, http.StatusInternalServerError)
	if body := w.Body.String(); !strings.Contains(body, "failed to save upload: permission denied") || strings.Contains(body, env.galleryDir) {
		t.Errorf("body = %s, want the errno without the path", body)
	}
}

func TestCreateImage_Multipart_NonMediaBytesAreATypeRefusal(t *testing.T) {
	for _, name := range []string{"clip.mp4", "clip.webm", "book.cbz"} {
		env := newTestEnv(t)
		w := env.postUpload(t, name, []byte("plain text, no media signature"), nil)
		wantCode(t, w, http.StatusBadRequest)
		wantIn(t, w.Body.String(), `"code":"unsupported_type"`)
		wantNotIn(t, w.Body.String(), env.galleryDir)
	}
}

func TestCreateImage_AFailedIngestNamesNoPath(t *testing.T) {
	env := newTestEnv(t)

	w := env.postUpload(t, "book.cbz", []byte("PK\x03\x04 and no archive after the signature"), nil)

	wantCode(t, w, http.StatusInternalServerError)
	wantNotIn(t, w.Body.String(), env.galleryDir)
}

func TestCreateImage_Multipart_RejectsMalformedTags(t *testing.T) {
	env := newTestEnv(t)

	imgBytes := pngFileBytes(t, 12, 12)
	w := env.postUpload(t, "badtags.png", imgBytes, map[string]string{"tags": "cat,dog"})

	mustCode(t, w, http.StatusBadRequest)
	n := countRows(t, env.database, "images")
	if n != 0 {
		t.Errorf("a rejected create left %d image row(s) behind", n)
	}
}

func TestCreateImage_Multipart_UnlimitedSize(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.MaxFileSizeMB = 0

	w := env.postUpload(t, "big.png", paddedPNG(t, multipartMetaSlack+1), nil)

	wantCode(t, w, http.StatusCreated)
}

func TestCreateImage_Multipart_MissingFile(t *testing.T) {
	env := newTestEnv(t)

	w := env.postUpload(t, "", nil, map[string]string{"other_field": "value"})

	wantCode(t, w, http.StatusBadRequest)
}

func TestListTags_WithCategory(t *testing.T) {
	env := newTestEnv(t)

	w := env.do("GET", "/api/v1/tags?category=general")

	wantCode(t, w, http.StatusOK)
	body := w.Body.String()
	wantIn(t, body, "results")
}

func listTagsMatchingNothing(t *testing.T, query string) {
	t.Helper()
	env := newTestEnv(t)
	mustCode(t, doJSON(env.mux, "POST", "/api/v1/tags", []byte(`{"name":"blue_sky"}`)), http.StatusCreated)
	if w := env.do("GET", "/api/v1/tags"); !strings.Contains(w.Body.String(), "blue_sky") {
		t.Fatalf("unfiltered list = %s, want the seeded tag", w.Body.String())
	}

	w := env.do("GET", "/api/v1/tags?"+query)
	wantCode(t, w, http.StatusOK)
	var page struct {
		Total   int               `json:"total"`
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || page.Results == nil || len(page.Results) != 0 {
		t.Errorf("%s = %s, want an empty page", query, w.Body.String())
	}
}

func TestListTags_SortOrderAndUsedBy(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "used_by.png", 10, 10)
	path := fmt.Sprintf("/api/v1/images/%d/tags", id)
	mustCode(t, doJSON(env.mux, "POST", path, []byte(`{"tags":["blue_sky","cloud"],"via":"my_app"}`)), http.StatusOK)
	mustCode(t, doJSON(env.mux, "POST", path, []byte(`{"tags":["red_hair"]}`)), http.StatusOK)

	names := func(query string) []string {
		t.Helper()
		w := env.do("GET", "/api/v1/tags?show_zero=0&type=tag&"+query)
		mustCode(t, w, http.StatusOK)
		var page struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		}
		mustDecode(t, w, &page)
		var out []string
		for _, r := range page.Results {
			if r.Name == "blue_sky" || r.Name == "cloud" || r.Name == "red_hair" {
				out = append(out, r.Name)
			}
		}
		return out
	}
	for query, want := range map[string][]string{
		"sort=name":                    {"blue_sky", "cloud", "red_hair"},
		"sort=alpha":                   {"blue_sky", "cloud", "red_hair"},
		"sort=name&order=desc":         {"red_hair", "cloud", "blue_sky"},
		"sort=created&order=asc":       {"blue_sky", "cloud", "red_hair"},
		"sort=name&used_by=my_app":     {"blue_sky", "cloud"},
		"sort=name&used_by=api":        {"red_hair"},
		"sort=name&used_by=nobody_yet": nil,
	} {
		if got := names(query); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
	}
}

func TestListTags_UnknownCategoryMatchesNothing(t *testing.T) {
	listTagsMatchingNothing(t, "category=nonexistent_cat_xyz")
}

func TestListTags_UnknownTypeMatchesNothing(t *testing.T) {
	listTagsMatchingNothing(t, "type=nope")
}

func TestAddImageTags_WriteFailureIs500(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_write_fail.png", 10, 10)
	path := fmt.Sprintf("/api/v1/images/%d/tags", id)
	for _, trigger := range []string{
		`CREATE TRIGGER fail_write BEFORE INSERT ON tags BEGIN SELECT RAISE(ABORT, 'busy'); END`,
		`CREATE TRIGGER fail_write BEFORE INSERT ON image_tags BEGIN SELECT RAISE(ABORT, 'busy'); END`,
	} {
		mustExec(t, env.database.Write, trigger)
		wantCode(t, doJSON(env.mux, "POST", path, []byte(`{"tags":["blue_sky"]}`)), http.StatusInternalServerError)
		mustExec(t, env.database.Write, `DROP TRIGGER fail_write`)
	}
	w := doJSON(env.mux, "POST", path, []byte(`{"tags":["blue_sky", "bad*name"]}`))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "tag_warnings")
}

func TestRemoveImageTags_DatabaseErrorIs500NotConflict(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_remove_fail.png", 10, 10)
	path := fmt.Sprintf("/api/v1/images/%d/tags", id)
	mustCode(t, doJSON(env.mux, "POST", path, []byte(`{"tags":["blue_sky"]}`)), http.StatusOK)

	mustExec(t, env.database.Write, `CREATE TRIGGER fail_tag_delete BEFORE DELETE ON image_tags BEGIN SELECT RAISE(ABORT, 'busy'); END`)
	wantCode(t, doJSON(env.mux, "DELETE", path, []byte(`{"tags":["blue_sky"]}`)), http.StatusInternalServerError)

	mustExec(t, env.database.Write, `ALTER TABLE tag_categories RENAME TO tag_categories_gone`)
	wantCode(t, doJSON(env.mux, "DELETE", path, []byte(`{"tags":["general:blue_sky"]}`)), http.StatusInternalServerError)
}

func TestCategoryLookup_ReadFailureIsNotAnUnknownCategory(t *testing.T) {
	env := newTestEnv(t)
	mustExec(t, env.database.Write, `ALTER TABLE tag_categories RENAME TO tag_categories_gone`)

	w := env.do("GET", "/api/v1/tags?category=general")
	wantCode(t, w, http.StatusInternalServerError)

	w = doJSON(env.mux, "POST", "/api/v1/tags", []byte(`{"name":"x","category":"general"}`))
	wantCode(t, w, http.StatusInternalServerError)
}

func TestDeleteImage_EmptyFolderKeptByDefault(t *testing.T) {
	env := newTestEnv(t)

	subDir := filepath.Join(env.galleryDir, "cleanup_default")
	mustMkdirAll(t, subDir, 0o755)
	imgPath := filepath.Join(subDir, "single.png")
	mustWriteFile(t, imgPath, pngFileBytes(t, 8, 8), 0o644)
	record, _ := mustIngest(t, env.database, env.galleryDir, env.thumbDir, imgPath, "")

	w := env.do("DELETE", fmt.Sprintf("/api/v1/images/%d", record.ID))
	mustCode(t, w, http.StatusNoContent)
	if _, err := os.Stat(subDir); err != nil {
		t.Errorf("empty parent folder should be kept by default; stat err = %v", err)
	}
}

func TestDeleteImage_DeleteEmptyFolderRefusesTraversal(t *testing.T) {
	env := newTestEnv(t)

	subDir := filepath.Join(env.galleryDir, "trav")
	mustMkdirAll(t, subDir, 0o755)
	imgPath := filepath.Join(subDir, "x.png")
	mustWriteFile(t, imgPath, pngFileBytes(t, 8, 8), 0o644)
	record, _ := mustIngest(t, env.database, env.galleryDir, env.thumbDir, imgPath, "")
	_ = os.Remove(imgPath)

	escapeDir := filepath.Join(filepath.Dir(env.galleryDir), "mb_escape_test")
	mustMkdirAll(t, escapeDir, 0o755)
	mustExec(t, env.database.Write, `UPDATE images SET folder_path = ? WHERE id = ?`, "../mb_escape_test", record.ID)

	env.do("DELETE", fmt.Sprintf("/api/v1/images/%d?delete_empty_folder=true", record.ID))

	if _, err := os.Stat(escapeDir); err != nil {
		t.Errorf("folder outside the gallery root must not be removed; stat err = %v", err)
	}
}

func TestDeleteImage_DeleteEmptyFolder(t *testing.T) {
	env := newTestEnv(t)

	subDir := filepath.Join(env.galleryDir, "sub2024")
	mustMkdirAll(t, subDir, 0o755)
	imgPath := filepath.Join(subDir, "sub_img.png")
	mustWriteFile(t, imgPath, pngFileBytes(t, 13, 13), 0o644)

	record, _ := mustIngest(t, env.database, env.galleryDir, env.thumbDir, imgPath, "")
	if err := os.Remove(imgPath); err != nil {
		t.Fatal(err)
	}

	w := env.do("DELETE", fmt.Sprintf("/api/v1/images/%d?delete_empty_folder=true", record.ID))

	mustCode(t, w, http.StatusOK)
	var body map[string]any
	mustDecode(t, w, &body)
	if body["folder_deleted"] != true {
		t.Errorf("folder_deleted = %v, want true", body["folder_deleted"])
	}
	if body["folder"] != "sub2024" {
		t.Errorf("folder = %v, want sub2024", body["folder"])
	}
	if _, err := os.Stat(subDir); !os.IsNotExist(err) {
		t.Errorf("sub-folder should have been removed, stat err = %v", err)
	}
}

func createImageJSON(t *testing.T, env *testEnv, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := doJSON(env.mux, "POST", "/api/v1/images", raw)
	mustCode(t, w, wantStatus)
	var resp map[string]any
	if w.Body.Len() > 0 {
		mustDecode(t, w, &resp)
	}
	return resp
}

func TestCreateImage_SetsProvenanceFields(t *testing.T) {
	env := newTestEnv(t)

	path := env.writePNG(t, "prov.png", 9, 9)

	resp := createImageJSON(t, env, map[string]any{
		"path":             path,
		"source":           "danbooru",
		"url":              "https://example.com/post/1",
		"collection":       "my_series",
		"collection_order": 3,
	}, http.StatusCreated)

	id := int64(resp["id"].(float64))
	gw := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	mustCode(t, gw, http.StatusOK)
	var got map[string]any
	if err := json.NewDecoder(gw.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["source"] != "danbooru" {
		t.Errorf("source = %v, want danbooru", got["source"])
	}
	if got["url"] != "https://example.com/post/1" {
		t.Errorf("url = %v, want the posted URL", got["url"])
	}
	if got["collection"] != "my_series" {
		t.Errorf("collection = %v, want my_series", got["collection"])
	}
	if got["collection_order"] != float64(3) {
		t.Errorf("collection_order = %v, want 3", got["collection_order"])
	}
}

func TestCreateImage_Multipart_SetsProvenanceFields(t *testing.T) {
	env := newTestEnv(t)

	w := env.postUpload(t, "mp_prov.png", pngFileBytes(t, 14, 14), map[string]string{
		"source": "scraper_v2", "collection": "set_a", "collection_order": "5",
	})
	mustCode(t, w, http.StatusCreated)
	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["source"] != "scraper_v2" {
		t.Errorf("source = %v, want scraper_v2", resp["source"])
	}
	if resp["collection"] != "set_a" {
		t.Errorf("collection = %v, want set_a", resp["collection"])
	}
	if resp["collection_order"] != float64(5) {
		t.Errorf("collection_order = %v, want 5", resp["collection_order"])
	}
}

func TestCreateImage_RejectsBadURL(t *testing.T) {
	env := newTestEnv(t)
	path := env.writePNG(t, "badurl.png", 9, 9)

	createImageJSON(t, env, map[string]any{
		"path": path,
		"url":  "ftp://nope",
	}, http.StatusBadRequest)
}

func TestCreateImage_RejectsOrderWithoutCollection(t *testing.T) {
	env := newTestEnv(t)
	path := env.writePNG(t, "orphan_order.png", 9, 9)

	createImageJSON(t, env, map[string]any{
		"path":             path,
		"collection_order": 2,
	}, http.StatusBadRequest)
}

func TestCreateImage_DuplicateKeepsOriginalProvenance(t *testing.T) {
	env := newTestEnv(t)
	path := env.writePNG(t, "dup_prov.png", 16, 16)

	first := createImageJSON(t, env, map[string]any{
		"path":   path,
		"source": "first_source",
	}, http.StatusCreated)
	id := int64(first["id"].(float64))

	resp := createImageJSON(t, env, map[string]any{
		"path":   path,
		"source": "second_source",
	}, http.StatusOK)
	if resp["alias_added"] != false {
		t.Fatalf("alias_added = %v, want false for a re-push of the canonical path", resp["alias_added"])
	}

	gw := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	var got map[string]any
	if err := json.NewDecoder(gw.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["source"] != "first_source" {
		t.Errorf("source = %v, want first_source (duplicate must not overwrite)", got["source"])
	}
}

func TestCreateImage_MultipartRestoringTheCanonicalFileKeepsIt(t *testing.T) {
	env := newTestEnv(t)

	imgBytes := pngFileBytes(t, 14, 14)
	w := env.postUpload(t, "restore.png", imgBytes, nil)
	mustCode(t, w, http.StatusCreated)
	var created map[string]any
	mustDecode(t, w, &created)
	id := int64(created["id"].(float64))
	_, canonPath, _, _, _ := env.imageRow(t, id)

	if err := os.Remove(canonPath); err != nil {
		t.Fatal(err)
	}

	w2 := env.postUpload(t, filepath.Base(canonPath), imgBytes, nil)
	mustCode(t, w2, http.StatusOK)
	var restored map[string]any
	mustDecode(t, w2, &restored)
	if restored["alias_added"] != false {
		t.Errorf("alias_added = %v, want false for a restored canonical file", restored["alias_added"])
	}
	if _, err := os.Stat(canonPath); err != nil {
		t.Fatalf("the push unlinked the file it restored: %v", err)
	}
	if n := countRows(t, env.database, "image_paths WHERE image_id = ? AND is_canonical = 1", id); n != 1 {
		t.Errorf("canonical image_paths rows = %d, want 1", n)
	}
}

func TestCreateImage_DuplicateAddsCollectionMembership(t *testing.T) {
	env := newTestEnv(t)
	path := env.writePNG(t, "dup_coll.png", 12, 12)

	first := createImageJSON(t, env, map[string]any{"path": path}, http.StatusCreated)
	id := int64(first["id"].(float64))

	getImage := func() map[string]any {
		gw := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
		var got map[string]any
		if err := json.NewDecoder(gw.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	createImageJSON(t, env, map[string]any{
		"path":             path,
		"collection":       "pool 100",
		"collection_order": 2,
	}, http.StatusOK)
	got := getImage()
	if got["collection"] != "pool 100" {
		t.Errorf("collection = %v, want pool 100", got["collection"])
	}
	if got["collection_order"] != float64(2) {
		t.Errorf("collection_order = %v, want 2", got["collection_order"])
	}

	createImageJSON(t, env, map[string]any{
		"path":       path,
		"collection": "pool 200",
	}, http.StatusOK)
	got = getImage()
	if got["collection"] != "pool 100" {
		t.Errorf("collection = %v, want pool 100 (membership must not displace the home)", got["collection"])
	}
	var names []string
	if cols, ok := got["collections"].([]any); ok {
		for _, c := range cols {
			names = append(names, c.(map[string]any)["name"].(string))
		}
	}
	if len(names) != 2 || names[0] != "pool 100" || names[1] != "pool 200" {
		t.Errorf("collections = %v, want [pool 100 pool 200]", names)
	}
}

func patchImage(t *testing.T, env *testEnv, id int64, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := doJSON(env.mux, "PATCH", fmt.Sprintf("/api/v1/images/%d", id), raw)
	mustCode(t, w, wantStatus)
	var resp map[string]any
	if w.Body.Len() > 0 {
		mustDecode(t, w, &resp)
	}
	return resp
}

func TestPatchImage_UpdatesFields(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_all.png", 10, 10)

	resp := patchImage(t, env, id, map[string]any{
		"source":           "booru",
		"url":              "https://example.com/x",
		"collection":       "vol1",
		"collection_order": 4,
		"is_favorited":     true,
		"is_inbox":         false,
	}, http.StatusOK)

	if resp["source"] != "booru" {
		t.Errorf("source = %v, want booru", resp["source"])
	}
	if resp["url"] != "https://example.com/x" {
		t.Errorf("url = %v", resp["url"])
	}
	if resp["collection"] != "vol1" {
		t.Errorf("collection = %v, want vol1", resp["collection"])
	}
	if resp["collection_order"] != float64(4) {
		t.Errorf("collection_order = %v, want 4", resp["collection_order"])
	}
	if resp["is_favorited"] != true {
		t.Errorf("is_favorited = %v, want true", resp["is_favorited"])
	}
	if resp["is_inbox"] != false {
		t.Errorf("is_inbox = %v, want false", resp["is_inbox"])
	}
}

func TestPatchImage_RefreshesTheSourceCounts(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_counts.png", 10, 10)
	if _, err := env.gallery.SourceLabelCounts(); err != nil {
		t.Fatal(err)
	}
	patchImage(t, env, id, map[string]any{"source": "booru", "url": "https://example.com/x"}, http.StatusOK)
	labels, err := env.gallery.SourceLabelCounts()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(labels, gallery.SourceLabelCount{Source: "booru", Count: 1}) {
		t.Errorf("source counts %+v, want booru counted once", labels)
	}
}

func TestPatchImage_PartialLeavesOthers(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_partial.png", 10, 10)

	patchImage(t, env, id, map[string]any{"source": "keep_me", "collection": "set_x"}, http.StatusOK)
	resp := patchImage(t, env, id, map[string]any{"url": "https://only.example.com"}, http.StatusOK)

	if resp["source"] != "keep_me" {
		t.Errorf("source = %v, want keep_me (untouched)", resp["source"])
	}
	if resp["collection"] != "set_x" {
		t.Errorf("collection = %v, want set_x (untouched)", resp["collection"])
	}
	if resp["url"] != "https://only.example.com" {
		t.Errorf("url = %v", resp["url"])
	}
}

func TestPatchImage_ClearCollectionNullsOrder(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_clear.png", 10, 10)

	patchImage(t, env, id, map[string]any{"collection": "temp", "collection_order": 2}, http.StatusOK)
	resp := patchImage(t, env, id, map[string]any{"collection": ""}, http.StatusOK)

	if resp["collection"] != "" {
		t.Errorf("collection = %v, want empty", resp["collection"])
	}
	if resp["collection_order"] != nil {
		t.Errorf("collection_order = %v, want null after clearing collection", resp["collection_order"])
	}
}

func TestPatchImage_OrderRequiresCollection(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_order_orphan.png", 10, 10)
	patchImage(t, env, id, map[string]any{"collection_order": 3}, http.StatusBadRequest)
}

func TestPatchImage_RejectsBadURL(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_badurl.png", 10, 10)
	patchImage(t, env, id, map[string]any{"url": "ftp://nope"}, http.StatusBadRequest)
}

func TestPatchImage_ARefusedRelabelWritesNothing(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_relabel.png", 10, 10)
	for _, site := range []string{"first", "second"} {
		if err := gallery.AddSourceMembership(env.database, id, site, "", "https://"+site+".example.com/1"); err != nil {
			t.Fatal(err)
		}
	}

	patchImage(t, env, id, map[string]any{"source": "second", "is_favorited": true, "is_inbox": false, "collection": "kept_out"}, http.StatusConflict)
	resp := patchImage(t, env, id, map[string]any{"url": "https://first.example.com/1"}, http.StatusOK)
	if resp["is_favorited"] != false || resp["is_inbox"] != true || resp["collection"] != "" {
		t.Errorf("favorited %v, inbox %v, collection %q: the refused PATCH stored part of the change", resp["is_favorited"], resp["is_inbox"], resp["collection"])
	}
}

func TestPatchImage_NoFields(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "patch_empty.png", 10, 10)
	patchImage(t, env, id, map[string]any{}, http.StatusBadRequest)
}

func TestPatchImage_NotFound(t *testing.T) {
	env := newTestEnv(t)
	patchImage(t, env, 99999, map[string]any{"source": "x"}, http.StatusNotFound)
}

func enrich(t *testing.T, env *testEnv, id int64, body map[string]any, wantStatus int) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), raw)
	mustCode(t, w, wantStatus)
}

func pngFileBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	mustEncodePNG(t, &buf, image.NewRGBA(image.Rect(0, 0, w, h)))
	return buf.Bytes()
}

func multipartForm(t *testing.T, filename string, fileBytes []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(fileBytes); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func (e *testEnv) postMultipart(target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", target, body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func (e *testEnv) postUpload(t *testing.T, filename string, fileBytes []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartForm(t, filename, fileBytes, fields)
	return e.postMultipart("/api/v1/images", body, ct)
}

func (e *testEnv) postRawMultipart(t *testing.T, target string, fileBytes []byte, cut int) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartForm(t, "raw.png", fileBytes, nil)
	return e.postMultipart(target, bytes.NewReader(body.Bytes()[:body.Len()-cut]), ct)
}

func TestCreateImage_Multipart_TruncatedBodyIs400(t *testing.T) {
	env := newTestEnv(t)
	w := env.postRawMultipart(t, "/api/v1/images", pngFileBytes(t, 16, 16), 80)
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "invalid_request")
}

func TestReplaceImageFile_TruncatedBodyIs400(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "keep.png", 10, 10)
	w := env.postRawMultipart(t, fmt.Sprintf("/api/v1/images/%d/file", id), pngFileBytes(t, 16, 16), 80)
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "invalid_request")
}

func TestCreateImage_Multipart_OverCapIs413(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.MaxFileSizeMB = 1
	w := env.postRawMultipart(t, "/api/v1/images", make([]byte, 2<<20), 0)
	mustCode(t, w, http.StatusRequestEntityTooLarge)
	wantIn(t, w.Body.String(), "file_too_large")
}

func TestReplaceImageFile_OverCapIs413(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.MaxFileSizeMB = 1
	id := env.createTestImage(t, "keep.png", 10, 10)
	oldSHA, _, _, _, _ := env.imageRow(t, id)
	w := env.postRawMultipart(t, fmt.Sprintf("/api/v1/images/%d/file", id), make([]byte, 2<<20), 0)
	mustCode(t, w, http.StatusRequestEntityTooLarge)
	wantIn(t, w.Body.String(), "file_too_large")
	if sha, _, _, _, _ := env.imageRow(t, id); sha != oldSHA {
		t.Errorf("sha = %q after the refusal, want %q", sha, oldSHA)
	}
}

func (e *testEnv) postReplace(t *testing.T, id int64, filename string, fileBytes []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartForm(t, filename, fileBytes, fields)
	return e.postMultipart(fmt.Sprintf("/api/v1/images/%d/file", id), body, ct)
}

func (e *testEnv) imageRow(t *testing.T, id int64) (sha, canonPath, fileType string, width, fileSize int64) {
	t.Helper()
	if err := e.database.Read.QueryRow(
		`SELECT sha256, canonical_path, file_type, COALESCE(width, 0), file_size FROM images WHERE id = ?`, id,
	).Scan(&sha, &canonPath, &fileType, &width, &fileSize); err != nil {
		t.Fatal(err)
	}
	return
}

func TestReplaceImageFile_SwapsBytesAndRederives(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "orig.png", 10, 10)
	oldSHA, oldPath, _, _, _ := env.imageRow(t, id)

	tagW := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), []byte(`{"tags":["keepme"]}`))
	mustCode(t, tagW, http.StatusOK)
	enrich(t, env, id, map[string]any{
		"source": "danbooru", "post_id": "77", "url": "https://danbooru.example/posts/77",
		"similarity": 92.0,
	}, http.StatusOK)
	if _, err := env.database.Write.Exec(
		`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body) VALUES (?, '', '', 2, 4, 6, 8, 'box')`,
		id); err != nil {
		t.Fatal(err)
	}

	w := env.postReplace(t, id, "better.png", pngFileBytes(t, 20, 20), map[string]string{
		"source": "danbooru", "post_id": "77", "url": "https://danbooru.example/posts/77",
		"md5": "0123456789abcdef0123456789abcdef", "tags": `["newtag"]`,
	})
	mustCode(t, w, http.StatusOK)
	var resp struct {
		Replaced bool `json:"replaced"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.Replaced {
		t.Fatalf("response = %s, want replaced=true", w.Body.String())
	}

	newSHA, newPath, fileType, width, _ := env.imageRow(t, id)
	if newSHA == oldSHA {
		t.Error("sha256 unchanged after replace")
	}
	if newPath != oldPath || fileType != "png" || width != 20 {
		t.Errorf("row = (%q, %q, %d), want same path, png, width 20", newPath, fileType, width)
	}
	if got, err := gallery.HashFile(newPath); err != nil || got != newSHA {
		t.Errorf("disk sha = %q (%v), want %q", got, err, newSHA)
	}

	srcs, err := gallery.SourcesForImage(env.database, id)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources = %+v (%v)", srcs, err)
	}
	wantMD5, _ := gallery.Md5File(newPath)
	if srcs[0].Similarity != 0 || srcs[0].MD5Match != "match" || srcs[0].MD5 != wantMD5 {
		t.Errorf("origin = %+v, want similarity 0, match, md5 %q", srcs[0], wantMD5)
	}

	var x, y, wBox, hBox int
	if err := env.database.Read.QueryRow(
		`SELECT x, y, w, h FROM image_annotations WHERE image_id = ?`, id).Scan(&x, &y, &wBox, &hBox); err != nil {
		t.Fatal(err)
	}
	if x != 4 || y != 8 || wBox != 12 || hBox != 16 {
		t.Errorf("annotation = (%d,%d,%d,%d), want doubled (4,8,12,16)", x, y, wBox, hBox)
	}

	tagNames, err := db.QueryStrings(env.database.Read,
		`SELECT t.name FROM image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? ORDER BY t.name`, id)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tagNames, ",")
	if !strings.Contains(got, "keepme") || !strings.Contains(got, "newtag") {
		t.Errorf("tags = %q, want keepme kept and newtag merged", got)
	}

	if leftovers, _ := filepath.Glob(filepath.Join(env.thumbDir, "replace-*")); len(leftovers) != 0 {
		t.Errorf("staging leftovers: %v", leftovers)
	}
}

func TestReplaceImageFile_ExtensionChange(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)
	_, oldPath, _, _, _ := env.imageRow(t, id)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	w := env.postReplace(t, id, "post.jpg", buf.Bytes(), nil)
	mustCode(t, w, http.StatusOK)

	_, newPath, fileType, _, _ := env.imageRow(t, id)
	if filepath.Ext(newPath) != ".jpg" || fileType != "jpeg" {
		t.Errorf("row = (%q, %q), want .jpg jpeg", newPath, fileType)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old path %q still exists", oldPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new path %q: %v", newPath, err)
	}
}

func TestReplaceImageFile_ShaNoopStillMerges(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "same.png", 10, 10)
	oldSHA, path, _, _, _ := env.imageRow(t, id)
	sameBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	w := env.postReplace(t, id, "same.png", sameBytes, map[string]string{
		"source": "danbooru", "post_id": "5", "url": "https://danbooru.example/posts/5",
		"tags": `["fromsource"]`,
	})
	mustCode(t, w, http.StatusOK)
	var resp struct {
		Replaced bool `json:"replaced"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Replaced {
		t.Fatalf("response = %s, want replaced=false", w.Body.String())
	}
	newSHA, _, _, _, _ := env.imageRow(t, id)
	if newSHA != oldSHA {
		t.Error("sha changed on a byte-identical replace")
	}
	srcs, _ := gallery.SourcesForImage(env.database, id)
	if len(srcs) != 1 || srcs[0].MD5Match != "match" {
		t.Errorf("sources = %+v, want one origin marked match", srcs)
	}
}

func TestReplace_TheUpstreamCopyIsTheOriginal(t *testing.T) {
	env := newTestEnv(t)
	local := env.createTestImage(t, "local.png", 10, 10)
	upstream := env.createTestImage(t, "upstream.png", 20, 20)
	_, pathUpstream, _, _, _ := env.imageRow(t, upstream)
	upstreamBytes, err := os.ReadFile(pathUpstream)
	if err != nil {
		t.Fatal(err)
	}

	mustCode(t, env.postReplace(t, local, "upstream.png", upstreamBytes, nil), http.StatusConflict)
	var original int64
	if err := env.database.Read.QueryRow(
		`SELECT g.original_image_id FROM dup_groups g JOIN dup_group_members m ON m.group_id = g.id WHERE m.image_id = ?`, local,
	).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if original != upstream {
		t.Errorf("group original = %d, want %d, the copy that already holds the upstream bytes", original, upstream)
	}
}

func TestReplaceImageFile_AFailedThumbnailLeavesNoStaleHash(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "stale.png", 10, 10)
	var oldPhash int64
	if err := env.database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&oldPhash); err != nil {
		t.Fatal(err)
	}
	tree := env.gallery.Source().Tree
	if err := tree.BuildFromDB(env.database); err != nil {
		t.Fatal(err)
	}

	thumb := gallery.ThumbnailPath(env.thumbDir, id)
	if err := os.Remove(thumb); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(thumb, "blocker"), 0o755)

	mustCode(t, env.postReplace(t, id, "fresh.png", pngFileBytes(t, 20, 20), nil), http.StatusOK)
	var phash sql.NullInt64
	if err := env.database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&phash); err != nil {
		t.Fatal(err)
	}
	if phash.Valid {
		t.Errorf("phash = %d, want NULL after a failed thumbnail", phash.Int64)
	}
	if got, _ := tree.SearchWithinDistance(oldPhash, 0); slices.Contains(got, id) {
		t.Error("the old bytes' hash still matches the replaced image")
	}
}

func TestReplaceImageFile_CollisionRecordsDuplicate(t *testing.T) {
	env := newTestEnv(t)
	idA := env.createTestImage(t, "a.png", 10, 10)
	idB := env.createTestImage(t, "b.png", 20, 20)
	shaA, pathA, _, _, _ := env.imageRow(t, idA)
	_, pathB, _, _, _ := env.imageRow(t, idB)
	bBytes, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}

	w := env.postReplace(t, idA, "b.png", bBytes, nil)
	mustCode(t, w, http.StatusConflict)
	if !strings.Contains(w.Body.String(), "already_exists") ||
		!strings.Contains(w.Body.String(), fmt.Sprintf("image %d", idB)) {
		t.Errorf("body = %s, want already_exists naming image %d", w.Body.String(), idB)
	}
	curSHA, _, _, _, _ := env.imageRow(t, idA)
	if curSHA != shaA {
		t.Error("target row changed on a refused replace")
	}
	if got, err := gallery.HashFile(pathA); err != nil || got != shaA {
		t.Error("target file changed on a refused replace")
	}
	var groups int
	if err := env.database.Read.QueryRow(
		`SELECT COUNT(DISTINCT group_id) FROM dup_group_members WHERE image_id IN (?, ?)`, idA, idB,
	).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	members := countRows(t, env.database, "dup_group_members WHERE image_id IN (?, ?)", idA, idB)
	if groups != 1 || members != 2 {
		t.Errorf("dup group = %d groups / %d members, want the pair in one group", groups, members)
	}
}

func TestReplaceImageFile_RefusesAnOverlongPostExt(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)
	oldSHA, _, _, _, _ := env.imageRow(t, id)

	w := env.postReplace(t, id, "post.png", pngFileBytes(t, 12, 12), map[string]string{
		"source": "danbooru", "post_ext": strings.Repeat("x", contract.MaxPostExt+1),
	})
	mustCode(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "post_ext") {
		t.Errorf("body = %s, want the post_ext limit named", w.Body.String())
	}
	if sha, _, _, _, _ := env.imageRow(t, id); sha != oldSHA {
		t.Errorf("sha = %s, want the refused replace to leave %s", sha, oldSHA)
	}
}

func TestReplaceImageFile_WrongTypeGate(t *testing.T) {
	env := newTestEnv(t)
	vidPath := filepath.Join(env.galleryDir, "clip.mp4")

	mustWriteFile(t, vidPath, []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0}, 0644)
	rec, _ := mustIngest(t, env.database, env.galleryDir, env.thumbDir, vidPath, "")

	w := env.postReplace(t, rec.ID, "x.png", pngFileBytes(t, 8, 8), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "wrong_type") {
		t.Errorf("replace on video row: %d %s, want 409 wrong_type", w.Code, w.Body.String())
	}
}

func TestCreateImage_RescueSkipsOtherFormatsNamedJPEG(t *testing.T) {
	bmp := ffmpegBMP(t)
	env := newTestEnv(t)
	w := env.postUpload(t, "picture.jpg", bmp, nil)
	wantCode(t, w, http.StatusUnsupportedMediaType)
}

func TestReplaceImageFile_RescueSkipsOtherFormatsNamedJPEG(t *testing.T) {
	bmp := ffmpegBMP(t)
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)
	oldSHA, _, _, _, _ := env.imageRow(t, id)

	w := env.postReplace(t, id, "post.jpg", bmp, nil)
	wantCode(t, w, http.StatusUnsupportedMediaType)
	if sha, _, _, _, _ := env.imageRow(t, id); sha != oldSHA {
		t.Errorf("sha = %s, want the original %s", sha, oldSHA)
	}
}

func jpegGoCannotDecode(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 40, 24))
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 40 * 6)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	b[bytes.Index(b, []byte{0xFF, 0xC0})+11] = 0x33
	return b
}

func TestCreateImage_RescueNeverExposesTheUndecodableBytes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	env := newTestEnv(t)
	dir := filepath.Join(env.galleryDir, "in")
	mustMkdirAll(t, dir, 0o755)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(dir); err != nil {
		t.Fatal(err)
	}

	w := env.postUpload(t, "odd.jpg", jpegGoCannotDecode(t), map[string]string{"folder": "in"})
	mustCode(t, w, http.StatusCreated)
	var resp struct {
		ID int64 `json:"id"`
	}
	mustDecode(t, w, &resp)
	_, path, fileType, width, _ := env.imageRow(t, resp.ID)
	if fileType != "jpeg" || width != 40 || !canDecodeImage(path) {
		t.Errorf("row = %s, %d wide, at %s; want the rescued 40 px JPEG", fileType, width, path)
	}

	drain := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case ev := <-watcher.Events:
			if ev.Name != path || ev.Has(fsnotify.Rename) || ev.Has(fsnotify.Remove) {
				t.Errorf("the gallery saw %v while the upload was rescued", ev)
			}
		case <-drain:
			done = true
		}
	}
	if left, _ := filepath.Glob(filepath.Join(env.thumbDir, "rescue-*")); len(left) > 0 {
		t.Errorf("staging left behind: %v", left)
	}
}

func TestReplaceImageFile_ARescuedJPEGStillRefreshesFromItsSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)
	raw := jpegGoCannotDecode(t)
	sum := md5.Sum(raw)
	postMD5 := hex.EncodeToString(sum[:])
	post := map[string]string{"source": "danbooru", "post_id": "5", "url": "https://danbooru.example/posts/5", "md5": postMD5}

	mustCode(t, env.postReplace(t, id, "post.jpg", raw, post), http.StatusOK)

	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), []byte(`{"source":"danbooru","post_id":"5","source_md5":"`+postMD5+`","verify":true,"tags":["fresh"]}`))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"verified":false`)
	wantNoUpgradeOffered(t, env, id)
}

func TestCreateImage_ARescuedJPEGStillRefreshesFromItsSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	env := newTestEnv(t)
	raw := jpegGoCannotDecode(t)
	sum := md5.Sum(raw)
	postMD5 := hex.EncodeToString(sum[:])

	w := env.postUpload(t, "odd.jpg", raw, map[string]string{"source": "danbooru", "post_id": "5", "url": "https://danbooru.example/posts/5", "md5": postMD5})
	mustCode(t, w, http.StatusCreated)
	var resp struct {
		ID int64 `json:"id"`
	}
	mustDecode(t, w, &resp)

	w = doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", resp.ID), []byte(`{"source":"danbooru","post_id":"5","source_md5":"`+postMD5+`","verify":true,"tags":["fresh"]}`))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"verified":false`)
	wantNoUpgradeOffered(t, env, resp.ID)
}

func wantNoUpgradeOffered(t *testing.T, env *testEnv, id int64) {
	t.Helper()
	srcs, err := gallery.SourcesForImage(env.database, id)
	if err != nil || len(srcs) == 0 {
		t.Fatalf("sources: %v %v", srcs, err)
	}
	for _, s := range srcs {
		if upgrade.Eligible(s) {
			t.Errorf("a rescued file's origin %s/%s offers [upgrade], which would rescue the same bytes again", s.Site, s.PostID)
		}
	}
}

var avifHeader = append([]byte{0x00, 0x00, 0x00, 0x1C, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0}, make([]byte, 64)...)

func TestCreateImage_TakesAnAVIFAsIs(t *testing.T) {
	sum := sha256.Sum256(avifHeader)
	for _, name := range []string{"post.avif", "post.jpg"} {
		env := newTestEnv(t)
		w := env.postUpload(t, name, avifHeader, nil)
		mustCode(t, w, http.StatusCreated)
		var resp map[string]any
		mustDecode(t, w, &resp)
		if resp["file_type"] != "avif" || resp["sha256"] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: row = %v %v, want avif with the pushed bytes", name, resp["file_type"], resp["sha256"])
		}
	}
}

func TestReplaceImageFile_TakesAnAVIF(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)

	w := env.postReplace(t, id, "post.avif", avifHeader, nil)
	mustCode(t, w, http.StatusOK)
	_, path, fileType, _, _ := env.imageRow(t, id)
	if filepath.Ext(path) != ".avif" || fileType != "avif" {
		t.Errorf("row = (%q, %q), want .avif avif", path, fileType)
	}
}

func ffmpegBMP(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dst := filepath.Join(t.TempDir(), "x.bmp")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "color=c=red:s=16x16", "-frames:v", "1", "--", dst)
	if err := cmd.Run(); err != nil {
		t.Skip("ffmpeg cannot write a BMP")
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReplaceImageFile_RefusesAFileAnotherGalleryOwns(t *testing.T) {
	env := newTestEnv(t)
	inner := filepath.Join(env.galleryDir, ".H")
	mustMkdirAll(t, inner, 0o755)
	id := env.createTestImage(t, filepath.Join(".H", "theirs.png"), 10, 10)
	oldSHA, oldPath, _, _, _ := env.imageRow(t, id)
	before, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	env.bounds.Store(gallery.NewBoundary(env.galleryDir, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))

	w := env.postReplace(t, id, "new.png", pngFileBytes(t, 12, 12), nil)
	wantCode(t, w, http.StatusConflict)
	wantIn(t, w.Body.String(), `"code":"conflict"`)
	wantIn(t, w.Body.String(), "belongs to gallery H")
	if sha, _, _, _, _ := env.imageRow(t, id); sha != oldSHA {
		t.Error("the row took the refused bytes")
	}
	if after, err := os.ReadFile(oldPath); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the file was rewritten (err=%v)", err)
	}
}

func TestReplaceImageFile_RefusesANewTypeWhoseNameTheGalleryIgnores(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)
	oldSHA, oldPath, _, _, _ := env.imageRow(t, id)
	env.bounds.Store(gallery.NewBoundary(env.galleryDir, nil, []string{"*.jpg"}))

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	w := env.postReplace(t, id, "post.jpg", buf.Bytes(), nil)
	wantCode(t, w, http.StatusConflict)
	wantIn(t, w.Body.String(), `"code":"conflict"`)
	if sha, path, _, _, _ := env.imageRow(t, id); sha != oldSHA || path != oldPath {
		t.Errorf("row = (%s, %q), want it unchanged", sha, path)
	}
	if _, err := os.Stat(strings.TrimSuffix(oldPath, ".png") + ".jpg"); !os.IsNotExist(err) {
		t.Errorf("a file landed on the ignored name (%v)", err)
	}
}

func TestReplaceImageFile_AFailureNamesNoPath(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)
	if err := os.Chmod(env.galleryDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(env.galleryDir, 0o755) })
	if f, err := os.Create(filepath.Join(env.galleryDir, "probe")); err == nil {
		_ = f.Close()
		t.Skip("the gallery folder stays writable (root)")
	}

	w := env.postReplace(t, id, "new.png", pngFileBytes(t, 12, 12), nil)

	wantCode(t, w, http.StatusInternalServerError)
	wantNotIn(t, w.Body.String(), env.galleryDir)
}

func TestReplaceImageFile_RejectsVideoUpload(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "still.png", 10, 10)
	w := env.postReplace(t, id, "clip.mp4", []byte("mp4 bytes"), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "wrong_type") {
		t.Errorf("video upload: %d %s, want 409 wrong_type", w.Code, w.Body.String())
	}
}

func TestEnrichImage_RecordsMd5Verdict(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "verdict.png", 10, 10)
	_, path, _, _, _ := env.imageRow(t, id)
	localMD5, err := gallery.Md5File(path)
	if err != nil {
		t.Fatal(err)
	}

	enrich(t, env, id, map[string]any{
		"source": "danbooru", "post_id": "1", "url": "https://danbooru.example/posts/1",
		"source_md5": localMD5, "verify": true,
	}, http.StatusOK)
	srcs, _ := gallery.SourcesForImage(env.database, id)
	if len(srcs) != 1 || srcs[0].MD5Match != "match" {
		t.Fatalf("after match enrich: %+v, want match", srcs)
	}

	enrich(t, env, id, map[string]any{
		"source": "danbooru", "post_id": "1", "url": "https://danbooru.example/posts/1",
		"source_md5": "ffffffffffffffffffffffffffffffff", "verify": true, "similarity": 88.0,
	}, http.StatusOK)
	srcs, _ = gallery.SourcesForImage(env.database, id)
	if len(srcs) != 1 || srcs[0].MD5Match != "differ" || srcs[0].Similarity != 88 {
		t.Fatalf("after waived mismatch: %+v, want differ at ~88", srcs)
	}

	mustExec(t, env.database.Write, `UPDATE image_sources SET similarity = 0, md5_match = '' WHERE image_id = ?`, id)
	enrich(t, env, id, map[string]any{
		"source": "danbooru", "post_id": "1", "url": "https://danbooru.example/posts/1",
		"source_md5": "ffffffffffffffffffffffffffffffff", "verify": true,
	}, http.StatusConflict)
	srcs, _ = gallery.SourcesForImage(env.database, id)
	if len(srcs) != 1 || srcs[0].MD5Match != "differ" {
		t.Fatalf("after refused mismatch: %+v, want differ", srcs)
	}
}

func TestReplaceImageFile_MisnamedPushRecordsTheBytes(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "pic.png", 10, 10)

	var buf bytes.Buffer
	mustEncodePNG(t, &buf, image.NewRGBA(image.Rect(0, 0, 24, 24)))
	w := env.postReplace(t, id, "post.jpg", buf.Bytes(), nil)
	mustCode(t, w, http.StatusOK)
	_, newPath, fileType, _, _ := env.imageRow(t, id)
	if fileType != "png" {
		t.Errorf("file_type = %q, want png", fileType)
	}
	if filepath.Ext(newPath) != ".png" {
		t.Errorf("path = %q, want a .png extension", newPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new path %q: %v", newPath, err)
	}
}

func pairScheduledLookups(env *testEnv) {
	env.cfg.Schedule.LookupPTR, env.cfg.Schedule.LookupBooru = true, true
	tok, _ := apitoken.New("monloader (paired)", config.AllScopes)
	tok.Paired = monloader.App
	env.cfg.Auth.Tokens = append(env.cfg.Auth.Tokens, tok)
}

func TestImageScheduledLookupRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	pairScheduledLookups(env)
	id := env.createTestImage(t, "sched_lookup.png", 10, 10)

	get := func() map[string]any {
		t.Helper()
		w := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
		mustCode(t, w, http.StatusOK)
		var resp map[string]any
		mustDecode(t, w, &resp)
		return resp
	}

	resp := get()
	if resp["scheduled_lookup"] != true {
		t.Errorf("scheduled_lookup = %v, want true by default", resp["scheduled_lookup"])
	}
	if _, present := resp["lookup"]; present {
		t.Errorf("lookup = %v, want it absent before anything is tried", resp["lookup"])
	}

	now := time.Now()
	for range lookup.BooruMaxAttempts {
		if err := lookup.Record(env.database, id, contract.LookupBooru, lookup.ResultMiss, 0, now); err != nil {
			t.Fatal(err)
		}
	}
	entry, _ := get()["lookup"].(map[string]any)
	booru, _ := entry["booru"].(map[string]any)
	if booru["attempts"] != float64(lookup.BooruMaxAttempts) || booru["last_result"] != "miss" {
		t.Fatalf("lookup.booru = %v, want an exhausted ladder", booru)
	}
	if _, scheduled := booru["next_due_at"]; scheduled {
		t.Errorf("lookup.booru = %v, want no next try scheduled", booru)
	}

	patch := func(body string) map[string]any {
		t.Helper()
		w := doJSON(env.mux, "PATCH", fmt.Sprintf("/api/v1/images/%d", id), []byte(body))
		mustCode(t, w, http.StatusOK)
		var resp map[string]any
		mustDecode(t, w, &resp)
		return resp
	}
	if got := patch(`{"scheduled_lookup":false}`); got["scheduled_lookup"] != false {
		t.Errorf("scheduled_lookup = %v after opting out, want false", got["scheduled_lookup"])
	}
	back := patch(`{"scheduled_lookup":true}`)
	if back["scheduled_lookup"] != true {
		t.Errorf("scheduled_lookup = %v after opting back in, want true", back["scheduled_lookup"])
	}
	entry, _ = back["lookup"].(map[string]any)
	booru, _ = entry["booru"].(map[string]any)
	if booru["attempts"] != float64(0) || booru["next_due_at"] == nil {
		t.Errorf("lookup.booru = %v, want a zeroed ladder due again", booru)
	}
	if booru["last_result"] != "miss" {
		t.Errorf("lookup.booru = %v, want the history kept through the reset", booru)
	}

	if got := patch(`{"scheduled_lookup_ptr":false}`); got["scheduled_lookup_ptr"] != false || got["scheduled_lookup"] != true {
		t.Errorf("flags = %v / %v after opting the PTR out, want the booru half untouched",
			got["scheduled_lookup"], got["scheduled_lookup_ptr"])
	}
	if got := patch(`{"scheduled_lookup_ptr":true}`); got["scheduled_lookup_ptr"] != true {
		t.Errorf("scheduled_lookup_ptr = %v after opting back in, want true", got["scheduled_lookup_ptr"])
	}
}

func TestImageLookupDueOnlyWhileARunWillRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *testEnv, id int64)
		want  bool
	}{
		{"scheduled", func(*testEnv, int64) {}, true},
		{"phase off", func(env *testEnv, _ int64) { env.cfg.Schedule.LookupBooru = false }, false},
		{"gallery left out", func(env *testEnv, _ int64) {
			env.cfg.Schedule.Galleries = map[string][]string{config.ActionLookupBooru: {}}
		}, false},
		{"unpaired", func(env *testEnv, _ int64) { env.cfg.Auth.Tokens = env.cfg.Auth.Tokens[:1] }, false},
		{"opted out", func(env *testEnv, id int64) {
			mustExec(t, env.database.Write, `UPDATE images SET scheduled_lookup = 0 WHERE id = ?`, id)
		}, false},
		{"sourced", func(env *testEnv, id int64) {
			mustExec(t, env.database.Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'danbooru', '1', 'https://d.example/1')`, id)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			pairScheduledLookups(env)
			id := env.createTestImage(t, "due.png", 10, 10)
			if err := lookup.Record(env.database, id, contract.LookupBooru, lookup.ResultMiss, 0, time.Now()); err != nil {
				t.Fatal(err)
			}
			tc.setup(env, id)

			w := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
			mustCode(t, w, http.StatusOK)
			var resp map[string]any
			mustDecode(t, w, &resp)
			entry, _ := resp["lookup"].(map[string]any)
			booru, _ := entry["booru"].(map[string]any)
			if booru["last_result"] != "miss" {
				t.Fatalf("lookup.booru = %v, want the miss kept", booru)
			}
			if _, got := booru["next_due_at"]; got != tc.want {
				t.Errorf("lookup.booru = %v, want next_due_at present = %v", booru, tc.want)
			}
		})
	}
}

func TestEnrichImage_RecordsWhatThePostServes(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "postfacts.png", 12, 12)

	enrich := func(body map[string]any) { env.enrichOK(t, id, body) }
	read := func() (w, h int, size int64, ext string) {
		if err := env.database.Read.QueryRow(
			`SELECT post_width, post_height, post_size, post_ext FROM image_sources WHERE image_id = ?`, id,
		).Scan(&w, &h, &size, &ext); err != nil {
			t.Fatal(err)
		}
		return
	}

	enrich(map[string]any{
		"source": "danbooru", "post_id": "5", "url": "https://danbooru.donmai.us/posts/5",
		"post_width": 2894, "post_height": 4093, "post_size": 3200000, "post_ext": "PNG",
	})
	if w, h, size, ext := read(); w != 2894 || h != 4093 || size != 3200000 || ext != "png" {
		t.Fatalf("stored post file = %dx%d %s %d, want 2894x4093 png 3200000", w, h, ext, size)
	}

	enrich(map[string]any{
		"source": "danbooru", "post_id": "5", "url": "https://danbooru.donmai.us/posts/5",
	})
	if w, _, _, ext := read(); w != 2894 || ext != "png" {
		t.Errorf("after a silent refetch = %d %s, want the stored facts kept", w, ext)
	}
}

func TestEnrichImage_ConvertsNoteHTML(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_html.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"source": "danbooru",
		"url":    "https://danbooru.donmai.us/posts/5",
		"notes": []map[string]any{{
			"x": 1, "y": 2, "w": 3, "h": 4,
			"body":      "Lapis Lazuli",
			"body_html": `<b><a href="/posts?tags=lapis_lazuli">Lapis Lazuli</a></b>`,
		}},
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)

	var stored string
	if err := env.database.Read.QueryRow(
		`SELECT body FROM image_annotations WHERE image_id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	want := "[b][tag=lapis_lazuli]Lapis Lazuli[/tag][/b]"
	if stored != want {
		t.Fatalf("stored = %q, want %q", stored, want)
	}

	w2 := env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantIn(t, w2.Body.String(), `"body_text":"Lapis Lazuli"`)
}

func TestEnrichImage_ConvertsCommentaryDText(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_dtext.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"source":                      "danbooru",
		"url":                         "https://danbooru.donmai.us/posts/5",
		"commentary":                  "Title \"pixiv\":[https://www.pixiv.net/artworks/1]",
		"commentary_dtext":            "[b]Title[/b] \"pixiv\":[https://www.pixiv.net/artworks/1]",
		"commentary_translated_dtext": "[quote]a note[/quote]",
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusOK)

	var stored, translated string
	if err := env.database.Read.QueryRow(
		`SELECT commentary, commentary_translated FROM image_sources WHERE image_id = ?`, id).
		Scan(&stored, &translated); err != nil {
		t.Fatal(err)
	}

	want := "[b]Title[/b] [url=https://www.pixiv.net/artworks/1]pixiv[/url]"
	if stored != want {
		t.Fatalf("stored commentary = %q, want %q", stored, want)
	}
	if translated != "a note" {
		t.Fatalf("stored translation = %q, want %q", translated, "a note")
	}
}

func TestEnrichImage_DTextSupersedesAnOversizedPlainCommentary(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_dtext_plain_big.png", 12, 12)
	provenance := map[string]any{
		"source":           "danbooru",
		"url":              "https://danbooru.donmai.us/posts/6",
		"commentary":       strings.Repeat("x", contract.MaxCommentary+1),
		"commentary_dtext": "[b]short[/b]",
	}
	body, _ := json.Marshal(provenance)
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body), http.StatusOK)

	provenance["path"] = env.writePNG(t, "create_dtext_plain_big.png", 9, 9)
	created := createImageJSON(t, env, provenance, http.StatusCreated)

	for _, imageID := range []int64{id, int64(created["id"].(float64))} {
		var stored string
		if err := env.database.Read.QueryRow(
			`SELECT commentary FROM image_sources WHERE image_id = ?`, imageID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != "[b]short[/b]" {
			t.Errorf("image %d commentary = %q, want the DText's conversion", imageID, stored)
		}
	}
}

func TestReplaceImageFile_ConvertsCommentaryDText(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "replace_dtext.png", 12, 12)

	w := env.postReplace(t, id, "replaced.png", pngFileBytes(t, 14, 14), map[string]string{
		"source":                      "danbooru",
		"url":                         "https://danbooru.donmai.us/posts/9",
		"commentary":                  "Title \"pixiv\":[https://www.pixiv.net/artworks/1]",
		"commentary_dtext":            "[b]Title[/b] \"pixiv\":[https://www.pixiv.net/artworks/1]",
		"commentary_translated_dtext": "[quote]a note[/quote]",
	})
	mustCode(t, w, http.StatusOK)

	var stored, translated string
	if err := env.database.Read.QueryRow(
		`SELECT commentary, commentary_translated FROM image_sources WHERE image_id = ?`, id).
		Scan(&stored, &translated); err != nil {
		t.Fatal(err)
	}
	if want := "[b]Title[/b] [url=https://www.pixiv.net/artworks/1]pixiv[/url]"; stored != want {
		t.Fatalf("stored commentary = %q, want %q", stored, want)
	}
	if translated != "a note" {
		t.Fatalf("stored translation = %q, want %q", translated, "a note")
	}
}

func TestEnrichImage_RefusesOversizedCommentaryDText(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_dtext_big.png", 12, 12)

	body, _ := json.Marshal(map[string]any{
		"source":           "danbooru",
		"url":              "https://danbooru.donmai.us/posts/7",
		"commentary_dtext": strings.Repeat("a", contract.MaxDText+1),
	})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "commentary_dtext")
}

func TestEnrichImage_CommentaryWithoutDText(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "enrich_plain.png", 12, 12)
	env.enrichOK(t, id, map[string]any{
		"source":     "danbooru",
		"url":        "https://danbooru.donmai.us/posts/6",
		"commentary": "  plain words  ",
	})
	var stored string
	if err := env.database.Read.QueryRow(
		`SELECT commentary FROM image_sources WHERE image_id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "plain words" {
		t.Fatalf("stored = %q, want %q", stored, "plain words")
	}
}

func (e *testEnv) enrichOK(t *testing.T, id int64, payload map[string]any) {
	t.Helper()
	enrich(t, e, id, payload, http.StatusOK)
}

func TestCORSAllowed(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BaseURL = "http://booru.example:8455"
	cfg.Server.CORSOrigins = []string{"moz-extension://abcdef"}

	req := func(host string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/images/search", nil)
		r.Host = host
		return r
	}
	for _, tc := range []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{"the address it arrived on", "192.168.1.5:8455", "http://192.168.1.5:8455", true},
		{"loopback by name", "localhost:8455", "http://localhost:8455", true},
		{"an ipv6 literal", "[::1]:8455", "http://[::1]:8455", true},
		{"the configured base url", "192.168.1.5:8455", "http://booru.example:8455", true},
		{"a listed extra origin", "192.168.1.5:8455", "moz-extension://abcdef", true},
		{"a forged host vouching for itself", "evil.example", "http://evil.example", false},
		{"an unrelated origin", "192.168.1.5:8455", "http://evil.example", false},
	} {
		if got := corsAllowed(cfg, req(tc.host), tc.origin); got != tc.want {
			t.Errorf("%s: corsAllowed(host %q, origin %q) = %v, want %v", tc.name, tc.host, tc.origin, got, tc.want)
		}
	}

	cfg.Server.CORSOrigins = []string{"*"}
	if !corsAllowed(cfg, req("192.168.1.5:8455"), "http://anything.example") {
		t.Error(`a "*" entry must allow any origin`)
	}
}

func TestCommentaryFromInput(t *testing.T) {
	for _, tc := range []struct{ name, plain, dtext, want string }{
		{"no dtext", "  plain words  ", "", "plain words"},
		{"blank dtext", "plain words", "   ", "plain words"},
		{"dtext wins", "plain words", "[b]bold[/b]", "[b]bold[/b]"},
		{"dtext converts", "", `"pixiv":[https://example.com/a]`, "[url=https://example.com/a]pixiv[/url]"},
	} {
		if got := commentaryFromInput(tc.plain, tc.dtext); got != tc.want {
			t.Errorf("%s: commentaryFromInput(%q, %q) = %q, want %q", tc.name, tc.plain, tc.dtext, got, tc.want)
		}
	}

	long := strings.Repeat("é", contract.MaxCommentary+50)
	if got := []rune(commentaryFromInput("", long)); len(got) != contract.MaxCommentary {
		t.Errorf("stored %d runes, want the %d-rune cap", len(got), contract.MaxCommentary)
	}
}

func TestAnnotationsFromInput_CapsBodyHTMLBeforeConverting(t *testing.T) {

	src := strings.Repeat("<!---->", maxAnnotationBodyHTMLLen/7+1) + "tail"
	got := annotationsFromInput([]contract.NoteBox{{W: 1, H: 1, BodyHTML: src}}, "")
	if len(got) != 1 {
		t.Fatalf("notes = %d, want 1", len(got))
	}
	if got[0].Body != "" {
		t.Errorf("body = %q, want the HTML cut before its tail", got[0].Body)
	}
	if got := firstRunes("aé😀b", 3); got != "aé😀" {
		t.Errorf("firstRunes = %q, want whole characters", got)
	}
}

func TestAPITextLimitsCountCharactersNotBytes(t *testing.T) {
	env := newTestEnv(t)
	kanji := func(n int) string { return strings.Repeat("髪", n) }
	prefix := "https://example.com/"
	resp := createImageJSON(t, env, map[string]any{
		"path":       env.writePNG(t, "caps.png", 9, 9),
		"via":        kanji(200),
		"source":     kanji(contract.MaxSourceLabel),
		"url":        prefix + kanji(contract.MaxURL-len(prefix)),
		"commentary": kanji(contract.MaxCommentary),
	}, http.StatusCreated)
	id := int64(resp["id"].(float64))
	enrich(t, env, id, map[string]any{"source": "danbooru", "commentary": kanji(contract.MaxCommentary + 1)}, http.StatusBadRequest)
	createImageJSON(t, env, map[string]any{"path": env.writePNG(t, "caps2.png", 10, 10), "via": kanji(201)}, http.StatusBadRequest)
}

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func mustDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
}

func mustEncodePNG(t *testing.T, w io.Writer, m image.Image) {
	t.Helper()
	if err := png.Encode(w, m); err != nil {
		t.Fatalf("png encode: %v", err)
	}
}

func mustCreate(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	return f
}

func mustIngest(t *testing.T, database *db.DB, galleryPath, thumbnailsPath, path, origin string) (*models.Image, bool) {
	t.Helper()
	img, dup, err := gallery.Ingest(gallery.Handle{GalleryPath: galleryPath, ThumbnailsPath: thumbnailsPath, DB: database, TagSvc: tags.New(database, nil)}, path, origin)
	if err != nil {
		t.Fatalf("Ingest %s: %v", path, err)
	}
	return img, dup
}

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestRemoveImageTags_ResolvesAnAliasAndTheCase(t *testing.T) {
	env := newTestEnv(t)
	genID := categoryID(t, env, "general")
	svc := tags.New(env.database, nil)
	canon, _ := svc.GetOrCreateTag("blue_sky", genID)
	if _, err := svc.CreateAlias("azure_sky", genID, canon.ID); err != nil {
		t.Fatal(err)
	}

	for i, token := range []string{"azure_sky", "Blue_Sky", "general:Azure_Sky", "general:BLUE_SKY"} {
		id := env.createTestImage(t, fmt.Sprintf("alias_rm_%d.png", i), 10+i, 10)
		if err := svc.AddTagToImage(id, canon.ID, false, nil); err != nil {
			t.Fatal(err)
		}
		remBody, _ := json.Marshal(map[string]any{"tags": []string{token}})
		w := doJSON(env.mux, "DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), remBody)
		mustCode(t, w, http.StatusOK)
		if n, _ := findImageTag(t, env, id, "blue_sky"); n != "" {
			t.Errorf("%s: blue_sky is still on the image", token)
		}
	}
}

func paddedPNG(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	mustEncodePNG(t, &buf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	raw := buf.Bytes()
	pad := size - len(raw) - 12
	if pad < 0 {
		t.Fatalf("size %d is under the bare PNG's %d", size, len(raw))
	}
	chunk := binary.BigEndian.AppendUint32(nil, uint32(pad))
	chunk = append(chunk, "paDd"...)
	chunk = append(chunk, make([]byte, pad)...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	iend := len(raw) - 12
	return append(append(append([]byte{}, raw[:iend]...), chunk...), raw[iend:]...)
}

func TestJSONBodyOverTheCapIsRefused(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "big-body.png", 8, 8)
	body, _ := json.Marshal(map[string]any{"source": "danbooru", "commentary": strings.Repeat("a", 17<<20)})

	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)

	wantCode(t, w, http.StatusRequestEntityTooLarge)
	wantIn(t, w.Body.String(), `"file_too_large"`)
}

func TestFetchStatusMessageIsCut(t *testing.T) {
	env := newTestEnv(t)
	var stored string
	env.handler.WithFetchStatus(func(_ string, _ int64, _, message string) { stored = message })
	id := env.createTestImage(t, "status.png", 8, 8)
	body, _ := json.Marshal(map[string]any{"state": "error", "message": strings.Repeat("é", 5000)})

	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/fetch-status", id), body)

	mustCode(t, w, http.StatusOK)
	if n := utf8.RuneCountInString(stored); n != 1000 {
		t.Errorf("stored message holds %d characters, want 1000", n)
	}
}

func TestPushNearTheFileCapWithFullMetadataLands(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.MaxFileSizeMB = 1

	w := env.postUpload(t, "near-cap.png", paddedPNG(t, 1<<20-1024), map[string]string{
		"source":     "danbooru",
		"commentary": strings.Repeat("あ", 10000),
	})

	wantCode(t, w, http.StatusCreated)
}

func TestReplaceImageFile_RecordsRefusalOnFetchStatus(t *testing.T) {
	env := newTestEnv(t)
	var gotState, gotMsg string
	env.handler.WithFetchStatus(func(_ string, _ int64, state, msg string) { gotState, gotMsg = state, msg })
	id, _ := mustExec(t, env.database.Write, `INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at) VALUES (?, 'mp4', 10, 'sha-video', datetime('now'))`,
		filepath.Join(env.galleryDir, "clip.mp4")).LastInsertId()

	w := env.postRawMultipart(t, fmt.Sprintf("/api/v1/images/%d/file", id), paddedPNG(t, 200), 0)

	wantCode(t, w, http.StatusConflict)
	wantIn(t, w.Body.String(), `"wrong_type"`)
	if gotState != "error" || gotMsg != "this file type cannot be replaced" {
		t.Errorf("fetch status = (%q, %q), want the refusal", gotState, gotMsg)
	}
}

func TestCreateImage_ARefusedNameCreatesNoFolder(t *testing.T) {
	env := newTestEnv(t)
	env.bounds.Store(gallery.NewBoundary(env.galleryDir, nil, []string{"._*"}))

	w := env.postUpload(t, "._x.png", paddedPNG(t, 200), map[string]string{"folder": "newdir"})

	mustCode(t, w, http.StatusBadRequest)
	if _, err := os.Stat(filepath.Join(env.galleryDir, "newdir")); !os.IsNotExist(err) {
		t.Errorf("the refused push left its folder behind: %v", err)
	}
}

func TestListImages_CarriesAliasesAndTaggerName(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "carrier.png", 8, 8)
	for _, q := range []string{
		`INSERT INTO image_paths (image_id, path) VALUES (?1, 'copies/b.png'), (?1, 'copies/a.png')`,
		`INSERT INTO tags (name, category_id) SELECT 'red_hair', id FROM tag_categories WHERE name = 'general'`,
		`INSERT INTO tags (name, category_id) SELECT 'sky', id FROM tag_categories WHERE name = 'general'`,
		`INSERT INTO image_tags (image_id, tag_id, is_auto, confidence, tagger_name) SELECT ?1, id, 1, 0.75, 'wd' FROM tags WHERE name = 'red_hair'`,
		`INSERT INTO image_tags (image_id, tag_id) SELECT ?1, id FROM tags WHERE name = 'sky'`,
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source) SELECT ?1, id, 'wd' FROM tags WHERE name = 'red_hair'`,
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source) SELECT ?1, id, 'user' FROM tags WHERE name = 'red_hair'`,
	} {
		mustExec(t, env.database.Write, q, id)
	}
	type tagJSON struct {
		Name       string   `json:"name"`
		Category   string   `json:"category"`
		IsAuto     bool     `json:"is_auto"`
		Confidence *float64 `json:"confidence"`
		TaggerName *string  `json:"tagger_name"`
	}
	type imageJSON struct {
		Aliases    []string            `json:"aliases"`
		Tags       []tagJSON           `json:"tags"`
		TagSources map[string][]string `json:"tag_sources"`
	}
	check := func(where string, img imageJSON) {
		t.Helper()
		if !slices.Equal(img.Aliases, []string{"copies/b.png", "copies/a.png"}) {
			t.Errorf("%s aliases = %v, want both copies in row order", where, img.Aliases)
		}
		byName := map[string]tagJSON{}
		for _, tg := range img.Tags {
			byName[tg.Name] = tg
		}
		auto, manual := byName["red_hair"], byName["sky"]
		if !auto.IsAuto || auto.Confidence == nil || *auto.Confidence != 0.75 || auto.TaggerName == nil || *auto.TaggerName != "wd" {
			t.Errorf("%s red_hair = %+v, want auto 0.75 by wd", where, auto)
		}
		if manual.Name != "sky" || manual.IsAuto || manual.Confidence != nil || manual.TaggerName != nil {
			t.Errorf("%s sky = %+v, want a manual row with null confidence and tagger", where, manual)
		}
	}

	w := env.do("GET", "/api/v1/images/search")
	wantCode(t, w, http.StatusOK)
	var page struct {
		Results []imageJSON `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Results) != 1 {
		t.Fatalf("search: %v, %d results\n%s", err, len(page.Results), w.Body.String())
	}
	check("search", page.Results[0])

	w = env.do("GET", fmt.Sprintf("/api/v1/images/%d", id))
	wantCode(t, w, http.StatusOK)
	var one imageJSON
	mustDecode(t, w, &one)
	check("get", one)
	if got := one.TagSources["red_hair"]; !slices.Equal(got, []string{"user", "wd"}) && !slices.Equal(got, []string{"wd", "user"}) {
		t.Errorf("tag_sources[red_hair] = %v, want wd and user", got)
	}
}
