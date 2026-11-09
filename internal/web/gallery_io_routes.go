package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) importGallery(name, format string, upload io.Reader) (int, error) {
	newCx, leftOut, err := s.replaceGalleryFromUpload(name, format, upload)
	if err != nil {
		return leftOut, err
	}
	logx.Infof("gallery: imported %q (format=%s)", name, format)

	// Before the rebuild starts: switchGallery refuses while a job runs.
	if err := s.switchGallery(name); err != nil {
		logx.Infof("gallery %q: post-import switch skipped: %v", name, err)
	}

	// The import wiped the thumbnails directory.
	if err := s.startRebuildThumbsJob(newCx); err != nil {
		logx.Infof("gallery %q: skipped post-import rebuild: %v", name, err)
	}
	return leftOut, nil
}

func leftOutNote(leftOut int) string {
	if leftOut == 0 {
		return ""
	}
	if leftOut == 1 {
		return " 1 file in the archive sits in a folder this gallery leaves out and was not extracted."
	}
	return fmt.Sprintf(" %d files in the archive sit in folders this gallery leaves out and were not extracted.", leftOut)
}

func (s *Server) replaceGalleryFromUpload(name, format string, upload io.Reader) (*library.Gallery, int, error) {
	var tmpPath string
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	var leftOut int
	newCx, err := s.set.Replace(name,
		// Next to the database so the rename into place stays on one filesystem.
		func(dataDir string) error {
			tmp, err := os.CreateTemp(dataDir, "import-*.upload")
			if err != nil {
				return fmt.Errorf("create temp: %w", err)
			}
			tmpPath = tmp.Name()
			_, err = io.Copy(tmp, upload)
			if closeErr := tmp.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fmt.Errorf("buffer upload: %w", err)
			}
			return nil
		},
		func(dbPath, thumbnailsPath string, bound *gallery.Boundary, quiesce func() error) error {
			var err error
			leftOut, err = galleryio.ApplyImport(format, tmpPath, dbPath, thumbnailsPath, bound, s.maxFileSizeMB(), quiesce)
			return err
		})
	return newCx, leftOut, err
}

func (s *Server) settingsGalleryExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	withImages := r.URL.Query().Get("with_images") == "true"

	if s.get(name) == nil {
		http.Error(w, "unknown gallery", http.StatusNotFound)
		return
	}
	switch format {
	case "db", "json", "light":
	default:
		http.Error(w, "format must be db, json, or light", http.StatusBadRequest)
		return
	}

	filename, contentType := exportFilename(name, format, withImages)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	var err error
	switch {
	case format == "light" && withImages:
		err = s.exportGallery(name, func(cx *library.Gallery) error { return galleryio.ExportGalleryLight(cx.Handle, w) })
	case format == "light":
		err = s.exportGallery(name, func(cx *library.Gallery) error { return galleryio.ExportGalleryLightManifest(cx.Handle, w) })
	case withImages:
		err = s.exportGallery(name, func(cx *library.Gallery) error { return galleryio.ExportGalleryArchive(cx.Handle, format, w) })
	case format == "db":
		err = s.exportGallery(name, func(cx *library.Gallery) error { return galleryio.ExportGalleryDB(cx.Handle, w) })
	case format == "json":
		err = s.exportGallery(name, func(cx *library.Gallery) error { return galleryio.ExportGalleryJSON(cx.Handle, w, nil) })
	}
	if err != nil {
		logx.Warnf("gallery export %q: %v", name, err)
		// The headers are out: a clean end would pass a truncated file
		// off as whole.
		panic(http.ErrAbortHandler)
	}
}

func exportFilename(name, format string, withImages bool) (string, string) {
	if format == "light" && withImages {
		return name + "-light.zip", "application/zip"
	}
	if format == "light" {
		return name + "-light.json", "application/json"
	}
	if withImages {
		return name + ".zip", "application/zip"
	}
	switch format {
	case "db":
		return name + ".db", "application/vnd.sqlite3"
	case "json":
		return name + ".json", "application/json"
	}
	return name, "application/octet-stream"
}

// The file part comes back unread, so an upload streams instead of spooling
// to a temp file.
func readFieldsToFile(mr *multipart.Reader) (map[string]string, *multipart.Part, error) {
	const maxFieldBytes = 1 << 20
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return fields, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if part.FileName() != "" {
			return fields, part, nil
		}
		body, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
		_ = part.Close()
		if err != nil {
			return nil, nil, err
		}
		fields[part.FormName()] = strings.TrimSpace(string(body))
	}
}

