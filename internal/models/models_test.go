package models

import (
	"errors"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"slices"
	"testing"
	"time"
)

func TestSavedSearchHRef(t *testing.T) {
	tests := []struct {
		name string
		in   SavedSearch
		want string
	}{
		{"query only", SavedSearch{Query: "1girl"}, "/?q=1girl"},
		{"empty query", SavedSearch{}, "/?q="},
		{"escapes reserved runes", SavedSearch{Query: `cat:artist "a b"&c`}, "/?q=cat%3Aartist+%22a+b%22%26c"},
		{"full shape", SavedSearch{Query: "1girl", Sort: "random", Order: "asc", Seed: "42"},
			"/?q=1girl&sort=random&order=asc&seed=42"},
		{"sort without order", SavedSearch{Query: "a", Sort: "filesize"}, "/?q=a&sort=filesize"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.HRef(); got != tt.want {
				t.Errorf("HRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

type fakeRow struct {
	vals []any
	err  error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != len(f.vals) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = f.vals[i].(int64)
		case *string:
			*p = f.vals[i].(string)
		case *int:
			*p = f.vals[i].(int)
		case **int:
			*p, _ = f.vals[i].(*int)
		case **int64:
			*p, _ = f.vals[i].(*int64)
		case **float64:
			*p, _ = f.vals[i].(*float64)
		case **string:
			*p, _ = f.vals[i].(*string)
		default:
			return errors.New("unhandled dest type")
		}
	}
	return nil
}

func TestScanImageRow(t *testing.T) {
	w, h := 800, 600
	tagged := "2026-07-01T10:00:00Z"
	row := fakeRow{vals: []any{
		int64(7), "abc", "def", "/g/a.png", "sub", FileTypePNG,
		&w, &h, int64(1234), 1, 0,
		1, &tagged, SourceTypeNone, OriginIngest, "danbooru", "https://e/1", "note", "",
		(*int)(nil), (*int)(nil), (*float64)(nil), "vol1", (*int)(nil), (*int64)(nil), "2026-07-02T11:00:00Z", (*int64)(nil),
	}}
	img, err := ScanImageRow(row)
	if err != nil {
		t.Fatalf("ScanImageRow: %v", err)
	}
	if img.ID != 7 || img.SHA256 != "abc" || img.MD5 != "def" || img.FileType != FileTypePNG {
		t.Errorf("identity fields wrong: %+v", img)
	}
	if !img.IsMissing || img.IsFavorited || !img.IsInbox {
		t.Errorf("int-as-bool folding wrong: missing=%v fav=%v inbox=%v", img.IsMissing, img.IsFavorited, img.IsInbox)
	}
	if img.Width == nil || *img.Width != 800 {
		t.Errorf("Width = %v, want 800", img.Width)
	}
	if img.AutoTaggedAt == nil || !img.AutoTaggedAt.Equal(time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("AutoTaggedAt = %v", img.AutoTaggedAt)
	}
	if !img.IngestedAt.Equal(time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("IngestedAt = %v", img.IngestedAt)
	}
}

func TestScanImageRowError(t *testing.T) {
	want := errors.New("boom")
	img, err := ScanImageRow(fakeRow{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if img != (Image{}) {
		t.Errorf("expected the zero Image on error, got %+v", img)
	}
}

func TestMediaKind(t *testing.T) {
	for fileType, want := range map[string]string{
		FileTypeJPEG: "image", FileTypeAVIF: "image", FileTypeJXL: "image",
		FileTypeCBZ: "archive", FileTypeGIF: "animated", FileTypeMP4: "animated",
		"heic": "",
	} {
		if got := MediaKind(fileType); got != want {
			t.Errorf("MediaKind(%q) = %q, want %q", fileType, got, want)
		}
	}
}

func TestEveryFileTypeHasAMediaKind(t *testing.T) {
	for _, ft := range FileTypes {
		if !slices.Contains(MediaKinds, ft.Kind) || len(ft.Exts) == 0 || ft.MIME == "" {
			t.Errorf("file type %+v needs a MediaKinds kind, an extension and a MIME type", ft)
		}
	}
}

func TestFileTypesMatchIngestExtensions(t *testing.T) {
	var exts []string
	for _, ft := range FileTypes {
		exts = append(exts, ft.Exts...)
	}
	slices.Sort(exts)
	want := slices.Sorted(slices.Values(contract.IngestExtensions))
	if !slices.Equal(exts, want) {
		t.Errorf("FileTypes extensions = %v, contract.IngestExtensions = %v", exts, want)
	}
}

func TestExtFileType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{"a.jpg", "jpeg"},
		{"a.JPEG", "jpeg"},
		{"a.png", "png"},
		{"a.avif", "avif"},
		{"a.JXL", "jxl"},
		{"a.zip", "cbz"},
		{"a.cbz", "cbz"},
		{"a.heic", ""},
		{"noext", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := ExtFileType(tt.path); got != tt.want {
				t.Errorf("ExtFileType(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
