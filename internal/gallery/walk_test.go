package gallery

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mustSymlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func walkedFiles(t *testing.T, root string) []string {
	t.Helper()
	return walkedFilesIn(t, NewBoundary(root, nil, nil))
}

func walkedFilesIn(t *testing.T, b *Boundary) []string {
	t.Helper()
	return walkedFilesUnder(t, b.Root(), b.Root(), b)
}

func walkedFilesUnder(t *testing.T, start, base string, b *Boundary) []string {
	t.Helper()
	var out []string
	if err := walkTreeUnder(start, b, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", start, err)
	}
	slices.Sort(out)
	return out
}

func TestWalkTree_DescendsALinkedDirectoryUnderTheLinksName(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, filepath.Join(elsewhere, "trip"), 0o755)
	mustWriteFile(t, filepath.Join(elsewhere, "a.png"), []byte("a"), 0o644)
	mustWriteFile(t, filepath.Join(elsewhere, "trip", "b.png"), []byte("b"), 0o644)
	mustSymlink(t, elsewhere, filepath.Join(root, "linked"))

	got := walkedFiles(t, root)
	want := []string{"linked/a.png", "linked/trip/b.png"}
	if !slices.Equal(got, want) {
		t.Errorf("walked %v, want %v", got, want)
	}
}

func TestWalkTree_WalksARootThatIsItselfALink(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	mustMkdirAll(t, real, 0o755)
	mustWriteFile(t, filepath.Join(real, "a.png"), []byte("a"), 0o644)
	root := filepath.Join(t.TempDir(), "root")
	mustSymlink(t, real, root)

	if got := walkedFiles(t, root); !slices.Equal(got, []string{"a.png"}) {
		t.Errorf("walked %v, want [a.png]: a gallery reached through a link still has files", got)
	}
}

func TestWalkTree_ALinkToAnAncestorDoesNotLoop(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.png"), []byte("a"), 0o644)
	mustSymlink(t, root, filepath.Join(root, "loop"))

	if got := walkedFiles(t, root); !slices.Equal(got, []string{"a.png"}) {
		t.Errorf("walked %v, want [a.png]", got)
	}
}

func TestWalkTree_ALinkToTheRootsParentIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.png"), []byte("a"), 0o644)
	mustSymlink(t, filepath.Dir(root), filepath.Join(root, "pictures"))

	if got := walkedFiles(t, root); !slices.Equal(got, []string{"a.png"}) {
		t.Errorf("walked %v, want [a.png]: the gallery would be reported a second time under pictures/", got)
	}
}

func TestWalkTree_ALinkToATreeHoldingTheRootIsNotFollowed(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "Pictures", "booru")
	mustMkdirAll(t, root, 0o755)
	mustWriteFile(t, filepath.Join(root, "a.png"), []byte("a"), 0o644)
	mustWriteFile(t, filepath.Join(home, "taxes.pdf"), []byte("private"), 0o644)
	mustSymlink(t, home, filepath.Join(root, "everything"))

	if got := walkedFiles(t, root); !slices.Equal(got, []string{"a.png"}) {
		t.Errorf("walked %v, want [a.png]: a link out to a tree the gallery sits in pulls the whole tree in", got)
	}
}

func TestWalkTreeUnder_JudgesLinksAgainstTheBoundaryNotTheStart(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "booru")
	sub := filepath.Join(root, "incoming")
	mustMkdirAll(t, sub, 0o755)
	mustWriteFile(t, filepath.Join(sub, "a.png"), []byte("a"), 0o644)
	mustWriteFile(t, filepath.Join(home, "taxes.pdf"), []byte("private"), 0o644)
	mustSymlink(t, home, filepath.Join(sub, "everything"))

	got := walkedFilesUnder(t, sub, sub, NewBoundary(root, nil, nil))
	if !slices.Equal(got, []string{"a.png"}) {
		t.Errorf("walked %v, want [a.png]", got)
	}
}