// Parts are read in order so the name check runs before the file is
// consumed: the form must put mode and confirm_name ahead of the file.
func (s *Server) settingsGalleryImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	r.Body = http.MaxBytesReader(w, r.Body, galleryio.MaxImportBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		writeInlineFlash(w, "err", "expected multipart/form-data")
		return
	}
	fields, filePart, err := readFieldsToFile(mr)
	if err != nil {
		writeInlineFlash(w, "err", "malformed upload")
		return
	}
	if filePart == nil {
		writeInlineFlash(w, "err", "missing file")
		return
	}
	defer func() { _ = filePart.Close() }()
	fileFilename := filePart.FileName()

	mode := cmp.Or(fields["mode"], "replace")
	if mode != "replace" && mode != "merge" {
		writeInlineFlash(w, "err", "mode must be replace or merge")
		return
	}
	if mode == "replace" {
		if fields["confirm_name"] != name {
			writeInlineFlash(w, "err", "type-to-confirm name does not match")
			return
		}
	}
	format := galleryio.FormatFromExt(fileFilename)
	if format == "" {
		writeInlineFlash(w, "err", "file must be .db, .json, or .zip")
		return
	}

	if mode == "merge" {
		res, err := s.mergeGallery(name, format, filePart)
		if err != nil {
			writeInlineFlash(w, "err", err.Error())
			return
		}
		writeInlineFlash(w, "ok", "Gallery "+name+" merged: "+res.Summary()+".")
		return
	}
	leftOut, err := s.importGallery(name, format, filePart)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	nowActive := ""
	if s.activeGallery() == name {
		nowActive = " and now active"
	}
	writeInlineFlash(w, "ok", "Gallery "+name+" imported"+nowActive+". Rebuilding thumbnails in the background."+leftOutNote(leftOut))
}

func (s *Server) batchTransfer(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	dstCx, removeAfter, ok := s.transferTarget(w, r)
	if !ok {
		return
	}
	s.startScopedJob(w, r, "batch-transfer", models.JobTypeTransfer, false, func(ids []int64) {
		s.runBatchTransfer(ids, dstCx, removeAfter)
	}, dstCx.Name)
}

func (s *Server) transferImage(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	dstCx, removeAfter, ok := s.transferTarget(w, r)
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeTransfer, dstCx.Name) {
		return
	}
	srcCx := s.active()
	srcCx.WaitIngests(s.jobs.Context())
	dstCx.WaitIngests(s.jobs.Context())
	kept, err := galleryio.TransferOneImage(srcCx, dstCx, id, removeAfter)
	if err != nil {
		s.jobs.Fail(err.Error())
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	msg := fmt.Sprintf("Transferred image to %s%s.", dstCx.Name, keptChanged(kept))
	s.jobs.Complete(msg)

	dest := fmt.Sprintf("/images/%d", id)
	if removeAfter {
		dest = "/"
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) transferTarget(w http.ResponseWriter, r *http.Request) (*library.Gallery, bool, bool) {
	target := strings.TrimSpace(r.FormValue("target"))
	msg := ""
	switch target {
	case "":
		msg = "Pick a target gallery."
	case s.activeGallery():
		msg = "The target must be a different gallery."
	}
	if msg == "" {
		if dst := s.get(target); dst == nil || dst.DB == nil {
			msg = "Unknown target gallery."
		} else if dst.Degraded {
			msg = "The target gallery is unavailable."
		} else {
			return dst, r.FormValue("remove_after") != "", true
		}
	}
	externalErr(w, r, msg, http.StatusBadRequest)
	return nil, false, false
}

func (s *Server) runBatchTransfer(ids []int64, dstCx *library.Gallery, removeAfter bool) {
	srcCx := s.active()
	dstCx.WaitIngests(s.jobs.Context())
	total := len(ids)
	keptFiles := 0
	transferred, failed, cancelled := s.perImageLoop(ids, "transfer", "transferring", func(_ int, id int64) error {
		kept, err := galleryio.TransferOneImage(srcCx, dstCx, id, removeAfter)
		keptFiles += kept
		return err
	})
	summary := fmt.Sprintf("Transferred %d image%s to %s", transferred, plural(transferred), dstCx.Name)
	if failed > 0 {
		summary += fmt.Sprintf(", %d failed", failed)
	}
	s.jobs.Finish(nil, cancelled, fmt.Sprintf("transfer cancelled (%d/%d done)", transferred, total), summary+keptChanged(keptFiles)+".")
}

// Streams without the set's lock, which would stall a queued switch and
// every request behind it; the pin is taken under the lock so nothing
// closes the gallery first.
func (s *Server) exportGallery(name string, write func(*library.Gallery) error) error {
	s.set.RLock()
	cx := s.get(name)
	if cx != nil {
		cx.BeginExport()
	}
	s.set.RUnlock()
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	defer cx.EndExport()
	return write(cx)
}

func (s *Server) mergeGallery(name, format string, upload io.Reader) (galleryio.MergeResult, error) {
	// A job, not just a check: rename, repoint and remove refuse while one
	// runs, and they would close the database mid-merge.
	if err := s.jobs.Start(models.JobTypeMerge, name); err != nil {
		return galleryio.MergeResult{}, library.ErrJobRunning
	}
	cx := s.get(name)
	if cx == nil {
		err := fmt.Errorf("unknown gallery %q", name)
		s.jobs.Fail(err.Error())
		return galleryio.MergeResult{}, err
	}
	cx.WaitIngests(s.jobs.Context())
	total := 0
	res, err := galleryio.MergeGallery(s.jobs.Context(), cx, format, upload, s.maxFileSizeMB(), func(done, n int) {
		total = n
		s.jobs.Update(done, n, "merging")
	})
	switch {
	case errors.Is(err, context.Canceled):
		done := res.Added + res.Tagged + res.Unchanged + res.Skipped + res.LeftOut
		err = fmt.Errorf("merge cancelled after %d of %d record%s: %s", done, total, plural(total), res.Summary())
		s.jobs.Complete(fmt.Sprintf("merge cancelled (%d/%d records)", done, total))
	case err != nil:
		s.jobs.Fail(err.Error())
	default:
		s.jobs.Complete("Gallery " + name + " merged: " + res.Summary() + ".")
	}
	return res, err
}
