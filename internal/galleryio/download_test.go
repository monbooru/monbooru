package galleryio

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func readZip(t *testing.T, buf *bytes.Buffer) *zip.Reader {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

func zipEntryBytes(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriteDownload_StoresEachFileUnderItsName(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	first := writeImage(t, cx, "2024/a.png", makeUniquePNG(t, 1))
	second := writeImage(t, cx, "b.png", makeUniquePNG(t, 2))

	var buf bytes.Buffer
	var progress []int
	written, skipped, err := WriteDownload(context.Background(), &buf, nil, []DownloadEntry{
		{Path: second.CanonicalPath, Name: "01-b.png"},
		{Path: first.CanonicalPath, Name: "02-a.png"},
	}, func(n int) { progress = append(progress, n) })
	if err != nil || written != 2 || skipped != 0 {
		t.Fatalf("WriteDownload = %d, %d, %v; want 2, 0, nil", written, skipped, err)
	}
	if !slices.Equal(progress, []int{1, 2}) {
		t.Errorf("progress = %v, want [1 2]", progress)
	}

	zr := readZip(t, &buf)
	if len(zr.File) != 2 || zr.File[0].Name != "01-b.png" || zr.File[1].Name != "02-a.png" {
		t.Fatalf("entries = %v, want 01-b.png then 02-a.png", zr.File)
	}
	for i, src := range []string{second.CanonicalPath, first.CanonicalPath} {
		f := zr.File[i]
		if f.Method != zip.Store {
			t.Errorf("%s method = %d, want stored", f.Name, f.Method)
		}
		want, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(zipEntryBytes(t, f), want) {
			t.Errorf("%s bytes differ from %s", f.Name, src)
		}
		info, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		if f.Modified.Unix() != info.ModTime().Unix() {
			t.Errorf("%s modified = %v, want the file's %v", f.Name, f.Modified, info.ModTime())
		}
	}
}

func TestWriteDownload_SkipsAFileThatVanished(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	kept := writeImage(t, cx, "kept.png", makeUniquePNG(t, 3))

	var buf bytes.Buffer
	written, skipped, err := WriteDownload(context.Background(), &buf, nil, []DownloadEntry{
		{Path: filepath.Join(cx.GalleryPath, "gone.png"), Name: "gone.png"},
		{Path: kept.CanonicalPath, Name: "kept.png"},
	}, nil)
	if err != nil || written != 1 || skipped != 1 {
		t.Fatalf("WriteDownload = %d, %d, %v; want 1, 1, nil", written, skipped, err)
	}
	if zr := readZip(t, &buf); len(zr.File) != 1 || zr.File[0].Name != "kept.png" {
		t.Errorf("entries = %v, want kept.png alone", zr.File)
	}
}

func TestWriteDownload_PutsTheManifestFirst(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	img := writeImage(t, cx, "one.png", makeUniquePNG(t, 4))
	manifest := []byte(`{"version":1,"images":[]}`)

	var buf bytes.Buffer
	if _, _, err := WriteDownload(context.Background(), &buf, manifest, []DownloadEntry{
		{Path: img.CanonicalPath, Name: "one.png"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	zr := readZip(t, &buf)
	if len(zr.File) != 2 || zr.File[0].Name != "tags.json" {
		t.Fatalf("entries = %v, want tags.json first", zr.File)
	}
	if got := zipEntryBytes(t, zr.File[0]); !bytes.Equal(got, manifest) {
		t.Errorf("tags.json = %q, want %q", got, manifest)
	}
}

func TestWriteDownload_StopsWhenCancelled(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	img := writeImage(t, cx, "one.png", makeUniquePNG(t, 5))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	written, _, err := WriteDownload(ctx, io.Discard, nil, []DownloadEntry{
		{Path: img.CanonicalPath, Name: "one.png"},
	}, nil)
	if err == nil || written != 0 {
		t.Errorf("WriteDownload = %d, %v; want 0 and the context's error", written, err)
	}
}

func TestDownloadManifest_NamesEachImageAsTheZipDoes(t *testing.T) {
	cx := newGallery(t, t.TempDir(), "stock")
	solo := writeImage(t, cx, "2024/solo.png", makeUniquePNG(t, 6))
	pair := writeImage(t, cx, "pair.png", makeUniquePNG(t, 7))
	left := writeImage(t, cx, "left_out.png", makeUniquePNG(t, 8))
	for id, tag := range map[int64]int64{
		solo.ID: mustTag(t, cx.TagSvc, "solo", generalCategoryID(t, cx)).ID,
		pair.ID: mustTag(t, cx.TagSvc, "hakurei_reimu", LookupCategoryID(cx.DB, "character")).ID,
		left.ID: mustTag(t, cx.TagSvc, "unseen", generalCategoryID(t, cx)).ID,
	} {
		if err := cx.TagSvc.AddTagToImage(id, tag, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	b, err := DownloadManifest(cx, map[int64]string{solo.ID: "01-solo.png", pair.ID: "02-pair.png"})
	if err != nil {
		t.Fatal(err)
	}
	var mf LightManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		t.Fatalf("decode: %v (%s)", err, b)
	}
	if mf.Version != LightManifestVersion || len(mf.Images) != 2 {
		t.Fatalf("manifest = %+v, want version %d with two images", mf, LightManifestVersion)
	}
	got := map[string][]string{}
	for _, img := range mf.Images {
		got[img.Path] = img.Tags
	}
	if !slices.Contains(got["01-solo.png"], "solo") {
		t.Errorf("01-solo.png tags = %v, want solo", got["01-solo.png"])
	}
	if !slices.Contains(got["02-pair.png"], "character:hakurei_reimu") {
		t.Errorf("02-pair.png tags = %v, want character:hakurei_reimu", got["02-pair.png"])
	}
}
