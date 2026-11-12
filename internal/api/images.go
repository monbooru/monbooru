package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/markup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

func linkParentRelations(g *library.Gallery, imageID int64, url, parentURL string) {
	if g.RelationsSvc == nil {
		return
	}
	if parentURL != "" {
		if parentID, ok := gallery.ImageIDBySourceURL(g.DB, parentURL); ok && parentID != imageID {
			linkFirstSource(g, parentID, imageID)
		}
	}
	if url == "" {
		return
	}
	children, err := gallery.ChildIDsByParentURL(g.DB, url)
	if err != nil {
		logx.Warnf("api: child lookup for %q: %v", url, err)
		return
	}
	for _, child := range children {
		if child == imageID {
			continue
		}
		linkFirstSource(g, imageID, child)
	}
}

// Only while the derivative names no source: a fetch must not stack its
// claim on one the operator declared by hand.
func linkFirstSource(g *library.Gallery, source, derivative int64) {
	has, err := relations.HasDerivativeSource(g.DB, derivative)
	if err != nil {
		logx.Debugf("api: parent link %d -> %d skipped: %v", source, derivative, err)
		return
	}
	if has {
		return
	}
	if err := g.RelationsSvc.AddDerivativeEdge(source, derivative); err != nil {
		logx.Debugf("api: parent link %d -> %d skipped: %v", source, derivative, err)
	}
}

