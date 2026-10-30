package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type tagDetailData struct {
	baseData
	Tag            *models.Tag
	Categories     []models.TagCategory
	IsRating       bool
	Canonical      *models.Tag
	Aliases        []models.Tag
	SameName       []models.Tag
	DistinctFrom   map[int64]bool
	Implications   []models.Implication
	ImpliedBy      []models.Implication
	RecentImageIDs []int64
	NoteBody       string
	NoteHTML       template.HTML
	NoteLinksText  string
	NoteLinks      []tagLinkView
	OriginKinds    map[string]string
	BackURL        string
	PrevURL        string
	NextURL        string
}

func (s *Server) tagDetailHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}
	tag, err := s.tagSvc().GetTag(id)
	if errors.Is(err, tags.ErrTagNotFound) {
		s.notFoundHandler(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cats, _ := s.tagSvc().ListCategories()

	data := tagDetailData{
		baseData:   s.base(r, "tags", tag.Name+" - "+s.booruName()),
		Tag:        tag,
		Categories: cats,
		IsRating:   tag.CategoryName == "rating",
	}
	if backRaw := r.URL.Query().Get("back"); backRaw != "" {
		if bq, err := url.ParseQuery(backRaw); err == nil {
			p := tagListingParamsFrom(bq)
			enc := p.backQS()
			data.BackURL = "/tags?" + enc
			prevID, nextID, err := s.tagSvc().AdjacentTags(s.tagListingFilter(p), id)
			if err != nil {
				logx.Warnf("adjacent tags: %v", err)
			}
			if prevID != nil {
				data.PrevURL = fmt.Sprintf("/tags/%d?back=%s", *prevID, url.QueryEscape(enc))
			}
			if nextID != nil {
				data.NextURL = fmt.Sprintf("/tags/%d?back=%s", *nextID, url.QueryEscape(enc))
			}
		}
	}
	labels := []string{tag.Origin}

	if tag.IsAlias {
		if tag.CanonicalTagID != nil {
			if canon, err := s.tagSvc().GetTag(*tag.CanonicalTagID); err == nil {
				data.Canonical = canon
				labels = append(labels, canon.Origin)
			}
		}
	} else {
		aliases, err := s.tagSvc().AliasesForTagIDs([]int64{id})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Aliases = aliases[id]
		if data.SameName, err = s.tagSvc().SameNameTags(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(data.SameName) > 0 {
			partners, err := s.tagSvc().DistinctPartners([]int64{id})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			data.DistinctFrom = map[int64]bool{}
			for _, p := range partners[id] {
				data.DistinctFrom[p.ID] = true
			}
		}
		if data.Implications, err = s.tagSvc().ListImplications(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if data.ImpliedBy, err = s.tagSvc().ImpliedBy(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		note, err := s.tagSvc().TagNote(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.NoteBody = note.Body
		data.NoteHTML = s.renderMarkup(note.Body)
		data.NoteLinksText = strings.Join(note.Links, "\n")
		data.NoteLinks = buildTagLinkViews(note.Links)
		// Newest by image id: that order streams off the index where created_at
		// would temp-sort. CROSS JOIN keeps images from driving the plan.
		recentQ := `SELECT it.image_id FROM image_tags it INDEXED BY idx_image_tags_tag_image
			 CROSS JOIN images i ON i.id = it.image_id
			 WHERE it.tag_id = ? AND i.is_missing = 0`
		recentArgs := []any{id}
		if where, wargs := resolveCeiling(r, s.active()).WhereOne("i.id"); where != "" {
			recentQ += ` AND ` + where
			recentArgs = append(recentArgs, wargs...)
		}
		data.RecentImageIDs, err = db.QueryIDs(s.db().Read, recentQ+` ORDER BY it.image_id DESC LIMIT 7`, recentArgs...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range data.Aliases {
			labels = append(labels, a.Origin)
		}
		for _, im := range data.Implications {
			labels = append(labels, im.Origin)
		}
		for _, im := range data.ImpliedBy {
			labels = append(labels, im.Origin)
		}
	}
	data.OriginKinds = s.originKinds(labels)
	s.renderTemplate(w, "tags_detail.html", data)
}

func (s *Server) setTagNote(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	body := strings.TrimSpace(r.FormValue("note"))
	if utf8.RuneCountInString(body) > tags.MaxTagNoteLen {
		externalErr(w, r, fmt.Sprintf("note too long (max %d chars)", tags.MaxTagNoteLen), http.StatusBadRequest)
		return
	}
	links, err := tags.ParseTagLinks(r.FormValue("links"))
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	switch err := s.tagSvc().SetTagNote(id, body, links); {
	case errors.Is(err, tags.ErrTagNotFound):
		externalErr(w, r, "Tag not found.", http.StatusNotFound)
	case errors.Is(err, tags.ErrAliasNote):
		externalErr(w, r, err.Error(), http.StatusBadRequest)
	case err != nil:
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
	default:
		hxDone(w, r, "Note updated.", "", fmt.Sprintf("/tags/%d", id))
	}
}

type tagLinkView struct {
	URL   string
	Label string
	Dead  bool
}

func buildTagLinkViews(links []string) []tagLinkView {
	out := make([]tagLinkView, 0, len(links))
	for _, l := range links {
		text := strings.TrimPrefix(l, "-")
		v := tagLinkView{Label: text, Dead: text != l}
		if gallery.ValidExternalURL(text) {
			v.URL = text
			v.Label = linkLabel(text)
		}
		out = append(out, v)
	}
	return out
}

// Host and path: two links to one site differ only past the host.
func linkLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	label := strings.TrimPrefix(u.Host, "www.") + u.Path
	if u.RawQuery != "" {
		label += "?" + u.RawQuery
	}
	return strings.TrimSuffix(label, "/")
}

// An empty origin is the unrecorded-source group, not a wildcard.
func relationGroupFilter(r *http.Request) (origin string, stale bool) {
	q := r.URL.Query()
	return q.Get("origin"), q.Get("stale") == "1"
}

type usageBar struct {
	Month string
	Count int
	Bar   string
}

const usageBarMaxWidth = 24

// Lazy, one pass: aggregating a big tag's image_tags rows takes seconds.
func (s *Server) tagUsagePanelHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags/%d", id), http.StatusSeeOther)
		return
	}
	applied, months, err := s.tagSvc().UsageBreakdown(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 24 months is enough to show whether the tag is still alive.
	if len(months) > 24 {
		months = months[len(months)-24:]
	}
	maxCount := 0
	for _, m := range months {
		if m.Count > maxCount {
			maxCount = m.Count
		}
	}
	bars := make([]usageBar, 0, len(months))
	for _, m := range months {
		width := 0
		if maxCount > 0 {
			width = m.Count * usageBarMaxWidth / maxCount
		}
		if width == 0 && m.Count > 0 {
			width = 1
		}
		bars = append(bars, usageBar{Month: m.Month, Count: m.Count, Bar: strings.Repeat("█", width)})
	}
	labels := make([]string, 0, len(applied))
	for _, a := range applied {
		labels = append(labels, a.Label)
	}
	s.renderTemplate(w, "partials/tag_usage_panel.html", map[string]any{
		"AppliedBy":   applied,
		"Bars":        bars,
		"OriginKinds": s.originKinds(labels),
	})
}
