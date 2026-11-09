package web

import (
	"cmp"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type tagsPageData struct {
	baseData
	Tags         []models.Tag
	Categories   []models.TagCategory
	Implications map[int64][]models.Implication
	Total        int
	Page         int
	TotalPages   int
	CategoryID   string
	Prefix       string
	Sort         string
	Order        string
	Origin       string
	Type         string
	// Raw, not the resolved cutoff, so the sidebar chips can match it.
	CreatedAfter    string
	Conflicts       string
	ConflictsTotal  int
	DistinctTotal   int
	Stale           string
	StaleTotal      int
	FullyStaleTotal int
	Folded          bool
	FoldedTotal     int
	RejectedTotal   int
	OriginCounts    []tags.OriginCount
	UsedBy          string
	UsedByLabels    []string
	UsedBySources   map[int64][]string
	OriginKinds     map[string]string
	ShowZero        bool
	ZeroOnly        bool
	BackQS          string
	RowsOnly        bool
	DistinctOf      map[int64][]tags.DistinctPartner
}

func (s *Server) originKinds(labels []string) map[string]string {
	kinds := make(map[string]string, len(labels))
	var unknown []string
	for _, l := range labels {
		switch l {
		case "":
		case "user", "ptr", "auto":
			kinds[l] = l
		default:
			unknown = append(unknown, l)
		}
	}
	if len(unknown) > 0 {
		autoSet, err := s.tagSvc().AutoTaggerLabels(unknown)
		if err != nil {
			logx.Warnf("classify origin labels: %v", err)
		}
		for _, l := range unknown {
			if _, ok := autoSet[l]; ok {
				kinds[l] = "auto"
			} else {
				kinds[l] = "site"
			}
		}
	}
	return kinds
}

func createdAfterCutoff(raw string) string {
	now := time.Now().UTC()
	switch raw {
	case "":
		return ""
	case "24h":
		return now.Add(-24 * time.Hour).Format(time.RFC3339)
	case "7d":
		return now.AddDate(0, 0, -7).Format(time.RFC3339)
	case "30d":
		return now.AddDate(0, 0, -30).Format(time.RFC3339)
	}
	return raw
}

type tagsSidebarCounts struct {
	Conflicts  int
	Distinct   int
	Stale      int
	FullyStale int
	Folded     int
	Rejected   int
	Origins    []tags.OriginCount
	UsedBy     []string
	Err        error
}

// In parallel: in sequence the scans cost more than the listing they decorate.
func (s *Server) tagsSidebarLoad(typeFilter string, full bool) tagsSidebarCounts {
	var c tagsSidebarCounts
	var mu sync.Mutex
	var wg sync.WaitGroup
	svc := s.tagSvc()
	run := func(fn func() error) {
		wg.Go(func() {
			if err := fn(); err != nil {
				mu.Lock()
				c.Err = cmp.Or(c.Err, err)
				mu.Unlock()
			}
		})
	}
	if full {
		run(func() (err error) { c.Conflicts, err = svc.ConflictsCount(); return })
		run(func() (err error) { c.Distinct, err = svc.DistinctCount(); return })
		run(func() (err error) { c.Stale, err = svc.StaleUsageCount(); return })
		run(func() (err error) { c.FullyStale, err = svc.FullyStaleCount(); return })
		run(func() (err error) { c.Folded, err = svc.FoldedDuplicatesCount(); return })
		run(func() (err error) { c.Rejected, err = svc.RejectedCount(); return })
		run(func() (err error) { c.Origins, err = svc.OriginCounts(typeFilter); return })
	}
	run(func() (err error) { c.UsedBy, err = svc.UsedByLabels(); return })
	wg.Wait()
	return c
}

func (s *Server) tagsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	catIDStr := q.Get("cat")
	prefix := q.Get("q")
	if catIDStr == "" && prefix != "" && strings.HasSuffix(prefix, ":") && strings.Count(prefix, ":") == 1 {
		catName := strings.TrimSuffix(prefix, ":")
		if catID, ok := s.categoryIDByName(catName); catName != "" && ok {
			q.Del("q")
			q.Set("cat", strconv.FormatInt(catID, 10))
			http.Redirect(w, r, "/tags?"+q.Encode(), http.StatusSeeOther)
			return
		}
	}
	p := tagListingParamsFrom(q)

	tagList, total, err := s.tagSvc().ListTags(s.tagListingFilter(p))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cats, _ := s.tagSvc().ListCategories()
	totalPages := (total + tagListingPageSize - 1) / tagListingPageSize

	if total > 0 && p.Page > totalPages {
		p.Page = totalPages
		tagList, total, err = s.tagSvc().ListTags(s.tagListingFilter(p))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	parentIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		if !t.IsAlias {
			parentIDs = append(parentIDs, t.ID)
		}
	}
	imps, err := s.tagSvc().ImplicationsForParents(parentIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The live search swaps the listing only: its input lives in the sidebar.
	rowsOnly := isHTMXRequest(r) && r.Header.Get("HX-Trigger") == "tags-q-input"
	counts := s.tagsSidebarLoad(p.Type, !rowsOnly)
	if counts.Err != nil {
		http.Error(w, counts.Err.Error(), http.StatusInternalServerError)
		return
	}
	rowIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		rowIDs = append(rowIDs, t.ID)
	}
	usedBySources, err := s.tagSvc().UsedByForTags(rowIDs, counts.UsedBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageLabels := make([]string, 0, len(tagList)+len(counts.Origins)+len(counts.UsedBy))
	for _, t := range tagList {
		pageLabels = append(pageLabels, t.Origin)
	}
	for _, oc := range counts.Origins {
		pageLabels = append(pageLabels, oc.Label)
	}
	pageLabels = append(pageLabels, counts.UsedBy...)

	data := tagsPageData{
		baseData:        s.base(r, "tags", "Tags - "+s.booruName()),
		Tags:            tagList,
		Categories:      cats,
		Implications:    imps,
		Total:           total,
		Page:            p.Page,
		TotalPages:      totalPages,
		CategoryID:      p.CatID,
		Prefix:          p.Prefix,
		Sort:            p.Sort,
		Order:           p.Order,
		Origin:          p.Origin,
		Type:            p.Type,
		CreatedAfter:    p.CreatedAfter,
		Conflicts:       p.Conflicts,
		ConflictsTotal:  counts.Conflicts,
		DistinctTotal:   counts.Distinct,
		Stale:           p.Stale,
		StaleTotal:      counts.Stale,
		FullyStaleTotal: counts.FullyStale,
		Folded:          p.Folded,
		FoldedTotal:     counts.Folded,
		RejectedTotal:   counts.Rejected,
		OriginCounts:    counts.Origins,
		UsedBy:          p.UsedBy,
		UsedByLabels:    counts.UsedBy,
		UsedBySources:   usedBySources,
		OriginKinds:     s.originKinds(pageLabels),
		ShowZero:        p.ShowZero,
		ZeroOnly:        p.ZeroOnly,
		BackQS:          p.backQS(),
		RowsOnly:        rowsOnly,
	}
	if p.Conflicts != "" {
		if data.DistinctOf, err = s.tagSvc().DistinctPartners(rowIDs); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.renderTemplate(w, "tags.html", data)
}

type tagListingParams struct {
	CatID, Prefix, Sort, Order, Origin, Type, CreatedAfter, ZeroParam, Stale, UsedBy, Conflicts string
	HasType, ShowZero, ZeroOnly, Folded                                                         bool
	Page                                                                                        int
}

func tagListingParamsFrom(q url.Values) tagListingParams {
	p := tagListingParams{
		CatID:        q.Get("cat"),
		Prefix:       q.Get("q"),
		Sort:         q.Get("sort"),
		Order:        q.Get("order"),
		Origin:       q.Get("origin"),
		UsedBy:       q.Get("used_by"),
		Type:         q.Get("type"),
		HasType:      q.Has("type"),
		CreatedAfter: q.Get("created_after"),
		ZeroParam:    q.Get("show_zero"),
		Page:         1,
	}
	if s := q.Get("stale"); s == "has" || s == "full" {
		p.Stale = s
	}
	if c := q.Get("conflicts"); c == "1" || c == "distinct" {
		p.Conflicts = c
	}
	p.Folded = q.Get("folded") == "1"
	p.Sort = cmp.Or(p.Sort, "usage")
	if p.Order != "asc" && p.Order != "desc" {
		switch p.Sort {
		case "usage", "created", "last_used":
			p.Order = "desc"
		default:
			p.Order = "asc"
		}
	}
	// An absent type means plain tags; an empty type= is All.
	// origin=alias selects alias rows, which type=tag would hide.
	if !p.HasType && p.Origin != "alias" {
		p.Type = "tag"
	}
	p.ZeroOnly = p.ZeroParam == "only"
	p.ShowZero = p.ZeroOnly || p.ZeroParam != "0"
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		p.Page = n
	}
	return p
}

const tagListingPageSize = 100

func (s *Server) tagListingFilter(p tagListingParams) tags.TagFilter {
	f := tags.TagFilter{
		Prefix:        p.Prefix,
		Sort:          p.Sort,
		Order:         p.Order,
		PageIndex:     p.Page - 1,
		Limit:         tagListingPageSize,
		Origin:        p.Origin,
		Type:          p.Type,
		CreatedAfter:  createdAfterCutoff(p.CreatedAfter),
		ShowZero:      p.ShowZero,
		ZeroOnly:      p.ZeroOnly,
		ConflictsOnly: p.Conflicts == "1",
		DistinctOnly:  p.Conflicts == "distinct",
		Stale:         p.Stale,
		FoldedOnly:    p.Folded,
		UsedBy:        p.UsedBy,
	}
	if p.CatID != "" {
		if id, err := strconv.ParseInt(p.CatID, 10, 64); err == nil {
			f.CategoryID = &id
		} else if id, ok := s.categoryIDByName(p.CatID); ok {
			f.CategoryID = &id
		}
	}
	return f
}

func (p tagListingParams) backQS() string {
	v := setNonEmpty(url.Values{}, "q", p.Prefix, "cat", p.CatID, "origin", p.Origin, "used_by", p.UsedBy,
		"created_after", p.CreatedAfter, "stale", p.Stale, "show_zero", p.ZeroParam, "conflicts", p.Conflicts)
	v.Set("sort", p.Sort)
	v.Set("order", p.Order)
	if p.HasType || p.Type != "" {
		v.Set("type", p.Type)
	}
	if p.Folded {
		v.Set("folded", "1")
	}
	if p.Page > 1 {
		v.Set("page", strconv.Itoa(p.Page))
	}
	return v.Encode()
}

// canonicalRef is an existing tag (id), or a name and category to create
// once the operation is sure to run.
type canonicalRef struct {
	id    int64
	name  string
	catID int64
}

func (s *Server) canonicalID(ref canonicalRef) (int64, error) {
	if ref.id != 0 {
		return ref.id, nil
	}
	tag, err := s.tagSvc().GetOrCreateTag(ref.name, ref.catID)
	if err != nil {
		return 0, err
	}
	return tag.ID, nil
}

// "#<id>" is a tag id, anything else a name: normalised, an alias followed to
// its canonical, a bare one taken only when one tag across the categories has
// it. With create, an unknown name comes back to be created; nothing is written.
func (s *Server) resolveCanonicalTagInput(input string, create bool) (canonicalRef, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return canonicalRef{}, "Tag name is required."
	}
	if digits, ok := strings.CutPrefix(input, "#"); ok {
		var exists int
		id, err := strconv.ParseInt(digits, 10, 64)
		if err == nil {
			if err := s.db().Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE id = ?`, id).Scan(&exists); err != nil {
				return canonicalRef{}, "Tag lookup failed: " + err.Error()
			}
		}
		if err != nil || exists == 0 {
			return canonicalRef{}, "Tag not found: " + input
		}
		return canonicalRef{id: id}, ""
	}
	f, err := tags.SplitForm(s.db(), input)
	if err != nil {
		return canonicalRef{}, "Tag lookup failed: " + err.Error()
	}
	catID, bare := f.CategoryID, strings.TrimSpace(f.Name)
	if catID != 0 && bare == "" {
		return canonicalRef{}, "Tag name is required after the category prefix."
	}
	name, err := tags.ValidateTagName(strings.Trim(bare, `"`))
	if err != nil {
		return canonicalRef{}, err.Error()
	}
	type choice struct {
		id    int64
		label string
	}
	choices, err := db.QueryAll(s.db().Read, db.ScanRow(func(c *choice) []any {
		return []any{&c.id, &c.label}
	}), `SELECT DISTINCT ct.id, c.name || ':' || ct.name
	    FROM tags t
	    JOIN tags ct ON ct.id = COALESCE(t.canonical_tag_id, t.id)
	    JOIN tag_categories c ON c.id = ct.category_id
	    WHERE t.name = ? AND (? = 0 OR t.category_id = ?)
	    ORDER BY c.name`, name, catID, catID)
	if err != nil {
		return canonicalRef{}, "Tag lookup failed: " + err.Error()
	}
	switch len(choices) {
	case 1:
		return canonicalRef{id: choices[0].id}, ""
	case 0:
		if !create {
			return canonicalRef{}, "Tag not found: " + input
		}
		if catID == 0 {
			cx := s.active()
			if cx == nil || cx.GeneralCategoryID == 0 {
				return canonicalRef{}, "Could not resolve the general category."
			}
			catID = cx.GeneralCategoryID
		}
		if catID == s.tagSvc().RatingCategoryID() && !tags.IsCanonicalRating(name) {
			return canonicalRef{}, tags.ErrNonCanonicalRating.Error()
		}
		return canonicalRef{name: name, catID: catID}, ""
	}
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.label
	}
	return canonicalRef{}, "Tag name " + name + " exists in more than one category; use " + strings.Join(labels, " or ")
}

func (s *Server) qualifiedTagName(id int64) string {
	t, err := s.tagSvc().GetTag(id)
	if err != nil {
		return "#" + strconv.FormatInt(id, 10)
	}
	return t.CategoryName + ":" + t.Name
}

// typedCategory reads a category prefix off a typed name the way the add
// box does: a real category wins over fallback, any other prefix stays in
// the name and comes back as its namespace.
func (s *Server) typedCategory(name string, fallback int64) (string, int64, string, error) {
	f, err := tags.SplitForm(s.db(), name)
	switch {
	case err != nil:
		return "", 0, "", err
	case f.CategoryID != 0 && f.Name == "":
		return "", 0, "", tags.ErrEmptyFormName
	case f.CategoryID != 0:
		return f.Name, f.CategoryID, "", nil
	}
	return name, fallback, f.Namespace, nil
}

func unknownPrefixNote(namespaces ...string) string {
	var seen []string
	for _, ns := range namespaces {
		if ns != "" && !slices.Contains(seen, ns) {
			seen = append(seen, ns)
		}
	}
	return unknownCategoryNote(seen)
}

func withNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return strings.TrimSuffix(msg, ".") + "; " + note + "."
}