func (h *Handler) enrichImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var canonPath, storedMD5 string
	switch err := g.DB.Read.QueryRow(`SELECT canonical_path, md5 FROM images WHERE id = ?`, id).Scan(&canonPath, &storedMD5); {
	case errors.Is(err, sql.ErrNoRows):
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		serverError(w, err)
		return
	}
	var body contract.EnrichRequest
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	prov, err := provenanceJSON{body.PostFields}.read()
	if badRequest(w, err) {
		return
	}
	sourceMD5 := strings.TrimSpace(body.SourceMD5)
	if err := validateMaxLen("source_md5", sourceMD5, contract.MaxMD5); badRequest(w, err) {
		return
	}
	verified := true
	md5Verdict := ""
	if body.Verify {
		if sourceMD5 == "" {
			verified = false
		} else {
			got := storedMD5
			if got == "" {
				var err error
				if got, err = g.StoreMD5(r.Context(), id); err != nil {
					h.refuse(w, g, id, "could not verify the file; fetch not applied", http.StatusInternalServerError, "internal_error", "cannot hash image: "+err.Error())
					return
				}
			}
			if !strings.EqualFold(got, sourceMD5) {
				md5Verdict = "differ"
				// A similarity-matched origin serves a different file by
				// design and applies unverified. The flag may sit on the
				// (site, "") row the merge below has not adopted yet.
				if body.Similarity <= 0 && !gallery.SourceSimilarityMatched(g.DB, id, prov.source, prov.postID) &&
					!gallery.SourceSimilarityMatched(g.DB, id, prov.source, "") {
					// Recorded although the fetch is refused: the
					// [upgrade] gate reads it.
					if err := g.Edit(func(d *db.DB) error { return gallery.SetSourceMD5Match(d, id, prov.source, prov.postID, md5Verdict) }); err != nil {
						logx.Warnf("api enrich: record md5 verdict: %v", err)
					}
					h.recordFetch(g, id, "mismatch", "the source returned a different file (hash mismatch); no tags applied")
					contract.WriteError(w, http.StatusConflict, contract.CodeHashMismatch, "the source returned a different file")
					return
				}
				verified = false
			} else {
				md5Verdict = "match"
			}
		}
	}
	sum, tagWarnings, err := h.mergeSource(g, id, prov.source, prov.postID, prov.url, sourceMD5, prov.parentURL,
		prov.postFile, body.Tags, prov.withheld, "api")
	if err != nil {
		h.refuse(w, g, id, "fetch failed while applying tags", http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	linkParentRelations(g, id, prov.url, prov.parentURL)
	failed := "recording the match score"
	if err := g.Edit(func(d *db.DB) error {
		if err := gallery.SetSourceSimilarity(d, id, prov.source, prov.postID, body.Similarity); err != nil {
			return err
		}
		// After the merge, which creates a first enrich's origin row.
		if md5Verdict != "" {
			failed = "recording the hash verdict"
			if err := gallery.SetSourceMD5Match(d, id, prov.source, prov.postID, md5Verdict); err != nil {
				return err
			}
		}
		step, err := gallery.ApplySourceProvenance(d, id, prov.source, prov.postID, prov.commentary, prov.translated, prov.original, prov.notes)
		failed = "applying " + step
		return err
	}); err != nil {
		h.refuse(w, g, id, "fetch failed while "+failed, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	h.recordFetch(g, id, "ok", fetchSummary(sum, len(body.Tags)))
	// A hash lookup reports its hit by enriching.
	recordLookupHit(g, id, prov.source)
	contract.WriteJSON(w, http.StatusOK, withWarnings(map[string]any{"merge": sum, "verified": verified}, tagWarnings))
}

func applySourceProvenance(g *library.Gallery, id int64, p provenance) (string, error) {
	var step string
	err := g.Edit(func(d *db.DB) error {
		var err error
		step, err = gallery.ApplySourceProvenance(d, id, p.source, p.postID, p.commentary, p.translated, p.original, p.notes)
		return err
	})
	return step, err
}

func fetchSummary(sum gallery.MergeSummary, tagsSent int) string {
	switch {
	case tagsSent == 0 && sum.SourceAdded:
		return "Recorded the source; no tags were fetched."
	case sum.TagsAdded > 0 && sum.TagsRetired > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d; %d no longer listed there).", sum.TagsAdded, sum.TagsRetired)
	case sum.TagsAdded > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d).", sum.TagsAdded)
	case sum.TagsRetired > 0:
		return fmt.Sprintf("Fetched tags from the source (%d no longer listed there).", sum.TagsRetired)
	default:
		return "Fetched tags from the source."
	}
}

func (h *Handler) fetchStatusReport(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		State   string `json:"state"`
		Message string `json:"message"`
		Kind    string `json:"kind"`
		Backend string `json:"backend"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if strings.TrimSpace(body.State) == "" {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "state is required")
		return
	}
	h.recordFetch(g, id, body.State, firstRunes(body.Message, maxFetchMessageLen))
	recordLookupTerminal(g, id, body.Kind, body.Backend, body.State)
	contract.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func recordLookupHit(g *library.Gallery, imageID int64, source string) {
	backend := contract.LookupBooru
	if strings.EqualFold(strings.TrimSpace(source), contract.SourcePTR) {
		backend = contract.LookupPTR
	}
	if err := g.Edit(func(d *db.DB) error { return lookup.RecordInFlight(d, imageID, backend, lookup.ResultHit, time.Now()) }); err != nil {
		logx.Warnf("api: lookup hit for image %d: %v", imageID, err)
	}
}

func recordLookupTerminal(g *library.Gallery, imageID int64, kind, backend, state string) {
	result, concluded := monloader.Outcome(state)
	if !concluded || (kind != "" && kind != contract.KindLookup) {
		return
	}
	// A monloader that names neither kind nor backend concludes every
	// in-flight row.
	backends := []string{""}
	if backend != "" {
		backends = lookup.Backends(backend)
	}
	if err := g.Edit(func(d *db.DB) error {
		for _, b := range backends {
			if err := lookup.RecordInFlight(d, imageID, b, result, time.Now()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		logx.Warnf("api: lookup outcome for image %d: %v", imageID, err)
	}
}

func (h *Handler) replaceImageFile(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var curSHA, fileType, canonical string
	var oldW, oldH *int
	var oldSize int64
	switch err := g.DB.Read.QueryRow(
		`SELECT sha256, file_type, width, height, file_size, canonical_path FROM images WHERE id = ?`, id,
	).Scan(&curSHA, &fileType, &oldW, &oldH, &oldSize, &canonical); {
	case errors.Is(err, sql.ErrNoRows):
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		serverError(w, err)
		return
	}
	if gallery.IsVideoType(fileType) || fileType == models.FileTypeCBZ {
		h.refuse(w, g, id, "this file type cannot be replaced", http.StatusConflict, contract.CodeWrongType, "only image rows can have their file replaced")
		return
	}
	if !gallery.NamedInside(g.GalleryPath, canonical) {
		h.refuse(w, g, id, "the file is outside this gallery", http.StatusConflict, "conflict", "the image's file is outside the gallery root")
		return
	}
	if err := g.Boundary().Check(canonical); err != nil {
		h.refuse(w, g, id, "the file sits in a folder this gallery leaves out", http.StatusConflict, "conflict", "the image's file is not this gallery's: "+err.Error())
		return
	}
	if !isMultipart(r.Header.Get("Content-Type")) {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "multipart body required")
		return
	}
	file, fh, err := r.FormFile(contract.FieldFile)
	if err != nil {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return
	}
	defer func() { _ = file.Close() }()
	if maxMB := int64(h.cfg().Gallery.MaxFileSizeMB); maxMB > 0 && fh.Size > maxMB<<20 {
		h.refuse(w, g, id, "the file is over the gallery's size cap", http.StatusRequestEntityTooLarge, contract.CodeFileTooLarge, fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
		return
	}

	prov, err := provenanceFromForm(r).read()
	if badRequest(w, err) {
		return
	}
	claimedMD5 := strings.TrimSpace(r.FormValue(contract.FieldMD5))
	if err := validateMaxLen("md5", claimedMD5, contract.MaxMD5); badRequest(w, err) {
		return
	}
	var tags []string
	if tagsJSON := r.FormValue(contract.FieldTags); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			contract.WriteError(w, http.StatusBadRequest, "invalid_request", "tags must be a JSON array of names")
			return
		}
	}

	// Staged outside the watched gallery tree so the watcher never sees a
	// half-written file.
	staged, err := os.CreateTemp(g.ThumbnailsPath, "replace-*"+filepath.Ext(fh.Filename))
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to stage upload")
		return
	}
	stagedPath := staged.Name()
	discardStaged := func() { _ = os.Remove(stagedPath) }
	shaH, md5H := sha256.New(), md5.New()
	if _, err := io.Copy(io.MultiWriter(staged, shaH, md5H), file); err != nil {
		_ = staged.Close()
		discardStaged()
		logx.Warnf("api replaceImage save: %v", err)
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", saveFailure(err))
		return
	}
	_ = staged.Close()
	newSHA := hex.EncodeToString(shaH.Sum(nil))
	newMD5 := hex.EncodeToString(md5H.Sum(nil))
	upstreamMD5, rescued := newMD5, false

	applyMeta := func() (gallery.MergeSummary, []string, bool) {
		sum, tagWarnings, err := h.mergeSource(g, id, prov.source, prov.postID, prov.url, claimedMD5, prov.parentURL, prov.postFile, tags, prov.withheld, "api")
		if err != nil {
			h.refuse(w, g, id, "replace failed while applying tags", http.StatusInternalServerError, "internal_error", err.Error())
			return sum, nil, false
		}
		linkParentRelations(g, id, prov.url, prov.parentURL)
		if step, err := applySourceProvenance(g, id, prov); err != nil {
			h.refuse(w, g, id, "replace failed while applying "+step, http.StatusInternalServerError, "internal_error", err.Error())
			return sum, tagWarnings, false
		}
		if prov.source != "" {
			if err := g.Edit(func(d *db.DB) error {
				if rescued {
					return gallery.MarkSourceRescued(d, id, prov.source, prov.postID, upstreamMD5)
				}
				return gallery.MarkSourceExact(d, id, prov.source, prov.postID, newMD5)
			}); err != nil {
				h.refuse(w, g, id, "replace failed while recording the source state", http.StatusInternalServerError, "internal_error", err.Error())
				return sum, tagWarnings, false
			}
		}
		return sum, tagWarnings, true
	}

	if strings.EqualFold(newSHA, curSHA) {
		discardStaged()
		sum, tagWarnings, ok := applyMeta()
		if !ok {
			return
		}
		msg := "The file already matches the source."
		if prov.source != "" {
			msg += " " + fetchSummary(sum, len(tags))
		}
		h.recordFetch(g, id, "ok", msg)
		contract.WriteJSON(w, http.StatusOK, withWarnings(map[string]any{"replaced": false, "merge": sum}, tagWarnings))
		return
	}

	// Rerun whenever the staged digest moves, or the write fails on the
	// UNIQUE constraint instead of this clean 409.
	refuseHeldSHA := func() bool {
		otherID, err := gallery.ImageIDBySHA(g.DB.Read, newSHA)
		if err != nil || otherID == id {
			return false
		}
		discardStaged()
		if g.RelationsSvc != nil {
			if err := g.RelationsSvc.AddDuplicate(otherID, id); err != nil {
				logx.Debugf("api replace: duplicate link %d - %d skipped: %v", id, otherID, err)
			}
		}
		h.recordFetch(g, id, contract.CodeAlreadyExists,
			fmt.Sprintf("You already hold the original as image %d.", otherID))
		contract.WriteError(w, http.StatusConflict, contract.CodeAlreadyExists,
			fmt.Sprintf("the original already exists as image %d", otherID))
		return true
	}
	if refuseHeldSHA() {
		return
	}

	newType, ftErr := gallery.DetectFileType(stagedPath)
	if ftErr != nil {
		discardStaged()
		h.refuse(w, g, id, "the source served an unsupported file type", http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	if magic, err := gallery.MagicFileType(stagedPath); err == nil {
		newType = magic
	}
	if gallery.IsVideoType(newType) || newType == models.FileTypeCBZ {
		discardStaged()
		h.refuse(w, g, id, "the source serves a video or archive; only image files can replace an image", http.StatusConflict, contract.CodeWrongType, "the replacement must be an image file")
		return
	}
	if !gallery.IsFFmpegStill(newType) && !canDecodeImage(stagedPath) {
		rescued = newType == models.FileTypeJPEG && jpegBytes(stagedPath) &&
			gallery.NormalizeImage(stagedPath) == nil && canDecodeImage(stagedPath)
		if !rescued {
			discardStaged()
			h.refuse(w, g, id, "the downloaded file does not decode as an image", http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return
		}
		if reSHA, err := gallery.HashFile(stagedPath); err == nil && reSHA != newSHA {
			newSHA = reSHA
			if refuseHeldSHA() {
				return
			}
		}
		if reMD5, err := gallery.Md5File(stagedPath); err == nil {
			newMD5 = reMD5
		}
	}

	err = g.ReplaceFile(id, stagedPath, newSHA, newMD5, newType)
	if errors.As(err, new(gallery.Exclusion)) {
		discardStaged()
		h.refuse(w, g, id, "the new file type's name is one this gallery leaves out", http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		discardStaged()
		logx.Warnf("api replace image %d: %v", id, err)
		h.refuse(w, g, id, "the file replacement failed", http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	sum, tagWarnings, ok := applyMeta()
	if !ok {
		return
	}

	var newW, newH *int
	var newSize int64
	_ = g.DB.Read.QueryRow(`SELECT width, height, file_size FROM images WHERE id = ?`, id).
		Scan(&newW, &newH, &newSize)
	msg := fmt.Sprintf("Replaced the file (%s -> %s, %d kB -> %d kB).",
		dimsLabel(oldW, oldH), dimsLabel(newW, newH), (oldSize+512)/1024, (newSize+512)/1024)
	if prov.source != "" {
		msg += " " + fetchSummary(sum, len(tags))
	}
	h.recordFetch(g, id, "ok", msg)
	contract.WriteJSON(w, http.StatusOK, withWarnings(map[string]any{"replaced": true, "merge": sum}, tagWarnings))
}

func dimsLabel(w, h *int) string {
	if w == nil || h == nil {
		return "?x?"
	}
	return fmt.Sprintf("%dx%d", *w, *h)
}

// The bytes decide what is checked, as they decide what ingest records.
func decodableImage(path, fileType string) error {
	if magic, err := gallery.MagicFileType(path); err == nil {
		fileType = magic
	}
	if gallery.IsVideoType(fileType) || fileType == models.FileTypeCBZ || gallery.IsFFmpegStill(fileType) || canDecodeImage(path) {
		return nil
	}
	return errors.New("file does not decode as an image")
}

func canDecodeImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	return err == nil && cfg.Width > 0 && cfg.Height > 0
}

// ffmpeg decodes whatever it can read, so a file merely named .jpg must
// not reach the rescue.
func jpegBytes(path string) bool {
	t, err := gallery.MagicFileType(path)
	return err == nil && t == models.FileTypeJPEG
}

func undecodableJPEG(f io.ReadSeeker) bool {
	defer func() { _, _ = f.Seek(0, io.SeekStart) }()
	magic := make([]byte, 3)
	if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, []byte{0xFF, 0xD8, 0xFF}) {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	_, _, err := image.DecodeConfig(f)
	return err != nil
}

// The rescue runs in dir, outside the watched gallery, so the watcher
// never sees the bytes it replaces. The caller removes the file.
func rescueJPEG(dir string, src io.Reader) (*os.File, string, error) {
	staged, err := os.CreateTemp(dir, "rescue-*.jpg")
	if err != nil {
		return nil, "", err
	}
	md5H := md5.New()
	_, err = io.Copy(io.MultiWriter(staged, md5H), src)
	_ = staged.Close()
	if err == nil {
		err = gallery.NormalizeImage(staged.Name())
	}
	if err == nil && !canDecodeImage(staged.Name()) {
		err = errors.New("the rescued file does not decode")
	}
	if err == nil {
		var rescued *os.File
		if rescued, err = os.Open(staged.Name()); err == nil {
			return rescued, hex.EncodeToString(md5H.Sum(nil)), nil
		}
	}
	_ = os.Remove(staged.Name())
	return nil, "", err
}

func (h *Handler) buildImageResponse(g *library.Gallery, imageID int64) (*imageResponse, error) {
	img, err := models.ScanImageRow(g.DB.Read.QueryRow(
		`SELECT `+models.ImageRowColumns+` FROM images i WHERE i.id = ?`, imageID))
	if err != nil {
		return nil, err
	}

	var aliases []string
	if byID, err := loadAliasesForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse aliases: %v", err)
	} else {
		aliases = byID[imageID]
	}
	var tags []imageTagJSON
	if byID, err := loadTagsForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse tags: %v", err)
	} else {
		tags = byID[imageID]
	}

	resp := makeImageResponse(g, img, tags, aliases)
	if srcs, err := loadTagSourcesForImage(g, imageID); err != nil {
		logx.Warnf("buildImageResponse tag sources: %v", err)
	} else {
		resp.TagSources = srcs
	}
	if cols, err := gallery.CollectionsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse collections: %v", err)
	} else {
		for _, c := range cols {
			resp.Collections = append(resp.Collections, collectionJSON{Name: c.Name, Order: c.Order})
		}
	}
	if srcs, err := gallery.SourcesForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse sources: %v", err)
	} else {
		for _, s := range srcs {
			resp.Sources = append(resp.Sources, sourceJSON{Site: s.Site, PostID: s.PostID, URL: s.URL, Commentary: s.Commentary,
				CommentaryTranslated: s.CommentaryTranslated, Original: s.Original, Similarity: s.Similarity})
		}
	}
	if anns, err := gallery.AnnotationsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse annotations: %v", err)
	} else {
		for _, a := range anns {
			aj := annotationJSON{Site: a.Site, PostID: a.PostID, X: a.X, Y: a.Y, W: a.W, H: a.H, Body: a.Body}
			if text := markup.Parse(a.Body).Text(); text != a.Body {
				aj.BodyText = text
			}
			resp.Annotations = append(resp.Annotations, aj)
		}
	}
	addLookupState(g, h.cfg().LookupBackends(g.Name), imageID, &resp)
	return &resp, nil
}

// The stored next_due_at outlives the phase, the gallery list, the opt-in
// and the pairing, so it is served only while a run will keep it.
func addLookupState(g *library.Gallery, scheduled []string, imageID int64, resp *imageResponse) {
	st, err := lookup.StandingOf(g.DB, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse scheduled_lookup: %v", err)
		return
	}
	resp.ScheduledLookup, resp.ScheduledLookupPTR = &st.Booru, &st.PTR
	optedIn := map[string]bool{contract.LookupPTR: st.PTR, contract.LookupBooru: st.Booru}
	rows, err := lookup.ForImage(g.DB, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse lookup history: %v", err)
		return
	}
	for backend, r := range rows {
		entry := lookupJSON{LastResult: r.LastResult, Attempts: r.Attempts}
		if !r.LastAt.IsZero() {
			entry.LastAt = r.LastAt.Format(time.RFC3339)
		}
		if st.Candidate && optedIn[backend] && slices.Contains(scheduled, backend) && !r.NextDueAt.IsZero() {
			entry.NextDueAt = r.NextDueAt.Format(time.RFC3339)
		}
		if resp.Lookup == nil {
			resp.Lookup = map[string]lookupJSON{}
		}
		resp.Lookup[backend] = entry
	}
}

func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	resp, err := h.buildImageResponse(g, id)
	if errors.Is(err, sql.ErrNoRows) {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if serverError(w, err) {
		return
	}
	contract.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) patchImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}

	var body struct {
		Source             *string `json:"source"`
		URL                *string `json:"url"`
		Collection         *string `json:"collection"`
		CollectionOrder    *int    `json:"collection_order"`
		IsFavorited        *bool   `json:"is_favorited"`
		IsInbox            *bool   `json:"is_inbox"`
		ScheduledLookup    *bool   `json:"scheduled_lookup"`
		ScheduledLookupPTR *bool   `json:"scheduled_lookup_ptr"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}

	setSrc := false
	var srcSite, srcURL string
	if body.Source != nil || body.URL != nil {
		if err := g.DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&srcSite, &srcURL); serverError(w, err) {
			return
		}
		if body.Source != nil {
			s := strings.TrimSpace(*body.Source)
			if err := validateImageSource(s); badRequest(w, err) {
				return
			}
			srcSite = s
		}
		if body.URL != nil {
			u := strings.TrimSpace(*body.URL)
			if err := validateImageURL(u); badRequest(w, err) {
				return
			}
			srcURL = u
		}
		setSrc = true
	}
	setHome := false
	var homeName string
	var homeOrder *int
	if body.Collection != nil || body.CollectionOrder != nil {
		var curSeries string
		var curOrder sql.NullInt64
		if err := g.DB.Read.QueryRow(`SELECT series, series_order FROM images WHERE id = ?`, id).Scan(&curSeries, &curOrder); serverError(w, err) {
			return
		}
		homeName = curSeries
		if body.Collection != nil {
			c := strings.TrimSpace(*body.Collection)
			if err := validateImageCollection(c); badRequest(w, err) {
				return
			}
			homeName = c
		}
		if body.CollectionOrder != nil {
			n := *body.CollectionOrder
			if n < 1 {
				contract.WriteError(w, http.StatusBadRequest, "invalid_request", "collection_order must be 1 or higher")
				return
			}
			if homeName == "" {
				contract.WriteError(w, http.StatusBadRequest, "invalid_request", "collection_order requires a non-empty collection")
				return
			}
			homeOrder = &n
		} else if homeName != "" && curOrder.Valid {
			v := int(curOrder.Int64)
			homeOrder = &v
		}
		setHome = true
	}
	flags := map[gallery.ImageFlag]*bool{gallery.FlagFavorited: body.IsFavorited, gallery.FlagInbox: body.IsInbox}
	lookups := map[string]*bool{contract.LookupBooru: body.ScheduledLookup, contract.LookupPTR: body.ScheduledLookupPTR}
	if body.IsFavorited == nil && body.IsInbox == nil && body.ScheduledLookup == nil && body.ScheduledLookupPTR == nil && !setHome && !setSrc {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "no editable fields supplied")
		return
	}

	// The relabel is the one write that can refuse, so it goes first.
	if setSrc {
		if err := g.Edit(func(d *db.DB) error { return gallery.SetPrimarySource(d, id, srcSite, srcURL) }); err != nil {
			if errors.Is(err, gallery.ErrSourceIdentityExists) {
				contract.WriteError(w, http.StatusConflict, "conflict", err.Error())
				return
			}
			serverError(w, err)
			return
		}
	}
	for flag, want := range flags {
		if want != nil {
			if err := g.SetFlag([]int64{id}, flag, *want); serverError(w, err) {
				return
			}
		}
	}
	for backend, want := range lookups {
		if want != nil {
			if err := g.SetScheduledLookup([]int64{id}, []string{backend}, *want); serverError(w, err) {
				return
			}
		}
	}
	if setHome {
		if err := g.Edit(func(d *db.DB) error { return gallery.SetHomeCollection(d, id, homeName, homeOrder) }); serverError(w, err) {
			return
		}
	}

	resp, err := h.buildImageResponse(g, id)
	if errors.Is(err, sql.ErrNoRows) {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if serverError(w, err) {
		return
	}
	contract.WriteJSON(w, http.StatusOK, resp)
}

