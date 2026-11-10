//go:build tagger

package tagger

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func makeTestImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x * 7) & 0xff),
				G: uint8((y*11 + 30) & 0xff),
				B: uint8((x*y + 17) & 0xff),
				A: 0xff,
			})
		}
	}
	return img
}

func TestBuildTensor_WD14_BitIdentical(t *testing.T) {
	const size = 16
	src := makeTestImage(20, 12)
	processed := padAndResize(src, size, wd14Profile)
	got := make([]float32, 3*size*size)
	shape, err := buildTensor(processed, got, size, wd14Profile)
	if err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	if shape[0] != 1 || shape[1] != int64(size) || shape[2] != int64(size) || shape[3] != 3 {
		t.Errorf("shape = %v, want [1,%d,%d,3]", shape, size, size)
	}
	pix := processed.Pix
	stride := processed.Stride
	want := make([]float32, size*size*3)
	for y := 0; y < size; y++ {
		row := pix[y*stride:]
		for x := 0; x < size; x++ {
			s := x * 4
			d := (y*size + x) * 3
			want[d+0] = float32(row[s+2])
			want[d+1] = float32(row[s+1])
			want[d+2] = float32(row[s+0])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestBuildTensor_NCHWPresetsAreBitIdentical(t *testing.T) {
	const size = 16
	for _, tc := range []struct {
		name      string
		prof      Profile
		mean, std [3]float32
	}{
		{"camie_imagenet", Profile{Layout: "nchw", Channels: "rgb", Normalize: "imagenet", Pad: "white_square"}, imageNetMean, imageNetStd},
		{"joytag", joytagProfile, clipMean, clipStd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			processed := padAndResize(makeTestImage(20, 12), size, tc.prof)
			got := make([]float32, 3*size*size)
			shape, err := buildTensor(processed, got, size, tc.prof)
			if err != nil {
				t.Fatalf("buildTensor: %v", err)
			}
			if shape[0] != 1 || shape[1] != 3 || shape[2] != int64(size) || shape[3] != int64(size) {
				t.Errorf("shape = %v, want [1,3,%d,%d]", shape, size, size)
			}
			pix := processed.Pix
			stride := processed.Stride
			plane := size * size
			want := make([]float32, 3*plane)
			for y := 0; y < size; y++ {
				row := pix[y*stride:]
				for x := 0; x < size; x++ {
					s := x * 4
					off := y*size + x
					want[0*plane+off] = (float32(row[s+0])/255 - tc.mean[0]) / tc.std[0]
					want[1*plane+off] = (float32(row[s+1])/255 - tc.mean[1]) / tc.std[1]
					want[2*plane+off] = (float32(row[s+2])/255 - tc.mean[2]) / tc.std[2]
				}
			}
			if len(got) != len(want) {
				t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
			}
			for i := range want {

				if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
					t.Fatalf("idx %d: got %v (bits %x), want %v (bits %x)",
						i, got[i], math.Float32bits(got[i]),
						want[i], math.Float32bits(want[i]))
				}
			}
		})
	}
}

func TestBuildTensor_MeanStd(t *testing.T) {
	const size = 8
	half := &[3]float32{0.5, 0.5, 0.5}
	for _, layout := range []string{"nchw", "nhwc"} {
		prof := Profile{Layout: layout, Channels: "rgb", Normalize: "mean_std", NormScale: 255, NormMean: half, NormStd: half, Pad: "white_square"}
		processed := padAndResize(makeTestImage(10, 6), size, prof)
		got := make([]float32, 3*size*size)
		if _, err := buildTensor(processed, got, size, prof); err != nil {
			t.Fatalf("%s: %v", layout, err)
		}
		px := processed.Pix[0:3]
		want := [3]float32{}
		for c := range 3 {
			want[c] = (float32(px[c])/255 - 0.5) / 0.5
		}
		first := [3]float32{got[0], got[size*size], got[2*size*size]}
		if layout == "nhwc" {
			first = [3]float32{got[0], got[1], got[2]}
		}
		if first != want {
			t.Errorf("%s: first pixel = %v, want %v", layout, first, want)
		}
	}
}

func TestBuildTensor_NHWCWithAPreset(t *testing.T) {
	const size = 8
	prof := Profile{Layout: "nhwc", Channels: "bgr", Normalize: "div255", Pad: "white_square"}
	processed := padAndResize(makeTestImage(10, 6), size, prof)
	got := make([]float32, 3*size*size)
	if _, err := buildTensor(processed, got, size, prof); err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	px := processed.Pix
	if want := float32(px[2]) / 255; got[0] != want {
		t.Errorf("first channel = %v, want blue/255 = %v", got[0], want)
	}
}

func TestPadAndResize_WhiteSquare_AlphaForced(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))

	dst := padAndResize(src, 8, wd14Profile)
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
	dst := padAndResize(src, 8, wd14Profile)
	for i := 0; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] < 250 || dst.Pix[i+1] < 250 || dst.Pix[i+2] < 250 || dst.Pix[i+3] != 0xff {
			t.Fatalf("pix %d = %v, want half-transparent white over white", i, dst.Pix[i:i+4])
		}
	}
}

