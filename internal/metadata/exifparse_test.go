package metadata

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func tiffBlock(order binary.ByteOrder, entries [][]byte, next uint32, trailer []byte) []byte {
	var b bytes.Buffer
	if order == binary.BigEndian {
		b.WriteString("MM")
	} else {
		b.WriteString("II")
	}
	_ = binary.Write(&b, order, uint16(42))
	_ = binary.Write(&b, order, uint32(8))
	_ = binary.Write(&b, order, uint16(len(entries)))
	for _, e := range entries {
		b.Write(e)
	}
	_ = binary.Write(&b, order, next)
	b.Write(trailer)
	return b.Bytes()
}

func entryBytes(order binary.ByteOrder, id, typ uint16, count uint32, inline []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, order, id)
	_ = binary.Write(&b, order, typ)
	_ = binary.Write(&b, order, count)
	padded := make([]byte, 4)
	copy(padded, inline)
	b.Write(padded)
	return b.Bytes()
}

func TestDecodeEXIF_CyclicChainTerminates(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	b.WriteString("II")
	_ = binary.Write(&b, binary.LittleEndian, uint16(42))
	_ = binary.Write(&b, binary.LittleEndian, uint32(8))

	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	_ = binary.Write(&b, binary.LittleEndian, uint32(14))
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	_ = binary.Write(&b, binary.LittleEndian, uint32(8))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = decodeEXIF(b.Bytes())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a cyclic IFD chain must not loop forever")
	}
}

func TestDecodeEXIF_CorruptSubIFDKeepsIFD0(t *testing.T) {
	t.Parallel()
	order := binary.LittleEndian
	entries := [][]byte{

		entryBytes(order, 0x010E, exifASCII, 4, []byte("abc\x00")),

		entryBytes(order, 0x8769, exifLong, 1, []byte{3, 0, 0, 0}),
	}
	x, err := decodeEXIF(tiffBlock(order, entries, 0, nil))
	if err != nil {
		t.Fatalf("a corrupt sub-IFD must not fail the block: %v", err)
	}
	tag, ok := x.tags["ImageDescription"]
	if !ok {
		t.Fatal("IFD0's tag must survive a corrupt sub-IFD pointer")
	}
	if got := tag.String(); got != `"abc"` {
		t.Errorf("ImageDescription = %s", got)
	}
}

func TestDecodeEXIF_UnknownTypeRefused(t *testing.T) {
	t.Parallel()
	order := binary.LittleEndian
	entries := [][]byte{entryBytes(order, 0x010E, 13, 1, []byte{1, 0, 0, 0})}
	if _, err := decodeEXIF(tiffBlock(order, entries, 0, nil)); err == nil {
		t.Error("an entry with an unknown value type must not decode")
	}
}

func TestDecodeEXIF_OutOfBoundsValueRefused(t *testing.T) {
	t.Parallel()
	order := binary.LittleEndian

	off := make([]byte, 4)
	binary.LittleEndian.PutUint32(off, 4096)
	entries := [][]byte{entryBytes(order, 0x010E, exifASCII, 64, off)}
	if _, err := decodeEXIF(tiffBlock(order, entries, 0, nil)); err == nil {
		t.Error("a value addressed outside the block must not decode")
	}
}

func TestDecodeEXIF_WrappingCountRefused(t *testing.T) {
	t.Parallel()
	order := binary.LittleEndian

	entries := [][]byte{entryBytes(order, 0x010E, exifLong, 0x40000001, []byte{0, 0, 0, 0})}
	if _, err := decodeEXIF(tiffBlock(order, entries, 0, nil)); err == nil {
		t.Error("a count that wraps a 32-bit length must not decode")
	}
}

