package gallery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoredInside_AFileUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "sub"), 0o755)
	path := filepath.Join(root, "sub", "a.png")
	mustWriteFile(t, path, []byte("a"), 0o644)

	if !storedInside(root, path) {
		t.Error("a plain file in the gallery is stored inside it")
	}
}

func TestStoredInside_AFileBehindALinkedFolder(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	path := filepath.Join(elsewhere, "a.png")
	mustWriteFile(t, path, []byte("a"), 0o644)
	mustSymlink(t, elsewhere, filepath.Join(root, "linked"))

	if storedInside(root, filepath.Join(root, "linked", "a.png")) {
		t.Error("a file reached through a link lives outside the gallery folder")
	}
}

func TestStoredInside_ARootThatIsItselfALink(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	mustMkdirAll(t, real, 0o755)
	path := filepath.Join(real, "a.png")
	mustWriteFile(t, path, []byte("a"), 0o644)
	root := filepath.Join(t.TempDir(), "root")
	mustSymlink(t, real, root)

	if !storedInside(root, filepath.Join(root, "a.png")) {
		t.Error("the gallery's own files are inside it whatever name the root is reached by")
	}
}

func TestStoredInside_ALinkPointingBackInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "sub"), 0o755)
	mustWriteFile(t, filepath.Join(root, "sub", "a.png"), []byte("a"), 0o644)
	mustSymlink(t, filepath.Join(root, "sub"), filepath.Join(root, "again"))

	if !storedInside(root, filepath.Join(root, "again", "a.png")) {
		t.Error("the bytes are in the gallery, whichever of its two names reached them")
	}
}

func TestStoredInside_APathThatIsNotThere(t *testing.T) {
	root := t.TempDir()
	if storedInside(root, filepath.Join(root, "gone.png")) {
		t.Error("a question that cannot be answered counts as outside")
	}
}

func TestClaimOwnership_LeavesALinkedFileAlone(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	target := filepath.Join(elsewhere, "a.png")
	mustWriteFile(t, target, []byte("a"), 0o644)
	mustSymlink(t, elsewhere, filepath.Join(root, "linked"))

	before := mustStat(t, target)
	claimOwnership(root, filepath.Join(root, "linked", "a.png"))
	after := mustStat(t, target)
	if before.Mode() != after.Mode() || before.Size() != after.Size() {
		t.Errorf("the linked file changed: %v/%d -> %v/%d", before.Mode(), before.Size(), after.Mode(), after.Size())
	}
	if _, err := os.Lstat(filepath.Join(root, "linked")); err != nil {
		t.Errorf("the link itself went: %v", err)
	}
}
