package gallery

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBoundary_ExcludesANestedGallerysFolder(t *testing.T) {
	root := t.TempDir()
	b := NewBoundary(root, []Fence{{Path: filepath.Join(root, ".H"), Owner: "H", Kind: FenceGallery}}, nil)
	for path, want := range map[string]bool{
		filepath.Join(root, ".H"):             true,
		filepath.Join(root, ".H", "a.png"):    true,
		filepath.Join(root, ".Hx", "a.png"):   false,
		filepath.Join(root, ".nsfw", "a.png"): false,
		filepath.Join(root, "a.png"):          false,
	} {
		if got := b.Excludes(path); got != want {
			t.Errorf("Excludes(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestBoundary_AFenceHoldingTheRootDoesNotApply(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, ".H")
	b := NewBoundary(root, []Fence{
		{Path: outer, Owner: "main", Kind: FenceGallery},
		{Path: root, Owner: "twin", Kind: FenceGallery},
		{Path: outer, Kind: FenceData},
	}, nil)
	if b.Excludes(filepath.Join(root, "a.png")) {
		t.Error("a fence around the root cut into it")
	}
}

func TestBoundary_FencesTheFolderALinkedGalleryResolvesTo(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	mustMkdirAll(t, inner, 0o755)
	link := filepath.Join(t.TempDir(), "h")
	mustSymlink(t, inner, link)
	b := NewBoundary(root, []Fence{{Path: link, Owner: "H", Kind: FenceGallery}}, nil)
	if !b.Excludes(filepath.Join(inner, "a.png")) {
		t.Error("the folder a gallery reaches through a link is not fenced")
	}
}

func TestEncloses(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct {
		outer, inner string
		want         bool
	}{
		{root, filepath.Join(root, "a"), true},
		{root, root, false},
		{filepath.Join(root, "a"), root, false},
		{filepath.Join(root, "a"), filepath.Join(root, "ab"), false},
	} {
		if got := Encloses(c.outer, c.inner); got != c.want {
			t.Errorf("Encloses(%s, %s) = %v, want %v", c.outer, c.inner, got, c.want)
		}
	}
}

func TestRemoveOwned_KeepsAFencedFolderAndTheWayToIt(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "sub", ".H")
	mustMkdirAll(t, filepath.Join(inner, "deep"), 0o755)
	mustMkdirAll(t, filepath.Join(root, "other"), 0o755)
	for _, p := range []string{"a.png", "sub/b.png", "other/c.png", "sub/.H/in.png", "sub/.H/deep/d.png"} {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(p)), []byte(p), 0o644)
	}
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "o.png"), []byte("o"), 0o644)
	mustSymlink(t, outside, filepath.Join(root, "linked"))
	b := NewBoundary(root, []Fence{{Path: inner, Owner: "H", Kind: FenceGallery}}, nil)

	kept, err := RemoveOwned(b)
	if err != nil {
		t.Fatalf("RemoveOwned: %v", err)
	}
	if want := []string{"sub/.H"}; !slices.Equal(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
	for _, p := range []string{"a.png", "sub/b.png", "other", "linked"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Errorf("%s survived the wipe", p)
		}
	}
	for _, p := range []string{"sub/.H/in.png", "sub/.H/deep/d.png"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Errorf("fenced %s went: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "o.png")); err != nil {
		t.Errorf("the link was followed: %v", err)
	}
}

func TestRemoveOwnedKeepsAFolderItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	root := t.TempDir()
	lost := filepath.Join(root, "lost+found")
	mustMkdirAll(t, lost, 0o755)
	for _, p := range []string{"a.png", "lost+found/inside.png", "z.png"} {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(p)), []byte(p), 0o644)
	}
	if err := os.Chmod(lost, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lost, 0o755) })

	kept, err := RemoveOwned(NewBoundary(root, nil, nil))

	if err != nil {
		t.Fatalf("RemoveOwned: %v", err)
	}
	if want := []string{"lost+found"}; !slices.Equal(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
	for _, p := range []string{"a.png", "z.png"} {
		if _, err := os.Lstat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s survived the wipe", p)
		}
	}
	if err := os.Chmod(lost, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lost, "inside.png")); err != nil {
		t.Errorf("the unreadable folder lost its file: %v", err)
	}
}

