package web

import (
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestPromoteCanonical_MissingFileFlashesForHTMX(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	imgPath := filepath.Join(cx.GalleryPath, "pc.png")
	mustWriteFile(t, imgPath, tinyPNG(t, 8, 8, color.RGBA{1, 2, 3, 255}), 0o644)
	rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, imgPath, "")
	gone := filepath.Join(cx.GalleryPath, "gone.png")
	mustExec(t, cx.DB.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, rec.ID, gone)

	form := url.Values{"path": {gone}}
	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(rec.ID, 10)+"/canonical-path", form)

	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Header().Get("HX-Trigger"), "action-flash")
	if canon := canonicalPathOf(t, srv, rec.ID); canon != imgPath {
		t.Errorf("canonical_path = %q, want unchanged %q", canon, imgPath)
	}
}

func TestSetNote_MissingImage404s(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"note": {"hello"}}
	w := postForm(t, srv, "/images/999999/note", form)

	mustCode(t, w, http.StatusNotFound)
}

func TestRemoveSourceCommentary_MissingSite400s(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "commentary.png", 6, 6)
	w := postForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/commentary/remove", url.Values{})
	mustCode(t, w, http.StatusBadRequest)
}

func TestImageEdits_UnknownImageOrTarget404(t *testing.T) {
	srv := newTestServer(t)
	id := strconv.FormatInt(seedImage(t, srv, "origin.png", 6, 6), 10)
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/images/999999/sources/set", url.Values{"site": {"danbooru"}}},
		{"/images/999999/commentary/set", url.Values{"site": {"danbooru"}, "commentary": {"hi"}}},
		{"/images/" + id + "/sources/keep", url.Values{"site": {"nowhere"}, "keep": {"1"}}},
		{"/images/" + id + "/sources/remove", url.Values{"site": {"nowhere"}}},
		{"/images/" + id + "/sources/primary", url.Values{"site": {"nowhere"}}},
		{"/images/" + id + "/annotations/remove", url.Values{"id": {"999999"}}},
	} {
		if w := postForm(t, srv, c.path, c.form); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", c.path, w.Code)
		}
	}
}

func TestAnnotationWrites_StayOnTheURLImage(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "own.png", 20, 20)
	b := seedImage(t, srv, "other.png", 21, 21)
	if err := gallery.AddManualAnnotation(srv.db(), a, 1, 1, 5, 5, "a's box"); err != nil {
		t.Fatal(err)
	}
	anns, err := gallery.AnnotationsForImage(srv.db(), a)
	if err != nil || len(anns) != 1 {
		t.Fatalf("seeded boxes = %v (%v)", anns, err)
	}
	box := strconv.FormatInt(anns[0].ID, 10)
	onB := "/images/" + strconv.FormatInt(b, 10)
	if w := postForm(t, srv, onB+"/annotations/set", url.Values{"id": {box}, "x": {"0"}, "y": {"0"}, "w": {"3"}, "h": {"3"}, "body": {"moved"}}); w.Code != http.StatusNotFound {
		t.Errorf("set through another image = %d, want 404", w.Code)
	}
	if w := postForm(t, srv, onB+"/annotations/remove", url.Values{"id": {box}}); w.Code != http.StatusNotFound {
		t.Errorf("remove through another image = %d, want 404", w.Code)
	}
	if after, _ := gallery.AnnotationsForImage(srv.db(), a); len(after) != 1 || after[0].Body != "a's box" || after[0].W != 5 {
		t.Errorf("a's boxes = %+v, want its box untouched", after)
	}
}

func TestChangeTagCategory_UnknownCategory400s(t *testing.T) {
	srv := newTestServer(t)
	tag := mustTag(t, srv.tagSvc(), "mover", 1)
	w := sendForm(t, srv, "PATCH", "/tags/"+strconv.FormatInt(tag.ID, 10)+"/category", url.Values{"category_id": {"999999"}})
	mustCode(t, w, http.StatusBadRequest)
}

func TestSetNote_PersistsAndFlashes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "note1.png", 6, 6)

	form := url.Values{"note": {"keep this"}}
	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/note", form)
	mustCode(t, w, http.StatusNoContent)
	var note string
	if err := srv.active().DB.Read.QueryRow(`SELECT note FROM images WHERE id = ?`, id).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "keep this" {
		t.Errorf("note = %q, want %q", note, "keep this")
	}
}

