package metadata

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

func decodeFileEXIF(path, fileType string) *exifData {
	if fileType != "jpeg" && fileType != "webp" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	return decodeReaderEXIF(f, fileType)
}

func decodeReaderEXIF(r io.Reader, fileType string) *exifData {
	var find func(io.Reader) []byte
	switch fileType {
	case "jpeg":
		find = jpegEXIF
	case "webp":
		find = readWebPEXIF
	default:
		return nil
	}
	x, _ := decodeEXIF(find(r))
	return x
}

// Segment lengths are uint16, so each read is bounded by the format itself.
func jpegEXIF(r io.Reader) []byte {
	br := bufio.NewReader(r)
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil || head[0] != 0xFF || head[1] != 0xD8 {
		return nil
	}
	for {
		marker, err := nextJPEGMarker(br)
		if err != nil {
			return nil
		}
		// EXIF sits in an APP segment before SOS (0xDA) and EOI (0xD9). TEM,
		// SOI and RSTn carry no length word, so they are skipped alone.
		switch {
		case marker == 0xDA || marker == 0xD9:
			return nil
		case marker == 0x01 || marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7):
			continue
		}
		lenBuf := make([]byte, 2)
		if _, err := io.ReadFull(br, lenBuf); err != nil {
			return nil
		}
		size := int(binary.BigEndian.Uint16(lenBuf))
		if size < 2 {
			return nil
		}
		payload := make([]byte, size-2)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil
		}
		if marker == 0xE1 && bytes.HasPrefix(payload, exifMagic) {
			return payload[len(exifMagic):]
		}
	}
}

// Tolerates 0xFF fill bytes and the 0xFF00 stuffing that stands for a
// literal 0xFF.
func nextJPEGMarker(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			continue
		}
		for b == 0xFF {
			if b, err = br.ReadByte(); err != nil {
				return 0, err
			}
		}
		if b != 0x00 {
			return b, nil
		}
	}
}
