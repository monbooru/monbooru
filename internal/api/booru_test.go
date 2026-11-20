package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
)

func TestBooruKey(t *testing.T) {
	a, b := BooruKey(apitoken.Hash("secret_a")), BooruKey(apitoken.Hash("secret_b"))
	if len(a) != 32 || a == b || a != BooruKey(apitoken.Hash("secret_a")) {
		t.Errorf("keys %q and %q: want 32 hex characters, one per token, the same every time", a, b)
	}
	if strings.Contains(apitoken.Hash("secret_a"), a) {
		t.Error("the key must not be part of the stored hash")
	}
}

func TestBooruToken(t *testing.T) {
	cases := []struct {
		name string
		tok  apitoken.Token
		want bool
	}{
		{"a read token", apitoken.Token{Scopes: []string{config.ScopeRead}}, true},
		{"all scopes", apitoken.Token{Scopes: config.AllScopes}, true},
		{"without read", apitoken.Token{Scopes: []string{config.ScopeWrite}}, false},
		{"paired to an app", apitoken.Token{Scopes: config.AllScopes, Paired: "monloader"}, false},
	}
	for _, tc := range cases {
		if got := BooruToken(tc.tok); got != tc.want {
			t.Errorf("%s: BooruToken = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBooruPaths(t *testing.T) {
	cases := []struct {
		path, redacted string
		file           bool
	}{
		{"/booru/main/0123abcd/posts.json", "/booru/main/<key>/posts.json", false},
		{"/booru/main/0123abcd/thumb/7.jpg", "/booru/main/<key>/thumb/7.jpg", true},
		{"/booru/main/0123abcd/file/7.png", "/booru/main/<key>/file/7.png", true},
		{"/booru/main/0123abcd/sample/7.jpg", "/booru/main/<key>/sample/7.jpg", true},
		{"/booru/file/0123abcd/posts.json", "/booru/file/<key>/posts.json", false},
		{"/booru/thumb/0123abcd/thumb/7.jpg", "/booru/thumb/<key>/thumb/7.jpg", true},
		{"/booru/main/0123abcd/", "/booru/main/<key>/", false},
		{"/api/v1/images/file", "/api/v1/images/file", false},
	}
	for _, tc := range cases {
		if got := RedactBooruPath(tc.path); got != tc.redacted {
			t.Errorf("RedactBooruPath(%q) = %q, want %q", tc.path, got, tc.redacted)
		}
		if got := IsBooruFile(tc.path); got != tc.file {
			t.Errorf("IsBooruFile(%q) = %v, want %v", tc.path, got, tc.file)
		}
	}
}

func TestTranslateBooruQuery(t *testing.T) {
	vocab := booruVocabulary{
		known: func(key string) bool { return slices.Contains([]string{"width", "date", "md5", "artist"}, key) },
		tag:   func(name string) bool { return name == "re:zero" },
	}
	cases := []struct {
		in, text, sort, order string
	}{
		{"", "", "newest", "desc"},
		{"blue_sky -cloud", "blue_sky -cloud", "newest", "desc"},
		{"~cat ~dog sky", "sky ( cat OR dog )", "newest", "desc"},
		{"~cat", "cat", "newest", "desc"},
		{"rating:g", "rating:general", "newest", "desc"},
		{"rating:safe", "rating:sensitive", "newest", "desc"},
		{"-rating:q,E", "-( rating:questionable OR rating:explicit )", "newest", "desc"},
		{"rating:nope", "", "newest", "desc"},
		{"id:1,2,3", "( id:1 OR id:2 OR id:3 )", "newest", "desc"},
		{"id:>5", "", "newest", "desc"},
		{"filetype:jpg,zip", "( mime:jpeg OR mime:cbz )", "newest", "desc"},
		{"filesize:1mb..2mb", "size:1mb..2mb", "newest", "desc"},
		{"source:https://a.example/*", "sourceurl:https://a.example/*", "newest", "desc"},
		{"fav:user_a", "fav:true", "newest", "desc"},
		{"ratio:16:9", "ratio:1.7777777777777777", "newest", "desc"},
		{"ratio:>=4:3", "ratio:>=1.3333333333333333", "newest", "desc"},
		{"ratio:1:1..2:1", "ratio:1..2", "newest", "desc"},
		{"ratio:1.5", "ratio:1.5", "newest", "desc"},
		{"ratio:1:0", "ratio:1:0", "newest", "desc"},
		{"ratio:..16:9", "ratio:..1.7777777777777777", "newest", "desc"},
		{"ratio:<1:2", "ratio:<0.5", "newest", "desc"},
		{"ratio:a:9", "ratio:a:9", "newest", "desc"},
		{"score:>5 user:user_a status:any", "", "newest", "desc"},
		{"status:Active sky", "sky", "newest", "desc"},
		{"status:deleted", "id:0", "newest", "desc"},
		{"-status:deleted", "-id:0", "newest", "desc"},
		{"status:nope", "id:0", "newest", "desc"},
		{"width:>100 date:2024-01-01..2024-02-01 artist:user_a", "width:>100 date:2024-01-01..2024-02-01 artist:user_a", "newest", "desc"},
		{"re:zero -re:zero", "re:zero -re:zero", "newest", "desc"},
		{"order:id", "", "newest", "asc"},
		{"order:filesize_asc sky", "sky", "filesize", "asc"},
		{"order:random", "", "random", "desc"},
		{"order:score", "", "newest", "desc"},
		{"-order:id", "", "newest", "desc"},
		{"- ~", "- ~", "newest", "desc"},
		{"*", "", "newest", "desc"},
		{"** sky", "sky", "newest", "desc"},
		{"sky*", "sky*", "newest", "desc"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := translateBooruQuery(tc.in, vocab)
			if got.text != tc.text || got.sort != tc.sort || got.order != tc.order {
				t.Errorf("translate(%q) = %q sort %s %s, want %q sort %s %s", tc.in, got.text, got.sort, got.order, tc.text, tc.sort, tc.order)
			}
		})
	}
}

func TestBooruObjectRoute(t *testing.T) {
	for route, want := range map[string]bool{
		"profile": true, "users/12": true, "comments/5": true, "wiki_pages/sky": true, "related_tag": true,
		"pools": false, "forum_topics": false, "explore/posts/popular": false, "comments": false,
	} {
		if got := booruObjectRoute(route); got != want {
			t.Errorf("booruObjectRoute(%q) = %v, want %v", route, got, want)
		}
	}
}

// A gallery with one tagged, noted image behind the test token's address.
func booruEnv(t *testing.T) (*testEnv, string, int64) {
	t.Helper()
	env := newTestEnv(t)
	env.cfg.Auth.BooruTokenID = env.cfg.Auth.Tokens[0].ID
	env.cfg.FindGallery(env.cfg.DefaultGallery).Booru = true
	id := env.createTestImage(t, "booru.png", 40, 30)
	body, _ := json.Marshal(map[string]any{"tags": []string{"blue_sky", "artist:user_a", "character:char_a", "rating:explicit"}})
	mustCode(t, doJSON(env.mux, "POST", fmt.Sprintf("/api/v1/images/%d/tags", id), body), http.StatusOK)
	mustExec(t, env.database.Write, `INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual) VALUES (?, '', '', 1, 2, 3, 4, 'a note', 1)`, id)
	return env, BooruPath(env.cfg.DefaultGallery, BooruKey(apitoken.Hash(testAPIToken))), id
}

func TestBooruAuth(t *testing.T) {
	env, base, _ := booruEnv(t)
	gallery := env.cfg.DefaultGallery
	cases := []struct {
		name, path string
		code       int
	}{
		{"the address", base + "/posts.json", http.StatusOK},
		{"a wrong key", BooruPath(gallery, strings.Repeat("0", 32)) + "/posts.json", http.StatusUnauthorized},
		{"a wrong key on a file", BooruPath(gallery, strings.Repeat("0", 32)) + "/thumb/1.jpg", http.StatusUnauthorized},
		{"an unknown gallery", BooruPath("no_such", BooruKey(apitoken.Hash(testAPIToken))) + "/posts.json", http.StatusNotFound},
	}
	for _, tc := range cases {
		if w := env.do("GET", tc.path); w.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.code)
		}
	}
	if w := env.do("POST", base+"/posts.json"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("a write answered %d, want 405", w.Code)
	}

	// Unpicking the token, or taking its read away, turns its address off.
	env.cfg.Auth.BooruTokenID = ""
	if w := env.do("GET", base+"/posts.json"); w.Code != http.StatusUnauthorized {
		t.Errorf("no token picked: %d, want 401", w.Code)
	}
	env.cfg.Auth.BooruTokenID = env.cfg.Auth.Tokens[0].ID
	env.cfg.Auth.Tokens[0].Scopes = []string{config.ScopeWrite}
	if w := env.do("GET", base+"/posts.json"); w.Code != http.StatusUnauthorized {
		t.Errorf("a token without read: %d, want 401", w.Code)
	}

	// A gallery not turned on answers as an unknown one, files too.
	env.cfg.Auth.Tokens[0].Scopes = []string{config.ScopeRead}
	env.cfg.FindGallery(gallery).Booru = false
	unknown := env.do("GET", BooruPath("no_such", BooruKey(apitoken.Hash(testAPIToken)))+"/posts.json").Body.String()
	for _, path := range []string{"/posts.json", "/thumb/1.jpg"} {
		w := env.do("GET", base+path)
		if w.Code != http.StatusNotFound || w.Body.String() != unknown {
			t.Errorf("a gallery turned off, %s: %d %s, want 404 %s", path, w.Code, w.Body.String(), unknown)
		}
	}
}

