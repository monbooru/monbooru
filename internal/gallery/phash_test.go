package gallery

import (
	"image"
	"image/color"
	"image/jpeg"
	"math/bits"
	"os"
	"path/filepath"
	"testing"
)

func TestPhashAllZeroBlack(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	h := computePhash(img)
	const want uint64 = 0xFFFFFFFFFFFFFFFE
	if h != want {
		t.Fatalf("all-zero phash: got %016x want %016x", h, want)
	}
}

func TestPhashDeterministic(t *testing.T) {
	img := newGradient(64, 64)
	a := computePhash(img)
	b := computePhash(img)
	if a != b {
		t.Fatal("computePhash is non-deterministic")
	}
}

func TestPhashMirrorCanonical(t *testing.T) {
	img := newGradient(64, 64)
	h := computePhash(img)
	mirror := mirrorImage(img)
	hm := computePhash(mirror)
	if h != hm {
		t.Fatalf("mirror canonicalisation broken: orig=%016x mirror=%016x", h, hm)
	}
}

func TestPhashDifferentImagesDiverge(t *testing.T) {
	left := newGradient(64, 64)
	right := newCheckerboard(64, 64, 8)
	hl := computePhash(left)
	hr := computePhash(right)
	dist := bits.OnesCount64(hl ^ hr)
	if dist < 8 {
		t.Fatalf("gradient vs checkerboard: only %d bits differ (h=%016x %016x)", dist, hl, hr)
	}
}

func TestComputePhashFromThumbRoundTrip(t *testing.T) {
	dir := t.TempDir()
	img := newGradient(96, 96)
	path := filepath.Join(dir, "test.jpg")
	f := mustCreate(t, path)
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		_ = f.Close()
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close jpeg: %v", err)
	}

	h, err := computePhashFromThumb(path)
	if err != nil {
		t.Fatalf("computePhashFromThumb: %v", err)
	}

	f2, err := os.Open(path)
	if err != nil {
		t.Fatalf("reopen temp jpeg: %v", err)
	}
	dec, err := jpeg.Decode(f2)
	_ = f2.Close()
	if err != nil {
		t.Fatalf("decode temp jpeg: %v", err)
	}
	if uint64(h) != computePhash(dec) {
		t.Fatalf("roundtrip mismatch: file=%016x mem=%016x", uint64(h), computePhash(dec))
	}
}

func TestComputePhashFromThumbMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.jpg")
	if _, err := computePhashFromThumb(missing); err == nil {
		t.Fatal("expected error on missing thumbnail, got nil")
	}
}

func newGradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x + y) * 255 / (w + h - 2))
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

func newCheckerboard(w, h, cell int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			on := ((x/cell)+(y/cell))%2 == 0
			c := color.RGBA{A: 255}
			if on {
				c.R, c.G, c.B = 255, 255, 255
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func mirrorImage(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(b.Max.X-1-(x-b.Min.X), y, src.At(x, y))
		}
	}
	return dst
}

func TestGreyResize32_ConcreteTypesMatchTheGenericLoop(t *testing.T) {
	type generic struct{ image.Image }
	var imgs []image.Image
	for _, ratio := range []image.YCbCrSubsampleRatio{image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio444} {
		m := image.NewYCbCr(image.Rect(3, 5, 303, 205), ratio)
		for i := range m.Y {
			m.Y[i] = uint8(i * 7)
		}
		for i := range m.Cb {
			m.Cb[i], m.Cr[i] = uint8(i*3), uint8(255-i*5)
		}
		imgs = append(imgs, m)
	}
	gray := image.NewGray(image.Rect(0, 0, 300, 200))
	for i := range gray.Pix {
		gray.Pix[i] = uint8(i * 13)
	}
	imgs = append(imgs, gray)
	for _, img := range imgs {
		if greyResize32(img) != greyResize32(generic{img}) {
			t.Errorf("%T: the fast path's matrix differs from the generic loop's", img)
		}
		if allocs := testing.AllocsPerRun(3, func() { greyResize32(img) }); allocs > 2 {
			t.Errorf("%T: greyResize32 allocates %.0f times, want a fixed few", img, allocs)
		}
	}
}
