package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/tags"
)

func apiJSON(t *testing.T, env *testEnv, method, path string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	w := doJSON(env.mux, method, path, raw)
	if w.Code != wantStatus {
		t.Fatalf("%s %s: expected %d, got %d: %s", method, path, wantStatus, w.Code, w.Body.String())
	}
	var resp map[string]any
	if w.Body.Len() > 0 {
		mustDecode(t, w, &resp)
	}
	return resp
}

func TestCreateTag(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "newtag", "category": "artist"}, http.StatusCreated)
	if resp["name"] != "newtag" {
		t.Errorf("name = %v, want newtag", resp["name"])
	}
	if resp["category"] != "artist" {
		t.Errorf("category = %v, want artist", resp["category"])
	}
}

func TestCreateTag_DefaultsToGeneral(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "plaintag"}, http.StatusCreated)
	if resp["category"] != "general" {
		t.Errorf("category = %v, want general", resp["category"])
	}
}

func TestCreateTag_UnknownCategory(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "x", "category": "nope_xyz"}, http.StatusBadRequest)
	if resp["error"] != "unknown category: nope_xyz" || resp["code"] != "invalid_request" {
		t.Errorf("body = %v, want invalid_request \"unknown category: nope_xyz\"", resp)
	}
}

func TestCreateTag_MissingName(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{}, http.StatusBadRequest)
}

func TestCreateTag_ACategoryPrefixFilesTheTagThere(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "artist:foo", "category": "character"}, http.StatusCreated)
	if resp["name"] != "foo" || resp["category"] != "artist" {
		t.Errorf("tag = %v:%v, want artist:foo", resp["category"], resp["name"])
	}
	resp = apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "nope_xyz:bar"}, http.StatusCreated)
	if resp["name"] != "nope_xyz:bar" || resp["category"] != "general" {
		t.Errorf("tag = %v:%v, want general:nope_xyz:bar for a prefix naming no category", resp["category"], resp["name"])
	}
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "artist:"}, http.StatusBadRequest)
}

func TestPatchTag_APrefixedRenameMovesTheTag(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "before"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "artist:bar"}, http.StatusOK)
	if resp["name"] != "bar" || resp["category"] != "artist" {
		t.Errorf("tag = %v:%v, want artist:bar", resp["category"], resp["name"])
	}
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "character:"}, http.StatusBadRequest)
}

func TestPatchTag_Rename(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "before"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "after"}, http.StatusOK)
	if resp["name"] != "after" {
		t.Errorf("name = %v, want after", resp["name"])
	}
}

func TestPatchTag_ChangeCategory(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "movable"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"category": "character"}, http.StatusOK)
	if resp["category"] != "character" {
		t.Errorf("category = %v, want character", resp["category"])
	}
}

func TestPatchTag_UnknownCategoryLeavesTheNameAlone(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "kept_name"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "lost_name", "category": "nope_xyz"}, http.StatusBadRequest)
	resp := apiJSON(t, env, "GET", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusOK)
	if resp["name"] != "kept_name" {
		t.Errorf("name = %v, want kept_name: the refused call renamed the tag", resp["name"])
	}
}

func TestPatchTag_ACollisionLeavesTheNameAlone(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "clash", "category": "character"}, http.StatusCreated)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "orig"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "clash", "category": "character"}, http.StatusConflict)
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "rated", "category": "rating"}, http.StatusBadRequest)
	resp := apiJSON(t, env, "GET", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusOK)
	if resp["name"] != "orig" || resp["category"] != "general" {
		t.Errorf("tag = %v:%v, want general:orig: the refused call stored part of the change", resp["category"], resp["name"])
	}
}

func TestPatchTag_NoFields(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "x"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{}, http.StatusBadRequest)
}

func TestPatchTag_NotFound(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "PATCH", "/api/v1/tags/99999", map[string]any{"name": "x"}, http.StatusNotFound)
}

