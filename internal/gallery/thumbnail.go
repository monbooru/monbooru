package gallery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

const thumbMaxDim = 300
const thumbQuality = 85

// Browsers refuse a decoded bitmap past 2 GiB, 2^29 pixels at four bytes; the
// cap sits well under that for stricter engines and above any real photo.
// Past it the detail view serves a rendition of at most ViewMaxDim.
const (
	viewMaxPixels = 100_000_000
	ViewMaxDim    = 4000
)

var viewRenditionMu sync.Mutex

func viewRenditionPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d_view.jpg", imageID))
}

func NeedsViewRendition(width, height int) bool { return int64(width)*int64(height) > viewMaxPixels }

// EnsureViewRendition keeps the image's own size for maxDim 0. It is lazy:
// only an oversized image, or an AVIF or JPEG XL the browser cannot show,
// needs one.
func EnsureViewRendition(srcPath, dstDir string, imageID int64, fileType string, maxDim int) (string, error) {
	dst := viewRenditionPath(dstDir, imageID)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	// One build at a time: concurrent views render once, and two big
	// decodes never share the memory budget.
	viewRenditionMu.Lock()
	defer viewRenditionMu.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("create rendition dir: %w", err)
	}
	if IsFFmpegStill(fileType) {
		if err := renderStill(srcPath, dst, maxDim); err != nil {
			return "", err
		}
		return dst, nil
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	src, err := DecodeImageWithCap(f)
	if err != nil {
		return "", fmt.Errorf("decoding image: %w", err)
	}
	if maxDim > 0 {
		src = scaleImage(src, maxDim)
	}
	if err := writeJPEGAtomic(src, dst, thumbQuality); err != nil {
		return "", err
	}
	return dst, nil
}

// A header claiming 50000x50000 truecolor would ask for 10 GiB. Budgeted
// in bytes, not pixels, since a decoded pixel costs 1 to 8 bytes.
const maxImageBytes = 3 << 30

func decodedBytesPerPixel(m color.Model) int64 {
	switch m {
	case color.GrayModel:
		return 1
	case color.Gray16Model:
		return 2
	case color.YCbCrModel:
		return 3
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	}
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	return 4
}

// Below this, keeping a spent bitmap costs less than forcing a collection.
const largeDecodeBytes = 32 << 20

func decodedBytes(img image.Image) int64 {
	b := img.Bounds()
	return int64(b.Dx()) * int64(b.Dy()) * decodedBytesPerPixel(img.ColorModel())
}

// head is the header DecodeConfig read, which a progressive JPEG's
// coefficient buffers are sized from.
func decodeBudgetError(cfg image.Config, format string, head []byte) error {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("image is %dx%d, it has no pixels", cfg.Width, cfg.Height)
	}
	// The pixel check also catches a byte count that overflowed.
	pixels := int64(cfg.Width) * int64(cfg.Height)
	need := pixels * decodedBytesPerPixel(cfg.ColorModel)
	if format == "jpeg" {
		need += progressiveJPEGCoefficientBytes(head)
	}
	if pixels > maxImageBytes || need > maxImageBytes {
		return fmt.Errorf("image %dx%d exceeds the %d GiB decode cap", cfg.Width, cfg.Height, maxImageBytes>>30)
	}
	return nil
}

// Go's progressive decoder keeps a [64]int32 block per 8x8 of every
// component, on top of the image: up to 5x what the pixels cost.
func progressiveJPEGCoefficientBytes(head []byte) int64 {
	for i := 2; i+1 < len(head); {
		// The marker grammar image/jpeg reads: junk before an 0xFF and fill
		// bytes are skipped, and FF00 and RST0-7 carry no length.
		if head[i] != 0xFF || head[i+1] == 0xFF {
			i++
			continue
		}
		marker := head[i+1]
		if marker == 0x00 || marker >= 0xD0 && marker <= 0xD7 {
			i += 2
			continue
		}
		if i+4 > len(head) {
			return 0
		}
		size := int(head[i+2])<<8 | int(head[i+3])
		if size < 2 {
			return 0
		}
		switch {
		case marker == 0xC2:
			seg := head[i+4 : min(i+2+size, len(head))]
			if len(seg) < 6 {
				return 0
			}
			height, width, n := int(seg[1])<<8|int(seg[2]), int(seg[3])<<8|int(seg[4]), int(seg[5])
			if len(seg) < 6+3*n {
				return 0
			}
			// As image/jpeg sizes them: MCUs from the first component's
			// factors, and a lone component read in 8x8 blocks whatever its own.
			h0, v0, blocks := 1, 1, 1
			if n > 1 {
				h0, v0, blocks = int(seg[7]>>4), int(seg[7]&0x0F), 0
				for c := range n {
					blocks += int(seg[7+3*c]>>4) * int(seg[7+3*c]&0x0F)
				}
			}
			mcusX := (width + 8*h0 - 1) / (8 * h0)
			mcusY := (height + 8*v0 - 1) / (8 * v0)
			return int64(mcusX) * int64(mcusY) * int64(blocks) * 256
		case marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC:
			return 0
		}
		i += 2 + size
	}
	return 0
}

