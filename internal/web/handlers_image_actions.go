package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/upgrade"
)

func (s *Server) ratingCeilingPost(w http.ResponseWriter, r *http.Request) {
	level := cmp.Or(r.URL.Query().Get("level"), r.FormValue("level"))
	writeRatingCookie(w, r, level)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) viewPrefsPost(w http.ResponseWriter, r *http.Request) {
	if v := r.FormValue("page_size"); v != "" {
		if v == "default" {
			v = ""
		} else if n, err := strconv.Atoi(v); err != nil || !slices.Contains(PageSizeOptions, n) {
			http.Error(w, "unknown page size", http.StatusBadRequest)
			return
		}
		setPrefCookie(w, r, pageSizeCookieName, v)
	}
	if v := r.FormValue("thumb"); v != "" {
		if v != "s" && v != "m" && v != "l" {
			http.Error(w, "unknown thumbnail size", http.StatusBadRequest)
			return
		}
		setPrefCookie(w, r, thumbSizeCookieName, v)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleBoolColumn(w http.ResponseWriter, r *http.Request, flag gallery.ImageFlag, onHTML, offHTML string, oob func(*http.Request, *library.Gallery) string) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.active()
	on, err := cx.ToggleFlag(id, flag)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if on {
		_, _ = w.Write([]byte(onHTML))
	} else {
		_, _ = w.Write([]byte(offHTML))
	}
	if oob != nil {
		_, _ = w.Write([]byte(oob(r, cx)))
	}
}

// FE0E pins the heart to text style; bare U+2665 turns into an oversized emoji.
func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, gallery.FlagFavorited,
		`<button type="submit" id="fav-btn" class="btn-fav active fav-burst" title="Unfavorite (f)">♥&#xFE0E;</button>`,
		`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite (f)">♡</button>`,
		nil,
	)
}

func (s *Server) toggleInbox(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, gallery.FlagInbox,
		`<button type="submit" id="inbox-btn" class="btn-inbox active" title="Archive (i)">Archive</button>`,
		`<button type="submit" id="inbox-btn" class="btn-inbox" title="Send to inbox (i)">Back to inbox</button>`,
		inboxNavOOB,
	)
}