func TestGetTag_CarriesTheNoteAndLinks(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "noted", "category": "artist"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	if err := tags.New(env.database, nil).SetTagNote(id, "an [i]artist[/i]", []string{"https://a.example/1", "-https://b.example/2"}); err != nil {
		t.Fatal(err)
	}
	resp := apiJSON(t, env, "GET", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusOK)
	if resp["name"] != "noted" || resp["category"] != "artist" || resp["note"] != "an [i]artist[/i]" {
		t.Errorf("GET = %v", resp)
	}
	if links, _ := resp["links"].([]any); len(links) != 2 || links[1] != "-https://b.example/2" {
		t.Errorf("links = %v", resp["links"])
	}

	bare := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "bare"}, http.StatusCreated)
	resp = apiJSON(t, env, "GET", fmt.Sprintf("/api/v1/tags/%d", int64(bare["id"].(float64))), nil, http.StatusOK)
	if links, ok := resp["links"].([]any); !ok || len(links) != 0 || resp["note"] != "" {
		t.Errorf("a tag with no note answered note=%v links=%v", resp["note"], resp["links"])
	}
	apiJSON(t, env, "GET", "/api/v1/tags/99999", nil, http.StatusNotFound)
}

func TestGetTag_ListsTheTagsItIsMarkedDistinctFrom(t *testing.T) {
	env := newTestEnv(t)
	artist := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "kirin", "category": "artist"}, http.StatusCreated)
	character := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "kirin", "category": "character"}, http.StatusCreated)
	a, c := int64(artist["id"].(float64)), int64(character["id"].(float64))

	resp := apiJSON(t, env, "GET", fmt.Sprintf("/api/v1/tags/%d", a), nil, http.StatusOK)
	if got, ok := resp["distinct_from"].([]any); !ok || len(got) != 0 {
		t.Errorf("unmarked distinct_from = %v, want []", resp["distinct_from"])
	}
	if _, _, err := tags.New(env.database, nil).MarkDistinct([]int64{a, c}); err != nil {
		t.Fatal(err)
	}
	resp = apiJSON(t, env, "GET", fmt.Sprintf("/api/v1/tags/%d", a), nil, http.StatusOK)
	got, _ := resp["distinct_from"].([]any)
	if len(got) != 1 {
		t.Fatalf("distinct_from = %v, want one entry", resp["distinct_from"])
	}
	if ref, _ := got[0].(map[string]any); ref["id"] != float64(c) || ref["category"] != "character" {
		t.Errorf("distinct_from[0] = %v, want character #%d", got[0], c)
	}
}

func TestDeleteTag(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "doomed"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusNoContent)

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusNotFound)
}

func TestCreateAlias(t *testing.T) {
	env := newTestEnv(t)
	canon := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "canonical"}, http.StatusCreated)
	canonID := int64(canon["id"].(float64))

	resp := apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym", "canonical_id": canonID}, http.StatusCreated)
	if resp["is_alias"] != true {
		t.Errorf("is_alias = %v, want true", resp["is_alias"])
	}
	if resp["name"] != "synonym" {
		t.Errorf("name = %v, want synonym", resp["name"])
	}
}

func TestCreateAlias_ACategoryPrefixFilesTheAliasThere(t *testing.T) {
	env := newTestEnv(t)
	canon := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "canonical"}, http.StatusCreated)
	canonID := int64(canon["id"].(float64))

	resp := apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "artist:synonym", "canonical_id": canonID}, http.StatusCreated)
	if resp["name"] != "synonym" || resp["category"] != "artist" {
		t.Errorf("alias = %v:%v, want artist:synonym", resp["category"], resp["name"])
	}
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "artist:", "canonical_id": canonID}, http.StatusBadRequest)
}

func TestCreateAlias_AnAliasOfAnotherTagIsAConflict(t *testing.T) {
	env := newTestEnv(t)
	first := int64(apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "first"}, http.StatusCreated)["id"].(float64))
	second := int64(apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "second"}, http.StatusCreated)["id"].(float64))
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym", "canonical_id": first}, http.StatusCreated)

	resp := apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym", "canonical_id": second}, http.StatusConflict)
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "general:first") {
		t.Errorf("error = %q, want it to name general:first", msg)
	}
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym", "canonical_id": first}, http.StatusCreated)
}

func TestCreateAlias_MissingCanonical(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym"}, http.StatusBadRequest)
}