func TestChosenBooruToken(t *testing.T) {
	reader := apitoken.Token{ID: "reader", TokenHash: apitoken.Hash("secret_a"), Scopes: []string{config.ScopeRead}}
	other := apitoken.Token{ID: "other", TokenHash: apitoken.Hash("secret_b"), Scopes: []string{config.ScopeRead}}
	writer := apitoken.Token{ID: "writer", Scopes: []string{config.ScopeWrite}}
	paired := apitoken.Token{ID: "paired", Scopes: config.AllScopes, Paired: "monloader"}
	tokens := apitoken.List{reader, other, writer, paired}
	cases := []struct {
		name, picked, want string
	}{
		{"none picked", "", ""},
		{"the picked one only", "other", "other"},
		{"one without read", "writer", ""},
		{"a paired one", "paired", ""},
		{"one revoked since", "gone", ""},
	}
	for _, tc := range cases {
		got := ChosenBooruToken(config.AuthConfig{Tokens: tokens, BooruTokenID: tc.picked})
		if id := ""; got != nil {
			id = got.ID
			if id != tc.want {
				t.Errorf("%s: chose %q, want %q", tc.name, id, tc.want)
			}
		} else if tc.want != "" {
			t.Errorf("%s: chose none, want %q", tc.name, tc.want)
		}
	}
	if booruKeyValid(config.AuthConfig{Tokens: tokens, BooruTokenID: "other"}, BooruKey(reader.TokenHash)) {
		t.Error("a read token that is not the picked one opened the layer")
	}
}