func TestWalkTreeUnder_AWalkFromALinkSkipsAFenceInItsTarget(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustMkdirAll(t, filepath.Join(outside, "B"), 0o755)
	mustWriteFile(t, filepath.Join(outside, "B", "x.png"), []byte("x"), 0o644)
	mustWriteFile(t, filepath.Join(outside, "y.png"), []byte("y"), 0o644)
	ext := filepath.Join(root, "ext")
	mustSymlink(t, outside, ext)
	b := NewBoundary(root, []Fence{{Path: filepath.Join(outside, "B"), Owner: "b", Kind: FenceGallery}}, nil)

	got := walkedFilesUnder(t, ext, root, b)
	if !slices.Equal(got, []string{"ext/y.png"}) {
		t.Errorf("walked %v, want [ext/y.png]", got)
	}
}

func TestWalkTree_ALinkPointingBackInsideTheRootIsNotWalkedTwice(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "sub"), 0o755)
	mustWriteFile(t, filepath.Join(root, "sub", "a.png"), []byte("a"), 0o644)
	mustSymlink(t, filepath.Join(root, "sub"), filepath.Join(root, "again"))

	if got := walkedFiles(t, root); !slices.Equal(got, []string{"sub/a.png"}) {
		t.Errorf("walked %v, want [sub/a.png]: the walk already reaches sub under its own name", got)
	}
}

func TestWalkTree_TwoLinksToOneDirectoryReportItOnce(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	mustWriteFile(t, filepath.Join(elsewhere, "a.png"), []byte("a"), 0o644)
	mustSymlink(t, elsewhere, filepath.Join(root, "first"))
	mustSymlink(t, elsewhere, filepath.Join(root, "second"))

	if got := walkedFiles(t, root); len(got) != 1 {
		t.Errorf("walked %v, want one entry: the same file under two names ingests twice", got)
	}
}

