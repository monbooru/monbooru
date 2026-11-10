package gallery

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func createTestJPEG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(dir, name)
	f := mustCreate(t, path)
	defer func() { _ = f.Close() }()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGenerate_JPEG(t *testing.T) {
	dir := t.TempDir()
	srcPath := createTestJPEG(t, dir, "test.jpg", 600, 400)

	dstDir := filepath.Join(dir, "thumbs")
	if err := generate(srcPath, dstDir, 1, "jpeg"); err != nil {
		t.Fatal(err)
	}

	dstPath := ThumbnailPath(dstDir, 1)
	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("thumbnail not created: %v", err)
	}

	if w, h := imageSize(t, dstPath, "jpeg"); w > 300 || h > 300 {
		t.Errorf("thumbnail too large: %dx%d", w, h)
	}
}

func TestGenerate_PNG(t *testing.T) {
	dir := t.TempDir()
	srcPath := createTestPNGFileSize(t, dir, "test.png", 200, 200)

	dstDir := filepath.Join(dir, "thumbs")
	if err := generate(srcPath, dstDir, 2, "png"); err != nil {
		t.Fatal(err)
	}

	dstPath := ThumbnailPath(dstDir, 2)
	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("thumbnail not created: %v", err)
	}
}

func TestGenerate_SmallImage_NoUpscale(t *testing.T) {
	dir := t.TempDir()
	srcPath := createTestJPEG(t, dir, "small.jpg", 100, 80)

	dstDir := filepath.Join(dir, "thumbs")
	if err := generate(srcPath, dstDir, 3, "jpeg"); err != nil {
		t.Fatalf("generate: %v", err)
	}

	if w, h := imageSize(t, ThumbnailPath(dstDir, 3), "jpeg"); w != 100 || h != 80 {
		t.Errorf("small image dimensions changed: %dx%d (want 100x80)", w, h)
	}
}

func TestThumbnailPath(t *testing.T) {
	got := ThumbnailPath("/thumbs", 42)
	want := "/thumbs/42.jpg"
	if got != want {
		t.Errorf("ThumbnailPath = %q, want %q", got, want)
	}
}

func TestHoverPath(t *testing.T) {
	got := hoverPath("/thumbs", 42)
	want := "/thumbs/42_hover.webp"
	if got != want {
		t.Errorf("hoverPath = %q, want %q", got, want)
	}
}

func createTestGIF(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), palette.Plan9)
	path := filepath.Join(dir, name)
	f := mustCreate(t, path)
	defer func() { _ = f.Close() }()
	if err := gif.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGenerate_GIF(t *testing.T) {
	dir := t.TempDir()
	srcPath := createTestGIF(t, dir, "test.gif", 500, 200)

	dstDir := filepath.Join(dir, "thumbs")
	if err := generate(srcPath, dstDir, 1, "gif"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ThumbnailPath(dstDir, 1)); err != nil {
		t.Errorf("thumbnail not created: %v", err)
	}
}

func TestGenerate_GIF_HonoursDecodeCap(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "huge.gif")
	header := []byte("GIF89a\xff\xff\xff\xff\x80\x00\x00\x00\x00\x00\xff\xff\xff;")
	mustWriteFile(t, srcPath, header, 0o644)

	err := generate(srcPath, filepath.Join(dir, "thumbs"), 1, "gif")
	if err == nil {
		t.Fatal("a 65535x65535 GIF must be refused, not decoded")
	}
	if !strings.Contains(err.Error(), "decode cap") {
		t.Errorf("want the decode-cap refusal, got %v", err)
	}
}

func pngHeader(w, h int, bitDepth, colorType byte) []byte {
	ihdr := []byte("IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, bitDepth, colorType, 0, 0, 0)

	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(ihdr)-4))
	buf.Write(ihdr)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(ihdr))
	return buf.Bytes()
}

func TestDecodeImageWithCap_BudgetsBytesNotPixels(t *testing.T) {
	const w, h = 25954, 25910

	if _, err := DecodeImageWithCap(bytes.NewReader(pngHeader(w, h, 8, 0))); err != nil &&
		strings.Contains(err.Error(), "decode cap") {
		t.Errorf("greyscale %dx%d must clear the budget, got %v", w, h, err)
	}

	_, err := DecodeImageWithCap(bytes.NewReader(pngHeader(w, h, 16, 6)))
	if err == nil || !strings.Contains(err.Error(), "decode cap") {
		t.Errorf("16-bit RGBA %dx%d must be refused, got %v", w, h, err)
	}
}

