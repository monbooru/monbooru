package tagger

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type pb []byte

func (b pb) varint(num int, v int64) pb {
	b = binary.AppendUvarint(b, uint64(num)<<3)
	return binary.AppendUvarint(b, uint64(v))
}

func (b pb) bytes(num int, p []byte) pb {
	b = binary.AppendUvarint(b, uint64(num)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(p)))
	return append(b, p...)
}

func (b pb) str(num int, s string) pb { return b.bytes(num, []byte(s)) }

func valueInfo(name string, dims ...int64) pb {
	var shape pb
	for _, d := range dims {
		shape = shape.bytes(1, pb{}.varint(1, d))
	}
	return pb{}.str(1, name).bytes(2, pb{}.bytes(1, pb{}.varint(1, 1).bytes(2, shape)))
}

func TestONNXInputDimsSkipsInitializersAndReadsUnnamedAsMinusOne(t *testing.T) {
	batch := pb{}.str(1, "x").bytes(2, pb{}.bytes(1, pb{}.varint(1, 1).bytes(2,
		pb{}.bytes(1, pb{}.str(2, "batch")).bytes(1, pb{}.varint(1, 448)).bytes(1, pb{}.varint(1, 448)).bytes(1, pb{}.varint(1, 3)))))
	graph := pb{}.str(2, "g").
		bytes(5, pb{}.varint(1, 4).varint(2, 1).str(8, "w")).
		bytes(11, valueInfo("w", 4)).
		bytes(11, batch).
		bytes(12, valueInfo("y", 1, 10))
	path := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(path, pb{}.varint(1, 7).bytes(7, graph), 0o644); err != nil {
		t.Fatal(err)
	}
	dims, err := onnxInputDims(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{-1, 448, 448, 3}; !slices.Equal(dims, want) {
		t.Errorf("dims = %v, want %v", dims, want)
	}
}

func TestONNXInputDimsRefusesAFileThatIsNoModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(path, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dims, err := onnxInputDims(path); err == nil {
		t.Errorf("dims = %v, want an error", dims)
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