func (s *Server) createTagPost(w http.ResponseWriter, r *http.Request) {
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
	var created, existing, refused, refusals, namespaces []string
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
			if t, ferr := s.tagSvc().FindTag(name, catID); ferr == nil && !t.Rejected {
				existing = append(existing, s.existingTagNote(name, catID, t))
				continue
			}
			var tag *models.Tag
			if tag, err = s.tagSvc().GetOrCreateTag(name, catID); err == nil {
				created = append(created, tag.Name)
				continue
			}
		}
		refused = append(refused, token+": "+err.Error())
		refusals = append(refusals, err.Error())
	}
	if len(created) == 0 && len(existing) == 0 {
		if len(tokens) > 1 {
			refusals = refused
		}
		externalErr(w, r, strings.Join(refusals, "; "), http.StatusBadRequest)
		return
	}
	var parts []string
	switch len(created) {
	case 0:
	case 1:
		parts = append(parts, "Tag "+created[0]+" created.")
	default:
		parts = append(parts, fmt.Sprintf("Created %d tags: %s.", len(created), strings.Join(created, ", ")))
	}
	parts = append(parts, existing...)
	msg := withNote(strings.Join(parts, " "), unknownPrefixNote(namespaces...))
	if len(refused) > 0 {
		msg += " Refused " + strings.Join(refused, "; ") + "."
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/tags", http.StatusSeeOther)
		return
	}
	// A refresh rather than a redirect keeps the listing's filter.
	kind := "ok"
	if len(refused) > 0 {
		kind = "warn"
	}
	setFlashHeader(w, msg, kind, nil)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) existingTagNote(typed string, catID int64, existing *models.Tag) string {
	if typed = tags.NormalizeTagName(typed); typed != existing.Name || existing.CategoryID != catID {
		return typed + " is an alias of " + s.qualifiedTagName(existing.ID) + "."
	}
	return "Tag " + existing.Name + " already exists."
}

