package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/gallery"
)

func setDestination(srv *Server, folder, name string) {
	srv.cfgMu.Lock()
	srv.cfg.Gallery.DefaultUploadFolder, srv.cfg.Gallery.DefaultUploadName = folder, name
	srv.cfgMu.Unlock()
}

func applyDestination(t *testing.T, srv *Server, check bool) string {
	t.Helper()
	form := url.Values{}
	if check {
		form.Set("check", "1")
	}
	mustCode(t, postForm(t, srv, "/settings/maintenance/apply-destination", form), http.StatusOK)
	awaitJobsDrain(t, srv)
	return srv.jobs.Get().Summary
}

func canonicalOf(t *testing.T, srv *Server, id int64) string {
	t.Helper()
	rel, _ := filepath.Rel(srv.active().GalleryPath, canonicalPathOf(t, srv, id))
	return filepath.ToSlash(rel)
}

func TestApplyDestination_RenamesWhatDoesNotFollowAndAReRunChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	mustMkdirAll(t, filepath.Join(srv.active().GalleryPath, "sub"), 0o755)
	root := seedImage(t, srv, "a.png", 10, 10)
	sub := seedImage(t, srv, "sub/b.png", 11, 11)
	setDestination(srv, "", "pic-{id}")

	if got := applyDestination(t, srv, false); got != "Renamed 2 images." {
		t.Errorf("summary = %q", got)
	}
	if got, want := canonicalOf(t, srv, root), fmt.Sprintf("pic-%d.png", root); got != want {
		t.Errorf("root image at %q, want %q", got, want)
	}
	if got, want := canonicalOf(t, srv, sub), fmt.Sprintf("sub/pic-%d.png", sub); got != want {
		t.Errorf("subfolder image at %q, want %q", got, want)
	}

	if got := applyDestination(t, srv, false); got != "Nothing to do: 2 images already follow the destination." {
		t.Errorf("second run summary = %q", got)
	}
}

func TestApplyDestination_AFolderMovesImagesOutOfEveryFolder(t *testing.T) {
	srv := newTestServer(t)
	mustMkdirAll(t, filepath.Join(srv.active().GalleryPath, "sub"), 0o755)
	root := seedImage(t, srv, "a.png", 10, 10)
	sub := seedImage(t, srv, "sub/b.png", 11, 11)
	setDestination(srv, "sorted", "")

	got := applyDestination(t, srv, false)
	if !strings.HasPrefix(got, "Moved 2 images.") || !strings.Contains(got, "1 folder is now empty.") {
		t.Errorf("summary = %q", got)
	}
	for id, want := range map[int64]string{root: "sorted/a.png", sub: "sorted/b.png"} {
		if got := canonicalOf(t, srv, id); got != want {
			t.Errorf("image %d at %q, want %q", id, got, want)
		}
	}
}

func TestApplyDestination_ANumberedNameStaysOnAReRun(t *testing.T) {
	srv := newTestServer(t)
	ids := []int64{
		seedImage(t, srv, "a.png", 10, 10),
		seedImage(t, srv, "b.png", 11, 11),
		seedImage(t, srv, "c.png", 12, 12),
	}
	setDestination(srv, "", "{gallery}")

	if got := applyDestination(t, srv, false); !strings.Contains(got, "2 had a taken name and were numbered.") {
		t.Errorf("summary = %q", got)
	}
	before := map[int64]string{}
	for _, id := range ids {
		before[id] = canonicalOf(t, srv, id)
	}

	if got := applyDestination(t, srv, false); got != "Nothing to do: 3 images already follow the destination." {
		t.Errorf("second run summary = %q", got)
	}
	for _, id := range ids {
		if got := canonicalOf(t, srv, id); got != before[id] {
			t.Errorf("image %d renumbered from %q to %q", id, before[id], got)
		}
	}
}

