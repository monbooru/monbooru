//go:build tagger

package tagger

import (
	"cmp"
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

var inputTensorPools sync.Map

// The pool holds *[]float32, not []float32: pooling the slice itself boxes
// its header into the interface on every Put, which allocates.
func acquireTensor(size int) []float32 {
	if p, ok := inputTensorPools.Load(size); ok {
		return *p.(*sync.Pool).Get().(*[]float32)
	}
	n := size
	fresh := &sync.Pool{New: func() any { buf := make([]float32, 3*n*n); return &buf }}
	actual, _ := inputTensorPools.LoadOrStore(size, fresh)
	return *actual.(*sync.Pool).Get().(*[]float32)
}

func releaseTensor(size int, buf []float32) {
	if p, ok := inputTensorPools.Load(size); ok {
		p.(*sync.Pool).Put(&buf)
	}
}

var (
	imageNetMean = [3]float32{0.485, 0.456, 0.406}
	imageNetStd  = [3]float32{0.229, 0.224, 0.225}
)

// OpenAI CLIP values from joytag's preprocess, kept verbatim to match its
// reference output.
var (
	clipMean = [3]float32{0.48145466, 0.4578275, 0.40821073}
	clipStd  = [3]float32{0.26862954, 0.26130258, 0.27577711}
)

var normalizePresets = map[string]struct {
	scale     float32
	mean, std [3]float32
}{
	"none":     {1, [3]float32{}, [3]float32{1, 1, 1}},
	"div255":   {255, [3]float32{}, [3]float32{1, 1, 1}},
	"imagenet": {255, imageNetMean, imageNetStd},
	"clip":     {255, clipMean, clipStd},
}

// normalizeTable indexes by tensor channel and byte value. Each entry
// is computed with the reference expression, so a preset's tensor is
// bit-identical to its model's own preprocessing.
func normalizeTable(profile Profile) *[3][256]float32 {
	scale, mean, std := float32(1), [3]float32{}, [3]float32{1, 1, 1}
	if p, ok := normalizePresets[profile.Normalize]; ok {
		scale, mean, std = p.scale, p.mean, p.std
	} else if profile.NormMean != nil && profile.NormStd != nil {
		scale, mean, std = cmp.Or(profile.NormScale, 1), *profile.NormMean, *profile.NormStd
	}
	var t [3][256]float32
	for c := range 3 {
		for v := range 256 {
			t[c][v] = (float32(v)/scale - mean[c]) / std[c]
		}
	}
	return &t
}

// buildTensor overwrites every element: pooled buffers are not zeroed.
// pix is a size x size RGB8 frame.
func buildTensor(pix []byte, tensor []float32, size int, profile Profile) (ort.Shape, error) {
	if len(pix) != 3*size*size {
		return nil, fmt.Errorf("buildTensor: %d bytes for a %d px frame", len(pix), size)
	}
	stride := 3 * size
	norm := normalizeTable(profile)
	r, b := 0, 2
	if profile.Channels == "bgr" {
		r, b = 2, 0
	}

	switch profile.Layout {
	case "nhwc":
		for y := 0; y < size; y++ {
			row := pix[y*stride:]
			for x := 0; x < size; x++ {
				src := x * 3
				dst := (y*size + x) * 3
				tensor[dst+0] = norm[0][row[src+r]]
				tensor[dst+1] = norm[1][row[src+1]]
				tensor[dst+2] = norm[2][row[src+b]]
			}
		}
		return ort.NewShape(1, int64(size), int64(size), 3), nil

	case "nchw":
		plane := size * size
		for y := 0; y < size; y++ {
			row := pix[y*stride:]
			for x := 0; x < size; x++ {
				src := x * 3
				off := y*size + x
				tensor[0*plane+off] = norm[0][row[src+r]]
				tensor[1*plane+off] = norm[1][row[src+1]]
				tensor[2*plane+off] = norm[2][row[src+b]]
			}
		}
		return ort.NewShape(1, 3, int64(size), int64(size)), nil
	}
	return nil, fmt.Errorf("buildTensor: unsupported layout %q", profile.Layout)
}
