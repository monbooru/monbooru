package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/webui"
	"github.com/monbooru/monbooru/internal/tagger"
)

// A non-zero imageID settles the run as that one image's.
func (s *Server) spawnAutoTagJob(ids []int64, selected []tagger.TaggerStatus, imageID int64) {
	cfg := s.cfgSnapshot()
	cx := s.active()
	peak := autotagMemoryMark()
	go func() {
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, cx.Handle, cfg, ids, selected, s.jobs, cfg.Tagger.ExecutionProvider)
		if imageID != 0 {
			if finished, _ := tagger.SettleImage(ctx, s.jobs, imageID, skipped, err); finished {
				peak(fmt.Sprintf("image #%d", imageID))
			}
			return
		}
		if finished, _ := tagger.Settle(ctx, s.jobs, "", "", len(ids), skipped, err); finished {
			peak(fmt.Sprintf("batch %d %s", len(ids), models.Plural(len(ids), "image", "images")))
		}
	}()
}

// autotagMemoryMark resets the peaks, so the func it returns logs the job's
// own peak over the RSS at the mark. Under the subprocess backend the job's
// memory is the worker's, logged beside the parent's.
func autotagMemoryMark() func(scope string) {
	resetPeakRSS("self")
	baseline := readVmRSS()
	workerPID, workerBase := "", uint64(0)
	if pid, ok := tagger.WorkerPID(); ok {
		workerPID = strconv.Itoa(pid)
		resetPeakRSS(workerPID)
		workerBase = readRSSOf(workerPID)
	}
	return func(scope string) {
		if baseline == 0 {
			return
		}
		var parts []string
		if peak := readPeakRSS("self"); peak > baseline {
			parts = append(parts, "+"+webui.HumanBytes(int64(peak-baseline)))
		}
		if pid, ok := tagger.WorkerPID(); ok {
			p, base := strconv.Itoa(pid), uint64(0)
			if p == workerPID {
				base = workerBase
			}
			if peak := readPeakRSS(p); peak > base {
				parts = append(parts, "worker +"+webui.HumanBytes(int64(peak-base)))
			}
		}
		if len(parts) > 0 {
			logx.Infof("autotag %s: peak RSS %s", scope, strings.Join(parts, ", "))
		}
	}
}

func (s *Server) uploadPost(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	maxFileSizeMB := cfg.Gallery.MaxFileSizeMB
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes*10+4096) // ten files at the cap, plus the other fields
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeInlineFlash(w, "err", "Upload too large or invalid.")
		return
	}
	cx, destDir, naming, ok := s.uploadTarget(w, r, cfg)
	if !ok {
		return
	}
	files := r.MultipartForm.File["files"]

	var totalBytes int64
	for _, fh := range files {
		totalBytes += fh.Size
	}
	logx.Infof("upload: ingesting %d %s (%s)", len(files), models.Plural(len(files), "file", "files"), webui.HumanBytes(totalBytes))

	var addedIDs []int64
	var dupeIDs []int64
	var refused, tooBig []string
	added, dupes, oversized := 0, 0, 0
	unsupported, ignored, unsaved, noPreview, abandoned := 0, 0, 0, 0, 0
	for i, fh := range files {
		// The body limit only bounds the total, so one file can still be
		// over the cap.
		if maxBytes > 0 && fh.Size > maxBytes {
			oversized++
			tooBig = append(tooBig, fh.Filename)
			continue
		}
		landed, current, err := s.landUploadFile(r.Context(), cx, destDir, naming, fh)
		if !current {
			abandoned = len(files) - i
			break
		}
		if err != nil {
			switch library.LandStageOf(err) {
			case library.LandPlace:
				ignored++
			case library.LandType:
				unsupported++
			case library.LandIngest:
				logx.Warnf("upload ingest %q: %v", fh.Filename, err)
				if errors.Is(err, gallery.ErrUnsupportedType) {
					unsupported++
				} else {
					unsaved++
				}
			default:
				unsaved++
			}
			refused = append(refused, fh.Filename)
			continue
		}
		img := landed.Image
		if landed.Duplicate {
			dupeIDs = append(dupeIDs, img.ID)
			dupes++
			continue
		}
		// Ingest only logs a failed thumbnail, so the missing file is the
		// one signal.
		if _, statErr := os.Stat(gallery.ThumbnailPath(cx.ThumbnailsPath, img.ID)); statErr != nil {
			noPreview++
		}
		addedIDs = append(addedIDs, img.ID)
		added++
	}

	inboxOOB := ""
	s.set.RLock()
	if s.set.Get(cx.Name) == cx {
		if added > 0 {
			if err := cx.StampUploadBatch(addedIDs, time.Now().UnixNano()); err != nil {
				logx.Warnf("upload: stamp batch token: %v", err)
			}
		}
		inboxOOB = inboxNavOOB(r, cx)
	}
	s.set.RUnlock()

	// Assembled as HTML for the duplicate links: numbers are safe, and
	// every operator or file supplied string must be escaped.
	var msg strings.Builder
	fmt.Fprintf(&msg, "%d added", added)
	if dupes > 0 {
		fmt.Fprintf(&msg, ", %d duplicate%s", dupes, plural(dupes))
		if len(dupeIDs) > 0 {
			msg.WriteString(" (")
			for i, id := range dupeIDs {
				if i > 0 {
					msg.WriteString(", ")
				}
				fmt.Fprintf(&msg, `<a href="/images/%d">#%d</a>`, id, id)
			}
			msg.WriteString(")")
		}
	}
	if noPreview > 0 {
		fmt.Fprintf(&msg, ", %d without a preview", noPreview)
	}
	if oversized > 0 {
		fmt.Fprintf(&msg, ", %d skipped over %d MB%s", oversized, maxFileSizeMB, namedFiles(tooBig))
	}
	failed := unsupported + ignored + unsaved
	if failed > 0 {
		fmt.Fprintf(&msg, ", %d error%s: %s", failed, plural(failed), uploadErrorReasons(unsupported, ignored, unsaved, refused))
	}
	if abandoned > 0 {
		fmt.Fprintf(&msg, ", %d not saved: the gallery changed during the upload", abandoned)
		failed += abandoned
	}
	kind := "ok"
	switch {
	case added == 0 && (failed > 0 || oversized > 0):
		kind = "err"
	case failed > 0 || oversized > 0:
		kind = "warn"
	}
	writeInlineFlashHTML(w, kind, msg.String())
	_, _ = w.Write([]byte(inboxOOB))
}