func TestApplyDestinationCheck_CountsAndChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	ids := []int64{
		seedImage(t, srv, "a.png", 10, 10),
		seedImage(t, srv, "b.png", 11, 11),
		seedImage(t, srv, "c.png", 12, 12),
	}
	setDestination(srv, "", "{gallery}")

	got := applyDestination(t, srv, true)
	if !strings.Contains(got, "3 would change, 0 already follow the destination; 2 would be numbered, their names are already taken.") {
		t.Errorf("summary = %q", got)
	}
	for i, id := range ids {
		if got, want := canonicalOf(t, srv, id), string(rune('a'+i))+".png"; got != want {
			t.Errorf("check moved image %d to %q", id, got)
		}
	}
	if _, err := os.Stat(filepath.Join(srv.active().GalleryPath, "default.png")); err == nil {
		t.Error("the check wrote a file")
	}
}

func TestApplyDestination_RefusesWithoutADestination(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "a.png", 10, 10)
	setDestination(srv, "", "")

	w := postForm(t, srv, "/settings/maintenance/apply-destination", url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "flash-err")
	if srv.jobs.Get() != nil {
		t.Error("a job started with nothing to apply")
	}
	wantIn(t, do(srv, "GET", "/settings/maintenance/apply-destination", [2]string{"HX-Request", "true"}).Body.String(), "in General first")
}

func TestApplyDestinationPreview_ListsOnlyWhatChanges(t *testing.T) {
	srv := newTestServer(t)
	placed := seedImage(t, srv, "a.png", 10, 10)
	other := seedImage(t, srv, "b.png", 11, 11)
	setDestination(srv, "", "pic-{id}")
	name := fmt.Sprintf("pic-%d", placed)
	if _, err := srv.active().PlaceImage(placed, nil, &name); err != nil {
		t.Fatal(err)
	}

	body := do(srv, "GET", "/settings/maintenance/apply-destination", [2]string{"HX-Request", "true"}).Body.String()
	wantIn(t, body, fmt.Sprintf("pic-%d.png", other))
	wantIn(t, body, "1 that change")
	if strings.Contains(body, fmt.Sprintf("pic-%d.png", placed)) {
		t.Errorf("preview listed the image that already follows: %q", body)
	}

	applyDestination(t, srv, false)
	wantIn(t, do(srv, "GET", "/settings/maintenance/apply-destination", [2]string{"HX-Request", "true"}).Body.String(), "Every image already follows the destination.")
}

func TestApplyDestinationPreview_NotesWhatTheRunCannotUndo(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "a.png", 10, 10)

	setDestination(srv, "sorted", "{name}-{source}")
	body := do(srv, "GET", "/settings/maintenance/apply-destination", [2]string{"HX-Request", "true"}).Body.String()
	for _, want := range []string{"move under sorted, subfolders included", "{name} is the name", "{source} and {post_id}"} {
		wantIn(t, body, want)
	}

	setDestination(srv, "{folder}/{year}", "{id}")
	body = do(srv, "GET", "/settings/maintenance/apply-destination", [2]string{"HX-Request", "true"}).Body.String()
	for _, unwanted := range []string{"subfolders included", "{name} is", "{source} and"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("preview carries %q: %q", unwanted, body)
		}
	}
}

func TestApplyDestinationNotes(t *testing.T) {
	const nests = "nests one level deeper on every run"
	for _, tc := range []struct {
		folder, name string
		want, absent []string
	}{
		{"sorted", "", []string{"move under sorted, subfolders included"}, []string{nests}},
		{"{folder}/sorted", "", []string{nests}, []string{"subfolders included"}},
		{"{folder}/{year}", "{id}", []string{nests}, nil},
		{"{folder}", "{id}", nil, []string{nests, "subfolders included"}},
		{"", "{name}-{source}", []string{"{name} is the name", "{source} and {post_id}"}, []string{nests}},
	} {
		got := applyDestinationNotes(gallery.IngestNaming("default", tc.folder, tc.name), tc.folder)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("folder %q name %q: notes = %q, want %q", tc.folder, tc.name, got, want)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(got, absent) {
				t.Errorf("folder %q name %q: notes = %q, carry %q", tc.folder, tc.name, got, absent)
			}
		}
	}
}
