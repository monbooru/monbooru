package web

import (
	"path/filepath"
	"testing"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
)

func seedMergeTarget(t *testing.T, srv *Server, gallery string) (int64, string, string) {
	t.Helper()
	cx := srv.get(gallery)
	if cx == nil {
		t.Fatalf("gallery %q missing", gallery)
	}
	pngBytes := makePNGBytes(t, 8, 8, 1, 2, 3)
	imgPath := filepath.Join(cx.GalleryPath, "seed.png")
	mustWriteFile(t, imgPath, pngBytes, 0o644)
	img, _, err := galleryIngest(srv, gallery, imgPath)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return img.ID, img.SHA256, imgPath
}

func galleryIngest(srv *Server, name, path string) (*models.Image, bool, error) {
	cx := srv.get(name)
	return gallery.Ingest(cx.Handle, path, models.OriginIngest)
}