func atoiOrZero(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func checkCreateProvenance(w http.ResponseWriter, in *createInput, p provenanceJSON) bool {
	prov, err := p.read()
	if badRequest(w, err) {
		return false
	}
	in.provenance = prov
	if err := validateMaxLen("md5", in.md5, contract.MaxMD5); badRequest(w, err) {
		return false
	}
	return !badRequest(w, validateCollection(in.collection, in.collectionOrder))
}

func postFileFrom(w, h int, size int64, ext string) gallery.PostFile {
	return gallery.PostFile{
		Width:  max(w, 0),
		Height: max(h, 0),
		Size:   max(size, 0),
		Ext:    strings.TrimSpace(ext),
	}
}

type createInput struct {
	provenance
	imgPath         string
	initialTags     []string
	clientTags      []string
	folder          string
	autotag         bool
	taggerName      string
	via             string
	md5             string // md5 the source claimed
	rescuedMD5      string // md5 of the pushed bytes a rescue re-encoded
	collection      string
	collectionOrder *int
	naming          gallery.Naming
	upload          io.Reader
	uploadName      string
	destDir         string
	closeUpload     func()
}

func (h *Handler) parseCreateMultipart(w http.ResponseWriter, r *http.Request, g *library.Gallery) (createInput, bool) {
	var in createInput
	file, fh, err := r.FormFile(contract.FieldFile)
	if err != nil {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return in, false
	}
	in.closeUpload = func() { _ = file.Close() }
	parsed := false
	defer func() {
		if !parsed {
			in.closeUpload()
		}
	}()

	in.folder = strings.TrimSpace(r.FormValue(contract.FieldFolder))
	in.autotag = isTrue(r.FormValue("autotag"))
	in.taggerName = strings.TrimSpace(r.FormValue("tagger_name"))
	in.via = strings.TrimSpace(r.FormValue(contract.FieldVia))
	if badRequest(w, contract.ValidateVia(in.via)) {
		return in, false
	}
	in.md5 = strings.TrimSpace(r.FormValue(contract.FieldMD5))
	if tagsJSON := r.FormValue(contract.FieldTags); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &in.initialTags); err != nil {
			contract.WriteError(w, http.StatusBadRequest, "invalid_request", "tags must be a JSON array of names")
			return in, false
		}
	}
	if tagsJSON := r.FormValue(contract.FieldClientTags); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &in.clientTags); err != nil {
			contract.WriteError(w, http.StatusBadRequest, "invalid_request", "client_tags must be a JSON array of names")
			return in, false
		}
	}
	in.collection = strings.TrimSpace(r.FormValue(contract.FieldCollection))
	if raw := strings.TrimSpace(r.FormValue(contract.FieldCollectionOrder)); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			contract.WriteError(w, http.StatusBadRequest, "invalid_request", "collection_order must be an integer")
			return in, false
		}
		in.collectionOrder = &n
	}
	if !checkCreateProvenance(w, &in, provenanceFromForm(r)) {
		return in, false
	}

	defaultFolder, defaultName := h.uploadDestination()
	writeDir, naming := gallery.ReceivedNaming(g.Name, in.folder, defaultFolder, defaultName)
	in.naming = naming

	destDir, destErr := g.Boundary().ResolveSubdir(writeDir)
	if badRequest(w, destErr) {
		return in, false
	}

	in.upload = file
	if undecodableJPEG(file) {
		rescued, upstreamMD5, err := rescueJPEG(g.ThumbnailsPath, file)
		if err != nil {
			contract.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return in, false
		}
		in.closeUpload = func() {
			_ = file.Close()
			_ = rescued.Close()
			_ = os.Remove(rescued.Name())
		}
		in.upload = rescued
		in.rescuedMD5 = upstreamMD5
	}
	in.uploadName = gallery.SanitizeReceivedName(fh.Filename)
	in.destDir = destDir
	parsed = true
	return in, true
}

