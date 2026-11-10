package gallery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHashFile(t *testing.T) {
	t.Parallel()
	content := []byte("hello monbooru")
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	mustWriteFile(t, path, content, 0644)

	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])

	got, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantHex {
		t.Errorf("hash = %q, want %q", got, wantHex)
	}
}

func TestDetectFileType_Extensions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		wantType string
	}{
		{"image.jpg", "jpeg"},
		{"image.jpeg", "jpeg"},
		{"IMAGE.JPG", "jpeg"},
		{"image.png", "png"},
		{"image.webp", "webp"},
		{"image.avif", "avif"},
		{"image.JXL", "jxl"},
		{"image.gif", "gif"},
		{"video.mp4", "mp4"},
		{"video.webm", "webm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DetectFileType(tt.name)
			if err != nil {
				t.Fatalf("DetectFileType error: %v", err)
			}
			if got != tt.wantType {
				t.Errorf("DetectFileType(%q) = %q, want %q", tt.name, got, tt.wantType)
			}
		})
	}
}

func TestDetectFileType_Unknown(t *testing.T) {
	_, err := DetectFileType("file.xyz")
	if err != ErrUnsupportedType {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestDetectMagic(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		buf        []byte
	}{
		{"png", "png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}},
		{"jpeg", "jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"gif", "gif", []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0, 0, 0, 0, 0, 0}},
		{"webp", "webp", []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50}},

		{"mp4", "mp4", []byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 'm', 'p', '4', '2', 0, 0, 0, 0}},

		{"webm", "webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x00, 0x00, 0x00, 0, 0, 0, 0}},
		{"avif", "avif", []byte{0x00, 0x00, 0x00, 0x1C, 0x66, 0x74, 0x79, 0x70, 'a', 'v', 'i', 'f', 0, 0, 0, 0}},

		{"jxl", "jxl", []byte{0xFF, 0x0A, 0xFA, 0x18, 0x1C, 0x00, 0x14, 0x48, 0, 0, 0, 0}},
		{"jxl", "jxl", []byte{0x00, 0x00, 0x00, 0x0C, 'J', 'X', 'L', ' ', 0x0D, 0x0A, 0x87, 0x0A}},
		{"RejectsMOVHeader", "", []byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 'q', 't', ' ', ' ', 0, 0, 0, 0}},
		{"AVIFBrands/avis", "avif", []byte{0x00, 0x00, 0x00, 0x1C, 0x66, 0x74, 0x79, 0x70, 'a', 'v', 'i', 's', 0, 0, 0, 0}},
		{"AVIFBrands/mif1", "avif", []byte{0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, 'm', 'i', 'f', '1', 0, 0, 0, 0, 'm', 'i', 'a', 'f', 'a', 'v', 'i', 'f'}},
		{"RejectsHEICHeader", "", []byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 'h', 'e', 'i', 'c', 0, 0, 0, 0}},
		{"MP4CompatibleBrand", "mp4", []byte{
			0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, 'i', 's', 'o', '5', 0x00, 0x00, 0x02, 0x00,
			'i', 's', 'o', '6', 'm', 'p', '4', '1',
		}},
		{"RejectsMP4WithNoServableBrand", "", []byte{
			0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, '3', 'g', 'p', '4', 0x00, 0x00, 0x03, 0x00,
			'3', 'g', 'p', '4', '3', 'g', 'p', '5',
		}},
		{"MP4BrandScanStopsAtBoxEnd", "", []byte{
			0x00, 0x00, 0x00, 0x10, 0x66, 0x74, 0x79, 0x70, 'q', 't', ' ', ' ', 0x00, 0x00, 0x00, 0x00,
			'm', 'p', '4', '1',
		}},
		{"Unknown", "", []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0, 0, 0, 0}},
		{"TooShort", "", []byte{0xFF, 0xD8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := detectMagic(tc.buf)
			if tc.want == "" {
				if err != ErrUnsupportedType {
					t.Errorf("got %q, %v; want ErrUnsupportedType", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHashFileDigests(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, content := range [][]byte{[]byte("hello monbooru"), {}} {
		path := filepath.Join(dir, fmt.Sprintf("f%d.bin", len(content)))
		mustWriteFile(t, path, content, 0644)
		sha, sum, err := hashFileDigests(path)
		if err != nil {
			t.Fatal(err)
		}
		wantSHA, _ := HashFile(path)
		wantMD5, _ := Md5File(path)
		if sha != wantSHA || sum != wantMD5 {
			t.Errorf("hashFileDigests(%d bytes) = %q/%q, want %q/%q", len(content), sha, sum, wantSHA, wantMD5)
		}
	}
}

func TestHashFileDigests_NonExistent(t *testing.T) {
	if _, _, err := hashFileDigests("/nonexistent/path/file.bin"); err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestHashFile_NonExistent(t *testing.T) {
	_, err := HashFile("/nonexistent/path/file.bin")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestDetectFileType_MagicFallback(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/noext"
	mustWriteFile(t, path, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, 0644)
	got, err := DetectFileType(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "jpeg" {
		t.Errorf("magic fallback: got %q, want jpeg", got)
	}
}

func TestIsVideoType(t *testing.T) {
	if !IsVideoType("mp4") {
		t.Error("mp4 should be video")
	}
	if !IsVideoType("webm") {
		t.Error("webm should be video")
	}
	if IsVideoType("png") {
		t.Error("png should not be video")
	}
}

func mustCreateUnique(t *testing.T, dir, filename string) string {
	t.Helper()
	got, err := CreateUnique(dir, filename)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCreateUnique_NoCollision(t *testing.T) {
	dir := t.TempDir()
	got := mustCreateUnique(t, dir, "fresh.png")
	want := filepath.Join(dir, "fresh.png")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("the name was not reserved: %v", err)
	}
}

func TestCreateUnique_SuffixesOnCollision(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"image.png", "image_1.png", "image_2.png"} {
		mustWriteFile(t, filepath.Join(dir, name), []byte(name), 0o644)
	}
	got := mustCreateUnique(t, dir, "image.png")
	want := filepath.Join(dir, "image_3.png")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "image.png")); err != nil || string(b) != "image.png" {
		t.Errorf("the taken name was touched (err=%v)", err)
	}
}

func TestCreateUnique_TwoReservationsOfOneNameDiffer(t *testing.T) {
	dir := t.TempDir()
	first, second := mustCreateUnique(t, dir, "a.png"), mustCreateUnique(t, dir, "a.png")
	if first == second {
		t.Errorf("both reservations got %q", first)
	}
}

func TestCreateUnique_PreservesExtension(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "shot.tar.gz"), nil, 0o644)
	got := mustCreateUnique(t, dir, "shot.tar.gz")
	want := filepath.Join(dir, "shot.tar_1.gz")
	if got != want {
		t.Errorf("got %q, want %q (only the last extension is preserved)", got, want)
	}
}

func TestCreateUnique_ReturnsTheErrorOfANameTheFilesystemRefuses(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", 5000) + ".png"

	done := make(chan error, 1)
	go func() {
		_, err := CreateUnique(dir, long)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a name the filesystem refuses was reserved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CreateUnique did not return: the numbering loop has no exit")
	}
}

func TestResolveSubdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []struct {
		folder string
		want   string
		errIs  string
	}{
		{folder: "", want: ""},
		{folder: "photos", want: "photos"},
		{folder: "photos/2024", want: "photos/2024"},
		{folder: "/photos", errIs: "must be relative"},
		{folder: "photos/", want: "photos"},
		{folder: "photos//2024", want: "photos/2024"},
		{folder: "./photos", want: "photos"},
		{folder: "ok/../ok2", want: "ok2"},
		{folder: "a/b/../../c", want: "c"},
		{folder: "..", errIs: "escapes the gallery root"},
		{folder: "../escape", errIs: "escapes the gallery root"},
		{folder: "a/../../escape", errIs: "escapes the gallery root"},
		{folder: "/abs/path", errIs: "must be relative"},
	}
	for _, tc := range cases {
		got, err := resolveSubdir(root, tc.folder)
		if tc.errIs != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errIs) {
				t.Errorf("ResolveSubdir(%q) err = %v, want one carrying %q", tc.folder, err, tc.errIs)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveSubdir(%q) unexpected error: %v", tc.folder, err)
			continue
		}
		want := root
		if tc.want != "" {
			want = filepath.Join(root, filepath.FromSlash(tc.want))
		}
		if got != want {
			t.Errorf("ResolveSubdir(%q) = %q, want %q", tc.folder, got, want)
		}
	}
}

func TestPathInside(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/data/gallery", "/data/gallery", true},
		{"/data/gallery", "/data/gallery/photo.png", true},
		{"/data/gallery", "/data/gallery/sub/photo.png", true},
		{"/data/gallery", "/data/gallery_backup/leak.png", false},
		{"/data/gallery", "/data/gallery/../escape.png", false},
		{"/data/gallery", "/etc/passwd", false},
		{"/data/gallery", "/data/galleryX/photo.png", false},

		{"/data/gallery", "/data/gallery/foo/../bar.png", true},

		{"/data/gallery", "/data/gallery/..drafts/photo.png", true},
		{"/data/gallery", "/data/gallery/...", true},
		{"/data/gallery", "/data/gallery/..", false},
	}
	for _, tc := range cases {

		root := filepath.Clean(tc.root)
		target := filepath.Clean(tc.target)
		if got := PathInside(root, target); got != tc.want {
			t.Errorf("PathInside(%q, %q) = %v, want %v", root, target, got, tc.want)
		}
	}
}

func TestNamedInside_AFileBehindALinkedFolderIsInside(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "photos")
	mustMkdirAll(t, elsewhere, 0o755)
	mustWriteFile(t, filepath.Join(elsewhere, "a.png"), []byte("a"), 0o644)
	mustSymlink(t, elsewhere, filepath.Join(root, "linked"))

	if !NamedInside(root, filepath.Join(root, "linked", "a.png")) {
		t.Error("a file the gallery reaches through a link is inside it")
	}
	if storedInside(root, filepath.Join(root, "linked", "a.png")) {
		t.Error("the same file is not stored inside it - the two checks must not agree here")
	}
}

