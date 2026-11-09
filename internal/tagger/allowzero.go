//go:build tagger

// Temporary workaround for https://github.com/microsoft/onnxruntime/issues/33720:
// the DirectML EP never infers a Reshape's -1 when allowzero is set. Once the
// bundled onnxruntime carries the fix, delete this file, its test and its call in ensure.

package tagger

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/monbooru/monbooru/internal/shared/logx"
	ort "github.com/yalue/onnxruntime_go"
)

// newDirectMLSession clears allowzero on the Reshapes the EP would mis-size,
// which reads the same since their shape holds no 0, and builds the session
// from that copy; the weights still load from the model's folder.
func newDirectMLSession(path string, inputs, outputs []string, opts *ort.SessionOptions) (*ort.DynamicAdvancedSession, error) {
	offsets, err := allowZeroOffsetsIn(path)
	if err != nil || len(offsets) == 0 {
		return ort.NewDynamicAdvancedSession(path, inputs, outputs, opts)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, off := range offsets {
		data[off] = 0
	}
	if err := opts.AddSessionConfigEntry("session.model_external_initializers_file_folder_path", filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("ort session config: %w", err)
	}
	logx.Infof("tagger: %s: allowzero cleared on %d Reshape nodes for directml", filepath.Base(filepath.Dir(path)), len(offsets))
	return ort.NewDynamicAdvancedSessionWithONNXData(data, inputs, outputs, opts)
}

func allowZeroOffsetsIn(path string) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return allowZeroOffsets(f, fi.Size())
}

