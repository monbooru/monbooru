//go:build tagger

package tagger

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
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

func int64Tensor(name string, vals []int64, packed bool) pb {
	t := pb{}.varint(1, int64(len(vals))).varint(2, 7).str(8, name)
	if packed {
		var p []byte
		for _, v := range vals {
			p = binary.AppendUvarint(p, uint64(v))
		}
		return t.bytes(7, p)
	}
	raw := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint64(raw[8*i:], uint64(v))
	}
	return t.bytes(9, raw)
}

func externalTensor(name string, dataType, dim, offset, length int64) pb {
	entry := func(k, v string) pb { return pb{}.str(1, k).str(2, v) }
	return pb{}.varint(1, dim).varint(2, dataType).str(8, name).
		bytes(13, entry("location", "model.onnx.data")).
		bytes(13, entry("offset", strconv.FormatInt(offset, 10))).
		bytes(13, entry("length", strconv.FormatInt(length, 10))).
		varint(14, 1)
}

func reshapeModel(shape pb, allowZero int64) []byte {
	reshape := pb{}.str(1, "x").str(1, "shape").str(2, "y").str(4, "Reshape").
		bytes(5, pb{}.str(1, "allowzero").varint(3, allowZero).varint(20, 2))
	add := pb{}.str(1, "y").str(1, "bias").str(2, "z").str(4, "Add")
	graph := pb{}.bytes(1, reshape).bytes(1, add).str(2, "g").
		bytes(5, shape).bytes(5, externalTensor("bias", 1, 4, 0, 16)).
		bytes(11, valueInfo("x", 2, 3, 4)).bytes(12, valueInfo("z", 6, 4))
	return pb{}.varint(1, 10).bytes(8, pb{}.varint(2, 20)).bytes(7, graph)
}

func TestAllowZeroOffsetsPickReshapesWithAMinusOneAndNoZero(t *testing.T) {
	cases := []struct {
		name      string
		shape     pb
		allowZero int64
		want      int
	}{
		{"raw -1", int64Tensor("shape", []int64{-1, 4}, false), 1, 1},
		{"packed -1", int64Tensor("shape", []int64{-1, 4}, true), 1, 1},
		{"a 0 beside the -1", int64Tensor("shape", []int64{2, 0, -1}, false), 1, 0},
		{"no -1", int64Tensor("shape", []int64{6, 4}, false), 1, 0},
		{"allowzero off", int64Tensor("shape", []int64{-1, 4}, false), 0, 0},
		{"shape stored outside the file", externalTensor("shape", 7, 2, 16, 16), 1, 0},
	}
	for _, c := range cases {
		model := reshapeModel(c.shape, c.allowZero)
		offsets, err := allowZeroOffsets(bytes.NewReader(model), int64(len(model)))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(offsets) != c.want {
			t.Errorf("%s: %d offsets, want %d", c.name, len(offsets), c.want)
		}
		for _, off := range offsets {
			if model[off] != 1 {
				t.Errorf("%s: offset %d holds %d, want the allowzero value 1", c.name, off, model[off])
			}
		}
	}
}

func TestDirectMLSessionRunsTheClearedGraphWithItsExternalWeights(t *testing.T) {
	requireORT(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "model.onnx")
	mustWriteFile(t, path, reshapeModel(int64Tensor("shape", []int64{-1, 4}, false), 1), 0o644)
	bias := []float32{10, 20, 30, 40}
	weights := make([]byte, 16)
	for i, v := range bias {
		binary.LittleEndian.PutUint32(weights[4*i:], math.Float32bits(v))
	}
	mustWriteFile(t, filepath.Join(dir, "model.onnx.data"), weights, 0o644)
	if offsets, err := allowZeroOffsetsIn(path); err != nil || len(offsets) != 1 {
		t.Fatalf("allowZeroOffsetsIn = %v, %v; want one offset", offsets, err)
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		t.Skipf("ort init: %v", err)
	}
	defer func() { _ = ort.DestroyEnvironment() }()
	opts, err := ort.NewSessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opts.Destroy() }()
	session, err := newDirectMLSession(path, []string{"x"}, []string{"z"}, opts)
	if err != nil {
		t.Fatalf("newDirectMLSession: %v", err)
	}
	defer func() { _ = session.Destroy() }()

	in := make([]float32, 24)
	for i := range in {
		in[i] = float32(i)
	}
	x, err := ort.NewTensor(ort.NewShape(2, 3, 4), in)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = x.Destroy() }()
	z, err := ort.NewEmptyTensor[float32](ort.NewShape(6, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Destroy() }()
	if err := session.Run([]ort.Value{x}, []ort.Value{z}); err != nil {
		t.Fatalf("run: %v", err)
	}
	for i, got := range z.GetData() {
		if want := in[i] + bias[i%4]; got != want {
			t.Fatalf("z[%d] = %v, want %v", i, got, want)
		}
	}
}

func TestPixaiGraphHas94ReshapesToClearForDirectML(t *testing.T) {
	path := filepath.Join(modelsDir(t), "pixai-v1", "model.onnx")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("pixai-v1 model not present: %v", err)
	}
	offsets, err := allowZeroOffsetsIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 94 {
		t.Errorf("%d Reshapes to clear in pixai-v1, want 94", len(offsets))
	}
}