func TestExifTag_Render(t *testing.T) {
	t.Parallel()
	order := binary.LittleEndian
	cases := []struct {
		name    string
		id, typ uint16
		count   uint32
		inline  []byte
		trailer []byte
		want    string
	}{
		{"ascii", 0x010E, exifASCII, 4, []byte("abc\x00"), nil, `"abc"`},
		{"short_single", 0x0112, exifShort, 1, []byte{6, 0, 0, 0}, nil, "6"},
		{"short_pair", 0x0112, exifShort, 2, []byte{6, 0, 7, 0}, nil, "[6,7]"},
		{"undefined", 0x9000, exifUndefined, 4, []byte{'1', '2', 0x00, '3'}, nil, `"123"`},
		{"signed_short", 0x0112, exifSShort, 1, []byte{0xFF, 0xFF, 0, 0}, nil, "-1"},
		{"ascii_utf8", 0x010E, exifASCII, 4, []byte("\xC3\xA0b\x00"), nil, "\"\u00e0b\""},
		{"xp_utf16le", 0x9C9B, exifByte, 4, []byte{'h', 0, 'i', 0}, nil, `"hi"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries := [][]byte{entryBytes(order, tc.id, tc.typ, tc.count, tc.inline)}
			x, err := decodeEXIF(tiffBlock(order, entries, 0, tc.trailer))
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, tag := range x.tags {
				got = tag.String()
			}
			if got != tc.want {
				t.Errorf("rendered %s, want %s", got, tc.want)
			}
		})
	}
}

func TestExifTag_RenderRational(t *testing.T) {
	t.Parallel()
	order := binary.LittleEndian

	off := make([]byte, 4)
	binary.LittleEndian.PutUint32(off, 26)
	var trailer bytes.Buffer
	_ = binary.Write(&trailer, order, uint32(3))
	_ = binary.Write(&trailer, order, uint32(2))

	entries := [][]byte{entryBytes(order, 0x011A, exifRational, 1, off)}
	x, err := decodeEXIF(tiffBlock(order, entries, 0, trailer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tag, ok := x.tags["XResolution"]
	if !ok {
		t.Fatal("XResolution must decode")
	}
	if got := tag.String(); got != `"3/2"` {
		t.Errorf("XResolution = %s, want \"3/2\"", got)
	}
}

func TestDecodeEXIF_BigEndian(t *testing.T) {
	t.Parallel()
	order := binary.BigEndian
	entries := [][]byte{entryBytes(order, 0x0112, exifShort, 1, []byte{0, 6, 0, 0})}
	x, err := decodeEXIF(tiffBlock(order, entries, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	tag, ok := x.tags["Orientation"]
	if !ok {
		t.Fatal("Orientation must decode from a big-endian block")
	}
	if got := tag.String(); got != "6" {
		t.Errorf("Orientation = %s, want 6", got)
	}
}

func TestExifTag_IntAtDecodesOneElement(t *testing.T) {
	tag := &exifTag{typ: exifShort, count: 1 << 20, val: make([]byte, 2<<20), order: binary.LittleEndian}
	binary.LittleEndian.PutUint16(tag.val[6:], 7)
	if v, ok := tag.intAt(3); !ok || v != 7 {
		t.Fatalf("intAt(3) = %d, %v; want 7", v, ok)
	}
	if _, ok := tag.intAt(1 << 20); ok {
		t.Error("intAt past the count answered")
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = tag.intAt(3) }); n != 0 {
		t.Errorf("intAt allocates %v times per call, want none", n)
	}
}

func TestQuotePrintable_DropsUnprintableAndInvalidUTF8(t *testing.T) {
	t.Parallel()
	if got := quotePrintable([]byte("ab\x00\x07c")); got != `"abc"` {
		t.Errorf("control bytes must be dropped, got %s", got)
	}
	if got := quotePrintable([]byte("\u00e0 \u6f22\x00")); got != "\"\u00e0 \u6f22\"" {
		t.Errorf("printable UTF-8 must be kept, got %s", got)
	}

	if got := quotePrintable([]byte{'a', 0xE9}); got != `""` {
		t.Errorf("a result that is not valid UTF-8 must render empty, got %s", got)
	}
}
