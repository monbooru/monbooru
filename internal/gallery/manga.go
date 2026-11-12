package gallery

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

const mangaPageCacheTTL = 5 * time.Minute

// Shorter than the TTL, so an idle page goes within one tick of its deadline.
const mangaReclaimInterval = 60 * time.Second

func MangaCacheDir(thumbnailsPath string) string {
	if thumbnailsPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(thumbnailsPath), "manga")
}

func mangaImageDir(thumbnailsPath string, imageID int64) string {
	return filepath.Join(MangaCacheDir(thumbnailsPath), fmt.Sprintf("%d", imageID))
}

func mangaPagePath(imageDir string, n int, ext string) string {
	ext = cmp.Or(ext, ".bin")
	return filepath.Join(imageDir, fmt.Sprintf("page_%04d%s", n, ext))
}

func mangaPageThumbPath(imageDir string, n int) string {
	return filepath.Join(imageDir, fmt.Sprintf("page_%04d_thumb.jpg", n))
}

func mangaPageViewPath(imageDir string, n int) string {
	return filepath.Join(imageDir, fmt.Sprintf("page_%04d_view.jpg", n))
}

// The first match wins: a page is only ever extracted under one extension.
func extractedPageInDir(imageDir string, n int) string {
	prefix := fmt.Sprintf("page_%04d", n)
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Extra digits mean another page: page_1000 is a prefix of page_10000.
		rest := name[len(prefix):]
		if rest == "" || rest[0] != '.' {
			continue
		}
		return filepath.Join(imageDir, name)
	}
	return ""
}

func touchCacheFile(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		logx.Debugf("manga: chtimes %q: %v", path, err)
	}
}

// EnsureMangaPage extracts page n, 1-based, into the cache on a miss.
func EnsureMangaPage(thumbnailsPath, canonPath string, imageID int64, n int) (string, error) {
	return ensureMangaPageInDir(mangaImageDir(thumbnailsPath, imageID), canonPath, n)
}

func EnsureMangaPageInCache(cacheRoot, canonPath string, imageID int64, n int) (string, error) {
	return ensureMangaPageInDir(filepath.Join(cacheRoot, fmt.Sprintf("%d", imageID)), canonPath, n)
}

func ensureMangaPageInDir(imageDir, canonPath string, n int) (string, error) {
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return "", fmt.Errorf("create manga cache dir: %w", err)
	}
	if existing := extractedPageInDir(imageDir, n); existing != "" {
		touchCacheFile(existing)
		return existing, nil
	}
	archive, err := OpenManga(canonPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = archive.Close() }()
	if n < 1 || n > len(archive.Pages) {
		return "", fmt.Errorf("page %d out of range [1,%d]", n, len(archive.Pages))
	}
	ext := archive.pageCacheExt(n - 1)
	dst := mangaPagePath(imageDir, n, ext)
	if err := archive.extractPage(n-1, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func EnsureMangaPageThumb(thumbnailsPath, canonPath string, imageID int64, n int) (string, error) {
	thumb := mangaPageThumbPath(mangaImageDir(thumbnailsPath, imageID), n)
	return ensureFromPage(thumbnailsPath, canonPath, imageID, n, thumb, generateImageThumbFromAny)
}

// EnsureMangaPageView is a full-size JPEG copy of page n for a browser
// without AVIF or JPEG XL; the idle reclaim evicts it with the page.
func EnsureMangaPageView(thumbnailsPath, canonPath string, imageID int64, n int) (string, error) {
	view := mangaPageViewPath(mangaImageDir(thumbnailsPath, imageID), n)
	return ensureFromPage(thumbnailsPath, canonPath, imageID, n, view, func(page, dst string) error {
		return renderStill(page, dst, 0)
	})
}

func ensureFromPage(thumbnailsPath, canonPath string, imageID int64, n int, dst string, render func(page, dst string) error) (string, error) {
	if _, err := os.Stat(dst); err == nil {
		touchCacheFile(dst)
		return dst, nil
	}
	pagePath, err := EnsureMangaPage(thumbnailsPath, canonPath, imageID, n)
	if err != nil {
		return "", err
	}
	if err := render(pagePath, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func generateImageThumbFromAny(srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("create thumb dir: %w", err)
	}
	fileType := models.ExtFileType(srcPath)
	if IsFFmpegStill(fileType) {
		return renderStill(srcPath, dstPath, thumbMaxDim)
	}
	return generateImageThumb(srcPath, dstPath, fileType)
}

// An empty path would put the dir relative to the working directory.
func removeMangaCache(thumbnailsPath string, imageID int64) {
	if thumbnailsPath == "" {
		return
	}
	dir := mangaImageDir(thumbnailsPath, imageID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		logx.Warnf("manga cache: remove %q: %v", dir, err)
	}
}

type MangaCacheReclaimer struct {
	dir string
	ttl time.Duration

	mu       sync.Mutex
	cancel   context.CancelFunc
	doneOnce sync.Once
	done     chan struct{}
}

// NewMangaCacheReclaimer's dir must be the gallery's MangaCacheDir.
func NewMangaCacheReclaimer(dir string) *MangaCacheReclaimer {
	return &MangaCacheReclaimer{dir: dir, ttl: mangaPageCacheTTL, done: make(chan struct{})}
}

func (r *MangaCacheReclaimer) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.run(cctx)
}

func (r *MangaCacheReclaimer) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-r.done
}

func (r *MangaCacheReclaimer) run(ctx context.Context) {
	defer r.doneOnce.Do(func() { close(r.done) })
	t := time.NewTicker(mangaReclaimInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweepOnce()
		}
	}
}

func (r *MangaCacheReclaimer) sweepOnce() {
	if r.dir == "" {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-r.ttl)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		idDir := filepath.Join(r.dir, e.Name())
		files, err := os.ReadDir(idDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasPrefix(name, "page_") {
				continue
			}
			// Page thumbnails are pregenerated at ingest; reclaiming one
			// sends the next page list back to lazy extraction.
			if strings.HasSuffix(name, "_thumb.jpg") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(idDir, name)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					logx.Debugf("manga reclaim: remove %q: %v", path, err)
				}
			}
		}
	}
}