// Only the count span is swapped: the link's classes depend on a search
// this POST doesn't carry.
func inboxNavOOB(r *http.Request, cx *library.Gallery) string {
	n, err := cx.InboxCountUnder(resolveCeiling(r, cx))
	if err != nil {
		return ""
	}
	suffix := ""
	if n > 0 {
		suffix = fmt.Sprintf(" (%d)", n)
	}
	return fmt.Sprintf(`<span id="inbox-nav-count" hx-swap-oob="true">%s</span>`, suffix)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}

	back := parseBackContext(r)

	// With a ref, back_* describes the source's search, not this image's.
	var refID *int64
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if parsed, err := strconv.ParseInt(refStr, 10, 64); err == nil && parsed != id {
			refID = &parsed
		}
	}

	// Before the delete: the neighbours can't be found once this row is gone.
	var prevID, nextID *int64
	if refID == nil && (back.Sort != "" || back.Q != "") {
		sortStr := cmp.Or(back.Sort, "newest")
		orderStr := cmp.Or(back.Order, search.DefaultOrder(sortStr))
		prevID, nextID = s.findAdjacentImages(r.Context(), id, back.Q, sortStr, orderStr, back.Seed, resolveCeiling(r, s.active()))
	}

	res, err := s.active().DeleteImages(context.Background(), []int64{id}, library.DeleteOptions{}, nil)
	if err != nil {
		logx.Errorf("delete image %d: %v", id, err)
		flashHeaderErr(w, http.StatusInternalServerError, "Delete failed; check server log.")
		return
	}
	if len(res.Deleted) == 0 {
		flashHeaderErr(w, http.StatusNotFound, fmt.Sprintf("Image #%d is already gone.", id))
		return
	}

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = back.DetailURL(*refID)
	case nextID != nil:
		redirectURL = back.DetailURL(*nextID)
	case prevID != nil:
		redirectURL = back.DetailURL(*prevID)
	default:
		redirectURL = back.GalleryURL()
	}

	flashText := fmt.Sprintf("Deleted image #%d%s.", id, keptChanged(res.Changed))
	if isHTMXRequest(r) {
		// A redirect would push a history entry that drops the ref chain; the
		// client goes back instead, the fallback serving a tab with no history.
		if refID != nil {
			setFlashHeader(w, flashText, "ok", map[string]any{
				"delete-go-back": map[string]any{"fallback": redirectURL},
			})
			w.WriteHeader(http.StatusOK)
			return
		}
		setFlashHeader(w, flashText, "ok", nil)
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Not externalErr: the form is hx-swap="none", so htmx shows only the header.
	refuse := func(code int, msg string) {
		if isHTMXRequest(r) {
			flashHeaderErr(w, code, msg)
			return
		}
		http.Error(w, msg, code)
	}
	requested := r.FormValue("path")
	if requested == "" {
		refuse(http.StatusBadRequest, "path required")
		return
	}

	// Tracked aliases only: any other path lets serveImageFile serve that file.
	var newCanonical string
	err := s.db().Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = ? AND path = ?`,
		id, requested,
	).Scan(&newCanonical)
	if errors.Is(err, sql.ErrNoRows) {
		refuse(http.StatusBadRequest, "path is not an alias of this image")
		return
	}
	if err != nil {
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	if _, statErr := os.Stat(newCanonical); statErr != nil {
		refuse(http.StatusBadRequest, "cannot set canonical: file is missing on disk")
		return
	}

	if err := s.active().Edit(func(d *db.DB) error { return gallery.PromoteCanonicalByPath(d, s.boundary(), id, newCanonical) }); err != nil {
		code := http.StatusInternalServerError
		if errors.As(err, new(gallery.Exclusion)) {
			code = http.StatusBadRequest
		}
		refuse(code, err.Error())
		return
	}
	hxDone(w, r, "Canonical path updated.", "", fmt.Sprintf("/images/%d", id))
}

const (
	maxAnnotationBodyLen = gallery.MaxAnnotationBodyLen
	maxImageNoteLen      = 10000
)

func (s *Server) setSource(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	url := strings.TrimSpace(r.FormValue("url"))
	if site == "" && url == "" {
		externalErr(w, r, "source label or url required", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(site) > contract.MaxSourceLabel {
		externalErr(w, r, fmt.Sprintf("source too long (max %d chars)", contract.MaxSourceLabel), http.StatusBadRequest)
		return
	}
	if url != "" {
		if utf8.RuneCountInString(url) > contract.MaxURL {
			externalErr(w, r, fmt.Sprintf("url too long (max %d chars)", contract.MaxURL), http.StatusBadRequest)
			return
		}
		if !gallery.ValidExternalURL(url) {
			externalErr(w, r, "url must start with http:// or https://", http.StatusBadRequest)
			return
		}
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	prevSite := strings.TrimSpace(r.FormValue("prev_site"))
	prevPost := strings.TrimSpace(r.FormValue("prev_post"))
	// A url-only origin is keyed ("", ""); has_prev tells relabel from add.
	hasPrev := r.FormValue("has_prev") == "1" || prevSite != "" || prevPost != ""
	if !s.imageExists(id) {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	if err := s.active().Edit(func(d *db.DB) error {
		if hasPrev && (!strings.EqualFold(prevSite, site) || prevPost != postID) {
			return gallery.RenameSourceMembership(d, id, prevSite, prevPost, site, postID, url)
		}
		return gallery.AddSourceMembership(d, id, site, postID, url)
	}); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageEditDone(w, r, id, "Source updated.")
}

func (s *Server) sourceMembershipAction(w http.ResponseWriter, r *http.Request, successMsg string, action func(d *db.DB, id int64, site, postID string) error) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	s.applyImageEdit(w, r, id, successMsg, func(d *db.DB) error { return action(d, id, site, postID) })
}

func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Source removed.", func(d *db.DB, id int64, site, postID string) error {
		return gallery.RemoveSourceMembership(d, id, site, postID)
	})
}

func (s *Server) makeSourcePrimary(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Primary source updated.", func(d *db.DB, id int64, site, postID string) error {
		return gallery.MakeSourcePrimary(d, id, site, postID)
	})
}

func (s *Server) keepLocalFile(w http.ResponseWriter, r *http.Request) {
	kept := r.FormValue("keep") == "1"
	msg := "Upgrade offered again."
	if kept {
		msg = "Keeping the local file."
	}
	s.sourceMembershipAction(w, r, msg, func(d *db.DB, id int64, site, postID string) error {
		return gallery.SetSourceUpgradeKept(d, id, site, postID, kept)
	})
}

func (s *Server) fetchSource(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	url, ok := requiredFormExternal(w, r, "url", "this source has no url to fetch")
	if !ok {
		return
	}
	galleryName := s.activeGallery()
	if pageGalleryStale(w, r, galleryName) {
		return
	}
	s.ml.Fetches.Record(galleryName, id, monloader.StatePending, "")
	if err := s.ml.EnqueueMetadataFetch(r.Context(), id, galleryName, url); err != nil {
		s.ml.Fetches.Clear(galleryName, id)
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	respondFetchPending(w, r, id)
}

func (s *Server) lookupImage(w http.ResponseWriter, r *http.Request) {
	id, cx, galleryName, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	backend := r.FormValue("backend")
	if backend != contract.LookupAll && backend != contract.LookupPTR && backend != contract.LookupBooru {
		externalErr(w, r, "unknown lookup backend", http.StatusBadRequest)
		return
	}
	var sha, storedMD5 string
	if err := cx.DB.Read.QueryRow(
		`SELECT sha256, md5 FROM images WHERE id = ?`, id,
	).Scan(&sha, &storedMD5); err != nil {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	var md5 string
	if backend != "ptr" {
		var err error
		if md5, err = monloader.LookupMD5(r.Context(), cx, id, storedMD5); err != nil {
			externalErr(w, r, "cannot hash the file: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	hashes := "md5 " + md5 + ", sha256 " + sha
	switch backend {
	case "ptr":
		hashes = "sha256 " + sha
	case "booru":
		hashes = "md5 " + md5
	}
	s.ml.Fetches.RecordLookup(galleryName, id, hashes)
	if err := s.ml.EnqueueLookup(r.Context(), cx, id, backend, md5, sha, false, false); err != nil {
		s.ml.Fetches.Clear(galleryName, id)
		if errors.Is(err, monloader.ErrPTRUnavailable) {
			externalErr(w, r, err.Error(), http.StatusConflict)
			return
		}
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	respondFetchPending(w, r, id)
}

// Replacing bytes is the strongest fetch, so it never stacks on a pending one.
func (s *Server) replaceImage(w http.ResponseWriter, r *http.Request) {
	id, cx, galleryName, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	src := models.ImageSource{Site: site, PostID: postID}
	if err := cx.DB.Read.QueryRow(
		`SELECT url, similarity, md5_match, upgrade_kept FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		id, site, postID,
	).Scan(&src.URL, &src.Similarity, &src.MD5Match, &src.UpgradeKept); err != nil {
		externalErr(w, r, "source not found", http.StatusNotFound)
		return
	}
	if !upgrade.Eligible(src) {
		externalErr(w, r, "this source's file is not known to differ from the local one", http.StatusConflict)
		return
	}
	if e, ok := s.ml.Fetches.Load(galleryName, id); ok && e.State == monloader.StatePending {
		externalErr(w, r, "a fetch is already running for this image; wait for it to finish", http.StatusConflict)
		return
	}
	s.ml.Fetches.Record(galleryName, id, monloader.StatePending, "")
	if err := s.ml.EnqueueReplace(r.Context(), id, galleryName, src.URL); err != nil {
		s.ml.Fetches.Clear(galleryName, id)
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	respondFetchPending(w, r, id)
}