// PreviewRefusal explains a missing thumbnail, and is "" when nothing
// does: an unreadable or corrupt file gets no reason.
func PreviewRefusal(path, fileType string) string {
	switch {
	case fileType == "cbz":
		return mangaCoverRefusal(path)
	case IsFFmpegStill(fileType) || IsVideoType(fileType):
		return ffmpegRefusal()
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return budgetRefusal(f)
}

func mangaCoverRefusal(path string) string {
	m, err := OpenManga(path)
	if err != nil {
		return ""
	}
	defer func() { _ = m.Close() }()
	if m.ffmpegPage(0) {
		return ffmpegRefusal()
	}
	rc, err := m.pageReader(0)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	return budgetRefusal(rc)
}

func ffmpegRefusal() string {
	if !FFmpegAvailable() {
		return "needs ffmpeg"
	}
	return "ffmpeg could not decode it"
}

func budgetRefusal(r io.Reader) string {
	var head bytes.Buffer
	cfg, format, err := image.DecodeConfig(io.TeeReader(r, &head))
	if err != nil {
		return ""
	}
	if err := decodeBudgetError(cfg, format, head.Bytes()); err != nil {
		return err.Error()
	}
	return ""
}

// DecodeImageWithCap is image.Decode refusing a bitmap past
// maxImageBytes; it replays the header bytes, so r need not seek.
func DecodeImageWithCap(r io.Reader) (image.Image, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)
	cfg, format, err := image.DecodeConfig(tee)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := decodeBudgetError(cfg, format, buf.Bytes()); err != nil {
		return nil, err
	}
	if format == "webp" && webpAnimated(buf.Bytes()) {
		return decodeFirstWebPFrame(io.MultiReader(&buf, r), cfg)
	}
	img, _, err := image.Decode(io.MultiReader(&buf, r))
	if err == nil && img.Bounds().Empty() {
		return nil, fmt.Errorf("image decodes to %v, it has no pixels", img.Bounds())
	}
	return img, err
}

// The animation flag of the VP8X header.
func webpAnimated(head []byte) bool {
	return len(head) > 20 && string(head[12:16]) == "VP8X" && head[20]&0x02 != 0
}

// x/image/webp decodes stills only, and an animated file keeps its
// bitstreams in ANMF chunks: the first one is rebuilt as a still.
func decodeFirstWebPFrame(r io.Reader, cfg image.Config) (image.Image, error) {
	if _, err := io.CopyN(io.Discard, r, 12); err != nil {
		return nil, err
	}
	chunk := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, fmt.Errorf("webp: no animation frame: %w", err)
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		if string(chunk[:4]) != "ANMF" {
			// A payload of odd length is padded to an even one.
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return nil, err
			}
			continue
		}
		if size < 16 {
			return nil, errors.New("webp: short ANMF chunk")
		}
		frame, err := io.ReadAll(io.LimitReader(r, size))
		if err != nil {
			return nil, err
		}
		if int64(len(frame)) < size {
			return nil, io.ErrUnexpectedEOF
		}
		return decodeANMF(frame, cfg)
	}
}

