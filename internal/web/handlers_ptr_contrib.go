package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

// An empty msg is a panel's refusal: a bare 200 collapses it in place
// rather than reading as breakage.
func ptrRefuse(w http.ResponseWriter, msg string, code int) {
	if msg == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, msg, code)
}

func (s *Server) ptrTagTarget(w http.ResponseWriter, r *http.Request, open func() bool, refusal string) (int64, *models.Tag, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, nil, false
	}
	if !open() {
		ptrRefuse(w, refusal, http.StatusConflict)
		return 0, nil, false
	}
	tag, ok := monloader.ContribTag(s.tagSvc, id)
	if !ok {
		ptrRefuse(w, ptrMissText(refusal), http.StatusNotFound)
		return 0, nil, false
	}
	return id, tag, true
}

// A cbz bundle has no single file to speak about, so it refuses like a
// missing row.
func (s *Server) ptrImageTarget(w http.ResponseWriter, r *http.Request, open func() bool, refusal string) (int64, string, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, "", false
	}
	if !open() {
		ptrRefuse(w, refusal, http.StatusConflict)
		return 0, "", false
	}
	sha, fileType, err := gallery.ImageSHAAndType(s.db().Read, id)
	if err != nil || fileType == models.FileTypeCBZ {
		ptrRefuse(w, ptrMissText(refusal), http.StatusNotFound)
		return 0, "", false
	}
	return id, sha, true
}

func ptrMissText(refusal string) string {
	if refusal == "" {
		return ""
	}
	return "not found"
}

func (s *Server) contribTagColor() func(string) string {
	database := s.db()
	colors := map[string]string{}
	if cats, err := s.tagSvc().ListCategories(); err == nil {
		for _, c := range cats {
			colors[c.Name] = c.Color
		}
	}
	return func(tag string) string {
		if f, err := tags.SplitForm(database, tag); err == nil && f.CategoryID != 0 {
			return colors[f.Category]
		}
		return colors["general"]
	}
}

func (s *Server) ptrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, sha, ok := s.ptrImageTarget(w, r, s.ml.ContribReadOpen, "")
	if !ok {
		return
	}
	preview, err := s.ml.ImageContribPreview(r.Context(), s.tagSvc, id, sha)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags, unknownTags []string
	hasKnown := false
	for _, t := range preview.ToAdd {
		switch t.Status {
		case contract.PreviewNew:
			newTags = append(newTags, t.Tag)
			if t.UnknownTag {
				unknownTags = append(unknownTags, t.Tag)
			}
		case contract.PreviewKnown:
			hasKnown = true
		}
	}
	for _, p := range preview.PTROnly {
		if p.Petitionable {
			petitionTags = append(petitionTags, p.Tag)
		}
	}
	canContribute, contribHint := s.ml.ContribHint()
	unattributed := s.ml.PTRUnattributed(r.Context(), s.tagSvc, id, preview)
	s.renderTemplate(w, "partials/ptr_contrib_panel.html", map[string]any{
		"ImageID":           id,
		"NewCount":          len(newTags),
		"PetitionCount":     len(petitionTags),
		"AllKnown":          hasKnown,
		"NewTip":            strings.Join(newTags, "\n"),
		"PetitionTip":       strings.Join(petitionTags, "\n"),
		"UnknownCount":      len(unknownTags),
		"UnknownTip":        strings.Join(unknownTags, "\n"),
		"UnattributedCount": len(unattributed),
		"UnattributedTip":   strings.Join(unattributed, "\n"),
		"CanContribute":     canContribute,
		"CanPull":           s.ml.PTRReady() && (len(petitionTags) > 0 || len(unattributed) > 0),
		"ContribHint":       contribHint,
		"Provisional":       preview.Provisional,
		"FailedUploads":     s.ml.Status().ContribFailed,
		"Monloader":         s.monloaderWebBase(),
	})
}

func (s *Server) ptrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, sha, ok := s.ptrImageTarget(w, r, s.ml.ContribOpen, "contributions unavailable")
	if !ok {
		return
	}
	preview, err := s.ml.ImageContribPreview(r.Context(), s.tagSvc, id, sha)
	if err != nil {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	color := s.contribTagColor()
	var actionable, known, queued, ineligible []monloader.ContribPreviewToAdd
	for _, t := range preview.ToAdd {
		t.Color = color(t.Tag)
		switch t.Status {
		case contract.PreviewNew:
			actionable = append(actionable, t)
		case contract.PreviewKnown:
			known = append(known, t)
		case contract.PreviewUnsent:
			queued = append(queued, t)
		default:
			ineligible = append(ineligible, t)
		}
	}
	var petitionable, petitioned []monloader.ContribPreviewPTROnly
	for _, p := range preview.PTROnly {
		p.Color = color(p.Tag)
		if p.Petitionable {
			petitionable = append(petitionable, p)
		} else {
			petitioned = append(petitioned, p)
		}
	}
	s.renderTemplate(w, "partials/ptr_contrib_dialog.html", map[string]any{
		"ImageID":      id,
		"SHA256":       sha,
		"Actionable":   actionable,
		"Known":        known,
		"Queued":       queued,
		"Ineligible":   ineligible,
		"Petitionable": petitionable,
		"Petitioned":   petitioned,
		"Provisional":  preview.Provisional,
	})
}

