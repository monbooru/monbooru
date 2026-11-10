package api

import (
	"archive/zip"
	"bytes"
	"image"
	"image/png"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

func (e *testEnv) createTestManga(t *testing.T, name string) int64 {
	t.Helper()
	pic := image.NewRGBA(image.Rect(0, 0, 8, 8))
	cbzPath := filepath.Join(e.galleryDir, name)
	f := mustCreate(t, cbzPath)
	zw := zip.NewWriter(f)
	w, err := zw.Create("01.png")
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := png.Encode(w, pic); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	rec, _ := mustIngest(t, e.database, e.galleryDir, e.thumbDir, cbzPath, "")
	return rec.ID
}

func TestAPI_PostImages_AcceptsCBZUpload(t *testing.T) {
	env := newTestEnv(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 1; i <= 3; i++ {
		w, _ := zw.Create("page_" + strconv.Itoa(i) + ".png")
		pic := image.NewRGBA(image.Rect(0, 0, 4, 4))
		mustEncodePNG(t, w, pic)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	rec := env.postUpload(t, "test.cbz", buf.Bytes(), nil)

	mustCode(t, rec, http.StatusCreated)
	for _, frag := range []string{
		`"file_type":"cbz"`,
		`"page_count":3`,
	} {
		wantIn(t, rec.Body.String(), frag)
	}
}

func TestAPI_AddTags_AcceptsMangaID(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "m.cbz")
	rec := doJSON(env.mux, "POST", "/api/v1/images/"+strconv.FormatInt(id, 10)+"/tags", []byte(`{"tags":["manga_demo"]}`))
	wantCode(t, rec, http.StatusOK)
}

func TestAPI_RemoveTags_AcceptsMangaID(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "m.cbz")
	rec := doJSON(env.mux, "DELETE", "/api/v1/images/"+strconv.FormatInt(id, 10)+"/tags", []byte(`{"tags":["1girl"]}`))
	wantCode(t, rec, http.StatusOK)
}

func TestAPI_GetImage_ReturnsMangaFields(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "m.cbz")
	rec := env.do("GET", "/api/v1/images/"+strconv.FormatInt(id, 10))
	mustCode(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, frag := range []string{
		`"file_type":"cbz"`,
		`"page_count":1`,
		`"collection":""`,
	} {
		wantIn(t, body, frag)
	}
}