func TestBoundary_CheckSaysWhyAPathIsNotThisGallerys(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, ".H")
	mustMkdirAll(t, inner, 0o755)
	stock := t.TempDir()
	mustSymlink(t, stock, filepath.Join(root, "stock"))
	mustSymlink(t, inner, filepath.Join(root, "l"))
	shared := t.TempDir()
	mustSymlink(t, shared, filepath.Join(root, "shared"))
	b := NewBoundary(root, []Fence{
		{Path: inner, Owner: "H", Kind: FenceGallery},
		{Path: stock, Owner: "stock", Kind: FenceGallery},
		{Path: filepath.Join(root, ".monbooru"), Kind: FenceData},
	}, nil)

	for _, c := range []struct{ path, want string }{
		{filepath.Join(inner, "a.png"), `".H" belongs to gallery H`},
		{filepath.Join(root, "stock", "new", "a.png"), `"stock" leads into gallery stock`},
		{filepath.Join(root, "l", "a.png"), `"l" leads into gallery H`},
		{filepath.Join(root, ".monbooru", "thumbs"), `".monbooru" is monbooru's data folder`},
	} {
		err := b.Check(c.path)
		if err == nil || err.Error() != c.want {
			t.Errorf("Check(%s) = %v, want %q", c.path, err, c.want)
		}
		if !errors.As(err, new(Exclusion)) {
			t.Errorf("Check(%s) answered %T, not an Exclusion", c.path, err)
		}
	}
	for _, p := range []string{filepath.Join(root, "shared", "a.png"), filepath.Join(root, "new", "a.png")} {
		if err := b.Check(p); err != nil {
			t.Errorf("Check(%s) = %v, want nil", p, err)
		}
	}
}

func TestBoundary_IgnoreList(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".H")
	b := NewBoundary(root, nil, []string{"@eaDir", ".Trash-*", "._*", "photos/drafts", "/wip", "2024/*/raw", "$RECYCLE.BIN", ".*x"})
	for rel, want := range map[string]bool{
		"@eaDir":              true,
		"2024/trip/@EADIR/a":  true,
		".Trash-1000/a.png":   true,
		"._IMG_0001.jpg":      true,
		"a/._x.png":           true,
		"photos/drafts/a.png": true,
		"old/photos/drafts/a": false,
		"wip/a.png":           true,
		"2024/wip/a.png":      false,
		"2024/05/raw/a.png":   true,
		"2024/raw/a.png":      false,
		"$Recycle.Bin/a.png":  true,
		"Trash/a.png":         false,
		"a.png":               false,
		".nsfw/a.png":         false,
		"..drafts/@eaDir/a":   true,
		"..drafts/a.png":      false,
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if got := b.Excludes(p); got != want {
			t.Errorf("Excludes(%s) = %v, want %v", rel, got, want)
		}
	}
	if b.Excludes(root) {
		t.Error("the root was matched by its own name")
	}
	err := b.Check(filepath.Join(root, ".Trash-1000", "a.png"))
	if err == nil || err.Error() != `".Trash-1000" is on the ignore list (.Trash-*)` {
		t.Errorf("Check = %v", err)
	}
	if err := b.Check(filepath.Join(root, "x", "@eaDir")); err == nil || err.Error() != `"x/@eaDir" is on the ignore list` {
		t.Errorf("Check = %v", err)
	}
}

func TestBoundary_IgnoredEntriesAreSkippedAndKept(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"a.png", "._a.png", "trip/@eaDir/t.jpg", "trip/b.png", "photos/drafts/d.png", "photos/e.png"} {
		mustMkdirAll(t, filepath.Join(root, filepath.Dir(filepath.FromSlash(p))), 0o755)
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(p)), []byte(p), 0o644)
	}
	b := NewBoundary(root, nil, []string{"@eaDir", "._*", "photos/drafts"})

	if got, want := walkedFilesIn(t, b), []string{"a.png", "photos/e.png", "trip/b.png"}; !slices.Equal(got, want) {
		t.Errorf("walked %v, want %v", got, want)
	}
	kept, err := RemoveOwned(b)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(kept)
	if want := []string{"._a.png", "photos/drafts", "trip/@eaDir"}; !slices.Equal(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
}

func TestSameFolder(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	mustMkdirAll(t, real, 0o755)
	link := filepath.Join(root, "link")
	mustSymlink(t, real, link)
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{real, real + "/", true},
		{real, link, true},
		{real, filepath.Join(real, "sub"), false},
		{real, root, false},
	} {
		if got := SameFolder(c.a, c.b); got != c.want {
			t.Errorf("SameFolder(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestBoundary_FencedListsTheOutermostFencesUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	b := NewBoundary(root, []Fence{
		{Path: filepath.Join(root, "b", ".H"), Owner: "H", Kind: FenceGallery},
		{Path: filepath.Join(root, "b", ".H", "deeper"), Owner: "D", Kind: FenceGallery},
		{Path: filepath.Join(root, "a", ".monbooru"), Kind: FenceData},
		{Path: filepath.Dir(root), Owner: "outer", Kind: FenceGallery},
		{Path: t.TempDir(), Owner: "stock", Kind: FenceGallery},
	}, nil)
	var got []string
	for _, e := range b.Fenced() {
		got = append(got, e.Rel+" ("+e.OwnerName()+")")
	}
	if want := []string{"a/.monbooru (monbooru's data folder)", "b/.H (gallery H)"}; !slices.Equal(got, want) {
		t.Errorf("Fenced = %q, want %q", got, want)
	}
}