func TestDecodedBytes_TracksTheColourModel(t *testing.T) {
	const w, h = 3000, 2000

	if got := decodedBytes(image.NewGray(image.Rect(0, 0, w, h))); got >= largeDecodeBytes {
		t.Errorf("greyscale %dx%d = %d bytes, must stay under the release threshold", w, h, got)
	}
	if got := decodedBytes(image.NewNRGBA64(image.Rect(0, 0, w, h))); got < largeDecodeBytes {
		t.Errorf("16-bit RGBA %dx%d = %d bytes, must reach the release threshold", w, h, got)
	}
}

const animatedWebP1x1 = "UklGRlIAAABXRUJQVlA4WAoAAAASAAAAAAAAAAAAQU5JTQYAAAD/////AABBTk1GJgAAAAAAAAAAAAAAAAAAAGQAAABWUDhMDQAAAC8AAAAQBxAREYiI/gcA"

func animatedWebPFrames(t *testing.T, frames int) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(animatedWebP1x1)
	if err != nil {
		t.Fatal(err)
	}
	anmf := slices.Clone(b[len(b)-46:])
	for range frames - 1 {
		b = append(b, anmf...)
	}
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

func TestDecodeImageWithCap_AnimatedWebP(t *testing.T) {
	for _, frames := range []int{1, 2} {
		img, err := DecodeImageWithCap(bytes.NewReader(animatedWebPFrames(t, frames)))
		if err != nil {
			t.Errorf("%d frame(s): %v", frames, err)
			continue
		}
		if img.Bounds() != image.Rect(0, 0, 1, 1) {
			t.Errorf("%d frame(s): bounds %v, want the 1x1 canvas", frames, img.Bounds())
		}
	}
}

func TestDecodeImageWithCap_AnimatedWebPPlacesItsFrame(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}

	dir := t.TempDir()
	red := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for i := range red.Pix {
		red.Pix[i] = []byte{255, 0, 0, 255}[i%4]
	}
	var src bytes.Buffer
	if err := jpeg.Encode(&src, red, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, "red.jpg"), src.Bytes(), 0o644)
	out := filepath.Join(dir, "red.webp")
	cmd := fixtureFFmpeg("-y", "-loglevel", "quiet", "-i", filepath.Join(dir, "red.jpg"),
		"-frames:v", "1", "-c:v", "libwebp", "-lossless", "1", "--", out)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg cannot encode libwebp: %v", err)
	}
	still := mustReadFile(t, out)
	vp8l := still[12:]

	var anim bytes.Buffer
	anim.WriteString("RIFF\x00\x00\x00\x00WEBP")
	anim.Write([]byte{'V', 'P', '8', 'X', 10, 0, 0, 0, 0x12, 0, 0, 0, 3, 0, 0, 3, 0, 0})
	anmf := []byte{'A', 'N', 'M', 'F', 0, 0, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 100, 0, 0, 0}
	binary.LittleEndian.PutUint32(anmf[4:], uint32(16+len(vp8l)))
	anim.Write(anmf)
	anim.Write(vp8l)
	b := anim.Bytes()
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))

	img, err := DecodeImageWithCap(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Fatalf("bounds %v, want the 4x4 canvas", img.Bounds())
	}
	if r, _, _, a := img.At(3, 3).RGBA(); a>>8 != 255 || r>>8 < 200 {
		t.Errorf("(3,3) = r%d a%d, want the frame's opaque red", r>>8, a>>8)
	}
	if _, _, _, a := img.At(1, 1).RGBA(); a != 0 {
		t.Errorf("(1,1) alpha = %d, want the canvas left transparent outside the frame", a>>8)
	}
}

