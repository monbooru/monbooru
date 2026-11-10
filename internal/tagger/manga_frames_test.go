//go:build tagger

package tagger

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

func solidPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeCBZ(t *testing.T, dir, name string, pages [][]byte) string {
	t.Helper()
	names := make([]string, len(pages))
	for i := range pages {
		names[i] = filepath.Join("p", fmt.Sprintf("%03d.png", i+1))
	}
	return makeNamedCBZ(t, dir, name, names, pages)
}

func makeNamedCBZ(t *testing.T, dir, name string, names []string, pages [][]byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create cbz: %v", err)
	}
	zw := zip.NewWriter(f)
	for i, body := range pages {
		w, err := zw.Create(names[i])
		if err != nil {
			_ = f.Close()
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(body); err != nil {
			_ = f.Close()
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("zip close: %v", err)
	}
	_ = f.Close()
	return path
}

func TestFramesForTagging_CBZIteratesEveryPage(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	cbz := makeCBZ(t, dir, "m.cbz", [][]byte{pic, pic, pic, pic, pic})

	cacheRoot := t.TempDir()
	paths, cleanup := framesForTagging(cbz, "cbz", cacheRoot, 42)
	defer cleanup()

	if len(paths) != 5 {
		t.Fatalf("got %d frame paths, want 5", len(paths))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("frame %q not on disk: %v", p, err)
		}
	}
}

func TestFramesForTagging_CBZKeepsPagesOutOfTheReaderCache(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	cbz := makeCBZ(t, dir, "m.cbz", [][]byte{pic, pic})
	dataDir := t.TempDir()
	cacheRoot := filepath.Join(dataDir, "manga")
	paths, cleanup := framesForTagging(cbz, "cbz", cacheRoot, 7)
	if len(paths) != 2 {
		t.Fatalf("got %d frames, want 2", len(paths))
	}
	for _, p := range paths {
		if rel, err := filepath.Rel(cacheRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("page %q sits in the reader cache %q", p, cacheRoot)
		}
		if rel, err := filepath.Rel(dataDir, p); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("page %q left the gallery's data dir %q", p, dataDir)
		}
	}
	cleanup()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("page %q outlived the cleanup", p)
		}
	}
}

func TestFramesForTagging_StaticImageUnchanged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.png")
	mustWriteFile(t, src, solidPNG(t, 4, 4, color.RGBA{0, 0, 0, 255}), 0o644)
	paths, cleanup := framesForTagging(src, "png", "", 1)
	defer cleanup()
	if len(paths) != 1 || paths[0] != src {
		t.Errorf("static branch = %v, want [%q]", paths, src)
	}
}

func TestFramesForTagging_FFmpegStills(t *testing.T) {
	for _, tc := range []struct{ fileType, codec string }{{"avif", "libaom-av1"}, {"jxl", "libjxl"}} {
		t.Run(tc.fileType, func(t *testing.T) {
			requireFFmpeg(t)
			src := filepath.Join(t.TempDir(), "x."+tc.fileType)
			cmd := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
				"-f", "lavfi", "-i", "testsrc2=size=64x48", "-frames:v", "1", "-c:v", tc.codec, "--", src)
			if err := cmd.Run(); err != nil {
				t.Skipf("ffmpeg cannot encode %s: %v", tc.codec, err)
			}

			paths, cleanup := framesForTagging(src, tc.fileType, "", 1)
			if len(paths) != 1 || paths[0] == src {
				cleanup()
				t.Fatalf("frames = %v, want one rendered WebP", paths)
			}
			f, err := os.Open(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := webp.DecodeConfig(f)
			_ = f.Close()
			if err != nil || cfg.Width != 64 || cfg.Height != 48 {
				t.Errorf("frame = %dx%d (%v), want a 64x48 WebP", cfg.Width, cfg.Height, err)
			}
			cleanup()
			if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
				t.Errorf("cleanup left %s behind: %v", paths[0], err)
			}
		})
	}
}

func TestFramesForTagging_CBZRendersAJPEGXLPage(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "p.jxl")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "testsrc2=size=64x48", "-frames:v", "1", "-c:v", "libjxl", "--", src)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg cannot encode libjxl: %v", err)
	}
	jxl, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cbz := makeNamedCBZ(t, dir, "m.cbz", []string{"01.jxl", "02.png"},
		[][]byte{jxl, solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})})

	paths, cleanup := framesForTagging(cbz, "cbz", t.TempDir(), 5)
	if len(paths) != 2 {
		cleanup()
		t.Fatalf("got %d frames, want 2", len(paths))
	}
	f, err := os.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := webp.DecodeConfig(f)
	_ = f.Close()
	if err != nil || cfg.Width != 64 || cfg.Height != 48 {
		t.Errorf("page 1 frame = %dx%d (%v), want a 64x48 WebP", cfg.Width, cfg.Height, err)
	}
	cleanup()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("cleanup left %q behind: %v", p, err)
		}
	}
}

func TestFramesForTagging_TransparentJXLKeepsItsAlpha(t *testing.T) {
	requireFFmpeg(t)
	src := filepath.Join(t.TempDir(), "clear.jxl")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "color=c=red@0.0:s=32x32,format=rgba", "-frames:v", "1", "-c:v", "libjxl", "--", src)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg cannot encode libjxl: %v", err)
	}
	paths, cleanup := framesForTagging(src, "jxl", "", 1)
	defer cleanup()
	if len(paths) != 1 {
		t.Fatalf("frames = %v, want one", paths)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	img, err := webp.Decode(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("frame is not a WebP: %v", err)
	}
	if _, _, _, a := img.At(16, 16).RGBA(); a != 0 {
		t.Errorf("alpha = %d, want the transparent pixel kept transparent", a)
	}
}

func TestFramesForTagging_UndecodableStillYieldsNothing(t *testing.T) {
	src := filepath.Join(t.TempDir(), "broken.jxl")
	mustWriteFile(t, src, []byte{0xFF, 0x0A, 0, 0}, 0o644)
	paths, cleanup := framesForTagging(src, "jxl", "", 1)
	defer cleanup()
	if len(paths) != 0 {
		t.Errorf("frames = %v, want none", paths)
	}
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
}
