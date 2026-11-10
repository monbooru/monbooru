package api

import (
	"bytes"
	"encoding/json"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pushImage(t *testing.T, env *testEnv, filename string, fields map[string]string) map[string]any {
	t.Helper()
	var imgBuf bytes.Buffer
	mustEncodePNG(t, &imgBuf, image.NewRGBA(image.Rect(0, 0, 9, 11)))

	imgBuf.WriteString(filename)

	w := env.postUpload(t, filename, imgBuf.Bytes(), fields)
	if w.Code != http.StatusCreated {
		t.Fatalf("push %q: got %d: %s", filename, w.Code, w.Body.String())
	}
	var out map[string]any
	mustDecode(t, w, &out)
	return out
}

func TestCreateImage_HonoursUploadDestination(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.DefaultUploadFolder = "inbox/{year}"
	env.cfg.Gallery.DefaultUploadName = "{source}-{post_id}"

	out := pushImage(t, env, "pushed.png", map[string]string{
		"source":  "danbooru",
		"post_id": "4821993",
		"url":     "https://danbooru.donmai.us/posts/4821993",
	})
	got, _ := out["canonical_path"].(string)
	want := filepath.Join(env.galleryDir, "inbox", time.Now().UTC().Format("2006"), "danbooru-4821993.png")
	if got != want {
		t.Errorf("canonical_path = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at the applied path: %v", err)
	}
}

func TestCreateImage_RequestFolderWinsOverTheSetting(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.DefaultUploadFolder = "inbox/{year}"
	env.cfg.Gallery.DefaultUploadName = ""

	out := pushImage(t, env, "explicit.png", map[string]string{"folder": "pushed"})
	got, _ := out["canonical_path"].(string)
	want := filepath.Join(env.galleryDir, "pushed", "explicit.png")
	if got != want {
		t.Errorf("canonical_path = %q, want %q", got, want)
	}
}

func TestCreateImage_PathReferenceKeepsItsPath(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.Gallery.DefaultUploadFolder = "inbox/{year}"
	env.cfg.Gallery.DefaultUploadName = "{id}"

	placed := filepath.Join(env.galleryDir, "placed.png")
	mustWriteFile(t, placed, pngFileBytes(t, 13, 7), 0o644)

	body, _ := json.Marshal(map[string]any{"path": placed})
	w := doJSON(env.mux, "POST", "/api/v1/images", body)
	mustCode(t, w, http.StatusCreated)
	var out map[string]any
	mustDecode(t, w, &out)
	if got, _ := out["canonical_path"].(string); got != placed {
		t.Errorf("canonical_path = %q, want %q", got, placed)
	}
}
