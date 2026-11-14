package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

func (s *Server) ptrLookupSearchPost(w http.ResponseWriter, r *http.Request) {
	s.startTagScopeRun(w, r, func(ids []int64) { s.ml.PTRTagSweep(s.tagSvc(), ids, "") })
}

func (s *Server) ptrLookupTagPost(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.ptrTagTarget(w, r, s.ml.PTRReady, "the Public Tag Repository is unavailable")
	if !ok || !parseFormOK(w, r) {
		return
	}
	as := strings.TrimSpace(r.FormValue("as"))
	if as != "" {
		form, ok := s.tagSvc().NormalizeForm(as)
		if !ok {
			http.Error(w, "not a tag name", http.StatusBadRequest)
			return
		}
		as = form
	}
	merge := r.FormValue("merge") != ""
	if merge && as == "" {
		http.Error(w, "not a tag name", http.StatusBadRequest)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	svc := s.tagSvc()
	s.goJobWork(models.JobTypeTag, func(context.Context) {
		if merge {
			s.ml.PTRMergeSweep(svc, id, as)
		} else {
			s.ml.PTRTagSweep(svc, []int64{id}, as)
		}
	})
	w.WriteHeader(http.StatusAccepted)
}

type ptrLookupRow struct {
	Name, Color string
	Class       string
	Note        string
}

type ptrLookupGroup struct {
	Title, Note string
	Rows        []ptrLookupRow
	Adds        int
}

func ptrLookupGroupView(g monloader.LookupGroup, tagForm string, color func(string) string) ptrLookupGroup {
	v := ptrLookupGroup{Adds: g.Adds}
	switch g.Kind {
	case monloader.LookupAliases:
		v.Title, v.Note = "aliases", "would become aliases of "+tagForm
	case monloader.LookupImplies:
		v.Title = "implies"
	case monloader.LookupImpliedBy:
		v.Title, v.Note = "implied by", "direct children only, at most 200"
	}
	for _, r := range g.Rows {
		v.Rows = append(v.Rows, ptrLookupRowView(g.Kind, r, color))
	}
	return v
}

func ptrLookupRowView(kind monloader.LookupGroupKind, r monloader.LookupRow, color func(string) string) ptrLookupRow {
	row := ptrLookupRow{Name: r.Name, Color: color(r.Name)}
	switch {
	case r.Adds:
		row.Class = "ptr-row-staged"
	case r.Standing == monloader.StandingHeld:
		row.Class = "ptr-row-ctx"
	default:
		row.Class = "ptr-row-refused"
	}
	alias := kind == monloader.LookupAliases
	switch r.Standing {
	case monloader.StandingInvalid:
		row.Note = "not representable"
	case monloader.StandingFree:
		row.Note = "new implication, new tag"
		if alias {
			row.Note = "new alias"
		}
	case monloader.StandingHeld:
		row.Note = "declared"
		if alias {
			row.Note = "already an alias here"
		}
	case monloader.StandingAliasElsewhere:
		row.Note = "an alias here of " + r.Canonical
	case monloader.StandingTag:
		row.Note = "new implication"
		if alias {
			row.Note = fmt.Sprintf("a tag here (usage %d)", r.Usage)
		}
	}
	return row
}

func ptrLookupStatusText(r monloader.LookupRow) string {
	switch r.Standing {
	case monloader.StandingSelf:
		return "this tag"
	case monloader.StandingFree:
		return "not in this catalog"
	case monloader.StandingHeld:
		return "alias of this tag"
	case monloader.StandingAliasElsewhere:
		return "alias of " + r.Canonical
	default:
		return fmt.Sprintf("a tag here (usage %d)", r.Usage)
	}
}

type ptrSearchRow struct {
	Spelling string
	Color    string
	Note     string
}

func (s *Server) ptrSearchRows(clusters []contract.PTRCluster) []ptrSearchRow {
	color := s.contribTagColor()
	rows := make([]ptrSearchRow, 0, len(clusters))
	for _, c := range clusters {
		var parts []string
		if len(c.Matched) > 0 && c.Matched[0] != c.Ideal {
			parts = append(parts, "via "+c.Matched[0])
		}
		if c.Aliases == 1 {
			parts = append(parts, "+1 alias")
		} else if c.Aliases > 1 {
			parts = append(parts, fmt.Sprintf("+%d aliases", c.Aliases))
		}
		if c.Implications > 0 {
			parts = append(parts, fmt.Sprintf("+%d implies", c.Implications))
		}
		if c.ImpliedBy > 0 {
			parts = append(parts, fmt.Sprintf("%d implied by", c.ImpliedBy))
		}
		if len(parts) == 0 {
			parts = append(parts, "no relations")
		}
		rows = append(rows, ptrSearchRow{Spelling: c.Ideal, Color: color(c.Ideal), Note: strings.Join(parts, " · ")})
	}
	return rows
}

// A request with no q at all is the seed the dialog opens on; an empty q
// is a typeahead.
func (s *Server) tagPtrLookupSearch(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ml.PTRReady, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	query := r.URL.Query()
	typed, isTypeahead := query["q"]
	data := map[string]any{"TagID": id, "Seed": !isTypeahead}
	render := func() { s.renderTemplate(w, "partials/tag_ptr_lookup_search.html", data) }

	tagForm := tags.FormName(tag.CategoryName, tag.Name)
	q := strings.TrimSpace(strings.Join(typed, ""))
	if isTypeahead && q == "" {
		render()
		return
	}
	// Wider than a single graph call: the seed walks several stems and a
	// substring pass a whole namespace.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var clusters []contract.PTRCluster
	var truncated bool
	var err error
	var ownHeld bool
	stem := q
	if isTypeahead {
		mode := ""
		if query.Get("anywhere") != "" {
			mode = contract.SearchContains
		}
		clusters, truncated, err = s.ml.PTRSpellingSearch(ctx, q, mode, contract.PTRSearchLimit)
	} else {
		clusters, stem, truncated, ownHeld, err = s.ml.PTRSeedSearch(ctx, tagForm)
	}
	switch {
	case errors.Is(err, monloader.ErrNoSearch):
		render()
		return
	case errors.Is(err, monloader.ErrSearchUnbounded):
		data["Note"] = "matching anywhere in the name needs a category: prefix."
		render()
		return
	case err != nil:
		data["Note"] = "the Public Tag Repository is unavailable."
		render()
		return
	}

	data["Rows"] = s.ptrSearchRows(clusters)
	switch {
	case isTypeahead:
		if truncated {
			data["Note"] = fmt.Sprintf("showing the first %d.", len(clusters))
		}
	case len(clusters) == 0:
		data["Note"] = "the Public Tag Repository holds no spelling near " + tagForm + "."
	case stem != tagForm && ownHeld:
		data["Note"] = "the Public Tag Repository holds " + tagForm + " with no relations; showing " + stem + "."
	case stem != tagForm:
		data["Note"] = "no PTR spelling starts with " + tagForm + "; showing " + stem + "."
	}
	render()
}

func (s *Server) tagPtrLookupDialog(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ml.PTRReady, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	s.renderTemplate(w, "partials/tag_ptr_lookup_dialog.html", map[string]any{
		"TagID":   id,
		"TagName": tag.Name,
	})
}

func (s *Server) tagPtrLookupPreview(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ml.PTRReady, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	data := map[string]any{"TagID": id, "TagName": tag.Name}
	spelling, ok := s.tagSvc().NormalizeForm(r.URL.Query().Get("as"))
	if !ok {
		data["Err"] = "not a tag name"
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	graph, err := s.ml.PTRTagLookup(ctx, []string{spelling})
	if err != nil {
		data["Err"] = "the Public Tag Repository is unavailable"
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	data["Spelling"] = spelling
	info := graph[spelling]
	if !info.Known {
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	tagForm := tags.FormName(tag.CategoryName, tag.Name)
	data["TagForm"] = tagForm
	found, err := monloader.LookupGroups(s.tagSvc, id, tagForm, spelling, info)
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	color := s.contribTagColor()
	groups := make([]ptrLookupGroup, len(found))
	for i, g := range found {
		groups[i] = ptrLookupGroupView(g, tagForm, color)
	}
	aliasAdds, implAdds, declared, skipped := 0, 0, 0, 0
	for _, g := range found {
		if g.Kind == monloader.LookupAliases {
			aliasAdds += g.Adds
		} else {
			implAdds += g.Adds
		}
		for _, r := range g.Rows {
			switch {
			case r.Adds:
			case r.Standing == monloader.StandingHeld:
				declared++
			default:
				skipped++
			}
		}
	}
	data["Known"] = true
	data["Ideal"] = info.Ideal
	data["SpellingColor"] = color(spelling)
	data["IdealColor"] = color(info.Ideal)
	local := monloader.LookupLocalStatus(s.tagSvc, id, tagForm, spelling)
	data["LocalStatus"], data["CanAlias"] = ptrLookupStatusText(local), local.Adds
	data["Groups"] = groups
	data["AliasAdds"] = aliasAdds
	data["ImplAdds"] = implAdds
	data["Declared"] = declared
	data["Skipped"] = skipped
	s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
}
