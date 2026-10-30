package metadata

import (
	"bytes"
	"encoding/binary"
	"io"
)

var exifMagic = []byte("Exif\x00\x00")

// A RIFF chunk length is attacker-controlled, up to 4 GiB; real EXIF
// chunks are kilobytes.
const maxChunkBytes = 16 * 1024 * 1024

func readWebPEXIF(r io.Reader) []byte {
	header := make([]byte, 12)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return nil
	}
	for {
		chunk := make([]byte, 8)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil
		}
		chunkType := string(chunk[0:4])
		size := binary.LittleEndian.Uint32(chunk[4:8])
		if chunkType != "EXIF" || size > maxChunkBytes {
			toSkip := int64(size)
			if size%2 == 1 {
				// RIFF chunks are word-aligned; skip the pad byte.
				toSkip++
			}
			if _, err := io.CopyN(io.Discard, r, toSkip); err != nil {
				return nil
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil
		}
		// Some encoders prepend the JPEG APP1 header; decodeEXIF
		// wants the bare TIFF payload.
		return bytes.TrimPrefix(data, exifMagic)
	}
}
