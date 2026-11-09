// Package api implements the /api/v1/ REST handlers for monbooru.
package api

import (
	"cmp"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// ResolverFunc resolves an empty name to the active gallery.
type ResolverFunc func(name string) (*library.Gallery, bool)

type Handler struct {
	// Must return a snapshot: settings rewrites the live config under a
	// lock this package does not hold.
	cfg         func() *config.Config
	jobs        *jobs.Manager
	resolver    ResolverFunc
	version     string
	lock        func() (unlock func())
	fetchStatus func(galleryName string, imageID int64, state, message string)
	memoryMark  func() func(scope string)
	done        <-chan struct{}

	autotagMu       sync.Mutex
	autotagQueue    []queuedAutotag
	autotagDraining bool
}

func New(cfg func() *config.Config, jobManager *jobs.Manager, resolver ResolverFunc, version string) *Handler {
	return &Handler{cfg: cfg, jobs: jobManager, resolver: resolver, version: version}
}

func (h *Handler) WithFetchStatus(record func(galleryName string, imageID int64, state, message string)) *Handler {
	h.fetchStatus = record
	return h
}

func (h *Handler) recordFetch(g *library.Gallery, imageID int64, state, message string) {
	if h.fetchStatus != nil {
		h.fetchStatus(g.Name, imageID, state, message)
	}
}

func (h *Handler) refuse(w http.ResponseWriter, g *library.Gallery, imageID int64, fetchMsg string, status int, code, msg string) {
	h.recordFetch(g, imageID, "error", fetchMsg)
	contract.WriteError(w, status, code, msg)
}

// WithMemoryMark: mark runs at an auto-tag job's start, and the func it
// returns logs the peak.
func (h *Handler) WithMemoryMark(mark func() func(scope string)) *Handler {
	h.memoryMark = mark
	return h
}

// WithDone stops the queued auto-tag uploads from starting a job once done
// closes.
func (h *Handler) WithDone(done <-chan struct{}) *Handler {
	h.done = done
	return h
}

// WithGalleryLock hands the upload routes the gallery lock their caller
// holds around every other route, to take once their body is read.
func (h *Handler) WithGalleryLock(lock func() (unlock func())) *Handler {
	h.lock = lock
	return h
}

// The file cap is checked by each upload route; the slack only bounds a push's metadata fields.
const multipartMetaSlack = 8 << 20

const maxJSONBody = 16 << 20

// A slow push holding the gallery lock while its body arrives would stall
// every request queued behind a gallery switch.
func (h *Handler) bodyThenLock(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isMultipart(r.Header.Get("Content-Type")) {
			if maxBytes := int64(h.cfg().Gallery.MaxFileSizeMB) * 1024 * 1024; maxBytes > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes+multipartMetaSlack)
			}
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					contract.WriteError(w, http.StatusRequestEntityTooLarge, contract.CodeFileTooLarge, "file exceeds max size")
				} else {
					contract.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid multipart body")
				}
				return
			}
		}
		if h.lock != nil {
			defer h.lock()()
		}
		next(w, r)
	}
}

func (h *Handler) uploadDestination() (folder, name string) {
	cfg := h.cfg()
	return cfg.Gallery.DefaultUploadFolder, cfg.Gallery.DefaultUploadName
}

func (h *Handler) resolveGallery(w http.ResponseWriter, r *http.Request) (*library.Gallery, bool) {
	name := cmp.Or(strings.TrimSpace(r.URL.Query().Get("gallery")), strings.TrimSpace(r.Header.Get("X-Monbooru-Gallery")))
	g, ok := h.resolver(name)
	if !ok {
		if name == "" {
			contract.WriteError(w, http.StatusServiceUnavailable, "api_disabled", "no active gallery")
		} else {
			contract.WriteError(w, http.StatusBadRequest, "invalid_gallery", "unknown gallery: "+name)
		}
		return nil, false
	}
	return g, true
}

func (h *Handler) galleryAndID(w http.ResponseWriter, r *http.Request) (*library.Gallery, int64, bool) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return nil, 0, false
	}
	id, ok := contract.PathInt64(w, r, "id")
	if !ok {
		return nil, 0, false
	}
	return g, id, true
}