func TestBooruPosts(t *testing.T) {
	env, base, id := booruEnv(t)
	other := env.createTestImage(t, "other.png", 20, 20)

	cases := []struct {
		query string
		want  []int64
	}{
		{"", []int64{other, id}},
		{"?tags=blue_sky", []int64{id}},
		{"?tags=rating:e", []int64{id}},
		{"?tags=-blue_sky", []int64{other}},
		{"?tags=order:id", []int64{id, other}},
		{"?tags=ratio:4:3", []int64{id}},
		{"?tags=score:>5+blue_sky", []int64{id}},
		{"?limit=1", []int64{other}},
		{"?limit=1&page=2", []int64{id}},
		{fmt.Sprintf("?page=b%d", id), nil},
		{"?page=a1", nil},
		{"?page=bx", []int64{other, id}},
		// The search one app tests a new site with.
		{"?tags=*&page=1&limit=30", []int64{other, id}},
		{fmt.Sprintf("?tags=id:%d,%d", id, other), []int64{other, id}},
		{"?tags=status:deleted", nil},
		{"?tags=-status:deleted", []int64{other, id}},
	}
	for _, tc := range cases {
		w := env.do("GET", base+"/posts.json"+tc.query)
		mustCode(t, w, http.StatusOK)
		var posts []booruPost
		mustDecode(t, w, &posts)
		var got []int64
		for _, p := range posts {
			got = append(got, p.ID)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("posts.json%s = %v, want %v", tc.query, got, tc.want)
		}
	}

	w := env.do("GET", fmt.Sprintf("%s/posts/%d.json", base, id))
	mustCode(t, w, http.StatusOK)
	var p booruPost
	mustDecode(t, w, &p)
	host := "http://example.com" + base
	checks := []struct {
		field     string
		got, want any
	}{
		{"rating", p.Rating, "e"},
		// Ingest adds the small, plain image's meta tags.
		{"tag_string", p.TagString, "blue_sky char_a greyscale lowres user_a"},
		{"tag_string_general", p.TagStringGeneral, "blue_sky"},
		{"tag_string_artist", p.TagStringArtist, "user_a"},
		{"tag_string_character", p.TagStringCharacter, "char_a"},
		{"tag_string_meta", p.TagStringMeta, "greyscale lowres"},
		{"tag_count", p.TagCount, 5},
		{"tag_count_meta", p.TagCountMeta, 2},
		{"size", fmt.Sprint(p.ImageWidth, "x", p.ImageHeight), "40x30"},
		{"file_ext", p.FileExt, "png"},
		{"file_url", p.FileURL, fmt.Sprintf("%s/file/%d.png", host, id)},
		{"large_file_url", p.LargeFileURL, p.FileURL},
		{"preview_file_url", p.PreviewFileURL, fmt.Sprintf("%s/thumb/%d.jpg", host, id)},
		{"noted", p.LastNotedAt != nil && *p.LastNotedAt == p.CreatedAt, true},
		{"time zone", strings.HasSuffix(p.CreatedAt, "+00:00") || p.CreatedAt[len(p.CreatedAt)-6] == '+' || p.CreatedAt[len(p.CreatedAt)-6] == '-', true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}

	for _, path := range []string{"/posts/999999.json", "/posts/x.json", "/posts/x", "/posts/random.json?tags=no_such_tag"} {
		if w := env.do("GET", base+path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}

	// The post's page, without .json, as one app tests a new site.
	pages := []struct {
		path, want string
	}{
		{fmt.Sprintf("/posts/%d", id), fmt.Sprintf(`"id":%d`, id)},
		{"/posts/100", `{}`},
	}
	for _, tc := range pages {
		w := env.do("GET", base+tc.path)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want 200 with %s", tc.path, w.Code, w.Body.String(), tc.want)
		}
	}

	randoms := []struct {
		query string
		want  []int64
	}{
		{"", []int64{id, other}},
		{"?tags=blue_sky", []int64{id}},
		{"?tags=-blue_sky", []int64{other}},
		{"?tags=rating:e", []int64{id}},
	}
	var md5 string
	if err := env.database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, id).Scan(&md5); err != nil || md5 == "" {
		t.Fatalf("md5 of the image: %q, %v", md5, err)
	}
	md5s := []struct {
		md5  string
		code int
	}{
		{md5, http.StatusOK},
		{strings.ToUpper(md5), http.StatusOK},
		{strings.Repeat("0", 32), http.StatusNotFound},
	}
	for _, tc := range md5s {
		w := env.do("GET", base+"/posts.json?tags=-blue_sky&md5="+tc.md5)
		if w.Code != tc.code {
			t.Errorf("posts.json?md5=%s: %d, want %d", tc.md5, w.Code, tc.code)
			continue
		}
		if tc.code == http.StatusOK {
			var p booruPost
			mustDecode(t, w, &p)
			if p.ID != id {
				t.Errorf("posts.json?md5=%s = post %d, want %d", tc.md5, p.ID, id)
			}
		}
	}

	for _, tc := range randoms {
		w := env.do("GET", base+"/posts/random.json"+tc.query)
		mustCode(t, w, http.StatusOK)
		var p booruPost
		mustDecode(t, w, &p)
		if !slices.Contains(tc.want, p.ID) {
			t.Errorf("posts/random.json%s = %d, want one of %v", tc.query, p.ID, tc.want)
		}
	}
}

