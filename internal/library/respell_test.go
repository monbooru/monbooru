package library

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
)

func TestOpen_RespellsRowsAGalleryWroteWithARelativeRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for rel, shade := range map[string]uint8{"gallery/a.png": 10, "gallery/sub/b.png": 200} {
		if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, rel, pngBytes(t, shade), 0o644)
	}
	g := config.Gallery{Name: "rel", GalleryPath: "gallery", DBPath: filepath.Join(dir, "data", "monbooru.db"), ThumbnailsPath: filepath.Join(dir, "data", "thumbnails")}
	sync := func(cx *Gallery) gallery.SyncResult {
		t.Helper()
		r, err := cx.Sync(context.Background(), 0, gallery.Naming{}, func(int, int, string) {}, func() {})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	relative, err := Open(g)
	if err != nil {
		t.Fatal(err)
	}
	if r := sync(relative); r.Added != 2 {
		t.Fatalf("test premise broken: the relative-root sync gave %+v", r)
	}

	if _, err := relative.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) SELECT image_id, ?, 0 FROM image_paths WHERE path = ?`,
		filepath.Join(dir, "gallery", "a.png"), filepath.Join("gallery", "a.png")); err != nil {
		t.Fatal(err)
	}
	relative.Close()

	g.GalleryPath = filepath.Join(dir, "gallery")
	cx, err := Open(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cx.Close)

	for _, want := range []string{filepath.Join(dir, "gallery", "a.png"), filepath.Join(dir, "gallery", "sub", "b.png")} {
		if n := countRows(t, cx.DB, "images i JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 1 WHERE i.canonical_path = ? AND ip.path = ?", want, want); n != 1 {
			t.Errorf("%s: no image and canonical row spelled absolute", want)
		}
	}
	if paths := countRows(t, cx.DB, "image_paths"); paths != 2 {
		t.Errorf("%d path rows, want one per file", paths)
	}
	if r := sync(cx); r.Removed != 0 || r.Duplicates != 0 || r.Added != 0 {
		t.Errorf("the absolute-root sync gave %+v, want nothing to do", r)
	}
}

func TestOpen_GivesARowLandedUnderARelativeRootItsFolder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for rel, shade := range map[string]uint8{"gallery/a.png": 10, "gallery/sub/deep/b.png": 200} {
		if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, rel, pngBytes(t, shade), 0o644)
	}
	g := config.Gallery{Name: "rel", GalleryPath: "gallery", DBPath: filepath.Join(dir, "data", "monbooru.db"), ThumbnailsPath: filepath.Join(dir, "data", "thumbnails")}
	relative, err := Open(g)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := relative.Sync(context.Background(), 0, gallery.Naming{}, func(int, int, string) {}, func() {}); err != nil || r.Added != 2 {
		t.Fatalf("test premise broken: the relative-root sync gave %+v, %v", r, err)
	}

	landed := filepath.Join(dir, "gallery", "sub", "deep", "b.png")
	for _, q := range []string{
		`UPDATE images SET canonical_path = ?, folder_path = '' WHERE canonical_path = ?`,
		`UPDATE image_paths SET path = ? WHERE path = ?`,
	} {
		mustExec(t, relative.DB.Write, q, landed, filepath.Join("gallery", "sub", "deep", "b.png"))
	}
	relative.Close()

	g.GalleryPath = filepath.Join(dir, "gallery")
	folders := func() map[string]string {
		t.Helper()
		cx, err := Open(g)
		if err != nil {
			t.Fatal(err)
		}
		defer cx.Close()
		got := map[string]string{}
		rows, err := cx.DB.Read.Query(`SELECT canonical_path, folder_path FROM images`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p, f string
			if err := rows.Scan(&p, &f); err != nil {
				t.Fatal(err)
			}
			got[filepath.Base(p)] = f
		}
		return got
	}
	want := map[string]string{"a.png": "", "b.png": "sub/deep"}
	for i := range 2 {
		if got := folders(); !maps.Equal(got, want) {
			t.Errorf("open %d: folders %v, want %v", i+1, got, want)
		}
	}
}

func TestOpen_RespellsARelativeRowBesideRowsUnderAnotherRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("gallery", 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join("gallery", "a.png"), pngBytes(t, 40), 0o644)
	g := config.Gallery{Name: "rel", GalleryPath: "gallery", DBPath: filepath.Join(dir, "data", "monbooru.db"), ThumbnailsPath: filepath.Join(dir, "data", "thumbnails")}
	relative, err := Open(g)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := relative.Sync(context.Background(), 0, gallery.Naming{}, func(int, int, string) {}, func() {}); err != nil || r.Added != 1 {
		t.Fatalf("test premise broken: the relative-root sync gave %+v, %v", r, err)
	}
	other := filepath.Join(dir, "elsewhere", "c.png")
	if _, err := relative.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES ('c', ?1, 'png', 1);
		 INSERT INTO image_paths (image_id, path, is_canonical) VALUES (last_insert_rowid(), ?1, 1)`, other); err != nil {
		t.Fatal(err)
	}
	relative.Close()

	g.GalleryPath = filepath.Join(dir, "gallery")
	cx, err := Open(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cx.Close)
	for _, want := range []string{filepath.Join(dir, "gallery", "a.png"), other} {
		if n := countRows(t, cx.DB, "image_paths WHERE path = ?", want); n != 1 {
			t.Errorf("no path row spelled %s", want)
		}
	}
}
