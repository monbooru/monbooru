package api

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (h *Handler) serveImageFile(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok || !canonicalOnDisk(w, canonPath) {
		return
	}
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

func (h *Handler) serveImageStill(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok || !canonicalOnDisk(w, canonPath) {
		return
	}
	if gallery.IsVideoType(fileType) || fileType == models.FileTypeCBZ {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image is a video or an archive")
		return
	}
	if !gallery.IsFFmpegStill(fileType) {
		if ct := gallery.MIMEForFileType(fileType); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeFile(w, r, canonPath)
		return
	}
	frame, err := gallery.RenderStillFrame(canonPath, os.TempDir(), 0)
	if serverError(w, err) {
		return
	}
	defer func() { _ = os.Remove(frame) }()
	http.ServeFile(w, r, frame)
}

func containedCanonical(w http.ResponseWriter, g *library.Gallery, id int64) (canonPath, fileType string, ok bool) {
	err := g.DB.Read.QueryRow(
		`SELECT canonical_path, file_type FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &fileType)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !gallery.NamedInside(g.GalleryPath, canonPath) {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return "", "", false
	}
	if serverError(w, err) {
		return "", "", false
	}
	return canonPath, fileType, true
}

func canonicalOnDisk(w http.ResponseWriter, canonPath string) bool {
	if _, err := os.Stat(canonPath); err != nil {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image file not found")
		return false
	}
	return true
}

func (h *Handler) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}
	thumbPath := filepath.Join(g.ThumbnailsPath, strconv.FormatInt(id, 10)+".jpg")
	if _, err := os.Stat(thumbPath); err != nil {
		contract.WriteError(w, http.StatusNotFound, "not_found", "thumbnail not found")
		return
	}
	http.ServeFile(w, r, thumbPath)
}

func (h *Handler) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	h.serveMangaPagePath(w, r, gallery.EnsureMangaPage)
}

func (h *Handler) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	h.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb)
}

func (h *Handler) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid page number")
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok {
		return
	}
	if fileType != models.FileTypeCBZ {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image is not a manga archive")
		return
	}
	page, err := ensure(g.ThumbnailsPath, canonPath, id, n)
	if err != nil {
		logx.Warnf("api: page %d of image %d: %v", n, id, err)
		contract.WriteError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	http.ServeFile(w, r, page)
}
