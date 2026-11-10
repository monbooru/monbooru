package gallery

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
)

func solidPNG(t *testing.T, w, h int, rgb [3]uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	col := color.RGBA{rgb[0], rgb[1], rgb[2], 0xff}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, col)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func writeTestZip(t *testing.T, dir, name string, entries map[string][]byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f := mustCreate(t, path)
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for k, v := range entries {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatalf("zip create %q: %v", k, err)
		}
		if _, err := w.Write(v); err != nil {
			t.Fatalf("zip write %q: %v", k, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

func TestNaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"page1.jpg", "page2.jpg", true},
		{"page2.jpg", "page10.jpg", true},
		{"page10.jpg", "page2.jpg", false},
		{"page001.jpg", "page2.jpg", true},
		{"a.jpg", "b.jpg", true},
		{"chapter1/01.jpg", "chapter1/10.jpg", true},
		{"chapter1/10.jpg", "chapter2/01.jpg", true},
		{"ch1/p01.jpg", "ch01/p1.jpg", true},
		{"ch01/p1.jpg", "ch1/p01.jpg", false},
	}
	for _, c := range cases {
		if got := NaturalLess(c.a, c.b); got != c.want {
			t.Errorf("NaturalLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestOpenManga_PageListNaturalSort(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{1, 2, 3})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"10.png": pic, "2.png": pic, "1.png": pic, "100.png": pic,
	})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	want := []string{"1.png", "2.png", "10.png", "100.png"}
	if len(m.Pages) != len(want) {
		t.Fatalf("page count = %d, want %d", len(m.Pages), len(want))
	}
	for i, p := range m.Pages {
		if p.Path != want[i] {
			t.Errorf("page[%d] = %q, want %q", i, p.Path, want[i])
		}
	}
}

func TestOpenManga_FilterMacOSX(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png":            pic,
		"__MACOSX/._01.png": []byte("garbage"),
		".DS_Store":         {},
		"Thumbs.db":         {},
		"chapter/02.png":    pic,
	})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 2 {
		var names []string
		for _, p := range m.Pages {
			names = append(names, p.Path)
		}
		t.Fatalf("got %d pages %v, want 2 (MacOSX/junk filtered)", len(m.Pages), names)
	}
}

func TestOpenManga_DeepSubfolders(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"chapter1/01.jpg": pic,
		"chapter1/02.jpg": pic,
		"chapter2/01.jpg": pic,
	})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 3 {
		t.Fatalf("got %d pages, want 3", len(m.Pages))
	}
	if m.Pages[0].Path != "chapter1/01.jpg" || m.Pages[2].Path != "chapter2/01.jpg" {
		t.Errorf("unexpected order: %v / %v", m.Pages[0].Path, m.Pages[2].Path)
	}
}

func TestOpenManga_EmptyArchiveRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTestZip(t, dir, "empty.cbz", map[string][]byte{
		"README.txt": []byte("no pages here"),
	})
	_, err := OpenManga(path)
	if !errors.Is(err, errEmptyManga) {
		t.Fatalf("err = %v, want errEmptyManga", err)
	}
}

func TestOpenManga_CorruptArchiveRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.cbz")
	mustWriteFile(t, path, []byte("PK\x03\x04 not a zip"), 0644)
	if _, err := OpenManga(path); err == nil {
		t.Fatal("expected open failure on corrupt archive")
	}
}

func TestOpenManga_SinglePage(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(m.Pages))
	}
}

func TestExtractPage_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{42, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()

	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "page_0001.png")
	if err := m.extractPage(0, dst); err != nil {
		t.Fatalf("extractPage: %v", err)
	}
	got := mustReadFile(t, dst)
	if !bytes.Equal(got, pic) {
		t.Errorf("extracted bytes differ from source")
	}
}

func TestCoverImage_DecodesPageOne(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"02.png": solidPNG(t, 16, 16, [3]uint8{0, 0, 0}),
		"01.png": pic,
	})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	w, h, err := m.coverDimensions()
	if err != nil {
		t.Fatalf("coverDimensions: %v", err)
	}
	if w != 8 || h != 4 {
		t.Errorf("cover dims = %dx%d, want 8x4 (page 1, not page 2)", w, h)
	}
}

func TestOpenManga_AVIFAndJPEGXLPages(t *testing.T) {
	path := writeTestZip(t, t.TempDir(), "m.cbz", map[string][]byte{
		"01.avif": []byte("avif"),
		"02.JXL":  []byte("jxl"),
		"03.png":  solidPNG(t, 4, 4, [3]uint8{0, 0, 0}),
	})
	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 3 {
		t.Fatalf("got %d pages, want 3", len(m.Pages))
	}
	if !m.ffmpegPage(0) || !m.ffmpegPage(1) || m.ffmpegPage(2) {
		t.Errorf("ffmpeg pages = %v %v %v, want the AVIF and the JPEG XL", m.ffmpegPage(0), m.ffmpegPage(1), m.ffmpegPage(2))
	}
}

func TestCoverDimensions_FFmpegPage(t *testing.T) {
	for _, ext := range []string{".avif", ".jxl"} {
		t.Run(ext, func(t *testing.T) {
			path := writeTestZip(t, t.TempDir(), "m.cbz", map[string][]byte{
				"01" + ext: stillBytes(t, ext, 640, 360),
				"02.png":   solidPNG(t, 16, 16, [3]uint8{0, 0, 0}),
			})
			m := mustOpenManga(t, path)
			defer func() { _ = m.Close() }()
			w, h, err := m.coverDimensions()
			if err != nil || w != 640 || h != 360 {
				t.Errorf("cover = %dx%d (%v), want 640x360", w, h, err)
			}
		})
	}
}

func TestExtractedPageInDir_IgnoresTheViewCopy(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "page_0001_view.jpg"), []byte("copy"), 0o644)
	if got := extractedPageInDir(dir, 1); got != "" {
		t.Errorf("extractedPageInDir = %q, want nothing before the page is extracted", got)
	}
	raw := filepath.Join(dir, "page_0001.jxl")
	mustWriteFile(t, raw, []byte("page"), 0o644)
	if got := extractedPageInDir(dir, 1); got != raw {
		t.Errorf("extractedPageInDir = %q, want %q", got, raw)
	}
}

func TestOpenManga_KeepsBothEntriesOfOneName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.cbz")
	f := mustCreate(t, path)
	zw := zip.NewWriter(f)
	for _, rgb := range [][3]uint8{{255, 0, 0}, {0, 0, 255}} {
		w, err := zw.Create("p.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(solidPNG(t, 4, 4, rgb)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	m := mustOpenManga(t, path)
	defer func() { _ = m.Close() }()
	for n, want := range []uint8{255, 0} {
		rc, err := m.pageReader(n)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if r, _, _, _ := img.At(0, 0).RGBA(); uint8(r>>8) != want {
			t.Errorf("page %d red channel = %d, want %d (archive order)", n+1, r>>8, want)
		}
	}
}
