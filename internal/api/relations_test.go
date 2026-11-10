package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

func TestRelationsAddAndFetch(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11)

	body, _ := json.Marshal(map[string]any{
		"type": "duplicate",
		"a":    a,
		"b":    b,
	})
	w := doJSON(env.mux, "POST", "/api/v1/relations", body)
	mustCode(t, w, http.StatusCreated)

	w = env.do("GET", "/api/v1/images/"+strconv.FormatInt(a, 10)+"/relations")
	mustCode(t, w, http.StatusOK)
	var resp relationsResponse
	mustDecode(t, w, &resp)
	if resp.DuplicateGroup == nil {
		t.Fatal("expected duplicate_group in response")
	}
	if len(resp.DuplicateGroup.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(resp.DuplicateGroup.Members))
	}
}

func TestRelationsForImage_UnknownIDIs404(t *testing.T) {
	env := newTestEnv(t)
	w := env.do("GET", "/api/v1/images/9999/relations")
	mustCode(t, w, http.StatusNotFound)
}

func TestAddRelation_MissingImage404s(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "only.png", 10, 10)
	body, _ := json.Marshal(map[string]any{"type": "duplicate", "a": a, "b": 999999})
	w := doJSON(env.mux, "POST", "/api/v1/relations", body)
	mustCode(t, w, http.StatusNotFound)
}

func TestRelationsAddConflict(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11)

	post := func(typ string) int {
		body, _ := json.Marshal(map[string]any{"type": typ, "a": a, "b": b})
		w := doJSON(env.mux, "POST", "/api/v1/relations", body)
		return w.Code
	}
	if post("duplicate") != http.StatusCreated {
		t.Fatal("first POST should succeed")
	}
	if got := post("alternate"); got != http.StatusConflict {
		t.Fatalf("conflicting POST: got %d, want 409", got)
	}
}

func TestRelationsRemove(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11)

	body, _ := json.Marshal(map[string]any{"type": "duplicate", "a": a, "b": b})
	w := doJSON(env.mux, "POST", "/api/v1/relations", body)
	mustCode(t, w, http.StatusCreated)

	remBody, _ := json.Marshal(map[string]any{"type": "duplicate", "image_id": a})
	w = doJSON(env.mux, "DELETE", "/api/v1/relations", remBody)
	mustCode(t, w, http.StatusNoContent)

	w = env.do("GET", "/api/v1/images/"+strconv.FormatInt(b, 10)+"/relations")
	var resp relationsResponse
	mustDecode(t, w, &resp)
	if resp.DuplicateGroup != nil {
		t.Fatalf("dup group survived after only-pair removal: %+v", resp.DuplicateGroup)
	}
}

func TestRemoveRelation_EdgeTypesRequireAPair(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11)
	for _, typ := range []string{"version", "derivative", "not_related"} {
		for _, body := range []map[string]any{{"type": typ}, {"type": typ, "a": a}, {"type": typ, "b": b}} {
			raw, _ := json.Marshal(body)
			if w := doJSON(env.mux, "DELETE", "/api/v1/relations", raw); w.Code != http.StatusBadRequest {
				t.Errorf("%v: got %d, want 400: %s", body, w.Code, w.Body.String())
			}
		}
		raw, _ := json.Marshal(map[string]any{"type": typ, "a": a, "b": b})
		wantCode(t, doJSON(env.mux, "DELETE", "/api/v1/relations", raw), http.StatusNoContent)
	}
}

func TestRelationsPromoteOriginal(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11)

	body, _ := json.Marshal(map[string]any{"type": "duplicate", "a": a, "b": b})
	w := doJSON(env.mux, "POST", "/api/v1/relations", body)
	mustCode(t, w, http.StatusCreated)

	get := func(id int64) dupGroupJSON {
		w := env.do("GET", "/api/v1/images/"+strconv.FormatInt(id, 10)+"/relations")
		var resp relationsResponse
		mustDecode(t, w, &resp)
		if resp.DuplicateGroup == nil {
			t.Fatal("no dup group")
		}
		return *resp.DuplicateGroup
	}

	grp := get(a)
	target := a
	if grp.Original == a {
		target = b
	}
	remBody, _ := json.Marshal(map[string]any{"type": "promote_original", "group_id": grp.ID, "image_id": target})
	w = doJSON(env.mux, "DELETE", "/api/v1/relations", remBody)
	mustCode(t, w, http.StatusNoContent)
	if got := get(a).Original; got != target {
		t.Errorf("original_image_id = %d, want %d", got, target)
	}
}

func TestRelationsAddSelfRefused(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTestImage(t, "a.png", 10, 10)
	body, _ := json.Marshal(map[string]any{"type": "duplicate", "a": a, "b": a})
	w := doJSON(env.mux, "POST", "/api/v1/relations", body)
	mustCode(t, w, http.StatusBadRequest)
}

func TestRelationsAddVersionDirection(t *testing.T) {
	env := newTestEnv(t)
	parent := env.createTestImage(t, "parent.png", 10, 10)
	child := env.createTestImage(t, "child.png", 11, 11)

	body, _ := json.Marshal(map[string]any{
		"type": "version",
		"a":    parent,
		"b":    child,
	})
	w := doJSON(env.mux, "POST", "/api/v1/relations", body)
	mustCode(t, w, http.StatusCreated)

	w = env.do("GET", "/api/v1/images/"+strconv.FormatInt(parent, 10)+"/relations")
	var resp relationsResponse
	mustDecode(t, w, &resp)
	if resp.VersionChild == nil || *resp.VersionChild != child {
		t.Fatalf("parent.VersionChild = %v, want %d", resp.VersionChild, child)
	}
	if resp.VersionParent != nil {
		t.Fatalf("parent.VersionParent = %v, want nil", resp.VersionParent)
	}
}

func TestImageGetIncludesPhashField(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "phash.png", 32, 32)
	w := env.do("GET", "/api/v1/images/"+strconv.FormatInt(id, 10))
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"phash"`)
}

func TestSearchImages_IncludesPhashField(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "phash_search.png", 32, 32)
	mustExec(t, env.database.Write, `UPDATE images SET phash = ? WHERE id = ?`, int64(0x19fb19d7190419fa), id)

	getRec := env.do("GET", "/api/v1/images/"+strconv.FormatInt(id, 10))
	var singular map[string]any
	if err := json.NewDecoder(getRec.Body).Decode(&singular); err != nil {
		t.Fatalf("decode singular: %v", err)
	}
	wantPhash, _ := singular["phash"].(string)
	if wantPhash == "" {
		t.Fatalf("singular GET should populate phash, got %v", singular["phash"])
	}

	searchRec := env.do("GET", "/api/v1/images/search?q=id:"+strconv.FormatInt(id, 10))
	var resp map[string]any
	if err := json.NewDecoder(searchRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	results, _ := resp["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("search returned no results for id:%d", id)
	}
	gotPhash, _ := results[0].(map[string]any)["phash"].(string)
	if gotPhash != wantPhash {
		t.Errorf("search phash = %q, want %q (matches singular GET)", gotPhash, wantPhash)
	}
}
