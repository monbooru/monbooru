package web

import (
	"mime"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func TestTheMediaTypesAreRegisteredFromTheFileTypeTable(t *testing.T) {
	for _, ft := range models.FileTypes {
		if ft.Kind == "archive" {
			continue
		}
		for _, ext := range ft.Exts {
			if got := mime.TypeByExtension(ext); got != ft.MIME {
				t.Errorf("TypeByExtension(%q) = %q, want %q", ext, got, ft.MIME)
			}
		}
	}
}