func TestGenerate_AnimatedWebPThumbnailsFromItsFirstFrame(t *testing.T) {
	src := filepath.Join(t.TempDir(), "anim.webp")
	mustWriteFile(t, src, animatedWebPFrames(t, 2), 0o644)
	thumbs := t.TempDir()
	if err := generate(src, thumbs, 1, "webp"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if w, h := imageSize(t, ThumbnailPath(thumbs, 1), "jpeg"); w != 1 || h != 1 {
		t.Errorf("thumbnail %dx%d, want 1x1", w, h)
	}
}

func jpegHeader(sof byte, w, h int, factors ...byte) []byte {
	b := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
	b = append(b, 0xFF, sof, 0, byte(8+3*len(factors)), 8, byte(h>>8), byte(h), byte(w>>8), byte(w), byte(len(factors)))
	for i, hv := range factors {
		b = append(b, byte(i+1), hv, byte(min(i, 1)))
	}
	return b
}

func TestDecodeImageWithCap_CountsProgressiveCoefficients(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  []byte
		refused bool
	}{

		{"progressive 4:4:4", jpegHeader(0xC2, 30000, 30000, 0x11, 0x11, 0x11), true},

		{"progressive 4:2:0", jpegHeader(0xC2, 20000, 20000, 0x22, 0x11, 0x11), true},
		{"baseline 4:2:0", jpegHeader(0xC0, 20000, 20000, 0x22, 0x11, 0x11), false},
		{"progressive 4:4:4 behind a junk byte", behindAPP0(jpegHeader(0xC2, 30000, 30000, 0x11, 0x11, 0x11), 0x00), true},
		{"progressive 4:4:4 behind a stuffed zero", behindAPP0(jpegHeader(0xC2, 30000, 30000, 0x11, 0x11, 0x11), 0xFF, 0x00), true},
		{"progressive 4:4:4 behind a restart marker", behindAPP0(jpegHeader(0xC2, 30000, 30000, 0x11, 0x11, 0x11), 0xFF, 0xD0), true},
	} {
		_, err := DecodeImageWithCap(bytes.NewReader(tc.header))
		capped := err != nil && strings.Contains(err.Error(), "decode cap")
		if capped != tc.refused {
			t.Errorf("%s: err = %v, want refused by the cap: %v", tc.name, err, tc.refused)
		}
	}
}

func behindAPP0(header []byte, between ...byte) []byte {
	return slices.Concat(header[:20], between, header[20:])
}

func TestDecodeImageWithCap_SurvivesAFakeSOF2BehindARestartMarker(t *testing.T) {

	head := slices.Concat(jpegHeader(0xC0, 16, 16, 0x11)[:20],
		[]byte{0xFF, 0xD0, 0x00, 0x0A},
		[]byte{0xFF, 0xE1, 0x00, 0x0A, 0, 0, 0, 0, 0xFF, 0xC2, 0x00, 0x00},
		jpegHeader(0xC0, 16, 16, 0x11)[20:])
	if _, _, err := image.DecodeConfig(bytes.NewReader(head)); err != nil {
		t.Fatalf("the fixture is not a header image/jpeg accepts: %v", err)
	}
	if _, err := DecodeImageWithCap(bytes.NewReader(head)); err == nil {
		t.Error("a header with no image data decoded")
	}
}

func TestDecodeImageWithCap_AnimatedWebPWithAnOversizedFrameChunk(t *testing.T) {
	still := animatedWebPFrames(t, 1)
	anmf := len(still) - 46
	vp8l := anmf + 8 + 16 + 8
	wide := slices.Clone(still)
	dims := binary.LittleEndian.Uint32(wide[vp8l+1:])
	binary.LittleEndian.PutUint32(wide[vp8l+1:], dims&^0x0FFFFFFF|0x3FFF|0x3FFF<<14)
	for _, tc := range []struct {
		name string
		file []byte
	}{
		{"a chunk claiming 1 GiB", slices.Concat(still[:anmf], []byte("ANMF"), binary.LittleEndian.AppendUint32(nil, 1<<30), make([]byte, 24))},
		{"a 16384x16384 frame on a 1x1 canvas", wide},
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodeImageWithCap(bytes.NewReader(tc.file))
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("%s: decoded", tc.name)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
			t.Errorf("%s: allocated %d MiB", tc.name, grew>>20)
		}
	}
}

