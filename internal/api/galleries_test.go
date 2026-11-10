package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
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

func TestListGalleriesAnswers500WhenACountFails(t *testing.T) {
	env := newTestEnv(t)
	if err := env.database.Read.Close(); err != nil {
		t.Fatal(err)
	}
	w := env.do("GET", "/api/v1/galleries")
	wantCode(t, w, http.StatusInternalServerError)
	wantIn(t, w.Body.String(), `"internal_error"`)
}
