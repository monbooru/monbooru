package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
)

func placeInFolder(database *db.DB, galleryPath string, id int64, folder string) error {
	_, err := gallery.PlaceImage(database, gallery.NewBoundary(galleryPath, nil, nil), id, &folder, nil)
	return err
}

func TestNamePreview_RefusesAnEscapingMoveDestination(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "moved.png", 10, 10)

	preview := func(scope, folder string) (int, string) {
		w := do(srv, "GET", fmt.Sprintf(
			"/internal/name/preview?scope=%s&folder=%s&name=&ids=%d", scope, folder, id))
		return w.Code, w.Body.String()
	}

	for _, tc := range []struct{ scope, folder, want string }{
		{"place", "..%2Fescape", "escapes the gallery root"},
		{"place", "%2Fabs%2Fpath", "relative"},
		{"place-batch", "..%2Fescape", "escapes the gallery root"},
	} {
		code, body := preview(tc.scope, tc.folder)
		if code != http.StatusOK || !strings.Contains(body, tc.want) {
			t.Errorf("preview %s %q = %d %q, want the refusal to mention %q", tc.scope, tc.folder, code, body, tc.want)
		}
		if strings.Contains(body, "->") {
			t.Errorf("preview %s %q rendered a destination row: %q", tc.scope, tc.folder, body)
		}
	}

	if code, body := preview("place", "sub%2Fdir"); code != http.StatusOK || !strings.Contains(body, "sub/dir/moved.png") {
		t.Errorf("valid destination = %d %q, want the sub/dir/moved.png row", code, body)
	}
}

func TestNamePreview_MoveRowsAreWholePaths(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)
	cx := srv.active()
	if err := placeInFolder(cx.DB, cx.GalleryPath, id, "here"); err != nil {
		t.Fatalf("place: %v", err)
	}

	w := do(srv, "GET", fmt.Sprintf(
		"/internal/name/preview?scope=place&folder=there%%2Fdeep&name=&ids=%d", id))
	body := w.Body.String()
	for _, want := range []string{"here/shot.png", "there/deep/shot.png"} {
		wantIn(t, body, want)
	}
}

func TestNamePreview_ShowsTheNormalisedMoveDestination(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)

	for _, tc := range []struct{ typed, want string }{
		{"a%2F%2Fb", "a/b/shot.png"},
		{".%2Fx", "x/shot.png"},
		{"trailing%2F", "trailing/shot.png"},
		{"%20%20spaced%20%20", "spaced/shot.png"},
	} {
		w := do(srv, "GET", fmt.Sprintf(
			"/internal/name/preview?scope=place&folder=%s&name=&ids=%d", tc.typed, id))
		body := w.Body.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("preview of %q = %q, want it to name %q", tc.typed, body, tc.want)
		}
	}
}

func TestNamePreview_PairsWholePathsForEitherHalf(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)
	cx := srv.active()
	if err := placeInFolder(cx.DB, cx.GalleryPath, id, "here"); err != nil {
		t.Fatalf("place: %v", err)
	}

	row := func(folder, name string) string {
		w := do(srv, "GET", fmt.Sprintf(
			"/internal/name/preview?scope=place&folder=%s&name=%s&ids=%d", folder, name, id))
		return w.Body.String()
	}

	for _, tc := range []struct{ folder, name, want string }{
		{"there", "", "there/shot.png"},
		{"%7Bfolder%7D", "cover", "here/cover.png"},
		{"there", "cover", "there/cover.png"},
		{"", "cover", "cover.png"},
	} {
		body := row(tc.folder, tc.name)
		wantIn(t, body, "here/shot.png")
		wantIn(t, body, tc.want)
	}

	if body := row("%7Bfolder%7D", ""); strings.Contains(body, "->") {
		t.Errorf("a destination that changes nothing rendered a row: %q", body)
	}
}

func TestNamePreview_NamesTheRefusedField(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)

	for _, tc := range []struct{ folder, name, want string }{
		{"%7Bnope%7D", "", "folder:"},
		{"", "sub%2Fdir", "name:"},
		{"", "%7Bfolder%7D", "name:"},
	} {
		w := do(srv, "GET", fmt.Sprintf(
			"/internal/name/preview?scope=place&folder=%s&name=%s&ids=%d", tc.folder, tc.name, id))
		wantIn(t, w.Body.String(), tc.want)
	}
}

func TestNamePreview_ElidesTheOldPathAndKeepsItWhole(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "a_really_long_original_filename_that_will_not_fit.png", 10, 10)
	if err := placeInFolder(srv.active().DB, srv.active().GalleryPath, id, "deep/nested/place"); err != nil {
		t.Fatalf("place: %v", err)
	}

	w := do(srv, "GET", fmt.Sprintf(
		"/internal/name/preview?scope=place&folder=&name=short&ids=%d", id))
	body := w.Body.String()

	full := "deep/nested/place/a_really_long_original_filename_that_will_not_fit.png"
	wantIn(t, body, `title="`+full+`"`)
	wantIn(t, body, "short.png")
	if strings.Contains(body, ">"+full+"<") {
		t.Errorf("the old path was not elided: %q", body)
	}
	if !strings.Contains(body, "...") {
		t.Errorf("no elision marker in %q", body)
	}
}