// Without the probe, a tag add on a missing id still creates its tags
// before failing.
func (h *Handler) galleryAndExistingID(w http.ResponseWriter, r *http.Request) (*library.Gallery, int64, bool) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return nil, 0, false
	}
	exists, err := imageExists(g, id)
	if serverError(w, err) {
		return nil, 0, false
	}
	if !exists {
		contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
		return nil, 0, false
	}
	return g, id, true
}

func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/images", h.auth(h.bodyThenLock(h.createImage)))
	mux.HandleFunc("GET /api/v1/images/search", h.auth(h.searchImages))
	mux.HandleFunc("GET /api/v1/images/{id}", h.auth(h.getImage))
	mux.HandleFunc("PATCH /api/v1/images/{id}", h.auth(h.patchImage))
	mux.HandleFunc("DELETE /api/v1/images/{id}", h.auth(h.deleteImage))
	mux.HandleFunc("GET /api/v1/images/{id}/tags", h.auth(h.listImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/tags", h.auth(h.addImageTags))
	mux.HandleFunc("DELETE /api/v1/images/{id}/tags", h.auth(h.removeImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/enrich", h.auth(h.enrichImage))
	mux.HandleFunc("POST /api/v1/images/{id}/fetch-status", h.auth(h.fetchStatusReport))
	mux.HandleFunc("POST /api/v1/images/autotag", h.auth(h.autotagImages))
	mux.HandleFunc("POST /api/v1/images/{id}/autotag", h.auth(h.autotagImage))

	mux.HandleFunc("GET /api/v1/images/{id}/file", h.auth(h.serveImageFile))
	mux.HandleFunc("POST /api/v1/images/{id}/file", h.auth(h.bodyThenLock(h.replaceImageFile)))
	mux.HandleFunc("GET /api/v1/images/{id}/still", h.auth(h.serveImageStill))
	mux.HandleFunc("GET /api/v1/images/{id}/thumbnail", h.auth(h.serveThumbnail))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}", h.auth(h.serveMangaPage))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}/thumb", h.auth(h.serveMangaPageThumb))

	mux.HandleFunc("GET /api/v1/tags", h.auth(h.listTags))
	mux.HandleFunc("POST /api/v1/tags", h.auth(h.createTag))
	mux.HandleFunc("POST /api/v1/tags/aliases", h.auth(h.createAlias))
	mux.HandleFunc("POST /api/v1/tags/merge", h.auth(h.mergeTags))
	mux.HandleFunc("POST /api/v1/tags/resolve", h.auth(h.resolveTags))
	mux.HandleFunc("GET /api/v1/tags/{id}", h.auth(h.getTag))
	mux.HandleFunc("PATCH /api/v1/tags/{id}", h.auth(h.patchTag))
	mux.HandleFunc("DELETE /api/v1/tags/{id}", h.auth(h.deleteTag))
	mux.HandleFunc("GET /api/v1/tags/{id}/implications", h.auth(h.listImplications))
	mux.HandleFunc("POST /api/v1/tags/{id}/implications", h.auth(h.addImplication))
	mux.HandleFunc("DELETE /api/v1/tags/{id}/implications/{impliedID}", h.auth(h.removeImplication))

	mux.HandleFunc("GET /api/v1/categories", h.auth(h.listCategories))
	mux.HandleFunc("POST /api/v1/categories", h.auth(h.createCategory))
	mux.HandleFunc("PATCH /api/v1/categories/{id}", h.auth(h.patchCategory))
	mux.HandleFunc("DELETE /api/v1/categories/{id}", h.auth(h.deleteCategory))

	mux.HandleFunc("GET /api/v1/collections", h.auth(h.listCollections))

	mux.HandleFunc("GET /api/v1/galleries", h.auth(h.listGalleries))
	mux.HandleFunc("GET /api/v1/taggers", h.auth(h.listTaggers))
	mux.HandleFunc("GET /api/v1/jobs/current", h.auth(h.currentJob))

	mux.HandleFunc("GET /api/v1/images/{id}/relations", h.auth(h.relationsForImage))
	mux.HandleFunc("POST /api/v1/relations", h.auth(h.addRelation))
	mux.HandleFunc("DELETE /api/v1/relations", h.auth(h.removeRelation))

	mux.HandleFunc("GET /api/v1/openapi.json", h.openAPIJSON)
	mux.HandleFunc("GET /api/v1/docs", h.openAPIDocs)

	mux.HandleFunc("OPTIONS /api/v1/", h.preflight)

	mux.HandleFunc("GET /api/v1/", h.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/" {
			contract.WriteError(w, http.StatusNotFound, "not_found", "endpoint not found: "+r.Method+" "+r.URL.Path)
			return
		}
		contract.WriteJSON(w, http.StatusOK, map[string]any{
			"api":     "monbooru",
			"version": h.version,
			"docs":    "/api/v1/docs",
			"openapi": "/api/v1/openapi.json",
		})
	}))
	mux.HandleFunc("/api/v1/", h.auth(func(w http.ResponseWriter, r *http.Request) {
		contract.WriteError(w, http.StatusNotFound, "not_found", "endpoint not found: "+r.Method+" "+r.URL.Path)
	}))
}

