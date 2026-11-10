package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var galleryDoorHelpers = []string{"applyImageEdit", "sourceMembershipAction", "setSourceText"}

var galleryCallsOutsideADoor = []string{
	"gallery.AnnotationsForImage",
	"gallery.CanonicalGone",
	"gallery.ChildIDsByParentURL",
	"gallery.CollectionCBZMembers",
	"gallery.CollectionCeilingHidden",
	"gallery.CollectionHiddenOrderedIDs",
	"gallery.CollectionMemberIDs",
	"gallery.CollectionMembers",
	"gallery.CollectionNavs",
	"gallery.CollectionSamples",
	"gallery.CollectionsForImage",
	"gallery.ContentDispositionFor",
	"gallery.CountCollections",
	"gallery.DetectFileType",
	"gallery.EnsureMangaPage",
	"gallery.EnsureMangaPageView",
	"gallery.EnsureViewRendition",
	"gallery.ExistingImageIDs",
	"gallery.ExtractGenerationMeta",
	"gallery.FileTypes",
	"gallery.FindFolder",
	"gallery.FolderPath",
	"gallery.Follows",
	"gallery.HashFile",
	"gallery.ImageIDBySHA",
	"gallery.ImageIDBySourceURL",
	"gallery.ImageIDsBySourceURL",
	"gallery.ImageSHAAndType",
	"gallery.IngestNaming",
	"gallery.IsFFmpegStill",
	"gallery.IsVideoType",
	"gallery.ListCollections",
	"gallery.MagicFileType",
	"gallery.Md5File",
	"gallery.MIMEForFileType",
	"gallery.NamedInside",
	"gallery.NaturalLess",
	"gallery.NeedsViewRendition",
	"gallery.NewBoundary",
	"gallery.NewZipNamer",
	"gallery.NormalizeImage",
	"gallery.ParseBatchRenameTemplate",
	"gallery.ParseNameTemplate",
	"gallery.PlannedPath",
	"gallery.PresentFileTypes",
	"gallery.PreviewRefusal",
	"gallery.ProbeDurationSeconds",
	"gallery.ProbeVideoDimensions",
	"gallery.PruneOrphanThumbnails",
	"gallery.ReadComicInfo",
	"gallery.Rebuild",
	"gallery.ReceivedNaming",
	"gallery.RemoveEmptyDirs",
	"gallery.RenderStillFrame",
	"gallery.ResolveCategoryTag",
	"gallery.ResolveTagRefs",
	"gallery.RewrittenBy",
	"gallery.SanitizeReceivedName",
	"gallery.ScanEmptyDirs",
	"gallery.SourcesForImage",
	"gallery.SourceSimilarityMatched",
	"gallery.SuppressesIngest",
	"gallery.ThumbnailPath",
	"gallery.TruncateFilename",
	"gallery.ValidExternalURL",
	"gallery.WriteCollectionCBZ",
	"lookup.Backends",
	"lookup.Due",
	"lookup.DueCount",
	"lookup.DueIDs",
	"lookup.ForImage",
	"lookup.StandingOf",
	"lookup.Waiting",

	"gallery.ApplyMetaTags",
	"gallery.BackfillMetaTags",
	"gallery.RemoveMetaTags",
	"gallery.ResolveTagNames",

	"gallery.SetNote",
	"gallery.SetReadPage",
}

func TestGalleryCallsOutsideADoor(t *testing.T) {
	allowed := map[string]bool{}
	for _, name := range galleryCallsOutsideADoor {
		allowed[name] = true
	}
	seen := map[string]bool{}
	for _, dir := range []string{".", "../api", "../monloader"} {
		for pos, name := range packageCallsOutsideADoor(t, dir) {
			seen[name] = true
			if !allowed[name] {
				t.Errorf("%s: %s is called outside library.Gallery.Edit: put a write behind a wrapper or Edit; list it here only if it reads", pos, name)
			}
		}
	}
	for _, name := range galleryCallsOutsideADoor {
		if !seen[name] {
			t.Errorf("%s is listed but no longer called outside a door: drop it from the list", name)
		}
	}
}

func packageCallsOutsideADoor(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkgs := map[string]bool{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if base := filepath.Base(p); (base == "gallery" || base == "lookup") && strings.HasPrefix(p, "github.com/monbooru/monbooru/internal/") {
				pkgs[base] = true
			}
		}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || !pkgs[pkg.Name] || insideADoor(stack) {
				return true
			}
			out[fset.Position(call.Pos()).String()] = pkg.Name + "." + sel.Sel.Name
			return true
		})
	}
	return out
}

func insideADoor(stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 1; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		call, ok := stack[i-1].(*ast.CallExpr)
		if !ok || !slices.Contains(call.Args, ast.Expr(lit)) {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Edit" || slices.Contains(galleryDoorHelpers, sel.Sel.Name)) {
			return true
		}
	}
	return false
}
