package web

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// Galleries shadows the layout's list with the config rows the picker renders.
type categoriesData struct {
	baseData
	Galleries    []config.Gallery
	Categories   []models.TagCategory
	DefaultOrder []int64
}

func (s *Server) categoriesHandler(w http.ResponseWriter, r *http.Request) {
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defaultOrder, err := s.tagSvc().DefaultCategoryOrder()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, "categories.html", categoriesData{
		baseData:     s.base(r, "categories", "Categories - "+s.booruName()),
		Galleries:    s.galleryList(),
		Categories:   cats,
		DefaultOrder: defaultOrder,
	})
}

func (s *Server) categoryColors() map[string]string {
	svc := s.tagSvc()
	if svc == nil {
		return nil
	}
	cats, err := svc.ListCategories()
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(cats))
	for _, c := range cats {
		out[c.Name] = tags.SafeCategoryColor(c.Color)
	}
	return out
}

func (s *Server) categoryCountHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	c, err := s.tagSvc().CategoryDeleteCounts(id)
	if err != nil {
		http.Error(w, err.Error(), categoryStatus(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"count":%d,"aliases":%d,"linked_aliases":%d,"implications":%d}`, c.Tags, c.Aliases, c.LinkedAliases, c.Implications)
}

func (s *Server) removeCategoryImplicationsPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	edges, err := s.tagSvc().ImplicationsDeclaredIn(id)
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.removeImplicationsAndSweep(w, edges, nil)
}

func (s *Server) createCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.FormValue("name")
	color := cmp.Or(r.FormValue("color"), "#888888")
	if _, err := s.tagSvc().CreateCategory(name, color); err != nil {
		externalErr(w, r, err.Error(), categoryStatus(err))
		return
	}
	hxDone(w, r, "Category "+name+" created.", "/categories", "/categories")
}

func (s *Server) updateCategoryPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	err := s.tagSvc().UpdateCategoryColor(id, r.FormValue("color"))
	if err != nil {
		logx.Warnf("update category %d color: %v", id, err)
	}
	if !isHTMXRequest(r) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/categories", http.StatusSeeOther)
		return
	}
	// A refused colour still gets the stored row, so the inputs swap back.
	if err != nil {
		setFlashHeader(w, err.Error(), "err", nil)
	}
	cat, err := s.tagSvc().GetCategory(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.renderTemplate(w, "partials/category_row.html", cat)
}

func (s *Server) reorderCategoriesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	var ids []int64
	for _, part := range strings.Split(r.FormValue("ids"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			http.Error(w, "Bad category id list.", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	if err := s.tagSvc().ReorderCategories(ids); err != nil {
		http.Error(w, err.Error(), categoryStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	action := cmp.Or(r.FormValue("action"), "move")
	var targetID int64
	if ts := r.FormValue("target_id"); ts != "" {
		var err error
		if targetID, err = strconv.ParseInt(ts, 10, 64); err != nil {
			http.Error(w, "Invalid target category.", http.StatusBadRequest)
			return
		}
	}
	if err := s.tagSvc().DeleteCategoryMoveOrDelete(id, action, targetID); err != nil {
		http.Error(w, err.Error(), categoryStatus(err))
		return
	}
	hxDone(w, r, "Category deleted.", "/tags", "/tags")
}

func categoryStatus(err error) int {
	var collision *tags.ErrCategoryMoveCollision
	switch {
	case errors.Is(err, tags.ErrCategoryNotFound):
		return http.StatusNotFound
	case errors.Is(err, tags.ErrBuiltinCategory), errors.Is(err, tags.ErrInvalidMoveTarget),
		errors.Is(err, tags.ErrRatingCategoryClosed), errors.Is(err, tags.ErrBuiltinCategoryName),
		errors.Is(err, tags.ErrInvalidCategoryName), errors.Is(err, tags.ErrReservedCategoryName),
		errors.Is(err, tags.ErrInvalidCategoryColor):
		return http.StatusBadRequest
	case errors.As(err, &collision), errors.Is(err, tags.ErrCategoryExists), errors.Is(err, tags.ErrCategoryOrderStale):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func (s *Server) renameCategoryPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	newName, ok := requiredFormExternal(w, r, "name", "Name required.")
	if !ok {
		return
	}
	if err := s.tagSvc().RenameCategory(id, newName); err != nil {
		externalErr(w, r, err.Error(), categoryStatus(err))
		return
	}
	hxDone(w, r, "Category renamed to "+newName+".", "/categories", "/categories")
}