func (h *Handler) parseCreateJSON(w http.ResponseWriter, r *http.Request, g *library.Gallery) (createInput, bool) {
	var in createInput
	var body struct {
		provenanceJSON
		Path            string   `json:"path"`
		Tags            []string `json:"tags"`
		ClientTags      []string `json:"client_tags"`
		Folder          string   `json:"folder"`
		Autotag         bool     `json:"autotag"`
		TaggerName      string   `json:"tagger_name"`
		Via             string   `json:"via"`
		MD5             string   `json:"md5"`
		Collection      string   `json:"collection"`
		CollectionOrder *int     `json:"collection_order"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return in, false
	}
	if body.Path == "" {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "path is required")
		return in, false
	}
	imgPath := body.Path
	in.initialTags = body.Tags
	in.clientTags = body.ClientTags
	in.folder = strings.TrimSpace(body.Folder)
	in.autotag = body.Autotag
	in.taggerName = strings.TrimSpace(body.TaggerName)
	in.via = strings.TrimSpace(body.Via)
	if badRequest(w, contract.ValidateVia(in.via)) {
		return in, false
	}
	in.md5 = strings.TrimSpace(body.MD5)
	in.collection = strings.TrimSpace(body.Collection)
	in.collectionOrder = body.CollectionOrder
	if !checkCreateProvenance(w, &in, body.provenanceJSON) {
		return in, false
	}

	if in.folder != "" && !filepath.IsAbs(imgPath) {
		destDir, destErr := g.Boundary().ResolveSubdir(in.folder)
		if badRequest(w, destErr) {
			return in, false
		}
		imgPath = filepath.Join(destDir, imgPath)
	}

	// A row outside the gallery would let a later DELETE unlink files the
	// operator never meant to manage.
	absPath, absErr := filepath.Abs(imgPath)
	if absErr != nil {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return in, false
	}
	galleryAbs, gErr := filepath.Abs(g.GalleryPath)
	if gErr != nil {
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "gallery path unresolvable")
		return in, false
	}
	rel, relErr := filepath.Rel(galleryAbs, absPath)
	if relErr != nil || !filepath.IsLocal(rel) {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "path must be inside the gallery root")
		return in, false
	}
	in.imgPath = filepath.Join(galleryAbs, rel)
	if err := g.Boundary().Check(in.imgPath); err != nil {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", "path is not this gallery's: "+err.Error())
		return in, false
	}

	// A fixed message, so the response never echoes the filesystem layout.
	if _, statErr := os.Stat(in.imgPath); os.IsNotExist(statErr) {
		contract.WriteError(w, http.StatusBadRequest, "not_found", "file not found")
		return in, false
	}
	return in, true
}

func markRescued(g *library.Gallery, id int64, in createInput) error {
	if in.rescuedMD5 == "" || in.source == "" {
		return nil
	}
	return g.Edit(func(d *db.DB) error { return gallery.MarkSourceRescued(d, id, in.source, in.postID, in.rescuedMD5) })
}

// saveFailure names the errno and never the path, which would show the
// server's layout.
func saveFailure(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return "failed to save upload: " + pe.Err.Error()
	}
	return "failed to save upload"
}

func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var in createInput
	if isMultipart(r.Header.Get("Content-Type")) {
		in, ok = h.parseCreateMultipart(w, r, g)
	} else {
		in, ok = h.parseCreateJSON(w, r, g)
	}
	if !ok {
		return
	}

	if in.closeUpload != nil {
		defer in.closeUpload()
	}
	origin := models.OriginIngest
	if in.upload != nil {
		origin = models.OriginUpload
	}
	origin = cmp.Or(in.via, origin)
	maxMB := h.cfg().Gallery.MaxFileSizeMB
	spec := library.LandSpec{
		Origin: origin,
		// MaxBytesReader covers only the multipart body; this also bounds a
		// path reference.
		MaxBytes:   int64(maxMB) * 1024 * 1024,
		Check:      decodableImage,
		Naming:     in.naming,
		NameSource: in.source,
		NamePostID: in.postID,
	}
	var landed library.LandResult
	var knownID int64
	var err error
	if in.upload != nil {
		landed, err = g.Land(r.Context(), in.destDir, in.uploadName, library.FromReader(in.upload), spec)
	} else {
		if err := g.DB.Read.QueryRow(`SELECT image_id FROM image_paths WHERE path = ?`, in.imgPath).Scan(&knownID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			serverError(w, err)
			return
		}
		landed, err = g.LandInPlace(r.Context(), in.imgPath, spec)
	}
	if err != nil {
		library.LogLandFailure("api createImage", err)
		switch library.LandStageOf(err) {
		case library.LandPlace:
			contract.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		case library.LandWrite:
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", saveFailure(err))
		case library.LandSize:
			contract.WriteError(w, http.StatusRequestEntityTooLarge, contract.CodeFileTooLarge, fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
		case library.LandType:
			contract.WriteError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		case library.LandCheck:
			contract.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
		default:
			serverError(w, err)
		}
		return
	}
	img, isDuplicate := landed.Image, landed.Duplicate
	if isDuplicate {
		// Our own upload of bytes already held is dropped; a path
		// reference is the operator's own second copy and stays an alias,
		// unless that path was already the image's.
		aliasAdded := !landed.Dropped && img.CanonicalPath != landed.Path && knownID != img.ID
		sum, tagWarnings, mergeErr := h.mergeSource(g, img.ID, in.source, in.postID, in.url, in.md5, in.parentURL, in.postFile, in.initialTags, in.withheld, in.via)
		if mergeErr != nil {
			logx.Warnf("api createImage merge: %v", mergeErr)
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+mergeErr.Error())
			return
		}
		_, clientWarnings, clientRejected, clientErr := h.applyInitialTags(g, img.ID, in.clientTags, in.via)
		if clientErr != nil {
			logx.Warnf("api createImage client tags: %v", clientErr)
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+clientErr.Error())
			return
		}
		tagWarnings = append(tagWarnings, clientWarnings...)
		rejected := append(sum.RejectedTags, clientRejected...)
		sum.RejectedTags = nil
		linkParentRelations(g, img.ID, in.url, in.parentURL)
		if step, err := applySourceProvenance(g, img.ID, in.provenance); err != nil {
			logx.Warnf("api createImage %s: %v", step, err)
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
			return
		}
		if err := markRescued(g, img.ID, in); err != nil {
			logx.Warnf("api createImage rescue mark: %v", err)
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
			return
		}
		if in.collection != "" {
			// Additive, so a held page joins the pool without displacing
			// its home.
			if err := g.Edit(func(d *db.DB) error {
				return gallery.AddCollectionMembership(d, img.ID, in.collection, in.collectionOrder)
			}); err != nil {
				logx.Warnf("api createImage collection: %v", err)
				contract.WriteError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
				return
			}
		}
		resp, respErr := h.buildImageResponse(g, img.ID)
		if respErr != nil {
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
			return
		}
		contract.WriteJSON(w, http.StatusOK, withRejected(withWarnings(map[string]any{
			"image":       resp,
			"alias_added": aliasAdded,
			"merge":       sum,
		}, tagWarnings), rejected))
		return
	}

	if err := g.Edit(func(d *db.DB) error {
		return gallery.ApplyCreateProvenance(d, img.ID, in.source, in.postID, in.url, in.md5, in.parentURL, in.collection, in.commentary, in.translated, in.original, in.postFile, in.collectionOrder)
	}); err != nil {
		logx.Warnf("api createImage provenance: %v", err)
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to set provenance fields")
		return
	}
	if err := markRescued(g, img.ID, in); err != nil {
		logx.Warnf("api createImage rescue mark: %v", err)
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to set provenance fields")
		return
	}
	linkParentRelations(g, img.ID, in.url, in.parentURL)
	if in.source != "" && len(in.notes) > 0 {
		if err := g.Edit(func(d *db.DB) error {
			return gallery.ReplaceSourceAnnotations(d, img.ID, in.source, in.postID, in.notes)
		}); err != nil {
			logx.Warnf("api createImage annotations: %v", err)
		}
	}

	// Attributed to the source so each source owns a prunable slice of tags.
	tagVia := in.via
	if in.source != "" {
		tagVia = in.source
	}
	_, tagWarnings, rejected, err := h.applyInitialTags(g, img.ID, in.initialTags, tagVia)
	if err != nil {
		logx.Warnf("api createImage tags: %v", err)
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to apply tags")
		return
	}
	// Under via, never the source, whose refetch would read them as dropped.
	_, clientWarnings, clientRejected, err := h.applyInitialTags(g, img.ID, in.clientTags, in.via)
	if err != nil {
		logx.Warnf("api createImage client tags: %v", err)
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to apply tags")
		return
	}
	tagWarnings = append(tagWarnings, clientWarnings...)
	rejected = append(rejected, clientRejected...)

	var autotagNote string
	if in.autotag {
		cfg := h.cfg()
		if !tagger.IsAvailable(cfg) {
			autotagNote = "autotag skipped: tagger not available"
		} else if _, selErr := tagger.SelectForGallery(cfg, g.Name, in.taggerName); selErr != nil {
			autotagNote = "autotag skipped: " + selErr.Error()
		} else {
			autotagNote = h.queueAutotag(g.Name, in.taggerName, img.ID)
		}
	}

	resp, err := h.buildImageResponse(g, img.ID)
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
		return
	}

	if len(tagWarnings) > 0 || len(rejected) > 0 || autotagNote != "" {
		envelope := withRejected(withWarnings(map[string]any{"image": resp}, tagWarnings), rejected)
		if autotagNote != "" {
			envelope["autotag"] = autotagNote
		}
		contract.WriteJSON(w, http.StatusCreated, envelope)
		return
	}
	contract.WriteJSON(w, http.StatusCreated, resp)
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	res, err := g.DeleteImages(context.Background(), []int64{id}, library.DeleteOptions{
		RemoveEmptyFolder: r.URL.Query().Get("delete_empty_folder") == "true",
	}, nil)
	if serverError(w, err) {
		return
	}
	if len(res.Deleted) == 0 {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	body := map[string]any{}
	if deleted := res.Deleted[0]; deleted.FolderRemoved {
		body["folder_deleted"] = true
		body["folder"] = deleted.FolderPath
	}
	if res.Changed > 0 {
		body["kept_changed_files"] = res.Changed
	}
	if len(body) > 0 {
		contract.WriteJSON(w, http.StatusOK, body)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := cmp.Or(q.Get("sort"), "newest")
	orderStr := cmp.Or(q.Get("order"), search.DefaultOrder(sortStr))

	offset, limit := parsePage(r, h.cfg().UI.PageSize, 200)
	pageNum := offset/limit + 1

	expr := search.Parse(queryStr)
	var randomSeed int64
	if seedStr := q.Get("seed"); seedStr != "" {
		if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
			randomSeed = s
		}
	}
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		Page:       pageNum,
		Limit:      limit,
		RandomSeed: randomSeed,
	}
	if sortStr == "order" {
		sq.OrderCollection = search.PinnedCollectionName(expr)
	}

	src := g.Source()
	src.LookupBackends = h.cfg().LookupBackends(g.Name)
	result, err := search.Execute(r.Context(), src, sq)
	if err != nil && r.Context().Err() != nil {
		return
	}
	if serverError(w, err) {
		return
	}

	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}
	tagsByID, tagsErr := loadTagsForImages(g, ids)
	if tagsErr != nil {
		logx.Warnf("api searchImages tag load: %v", tagsErr)
		tagsByID = nil
	}
	aliasesByID, aliasErr := loadAliasesForImages(g, ids)
	if aliasErr != nil {
		logx.Warnf("api searchImages alias load: %v", aliasErr)
		aliasesByID = nil
	}

	images := make([]imageResponse, 0, len(result.Results))
	for _, img := range result.Results {
		images = append(images, makeImageResponse(g, img, tagsByID[img.ID], aliasesByID[img.ID]))
	}

	writePage(w, result.Page, result.Limit, result.Total, images)
}

func loadAliasesForImages(g *library.Gallery, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	type aliasRow struct {
		imageID int64
		path    string
	}
	placeholders, args := db.InPlaceholders(ids)
	rows, err := db.QueryAll(g.DB.Read, db.ScanRow(func(r *aliasRow) []any { return []any{&r.imageID, &r.path} }),
		`SELECT image_id, path FROM image_paths
		 WHERE is_canonical = 0 AND image_id IN (`+placeholders+`)
		 ORDER BY image_id, id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.imageID] = append(out[r.imageID], r.path)
	}
	return out, nil
}

