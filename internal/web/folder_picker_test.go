package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFolderPickerOnTheUploadForm(t *testing.T) {
	cases := []struct {
		name, defaultFolder, path string
		want, wantNot             []string
	}{
		{"the inbox, no default folder", "", "/?q=" + url.QueryEscape("inbox:true"),
			[]string{`id="upload-folder-input" name="folder"`, `placeholder="(gallery root)"`, `hx-get="/internal/folder-picker/level?kind=upload`}, nil},
		{"the default folder named", "incoming/{date}", "/?q=" + url.QueryEscape("inbox:true"),
			[]string{`placeholder="(default: incoming/{date})"`}, nil},
		{"not outside the inbox", "", "/", nil, []string{`upload-folder`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.cfgMu.Lock()
			srv.cfg.Gallery.DefaultUploadFolder = tc.defaultFolder
			srv.cfgMu.Unlock()
			w := do(srv, "GET", tc.path)
			mustCode(t, w, http.StatusOK)
			for _, s := range tc.want {
				wantIn(t, w.Body.String(), s)
			}
			for _, s := range tc.wantNot {
				wantNotIn(t, w.Body.String(), s)
			}
		})
	}
}

func TestFolderPickerInTheMoveDialogsAndSettings(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, filepath.Join(mkFolder(t, srv, "art"), "a.png"), 8, 8)
	srv.cfgMu.Lock()
	srv.cfg.Gallery.DefaultUploadFolder = "art/{year}"
	srv.cfgMu.Unlock()
	cases := []struct {
		name, path string
		want       []string
	}{
		{"the batch move", "/",
			[]string{`id="batch-place-folder-input" name="folder"`, `hx-get="/internal/folder-picker/level?kind=batch-place`, `change from:#batch-place-folder-input`}},
		{"the image move", "/images/" + strconv.FormatInt(id, 10),
			[]string{`id="place-image-folder-input" name="folder"`, `hx-get="/internal/folder-picker/level?kind=place-image`, `change from:#place-image-folder-input`}},
		{"the default upload folder, holding its template", "/settings",
			[]string{`id="default-upload-folder-input" name="default_upload_folder" value="art/{year}"`, `hx-get="/internal/folder-picker/level?kind=default-upload`, `change from:#default-upload-folder-input`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(srv, "GET", tc.path)
			mustCode(t, w, http.StatusOK)
			for _, s := range tc.want {
				wantIn(t, w.Body.String(), s)
			}
			wantNotIn(t, w.Body.String(), "Not in the gallery yet")
		})
	}
}

func TestFolderPickerListsTheActiveGallery(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	seedImage(t, srv, filepath.Join(mkFolder(t, srv, "safe"), "a.png"), 8, 8)
	seedImage(t, srv, filepath.Join(mkFolder(t, srv, "art/sky"), "b.png"), 9, 9)
	hidden := seedImage(t, srv, filepath.Join(mkFolder(t, srv, "nsfw/sub"), "c.png"), 10, 10)
	mustAddTag(t, cx.TagSvc, hidden, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil)
	cx.InvalidateCaches()
	ceiling := [2]string{"Cookie", "monbooru_rating_ceiling=sensitive"}

	cases := []struct {
		name, path string
		headers    [][2]string
		code       int
		want       []string
		wantNot    []string
	}{
		{"the top level", "/level?kind=upload&path=", nil, http.StatusOK,
			[]string{`data-folder-pick="art"`, `data-folder-pick="safe"`, `data-folder-pick="nsfw"`}, []string{`data-folder-pick="art/sky"`}},
		{"a branch", "/level?kind=upload&path=art", nil, http.StatusOK, []string{`data-folder-pick="art/sky"`}, nil},
		{"the branch holding the folder opens", "/level?kind=upload&path=&folder=art/sky", nil, http.StatusOK,
			[]string{`class="folder-node-row active" data-folder-pick="art/sky"`}, nil},
		{"what the ceiling hides is left out", "/level?kind=upload&path=", [][2]string{ceiling}, http.StatusOK,
			[]string{`data-folder-pick="safe"`}, []string{`data-folder-pick="nsfw"`}},
		{"a hidden level answers no rows", "/level?kind=upload&path=nsfw", [][2]string{ceiling}, http.StatusOK,
			nil, []string{`folder-node-row`}},
		{"suggestions", "/suggest?kind=upload&folder=sk", nil, http.StatusOK, []string{`data-folder-suggest="art/sky"`}, []string{`data-folder-suggest="safe"`}},
		{"no suggestion the ceiling hides", "/suggest?kind=upload&folder=nsfw", [][2]string{ceiling}, http.StatusOK,
			nil, []string{`data-folder-suggest`}},
		{"an unknown kind", "/level?kind=nope&path=", nil, http.StatusNotFound, nil, nil},
		{"the batch move's level", "/level?kind=batch-place&path=art", nil, http.StatusOK, []string{`data-folder-pick="art/sky"`}, nil},
		{"the image move's level", "/level?kind=place-image&path=", nil, http.StatusOK, []string{`data-folder-pick="safe"`}, nil},
		{"the default folder's suggestions", "/suggest?kind=default-upload&default_upload_folder=sk", nil, http.StatusOK,
			[]string{`data-folder-suggest="art/sky"`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(srv, "GET", folderPickerRoute+tc.path, tc.headers...)
			mustCode(t, w, tc.code)
			body := w.Body.String()
			for _, s := range tc.want {
				wantIn(t, body, s)
			}
			for _, s := range tc.wantNot {
				if strings.Contains(body, s) {
					t.Errorf("unexpected %s in %s", s, body)
				}
			}
		})
	}
}
