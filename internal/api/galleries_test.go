package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
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
	cases := []struct {
		name   string
		images []image
		want   []string // path=images, depth first
	}{
		{"empty gallery", nil, []string{"=0"}},
		{"root only", []image{{"", false}}, []string{"=1"}},
		{
			"nested counts roll up",
			[]image{{"", false}, {"art", false}, {"art/sketches", false}, {"art/sketches", false}, {"photos", false}},
			[]string{"=5", "art=3", "art/sketches=2", "photos=1"},
		},
		{"a folder holding only subfolders", []image{{"a/b/c", false}}, []string{"=1", "a=1", "a/b=1", "a/b/c=1"}},
		{"missing images are left out", []image{{"kept", false}, {"gone", true}}, []string{"=1", "kept=1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			for i, img := range tc.images {
				id := env.createTestImage(t, fmt.Sprintf("f%d.png", i), 10+i, 10)
				mustExec(t, env.database.Write, `UPDATE images SET folder_path = ?, is_missing = ? WHERE id = ?`, img.folder, img.missing, id)
			}

			w := env.do("GET", "/api/v1/folders")
			mustCode(t, w, http.StatusOK)
			var root contract.Folder
			mustDecode(t, w, &root)
			if root.Path != "" || root.Name != "" {
				t.Errorf("root = %q/%q, want an empty path and name", root.Path, root.Name)
			}
			var got []string
			var walk func(f contract.Folder)
			walk = func(f contract.Folder) {
				got = append(got, fmt.Sprintf("%s=%d", f.Path, f.Images))
				if f.Children == nil {
					t.Errorf("%q: children is null, want an array", f.Path)
				}
				for _, c := range f.Children {
					if parent, name := path.Split(c.Path); strings.TrimSuffix(parent, "/") != f.Path || name != c.Name {
						t.Errorf("%q (name %q) is not a child of %q", c.Path, c.Name, f.Path)
					}
					walk(c)
				}
			}
			walk(root)
			if !slices.Equal(got, tc.want) {
				t.Errorf("tree = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListFoldersPicksTheGallery(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/folders?gallery=no_such_gallery")
	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), `"invalid_gallery"`)

	w = env.do("GET", "/api/v1/folders?gallery="+env.cfg.DefaultGallery)
	mustCode(t, w, http.StatusOK)
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