// The JSON type of each field of a real Danbooru post. media_asset carries
// no variants here, which the apps fall back from to the three URLs.
var danbooruPostTypes = map[string]string{
	"approver_id": "null", "bit_flags": "number", "created_at": "string", "down_score": "number",
	"fav_count": "number", "file_ext": "string", "file_size": "number", "file_url": "string",
	"has_active_children": "bool", "has_children": "bool", "has_large": "bool", "has_visible_children": "bool",
	"id": "number", "image_height": "number", "image_width": "number", "is_banned": "bool",
	"is_deleted": "bool", "is_flagged": "bool", "is_pending": "bool", "large_file_url": "string",
	"last_comment_bumped_at": "null", "last_commented_at": "null", "last_noted_at": "null", "md5": "string",
	"media_asset": "object", "parent_id": "null", "pixiv_id": "null", "preview_file_url": "string",
	"rating": "string", "score": "number", "source": "string", "tag_count": "number",
	"tag_count_artist": "number", "tag_count_character": "number", "tag_count_copyright": "number",
	"tag_count_general": "number", "tag_count_meta": "number", "tag_string": "string",
	"tag_string_artist": "string", "tag_string_character": "string", "tag_string_copyright": "string",
	"tag_string_general": "string", "tag_string_meta": "string", "up_score": "number", "updated_at": "string",
	"uploader_id": "number",
}