func loadTagsForImages(g *library.Gallery, ids []int64) (map[int64][]imageTagJSON, error) {
	out := make(map[int64][]imageTagJSON, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	type tagRow struct {
		imageID int64
		imageTagJSON
	}
	placeholders, args := db.InPlaceholders(ids)
	rows, err := db.QueryAll(g.DB.Read, db.ScanRow(func(r *tagRow) []any {
		return []any{&r.imageID, &r.Name, &r.Category, &r.IsAuto, &r.Confidence, &r.TaggerName}
	}), `
		SELECT it.image_id, t.name, tc.name, it.is_auto, it.confidence, it.tagger_name
		FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE it.image_id IN (`+placeholders+`)
		ORDER BY it.image_id, tc.name, t.name`, args...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.imageID] = append(out[r.imageID], r.imageTagJSON)
	}
	return out, nil
}

func loadTagSourcesForImage(g *library.Gallery, imageID int64) (map[string][]string, error) {
	type sourceRow struct{ name, cat, source string }
	rows, err := db.QueryAll(g.DB.Read, db.ScanRow(func(r *sourceRow) []any { return []any{&r.name, &r.cat, &r.source} }), `
		SELECT t.name, tc.name, its.source
		FROM image_tag_sources its
		JOIN tags t ON t.id = its.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE its.image_id = ?
		ORDER BY t.name, its.created_at, its.source`, imageID)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range rows {
		key := r.name
		if r.cat != "general" {
			key = r.cat + ":" + r.name
		}
		out[key] = append(out[key], r.source)
	}
	return out, nil
}