func (s *Server) createAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))
	catID, err := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	name, catID, ns, err := s.typedCategory(strings.TrimSpace(r.FormValue("name")), catID)
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	canon, msg := s.resolveCanonicalTagInput(canonInput, true)
	if msg != "" {
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}

	var alias *models.Tag
	if canon.id != 0 {
		alias, err = s.tagSvc().CreateAlias(name, catID, canon.id)
	} else {
		alias, err = s.tagSvc().CreateAliasToNewTag(name, catID, canon.name, canon.catID)
	}
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	hxDone(w, r, withNote("Alias "+alias.Name+" created, resolving to "+s.qualifiedTagName(*alias.CanonicalTagID)+".", unknownPrefixNote(ns)), "/tags?type=alias&q="+url.QueryEscape(alias.Name), "/tags?type=alias")
}

func (s *Server) addTagAliasPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	rawInput := strings.TrimSpace(r.FormValue("name"))
	if rawInput == "" {
		writeInlineFlash(w, "err", "Alias name is required.")
		return
	}
	catTags, unknownCats, parseErrMsg := s.parseTagInput(rawInput)
	if parseErrMsg != "" {
		writeInlineFlash(w, "err", parseErrMsg)
		return
	}

	added := 0
	var failures []string
	for _, ct := range catTags {
		if _, err := s.tagSvc().CreateAlias(ct.name, ct.catID, id); err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		added++
	}
	switch {
	case len(failures) == 0:
		msg := strconv.Itoa(added) + " " + models.Plural(added, "alias", "aliases") + " created."
		if note := unknownCategoryNote(unknownCats); note != "" {
			msg += " " + note + "."
		}
		hxDone(w, r, msg, "", fmt.Sprintf("/tags/%d", id))
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
}

