package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
)

func getCategories(t *testing.T, env *testEnv) []map[string]any {
	t.Helper()
	w := env.do("GET", "/api/v1/categories")
	mustCode(t, w, http.StatusOK)
	var out []map[string]any
	mustDecode(t, w, &out)
	return out
}

func categoryID(t *testing.T, env *testEnv, name string) int64 {
	t.Helper()
	var id int64
	if err := env.database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("lookup category %q: %v", name, err)
	}
	return id
}

func TestListCategories(t *testing.T) {
	env := newTestEnv(t)
	cats := getCategories(t, env)
	if len(cats) == 0 {
		t.Fatal("expected built-in categories")
	}
	var general map[string]any
	for _, c := range cats {
		if c["name"] == "general" {
			general = c
		}
	}
	if general == nil {
		t.Fatal("general category missing")
	}
	if general["is_builtin"] != true {
		t.Errorf("general is_builtin = %v, want true", general["is_builtin"])
	}
}

func TestCreateCategory(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "mood", "color": "#abc"}, http.StatusCreated)
	if resp["name"] != "mood" {
		t.Errorf("name = %v, want mood", resp["name"])
	}
	if resp["color"] != "#aabbcc" {
		t.Errorf("color = %v, want the shorthand stored as #aabbcc", resp["color"])
	}
	if resp["is_builtin"] != false {
		t.Errorf("is_builtin = %v, want false", resp["is_builtin"])
	}
}

func TestCreateCategory_EmptyName(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "  "}, http.StatusBadRequest)
}

func TestPatchCategory_EmptyName(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "mood"}, http.StatusCreated)
	id := int64(resp["id"].(float64))
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"name": " "}, http.StatusBadRequest)
}

func TestCreateCategory_DefaultColor(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "vibe"}, http.StatusCreated)
	if resp["color"] != "#888888" {
		t.Errorf("color = %v, want #888888", resp["color"])
	}
}

func TestCreateCategory_Duplicate(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "general"}, http.StatusConflict)
}

func TestCreateCategory_Reserved(t *testing.T) {
	env := newTestEnv(t)

	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "source"}, http.StatusBadRequest)
}

func TestCreateCategory_InvalidName(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "Bad Name!"}, http.StatusBadRequest)
}

func TestPatchCategory_Recolor(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "tone"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"color": "#123456"}, http.StatusOK)
	if resp["color"] != "#123456" {
		t.Errorf("color = %v, want #123456", resp["color"])
	}
}

func TestPatchCategory_Rename(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "oldcat"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"name": "newcat"}, http.StatusOK)
	if resp["name"] != "newcat" {
		t.Errorf("name = %v, want newcat", resp["name"])
	}
}

func TestPatchCategory_RenameDropsCachedSearches(t *testing.T) {
	var catID int64
	var q search.Query
	var gal *library.Gallery
	mux := newBareEnv(t, nil, func(cfg *config.Config, g *library.Gallery) {
		gal = g
		cfg.Auth.Tokens = []apitoken.Token{{ID: "t", Name: "t", TokenHash: apitoken.Hash("secret-token"), Scopes: config.AllScopes}}
		cat, err := g.TagSvc.CreateCategory("oldcat", "#123456")
		if err != nil {
			t.Fatal(err)
		}
		catID = cat.ID
		q = search.Query{Cached: true, Text: "oldcat:x", Sort: "newest", Order: "desc"}
		g.Source().Cache.Seed(q, []int64{1})
	})
	req := httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/categories/%d", catID), strings.NewReader(`{"name":"newcat"}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	mustCode(t, w, http.StatusOK)
	if _, ok := search.PageOf(gal.Source(), q, 1, 1); ok {
		t.Error("a category rename left the gallery's cached searches in place")
	}
}

func TestPatchCategory_BuiltinRenameRejected(t *testing.T) {
	env := newTestEnv(t)
	id := categoryID(t, env, "general")
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"name": "renamed_general"}, http.StatusBadRequest)
}

func TestPatchCategory_BuiltinRecolorOK(t *testing.T) {
	env := newTestEnv(t)
	id := categoryID(t, env, "general")
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"color": "#020202"}, http.StatusOK)
}

func TestPatchCategory_BadColor(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "hue"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"color": "notacolor"}, http.StatusBadRequest)
}

func TestPatchCategory_NotFound(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "PATCH", "/api/v1/categories/99999", map[string]any{"name": "x"}, http.StatusNotFound)
}

func TestDeleteCategory_MoveReparentsTags(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "doomedcat"}, http.StatusCreated)
	catID := int64(created["id"].(float64))
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "orphan", "category": "doomedcat"}, http.StatusCreated)

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/categories/%d", catID), map[string]any{"action": "move"}, http.StatusNoContent)

	var movedTo int64
	if err := env.database.Read.QueryRow(`SELECT category_id FROM tags WHERE name = 'orphan'`).Scan(&movedTo); err != nil {
		t.Fatal(err)
	}
	if movedTo != categoryID(t, env, "general") {
		t.Errorf("orphan tag category = %d, want general", movedTo)
	}
}

func TestDeleteCategory_SelfMoveTargetRejected(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "doomedcat"}, http.StatusCreated)
	catID := int64(created["id"].(float64))
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "orphan", "category": "doomedcat"}, http.StatusCreated)

	body := apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/categories/%d", catID),
		map[string]any{"action": "move", "target_id": catID}, http.StatusBadRequest)
	if body["code"] != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", body["code"])
	}
	still := countRows(t, env.database, "tag_categories WHERE id = ?", catID)
	if still != 1 {
		t.Errorf("category rows = %d, want the category left intact", still)
	}
}

func TestDeleteCategory_BuiltinRejected(t *testing.T) {
	env := newTestEnv(t)
	id := categoryID(t, env, "general")
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/categories/%d", id), nil, http.StatusBadRequest)
}

func TestDeleteCategory_NotFound(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "DELETE", "/api/v1/categories/99999", nil, http.StatusNotFound)
}