func TestPadAndResize_MeanColorAspect_TransparentTakesTheFill(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	dst := padAndResize(src, 8, Profile{Pad: "mean_color_aspect"})
	for i := 0; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] != camieDefaultFill[0] || dst.Pix[i+1] != camieDefaultFill[1] || dst.Pix[i+2] != camieDefaultFill[2] || dst.Pix[i+3] != 0xff {
			t.Fatalf("pix %d = %v, want the fill %v", i, dst.Pix[i:i+4], camieDefaultFill)
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

	dst := padAndResize(src, size, wd14Profile)
	lo, hi := uint8(0xff), uint8(0)
	for i := 0; i < len(dst.Pix); i += 4 {
		lo, hi = min(lo, dst.Pix[i]), max(hi, dst.Pix[i])
	}
	if hi-lo > 8 {
		t.Fatalf("output spans %d..%d: the reduction is sampling the source, not averaging it", lo, hi)
	}
}

func TestPadAndResize_MeanColorAspect(t *testing.T) {

	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i+0] = 0xff
		src.Pix[i+1] = 0x00
		src.Pix[i+2] = 0x00
		src.Pix[i+3] = 0xff
	}
	prof := Profile{Pad: "mean_color_aspect"}
	dst := padAndResize(src, 64, prof)
	if dst.Bounds().Dx() != 64 || dst.Bounds().Dy() != 64 {
		t.Fatalf("size = %v, want 64x64", dst.Bounds())
	}
	stride := dst.Stride
	off := func(x, y int) int { return y*stride + x*4 }
	if dst.Pix[off(0, 0)+0] != camieDefaultFill[0] ||
		dst.Pix[off(0, 0)+1] != camieDefaultFill[1] ||
		dst.Pix[off(0, 0)+2] != camieDefaultFill[2] {
		t.Errorf("top-left = (%d,%d,%d), want camie default %v",
			dst.Pix[off(0, 0)], dst.Pix[off(0, 0)+1], dst.Pix[off(0, 0)+2], camieDefaultFill)
	}
	if dst.Pix[off(32, 32)+0] != 0xff ||
		dst.Pix[off(32, 32)+1] != 0x00 ||
		dst.Pix[off(32, 32)+2] != 0x00 {
		t.Errorf("centre = (%d,%d,%d), want red",
			dst.Pix[off(32, 32)], dst.Pix[off(32, 32)+1], dst.Pix[off(32, 32)+2])
	}
}

func TestPadAndResize_MeanColorAspect_FillColorOverride(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i+0] = 0xff
		src.Pix[i+3] = 0xff
	}
	prof := Profile{Pad: "mean_color_aspect", FillColor: &[3]uint8{10, 20, 30}}
	dst := padAndResize(src, 64, prof)

	off := dst.Stride*0 + 0
	if dst.Pix[off] != 10 || dst.Pix[off+1] != 20 || dst.Pix[off+2] != 30 {
		t.Errorf("override fill = (%d,%d,%d), want (10,20,30)",
			dst.Pix[off], dst.Pix[off+1], dst.Pix[off+2])
	}
}

