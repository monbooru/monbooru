package tagger

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// onnxInputDims reads the dimensions of a model's first graph input that
// is not an initializer, as the runtime lists its inputs, without loading
// the weights: an unnamed dimension reads -1.
func onnxInputDims(path string) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var inputs []pbField
	initializers := map[string]bool{}
	err = eachField(f, 0, fi.Size(), func(m pbField) error {
		if m.num != 7 || m.wire != 2 { // ModelProto.graph
			return nil
		}
		return eachField(f, m.start, m.end, func(g pbField) error {
			if g.wire != 2 {
				return nil
			}
			switch g.num {
			case 5: // GraphProto.initializer
				return eachField(f, g.start, g.end, func(t pbField) error {
					if t.num != 8 || t.wire != 2 { // TensorProto.name
						return nil
					}
					name, err := readString(f, t)
					initializers[name] = true
					return err
				})
			case 11: // GraphProto.input
				inputs = append(inputs, g)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	for _, in := range inputs {
		name, dims, err := valueInfoDims(f, in)
		if err != nil {
			return nil, err
		}
		if !initializers[name] {
			return dims, nil
		}
	}
	return nil, errors.New("onnx model has no input")
}

// valueInfoDims walks ValueInfoProto.type.tensor_type.shape.dim.
func valueInfoDims(r io.ReaderAt, vi pbField) (string, []int64, error) {
	var name string
	var dims []int64
	sub := func(f pbField, num int, fn func(pbField) error) error {
		return eachField(r, f.start, f.end, func(g pbField) error {
			if g.num == num && g.wire == 2 {
				return fn(g)
			}
			return nil
		})
	}
	err := eachField(r, vi.start, vi.end, func(f pbField) error {
		var err error
		switch {
		case f.num == 1 && f.wire == 2:
			name, err = readString(r, f)
		case f.num == 2 && f.wire == 2:
			err = sub(f, 1, func(tensor pbField) error {
				return sub(tensor, 2, func(shape pbField) error {
					return sub(shape, 1, func(dim pbField) error {
						d := int64(-1)
						err := eachField(r, dim.start, dim.end, func(v pbField) error {
							if v.num == 1 && v.wire == 0 {
								d = int64(v.val)
							}
							return nil
						})
						dims = append(dims, d)
						return err
					})
				})
			})
		}
		return err
	})
	return name, dims, err
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