func TestTextLimitsCountCharactersNotBytes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "caps.png", 20, 20)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	base := "/images/" + strconv.FormatInt(id, 10)
	for _, c := range []struct {
		path, field string
		max         int
		extra       url.Values
	}{
		{"/note", "note", maxImageNoteLen, nil},
		{"/commentary/set", "commentary", contract.MaxCommentary, url.Values{"site": {"danbooru"}}},
		{"/annotations/set", "body", maxAnnotationBodyLen, url.Values{"x": {"1"}, "y": {"1"}, "w": {"5"}, "h": {"5"}}},
	} {
		post := func(n int) *httptest.ResponseRecorder {
			form := url.Values{c.field: {strings.Repeat("髪", n)}}
			for k, v := range c.extra {
				form[k] = v
			}
			return postHTMXForm(t, srv, base+c.path, form)
		}
		if w := post(c.max); w.Code != http.StatusNoContent {
			t.Errorf("%s with %d characters: %d %s", c.path, c.max, w.Code, w.Body.String())
		}
		if w := post(c.max + 1); w.Code == http.StatusNoContent {
			t.Errorf("%s with %d characters was accepted", c.path, c.max+1)
		}
	}
}

func TestLabelLimitsCountCharactersNotBytes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "labels.png", 12, 12)
	page := tinyPNG(t, 8, 8, color.RGBA{9, 9, 9, 255})
	manga := seedManga(t, srv, "labels.cbz", [][]byte{page})
	kanji := func(n int) string { return strings.Repeat("髪", n) }
	img := "/images/" + strconv.FormatInt(id, 10)
	prefix := "https://a.example/"
	for _, c := range []struct {
		name string
		max  int
		post func(n int) *httptest.ResponseRecorder
	}{
		{"source label", contract.MaxSourceLabel, func(n int) *httptest.ResponseRecorder {
			return postHTMXForm(t, srv, img+"/sources/set", url.Values{"site": {kanji(n)}})
		}},
		{"source url", contract.MaxURL, func(n int) *httptest.ResponseRecorder {
			return postHTMXForm(t, srv, img+"/sources/set", url.Values{"url": {prefix + kanji(n-len(prefix))}})
		}},
		{"collection", contract.MaxCollection, func(n int) *httptest.ResponseRecorder {
			return postHTMXForm(t, srv, img+"/collections/set", url.Values{"collection": {kanji(n)}})
		}},
		{"batch collection", contract.MaxCollection, func(n int) *httptest.ResponseRecorder {
			return postForm(t, srv, "/internal/batch-collection", url.Values{"scope": {"selection"}, "ids": {strconv.FormatInt(id, 10)}, "collection": {kanji(n)}})
		}},
		{"collection rename", contract.MaxCollection, func(n int) *httptest.ResponseRecorder {
			return postForm(t, srv, "/collections/rename", url.Values{"prev": {"Old"}, "name": {kanji(n)}})
		}},
		{"generate collection", contract.MaxCollection, func(n int) *httptest.ResponseRecorder {
			return postForm(t, srv, "/images/"+strconv.FormatInt(manga, 10)+"/generate-collection", url.Values{"collection": {kanji(n)}})
		}},
	} {
		if w := c.post(c.max); strings.Contains(w.Body.String(), "too long") {
			t.Errorf("%s with %d characters was refused: %s", c.name, c.max, w.Body.String())
		}
		awaitJobsDrain(t, srv)
		if w := c.post(c.max + 1); !strings.Contains(w.Body.String(), "too long") {
			t.Errorf("%s with %d characters: %d %s", c.name, c.max+1, w.Code, w.Body.String())
		}
	}
}

func TestAnnotationHandlers_AddEditRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ann.png", 100, 80)
	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		return postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/annotations/set", form)
	}

	if w := post(url.Values{"x": {"5"}, "y": {"6"}, "w": {"10"}, "h": {"12"}, "body": {"hello"}}); w.Code != http.StatusNoContent {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	anns, _ := gallery.AnnotationsForImage(srv.db(), id)
	if len(anns) != 1 || !anns[0].Manual || anns[0].Body != "hello" {
		t.Fatalf("want one manual box, got %+v", anns)
	}
	annID := anns[0].ID

	if w := post(url.Values{"id": {strconv.FormatInt(annID, 10)}, "x": {"1"}, "y": {"2"}, "w": {"3"}, "h": {"4"}, "body": {"edited"}}); w.Code != http.StatusNoContent {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	if anns, _ = gallery.AnnotationsForImage(srv.db(), id); anns[0].Body != "edited" || anns[0].X != 1 {
		t.Fatalf("edit did not apply: %+v", anns)
	}

	post(url.Values{"x": {"200"}, "y": {"0"}, "w": {"10"}, "h": {"10"}})
	post(url.Values{"x": {"-1"}, "y": {"0"}, "w": {"10"}, "h": {"10"}})
	if anns, _ := gallery.AnnotationsForImage(srv.db(), id); len(anns) != 1 {
		t.Errorf("invalid boxes must be rejected; want 1 box, got %d", len(anns))
	}

	rem := url.Values{"id": {strconv.FormatInt(annID, 10)}}
	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/annotations/remove", rem)
	mustCode(t, w, http.StatusNoContent)
	if anns, _ := gallery.AnnotationsForImage(srv.db(), id); len(anns) != 0 {
		t.Fatalf("want 0 after remove, got %d", len(anns))
	}
}

func TestAnnotationHandler_DimensionlessAccepts(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "dim.png", 10, 10)
	mustExec(t, srv.db().Write, `UPDATE images SET width = NULL, height = NULL WHERE id = ?`, id)
	form := url.Values{"x": {"3"}, "y": {"4"}, "w": {"5"}, "h": {"6"}, "body": {"no-dims"}}
	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/annotations/set", form)
	mustCode(t, w, http.StatusNoContent)
	if anns, _ := gallery.AnnotationsForImage(srv.db(), id); len(anns) != 1 {
		t.Fatalf("a dimensionless image should still accept the box, got %d", len(anns))
	}
}

