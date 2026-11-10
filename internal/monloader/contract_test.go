package monloader

import (
	"cmp"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func monloaderDoc(t *testing.T) map[string]any {
	t.Helper()
	path := cmp.Or(os.Getenv("MONBOORU_TEST_MONLOADER_OPENAPI"), filepath.Join("testdata", "monloader-openapi.json"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

func TestMonloaderServesEveryRouteThisClientCalls(t *testing.T) {
	doc := monloaderDoc(t)
	for _, r := range monloaderRoutes {
		if dig(doc, "paths", r.Path, strings.ToLower(r.Method)) == nil {
			t.Errorf("monloader serves no %s %s", r.Method, r.Path)
		}
	}
}

func TestEveryMonloaderCallIsAListedRoute(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	literal := regexp.MustCompile(`"(/api/v1/[^"?]*)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			if !listedRoute(m[1]) {
				t.Errorf("%s calls %s, which monloaderRoutes does not list", f, m[1])
			}
		}
	}
}

func listedRoute(lit string) bool {
	for _, r := range monloaderRoutes {
		prefix, _, templated := strings.Cut(r.Path, "{")
		if lit == r.Path || templated && lit == prefix {
			return true
		}
	}
	return false
}

func TestMonloaderDocumentsEveryFieldThisClientUses(t *testing.T) {
	doc := monloaderDoc(t)
	for _, x := range []struct {
		method, path string
		sent, read   reflect.Type
	}{
		{"post", "/api/v1/metadata", reflect.TypeFor[contract.ImageJobRequest](), reflect.TypeFor[contract.JobAccepted]()},
		{"post", "/api/v1/replace", reflect.TypeFor[contract.ImageJobRequest](), reflect.TypeFor[contract.JobAccepted]()},
		{"post", "/api/v1/lookup", reflect.TypeFor[contract.LookupRequest](), reflect.TypeFor[contract.JobAccepted]()},
		{"post", "/api/v1/galleries/rename", reflect.TypeFor[contract.GalleryRename](), nil},
		{"post", "/api/v1/ptr/lookup", reflect.TypeFor[contract.PTRLookupRequest](), reflect.TypeFor[contract.PTRLookupResponse]()},
		{"post", "/api/v1/ptr/tags", reflect.TypeFor[contract.PTRTagsRequest](), reflect.TypeFor[contract.PTRTagsResponse]()},
		{"get", "/api/v1/ptr/tags/search", nil, reflect.TypeFor[contract.PTRSearchResponse]()},
		{"post", "/api/v1/ptr/contrib/preview", reflect.TypeFor[contract.ContribPreviewRequest](), reflect.TypeFor[contract.ContribPreviewResponse]()},
		{"post", "/api/v1/ptr/contrib", reflect.TypeFor[contract.ContribRequest](), reflect.TypeFor[contract.ContribResponse]()},
		{"post", "/api/v1/ptr/contrib/pair-preview", reflect.TypeFor[contract.PairPreviewRequest](), reflect.TypeFor[contract.PairPreviewResponse]()},
		{"get", "/health", nil, reflect.TypeFor[contract.Health]()},
		{"get", "/api/v1/queue/{id}", nil, reflect.TypeFor[queueJob]()},
		{"get", "/api/v1/ptr/status", nil, reflect.TypeFor[ptrStatus]()},
		{"get", "/api/v1/rules", nil, reflect.TypeFor[TagRules]()},
		{"post", "/api/v1/rules", reflect.TypeFor[tagRulesRequest](), reflect.TypeFor[tagRulesBatch]()},
	} {
		op, _ := dig(doc, "paths", x.path, x.method).(map[string]any)
		if op == nil {
			t.Errorf("monloader documents no %s %s", x.method, x.path)
			continue
		}
		if x.sent != nil {
			declared := props(doc, dig(op, "requestBody", "content", "application/json", "schema"), "")
			for _, name := range jsonFields(x.sent, "") {
				if !declares(declared, name) {
					t.Errorf("%s %s body: monloader does not declare %q, which this client sends", x.method, x.path, name)
				}
			}
		}
		if x.read != nil {
			var declared map[string]bool
			for code, resp := range dig(op, "responses").(map[string]any) {
				if strings.HasPrefix(code, "2") {
					declared = props(doc, dig(resp, "content", "application/json", "schema"), "")
				}
			}
			for _, name := range jsonFields(x.read, "") {
				if !declares(declared, name) {
					t.Errorf("%s %s answer: monloader does not declare %q, which this client reads", x.method, x.path, name)
				}
			}
		}
	}
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func resolve(doc map[string]any, schema any) any {
	if ref, ok := dig(schema, "$ref").(string); ok {
		return dig(doc, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...)
	}
	return schema
}

func props(doc map[string]any, schema any, prefix string) map[string]bool {
	out := map[string]bool{}
	m, _ := dig(resolve(doc, schema), "properties").(map[string]any)
	for name, sub := range m {
		out[prefix+name] = true
		sub = resolve(doc, sub)
		if items := dig(sub, "items"); items != nil {
			sub = resolve(doc, items)
		}
		if _, ok := dig(sub, "properties").(map[string]any); ok {
			maps.Copy(out, props(doc, sub, prefix+name+"."))
		} else if dig(sub, "type") == "object" {
			out[prefix+name+".*"] = true
		}
	}
	return out
}

func declares(declared map[string]bool, field string) bool {
	if declared[field] {
		return true
	}
	for i := range len(field) {
		if field[i] == '.' && declared[field[:i]+".*"] {
			return true
		}
	}
	return false
}

func jsonFields(typ reflect.Type, prefix string) []string {
	var names []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			names = append(names, jsonFields(f.Type, prefix)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, prefix+name)
		sub, path := f.Type, prefix+name+"."
		for sub.Kind() == reflect.Pointer || sub.Kind() == reflect.Slice || sub.Kind() == reflect.Map {
			if sub.Kind() == reflect.Map {
				path += "*."
			}
			sub = sub.Elem()
		}
		if sub.Kind() == reflect.Struct {
			names = append(names, jsonFields(sub, path)...)
		}
	}
	return names
}