func TestPadAndResize_MeanColorAspect_BlackFillIsKept(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	dst := padAndResize(src, 64, Profile{Pad: "mean_color_aspect", FillColor: &[3]uint8{0, 0, 0}})
	if p := dst.Pix[0:3]; p[0] != 0 || p[1] != 0 || p[2] != 0 {
		t.Errorf("black fill = %v, want (0,0,0)", p)
	}
}

func TestPadAndResize_FillTakesTheProfileColour(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	dst := padAndResize(src, 64, Profile{Pad: "fill", FillColor: &[3]uint8{0, 0, 0}})
	if p := dst.Pix[0:3]; p[0] != 0 || p[1] != 0 || p[2] != 0 {
		t.Errorf("fill = %v, want (0,0,0)", p)
	}
}

func TestBuildTensor_PoolReuse_NoStaleData(t *testing.T) {
	const size = 16
	src := makeTestImage(20, 12)
	processed := padAndResize(src, size, wd14Profile)

	buf := make([]float32, 3*size*size)
	for i := range buf {
		buf[i] = -777
	}
	if _, err := buildTensor(processed, buf, size, wd14Profile); err != nil {
		t.Fatalf("buildTensor: %v", err)
	}
	for i, v := range buf {
		if v == -777 {
			t.Fatalf("tensor[%d] left at sentinel - buildTensor missed this position", i)
		}
	}

	prof := Profile{Layout: "nchw", Channels: "rgb", Normalize: "imagenet", Pad: "white_square"}
	processed = padAndResize(src, size, prof)
	for i := range buf {
		buf[i] = -777
	}
	if _, err := buildTensor(processed, buf, size, prof); err != nil {
		t.Fatalf("buildTensor (nchw): %v", err)
	}
	for i, v := range buf {
		if v == -777 {
			t.Fatalf("nchw tensor[%d] left at sentinel", i)
		}
	}
}

func TestInferInputSize_ReadsFromONNXShape(t *testing.T) {
	cases := []struct {
		name   string
		layout string
		dims   []int64
		want   int
	}{
		{"NHWC standard 448", "nhwc", []int64{1, 448, 448, 3}, 448},
		{"NCHW standard 448", "nchw", []int64{1, 3, 448, 448}, 448},
		{"NCHW Camie 512", "nchw", []int64{1, 3, 512, 512}, 512},
		{"dynamic axis", "nchw", []int64{-1, 3, -1, -1}, 0},
		{"unexpected rank", "nhwc", []int64{448, 448, 3}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inferInputSize(c.dims, c.layout); got != c.want {
				t.Errorf("inferInputSize(%v, %q) = %d, want %d", c.dims, c.layout, got, c.want)
			}
		})
	}
}

func TestCheckLabelWidth(t *testing.T) {
	cases := []struct {
		name string
		dims []int64
		n    int
		ok   bool
	}{
		{"equal", []int64{1, 10861}, 10861, true},
		{"dynamic width", []int64{-1, -1}, 10861, true},
		{"no dimensions", nil, 10861, true},
		{"one label short", []int64{1, 10861}, 10860, false},
		{"one label over", []int64{1, 10861}, 10862, false},
	}
	for _, c := range cases {
		if err := checkLabelWidth(c.dims, c.n); (err == nil) != c.ok {
			t.Errorf("%s: checkLabelWidth(%v, %d) = %v, want ok %v", c.name, c.dims, c.n, err, c.ok)
		}
	}
}

func TestTensorPoolRoundTrips(t *testing.T) {
	const size = 64
	buf := acquireTensor(size)
	if len(buf) != 3*size*size {
		t.Fatalf("len = %d, want %d", len(buf), 3*size*size)
	}
	buf[0] = 1
	releaseTensor(size, buf)
	if again := acquireTensor(size); len(again) != 3*size*size {
		t.Errorf("len after reuse = %d", len(again))
	}
}