// allowZeroOffsets returns the byte offset of each top-level Reshape's
// allowzero=1 value whose shape is an inline int64 constant holding a -1 and
// no 0. The graph is walked in place so weights are skipped, not read.
func allowZeroOffsets(r io.ReaderAt, size int64) ([]int64, error) {
	type reshape struct {
		shape string
		off   int64
	}
	var reshapes []reshape
	shapes := map[string][]int64{}
	err := eachField(r, 0, size, func(m pbField) error {
		if m.num != 7 || m.wire != 2 { // ModelProto.graph
			return nil
		}
		return eachField(r, m.start, m.end, func(g pbField) error {
			if g.wire != 2 {
				return nil
			}
			switch g.num {
			case 1: // GraphProto.node
				shape, off, err := reshapeAllowZero(r, g)
				if err == nil && off >= 0 {
					reshapes = append(reshapes, reshape{shape, off})
				}
				return err
			case 5: // GraphProto.initializer
				name, vals, err := int64Constant(r, g)
				if err == nil && vals != nil {
					shapes[name] = vals
				}
				return err
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	var offsets []int64
	for _, rs := range reshapes {
		vals, ok := shapes[rs.shape]
		if ok && slices.Contains(vals, -1) && !slices.Contains(vals, 0) {
			offsets = append(offsets, rs.off)
		}
	}
	return offsets, nil
}

// reshapeAllowZero returns a Reshape node's shape input and the offset of its
// one-byte allowzero=1 value, or -1 when the node is anything else.
func reshapeAllowZero(r io.ReaderAt, node pbField) (string, int64, error) {
	var inputs []string
	var opType, domain string
	off := int64(-1)
	err := eachField(r, node.start, node.end, func(f pbField) error {
		if f.wire != 2 {
			return nil
		}
		var err error
		switch f.num {
		case 1:
			var in string
			in, err = readString(r, f)
			inputs = append(inputs, in)
		case 4:
			opType, err = readString(r, f)
		case 7:
			domain, err = readString(r, f)
		case 5:
			var name string
			var value pbField
			err = eachField(r, f.start, f.end, func(a pbField) error {
				var err error
				switch {
				case a.num == 1 && a.wire == 2:
					name, err = readString(r, a)
				case a.num == 3 && a.wire == 0:
					value = a
				}
				return err
			})
			if name == "allowzero" && value.val == 1 && value.end-value.start == 1 {
				off = value.start
			}
		}
		return err
	})
	if err != nil || opType != "Reshape" || (domain != "" && domain != "ai.onnx") || len(inputs) < 2 {
		return "", -1, err
	}
	return inputs[1], off, nil
}

// int64Constant returns an initializer's name and, for a small inline 1-D
// int64 tensor, its values.
func int64Constant(r io.ReaderAt, t pbField) (string, []int64, error) {
	const int64Type = 7
	var name string
	var dims, ints []int64
	var dataType, location uint64
	var raw, packed *pbField
	err := eachField(r, t.start, t.end, func(f pbField) error {
		switch {
		case f.num == 1 && f.wire == 0:
			dims = append(dims, int64(f.val))
		case f.num == 1 && f.wire == 2:
			b, err := readPayload(r, f)
			if err != nil {
				return err
			}
			vals, err := unpackVarints(b)
			dims = append(dims, vals...)
			return err
		case f.num == 2 && f.wire == 0:
			dataType = f.val
		case f.num == 7 && f.wire == 0:
			ints = append(ints, int64(f.val))
		case f.num == 7 && f.wire == 2:
			packed = &f
		case f.num == 8 && f.wire == 2:
			var err error
			name, err = readString(r, f)
			return err
		case f.num == 9 && f.wire == 2:
			raw = &f
		case f.num == 14 && f.wire == 0:
			location = f.val
		}
		return nil
	})
	if err != nil || dataType != int64Type || location != 0 || len(dims) != 1 || dims[0] < 1 || dims[0] > 16 {
		return name, nil, err
	}
	switch {
	case raw != nil:
		if raw.end-raw.start != 8*dims[0] {
			return name, nil, nil
		}
		b, err := readPayload(r, *raw)
		if err != nil {
			return name, nil, err
		}
		ints = make([]int64, dims[0])
		for i := range ints {
			ints[i] = int64(binary.LittleEndian.Uint64(b[8*i:]))
		}
	case packed != nil:
		b, err := readPayload(r, *packed)
		if err != nil {
			return name, nil, err
		}
		if ints, err = unpackVarints(b); err != nil {
			return name, nil, err
		}
	}
	if int64(len(ints)) != dims[0] {
		return name, nil, nil
	}
	return name, ints, nil
}

// pbField is one protobuf field: a varint's value, or the byte range of a
// length-delimited payload (for a varint, the range of the value itself).
type pbField struct {
	num, wire  int
	val        uint64
	start, end int64
}

var errBadProto = errors.New("malformed onnx protobuf")

func eachField(r io.ReaderAt, start, end int64, fn func(pbField) error) error {
	for off := start; off < end; {
		key, next, err := readVarint(r, off)
		if err != nil {
			return err
		}
		f := pbField{num: int(key >> 3), wire: int(key & 7), start: next}
		switch f.wire {
		case 0:
			f.val, f.end, err = readVarint(r, next)
		case 1:
			f.end = next + 8
		case 2:
			var n uint64
			n, f.start, err = readVarint(r, next)
			f.end = f.start + int64(n)
		case 5:
			f.end = next + 4
		default:
			return errBadProto
		}
		if err != nil {
			return err
		}
		if f.end > end || f.end < f.start {
			return errBadProto
		}
		if err := fn(f); err != nil {
			return err
		}
		off = f.end
	}
	return nil
}

func readVarint(r io.ReaderAt, off int64) (uint64, int64, error) {
	var buf [binary.MaxVarintLen64]byte
	n, _ := r.ReadAt(buf[:], off)
	v, k := binary.Uvarint(buf[:n])
	if k <= 0 {
		return 0, 0, errBadProto
	}
	return v, off + int64(k), nil
}

func readPayload(r io.ReaderAt, f pbField) ([]byte, error) {
	b := make([]byte, f.end-f.start)
	if n, err := r.ReadAt(b, f.start); n < len(b) {
		return nil, err
	}
	return b, nil
}

func readString(r io.ReaderAt, f pbField) (string, error) {
	b, err := readPayload(r, f)
	return string(b), err
}

func unpackVarints(b []byte) ([]int64, error) {
	var vals []int64
	for rd := bytes.NewReader(b); rd.Len() > 0; {
		v, err := binary.ReadUvarint(rd)
		if err != nil {
			return nil, errBadProto
		}
		vals = append(vals, int64(v))
	}
	return vals, nil
}
