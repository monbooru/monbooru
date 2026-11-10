package web

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/gallery"
)

func TestSanitizeCollectionFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "My Set", "My Set"},
		{"kept punctuation", "vol.2 - extras_final", "vol.2 - extras_final"},
		{"non-latin kept", "東方Project", "東方Project"},
		{"cyrillic kept", "Сказка", "Сказка"},
		{"path separators", "a/b\\c", "a_b_c"},
		{"windows reserved", `a<b>c:d"e|f?g*h`, "a_b_c_d_e_f_g_h"},
		{"control characters", "a\tb\nc", "a_b_c"},
		{"trailing dots and spaces", "  ..name..  ", "name"},
		{"traversal is not a path", "../../etc/passwd", "_.._etc_passwd"},
		{"empty falls back", "", "collection"},
		{"dots only falls back", "...", "collection"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeCollectionFilename(tt.in); got != tt.want {
				t.Errorf("sanitizeCollectionFilename(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeCollectionFilenameNeverEscapes(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "/abs/path", `C:\Windows`, "..", ".", "a/../../b"} {
		got := sanitizeCollectionFilename(in)
		if got != filepath.Base(got) || got == "." || got == ".." {
			t.Errorf("sanitizeCollectionFilename(%q) = %q, want a single harmless component", in, got)
		}
	}
}

func TestSanitizeCollectionFilenameTruncation(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("a", 400),
		strings.Repeat("東", 400),
		strings.Repeat("ab東", 200),
	} {
		got := sanitizeCollectionFilename(in)
		if len(got) > maxCBZStemBytes {
			t.Errorf("stem is %d bytes, want at most %d", len(got), maxCBZStemBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("stem for %d-byte input is not valid UTF-8: %q", len(in), got)
		}
	}
}

func TestCollectionCBZFilename(t *testing.T) {
	got := collectionCBZFilename("My Set")
	want := regexp.MustCompile(`^\d{8}-\d{6}-My Set\.cbz$`)
	if !want.MatchString(got) {
		t.Errorf("collectionCBZFilename = %q, want YYYYMMDD-HHMMSS-<label>.cbz", got)
	}
}

func TestCollectionCBZResultSummary(t *testing.T) {
	tests := []struct {
		name string
		res  collectionCBZResult
		want string
	}{
		{
			"created",
			collectionCBZResult{Pages: 5, Filename: "20260808-133736-Set.cbz"},
			"Generated 5 images as 20260808-133736-Set.cbz.",
		},
		{
			"created with skips",
			collectionCBZResult{Pages: 4, Skipped: 1, Filename: "a.cbz"},
			"Generated 4 images as a.cbz. 1 member skipped (missing files).",
		},
		{
			"folded onto an existing archive",
			collectionCBZResult{Pages: 5, Filename: "b.cbz", Existing: "a.cbz"},
			"These 5 pages are already packed as a.cbz; nothing new was created.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.res.summary(); got != tt.want {
				t.Errorf("summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShortHash(t *testing.T) {
	if got := shortHash("0123456789abcdef"); got != "01234567" {
		t.Errorf("shortHash = %q, want the first 8 chars", got)
	}
	if got := shortHash("abc"); got != "abc" {
		t.Errorf("shortHash of a short input = %q, want it unchanged", got)
	}
	if got := shortHash(""); got != "" {
		t.Errorf("shortHash of an empty hash = %q, want empty", got)
	}
}

func TestRunCollectionCBZ_FeedsABuiltBKTree(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.active()
	for i, name := range []string{"a.png", "b.png"} {
		mustAddCollection(t, cx.DB, seedImage(t, srv, name, 16+i, 16), "set", nil)
	}
	tree := builtTree(t, srv)

	if _, err := srv.runCollectionCBZ(context.Background(), cx, "set", "", gallery.Naming{}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	var archiveID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE file_type = 'cbz'`).Scan(&archiveID); err != nil {
		t.Fatalf("generated archive row: %v", err)
	}
	if !treeHolds(t, srv, tree, archiveID) {
		t.Error("a generated archive is missing from the built BK-tree")
	}
}
