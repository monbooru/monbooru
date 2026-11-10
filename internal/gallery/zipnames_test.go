package gallery

import (
	"strings"
	"testing"
)

func zipNames(z *ZipNamer, paths ...string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = z.Name(p)
	}
	return out
}

func assertNames(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("names = %q, want %q", got, want)
	}
}

func TestZipNamer_KeepsNamesAndNumbersClashesApart(t *testing.T) {
	got := zipNames(NewZipNamer("tags.json"),
		"/g/2024/07/IMG_0412.JPG",
		"/g/2024/07/IMG_0413.jpg",
		"/g/phone/IMG_0413.jpg",
		"/g/phone/copy/IMG_0413.jpg",
		"/g/scans/img_0412.jpg",
		"/g/odd/Tags.json",
	)
	assertNames(t, got, []string{
		"IMG_0412.JPG", "IMG_0413.jpg", "IMG_0413_1.jpg", "IMG_0413_2.jpg", "img_0412_1.jpg", "Tags_1.json",
	})
}

func TestZipNamer_FoldsWhatWindowsRefuses(t *testing.T) {
	got := zipNames(NewZipNamer(), "/g/edits/sunset:v2.png", "/g/why?.png", "/g/dots.png.")
	assertNames(t, got, []string{"sunset_v2.png", "why_.png", "dots.png"})
}

func TestZipNamer_CutsALongNameAndKeepsItsExtension(t *testing.T) {
	stem := strings.Repeat("é", 120)
	got := NewZipNamer().Name("/g/" + stem + ".jpg")
	if !strings.HasSuffix(got, "é.jpg") || len(got) > maxNameBytes+len(".jpg") {
		t.Errorf("long name = %q (%d bytes)", got, len(got))
	}
}
