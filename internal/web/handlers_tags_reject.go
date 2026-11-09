package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func (s *Server) rejectTagsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	fallback, err := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	tokens, err := splitTagTokens(r.FormValue("name"))
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	if len(tokens) == 0 {
		externalErr(w, r, "Name required.", http.StatusBadRequest)
		return
	}
	var rejected, refused, namespaces []string
	seen := map[string]bool{}
	for _, token := range tokens {
		name, catID, ns, err := s.typedCategory(token, fallback)
		namespaces = append(namespaces, ns)
		if err == nil {
			key := strconv.FormatInt(catID, 10) + ":" + tags.NormalizeTagName(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			tag, terr := s.tagSvc().RejectTagName(name, catID)
			if terr == nil {
				rejected = append(rejected, tag.Name)
				continue
			}
			err = terr
		}
		refused = append(refused, token+": "+err.Error())
	}
	if len(rejected) == 0 {
		externalErr(w, r, strings.Join(refused, "; "), http.StatusBadRequest)
		return
	}
	msg := "Rejected " + rejected[0] + "."
	if len(rejected) > 1 {
		msg = fmt.Sprintf("Rejected %d names: %s.", len(rejected), strings.Join(rejected, ", "))
	}
	msg = withNote(msg, unknownPrefixNote(namespaces...))
	if len(refused) > 0 {
		msg += " Refused " + strings.Join(refused, "; ") + "."
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/tags?type=rejected", http.StatusSeeOther)
		return
	}
	kind := "ok"
	if len(refused) > 0 {
		kind = "warn"
	}
	setFlashHeader(w, msg, kind, nil)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rejectTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tag, err := s.tagSvc().GetTag(id)
	if err == nil {
		err = s.tagSvc().RejectTag(id)
	}
	if err != nil {
		msg := "Error: " + err.Error()
		if errors.Is(err, tags.ErrTagNotFound) {
			msg = "Tag not found."
		}
		hxRefuse(w, r, http.StatusBadRequest, msg)
		return
	}
	setFlashHeader(w, "Rejected "+tag.Name+": it is off every image, and nothing brings it back until you un-reject it.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unrejectTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tag, err := s.tagSvc().GetTag(id)
	if err == nil {
		err = s.tagSvc().UnrejectTag(id)
	}
	if err != nil {
		msg := "Error: " + err.Error()
		if errors.Is(err, tags.ErrTagNotFound) {
			msg = "Tag not found."
		}
		flashHeaderErr(w, http.StatusBadRequest, msg)
		return
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/tags/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	setFlashHeader(w, tag.Name+" is a tag again.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rejectTagsSearchPost(w http.ResponseWriter, r *http.Request) {
	s.startTagScopeRun(w, r, s.runRejectTagsByIDs)
}

func (s *Server) runRejectTagsByIDs(ids []int64) {
	rejected, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "rejecting tags…", 50, func(id int64) (bool, error) {
		if err := s.tagSvc().RejectTag(id); err != nil {
			logx.Warnf("reject tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	summary := skippedSuffix(fmt.Sprintf("rejected %d tag%s", rejected, plural(rejected)), skipped)
	s.finishTagScopeJob(rejected, reasons, cancelled, "reject tags", summary)
}