func TestMergeTags(t *testing.T) {
	env := newTestEnv(t)
	a := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "from_tag"}, http.StatusCreated)
	b := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "into_tag"}, http.StatusCreated)
	aID := int64(a["id"].(float64))
	bID := int64(b["id"].(float64))

	resp := apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": aID, "canonical_id": bID}, http.StatusOK)
	if int64(resp["id"].(float64)) != bID {
		t.Errorf("merge returned id %v, want canonical %d", resp["id"], bID)
	}
	if resp["is_alias"] != false {
		t.Errorf("canonical is_alias = %v, want false", resp["is_alias"])
	}

	var isAlias int
	var canonID int64
	if err := env.database.Read.QueryRow(
		`SELECT is_alias, COALESCE(canonical_tag_id, 0) FROM tags WHERE id = ?`, aID,
	).Scan(&isAlias, &canonID); err != nil {
		t.Fatal(err)
	}
	if isAlias != 1 || canonID != bID {
		t.Errorf("source tag is_alias=%d canonical=%d, want 1 and %d", isAlias, canonID, bID)
	}
}

func TestMergeTags_UnknownAliasIs404(t *testing.T) {
	env := newTestEnv(t)
	b := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "merge_target"}, http.StatusCreated)
	apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": 987654, "canonical_id": int64(b["id"].(float64))}, http.StatusNotFound)
}

func TestMergeTags_SelfRejected(t *testing.T) {
	env := newTestEnv(t)
	a := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "lonely"}, http.StatusCreated)
	aID := int64(a["id"].(float64))
	apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": aID, "canonical_id": aID}, http.StatusBadRequest)
}

func TestTagRefusalsAnswer400(t *testing.T) {
	env := newTestEnv(t)
	canonID := int64(apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "canon"}, http.StatusCreated)["id"].(float64))
	aliasID := int64(apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "syn", "canonical_id": canonID}, http.StatusCreated)["id"].(float64))
	otherID := int64(apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "other"}, http.StatusCreated)["id"].(float64))

	apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": otherID, "canonical_id": aliasID}, http.StatusBadRequest)
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "canon", "canonical_id": canonID}, http.StatusBadRequest)
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "syn2", "canonical_id": aliasID}, http.StatusBadRequest)
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", otherID), map[string]any{"implied_id": aliasID}, http.StatusBadRequest)
}

func TestMergeTags_ADatabaseErrorIs500(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "merge_fail.png", 10, 10)
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), []byte(`{"tags":["merge_src"]}`)), http.StatusOK)
	var srcID int64
	if err := env.database.Read.QueryRow(`SELECT id FROM tags WHERE name = 'merge_src'`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	dst := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "merge_dst"}, http.StatusCreated)
	mustExec(t, env.database.Write, `CREATE TRIGGER fail_tag_delete BEFORE DELETE ON image_tags BEGIN SELECT RAISE(ABORT, 'busy'); END`)
	apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": srcID, "canonical_id": int64(dst["id"].(float64))}, http.StatusInternalServerError)
}

func TestCreateTag_RejectedNameAnswersConflict(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "watermark"}, http.StatusCreated)
	if err := env.gallery.TagSvc.RejectTag(int64(resp["id"].(float64))); err != nil {
		t.Fatal(err)
	}
	resp = apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "watermark"}, http.StatusConflict)
	if resp["code"] != "rejected" {
		t.Errorf("code = %v, want rejected", resp["code"])
	}
	general, _, _ := tags.CategoryIDByName(env.database, "general")
	if tag, _ := env.gallery.TagSvc.FindTag("watermark", general); tag.RejectedDrops != 0 {
		t.Errorf("RejectedDrops = %d after a refused create, want 0", tag.RejectedDrops)
	}
	w := env.do("GET", "/api/v1/tags?type=rejected")
	wantCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `"rejected":true`)
	wantIn(t, w.Body.String(), `"name":"watermark"`)
	w = env.do("GET", "/api/v1/tags?type=tag")
	wantNotIn(t, w.Body.String(), `"name":"watermark"`)
}