func TestSetSourceCommentary_SetAndRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "comm1.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder { return postHTMXForm(t, srv, path, form) }
	commentary := func() string {
		t.Helper()
		var c string
		if err := srv.active().DB.Read.QueryRow(
			`SELECT commentary FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	base := "/images/" + strconv.FormatInt(id, 10)
	if w := post(base+"/commentary/set", url.Values{"site": {"danbooru"}, "commentary": {"artist words"}}); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	if got := commentary(); got != "artist words" {
		t.Errorf("commentary = %q, want set", got)
	}
	if w := post(base+"/commentary/remove", url.Values{"site": {"danbooru"}}); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if got := commentary(); got != "" {
		t.Errorf("commentary after remove = %q, want empty (origin kept)", got)
	}
}

func TestSetSourceOriginal_SetAndRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "orig1.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder { return postHTMXForm(t, srv, path, form) }
	original := func() string {
		t.Helper()
		var o string
		if err := srv.active().DB.Read.QueryRow(
			`SELECT original FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}

	base := "/images/" + strconv.FormatInt(id, 10)
	if w := post(base+"/original/set", url.Values{"site": {"danbooru"}, "original": {"https://pixiv/artworks/1"}}); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	if got := original(); got != "https://pixiv/artworks/1" {
		t.Errorf("original = %q, want set", got)
	}
	if w := post(base+"/original/remove", url.Values{"site": {"danbooru"}}); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if got := original(); got != "" {
		t.Errorf("original after remove = %q, want empty (origin kept)", got)
	}

	freeform := "some artist\nhttps://pixiv/artworks/2"
	if w := post(base+"/original/set", url.Values{"site": {"danbooru"}, "original": {freeform}}); w.Code != http.StatusNoContent {
		t.Fatalf("freeform set: %d %s", w.Code, w.Body.String())
	}
	if got := original(); got != freeform {
		t.Errorf("freeform original = %q, want it stored verbatim", got)
	}
}

func TestRemoveSource_HandlerCascadesAnnotations(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "remsrc.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	if err := gallery.ReplaceSourceAnnotations(srv.db(), id, "danbooru", "",
		[]models.Annotation{{X: 1, Y: 1, W: 2, H: 2, Body: "b"}}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"site": {"danbooru"}}
	w := postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/sources/remove", form)
	mustCode(t, w, http.StatusNoContent)
	n := countRows(t, srv.active().DB, "image_annotations WHERE image_id = ?", id)
	if n != 0 {
		t.Errorf("annotations after source remove = %d, want 0", n)
	}
}

func TestFetchSource_PendingPillAndUnreachable(t *testing.T) {
	accepted := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	id := seedImage(t, srv, "fetch1.png", 6, 6)
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	post := func(u string) *httptest.ResponseRecorder {
		form := url.Values{"url": {u}}
		return postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/sources/fetch", form)
	}

	w := post("https://d/1")
	if accepted != 1 {
		t.Fatalf("peer accepted %d enqueues, want 1", accepted)
	}
	if body := w.Body.String(); !strings.Contains(body, "Fetching data via monloader") || !strings.Contains(body, "fetch-status") {
		t.Errorf("expected the polling pending pill, got %q", body)
	}

	stub.Close()
	w = post("https://d/2")
	if body := w.Body.String(); !strings.Contains(body, "could not reach monloader") {
		t.Errorf("expected the unreachable error inline, got %q", body)
	}
}

