package gallery

import (
	"archive/zip"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/fsx"
)

type MangaPage struct {
	Path         string
	OriginalName string
	file         *zip.File
}

type Manga struct {
	Pages []MangaPage
	zr    *zip.ReadCloser
}

var errEmptyManga = errors.New("archive contains no recognised image entries")

var pageImageExts = func() map[string]struct{} {
	exts := map[string]struct{}{}
	for _, ft := range models.FileTypes {
		if ft.PageImage {
			for _, e := range ft.Exts {
				exts[e] = struct{}{}
			}
		}
	}
	return exts
}()

var pageSkipBasenames = map[string]struct{}{
	".ds_store":   {},
	"thumbs.db":   {},
	"desktop.ini": {},
}

func OpenManga(path string) (*Manga, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open archive %q: %w", path, err)
	}
	pages := make([]MangaPage, 0, len(zr.File))
	for _, f := range zr.File {
		name := f.Name
		if strings.HasSuffix(name, "/") {
			continue
		}
		if strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		base := strings.ToLower(filepath.Base(name))
		if _, skip := pageSkipBasenames[base]; skip {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if _, ok := pageImageExts[ext]; !ok {
			continue
		}
		if f.UncompressedSize64 == 0 {
			continue
		}
		pages = append(pages, MangaPage{Path: name, OriginalName: filepath.Base(name), file: f})
	}
	sort.SliceStable(pages, func(i, j int) bool {
		return NaturalLess(strings.ToLower(pages[i].Path), strings.ToLower(pages[j].Path))
	})
	if len(pages) == 0 {
		_ = zr.Close()
		return nil, errEmptyManga
	}
	return &Manga{Pages: pages, zr: zr}, nil
}

func (m *Manga) Close() error {
	if m == nil || m.zr == nil {
		return nil
	}
	return m.zr.Close()
}

func (m *Manga) Reader() *zip.Reader {
	if m == nil || m.zr == nil {
		return nil
	}
	return &m.zr.Reader
}

func (m *Manga) pageReader(n int) (io.ReadCloser, error) {
	if n < 0 || n >= len(m.Pages) {
		return nil, fmt.Errorf("page %d out of range [1,%d]", n+1, len(m.Pages))
	}
	return m.Pages[n].file.Open()
}

func (m *Manga) pageOrientation(n int) int {
	rc, err := m.pageReader(n)
	if err != nil {
		return 1
	}
	defer func() { _ = rc.Close() }()
	o, _ := metadata.EXIFOrientationFrom(rc, models.ExtFileType(m.Pages[n].OriginalName))
	return o
}

// CoverOrientation is the EXIF orientation of a cbz's first page, 0 for none.
func CoverOrientation(path string) int {
	m, err := OpenManga(path)
	if err != nil {
		return 0
	}
	defer func() { _ = m.Close() }()
	return m.pageOrientation(0)
}

func (m *Manga) extractPage(n int, dst string) error {
	rc, err := m.pageReader(n)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	return fsx.WriteAtomic(dst, ".page.*", func(f *os.File) error {
		if _, err := io.Copy(f, rc); err != nil {
			return fmt.Errorf("write page %d: %w", n+1, err)
		}
		return nil
	})
}

func (m *Manga) coverDimensions() (int, int, error) {
	if m.ffmpegPage(0) {
		var w, h int
		err := m.withPageFile(0, func(page string) error {
			var ok bool
			if w, h, ok = ProbeVideoDimensions(page); !ok {
				return errors.New("ffprobe could not read the cover's size")
			}
			return nil
		})
		return w, h, err
	}
	rc, err := m.pageReader(0)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rc.Close() }()
	cfg, _, err := image.DecodeConfig(rc)
	if err != nil {
		return 0, 0, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, fmt.Errorf("the cover is %dx%d, it has no pixels", cfg.Width, cfg.Height)
	}
	return cfg.Width, cfg.Height, nil
}

func (m *Manga) ffmpegPage(n int) bool {
	return IsFFmpegStill(models.ExtFileType(m.Pages[n].OriginalName))
}

// ffmpeg reads a file, not the zip stream, so the page goes to a temp file.
func (m *Manga) withPageFile(n int, fn func(page string) error) error {
	tmp, err := os.CreateTemp("", ".manga-page.*"+m.pageCacheExt(n))
	if err != nil {
		return fmt.Errorf("creating temp page file: %w", err)
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := m.extractPage(n, tmp.Name()); err != nil {
		return err
	}
	return fn(tmp.Name())
}

func (m *Manga) pageCacheExt(n int) string {
	if n < 0 || n >= len(m.Pages) {
		return ""
	}
	return strings.ToLower(filepath.Ext(m.Pages[n].OriginalName))
}

// NaturalLess does not fold case; callers lowercase both sides.
func NaturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ai, aj := a[i], b[j]
		ad := ai >= '0' && ai <= '9'
		bd := aj >= '0' && aj <= '9'
		if ad && bd {
			as, bs := i, j
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			as2 := as
			for as2 < i && a[as2] == '0' {
				as2++
			}
			bs2 := bs
			for bs2 < j && b[bs2] == '0' {
				bs2++
			}
			la, lb := i-as2, j-bs2
			if la != lb {
				return la < lb
			}
			if cmp := strings.Compare(a[as2:i], b[bs2:j]); cmp != 0 {
				return cmp < 0
			}
			if i-as != j-bs {
				return i-as < j-bs
			}
			continue
		}
		if ai != aj {
			return ai < aj
		}
		i++
		j++
	}
	return len(a) < len(b)
}