func TestWalkTree_OverlappingLinksReportEachFileOnce(t *testing.T) {
	for _, tc := range []struct {
		name, parentLink, childLink string
		want                        []string
	}{
		{"parent first", "a_link", "b_link", []string{"a_link/sub/x.png", "a_link/top.png"}},
		{"child first", "b_link", "a_link", []string{"a_link/x.png", "b_link/top.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outer := filepath.Join(t.TempDir(), "outer")
			mustMkdirAll(t, filepath.Join(outer, "sub"), 0o755)
			mustWriteFile(t, filepath.Join(outer, "top.png"), []byte("t"), 0o644)
			mustWriteFile(t, filepath.Join(outer, "sub", "x.png"), []byte("x"), 0o644)
			mustSymlink(t, outer, filepath.Join(root, tc.parentLink))
			mustSymlink(t, filepath.Join(outer, "sub"), filepath.Join(root, tc.childLink))

			if got := walkedFiles(t, root); !slices.Equal(got, tc.want) {
				t.Errorf("walked %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWalkTree_ALinkPairPointingAtEachOtherTerminates(t *testing.T) {
	root := t.TempDir()
	outer := t.TempDir()
	a, b := filepath.Join(outer, "a"), filepath.Join(outer, "b")
	mustMkdirAll(t, a, 0o755)
	mustMkdirAll(t, b, 0o755)
	mustWriteFile(t, filepath.Join(a, "a.png"), []byte("a"), 0o644)
	mustSymlink(t, b, filepath.Join(a, "to-b"))
	mustSymlink(t, a, filepath.Join(b, "to-a"))
	mustSymlink(t, a, filepath.Join(root, "linked"))

	if got := walkedFiles(t, root); !slices.Equal(got, []string{"linked/a.png"}) {
		t.Errorf("walked %v, want [linked/a.png]", got)
	}
}

func TestWalkTree_ADanglingLinkIsSkipped(t *testing.T) {
	root := t.TempDir()
	mustSymlink(t, filepath.Join(t.TempDir(), "gone"), filepath.Join(root, "dangling.png"))

	if got := walkedFiles(t, root); len(got) != 0 {
		t.Errorf("walked %v, want nothing: a link pointing at nothing is neither file nor folder", got)
	}
}

func TestWalkTree_ALinkedFileCarriesItsTargetsSize(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "original.png")
	mustWriteFile(t, target, []byte("some actual contents"), 0o644)
	mustSymlink(t, target, filepath.Join(root, "linked.png"))

	var size int64 = -1
	if err := WalkTree(NewBoundary(root, nil, nil), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		size = info.Size()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := int64(len("some actual contents")); size != want {
		t.Errorf("size = %d, want %d", size, want)
	}
}

func TestWalkTree_SkipAllStopsTheWalkThroughALink(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	for _, n := range []string{"a.png", "b.png", "c.png"} {
		mustWriteFile(t, filepath.Join(elsewhere, n), []byte(n), 0o644)
	}
	mustSymlink(t, elsewhere, filepath.Join(root, "linked"))

	seen := 0
	if err := WalkTree(NewBoundary(root, nil, nil), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		seen++
		return fs.SkipAll
	}); err != nil {
		t.Fatalf("SkipAll should end the walk cleanly, got %v", err)
	}
	if seen != 1 {
		t.Errorf("visited %d files after SkipAll, want 1", seen)
	}
}

func TestWalkTree_SkipDirOnAnUnreadableDirectorySkipsOnlyIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.png"), []byte("a"), 0o644)
	locked := filepath.Join(root, "locked")
	mustMkdirAll(t, locked, 0o755)
	mustWriteFile(t, filepath.Join(locked, "hidden.png"), []byte("h"), 0o644)
	mustMkdirAll(t, filepath.Join(root, "sub"), 0o755)
	mustWriteFile(t, filepath.Join(root, "sub", "b.png"), []byte("b"), 0o644)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	var out []string
	if err := WalkTree(NewBoundary(root, nil, nil), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fs.SkipDir
		}
		if !d.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	slices.Sort(out)
	if want := []string{"a.png", "sub/b.png"}; !slices.Equal(out, want) {
		t.Errorf("walked %v, want %v: the unreadable directory took the rest of the walk with it", out, want)
	}
}

func TestWalkTree_AnUnreadableRootIsReportedToTheCallback(t *testing.T) {
	var got error
	err := WalkTree(NewBoundary(filepath.Join(t.TempDir(), "absent"), nil, nil), func(path string, d fs.DirEntry, err error) error {
		got = err
		return nil
	})
	if err != nil {
		t.Fatalf("WalkTree = %v, want nil: the callback swallowed the error", err)
	}
	if got == nil {
		t.Error("a root that is not there should reach the callback as an error")
	}
}

func TestWalkTree_SkipsAFencedFolderWhole(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "wall", "deep"), 0o755)
	mustWriteFile(t, filepath.Join(root, "a.png"), []byte("a"), 0o644)
	mustWriteFile(t, filepath.Join(root, "wall", "b.png"), []byte("b"), 0o644)
	mustWriteFile(t, filepath.Join(root, "wall", "deep", "c.png"), []byte("c"), 0o644)
	b := NewBoundary(root, []Fence{{Path: filepath.Join(root, "wall"), Owner: "wall", Kind: FenceGallery}}, nil)

	if got, want := walkedFilesIn(t, b), []string{"a.png"}; !slices.Equal(got, want) {
		t.Errorf("walked %v, want %v", got, want)
	}
}

func TestWalkTree_SkipsALinkIntoAnotherGallery(t *testing.T) {
	root := t.TempDir()
	stock := t.TempDir()
	mustWriteFile(t, filepath.Join(stock, "s.png"), []byte("s"), 0o644)
	mustSymlink(t, stock, filepath.Join(root, "stock"))
	shared := t.TempDir()
	mustWriteFile(t, filepath.Join(shared, "x.png"), []byte("x"), 0o644)
	mustSymlink(t, shared, filepath.Join(root, "shared"))
	b := NewBoundary(root, []Fence{{Path: stock, Owner: "stock", Kind: FenceGallery}}, nil)

	if got, want := walkedFilesIn(t, b), []string{"shared/x.png"}; !slices.Equal(got, want) {
		t.Errorf("walked %v, want %v", got, want)
	}
}