func TestLookupImage_EnqueuesHashOnMonloader(t *testing.T) {
	var gotPath, gotBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	id := seedImage(t, srv, "lookup1.png", 6, 6)
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	post := func(backend string) *httptest.ResponseRecorder {
		form := url.Values{"backend": {backend}}
		return postHTMXForm(t, srv, "/images/"+strconv.FormatInt(id, 10)+"/lookup", form)
	}

	var canonPath, sha string
	if err := srv.db().Read.QueryRow(
		`SELECT canonical_path, sha256 FROM images WHERE id = ?`, id).Scan(&canonPath, &sha); err != nil {
		t.Fatal(err)
	}
	md5, err := gallery.Md5File(canonPath)
	if err != nil {
		t.Fatal(err)
	}

	w := post("all")
	if gotPath != "/api/v1/lookup" {
		t.Fatalf("peer path = %q, want /api/v1/lookup", gotPath)
	}
	for _, want := range []string{`"backend":"all"`, `"md5":"` + md5 + `"`, `"sha256":"` + sha + `"`, `"gallery":"default"`} {
		wantIn(t, gotBody, want)
	}
	if body := w.Body.String(); !strings.Contains(body, "Fetching data via monloader") {
		t.Errorf("expected the polling pending pill, got %q", body)
	}

	post("ptr")
	if !strings.Contains(gotBody, `"backend":"ptr"`) || !strings.Contains(gotBody, `"sha256":"`+sha+`"`) {
		t.Errorf("ptr payload %q missing backend/sha256", gotBody)
	}

	post("booru")
	if !strings.Contains(gotBody, `"backend":"booru"`) || !strings.Contains(gotBody, `"md5":"`+md5+`"`) {
		t.Errorf("booru payload %q missing backend/md5", gotBody)
	}

	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer refuse.Close()
	srv.ml.Fetches.Clear(srv.activeGallery(), id)
	srv.cfg.Monloader.APIURL = refuse.URL
	w = post("ptr")
	if body := w.Body.String(); !strings.Contains(body, "PTR lookup is unavailable") {
		t.Errorf("expected the ptr-unavailable message inline, got %q", body)
	}
	if _, ok := srv.ml.Fetches.Load(srv.activeGallery(), id); ok {
		t.Error("a refused lookup must not record a pending fetch")
	}
}

func TestKeepLocalFile_HandlerWithholdsTheReplace(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "keep.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	if err := gallery.SetSourceMD5Match(srv.db(), id, "danbooru", "", "differ"); err != nil {
		t.Fatal(err)
	}
	base := "/images/" + strconv.FormatInt(id, 10)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		return postHTMXForm(t, srv, path, form)
	}
	kept := func() bool {
		srcs, err := gallery.SourcesForImage(srv.db(), id)
		if err != nil || len(srcs) != 1 {
			t.Fatalf("sources = %+v (err %v)", srcs, err)
		}
		return srcs[0].UpgradeKept
	}

	if w := post(base+"/sources/keep", url.Values{"site": {"danbooru"}, "keep": {"1"}}); w.Code != http.StatusNoContent {
		t.Fatalf("keep: %d %s", w.Code, w.Body.String())
	}
	if !kept() {
		t.Fatal("keep did not record the ruling")
	}

	if w := post(base+"/replace", url.Values{"site": {"danbooru"}}); !strings.Contains(w.Body.String(), "not known to differ") {
		t.Errorf("replace on a kept origin rendered %q, want the refusal", w.Body.String())
	}
	form := url.Values{"site": {"danbooru"}}
	w := postForm(t, srv, base+"/replace", form)
	wantCode(t, w, http.StatusConflict)

	if w := post(base+"/sources/keep", url.Values{"site": {"danbooru"}, "keep": {"0"}}); w.Code != http.StatusNoContent {
		t.Fatalf("allow: %d %s", w.Code, w.Body.String())
	}
	if kept() {
		t.Error("allow did not clear the ruling")
	}
}

func TestHTMXRefusalsThatSwapNothingFlashTheirReason(t *testing.T) {
	srv := newTestServer(t)
	hx := [2]string{"HX-Request", "true"}

	gone := postForm(t, srv, "/images/999999/favorite", url.Values{}, hx)
	mustCode(t, gone, http.StatusNotFound)
	wantIn(t, gone.Header().Get("HX-Trigger"), `"action-flash":{"kind":"err","text":"This image is gone; reload the page."}`)
	if plain := postForm(t, srv, "/images/999999/favorite", url.Values{}); strings.Contains(plain.Body.String(), "flash") {
		t.Errorf("a plain post got markup: %q", plain.Body.String())
	}

	srv.active().Degraded = true
	sync := postForm(t, srv, "/internal/sync", url.Values{}, hx)
	mustCode(t, sync, http.StatusServiceUnavailable)
	wantIn(t, sync.Header().Get("HX-Trigger"), "Sync unavailable")
}