// Free routes hold no lock across a peer call, so they snapshot the name.
func (s *Server) imageAndGallery(w http.ResponseWriter, r *http.Request) (int64, *library.Gallery, string, bool) {
	id, ok := idAndForm(w, r)
	if !ok {
		return 0, nil, "", false
	}
	cx := s.active()
	if cx == nil {
		externalErr(w, r, "no active gallery", http.StatusServiceUnavailable)
		return 0, nil, "", false
	}
	if pageGalleryStale(w, r, cx.Name) {
		return 0, nil, "", false
	}
	return id, cx, cx.Name, true
}

func (s *Server) imageEditDone(w http.ResponseWriter, r *http.Request, id int64, msg string) {
	hxDone(w, r, msg, "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) applyImageEdit(w http.ResponseWriter, r *http.Request, id int64, msg string, edit func(*db.DB) error) {
	if !s.imageExists(id) {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	if err := s.active().Edit(edit); err != nil {
		externalErr(w, r, err.Error(), imageEditStatus(err))
		return
	}
	s.imageEditDone(w, r, id, msg)
}

func imageEditStatus(err error) int {
	switch {
	case errors.Is(err, gallery.ErrSourceLabelRequired):
		return http.StatusBadRequest
	case errors.Is(err, gallery.ErrSourceNotFound), errors.Is(err, gallery.ErrAnnotationNotFound):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func (s *Server) imageExists(id int64) bool {
	var one int
	return s.db().Read.QueryRow(`SELECT 1 FROM images WHERE id = ?`, id).Scan(&one) == nil
}

func (s *Server) setNote(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if utf8.RuneCountInString(note) > maxImageNoteLen {
		externalErr(w, r, fmt.Sprintf("note too long (max %d chars)", maxImageNoteLen), http.StatusBadRequest)
		return
	}
	if err := gallery.SetNote(s.db(), id, note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			externalErr(w, r, "image not found", http.StatusNotFound)
			return
		}
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	hxDone(w, r, "Note updated.", "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) setSourceText(w http.ResponseWriter, r *http.Request, field, label string, maxLen int, successMsg string, setter func(d *db.DB, id int64, site, postID, val string) error) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	site, ok := requiredFormExternal(w, r, "site", "source label required")
	if !ok {
		return
	}
	val := strings.TrimSpace(r.FormValue(field))
	if utf8.RuneCountInString(val) > maxLen {
		externalErr(w, r, fmt.Sprintf("%s too long (max %d chars)", label, maxLen), http.StatusBadRequest)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	s.applyImageEdit(w, r, id, successMsg, func(d *db.DB) error { return setter(d, id, site, postID, val) })
}

func (s *Server) setSourceCommentary(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "commentary", "commentary", contract.MaxCommentary, "Commentary updated.",
		func(d *db.DB, id int64, site, postID, val string) error {
			return gallery.SetSourceCommentary(d, id, site, postID, val)
		})
}

func (s *Server) removeSourceCommentary(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Commentary removed.", func(d *db.DB, id int64, site, postID string) error {
		return gallery.SetSourceCommentary(d, id, site, postID, "")
	})
}

func (s *Server) setSourceTranslation(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "commentary_translated", "translation", contract.MaxCommentary, "Translation updated.",
		func(d *db.DB, id int64, site, postID, val string) error {
			return gallery.SetSourceCommentaryTranslated(d, id, site, postID, val)
		})
}

func (s *Server) removeSourceTranslation(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Translation removed.", func(d *db.DB, id int64, site, postID string) error {
		return gallery.SetSourceCommentaryTranslated(d, id, site, postID, "")
	})
}

