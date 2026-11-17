package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestListGalleries(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "g.png", 10, 10)
	body, _ := json.Marshal(map[string]any{"tags": []string{"a_tag"}})
	doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)

	w := env.do("GET", "/api/v1/galleries")
	mustCode(t, w, http.StatusOK)
	var out []map[string]any
	mustDecode(t, w, &out)
	if len(out) != 1 {
		t.Fatalf("expected 1 gallery, got %d", len(out))
	}
	g := out[0]
	if g["name"] != env.cfg.DefaultGallery {
		t.Errorf("name = %v, want %q", g["name"], env.cfg.DefaultGallery)
	}
	if g["active"] != true {
		t.Errorf("active = %v, want true", g["active"])
	}
	if g["images"] != float64(1) {
		t.Errorf("images = %v, want 1", g["images"])
	}
	if tags, _ := g["tags"].(float64); tags < 1 {
		t.Errorf("tags = %v, want >= 1", g["tags"])
	}
}

func TestListFolders(t *testing.T) {
	type image struct {
		folder  string
		missing bool
	}
	nested := []image{{"", false}, {"art", false}, {"art/sketches", false}, {"art/sketches", false}, {"photos", false}}
	cases := []struct {
		name   string
		images []image
		query  string
		want   []string // path:name=images/folders
		total  int
	}{
		{"empty gallery", nil, "", []string{}, 0},
		{"root images only", []image{{"", false}}, "", []string{}, 0},
		{"the top level, counts rolled up", nested, "", []string{"art:art=3/1", "photos:photos=1/0"}, 2},
		{"a folder's subfolders", nested, "path=art", []string{"art/sketches:sketches=2/0"}, 1},
		{"slashes around the path are dropped", nested, "path=/art/", []string{"art/sketches:sketches=2/0"}, 1},
		{"a leaf lists nothing", nested, "path=art/sketches", []string{}, 0},
		{"a folder holding only subfolders", []image{{"a/b/c", false}}, "path=a", []string{"a/b:b=1/1"}, 1},
		{"missing images are left out", []image{{"kept", false}, {"gone", true}}, "", []string{"kept:kept=1/0"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			for i, img := range tc.images {
				id := env.createTestImage(t, fmt.Sprintf("f%d.png", i), 10+i, 10)
				mustExec(t, env.database.Write, `UPDATE images SET folder_path = ?, is_missing = ? WHERE id = ?`, img.folder, img.missing, id)
			}

			w := env.do("GET", "/api/v1/folders?"+tc.query)
			mustCode(t, w, http.StatusOK)
			if !strings.Contains(w.Body.String(), `"results":[`) {
				t.Errorf("results should be an array, never null: %s", w.Body.String())
			}
			var page struct {
				Page, Limit, Total int
				Results            []contract.Folder
			}
			mustDecode(t, w, &page)
			got := []string{}
			for _, f := range page.Results {
				got = append(got, fmt.Sprintf("%s:%s=%d/%d", f.Path, f.Name, f.Images, f.Folders))
			}
			if !slices.Equal(got, tc.want) || page.Total != tc.total || page.Page != 1 || page.Limit != 500 {
				t.Errorf("level = %v (page %d, limit %d, total %d), want %v total %d", got, page.Page, page.Limit, page.Total, tc.want, tc.total)
			}
		})
	}
}

func TestListFoldersPages(t *testing.T) {
	env := newTestEnv(t)
	for i := range 5 {
		id := env.createTestImage(t, fmt.Sprintf("p%d.png", i), 10+i, 10)
		mustExec(t, env.database.Write, `UPDATE images SET folder_path = ? WHERE id = ?`, fmt.Sprintf("d%d", i), id)
	}
	cases := []struct {
		query string
		want  []string
	}{
		{"limit=2", []string{"d0", "d1"}},
		{"limit=2&page=2", []string{"d2", "d3"}},
		{"limit=2&page=3", []string{"d4"}},
		{"limit=2&page=9", []string{}},
		{"page_size=3", []string{"d0", "d1", "d2"}},
		{"limit=9999", []string{"d0", "d1", "d2", "d3", "d4"}},
	}
	for _, tc := range cases {
		w := env.do("GET", "/api/v1/folders?"+tc.query)
		mustCode(t, w, http.StatusOK)
		var page struct {
			Total   int
			Limit   int
			Results []contract.Folder
		}
		mustDecode(t, w, &page)
		got := []string{}
		for _, f := range page.Results {
			got = append(got, f.Path)
		}
		if !slices.Equal(got, tc.want) || page.Total != 5 || page.Limit > 500 {
			t.Errorf("%s: %v (total %d, limit %d), want %v of 5", tc.query, got, page.Total, page.Limit, tc.want)
		}
	}
}

func TestListFoldersUnknownPath(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "u.png", 10, 10)
	mustExec(t, env.database.Write, `UPDATE images SET folder_path = 'art' WHERE id = ?`, id)
	w := env.do("GET", "/api/v1/folders?path=nowhere")
	wantCode(t, w, http.StatusNotFound)
	wantIn(t, w.Body.String(), `"folder not found: nowhere"`)
}

func TestListFoldersPicksTheGallery(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/folders?gallery=no_such_gallery")
	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), `"invalid_gallery"`)

	id := env.createTestImage(t, "g.png", 10, 10)
	mustExec(t, env.database.Write, `UPDATE images SET folder_path = 'mine' WHERE id = ?`, id)
	w = env.do("GET", "/api/v1/folders?gallery="+env.cfg.DefaultGallery)
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"path":"mine"`)
}

func TestListFoldersAnswers500WhenTheReadFails(t *testing.T) {
	env := newTestEnv(t)
	if err := env.database.Read.Close(); err != nil {
		t.Fatal(err)
	}
	w := env.do("GET", "/api/v1/folders")
	wantCode(t, w, http.StatusInternalServerError)
	wantIn(t, w.Body.String(), `"internal_error"`)
}

func TestListGalleriesAnswers500WhenACountFails(t *testing.T) {
	env := newTestEnv(t)
	if err := env.database.Read.Close(); err != nil {
		t.Fatal(err)
	}
	w := env.do("GET", "/api/v1/galleries")
	wantCode(t, w, http.StatusInternalServerError)
	wantIn(t, w.Body.String(), `"internal_error"`)
}