func requireID(w http.ResponseWriter, v int64, name string) bool {
	if v == 0 {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", name+" required")
		return false
	}
	return true
}

func SetCORS(w http.ResponseWriter, r *http.Request, cfg *config.Config) bool {
	w.Header().Set("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if !corsAllowed(cfg, r, origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	// Content-Disposition is not CORS-safelisted; without this a browser
	// client loses the download's filename.
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
	return true
}

// CORSOK writes the 403 itself when SetCORS refuses the origin.
func CORSOK(w http.ResponseWriter, r *http.Request, cfg *config.Config) bool {
	if SetCORS(w, r, cfg) {
		return true
	}
	contract.WriteError(w, http.StatusForbidden, "forbidden", "CORS: origin not allowed")
	return false
}

// The arrival address counts alongside base_url: a base_url left at
// "localhost" would otherwise refuse the page monbooru itself just served.
func corsAllowed(cfg *config.Config, r *http.Request, origin string) bool {
	if self := requestOrigin(cfg, r); self != "" && origin == self {
		return true
	}
	// Origin never ends in a slash; base_url may.
	if origin == strings.TrimRight(cfg.Server.BaseURL, "/") {
		return true
	}
	return slices.ContainsFunc(cfg.Server.CORSOrigins, func(allowed string) bool {
		return allowed == "*" || allowed == origin
	})
}

// Empty unless Host is a literal address: Host is client-supplied, so a
// rebound or forged name would vouch for itself. The scheme comes from
// base_url, which a TLS-terminating proxy hides from the listener.
func requestOrigin(cfg *config.Config, r *http.Request) string {
	name := r.Host
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if name != "localhost" && net.ParseIP(name) == nil {
		return ""
	}
	scheme := "http"
	if strings.HasPrefix(cfg.Server.BaseURL, "https://") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Outside auth: a preflight never carries credentials.
func (h *Handler) preflight(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg()
	if !CORSOK(w, r, cfg) {
		return
	}
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Monbooru-Gallery")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfg()
		if !CORSOK(w, r, cfg) {
			return
		}

		if _, status, code, msg := cfg.Auth.Tokens.Authorize(r.Header.Get("Authorization"), scopeForMethod(r.Method)); status != 0 {
			contract.WriteError(w, status, code, msg)
			return
		}

		next(w, r)
	}
}

func scopeForMethod(method string) string {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return config.ScopeWrite
	case http.MethodDelete:
		return config.ScopeDelete
	default:
		return config.ScopeRead
	}
}

func serverError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	contract.WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
	return true
}

func badRequest(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	contract.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
	return true
}

func withWarnings(m map[string]any, warnings []string) map[string]any {
	if len(warnings) > 0 {
		m["tag_warnings"] = warnings
	}
	return m
}

func withRejected(m map[string]any, rejected []string) map[string]any {
	if len(rejected) > 0 {
		m["rejected_tags"] = rejected
	}
	return m
}

func writePage(w http.ResponseWriter, page, limit, total int, results any) {
	contract.WriteJSON(w, http.StatusOK, map[string]any{
		"page":    page,
		"limit":   limit,
		"total":   total,
		"results": results,
	})
}

func parsePage(r *http.Request, defaultLimit, maxLimit int) (offset, limit int) {
	q := r.URL.Query()
	page, limit := contract.PageParams(q.Get("page"), cmp.Or(q.Get("limit"), q.Get("page_size")), min(defaultLimit, maxLimit), maxLimit)
	return (page - 1) * limit, limit
}