func decodeANMF(frame []byte, cfg image.Config) (image.Image, error) {
	u24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	x, y := 2*u24(frame[0:]), 2*u24(frame[3:])
	w, h := u24(frame[6:])+1, u24(frame[9:])+1
	data := frame[16:]
	var still bytes.Buffer
	still.WriteString("RIFF\x00\x00\x00\x00WEBP")
	if len(data) >= 4 && string(data[:4]) == "ALPH" {
		// An alpha chunk is only read behind a VP8X header that names it.
		still.WriteString("VP8X\x0a\x00\x00\x00\x10\x00\x00\x00")
		still.Write([]byte{byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)})
	}
	still.Write(data)
	b := still.Bytes()
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	// The budget was sized for the canvas, not the frame's own bitstream.
	fc, err := webp.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if fc.Width > cfg.Width || fc.Height > cfg.Height {
		return nil, fmt.Errorf("webp: a %dx%d frame on a %dx%d canvas", fc.Width, fc.Height, cfg.Width, cfg.Height)
	}
	img, err := webp.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if x == 0 && y == 0 && img.Bounds().Dx() == cfg.Width && img.Bounds().Dy() == cfg.Height {
		return img, nil
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(canvas, image.Rect(x, y, x+w, y+h), img, img.Bounds().Min, draw.Src)
	return canvas, nil
}

func ThumbnailPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.jpg", imageID))
}

func hoverPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d_hover.webp", imageID))
}

func generate(srcPath, dstDir string, imageID int64, fileType string) error {
	return writeThumbnails(srcPath, dstDir, imageID, fileType, false)
}

// Rebuild writes a manga's page thumbs before it returns: queued, an archive
// past a full queue keeps its old ones.
func Rebuild(srcPath, dstDir string, imageID int64, fileType string) error {
	return writeThumbnails(srcPath, dstDir, imageID, fileType, true)
}

func writeThumbnails(srcPath, dstDir string, imageID int64, fileType string, pagesInline bool) error {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("creating thumbnail dir: %w", err)
	}
	// The display rendition would otherwise keep showing the old bytes
	// after a replace or rebuild.
	_ = os.Remove(viewRenditionPath(dstDir, imageID))

	dstPath := ThumbnailPath(dstDir, imageID)

	if IsVideoType(fileType) {
		offset := tenPercentOffset(srcPath)
		if err := generateVideoThumb(srcPath, dstPath, offset); err != nil {
			return err
		}
		hoverDst := hoverPath(dstDir, imageID)
		if err := generateVideoHover(srcPath, hoverDst, offset); err != nil {
			logx.Warnf("hover preview for %q: %v", srcPath, err)
		}
		return nil
	}
	if IsFFmpegStill(fileType) {
		return renderStill(srcPath, dstPath, thumbMaxDim)
	}
	if fileType == "cbz" {
		return generateMangaThumbnails(srcPath, dstDir, imageID, pagesInline)
	}
	if err := generateImageThumb(srcPath, dstPath); err != nil {
		return err
	}
	if fileType == "gif" {
		hoverDst := hoverPath(dstDir, imageID)
		if err := generateGIFHover(srcPath, hoverDst); err != nil {
			logx.Warnf("hover preview for %q: %v", srcPath, err)
		}
	}
	return nil
}

// The cover is the phash input, so it is written here; the pages can take
// minutes and go to the background workers unless pagesInline.
func generateMangaThumbnails(srcPath, dstDir string, imageID int64, pagesInline bool) error {
	archive, err := OpenManga(srcPath)
	if err != nil {
		return fmt.Errorf("open manga thumb: %w", err)
	}
	defer func() { _ = archive.Close() }()

	if err := generateOneMangaPageThumb(archive, 0, ThumbnailPath(dstDir, imageID)); err != nil {
		return fmt.Errorf("manga cover: %w", err)
	}

	imageDir := mangaImageDir(dstDir, imageID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("create manga thumb dir: %w", err)
	}
	if pagesInline {
		pregenerateMangaPageThumbs(srcPath, imageDir)
	} else {
		queueMangaPageThumbs(srcPath, imageDir)
	}
	return nil
}

// Well under the core count: background work must leave a modest host
// room for requests.
const mangaThumbWorkers = 2

type mangaThumbJob struct{ srcPath, imageDir string }

// Bounded; an archive that overflows it is skipped, and its pages are
// thumbnailed lazily on access.
var mangaThumbQueue = make(chan mangaThumbJob, 256)

var mangaThumbOnce sync.Once