func TestNamedInside_RefusesWhatEscapesTheRoot(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/data/gallery", "/data/gallery/photo.png", true},
		{"/data/gallery", "/data/gallery", true},
		{"/data/gallery", "/data/gallery_backup/leak.png", false},
		{"/data/gallery", "/data/gallery/../escape.png", false},
		{"/data/gallery", "/etc/passwd", false},
	}
	for _, tc := range cases {
		if got := NamedInside(tc.root, tc.target); got != tc.want {
			t.Errorf("NamedInside(%q, %q) = %v, want %v", tc.root, tc.target, got, tc.want)
		}
	}
}

func TestNamedInside_TakesARootThatIsNotAbsoluteYet(t *testing.T) {
	root := t.TempDir()
	t.Chdir(filepath.Dir(root))
	target := filepath.Join(root, "a.png")

	if !NamedInside(filepath.Base(root), target) {
		t.Error("a relative root names the same directory and must answer the same")
	}
	if PathInside(filepath.Base(root), target) {
		t.Error("PathInside compares the two as given; that is what NamedInside is for")
	}
}

func TestMIMEForFileType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fileType string
		want     string
	}{
		{"jpeg", "image/jpeg"},
		{"png", "image/png"},
		{"webp", "image/webp"},
		{"avif", "image/avif"},
		{"jxl", "image/jxl"},
		{"gif", "image/gif"},
		{"mp4", "video/mp4"},
		{"webm", "video/webm"},
		{"cbz", "application/zip"},
		{"", ""},
		{"heic", ""},
	}
	for _, tt := range tests {
		t.Run(tt.fileType, func(t *testing.T) {
			t.Parallel()
			if got := MIMEForFileType(tt.fileType); got != tt.want {
				t.Errorf("MIMEForFileType(%q) = %q, want %q", tt.fileType, got, tt.want)
			}
		})
	}
}

func TestContentDispositionFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want string
	}{
		{"cbz", "/gallery/vol 1.cbz", `inline; filename="vol 1.cbz"`},
		{"token", "/gallery/plain.png", "inline; filename=plain.png"},
		{"non_ascii", "/gallery/café.png", "inline; filename*=utf-8''caf%C3%A9.png"},
		{"quote", `/gallery/a".png`, `inline; filename="a\".png"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ContentDispositionFor(tt.path); got != tt.want {
				t.Errorf("ContentDispositionFor(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
