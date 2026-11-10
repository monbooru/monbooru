package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func seedInto(t *testing.T, srv *Server, gallery, name string, w, h int) int64 {
	t.Helper()
	cx := srv.get(gallery)
	path := filepath.Join(cx.GalleryPath, name)
	mustWriteFile(t, path, makePNGBytes(t, w, h, byte(w), byte(h), 0), 0o644)
	img, _, err := galleryIngest(srv, gallery, path)
	if err != nil {
		t.Fatal(err)
	}
	return img.ID
}

func TestTransferImage_CopiesTheImageAndStaysOnIt(t *testing.T) {
	srv := newMultiGalleryServer(t)
	id := seedInto(t, srv, "default", "carried.png", 21, 21)
	tag := mustTag(t, srv.tagSvc(), "carried_tag", srv.active().GeneralCategoryID)
	mustAddTag(t, srv.tagSvc(), id, tag.ID, false, nil)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/transfer", id), url.Values{"target": {"stock"}})
	mustCode(t, w, http.StatusSeeOther)
	if got := w.Header().Get("Location"); got != fmt.Sprintf("/images/%d", id) {
		t.Errorf("redirect = %q, want the source image", got)
	}
	if n := countRows(t, srv.get("default").DB, "images WHERE id = ?", id); n != 1 {
		t.Error("the source row went without remove_after")
	}
	if n := countRows(t, srv.get("stock").DB, "images"); n != 1 {
		t.Errorf("target holds %d image(s), want the copy", n)
	}
	if n := countRows(t, srv.get("stock").DB, "tags WHERE name = 'carried_tag'"); n != 1 {
		t.Error("the tag did not ride along")
	}
}

func TestTransferImage_JobRecordsBothGalleries(t *testing.T) {
	srv := newMultiGalleryServer(t)
	id := seedInto(t, srv, "default", "recorded.png", 23, 23)

	mustCode(t, postForm(t, srv, fmt.Sprintf("/images/%d/transfer", id), url.Values{"target": {"stock"}}), http.StatusSeeOther)

	if got := srv.jobs.Get().Galleries; !slices.Equal(got, []string{"default", "stock"}) {
		t.Errorf("job galleries = %v, want the source and the target", got)
	}
}

func TestTransferImage_RemoveAfterGoesBackToTheGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	id := seedInto(t, srv, "default", "moved.png", 22, 22)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/transfer", id), url.Values{
		"target": {"stock"}, "remove_after": {"on"},
	})
	mustCode(t, w, http.StatusSeeOther)
	if got := w.Header().Get("Location"); got != "/" {
		t.Errorf("redirect = %q, want the gallery: the source image is gone", got)
	}
	if n := countRows(t, srv.get("default").DB, "images WHERE id = ?", id); n != 0 {
		t.Error("remove_after left the source row behind")
	}
	if n := countRows(t, srv.get("stock").DB, "images"); n != 1 {
		t.Errorf("target holds %d image(s), want the copy", n)
	}
}

func TestTransferImage_RefusesTheActiveGalleryAsTarget(t *testing.T) {
	srv := newMultiGalleryServer(t)
	id := seedInto(t, srv, "default", "staying.png", 23, 23)

	for _, tc := range []struct{ target, want string }{
		{"", "Pick a target gallery."},
		{srv.activeGallery(), "The target must be a different gallery."},
		{"nope", "Unknown target gallery."},
	} {
		w := postForm(t, srv, fmt.Sprintf("/images/%d/transfer", id), url.Values{"target": {tc.target}})
		wantCode(t, w, http.StatusBadRequest)
		wantIn(t, w.Body.String(), tc.want)
	}
	if n := countRows(t, srv.get("stock").DB, "images"); n != 0 {
		t.Error("a refused transfer copied anyway")
	}
	if srv.jobs.IsRunning() {
		t.Error("a refused transfer left the job lane held")
	}
}

func TestTransferImage_HTMXRefusalAnswersTheDialogAt200(t *testing.T) {
	srv := newMultiGalleryServer(t)
	id := seedInto(t, srv, "default", "dialog.png", 24, 24)

	w := postForm(t, srv, fmt.Sprintf("/images/%d/transfer", id), url.Values{"target": {"nope"}},
		[2]string{"HX-Request", "true"})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `<div class="flash flash-err">Unknown target gallery.</div>`)
}

func TestTransferMoves_SayHowManyChangedFilesTheyKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, srv *Server, id int64) string
	}{
		{"one image", func(t *testing.T, srv *Server, id int64) string {
			w := postForm(t, srv, fmt.Sprintf("/images/%d/transfer", id), url.Values{
				"target": {"stock"}, "remove_after": {"on"},
			}, [2]string{"HX-Request", "true"})
			return w.Header().Get("HX-Trigger")
		}},
		{"a batch", func(t *testing.T, srv *Server, id int64) string {
			mustStartJob(t, srv.jobs, models.JobTypeTransfer)
			srv.runBatchTransfer([]int64{id}, srv.get("stock"), true)
			return srv.jobs.Get().Summary
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMultiGalleryServer(t)
			id := seedInto(t, srv, "default", "moved.png", 22, 22)
			path := canonicalPathOf(t, srv, id)
			writeOverWithMore(t, path)

			wantIn(t, tc.run(t, srv, id), "kept 1 file changed since indexing")
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the file written over since was removed: %v", err)
			}
		})
	}
}
