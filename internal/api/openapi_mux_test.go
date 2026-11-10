package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

var metaEndpoints = []string{"GET /docs", "GET /openapi.json", "OPTIONS /"}

func TestOpenAPIDeclaresEveryMountedRoute(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	mounted := regexp.MustCompile(`mux\.HandleFunc\("((?:GET|POST|PATCH|PUT|DELETE|OPTIONS|HEAD) /api/v1[^"]*)"`).
		FindAllStringSubmatch(string(src), -1)
	if len(mounted) < 30 {
		t.Fatalf("found %d registrations; the pattern stopped matching Mount", len(mounted))
	}

	var doc map[string]any
	b, err := json.Marshal(buildSpec("v0.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("the document declares no paths")
	}

	declared := map[string]bool{}
	for path, ops := range paths {
		for method := range ops.(map[string]any) {
			declared[strings.ToUpper(method)+" "+path] = true
		}
	}

	for _, m := range mounted {
		route := strings.TrimPrefix(m[1], "")
		verb, path, _ := strings.Cut(route, " ")
		path = strings.TrimPrefix(path, "/api/v1")
		if path == "" {
			path = "/"
		}
		key := verb + " " + path
		if slices.Contains(metaEndpoints, key) || declared[key] {
			continue
		}
		t.Errorf("%s is registered by Mount and absent from the OpenAPI document", route)
	}
}

func TestOpenAPIDeclaresTheRoutesTheWebRouterServes(t *testing.T) {
	src, err := os.ReadFile("../web/router.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`rt\.\w+\("((?:GET|POST|PATCH|PUT|DELETE) (?:/health|/api/v1/[^"]*))"`).
		FindAllStringSubmatch(string(src), -1)
	if len(routes) < 4 {
		t.Fatalf("found %d routes; the pattern stopped matching the router", len(routes))
	}
	doc := openAPIDoc(t)
	for _, m := range routes {
		verb, path, _ := strings.Cut(m[1], " ")
		if p, ok := strings.CutPrefix(path, "/api/v1"); ok {
			path = p
		} else if servers, _ := dig(doc, "paths", path)["servers"].([]any); len(servers) == 0 {
			t.Errorf("%s is outside /api/v1 and its path names no server", m[1])
		}
		if dig(doc, "paths", path, strings.ToLower(verb)) == nil {
			t.Errorf("%s is served by the web router and absent from the OpenAPI document", m[1])
		}
	}
}

var cannotReach = map[string][]int{
	"POST /tags":                                 {404},
	"GET /tags/{id}":                             {409},
	"DELETE /tags/{id}":                          {409},
	"GET /tags/{id}/implications":                {409},
	"DELETE /tags/{id}/implications/{impliedID}": {409},
	"POST /categories":                           {404},
	"DELETE /relations":                          {409},
}

func TestOpenAPIDeclaresEveryEmittedStatus(t *testing.T) {
	fset := token.NewFileSet()
	funcs := map[string][]*ast.FuncDecl{}
	var mount *ast.FuncDecl
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				funcs[fd.Name.Name] = append(funcs[fd.Name.Name], fd)
				if fd.Name.Name == "Mount" {
					mount = fd
				}
			}
		}
	}
	if mount == nil {
		t.Fatal("no Mount in the package")
	}

	codes := map[string]int{}
	for c := 100; c < 600; c++ {
		if text := http.StatusText(c); text != "" {
			codes["Status"+strings.NewReplacer(" ", "", "-", "", "'", "").Replace(text)] = c
		}
	}
	statusOf := func(e ast.Expr) (int, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "Status") {
			return 0, false
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "http" {
			return 0, false
		}
		c, ok := codes[sel.Sel.Name]
		if !ok {
			t.Fatalf("unknown status constant http.%s", sel.Sel.Name)
		}
		return c, true
	}
	writerParam := func(ft *ast.FuncType) string {
		for _, p := range ft.Params.List {
			if sel, ok := p.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ResponseWriter" && len(p.Names) > 0 {
				return p.Names[0].Name
			}
		}
		return ""
	}

	friendly := map[int]bool{}
	rel, err := parser.ParseFile(fset, "../relations/service.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rel.Decls {
		if fd, ok := d.(*ast.FuncDecl); !ok || fd.Name.Name != "FriendlyErrorFor" {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Status" {
					if v, ok := kv.Value.(*ast.BasicLit); ok && v.Kind == token.INT {
						c, _ := strconv.Atoi(v.Value)
						friendly[c] = true
					}
				}
			}
			return true
		})
	}
	if len(friendly) == 0 {
		t.Fatal("found no status in relations.FriendlyErrorFor")
	}
	calleeName := func(fun ast.Expr) string {
		switch f := fun.(type) {
		case *ast.Ident:
			return f.Name
		case *ast.SelectorExpr:
			return f.Sel.Name
		}
		return ""
	}

	memo := map[string]map[int]bool{}
	var emitted func(name string) map[int]bool
	emitted = func(name string) map[int]bool {
		if got, ok := memo[name]; ok {
			return got
		}
		out := map[int]bool{}
		memo[name] = out

		var scopes []*ast.FuncLit
		for _, fd := range funcs[name] {
			if fd.Body == nil {
				continue
			}
			scopes = append(scopes, &ast.FuncLit{Type: fd.Type, Body: fd.Body})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok {
					scopes = append(scopes, lit)
				}
				return true
			})
		}
		for _, scope := range scopes {
			w := writerParam(scope.Type)
			if w == "" {
				continue
			}
			ast.Inspect(scope.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if calleeName(call.Fun) == "FriendlyErrorFor" {
					for c := range friendly {
						out[c] = true
					}
				}
				uses := false
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == w {
						uses = true
					}
				}
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == w {
						uses = true
					}
				}
				if !uses {
					return true
				}
				for _, a := range call.Args {
					ast.Inspect(a, func(n ast.Node) bool {
						if _, ok := n.(*ast.FuncLit); ok {
							return false
						}
						if e, ok := n.(ast.Expr); ok {
							if c, ok := statusOf(e); ok {
								out[c] = true
							}
						}
						return true
					})
				}
				if callee := calleeName(call.Fun); callee != name && len(funcs[callee]) > 0 {
					for c := range emitted(callee) {
						out[c] = true
					}
				}
				return true
			})
		}
		return out
	}

	var doc map[string]any
	b, err := json.Marshal(buildSpec("v0.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	paths, _ := doc["paths"].(map[string]any)

	checked := 0
	ast.Inspect(mount.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != "HandleFunc" || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return false
		}
		route, _ := strconv.Unquote(lit.Value)
		verb, path, _ := strings.Cut(route, " ")
		path = cmp.Or(strings.TrimPrefix(path, "/api/v1"), "/")
		if slices.Contains(metaEndpoints, verb+" "+path) {
			return false
		}
		want := map[int]bool{}
		for e := call.Args[1]; e != nil; {
			inner, ok := e.(*ast.CallExpr)
			var name string
			if ok {
				name = calleeName(inner.Fun)
				e = nil
				if len(inner.Args) == 1 {
					e = inner.Args[0]
				}
			} else if sel, ok := e.(*ast.SelectorExpr); ok {
				name, e = sel.Sel.Name, nil
			} else {
				break
			}
			if name == "auth" {
				continue
			}
			for c := range emitted(name) {
				want[c] = true
			}
		}
		for _, c := range cannotReach[verb+" "+path] {
			delete(want, c)
		}
		if len(want) == 0 {
			return false
		}
		checked++
		op, _ := paths[path].(map[string]any)[strings.ToLower(verb)].(map[string]any)
		responses, _ := op["responses"].(map[string]any)
		var missing []int
		for c := range want {
			if _, ok := responses[strconv.Itoa(c)]; !ok && c != http.StatusInternalServerError {
				missing = append(missing, c)
			}
		}
		slices.Sort(missing)
		if len(missing) > 0 {
			t.Errorf("%s %s answers %v, which the document does not declare", verb, path, missing)
		}
		return false
	})
	if checked < 30 {
		t.Fatalf("checked %d routes; the walk stopped matching Mount", checked)
	}
}

func TestOpenAPIEnrichBodyDeclaresEveryDecodedField(t *testing.T) {
	props := dig(openAPIDoc(t), "paths", "/images/{id}/enrich", "post", "requestBody", "content", "application/json", "schema", "properties")
	if props == nil {
		t.Fatal("the enrich body declares no properties")
	}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for f := range typ.Fields() {
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if _, ok := props[name]; !ok {
				t.Errorf("enrich decodes %q, which its body schema does not declare", name)
			}
		}
	}
	walk(reflect.TypeFor[contract.EnrichRequest]())
}

func TestOpenAPIRootInfoDeclaresEveryField(t *testing.T) {
	w := newTestEnv(t).do("GET", "/api/v1/")
	mustCode(t, w, http.StatusOK)
	wantDeclared(t, w, dig(openAPIDoc(t), "components", "schemas", "APIInfo", "properties"))
}

func TestOpenAPIDeleteImageDeclaresBody(t *testing.T) {
	env := newTestEnv(t)
	sub := filepath.Join(env.galleryDir, "sub")
	mustMkdirAll(t, sub, 0o755)
	path := filepath.Join(sub, "a.png")
	f := mustCreate(t, path)
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	record, _ := mustIngest(t, env.database, env.galleryDir, env.thumbDir, path, "")

	w := env.do("DELETE", fmt.Sprintf("/api/v1/images/%d?delete_empty_folder=true", record.ID))
	mustCode(t, w, http.StatusOK)
	doc := openAPIDoc(t)
	ref, _ := dig(doc, "paths", "/images/{id}", "delete", "responses", "200", "content", "application/json", "schema")["$ref"].(string)
	wantDeclared(t, w, dig(doc, "components", "schemas", strings.TrimPrefix(ref, "#/components/schemas/"), "properties"))
}

func wantDeclared(t *testing.T, w *httptest.ResponseRecorder, props map[string]any) {
	t.Helper()
	var body map[string]any
	mustDecode(t, w, &body)
	if len(body) == 0 {
		t.Fatal("empty body")
	}
	for k := range body {
		if _, ok := props[k]; !ok {
			t.Errorf("the reply carries %q, which its schema does not declare", k)
		}
	}
}

func openAPIDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	b, err := json.Marshal(buildSpec("v0.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m, _ = m[k].(map[string]any)
	}
	return m
}