func queueMangaPageThumbs(srcPath, imageDir string) {
	mangaThumbOnce.Do(func() {
		for i := 0; i < mangaThumbWorkers; i++ {
			go func() {
				for job := range mangaThumbQueue {
					pregenerateMangaPageThumbs(job.srcPath, job.imageDir)
					pageThumbsPending.Add(-1)
				}
			}()
		}
	})
	pageThumbsPending.Add(1)
	select {
	case mangaThumbQueue <- mangaThumbJob{srcPath: srcPath, imageDir: imageDir}:
	default:
		pageThumbsPending.Add(-1)
	}
}

var pageThumbsPending atomic.Int64

// WaitPageThumbs reports whether every queued page-thumbnail precompute,
// in any gallery, finished within d.
func WaitPageThumbs(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for pageThumbsPending.Load() > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// Reopens the archive so no handle is held while the job waits in the queue.
func pregenerateMangaPageThumbs(srcPath, imageDir string) {
	archive, err := OpenManga(srcPath)
	if err != nil {
		logx.Warnf("manga page thumbs for %q: %v", srcPath, err)
		return
	}
	defer func() { _ = archive.Close() }()

	for i := range archive.Pages {
		// The directory goes when the image is deleted or its bytes
		// change, possibly mid-loop.
		if _, err := os.Stat(imageDir); err != nil {
			return
		}
		pageNum := i + 1
		thumbPath := mangaPageThumbPath(imageDir, pageNum)
		if err := generateOneMangaPageThumb(archive, i, thumbPath); err != nil {
			logx.Warnf("manga page thumb %d for %q: %v", pageNum, srcPath, err)
		}
	}
}

// Straight from the archive, so the raw page cache stays lazy.
func generateOneMangaPageThumb(archive *Manga, idx int, dstPath string) error {
	if archive.ffmpegPage(idx) {
		return archive.withPageFile(idx, func(page string) error {
			return renderStill(page, dstPath, thumbMaxDim)
		})
	}
	rc, err := archive.pageReader(idx)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	src, err := DecodeImageWithCap(rc)
	if err != nil {
		return fmt.Errorf("decode page %d: %w", idx+1, err)
	}
	return writeJPEGAtomic(scaleImage(src, thumbMaxDim), dstPath, thumbQuality)
}

func generateImageThumb(srcPath, dstPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer func() { _ = f.Close() }()

	src, err := DecodeImageWithCap(f)
	if err != nil {
		return fmt.Errorf("decoding image: %w", err)
	}
	// Asked here, not below: reading src after the release would keep the
	// bitmap alive across it.
	large := decodedBytes(src) >= largeDecodeBytes

	thumb := scaleImage(src, thumbMaxDim)
	if err := writeJPEGAtomic(thumb, dstPath, thumbQuality); err != nil {
		return err
	}
	// A spent bitmap stays resident until the heap next reaches its goal,
	// so a sync over large files holds two of them at once.
	if large {
		debug.FreeOSMemory()
	}
	return nil
}

func scaleImage(src image.Image, maxDim int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	if w <= maxDim && h <= maxDim {
		return src
	}

	var nw, nh int
	if w >= h {
		nw = maxDim
		nh = h * maxDim / w
	} else {
		nh = maxDim
		nw = w * maxDim / h
	}
	nh, nw = max(nh, 1), max(nw, 1)

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func writeJPEGAtomic(img image.Image, path string, quality int) error {
	return fsx.WriteAtomic(path, ".thumb.*", func(f *os.File) error {
		if err := jpeg.Encode(f, img, &jpeg.Options{Quality: quality}); err != nil {
			return fmt.Errorf("encoding jpeg: %w", err)
		}
		return nil
	})
}

// The caller hands the returned phash to the in-memory index.
func regenerateDerived(database *db.DB, thumbnailsPath, path string, imageID int64, fileType, logCtx string) *int64 {
	if err := generate(path, thumbnailsPath, imageID, fileType); err != nil {
		logx.Warnf("%s: thumbnail for %q: %v", logCtx, path, err)
		return nil
	}
	h, err := RecomputeAndStorePhash(context.Background(), database, imageID, thumbnailsPath)
	if err != nil {
		logx.Warnf("%s: phash for %q: %v", logCtx, path, err)
		return nil
	}
	return &h
}
