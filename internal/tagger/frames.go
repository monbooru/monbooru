package tagger

import (
	"fmt"
	"image"
	"os"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"golang.org/x/image/draw"
)

// inputKey is one model input; the models that share it share a frame.
type inputKey struct {
	size int
	fill [3]uint8
}

// prepareFrame decodes a frame file once and returns it as RGB8, padded,
// resized and turned, once per input; the decoded frame is dropped before
// any inference runs.
func prepareFrame(path, fileType string, inputs []inputKey) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	img, err := gallery.DecodeImageWithCap(f)
	if err != nil {
		return nil, err
	}
	if img.Bounds().Empty() {
		return nil, fmt.Errorf("image has no pixels (%dx%d)", img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Turned after the padding: the square is centred, so it turns the
	// same, and the full-size frame is never copied.
	// A still image is read as the type sniffed at ingest, which can differ
	// from its extension; an extracted frame or page has its own name.
	switch fileType {
	case "", models.FileTypeMP4, models.FileTypeWEBM, models.FileTypeAVIF, models.FileTypeJXL, models.FileTypeCBZ:
		fileType = models.ExtFileType(path)
	}
	o, _ := metadata.EXIFOrientation(path, fileType)
	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		out[i] = rgb8(gallery.Orient(padAndResize(img, in.size, in.fill), o).(*image.RGBA))
	}
	return out, nil
}

// rgb8 drops the alpha of an opaque square, row by row.
func rgb8(img *image.RGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := make([]byte, 0, w*h*3)
	for y := range h {
		row := img.Pix[y*img.Stride:]
		for x := range w {
			out = append(out, row[4*x], row[4*x+1], row[4*x+2])
		}
	}
	return out
}

// camieDefaultFill is the padding colour of Camie's onnx_inference.py.
var camieDefaultFill = [3]uint8{124, 116, 104}

func padFill(profile Profile) [3]uint8 {
	switch {
	case profile.Pad == "white_square":
		return [3]uint8{0xff, 0xff, 0xff}
	case profile.FillColor != nil:
		return *profile.FillColor
	}
	return camieDefaultFill
}

func padAndResize(src image.Image, size int, fill [3]uint8) *image.RGBA {
	scaled, offX, offY := resizeAspect(src, size)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < len(dst.Pix); i += 4 {
		dst.Pix[i+0] = fill[0]
		dst.Pix[i+1] = fill[1]
		dst.Pix[i+2] = fill[2]
		dst.Pix[i+3] = 0xFF
	}
	// Over composites a (half-)transparent pixel onto the fill, as WD14's
	// reference does onto white; buildTensor ignores alpha.
	draw.Draw(dst, scaled.Bounds().Add(image.Pt(offX, offY)), scaled, image.Point{}, draw.Over)
	return dst
}

// Resizing before padding bounds each transient buffer by size^2, which keeps
// a parallel burst on huge sources inside small container memory caps.
func resizeAspect(src image.Image, size int) (scaled *image.RGBA, offX, offY int) {
	b := src.Bounds()
	w, h := b.Max.X-b.Min.X, b.Max.Y-b.Min.Y
	scaleW, scaleH := size, size
	if w >= h {
		scaleH = max(1, h*size/w)
	} else {
		scaleW = max(1, w*size/h)
	}
	scaled = image.NewRGBA(image.Rect(0, 0, scaleW, scaleH))
	// Not ApproxBiLinear: it reads four source pixels at any ratio, and
	// the aliasing it leaves gets tagged as pixel art.
	draw.BiLinear.Scale(scaled, scaled.Bounds(), src, b, draw.Src, nil)
	return scaled, (size - scaleW) / 2, (size - scaleH) / 2
}

func inferInputSize(dims []int64, layout string) int {
	if len(dims) != 4 {
		return 0
	}
	var d int64
	switch layout {
	case "nhwc":
		d = dims[1]
	case "nchw":
		d = dims[2]
	default:
		return 0
	}
	if d <= 0 {
		return 0
	}
	return int(d)
}