func (h *Handler) listImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}
	contract.WriteJSON(w, http.StatusOK, loadImageTagsJSON(g, id))
}

func (h *Handler) addImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}

	var body struct {
		Tags []string `json:"tags"`
		Via  string   `json:"via"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if len(body.Tags) == 0 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}
	via := strings.TrimSpace(body.Via)
	if err := contract.ValidateVia(via); badRequest(w, err) {
		return
	}

	_, tagWarnings, rejected, err := h.applyInitialTags(g, id, body.Tags, via)
	if serverError(w, err) {
		return
	}

	h.writeImageTagsResponse(w, g, id, tagWarnings, rejected)
}

func (h *Handler) writeImageTagsResponse(w http.ResponseWriter, g *library.Gallery, id int64, tagWarnings, rejected []string) {
	tags := loadImageTagsJSON(g, id)
	if len(tagWarnings) > 0 || len(rejected) > 0 {
		contract.WriteJSON(w, http.StatusOK, withRejected(withWarnings(map[string]any{"tags": tags}, tagWarnings), rejected))
		return
	}
	contract.WriteJSON(w, http.StatusOK, tags)
}

// Not the full image response: its provenance, collection and lookup
// reads are freight here.
func loadImageTagsJSON(g *library.Gallery, imageID int64) []imageTagJSON {
	byID, err := loadTagsForImages(g, []int64{imageID})
	if err != nil {
		logx.Warnf("image tags: %v", err)
		return []imageTagJSON{}
	}
	if tags := byID[imageID]; tags != nil {
		return tags
	}
	return []imageTagJSON{}
}

func imageExists(g *library.Gallery, id int64) (bool, error) {
	var n int
	err := g.DB.Read.QueryRow(`SELECT 1 FROM images WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (h *Handler) applyInitialTags(g *library.Gallery, imgID int64, rawTags []string, via string) (added int, warnings, rejected []string, err error) {
	// Default "api" so API tags don't read as anonymous UI adds.
	via = cmp.Or(via, "api")
	tagIDs, warnings, rejected, err := gallery.ResolveTagNames(g.DB, g.TagSvc, rawTags, via)
	if err != nil || len(tagIDs) == 0 {
		return 0, warnings, rejected, err
	}
	results, err := g.TagSvc.AddTagsToOneImage(imgID, tagIDs, via)
	if err != nil {
		return 0, warnings, rejected, fmt.Errorf("apply tags: %w", err)
	}
	for _, r := range results {
		if r.Added {
			added++
		}
	}
	return added, warnings, rejected, nil
}

// A source owns the tags it pushes; tags pushed without one go on add-only.
func (h *Handler) mergeSource(g *library.Gallery, id int64, source, postID, url, md5, parentURL string, post gallery.PostFile, rawTags []string, withheld gallery.Withheld, via string) (gallery.MergeSummary, []string, error) {
	var sum gallery.MergeSummary
	var warnings []string
	err := g.Edit(func(d *db.DB) error {
		var err error
		sum, warnings, err = gallery.MergeSource(d, g.TagSvc, id, source, postID, url, md5, parentURL, post, rawTags, withheld)
		return err
	})
	if err != nil || source != "" || len(rawTags) == 0 {
		return sum, warnings, err
	}
	added, warns, rejected, err := h.applyInitialTags(g, id, rawTags, via)
	sum.TagsAdded += added
	sum.RejectedTags = append(sum.RejectedTags, rejected...)
	return sum, append(warnings, warns...), err
}

func (h *Handler) removeImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	if len(body.Tags) == 0 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}

	tagIDs := make([]int64, 0, len(body.Tags))
	for _, tagName := range body.Tags {
		tagID, err := h.resolveImageTagID(g, id, tagName)
		if errors.Is(err, errTagAmbiguous) {
			contract.WriteError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if serverError(w, err) {
			return
		}
		if tagID == 0 {
			continue
		}
		tagIDs = append(tagIDs, tagID)
	}
	if len(tagIDs) > 0 {
		err := g.TagSvc.RemoveTagsFromOneImage(id, tagIDs)
		if errors.Is(err, tags.ErrTagImplied) {
			contract.WriteError(w, http.StatusConflict, "tag_implied", err.Error())
			return
		}
		if serverError(w, err) {
			return
		}
	}

	h.writeImageTagsResponse(w, g, id, nil, nil)
}

