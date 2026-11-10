package web

import (
	"fmt"
	"image/color"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestTagSuggest_TargetParam(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	for _, name := range []string{"sword", "shield"} {
		if _, err := cx.TagSvc.GetOrCreateTag(name, 1); err != nil {
			t.Fatalf("create tag %q: %v", name, err)
		}
	}
	w := do(srv, "GET", "/internal/tags/suggest?target=swo")
	body := w.Body.String()
	wantIn(t, body, "sword")
	wantNotIn(t, body, "shield")
}

func TestTagSuggest_AnEmptyWordListsNothing(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.active().TagSvc.GetOrCreateTag("sword", 1); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"tag=", "q=", "target=", ""} {
		w := do(srv, "GET", "/internal/tags/suggest?"+query)
		wantCode(t, w, http.StatusNoContent)
		wantNotIn(t, w.Body.String(), "sword")
	}
}

func TestTagSuggest_TheCreateDialogLeavesOutRatings(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.active().TagSvc.GetOrCreateTag("questline", srv.active().GeneralCategoryID); err != nil {
		t.Fatal(err)
	}
	body := do(srv, "GET", "/internal/tags/suggest?name=ques").Body.String()
	wantIn(t, body, `data-tag-name="questline"`)
	wantNotIn(t, body, "rating:questionable")
	wantNotIn(t, do(srv, "GET", "/internal/tags/suggest?name=rating:ques").Body.String(), "questionable")
	wantIn(t, do(srv, "GET", "/internal/tags/suggest?tag=ques").Body.String(), "rating:questionable")
}

func TestTagSuggest_CanonicalInputQualifiesAnAmbiguousGeneralName(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	speciesID := categoryIDByName(t, srv, "species")
	for _, catID := range []int64{cx.GeneralCategoryID, speciesID} {
		if _, err := cx.TagSvc.GetOrCreateTag("cat", catID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cx.TagSvc.GetOrCreateTag("catfish", cx.GeneralCategoryID); err != nil {
		t.Fatal(err)
	}

	canonical := do(srv, "GET", "/internal/tags/suggest?canonical_id=ca").Body.String()
	wantIn(t, canonical, `data-tag-name="general:cat"`)
	wantIn(t, canonical, `data-tag-name="species:cat"`)
	wantIn(t, canonical, `data-tag-name="catfish"`)

	wantIn(t, do(srv, "GET", "/internal/tags/suggest?tag=ca").Body.String(), `data-tag-name="cat"`)
}

func TestFoldersSuggest_CaseBoundary(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	folders := []string{"Zoo/lions", "zebra/herd", "Foo/Bar", "fooBaz", "Az/one", "aZ/two", "\u00c9bauches/a", "\u00c9T\u00c9/b"}
	for i, fp := range folders {
		p := filepath.Join(cx.GalleryPath, fmt.Sprintf("f%d.png", i))
		mustWriteFile(t, p, tinyPNG(t, 8, 8, color.RGBA{byte(i + 1), 0, 0, 255}), 0o644)
		rec, _ := mustIngest(t, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "")
		mustExec(t, cx.DB.Write, `UPDATE images SET folder_path=? WHERE id=?`, fp, rec.ID)
	}
	body := func(prefix string) string {
		w := do(srv, "GET", "/internal/folders/suggest?prefix="+url.QueryEscape(prefix))
		return w.Body.String()
	}
	for _, tc := range []struct{ prefix, want string }{
		{"zoo", "Zoo/lions"},
		{"ZEBRA", "zebra/herd"},
		{"foo", "Foo/Bar"},
		{"foo", "fooBaz"},
		{"az", "Az/one"},
		{"AZ", "aZ/two"},
		{"Z", "zebra/herd"},
		{"z", "Zoo/lions"},
		{"\u00c9b", "\u00c9bauches/a"},
		{"\u00c9t", "\u00c9T\u00c9/b"},
	} {
		if got := body(tc.prefix); !strings.Contains(got, tc.want) {
			t.Errorf("prefix %q: %q missing from suggestions:\n%s", tc.prefix, tc.want, got)
		}
	}
}

func TestNameSuggestKeepsANonASCIICapital(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "\u00c9t\u00e9.png", 8, 8)
	w := do(srv, "GET", "/internal/search/suggest?q="+url.QueryEscape("name:\u00c9t"))
	if !strings.Contains(w.Body.String(), "\u00c9t\u00e9.png") {
		t.Errorf("name:\u00c9t suggests %q, want the stored basename", w.Body.String())
	}
}

func TestSystemSuggestKeepsTheCaseOfFreeTextValues(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv)
	mustExec(t, srv.db().Write, `INSERT INTO sd_metadata (image_id, prompt, sampler) VALUES (?, 'a cat', 'DPM++ 2M')`, id)

	for _, q := range []string{"sampler:D", "system:sampler:D", `system:sampler:"D`} {
		w := do(srv, "GET", "/internal/search/suggest?q="+url.QueryEscape(q))
		if !strings.Contains(w.Body.String(), "DPM&#43;&#43; 2M") {
			t.Errorf("%s: %d %q, want the sampler listed", q, w.Code, w.Body.String())
		}
	}
}