var danbooruMediaAssetTypes = map[string]string{
	"created_at": "string", "duration": "null", "file_ext": "string", "file_key": "string", "file_size": "number",
	"id": "number", "image_height": "number", "image_width": "number", "is_public": "bool", "md5": "string",
	"pixel_hash": "string", "status": "string", "updated_at": "string",
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	}
	return "array"
}

func TestBooruPostMatchesDanbooruTypes(t *testing.T) {
	env, base, _ := booruEnv(t)
	plain := env.createTestImage(t, "plain.png", 20, 20)
	w := env.do("GET", fmt.Sprintf("%s/posts/%d.json", base, plain))
	mustCode(t, w, http.StatusOK)
	var post map[string]any
	mustDecode(t, w, &post)
	compare := func(where string, got map[string]any, want map[string]string) {
		for field, kind := range want {
			v, ok := got[field]
			if !ok {
				t.Errorf("%s%s is missing", where, field)
			} else if jsonKind(v) != kind {
				t.Errorf("%s%s is %s, Danbooru's is %s", where, field, jsonKind(v), kind)
			}
		}
		for field := range got {
			if _, ok := want[field]; !ok {
				t.Errorf("%s%s is not a Danbooru field", where, field)
			}
		}
	}
	compare("", post, danbooruPostTypes)
	if asset, ok := post["media_asset"].(map[string]any); ok {
		compare("media_asset.", asset, danbooruMediaAssetTypes)
	}
}

func TestBooruLookups(t *testing.T) {
	env, base, id := booruEnv(t)
	cases := []struct {
		name, path, want string
	}{
		{"count", "/counts/posts.json?tags=blue_sky", `{"counts":{"posts":1}}`},
		{"count none", "/counts/posts.json?tags=no_such_tag", `{"counts":{"posts":0}}`},
		{"autocomplete", "/autocomplete.json?search[query]=blue&search[type]=tag_query&limit=5",
			`[{"type":"tag","label":"blue sky","value":"blue_sky","category":0,"post_count":1}]`},
		{"autocomplete in wildcards", "/autocomplete.json?search[query]=*user*",
			`[{"type":"tag","label":"user a","value":"user_a","category":1,"post_count":1}]`},
		{"autocomplete leaves ratings out", "/autocomplete.json?search[query]=expl", `[]`},
		{"tags by name", "/tags.json?search[name_comma]=user_a,char_a,explicit&search[order]=count",
			`[{"id":%d,"name":"char_a","post_count":1,"category":4,"created_at":null,"updated_at":null,"is_deprecated":false,"words":["char","a"]},{"id":%d,"name":"user_a","post_count":1,"category":1,"created_at":null,"updated_at":null,"is_deprecated":false,"words":["user","a"]}]`},
		{"notes", fmt.Sprintf("/notes.json?search[post_id]=%d", id),
			`[{"id":%d,"created_at":null,"updated_at":null,"x":1,"y":2,"width":3,"height":4,"is_active":true,"post_id":%d,"body":"a note","version":1}]`},
		{"notes of nothing", "/notes.json", `[]`},
		{"a list route", "/pools.json?search[name_matches]=x", `[]`},
		{"a record route", "/profile.json", `{}`},
		{"a record by id", "/users/3.json", `{}`},
	}
	tagID := func(name string) int64 {
		var n int64
		if err := env.database.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var noteID int64
	if err := env.database.Read.QueryRow(`SELECT id FROM image_annotations WHERE image_id = ?`, id).Scan(&noteID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		want := tc.want
		switch tc.name {
		case "tags by name":
			want = fmt.Sprintf(want, tagID("char_a"), tagID("user_a"))
		case "notes":
			want = fmt.Sprintf(want, noteID, id)
		}
		w := env.do("GET", base+tc.path)
		mustCode(t, w, http.StatusOK)
		if got := strings.TrimSpace(w.Body.String()); got != want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, want)
		}
	}
	if w := env.do("GET", base+"/posts"); w.Code != http.StatusNotFound {
		t.Errorf("a route without .json: %d, want 404", w.Code)
	}
}