func (s *Server) ptrContribSend(w http.ResponseWriter, r *http.Request) {
	id, sha, ok := s.ptrImageTarget(w, r, s.ml.ContribOpen, "contributions unavailable")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// After a gallery switch the id can resolve to another file; the
	// ticked tags belong to the one previewed.
	if want := r.FormValue("sha256"); want != "" && want != sha {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "this image changed; reopen the dialog"})
		return
	}
	petitionReason := strings.TrimSpace(r.FormValue("petition_reason"))
	if len(r.Form["petition"]) > 0 && petitionReason == "" {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "a reason is required to petition a removal"})
		return
	}
	items := monloader.MappingItems(sha, r.Form["add"], r.Form["petition"], petitionReason)
	s.sendContribItems(w, r, "image "+strconv.FormatInt(id, 10), items)
}

func (s *Server) sendContribItems(w http.ResponseWriter, r *http.Request, scope string, items []contract.ContribItem) {
	if len(items) == 0 {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "nothing selected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), contract.ContribSendBudget)
	defer cancel()
	resp, err := s.ml.SendContrib(ctx, scope, items)
	if err != nil {
		msg := "contributions unavailable"
		var refused monloader.ContribRefused
		if errors.As(err, &refused) {
			msg = refused.Error()
		}
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": msg})
		return
	}
	s.renderContribReceipt(w, items, resp)
}

func (s *Server) renderContribReceipt(w http.ResponseWriter, items []contract.ContribItem, resp *contract.ContribResponse) {
	receipt, ok := monloader.ReadContribReceipt(items, resp.Results)
	if !ok {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{
			"Err": fmt.Sprintf("monloader answered %d of %d items; check its contribution history", len(resp.Results), len(items)),
		})
		return
	}
	w.Header().Set("HX-Retarget", "#ptr-contrib-form")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{
		"Sent":            receipt.Sent,
		"Refused":         receipt.Refused,
		"StagedAdds":      receipt.Adds,
		"StagedPetitions": receipt.Petitions,
		"RefusedRows":     receipt.RefusedRows,
		"JobID":           resp.JobID,
		"Monloader":       s.monloaderWebBase(),
	})
}

func (s *Server) tagPtrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ml.ContribReadOpen, "")
	if !ok {
		return
	}
	diff, err := s.ml.TagContribPreview(r.Context(), s.tagSvc, id, tag)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags []string
	for _, p := range diff.Local {
		if p.Direction == contract.PairSuggest {
			newTags = append(newTags, p.Label())
		}
	}
	for _, p := range diff.PTROnly {
		if p.Direction == contract.PairPetition {
			petitionTags = append(petitionTags, p.Label())
		}
	}
	canContribute, contribHint := s.ml.ContribHint()
	s.renderTemplate(w, "partials/tag_ptr_contrib_panel.html", map[string]any{
		"TagID":          id,
		"NewCount":       len(newTags),
		"PetitionCount":  len(petitionTags),
		"Pullable":       diff.Pullable && s.ml.PTRReady(),
		"NewTip":         strings.Join(newTags, "\n"),
		"PetitionTip":    strings.Join(petitionTags, "\n"),
		"KnownAs":        diff.KnownAs,
		"Unknown":        diff.Unknown,
		"Empty":          diff.Empty,
		"IdealElsewhere": diff.IdealElsewhere,
		"CanContribute":  canContribute,
		"ContribHint":    contribHint,
		"Provisional":    diff.Provisional,
		"FailedUploads":  s.ml.Status().ContribFailed,
		"Monloader":      s.monloaderWebBase(),
	})
}

func (s *Server) tagPtrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ml.ContribOpen, "contributions unavailable")
	if !ok {
		return
	}
	diff, err := s.ml.TagContribPreview(r.Context(), s.tagSvc, id, tag)
	if err != nil {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	color := s.contribTagColor()
	paint := func(p monloader.TagPairRow) monloader.TagPairRow {
		p.AColor, p.BColor = color(p.A), color(p.B)
		return p
	}
	var actionable, pending, inSync, ineligible []monloader.TagPairRow
	for _, p := range diff.Local {
		p = paint(p)
		switch p.Direction {
		case contract.PairSuggest:
			actionable = append(actionable, p)
		case contract.PairPending:
			pending = append(pending, p)
		case contract.PairPetition, contract.PairCovered:
			// Held on the PTR too, directly or through the child's parent
			// closure.
			inSync = append(inSync, p)
		default:
			ineligible = append(ineligible, p)
		}
	}
	var petitionable, petitioned []monloader.TagPairRow
	for _, p := range diff.PTROnly {
		p = paint(p)
		if p.Direction == contract.PairPetition {
			petitionable = append(petitionable, p)
		} else {
			petitioned = append(petitioned, p)
		}
	}
	s.renderTemplate(w, "partials/tag_ptr_contrib_dialog.html", map[string]any{
		"TagID":        id,
		"TagName":      tag.Name,
		"Actionable":   actionable,
		"Pending":      pending,
		"InSync":       inSync,
		"Ineligible":   ineligible,
		"Petitionable": petitionable,
		"Petitioned":   petitioned,
		"Provisional":  diff.Provisional,
	})
}

func (s *Server) tagPtrContribSend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.ml.ContribOpen() {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	suggestReason := strings.TrimSpace(r.FormValue("suggest_reason"))
	petitionReason := strings.TrimSpace(r.FormValue("petition_reason"))
	if (len(r.Form["pair"]) > 0 && suggestReason == "") || (len(r.Form["pair_petition"]) > 0 && petitionReason == "") {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "a reason is required"})
		return
	}
	items := monloader.PairItems(r.Form["pair"], contract.PairSuggest, suggestReason)
	items = append(items, monloader.PairItems(r.Form["pair_petition"], contract.PairPetition, petitionReason)...)
	s.sendContribItems(w, r, "tag "+strconv.FormatInt(id, 10), items)
}