var errTagAmbiguous = errors.New("exists on this image in multiple categories; use category:name")

// A category-qualified miss falls through to the literal name, so a
// general tag named like "artist:foo" stays removable.
func (h *Handler) resolveImageTagID(g *library.Gallery, imageID int64, tagName string) (int64, error) {
	tagName = strings.TrimSpace(tagName)
	f, err := tags.SplitForm(g.DB, tagName)
	if err != nil {
		return 0, err
	}
	if f.CategoryID != 0 {
		t, err := g.TagSvc.FindTag(f.Name, f.CategoryID)
		if err != nil && !errors.Is(err, tags.ErrTagNotFound) {
			return 0, err
		}
		if t != nil {
			var carried bool
			if err := g.DB.Read.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM image_tags WHERE image_id = ? AND tag_id = ?)`, imageID, t.ID,
			).Scan(&carried); err != nil {
				return 0, err
			}
			if carried {
				return t.ID, nil
			}
		}
	}

	ids, err := db.QueryIDs(g.DB.Read,
		`SELECT DISTINCT it.tag_id FROM tags t
		 JOIN image_tags it ON it.tag_id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE it.image_id = ? AND t.name = ?`,
		imageID, tags.NormalizeTagName(tagName),
	)
	if err != nil {
		return 0, fmt.Errorf("tag lookup failed: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("tag %q %w", tagName, errTagAmbiguous)
	}
}

func isMultipart(ct string) bool { return strings.HasPrefix(ct, "multipart/form-data") }
