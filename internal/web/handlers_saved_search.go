package web

import (
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := search.DeleteSavedSearch(s.db(), id); err != nil {
		logx.Warnf("delete saved search %d: %v", id, err)
		flashHeaderErr(w, http.StatusInternalServerError, "Delete failed; check server log.")
		return
	}
	// 200 + empty body - HTMX outerHTML swap removes the element.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) createSavedSearch(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	query := strings.TrimSpace(r.FormValue("query"))
	if name == "" || query == "" {
		externalErr(w, r, "Name and query required.", http.StatusBadRequest)
		return
	}
	sortStr := strings.TrimSpace(r.FormValue("sort"))
	orderStr := strings.TrimSpace(r.FormValue("order"))
	seedStr := strings.TrimSpace(r.FormValue("seed"))
	if err := search.SaveSearch(s.db(), models.SavedSearch{Name: name, Query: query, Sort: sortStr, Order: orderStr, Seed: seedStr}); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "A saved search named " + name + " already exists. Delete it first or pick another name."
		}
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		writeInlineFlash(w, "ok", "Saved.")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