func (s *Server) removeTagAliasesDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	origin, stale := relationGroupFilter(r)
	byCanonical, err := s.tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		flashHeaderErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	removed := 0
	for _, a := range byCanonical[id] {
		if a.Origin != origin || a.Stale != stale {
			continue
		}
		if err := s.tagSvc().DeleteTag(a.ID); err != nil {
			logx.Warnf("delete alias %d: %v", a.ID, err)
			continue
		}
		removed++
	}
	setFlashHeader(w, strconv.Itoa(removed)+" "+models.Plural(removed, "alias", "aliases")+" removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTagHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tag, _ := s.tagSvc().GetTag(id)
	if err := s.tagSvc().DeleteTag(id); err != nil {
		msg := "Error: " + err.Error()
		if errors.Is(err, tags.ErrTagNotFound) {
			msg = "Tag not found."
		}
		hxRefuse(w, r, http.StatusBadRequest, msg)
		return
	}
	// tag-relations-changed makes the tag page's PTR panel refetch its diff.
	if tag != nil && tag.IsAlias {
		setFlashHeader(w, "Alias removed.", "ok", map[string]any{"tag-relations-changed": ""})
	} else {
		w.Header().Set("HX-Trigger", "tag-relations-changed")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTagsSearchPost(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("reject") == "1" {
		s.startTagScopeRun(w, r, s.runRejectTagsByIDs)
		return
	}
	s.startTagScopeRun(w, r, s.runDeleteTagsByIDs)
}

func (s *Server) runDeleteTagsByIDs(ids []int64) {
	deleted, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "deleting tags…", 50, func(id int64) (bool, error) {
		if err := s.tagSvc().DeleteTag(id); err != nil {
			logx.Warnf("delete tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	summary := skippedSuffix(fmt.Sprintf("deleted %d tag%s", deleted, plural(deleted)), skipped)
	s.finishTagScopeJob(deleted, reasons, cancelled, "delete tags", summary)
}

func (s *Server) renameCollisionOffer(id int64, name string, catID int64) string {
	var clash, canon int64
	if err := s.db().Read.QueryRow(
		`SELECT id, COALESCE(canonical_tag_id, id) FROM tags WHERE name = ? AND category_id = ? AND id != ?`,
		name, catID, id,
	).Scan(&clash, &canon); err != nil {
		return ""
	}
	if canon == id {
		return fmt.Sprintf(`<button type="button" class="btn-sm" data-alias="%d" onclick="dropAliasAndRename(this)">Drop the alias and rename</button>`, clash)
	}
	return fmt.Sprintf(`<button type="button" class="btn-sm" data-id="%d" data-target="%d" onclick="mergeRenameCollision(this)">Merge into %s</button>`,
		id, canon, html.EscapeString(s.qualifiedTagName(canon)))
}

func (s *Server) renameTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	newName, ok := requiredFormExternal(w, r, "name", "Name required.")
	if !ok {
		return
	}
	t, err := s.tagSvc().GetTag(id)
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusNotFound)
		return
	}
	newName, catID, ns, err := s.typedCategory(newName, t.CategoryID)
	if err == nil {
		keep := r.FormValue("keep_alias") == "1"
		switch {
		case catID != t.CategoryID && keep:
			err = s.tagSvc().RenameAndMoveTagKeepAlias(id, newName, catID)
		case catID != t.CategoryID:
			err = s.tagSvc().RenameAndMoveTag(id, newName, catID)
		case keep:
			err = s.tagSvc().RenameTagKeepAlias(id, newName)
		default:
			err = s.tagSvc().RenameTag(id, newName)
		}
	}
	if err != nil {
		var coll *tags.ErrCategoryCollision
		if isHTMXRequest(r) && !t.IsAlias && (errors.Is(err, tags.ErrTagNameTaken) || errors.As(err, &coll)) {
			if offer := s.renameCollisionOffer(id, tags.NormalizeTagName(newName), catID); offer != "" {
				writeInlineFlashHTML(w, "err", html.EscapeString(err.Error())+" "+offer)
				return
			}
		}
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	// HX-Refresh, not a redirect: the listing keeps its filters.
	label := tags.NormalizeTagName(newName)
	if catID != t.CategoryID {
		label = s.qualifiedTagName(id)
	}
	hxDone(w, r, withNote("Renamed to "+label+".", unknownPrefixNote(ns)), "", "/tags")
}