// Freeform, no URL check: a booru declares it as lines of URLs or plain text.
func (s *Server) setSourceOriginal(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "original", "original source", contract.MaxOriginal, "Original source updated.",
		func(d *db.DB, id int64, site, postID, val string) error {
			return gallery.SetSourceOriginal(d, id, site, postID, val)
		})
}

func (s *Server) removeSourceOriginal(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Original source removed.", func(d *db.DB, id int64, site, postID string) error {
		return gallery.SetSourceOriginal(d, id, site, postID, "")
	})
}

func (s *Server) setAnnotation(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	var wImg, hImg sql.NullInt64
	if err := s.db().Read.QueryRow(`SELECT width, height FROM images WHERE id = ?`, id).Scan(&wImg, &hImg); err != nil {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	x, okX := annotationCoord(r, "x")
	y, okY := annotationCoord(r, "y")
	bw, okW := annotationCoord(r, "w")
	bh, okH := annotationCoord(r, "h")
	if !okX || !okY || !okW || !okH {
		externalErr(w, r, "coordinates must be non-negative integers", http.StatusBadRequest)
		return
	}
	if wImg.Valid && hImg.Valid && wImg.Int64 > 0 && hImg.Int64 > 0 {
		iw, ih := int(wImg.Int64), int(hImg.Int64)
		x = min(x, iw)
		y = min(y, ih)
		bw = min(bw, iw-x)
		bh = min(bh, ih-y)
	}
	if bw <= 0 || bh <= 0 {
		externalErr(w, r, "the box has no area inside the image", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if utf8.RuneCountInString(body) > maxAnnotationBodyLen {
		externalErr(w, r, fmt.Sprintf("annotation too long (max %d chars)", maxAnnotationBodyLen), http.StatusBadRequest)
		return
	}
	done := "Annotation added."
	if raw := strings.TrimSpace(r.FormValue("id")); raw != "" {
		annID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			externalErr(w, r, "bad annotation id", http.StatusBadRequest)
			return
		}
		if err := s.active().Edit(func(d *db.DB) error { return gallery.UpdateAnnotation(d, id, annID, x, y, bw, bh, body) }); err != nil {
			externalErr(w, r, err.Error(), imageEditStatus(err))
			return
		}
		done = "Annotation updated."
	} else if err := s.active().Edit(func(d *db.DB) error { return gallery.AddManualAnnotation(d, id, x, y, bw, bh, body) }); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageEditDone(w, r, id, done)
}

func (s *Server) removeAnnotation(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	annID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil {
		externalErr(w, r, "bad annotation id", http.StatusBadRequest)
		return
	}
	s.applyImageEdit(w, r, id, "Annotation removed.", func(d *db.DB) error { return gallery.DeleteAnnotation(d, id, annID) })
}

func (s *Server) previewMarkup(w http.ResponseWriter, r *http.Request) {
	if _, ok := idAndForm(w, r); !ok {
		return
	}
	s.renderTemplate(w, "partials/markup_preview.html", s.renderMarkup(r.FormValue("body")))
}

func annotationCoord(r *http.Request, field string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (s *Server) setCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	name, ok := requiredFormExternal(w, r, "collection", "collection label required")
	if !ok {
		return
	}
	if utf8.RuneCountInString(name) > contract.MaxCollection {
		externalErr(w, r, fmt.Sprintf("collection too long (max %d chars)", contract.MaxCollection), http.StatusBadRequest)
		return
	}
	var order *int
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			externalErr(w, r, "order must be an integer or empty", http.StatusBadRequest)
			return
		}
		if n < 1 {
			externalErr(w, r, "order must be 1 or higher", http.StatusBadRequest)
			return
		}
		order = &n
	}
	prev := strings.TrimSpace(r.FormValue("prev"))
	if err := s.active().Edit(func(d *db.DB) error {
		if prev != "" && !strings.EqualFold(prev, name) {
			return gallery.RenameCollectionMembership(d, id, prev, name, order)
		}
		return gallery.AddCollectionMembership(d, id, name, order)
	}); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageEditDone(w, r, id, "Collection updated.")
}

