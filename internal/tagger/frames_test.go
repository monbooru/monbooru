package tagger

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var (
	white = [3]uint8{0xff, 0xff, 0xff}

	camieFill = [3]uint8{124, 116, 104}
)

func TestPrepareFrameRefusesAnEmptyImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 0, 0)), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareFrame(path, "jpeg", []inputKey{{448, white}}); err == nil {
		t.Error("prepareFrame took an image with no pixels")
	}
}

func TestPrepareFrameTurnsByEXIFOrientation(t *testing.T) {
	cases := []struct{ name, fileType string }{
		{"turned.jpg", "jpeg"},
		{"turned.png", "jpeg"},
		{"page.jpg", "cbz"},
	}
	for _, c := range cases {
		path := writeOrientation6JPEG(t, c.name)
		inputs, err := prepareFrame(path, c.fileType, []inputKey{{40, white}})
		if err != nil {
			t.Fatal(err)
		}

		for _, p := range []struct {
			x, y int
			red  bool
		}{{25, 5, true}, {15, 5, false}, {25, 35, false}, {15, 35, false}} {
			px := inputs[0][(p.y*40+p.x)*3:][:3]
			if red := px[0] > 200 && px[2] < 60; red != p.red {
				t.Errorf("%s as %s: pixel (%d,%d) = %v, red %v, want %v", c.name, c.fileType, p.x, p.y, px, red, p.red)
			}
		}
	}
}

func writeOrientation6JPEG(t *testing.T, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			img.Set(x, y, color.RGBA{0, 0, 255, 255})
			if x < 20 && y < 10 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	exif := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	app1 := append([]byte{0xff, 0xe1, 0, byte(len(exif) + 2)}, exif...)
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, slices.Concat(buf.Bytes()[:2], app1, buf.Bytes()[2:]), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRGB8DropsAlphaRowByRow(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	copy(img.Pix, []byte{1, 2, 3, 255, 4, 5, 6, 255, 7, 8, 9, 255, 10, 11, 12, 255})
	if got := rgb8(img); !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}) {
		t.Errorf("rgb8 = %v", got)
	}
}

func TestPadAndResize_WhiteSquare_AlphaForced(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	dst := padAndResize(src, 8, white)
	for i := 0; i+3 < len(dst.Pix); i += 4 {
		if dst.Pix[i] != 0xff || dst.Pix[i+1] != 0xff || dst.Pix[i+2] != 0xff || dst.Pix[i+3] != 0xff {
			t.Fatalf("pix %d not forced to opaque white: %v", i, dst.Pix[i:i+4])
		}
	}
}

func TestPadAndResize_WhiteSquare_BlendsHalfTransparentOverWhite(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(src.Pix); i += 4 {
		src.Pix[i], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3] = 0xff, 0xff, 0xff, 0x80
	}
	dst := padAndResize(src, 8, white)
	for i := 0; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] < 250 || dst.Pix[i+1] < 250 || dst.Pix[i+2] < 250 || dst.Pix[i+3] != 0xff {
			t.Fatalf("pix %d = %v, want half-transparent white over white", i, dst.Pix[i:i+4])
		}
	}
}

func TestPadAndResize_MeanColorAspect_TransparentTakesTheFill(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	dst := padAndResize(src, 8, camieFill)
	for i := 0; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] != camieFill[0] || dst.Pix[i+1] != camieFill[1] || dst.Pix[i+2] != camieFill[2] || dst.Pix[i+3] != 0xff {
			t.Fatalf("pix %d = %v, want the fill %v", i, dst.Pix[i:i+4], camieFill)
		}
	}
}

func TestPadAndResize_Antialiases(t *testing.T) {
	const srcEdge, size = 449, 64
	src := image.NewRGBA(image.Rect(0, 0, srcEdge, srcEdge))
	for y := 0; y < srcEdge; y++ {
		for x := 0; x < srcEdge; x++ {
			v := uint8(0)
			if (x+y)%2 == 0 {
				v = 0xff
			}
			i := src.PixOffset(x, y)
			src.Pix[i], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3] = v, v, v, 0xff
		}
	}

	dst := padAndResize(src, size, white)
	lo, hi := uint8(0xff), uint8(0)
	for i := 0; i < len(dst.Pix); i += 4 {
		lo, hi = min(lo, dst.Pix[i]), max(hi, dst.Pix[i])
	}
	if hi-lo > 8 {
		t.Fatalf("output spans %d..%d: the reduction is sampling the source, not averaging it", lo, hi)
	}
}

func TestPadAndResize_CentresOnTheFill(t *testing.T) {

	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i+0] = 0xff
		src.Pix[i+1] = 0x00
		src.Pix[i+2] = 0x00
		src.Pix[i+3] = 0xff
	}
	dst := padAndResize(src, 64, camieFill)
	if dst.Bounds().Dx() != 64 || dst.Bounds().Dy() != 64 {
		t.Fatalf("size = %v, want 64x64", dst.Bounds())
	}
	stride := dst.Stride
	off := func(x, y int) int { return y*stride + x*4 }
	if dst.Pix[off(0, 0)+0] != camieFill[0] ||
		dst.Pix[off(0, 0)+1] != camieFill[1] ||
		dst.Pix[off(0, 0)+2] != camieFill[2] {
		t.Errorf("top-left = (%d,%d,%d), want camie default %v",
			dst.Pix[off(0, 0)], dst.Pix[off(0, 0)+1], dst.Pix[off(0, 0)+2], camieFill)
	}
	if dst.Pix[off(32, 32)+0] != 0xff ||
		dst.Pix[off(32, 32)+1] != 0x00 ||
		dst.Pix[off(32, 32)+2] != 0x00 {
		t.Errorf("centre = (%d,%d,%d), want red",
			dst.Pix[off(32, 32)], dst.Pix[off(32, 32)+1], dst.Pix[off(32, 32)+2])
	}
}

func TestPadAndResize_TakesTheFillItIsGiven(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i+0] = 0xff
		src.Pix[i+3] = 0xff
	}
	dst := padAndResize(src, 64, [3]uint8{10, 20, 30})

	if p := dst.Pix[0:3]; p[0] != 10 || p[1] != 20 || p[2] != 30 {
		t.Errorf("override fill = %v, want (10,20,30)", p)
	}
}

func TestPadAndResize_BlackFillIsKept(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	dst := padAndResize(src, 64, [3]uint8{0, 0, 0})
	if p := dst.Pix[0:3]; p[0] != 0 || p[1] != 0 || p[2] != 0 {
		t.Errorf("black fill = %v, want (0,0,0)", p)
	}
}