func TestPushAndTagAdd_NameARejectedTagAndDropIt(t *testing.T) {
	env := newTestEnv(t)
	general, _, _ := tags.CategoryIDByName(env.database, "general")
	if _, err := env.gallery.TagSvc.RejectTagName("watermark", general); err != nil {
		t.Fatal(err)
	}
	id := env.createTestImage(t, "rejected_add.png", 10, 10)
	body, _ := json.Marshal(map[string]any{"tags": []string{"watermark", "kept", "Watermark"}})
	w := doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body)
	wantCode(t, w, http.StatusOK)
	var resp struct {
		Tags []struct {
			Name string `json:"name"`
		} `json:"tags"`
		Rejected []string `json:"rejected_tags"`
	}
	mustDecode(t, w, &resp)
	if len(resp.Rejected) != 1 || resp.Rejected[0] != "watermark" {
		t.Errorf("rejected_tags = %v, want [watermark]", resp.Rejected)
	}
	for _, tg := range resp.Tags {
		if tg.Name == "watermark" {
			t.Error("the rejected name landed on the image")
		}
	}

	body, _ = json.Marshal(map[string]any{"tags": []string{"watermark"}, "source": "danbooru"})
	w = doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/enrich", id), body)
	wantCode(t, w, http.StatusOK)
	var enrich struct {
		Merge struct {
			Rejected []string `json:"rejected_tags"`
		} `json:"merge"`
	}
	mustDecode(t, w, &enrich)
	if len(enrich.Merge.Rejected) != 1 {
		t.Errorf("enrich merge.rejected_tags = %v, want [watermark]", enrich.Merge.Rejected)
	}
	tag, _ := env.gallery.TagSvc.FindTag("watermark", general)
	if tag.RejectedDrops != 2 {
		t.Errorf("RejectedDrops = %d, want 2", tag.RejectedDrops)
	}
}

func TestResolveTags_RefusesANonCanonicalRating(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags/resolve", map[string]any{"names": []string{"rating:foo", "rating:general"}}, http.StatusOK)
	results := resp["results"].([]any)
	if row := results[0].(map[string]any); row["status"] != "refused" || row["reason"] == "" {
		t.Errorf("rating:foo = %v, want refused with a reason", row)
	}
	if row := results[1].(map[string]any); row["status"] == "refused" {
		t.Errorf("rating:general = %v, want it accepted", row)
	}
}

func TestResolveTags_ImpliesTheWholeClosure(t *testing.T) {
	env := newTestEnv(t)
	general, _, _ := tags.CategoryIDByName(env.database, "general")
	svc := env.gallery.TagSvc
	a, b, c := mustTag(t, svc, "aaa", general), mustTag(t, svc, "bbb", general), mustTag(t, svc, "ccc", general)
	for _, edge := range [][2]int64{{a.ID, b.ID}, {b.ID, c.ID}} {
		if _, err := svc.AddImplication(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	resp := apiJSON(t, env, "POST", "/api/v1/tags/resolve", map[string]any{"names": []string{"aaa", "ccc"}}, http.StatusOK)
	results := resp["results"].([]any)
	if imps := results[0].(map[string]any)["implies"].([]any); len(imps) != 2 || imps[0] != "bbb" || imps[1] != "ccc" {
		t.Errorf("aaa implies = %v, want [bbb ccc]", imps)
	}
	if imps := results[1].(map[string]any)["implies"].([]any); len(imps) != 0 {
		t.Errorf("ccc implies = %v, want none", imps)
	}
}

func TestResolveTags_SaysWhatArrivesAsWhat(t *testing.T) {
	env := newTestEnv(t)
	general, _, _ := tags.CategoryIDByName(env.database, "general")
	svc := env.gallery.TagSvc
	held := mustTag(t, svc, "white_background", general)
	simple := mustTag(t, svc, "simple_background", general)
	if _, err := svc.AddImplication(held.ID, simple.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAlias("white_bg", general, held.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RejectTagName("watermark", general); err != nil {
		t.Fatal(err)
	}
	resp := apiJSON(t, env, "POST", "/api/v1/tags/resolve", map[string]any{"names": []string{"white_background", "white_bg", "watermark", "never_seen", "artist:"}}, http.StatusOK)
	results := resp["results"].([]any)
	want := []struct{ status, resolves string }{{"exists", ""}, {"alias", "white_background"}, {"rejected", ""}, {"new", ""}, {"refused", ""}}
	for i, w := range want {
		row := results[i].(map[string]any)
		if row["status"] != w.status {
			t.Errorf("%v: status = %v, want %s", row["name"], row["status"], w.status)
		}
		if r, _ := row["resolves_to"].(string); r != w.resolves {
			t.Errorf("%v: resolves_to = %q, want %q", row["name"], r, w.resolves)
		}
	}
	if imps := results[0].(map[string]any)["implies"].([]any); len(imps) != 1 || imps[0] != "simple_background" {
		t.Errorf("implies = %v, want [simple_background]", imps)
	}
	if results[2].(map[string]any)["implies"].([]any) == nil {
		t.Error("a rejected name should still carry an empty implies list")
	}
	apiJSON(t, env, "POST", "/api/v1/tags/resolve", map[string]any{"names": []string{}}, http.StatusBadRequest)
}