func TestPreviewRefusal_NamesAnOversizedProgressiveJPEG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.jpg")
	mustWriteFile(t, path, jpegHeader(0xC2, 20000, 20000, 0x22, 0x11, 0x11), 0o644)
	if got := PreviewRefusal(path, "jpeg"); !strings.Contains(got, "decode cap") {
		t.Errorf("PreviewRefusal = %q, want the decode cap named", got)
	}
}

func TestRebuildThumbnails_WritesEveryPageThumbBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	pages := map[string][]byte{}
	for i := 1; i <= 5; i++ {
		pages[fmt.Sprintf("%02d.png", i)] = solidPNG(t, 16, 16, [3]uint8{uint8(40 * i), 20, 20})
	}
	cbz := writeTestZip(t, dir, "book.cbz", pages)
	thumbs := filepath.Join(t.TempDir(), "thumbnails")

	for len(mangaThumbQueue) < cap(mangaThumbQueue) {
		queueMangaPageThumbs(filepath.Join(dir, "absent.cbz"), dir)
	}

	if err := Rebuild(cbz, thumbs, 7, "cbz"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	for n := 1; n <= 5; n++ {
		if _, err := os.Stat(mangaPageThumbPath(mangaImageDir(thumbs, 7), n)); err != nil {
			t.Errorf("page %d thumb missing when Rebuild returned: %v", n, err)
		}
	}
}

func gifScreen(w, h byte) []byte {
	b := []byte("GIF89a")
	b = append(b, w, 0, h, 0, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
	b = append(b, 0x2C, 0, 0, 0, 0, w, 0, h, 0, 0)
	return append(b, 2, 1, 0x2C, 0, 0x3B)
}

func noPixelImages(t *testing.T) map[string][]byte {
	t.Helper()
	jpegOf := func(w, h int) []byte {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	return map[string][]byte{
		"0x0.gif":  gifScreen(0, 0),
		"10x0.gif": gifScreen(10, 0),
		"0x0.jpg":  jpegOf(0, 0),
		"10x0.jpg": jpegOf(10, 0),
	}
}

func TestDecodeImageWithCap_RefusesAnImageWithNoPixels(t *testing.T) {
	for name, data := range noPixelImages(t) {
		if img, err := DecodeImageWithCap(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: decoded to %v", name, img.Bounds())
		}
	}
}

func TestPreviewRefusal_NamesAnImageWithNoPixels(t *testing.T) {
	dir := t.TempDir()
	for name, data := range noPixelImages(t) {
		path := filepath.Join(dir, name)
		mustWriteFile(t, path, data, 0o644)
		if got := PreviewRefusal(path, models.ExtFileType(name)); !strings.Contains(got, "no pixels") {
			t.Errorf("%s: PreviewRefusal = %q, want the empty bitmap named", name, got)
		}
	}
}

func TestIngest_AnImageWithNoPixelsGetsNoSizeThumbnailOrPhash(t *testing.T) {
	for name, data := range noPixelImages(t) {
		database, env, galleryDir := setupSyncTest(t)
		path := filepath.Join(galleryDir, name)
		mustWriteFile(t, path, data, 0o644)
		img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, path, models.OriginIngest)

		var w, h, phash sql.NullInt64
		if err := database.Read.QueryRow(`SELECT width, height, phash FROM images WHERE id = ?`, img.ID).Scan(&w, &h, &phash); err != nil {
			t.Fatal(err)
		}
		if w.Valid || h.Valid || phash.Valid {
			t.Errorf("%s: width %v, height %v, phash %v, want all NULL", name, w, h, phash)
		}
		if _, err := os.Stat(ThumbnailPath(env.thumbnailsPath, img.ID)); err == nil {
			t.Errorf("%s: a thumbnail was written for an image with no pixels", name)
		}
	}
}

func TestIngest_CBZWithANoPixelCoverGetsNoSize(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	cbz := writeTestZip(t, galleryDir, "vol.cbz", map[string][]byte{
		"01.gif": gifScreen(10, 0),
		"02.png": solidPNG(t, 16, 16, [3]uint8{0, 0, 0}),
	})
	img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, cbz, models.OriginIngest)
	if img.Width != nil || img.Height != nil {
		t.Error("a cover with no pixels gave the row a size")
	}
}