func TestNamePreview_NumbersCollisionsTheWayTheRunDoes(t *testing.T) {
	srv := newTestServer(t)
	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		ids = append(ids, strconv.FormatInt(seedImage(t, srv, fmt.Sprintf("c%02d.png", i), 10+i, 10+i), 10))
	}
	joined := strings.Join(ids, ",")

	w := do(srv, "GET", fmt.Sprintf(
		"/internal/name/preview?scope=place&folder=&name=same&rows=4&ids=%s&total=4", joined))
	body := w.Body.String()

	for _, want := range []string{"same.png", "same01.png", "same02.png", "same03.png"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview is missing %q:\n%s", want, body)
		}
	}
}

func TestNamePreviewStoringAnMD5DropsCachedSearches(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "a.png", 10, 10)
	mustExec(t, srv.db().Write, `UPDATE images SET md5 = '' WHERE id = ?`, id)
	if got := galleryResultCount(t, srv, "-md5:"); got != "1" {
		t.Fatalf("-md5: = %s before the preview, want 1", got)
	}
	w := do(srv, "GET", fmt.Sprintf("/internal/name/preview?scope=place&folder=&name=%%7Bmd5%%7D&ids=%d", id))
	if !strings.Contains(w.Body.String(), "name-preview-to") {
		t.Fatalf("the preview rendered no row: %q", w.Body.String())
	}
	if got := galleryResultCount(t, srv, "-md5:"); got != "0" {
		t.Errorf("-md5: = %s after the preview stored the digest, want 0", got)
	}
}

func TestNamePreview_RowWindow(t *testing.T) {
	srv := newTestServer(t)
	ids := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		ids = append(ids, strconv.FormatInt(seedImage(t, srv, fmt.Sprintf("w%02d.png", i), 10+i, 10+i), 10))
	}
	joined := strings.Join(ids, ",")

	rows := func(want string) int {
		w := do(srv, "GET", fmt.Sprintf(
			"/internal/name/preview?scope=place-batch&folder=&name=seq&rows=%s&ids=%s&total=30", want, joined))
		return strings.Count(w.Body.String(), "name-preview-to")
	}
	if got := rows(""); got != namePreviewRows {
		t.Errorf("default window = %d rows, want %d", got, namePreviewRows)
	}
	if got := rows("25"); got != namePreviewRowsMax {
		t.Errorf("widened window = %d rows, want %d", got, namePreviewRowsMax)
	}
	if got := rows("500"); got != namePreviewRowsMax {
		t.Errorf("window asked past the cap = %d rows, want %d", got, namePreviewRowsMax)
	}
}

func TestNamePreview_ASearchScopeStartsAtItsFirstMatch(t *testing.T) {
	srv := newTestServer(t)
	var oldest int64
	for i := 0; i < 30; i++ {
		id := seedImage(t, srv, fmt.Sprintf("s%02d.png", i), 10+i, 10+i)
		if i == 0 {
			oldest = id
		}
	}
	w := do(srv, "GET", fmt.Sprintf(
		"/internal/name/preview?scope=place-batch&folder=&name=seq&kind=search&q=&sort=newest&order=desc&ids=%d&total=30", oldest))
	body := w.Body.String()
	wantIn(t, body, "s29.png")
	if strings.Contains(body, "s00.png") {
		t.Error("the preview read the ids the client sent instead of the scope's first rows")
	}
}

func TestElideMiddle_CutsOnRunes(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("é", 80) + ".png",
		strings.Repeat("日本語", 40) + ".png",
		"plain/ascii/" + strings.Repeat("a", 80) + ".png",
	} {
		got := elideMiddle(in, namePreviewFrom)
		if !utf8.ValidString(got) {
			t.Errorf("elideMiddle(%.20q...) = invalid utf8 %q", in, got)
		}
		if n := utf8.RuneCountInString(got); n != namePreviewFrom {
			t.Errorf("elideMiddle(%.20q...) = %d runes, want %d", in, n, namePreviewFrom)
		}
	}
	if got := elideMiddle("short.png", namePreviewFrom); got != "short.png" {
		t.Errorf("a path under the budget was elided: %q", got)
	}
}

func TestNamePreview_NamesAFolderPerImage(t *testing.T) {
	srv := newTestServer(t)
	ids := []int64{
		seedImage(t, srv, "one.png", 10, 10),
		seedImage(t, srv, "two.png", 11, 11),
	}
	preview := func(folder string) string {
		url := fmt.Sprintf("/internal/name/preview?scope=place-batch&folder=%s&total=%d", folder, len(ids))
		for _, id := range ids {
			url += fmt.Sprintf("&ids=%d", id)
		}
		w := do(srv, "GET", url)
		return w.Body.String()
	}

	if got := preview("%7Bname%7D"); !strings.Contains(got, "a folder per image, up to 2 new ones") {
		t.Errorf("preview of {name} = %q, want the aggregate named", got)
	}

	for _, folder := range []string{"%7Bfolder%7D%2Farchive", "archive%2F%7Byear%7D"} {
		if got := preview(folder); strings.Contains(got, "a folder per image") {
			t.Errorf("preview of %q warned about a folder per image: %q", folder, got)
		}
	}
}

func TestNamePreview_RefusesANestedGallerysFolder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "moved.png", 10, 10)
	addRemovableGallery(t, srv, "H", filepath.Join(srv.get("default").GalleryPath, ".H"))

	w := do(srv, "GET", fmt.Sprintf(
		"/internal/name/preview?scope=place&folder=.H%%2Fsub&name=&ids=%d", id))
	if body := w.Body.String(); !strings.Contains(body, "belongs to gallery H") || strings.Contains(body, "->") {
		t.Errorf("preview = %d %q, want the refusal naming gallery H", w.Code, body)
	}
}
