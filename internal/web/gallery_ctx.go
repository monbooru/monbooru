package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/tags"
)

// Image ids are per gallery, so a write from a page rendered for another
// gallery would act on different images; the page says which one it shows.
func pageGalleryStale(w http.ResponseWriter, r *http.Request, active string) bool {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !isHTMXRequest(r) || strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	page := r.Header.Get("X-Monbooru-Gallery")
	if page == "" && !session.IsMultipart(r) {
		page = r.FormValue("_gallery")
	}
	if page == "" || page == active {
		return false
	}
	w.Header().Set("X-Monbooru-Gallery", active)
	http.Error(w, fmt.Sprintf("This page shows gallery %s, but %s is now active. Reload the page.", page, active), http.StatusConflict)
	return true
}

func (s *Server) requireActive(w http.ResponseWriter, r *http.Request) (*library.Gallery, bool) {
	cx := s.active()
	if cx == nil || cx.DB == nil {
		hxRefuse(w, r, http.StatusServiceUnavailable, "No gallery is open.")
		return nil, false
	}
	return cx, true
}

// Each accessor resolves the active gallery anew; only a read route's
// lock keeps them agreeing.

func (s *Server) db() *db.DB {
	if cx := s.active(); cx != nil {
		return cx.DB
	}
	return nil
}

func (s *Server) source() search.Source {
	if cx := s.active(); cx != nil {
		src := cx.Source()
		s.cfgMu.RLock()
		src.LookupBackends = s.cfg.LookupBackends(cx.Name)
		s.cfgMu.RUnlock()
		return src
	}
	return search.Source{}
}

func (s *Server) tagSvc() *tags.Service {
	if cx := s.active(); cx != nil {
		return cx.TagSvc
	}
	return nil
}

func (s *Server) categoryExists(name string) bool {
	_, ok := s.categoryIDByName(name)
	return ok
}

func (s *Server) categoryIDByName(name string) (int64, bool) {
	d := s.db()
	if d == nil {
		return 0, false
	}
	id, ok, err := tags.CategoryIDByName(d, name)
	return id, ok && err == nil
}

func (s *Server) boundary() *gallery.Boundary {
	if cx := s.active(); cx != nil {
		return cx.Boundary()
	}
	return gallery.NewBoundary("", nil, nil)
}

func (s *Server) thumbnailsPath() string {
	if cx := s.active(); cx != nil {
		return cx.ThumbnailsPath
	}
	return ""
}

type Ceiling = library.Ceiling
