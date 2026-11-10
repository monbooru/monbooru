package gallery

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func writeMember(t *testing.T, dir, name, fileType, contents string) CBZMember {
	t.Helper()
	p := filepath.Join(dir, name)
	mustWriteFile(t, p, []byte(contents), 0o644)
	return CBZMember{Path: p, FileType: fileType, filename: name}
}

type comicInfo struct {
	Title     string `xml:"Title"`
	PageCount *int   `xml:"PageCount"`
}

func readCBZ(t *testing.T, path string) ([]string, comicInfo) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = zr.Close() }()
	var names []string
	var ci comicInfo
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name != "ComicInfo.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := xml.Unmarshal(raw, &ci); err != nil {
			t.Fatalf("ComicInfo.xml does not parse: %v\n%s", err, raw)
		}
	}
	return names, ci
}

func TestWriteCollectionCBZ(t *testing.T) {
	src, dstDir := t.TempDir(), t.TempDir()
	members := []CBZMember{
		writeMember(t, src, "b.jpg", models.FileTypeJPEG, "one"),
		writeMember(t, src, "a.png", models.FileTypePNG, "two"),
		writeMember(t, src, "c", models.FileTypeWEBP, "three"),
	}
	dst := filepath.Join(dstDir, "out.cbz")

	pages, skipped, err := WriteCollectionCBZ(context.Background(), dst, members, "My Set", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 3 || skipped != 0 {
		t.Errorf("pages, skipped = %d, %d; want 3, 0", pages, skipped)
	}

	names, meta := readCBZ(t, dst)
	want := []string{"0001.jpeg", "0002.png", "0003.webp", "ComicInfo.xml"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, names[i], want[i])
		}
	}
	if meta.Title != "My Set" || meta.PageCount == nil || *meta.PageCount != 3 {
		t.Errorf("ComicInfo = %+v, want title %q and 3 pages", meta, "My Set")
	}

	info := mustStat(t, dst)
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("output dir holds %d entries, want only the archive", len(entries))
	}
}

func TestWriteCollectionCBZ_AVIFAndJPEGXLMembers(t *testing.T) {
	src, dstDir := t.TempDir(), t.TempDir()
	members := []CBZMember{
		writeMember(t, src, "a.avif", models.FileTypeAVIF, "one"),
		writeMember(t, src, "b.jxl", models.FileTypeJXL, "two"),
	}
	dst := filepath.Join(dstDir, "out.cbz")
	if _, _, err := WriteCollectionCBZ(context.Background(), dst, members, "Set", nil); err != nil {
		t.Fatal(err)
	}
	m := mustOpenManga(t, dst)
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 2 || m.Pages[0].OriginalName != "0001.avif" || m.Pages[1].OriginalName != "0002.jxl" {
		t.Errorf("pages = %+v, want 0001.avif and 0002.jxl", m.Pages)
	}
}

func TestWriteCollectionCBZVetoesNonImages(t *testing.T) {
	for _, ft := range []string{models.FileTypeGIF, models.FileTypeMP4, models.FileTypeWEBM, models.FileTypeCBZ} {
		t.Run(ft, func(t *testing.T) {
			src, dstDir := t.TempDir(), t.TempDir()
			members := []CBZMember{
				writeMember(t, src, "ok.jpg", models.FileTypeJPEG, "one"),
				writeMember(t, src, "bad."+ft, ft, "two"),
			}
			dst := filepath.Join(dstDir, "out.cbz")

			if _, _, err := WriteCollectionCBZ(context.Background(), dst, members, "Set", nil); err == nil {
				t.Fatal("want an error naming the offending member")
			}
			entries, err := os.ReadDir(dstDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("output dir holds %d entries, want nothing created", len(entries))
			}
		})
	}
}

func TestWriteCollectionCBZSkipsVanishedMembers(t *testing.T) {
	src, dstDir := t.TempDir(), t.TempDir()
	members := []CBZMember{
		writeMember(t, src, "1.jpg", models.FileTypeJPEG, "one"),
		writeMember(t, src, "2.jpg", models.FileTypeJPEG, "two"),
		writeMember(t, src, "3.jpg", models.FileTypeJPEG, "three"),
	}
	mustRemove(t, members[1].Path)
	dst := filepath.Join(dstDir, "out.cbz")

	pages, skipped, err := WriteCollectionCBZ(context.Background(), dst, members, "Set", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || skipped != 1 {
		t.Errorf("pages, skipped = %d, %d; want 2, 1", pages, skipped)
	}
	names, meta := readCBZ(t, dst)
	want := []string{"0001.jpeg", "0002.jpeg", "ComicInfo.xml"}
	for i := range want {
		if i >= len(names) || names[i] != want[i] {
			t.Fatalf("entries = %v, want %v (contiguous numbering)", names, want)
		}
	}
	if meta.PageCount == nil || *meta.PageCount != 2 {
		t.Errorf("ComicInfo page count = %+v, want 2", meta)
	}
}

func TestWriteCollectionCBZAllMembersMissing(t *testing.T) {
	dstDir := t.TempDir()
	members := []CBZMember{{Path: filepath.Join(t.TempDir(), "gone.jpg"), FileType: models.FileTypeJPEG}}
	dst := filepath.Join(dstDir, "out.cbz")

	if _, _, err := WriteCollectionCBZ(context.Background(), dst, members, "Set", nil); err == nil {
		t.Fatal("want an error when no member could be read")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("archive exists at %s, want none", dst)
	}
}

func TestWriteCollectionCBZCancel(t *testing.T) {
	src, dstDir := t.TempDir(), t.TempDir()
	members := []CBZMember{writeMember(t, src, "1.jpg", models.FileTypeJPEG, "one")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := WriteCollectionCBZ(ctx, filepath.Join(dstDir, "out.cbz"), members, "Set", nil); err == nil {
		t.Fatal("want the cancellation surfaced as an error")
	}
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("output dir holds %d entries, want nothing left behind", len(entries))
	}
}

func TestWriteCollectionCBZDeterministic(t *testing.T) {
	src, dstDir := t.TempDir(), t.TempDir()
	members := []CBZMember{
		writeMember(t, src, "1.jpg", models.FileTypeJPEG, "one"),
		writeMember(t, src, "2.jpg", models.FileTypeJPEG, "two"),
	}
	first := filepath.Join(dstDir, "a.cbz")
	second := filepath.Join(dstDir, "b.cbz")
	for _, dst := range []string{first, second} {
		if _, _, err := WriteCollectionCBZ(context.Background(), dst, members, "Set", nil); err != nil {
			t.Fatal(err)
		}
	}
	a := mustReadFile(t, first)
	b := mustReadFile(t, second)
	if string(a) != string(b) {
		t.Error("two packs of the same members differ; the dedup unwind in generate-cbz depends on them matching")
	}
}