func TestBooruTagNamesCap(t *testing.T) {
	env, base, _ := booruEnv(t)
	filler := func(n int) []string {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("no_such_%d", i)
		}
		return names
	}
	cases := []struct {
		name  string
		names []string
		found bool
	}{
		{"last within the cap", append(filler(booruMaxNames-1), "blue_sky"), true},
		{"past the cap", append(filler(booruMaxNames), "blue_sky"), false},
		{"repeats take no room", append(slices.Repeat([]string{"no_such_0"}, 5000), "blue_sky"), true},
	}
	for _, tc := range cases {
		w := env.do("GET", base+"/tags.json?search[name_comma]="+strings.Join(tc.names, ","))
		mustCode(t, w, http.StatusOK)
		if got := strings.Contains(w.Body.String(), `"name":"blue_sky"`); got != tc.found {
			t.Errorf("%s: blue_sky found = %v, want %v", tc.name, got, tc.found)
		}
	}
}

func TestBooruFavicon(t *testing.T) {
	env, base, _ := booruEnv(t)
	if w := env.do("GET", base+"/favicon.ico"); w.Code != http.StatusNotFound {
		t.Errorf("favicon.ico with no icon given: %d, want 404", w.Code)
	}
	env.handler.WithFavicon(func() string { return "/static/favicon.png?v=1" })
	w := env.do("GET", base+"/favicon.ico")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/static/favicon.png?v=1" {
		t.Errorf("favicon.ico: %d to %q, want 302 to the web UI's icon", w.Code, w.Header().Get("Location"))
	}
	wrong := BooruPath(env.cfg.DefaultGallery, strings.Repeat("0", 32)) + "/favicon.ico"
	if w := env.do("GET", wrong); w.Code != http.StatusUnauthorized {
		t.Errorf("favicon.ico under a wrong key: %d, want 401", w.Code)
	}
}

func TestBooruFiles(t *testing.T) {
	env, base, id := booruEnv(t)
	cases := []struct {
		path        string
		code        int
		contentType string
	}{
		{fmt.Sprintf("/file/%d.png", id), http.StatusOK, "image/png"},
		{fmt.Sprintf("/thumb/%d.jpg", id), http.StatusOK, "image/jpeg"},
		{fmt.Sprintf("/sample/%d.jpg", id), http.StatusOK, "image/png"},
		{"/file/999999.png", http.StatusNotFound, ""},
		{"/thumb/x.jpg", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		w := env.do("GET", base+tc.path)
		if w.Code != tc.code || tc.contentType != "" && w.Header().Get("Content-Type") != tc.contentType {
			t.Errorf("%s: %d %q, want %d %q", tc.path, w.Code, w.Header().Get("Content-Type"), tc.code, tc.contentType)
		}
	}

	// An app that has the bytes gets a 304, and other bytes another tag.
	for _, path := range []string{fmt.Sprintf("/file/%d.png", id), fmt.Sprintf("/thumb/%d.jpg", id)} {
		w := env.do("GET", base+path)
		tag := w.Header().Get("ETag")
		if tag == "" || w.Header().Get("Cache-Control") != "private, no-cache" {
			t.Errorf("%s: ETag %q, Cache-Control %q", path, tag, w.Header().Get("Cache-Control"))
			continue
		}
		req := httptest.NewRequest("GET", base+path, nil)
		req.Header.Set("If-None-Match", tag)
		again := httptest.NewRecorder()
		env.mux.ServeHTTP(again, req)
		if again.Code != http.StatusNotModified {
			t.Errorf("%s with its ETag: %d, want 304", path, again.Code)
		}
	}
	if a, b := env.do("GET", fmt.Sprintf("%s/file/%d.png", base, id)), env.do("GET", fmt.Sprintf("%s/thumb/%d.jpg", base, id)); a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Error("the file and its thumbnail share an ETag")
	}

	// Past the web UI's pixel cap, the sample is its cached JPEG.
	mustExec(t, env.database.Write, `UPDATE images SET width = 20000, height = 10000 WHERE id = ?`, id)
	w := env.do("GET", fmt.Sprintf("%s/sample/%d.jpg", base, id))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("oversized sample: %d %q, want 200 image/jpeg", w.Code, w.Header().Get("Content-Type"))
	}
	if _, err := os.Stat(filepath.Join(env.thumbDir, fmt.Sprintf("%d_view.jpg", id))); err != nil {
		t.Errorf("the sample was not kept: %v", err)
	}
}

