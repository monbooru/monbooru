package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var sharedWordAliases = []string{
	"config.ScopeRead",
	"config.ScopeWrite",
	"monloader.App",
	"monloader.StateAlreadyExists",
	"monloader.StateCanceled",
	"monloader.StateHashNotFound",
	"peer.ConnDown",
	"peer.ConnOK",
	"peer.ConnPaused",
	"peer.ConnRejected",
}

func TestSharedWordsHaveOneSpelling(t *testing.T) {
	allowed := map[string]bool{}
	for _, name := range sharedWordAliases {
		allowed[name] = true
	}
	seen := map[string]bool{}
	for _, root := range []string{"..", "../../cmd"} {
		for pos, name := range sharedWordRespellings(t, root) {
			seen[name] = true
			if !allowed[name] {
				t.Errorf("%s: %s respells a shared word: read it from its shared package instead", pos, name)
			}
		}
	}
	for _, name := range sharedWordAliases {
		if !seen[name] {
			t.Errorf("%s is listed but no longer respells a shared word: drop it from the list", name)
		}
	}
}

func sharedWordRespellings(t *testing.T, root string) map[string]string {
	t.Helper()
	const sharedPrefix = "github.com/monbooru/monbooru/internal/shared/"
	fset := token.NewFileSet()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(path) == "shared" && filepath.Base(filepath.Dir(path)) == "internal" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		shared := map[string]bool{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, sharedPrefix) {
				continue
			}
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			shared[name] = true
		}
		respells := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && shared[pkg.Name]
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for i, v := range s.Values {
						if i < len(s.Names) && s.Names[i].IsExported() && respells(v) {
							out[fset.Position(s.Names[i].Pos()).String()] = f.Name.Name + "." + s.Names[i].Name
						}
					}
				case *ast.TypeSpec:
					if s.Assign.IsValid() && s.Name.IsExported() && respells(s.Type) {
						out[fset.Position(s.Name.Pos()).String()] = f.Name.Name + "." + s.Name.Name
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