// The body is read and parsed before the gallery lock, which a slow upload
// would otherwise hold with every request queued behind a gallery switch.
func (s *Server) uploadTarget(w http.ResponseWriter, r *http.Request, cfg *config.Config) (cx *library.Gallery, destDir string, naming gallery.Naming, ok bool) {
	s.set.RLock()
	defer s.set.RUnlock()
	if pageGalleryStale(w, r, s.activeGallery()) {
		return nil, "", naming, false
	}
	cx = s.active()
	if cx == nil || cx.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Upload unavailable: gallery path is unreadable.")
		return nil, "", naming, false
	}
	folderInput, naming := gallery.ReceivedNaming(cx.Name,
		strings.TrimSpace(r.FormValue("folder")),
		strings.TrimSpace(cfg.Gallery.DefaultUploadFolder),
		strings.TrimSpace(cfg.Gallery.DefaultUploadName))
	if len(r.MultipartForm.File["files"]) == 0 {
		writeInlineFlash(w, "err", "No files selected.")
		return nil, "", naming, false
	}
	destDir, err := cx.Boundary().ResolveSubdir(folderInput)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return nil, "", naming, false
	}
	return cx, destDir, naming, true
}

// The read lock is held for one file, so a gallery switch waits for at
// most one ingest; current is false once cx is no longer the member by its
// name (removed, repointed or replaced).
func (s *Server) landUploadFile(ctx context.Context, cx *library.Gallery, destDir string, naming gallery.Naming, fh *multipart.FileHeader) (landed library.LandResult, current bool, err error) {
	s.set.RLock()
	defer s.set.RUnlock()
	if s.set.Get(cx.Name) != cx {
		return landed, false, nil
	}
	file, err := fh.Open()
	if err != nil {
		return landed, true, err
	}
	defer func() { _ = file.Close() }()
	landed, err = cx.Land(ctx, destDir, gallery.SanitizeReceivedName(fh.Filename), library.FromReader(file),
		library.LandSpec{Origin: models.OriginUpload, Naming: naming, KeepOnFailure: true})
	return landed, true, err
}

const maxNamedRefusals = 3

func namedFiles(names []string) string {
	if len(names) == 0 || len(names) > maxNamedRefusals {
		return ""
	}
	return " (" + html.EscapeString(strings.Join(names, ", ")) + ")"
}

func uploadErrorReasons(unsupported, ignored, unsaved int, refused []string) string {
	reason := "could not be saved"
	var counted []string
	for _, r := range []struct {
		n             int
		bare, counted string
	}{
		{unsupported, "unsupported file type", "%d unsupported file type" + plural(unsupported)},
		{ignored, "on the ignore list", "%d on the ignore list"},
		{unsaved, "could not be saved", "%d could not be saved"},
	} {
		if r.n > 0 {
			reason = r.bare
			counted = append(counted, fmt.Sprintf(r.counted, r.n))
		}
	}
	if len(counted) > 1 {
		reason = strings.Join(counted, ", ")
	}
	return reason + namedFiles(refused)
}

func (s *Server) autotagTrigger(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	if !tagger.IsAvailable(cfg) {
		flashStatus(w, http.StatusServiceUnavailable, "Auto-tagger not available: "+tagger.UnavailableReason(cfg)+".")
		return
	}

	if !parseFormOK(w, r) {
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := tagger.SelectForGallery(cfg, s.activeGallery(), taggerName)
	if selErr != nil {
		flashStatus(w, http.StatusBadRequest, selErr.Error())
		return
	}

	ids, ok := s.resolveBatchScope(w, r, "autotag", false)
	if !ok {
		return
	}
	if len(ids) == 0 {
		flashStatus(w, http.StatusBadRequest, "No images to tag.")
		return
	}

	if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
		flashStatus(w, http.StatusConflict, "A job is already running.")
		return
	}
	s.jobs.Update(0, len(ids), "starting (loading model may take a few seconds)…")

	s.spawnAutoTagJob(ids, selected, 0)

	if isHTMXRequest(r) {
		setFlashHeader(w, fmt.Sprintf("Auto-tagger started for %d image%s.", len(ids), plural(len(ids))), "ok", nil)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) autotagImage(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgSnapshot()
	if !tagger.IsAvailable(cfg) {
		reason := tagger.UnavailableReason(cfg)
		hxErr(w, r, "Auto-tagger not available: "+reason+".", "auto-tagger not available: "+reason, http.StatusServiceUnavailable)
		return
	}

	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := tagger.SelectForGallery(cfg, s.activeGallery(), taggerName)
	if selErr != nil {
		externalErr(w, r, selErr.Error(), http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
		hxErr(w, r, "A job is already running.", "job already running", http.StatusConflict)
		return
	}
	s.jobs.Update(0, 1, "starting (loading model may take a few seconds)…")

	s.spawnAutoTagJob([]int64{id}, selected, id)

	if isHTMXRequest(r) {
		setFlashHeader(w, "Auto-tagger started for this image.", "ok", nil)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}