// lockSeen notes whether the gallery lock was held when the answer began.
type lockSeen struct {
	*httptest.ResponseRecorder
	held     *bool
	began    bool
	heldThen bool
}

func (l *lockSeen) WriteHeader(code int) {
	if !l.began {
		l.began, l.heldThen = true, *l.held
	}
	l.ResponseRecorder.WriteHeader(code)
}

func (l *lockSeen) Write(b []byte) (int, error) {
	l.WriteHeader(http.StatusOK)
	return l.ResponseRecorder.Write(b)
}

func TestBooruLetsGoOfTheLockForFiles(t *testing.T) {
	env, base, id := booruEnv(t)
	var held bool
	env.handler.WithGalleryLock(func() func() {
		held = true
		return func() { held = false }
	})
	cases := []struct {
		path string
		held bool
	}{
		{"/posts.json", true},
		{fmt.Sprintf("/file/%d.png", id), false},
		{fmt.Sprintf("/thumb/%d.jpg", id), false},
		{fmt.Sprintf("/sample/%d.jpg", id), false},
	}
	for _, tc := range cases {
		w := &lockSeen{ResponseRecorder: httptest.NewRecorder(), held: &held}
		env.mux.ServeHTTP(w, httptest.NewRequest("GET", base+tc.path, nil))
		if w.Code != http.StatusOK || w.heldThen != tc.held {
			t.Errorf("%s: %d, lock held = %v, want 200 and %v", tc.path, w.Code, w.heldThen, tc.held)
		}
		if held {
			t.Errorf("%s: the lock was never let go", tc.path)
		}
	}
}

// A rendition that cannot be built falls back to the file, as in the web UI.
func TestBooruSampleFallsBackToTheFile(t *testing.T) {
	env, base, id := booruEnv(t)
	var canonPath string
	if err := env.database.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonPath, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, env.database.Write, `UPDATE images SET width = 20000, height = 10000 WHERE id = ?`, id)
	w := env.do("GET", fmt.Sprintf("%s/sample/%d.jpg", base, id))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != "not an image" {
		t.Errorf("sample of an undecodable image: %d %q %q, want 200 image/png and the file", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

// An app declaring duration a double fails on a bare 5, so it is 5.0.
func TestBooruDuration(t *testing.T) {
	seconds := func(f float64) *float64 { return &f }
	cases := []struct {
		name string
		dur  *float64
		want string
	}{
		{"whole seconds", seconds(5), `"duration":5.0`},
		{"no length", seconds(0), `"duration":0.0`},
		{"a fraction", seconds(5.005), `"duration":5.005`},
		{"half", seconds(12.5), `"duration":12.5`},
		{"none measured", nil, `"duration":null`},
	}
	for _, tc := range cases {
		b, err := json.Marshal(booruPostOf("http://a.example", models.Image{ID: 1, FileType: models.FileTypeMP4, DurationSec: tc.dur}, nil, false))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("%s: post has no %s in %s", tc.name, tc.want, b)
		}
	}
}

func TestBooruSampled(t *testing.T) {
	cases := []struct {
		name          string
		fileType      string
		width, height int
		want          bool
	}{
		{"a plain image", models.FileTypePNG, 1000, 1000, false},
		{"an image past the cap", models.FileTypePNG, 20000, 10000, true},
		{"an AVIF", models.FileTypeAVIF, 100, 100, true},
		{"an archive", models.FileTypeCBZ, 0, 0, true},
		{"a large video", models.FileTypeMP4, 20000, 10000, false},
	}
	for _, tc := range cases {
		if got := booruSampled(tc.fileType, tc.width, tc.height); got != tc.want {
			t.Errorf("%s: sampled = %v, want %v", tc.name, got, tc.want)
		}
	}
}
