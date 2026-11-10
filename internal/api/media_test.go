package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

func TestServeImageFile(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "bytes.png", 12, 12)

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/file", id))
	mustCode(t, w, http.StatusOK)
	if w.Body.Len() == 0 {
		t.Error("file: expected non-empty body")
	}

	w = env.do("GET", "/api/v1/images/99999/file")
	wantCode(t, w, http.StatusNotFound)
}

func TestServeImageFile_NamesTheDownload(t *testing.T) {
	env := newTestEnv(t)
	cbzID := env.createTestManga(t, "vol 1.cbz")
	pngID := env.createTestImage(t, "café.png", 12, 12)

	cases := []struct {
		id   int64
		want string
	}{
		{cbzID, `inline; filename="vol 1.cbz"`},
		{pngID, "inline; filename*=utf-8''caf%C3%A9.png"},
	}
	for _, tc := range cases {
		w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/file", tc.id))
		if w.Code != http.StatusOK {
			t.Fatalf("file id=%d: expected 200, got %d: %s", tc.id, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Disposition"); got != tc.want {
			t.Errorf("file id=%d: Content-Disposition = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func (e *testEnv) createTestStill(t *testing.T, name string, w, h int) int64 {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	codec := map[string]string{".avif": "libaom-av1", ".jxl": "libjxl"}[filepath.Ext(name)]
	path := filepath.Join(e.galleryDir, name)
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d", w, h),
		"-frames:v", "1", "-c:v", codec, "--", path)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg cannot encode %s: %v", codec, err)
	}
	record, _ := mustIngest(t, e.database, e.galleryDir, e.thumbDir, path, "")
	return record.ID
}

func TestServeImageStill_AnswersADecodableImageAsIs(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "plain.png", 12, 12)

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/still", id))
	mustCode(t, w, http.StatusOK)
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	want, err := os.ReadFile(filepath.Join(env.galleryDir, "plain.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Error("body differs from the file")
	}
}

func TestServeImageStill_RendersAVIFAndJPEGXLToFullSizeWebP(t *testing.T) {
	for _, name := range []string{"wide.avif", "wide.jxl"} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			id := env.createTestStill(t, name, 4100, 64)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)

			w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/still", id))
			mustCode(t, w, http.StatusOK)
			if ct := w.Header().Get("Content-Type"); ct != "image/webp" {
				t.Errorf("Content-Type = %q, want image/webp", ct)
			}
			cfg, err := webp.DecodeConfig(w.Body)
			if err != nil || cfg.Width != 4100 || cfg.Height != 64 {
				t.Errorf("body = %dx%d (%v), want a 4100x64 WebP", cfg.Width, cfg.Height, err)
			}
			if left, _ := os.ReadDir(tmp); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

func TestServeImageStill_RefusesAnArchiveAndAMissingRow(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "vol.cbz")

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/still", id))
	wantCode(t, w, http.StatusNotFound)

	w = env.do("GET", "/api/v1/images/99999/still")
	wantCode(t, w, http.StatusNotFound)
}

func TestServeThumbnail(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "thumb.png", 12, 12)

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/thumbnail", id))
	mustCode(t, w, http.StatusOK)
	if w.Body.Len() == 0 {
		t.Error("thumbnail: expected non-empty body")
	}

	w = env.do("GET", "/api/v1/images/99999/thumbnail")
	wantCode(t, w, http.StatusNotFound)
}

func TestServeMangaPage(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "pages.cbz")

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/page/1", id))
	mustCode(t, w, http.StatusOK)
	if w.Body.Len() == 0 {
		t.Error("page 1: expected non-empty body")
	}

	w = env.do("GET", fmt.Sprintf("/api/v1/images/%d/page/1/thumb", id))
	mustCode(t, w, http.StatusOK)

	w = env.do("GET", fmt.Sprintf("/api/v1/images/%d/page/0", id))
	wantCode(t, w, http.StatusBadRequest)
}

func TestServeMangaPage_NotManga(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "still.png", 10, 10)

	w := env.do("GET", fmt.Sprintf("/api/v1/images/%d/page/1", id))
	wantCode(t, w, http.StatusNotFound)
}

func TestImageRoutesAnswer500WhenTheDatabaseFails(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "held.png", 8, 8)
	if err := env.database.Read.Close(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/v1/images/%d", ""},
		{"PATCH", "/api/v1/images/%d", `{"favorite":true}`},
		{"POST", "/api/v1/images/%d/tags", `{"tags":["red"]}`},
		{"GET", "/api/v1/images/%d/file", ""},
		{"GET", "/api/v1/images/%d/thumbnail", ""},
		{"GET", "/api/v1/images/%d/relations", ""},
		{"POST", "/api/v1/relations", `{"a":%[1]d,"b":%[1]d,"type":"duplicate"}`},
	} {
		path, body := route.path, route.body
		if strings.Contains(path, "%d") {
			path = fmt.Sprintf(path, id)
		} else {
			body = fmt.Sprintf(body, id)
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(route.method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		env.mux.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"internal_error"`) {
			t.Errorf("%s %s = %d %s, want 500 internal_error", route.method, path, w.Code, w.Body.String())
		}
	}
}

func TestServeImageFileAnswersJSONWhenTheFileIsGone(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "gone.png", 8, 8)
	if err := os.Remove(filepath.Join(env.galleryDir, "gone.png")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/images/%d/file", "/api/v1/images/%d/still"} {
		w := env.do("GET", fmt.Sprintf(path, id))
		if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"not_found"`) {
			t.Errorf("%s = %d %q %s, want a JSON 404", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
}
