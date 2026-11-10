package gallery

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBoundary_SkipsANestedGalleryTypedInAnotherCase(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "Private"), 0o755)
	mustWriteFile(t, filepath.Join(root, "Private", "x.png"), []byte("x"), 0o644)
	mustWriteFile(t, filepath.Join(root, "a.png"), []byte("a"), 0o644)
	b := NewBoundary(root, []Fence{{Path: filepath.Join(root, "PRIVATE"), Owner: "inner", Kind: FenceGallery}}, nil)

	if got := walkedFilesIn(t, b); !slices.Equal(got, []string{"a.png"}) {
		t.Errorf("walked %v, want only a.png", got)
	}
}

func TestRemoveOwned_KeepsANestedGalleryTypedInAnotherCase(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "Private"), 0o755)
	inner := filepath.Join(root, "Private", "x.png")
	mustWriteFile(t, inner, []byte("x"), 0o644)
	b := NewBoundary(root, []Fence{{Path: filepath.Join(root, "PRIVATE"), Owner: "inner", Kind: FenceGallery}}, nil)

	if _, err := RemoveOwned(b); err != nil {
		t.Fatalf("RemoveOwned: %v", err)
	}
	if _, err := os.Stat(inner); err != nil {
		t.Errorf("the nested gallery's file went: %v", err)
	}
}
