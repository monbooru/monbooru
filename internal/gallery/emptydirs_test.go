package gallery

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		mustMkdirAll(t, filepath.Join(root, filepath.FromSlash(d)), 0o755)
	}
}

func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	mustMkdirAll(t, filepath.Dir(path), 0o755)
	mustWriteFile(t, path, []byte("x"), 0o644)
}

func scanPaths(t *testing.T, root string) []string {
	t.Helper()
	dirs, err := ScanEmptyDirs(NewBoundary(root, nil, nil))
	if err != nil {
		t.Fatalf("ScanEmptyDirs: %v", err)
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Path
	}
	return out
}

func TestScanEmptyDirs_ReportsAWholeEmptyChainParentFirst(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "deep/a/b", "archive/2025", "inbox")
	writeFile(t, root, "archive/2026/kept.png")

	got := scanPaths(t, root)
	want := []string{"archive/2025", "deep", "deep/a", "deep/a/b", "inbox"}
	if len(got) != len(want) {
		t.Fatalf("scan = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scan = %v, want %v", got, want)
		}
	}
}

func TestScanEmptyDirs_AnyFileKeepsTheFolder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "notes/readme.txt")
	mkdirs(t, root, "gone")

	if got := scanPaths(t, root); len(got) != 1 || got[0] != "gone" {
		t.Errorf("scan = %v, want only [gone]; an unsupported file still holds its folder", got)
	}
}

func TestScanEmptyDirs_DotDirsAreLeftAloneAndKeepTheirParent(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "synced/.stfolder", ".git")

	if got := scanPaths(t, root); len(got) != 0 {
		t.Errorf("scan = %v, want nothing: a dot-directory is neither removed nor descended into", got)
	}
}

func TestScanEmptyDirs_ASymlinkedDirCountsAsContent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	mustMkdirAll(t, target, 0o755)
	mkdirs(t, root, "holder")
	if err := os.Symlink(target, filepath.Join(root, "holder", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := scanPaths(t, root); len(got) != 0 {
		t.Errorf("scan = %v, want nothing: the link is an entry, so holder is not empty", got)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the link target went away: %v", err)
	}
}

func TestScanEmptyDirs_RefusesAnUnreadableRoot(t *testing.T) {
	if _, err := ScanEmptyDirs(NewBoundary(filepath.Join(t.TempDir(), "absent"), nil, nil)); err == nil {
		t.Error("scanning a root that is not there should surface the error")
	}
}

func TestRemoveEmptyDirs_CollapsesAChainInOnePass(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "deep/a/b")

	if n := RemoveEmptyDirs(NewBoundary(root, nil, nil), []string{"deep", "deep/a", "deep/a/b"}); n != 3 {
		t.Errorf("removed %d, want 3", n)
	}
	if _, err := os.Stat(filepath.Join(root, "deep")); !os.IsNotExist(err) {
		t.Error("the chain is still standing")
	}
}

func TestRemoveEmptyDirs_KeepsWhatIsNoLongerEmpty(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "empty")
	writeFile(t, root, "refilled/landed.png")

	if n := RemoveEmptyDirs(NewBoundary(root, nil, nil), []string{"empty", "refilled"}); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(root, "refilled", "landed.png")); err != nil {
		t.Errorf("a folder that gained a file was emptied anyway: %v", err)
	}
}

func TestRemoveEmptyDirs_DoesNotCountAFolderThatWasAlreadyGone(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "here")

	if n := RemoveEmptyDirs(NewBoundary(root, nil, nil), []string{"here", "vanished"}); n != 1 {
		t.Errorf("removed %d, want only the folder that was there", n)
	}
}

func TestRemoveEmptyDirs_RefusesTheRootAndAnythingOutsideIt(t *testing.T) {
	parent := t.TempDir()
	root, outside := filepath.Join(parent, "gallery"), filepath.Join(parent, "outside")
	mkdirs(t, parent, "gallery", "outside")

	if n := RemoveEmptyDirs(NewBoundary(root, nil, nil), []string{"", ".", "../outside", outside, "/tmp"}); n != 0 {
		t.Errorf("removed %d, want 0", n)
	}
	for _, p := range []string{root, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%q went away: %v", p, err)
		}
	}
}

func TestRemoveEmptyDirs_RefusesAFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keeper.png")

	if n := RemoveEmptyDirs(NewBoundary(root, nil, nil), []string{"keeper.png"}); n != 0 {
		t.Errorf("removed %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(root, "keeper.png")); err != nil {
		t.Errorf("a posted file path unlinked the file: %v", err)
	}
}

func TestScanEmptyDirs_LeavesAFencedFolderAlone(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "wall/deep", "empty")
	b := NewBoundary(root, []Fence{{Path: filepath.Join(root, "wall"), Owner: "wall", Kind: FenceGallery}}, nil)

	dirs, err := ScanEmptyDirs(b)
	if err != nil {
		t.Fatalf("ScanEmptyDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0].Path != "empty" {
		t.Errorf("listed %v, want only empty", dirs)
	}
	if n := RemoveEmptyDirs(b, []string{"wall/deep", "wall"}); n != 0 {
		t.Errorf("removed %d fenced folder(s), want 0", n)
	}
	if _, err := os.Stat(filepath.Join(root, "wall", "deep")); err != nil {
		t.Errorf("fenced folder went: %v", err)
	}
}