func (s *Server) removeCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	name, ok := requiredFormExternal(w, r, "collection", "collection label required")
	if !ok {
		return
	}
	s.applyImageEdit(w, r, id, "Collection removed.", func(d *db.DB) error { return gallery.RemoveCollectionMembership(d, id, name) })
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	pathID, ok := pathInt64(w, r, "pathID")
	if !ok {
		return
	}

	var isCanon int
	if err := s.db().Read.QueryRow(
		`SELECT is_canonical FROM image_paths WHERE id = ? AND image_id = ?`, pathID, id,
	).Scan(&isCanon); err != nil {
		flashHeaderErr(w, http.StatusNotFound, "That copy is no longer listed.")
		return
	}
	if isCanon == 1 {
		flashHeaderErr(w, http.StatusBadRequest, "The canonical path cannot be deleted.")
		return
	}

	res, err := s.active().DeleteCopies(context.Background(), []int64{pathID}, nil)
	if err != nil {
		logx.Warnf("delete alias %d: %v", pathID, err)
		flashHeaderErr(w, http.StatusInternalServerError, "Delete failed; check server log.")
		return
	}
	if len(res.Kept) > 0 {
		replyKeptCopy(w, r, res.Kept[0])
		return
	}

	if isHTMXRequest(r) {
		// Empty body for HTMX outerHTML swap - removes the row.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// A 204 under htmx swaps nothing, so the refused copy's row stays listed.
func replyKeptCopy(w http.ResponseWriter, r *http.Request, k library.KeptCopy) {
	msg := fmt.Sprintf("Kept %s: %v.", k.Path, k.Reason)
	if !isHTMXRequest(r) {
		flashStatus(w, http.StatusConflict, msg)
		return
	}
	setFlashHeader(w, msg, "err", nil)
	w.WriteHeader(http.StatusNoContent)
}

// A move job even for one image: the watcher suppression hangs off it.
func (s *Server) singleImageMoveJob(w http.ResponseWriter, r *http.Request, op func(id int64) (summary string, answered bool, err error)) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeMove) {
		return
	}
	s.active().WaitIngests(s.jobs.Context())
	summary, answered, err := op(id)
	if err != nil {
		s.jobs.Fail(err.Error())
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.jobs.Complete(summary)
	if answered {
		return
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, summary, "ok", nil)
		w.Header().Set("HX-Redirect", fmt.Sprintf("/images/%d", id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// No folder field keeps the folder; an empty one means the gallery root.
func (s *Server) placeImage(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	rawFolder, wantFolder := strings.TrimSpace(r.FormValue("folder")), r.Form.Has("folder")
	rawName := strings.TrimSpace(r.FormValue("name"))
	inline := r.FormValue("inline") == "1"
	folderTmpl, folderErr := gallery.ParseNameTemplate(rawFolder, gallery.ScopeMove)
	nameTmpl, nameErr := gallery.ParseNameTemplate(rawName, gallery.ScopeRename)

	s.singleImageMoveJob(w, r, func(id int64) (string, bool, error) {
		if folderErr != nil {
			return "", false, folderErr
		}
		if nameErr != nil {
			return "", false, nameErr
		}
		var folder, name *string
		if wantFolder {
			rendered, err := s.renderName(r.Context(), folderTmpl, rawFolder, 0, 1, id)
			if err != nil {
				return "", false, err
			}
			folder = &rendered
		}
		if nameTmpl != nil {
			rendered, err := s.renderName(r.Context(), nameTmpl, rawName, 0, 1, id)
			if err != nil {
				return "", false, err
			}
			name = &rendered
		}
		res, err := s.active().PlaceImage(id, folder, name)
		if err != nil {
			return "", false, err
		}
		newBase := filepath.Base(res.NewCanonicalPath)
		// From the result: a plain move still submits the row's name.
		_, _, past := placeVerbs(res.Moved, res.Renamed)
		summary := fmt.Sprintf("%s image to %s.", titleCase(past), namePath(res.NewFolderPath, newBase))
		if inline {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(newBase))
			return summary, true, nil
		}
		return summary, false, nil
	})
}

func placeVerbs(folder, name bool) (verb, gerund, past string) {
	switch {
	case folder && name:
		return "file", "filing", "moved and renamed"
	case name:
		return "rename", "renaming", "renamed"
	default:
		return "move", "moving", "moved"
	}
}

func titleCase(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
