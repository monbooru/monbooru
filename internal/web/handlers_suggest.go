package web

import (
	"cmp"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type suggestItem struct {
	Name        string
	Color       string
	Description string
	UsageCount  int
	ShowCount   bool
}

func tagSuggestItems(ts []models.Tag, prefix string) []suggestItem {
	items := make([]suggestItem, len(ts))
	for i, t := range ts {
		items[i] = suggestItem{Name: prefix + t.Name, Color: t.CategoryColor, UsageCount: t.UsageCount, ShowCount: true}
	}
	return items
}

// attr and onclick become HTMLAttr unescaped: only ever pass constants.
func (s *Server) renderSuggestList(w http.ResponseWriter, attr, onclick string, items []suggestItem) {
	s.renderTemplate(w, "partials/suggest_list.html", map[string]any{
		"Attr":    template.HTMLAttr(attr),
		"OnClick": template.HTMLAttr(onclick),
		"Items":   items,
	})
}

func (s *Server) renderSearchSuggest(w http.ResponseWriter, rows []suggestItem) {
	s.renderSuggestList(w, `data-tag-name`, `onclick="applySearchSuggest(this.dataset.tagName)"`, rows)
}

func (s *Server) suggestLabels(q db.Querier, logLabel, query string, args ...any) []string {
	out, err := db.QueryStrings(q, query, args...)
	if err != nil {
		logx.Warnf("%s suggest: %v", logLabel, err)
		return nil
	}
	return slices.DeleteFunc(out, func(v string) bool { return v == "" })
}

// The NOCASE range seeks the index where LIKE 'prefix%' would walk all of
// it, and folds case the way the folder: filter does.
func (s *Server) foldersSuggest(w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	var folders []string
	if prefix == "" {
		folders = s.suggestLabels(s.db().Read, "folders",
			`SELECT DISTINCT folder_path FROM images INDEXED BY idx_images_folder_nocase_visible
			 WHERE is_missing = 0 AND folder_path != ''
			 ORDER BY folder_path COLLATE NOCASE LIMIT 10`)
	} else {
		lo, hi := nocasePrefixRange(prefix)
		folders = s.suggestLabels(s.db().Read, "folders",
			`SELECT DISTINCT folder_path FROM images INDEXED BY idx_images_folder_nocase_visible
			 WHERE is_missing = 0
			   AND folder_path >= ? COLLATE NOCASE
			   AND folder_path < ? COLLATE NOCASE
			 ORDER BY folder_path COLLATE NOCASE LIMIT 10`,
			lo, hi)
	}
	if len(folders) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	items := make([]suggestItem, len(folders))
	for i, fp := range folders {
		items[i] = suggestItem{Name: fp}
	}
	s.renderSuggestList(w, `data-folder-path`, `onclick="applyLabelSuggest(this, 'folderPath')"`, items)
}

func (s *Server) collectionSuggest(w http.ResponseWriter, r *http.Request) {
	s.renderLabelSuggest(w, r, s.queryCollectionLabels)
}

func (s *Server) sourceSuggest(w http.ResponseWriter, r *http.Request) {
	s.renderLabelSuggest(w, r, s.querySourceLabels)
}

func (s *Server) renderLabelSuggest(w http.ResponseWriter, r *http.Request, query func(string, int) []string) {
	labels := query(strings.TrimSpace(r.URL.Query().Get("prefix")), 10)
	if len(labels) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	items := make([]suggestItem, len(labels))
	for i, lbl := range labels {
		items[i] = suggestItem{Name: lbl}
	}
	s.renderSuggestList(w, `data-series`, `onclick="applyLabelSuggest(this, 'series')"`, items)
}

// col must be declared NOCASE: the lowered range bounds rely on it.
func (s *Server) queryDistinctLabels(table, col, prefix string, limit int, logLabel string) []string {
	if prefix == "" {
		return s.suggestLabels(s.db().Read, logLabel,
			`SELECT DISTINCT `+col+` FROM `+table+` WHERE `+col+` != ''
			 ORDER BY `+col+` LIMIT ?`, limit)
	}
	lo, hi := nocasePrefixRange(prefix)
	return s.suggestLabels(s.db().Read, logLabel,
		`SELECT DISTINCT `+col+` FROM `+table+`
		 WHERE `+col+` >= ? AND `+col+` < ?
		 ORDER BY `+col+` LIMIT ?`, lo, hi, limit)
}

// The ledger holds a handful of labels at any size; matching them here
// spares a per-keystroke scan of image_tag_sources.
func (s *Server) queryTaggerLabels(prefix string, limit int, auto bool) []string {
	svc := s.tagSvc()
	if svc == nil {
		return nil
	}
	labels, err := svc.UsedByLabels()
	if err != nil {
		logx.Warnf("tagger suggest: %v", err)
		return nil
	}
	if auto {
		autoSet, err := svc.AutoTaggerLabels(labels)
		if err != nil {
			logx.Warnf("tagger suggest: %v", err)
			return nil
		}
		labels = slices.DeleteFunc(labels, func(l string) bool {
			_, ok := autoSet[l]
			return !ok
		})
	}
	return matchLabelPrefix(labels, prefix, limit)
}

// Deduped case-blind: the filters match NOCASE, so PTR and ptr are one row.
func matchLabelPrefix(labels []string, prefix string, limit int) []string {
	low := strings.ToLower(prefix)
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, limit)
	for _, l := range labels {
		folded := strings.ToLower(l)
		if l == "" || !strings.HasPrefix(folded, low) {
			continue
		}
		if _, dup := seen[folded]; dup {
			continue
		}
		seen[folded] = struct{}{}
		out = append(out, l)
		if len(out) == limit {
			break
		}
	}
	return out
}

func (s *Server) querySourceLabels(prefix string, limit int) []string {
	return s.queryDistinctLabels("image_sources", "site", prefix, limit, "source")
}

func (s *Server) queryCollectionLabels(prefix string, limit int) []string {
	return s.queryDistinctLabels("image_collections", "name", prefix, limit, "collection")
}

// Prefix-only, unlike the name: filter, so it seeks the basename_lower
// index instead of scanning the library.
func (s *Server) queryNameBasenames(prefix string, limit int) []string {
	d := s.db()
	if d == nil || prefix == "" {
		return nil
	}
	low := db.ASCIILower(prefix)
	return s.suggestLabels(d.Read, "name",
		`SELECT DISTINCT basename_lower FROM images INDEXED BY idx_images_basename_lower_visible
		 WHERE is_missing = 0 AND basename_lower != ''
		   AND basename_lower >= ? AND basename_lower < ?
		 ORDER BY basename_lower LIMIT ?`,
		low, nextPrefix(low), limit)
}

// A bare value names a node class, so it seeks the node= slice. Two
// characters minimum: a DISTINCT over the whole node= half takes seconds.
func (s *Server) queryComfyTerms(prefix string, limit int) []string {
	d := s.db()
	if d == nil || len(prefix) < 2 {
		return nil
	}
	bare := !strings.ContainsAny(prefix, "=<>")
	seek := prefix
	if bare {
		seek = "node=" + prefix
	}
	lo, hi := search.ComfyTermRange(seek)
	terms := s.suggestLabels(d.Read, "comfyui",
		`SELECT DISTINCT term FROM comfyui_terms
		 WHERE term >= ? COLLATE NOCASE AND term < ? COLLATE NOCASE
		 ORDER BY term LIMIT ?`,
		lo, hi, limit)
	if bare {
		for i, t := range terms {
			terms[i] = strings.TrimPrefix(t, "node=")
		}
	}
	return terms
}

func (s *Server) querySDStringField(sdField, comfyField, prefix string, limit int, substring bool) []string {
	d := s.db()
	if d == nil {
		return nil
	}
	type pair struct{ table, field string }
	tables := []pair{{"sd_metadata", sdField}, {"comfyui_metadata", comfyField}}
	seen := make(map[string]struct{}, limit*2)
	out := make([]string, 0, limit*2)
	for _, t := range tables {
		var values []string
		switch {
		case prefix == "":
			values = s.suggestLabels(d.Read, t.field,
				`SELECT DISTINCT `+t.field+` FROM `+t.table+`
				 WHERE `+t.field+` IS NOT NULL AND `+t.field+` != ''
				 ORDER BY `+t.field+` LIMIT ?`,
				limit)
		case substring:
			values = s.suggestLabels(d.Read, t.field,
				`SELECT DISTINCT `+t.field+` FROM `+t.table+`
				 WHERE `+t.field+` LIKE ? ESCAPE '\'
				 ORDER BY `+t.field+` LIMIT ?`,
				"%"+db.EscapeLike(prefix)+"%", limit)
		default:
			values = s.suggestLabels(d.Read, t.field,
				`SELECT DISTINCT `+t.field+` FROM `+t.table+`
				 WHERE `+t.field+` >= ? AND `+t.field+` < ?
				 ORDER BY `+t.field+` LIMIT ?`,
				prefix, nextPrefix(prefix), limit)
		}
		for _, v := range values {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
		if len(out) >= limit {
			out = out[:limit]
			break
		}
	}
	return out
}

func (s *Server) ambiguousGeneralNames(suggestions []models.Tag) map[string]bool {
	var names []string
	for _, t := range suggestions {
		if t.CategoryName == "general" {
			names = append(names, t.Name)
		}
	}
	out := map[string]bool{}
	if len(names) == 0 {
		return out
	}
	placeholders, args := db.InPlaceholders(names)
	shared, err := db.QueryStrings(s.db().Read,
		`SELECT name FROM tags WHERE name IN (`+placeholders+`)
		 GROUP BY name HAVING COUNT(DISTINCT COALESCE(canonical_tag_id, id)) > 1`, args...)
	if err != nil {
		logx.Warnf("tag suggest: %v", err)
	}
	for _, name := range shared {
		out[name] = true
	}
	return out
}

func (s *Server) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Inputs submit under their own names; the batch-imply one uses target.
	prefix := cmp.Or(q.Get("q"), q.Get("tag"), q.Get("canonical_id"), q.Get("target"), q.Get("name"))
	if prefix == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Only a real category splits: "nier:automata" is one tag name.
	catName, tagPrefix := "", prefix
	if f, err := tags.SplitForm(s.db(), prefix); err == nil && f.CategoryID != 0 {
		catName, tagPrefix = f.Category, f.Name
	}

	suggestions, _ := s.tagSvc().SuggestTags(tagPrefix, catName, 10)
	if q.Has("name") {
		suggestions = slices.DeleteFunc(suggestions, func(t models.Tag) bool { return t.CategoryName == "rating" })
	}

	if catName != "" {
		for i := range suggestions {
			suggestions[i].Name = catName + ":" + suggestions[i].Name
		}
	} else {
		// A canonical input reads a bare name in every category, so a general
		// name that another tag shares is written out in full.
		var ambiguous map[string]bool
		if q.Has("canonical_id") || q.Has("target") {
			ambiguous = s.ambiguousGeneralNames(suggestions)
		}
		for i := range suggestions {
			cat := suggestions[i].CategoryName
			if cat != "" && (cat != "general" || ambiguous[suggestions[i].Name]) {
				suggestions[i].Name = cat + ":" + suggestions[i].Name
			}
		}
	}

	s.renderSuggestList(w, `data-tag-name`, `onclick="applyTagSuggest(this)"`, tagSuggestItems(suggestions, ""))
}

// Beyond this the id list outgrows what a batch POST can usefully carry.
var searchIDsCap = 5000

var errSearchIDsFull = errors.New("search ids: cap reached")

// With ids, the answer is no longer than the ask, so the cap does not apply.
func (s *Server) searchIDs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc := search.NewScope(search.Parse(q.Get("q")), readRatingCookie(r))
	picks := parseIDList(q["ids"])
	if len(picks) > 0 {
		sc = sc.Among(picks)
	}
	ids := []int64{}
	err := sc.Stream(s.source(), func(id int64) error {
		ids = append(ids, id)
		if len(picks) == 0 && len(ids) >= searchIDsCap {
			return errSearchIDsFull
		}
		return nil
	})
	truncated := errors.Is(err, errSearchIDsFull)
	if err != nil && !truncated {
		logx.Errorf("search ids: %v", err)
		http.Error(w, "Search error.", http.StatusInternalServerError)
		return
	}
	contract.WriteJSON(w, http.StatusOK, struct {
		IDs       []int64 `json:"ids"`
		Truncated bool    `json:"truncated,omitempty"`
	}{IDs: ids, Truncated: truncated})
}

func (s *Server) searchSuggest(w http.ResponseWriter, r *http.Request) {
	// htmx can resolve the input's target to the form's #gallery-grid when a
	// grid refresh races the request; pin the dropdown's target here.
	w.Header().Set("HX-Retarget", "#search-suggest")
	w.Header().Set("HX-Reswap", "innerHTML")

	input := r.URL.Query().Get("q")
	words := strings.Fields(input)
	prefix := ""
	var catFilter string
	var contextTokens []string
	if len(words) > 0 {
		last := words[len(words)-1]
		contextTokens = words[:len(words)-1]
		last = strings.TrimPrefix(last, "-")
		if rest, ok := strings.CutPrefix(last, "system:"); ok {
			s.renderSystemSuggest(w, rest)
			return
		}
		if colonIdx := strings.IndexByte(last, ':'); colonIdx >= 0 {
			key := strings.ToLower(last[:colonIdx])
			val := last[colonIdx+1:]
			if searchkw.IsKeyword(key) {
				rows := s.systemSuggestLevel2(key, suggestValuePrefix(key, val))
				if len(rows) == 0 {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				s.renderSearchSuggest(w, rows)
				return
			}
			if colonIdx > 0 && s.categoryExists(key) {
				catFilter = key
				prefix = val
			} else {
				prefix = last
			}
		} else {
			prefix = last
		}
	}
	if prefix == "" && catFilter == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	contextExpr := search.Parse(strings.Join(contextTokens, " "))

	suggestions, _ := search.SuggestTagsWithFilter(s.source(), contextExpr, prefix, catFilter, 10)

	for i := range suggestions {
		if catFilter != "" {
			suggestions[i].Name = catFilter + ":" + suggestions[i].Name
		} else if suggestions[i].CategoryName != "" && suggestions[i].CategoryName != "general" {
			suggestions[i].Name = suggestions[i].CategoryName + ":" + suggestions[i].Name
		}
	}

	if alreadyTyped := alreadyTypedTags(contextTokens); len(alreadyTyped) > 0 {
		out := suggestions[:0]
		for _, sug := range suggestions {
			if _, ok := alreadyTyped[sug.Name]; ok {
				continue
			}
			out = append(out, sug)
		}
		suggestions = out
	}

	s.renderSearchSuggest(w, tagSuggestItems(suggestions, ""))
}

// Free-text values keep their case for the range scans.
func suggestValuePrefix(key, val string) string {
	switch key {
	case "collection", "source", "name", "prompt", "model", "sampler", "comfyui":
		return strings.TrimPrefix(val, `"`)
	case "tagged", "autotagged":
		// expansionRows compares case-sensitively; keep the fold.
		return strings.TrimPrefix(strings.ToLower(val), `"`)
	}
	return strings.ToLower(val)
}

func (s *Server) renderSystemSuggest(w http.ResponseWriter, rest string) {
	var rows []suggestItem
	if colonIdx := strings.IndexByte(rest, ':'); colonIdx >= 0 {
		key := strings.ToLower(rest[:colonIdx])
		rows = s.systemSuggestLevel2(key, suggestValuePrefix(key, rest[colonIdx+1:]))
	} else {
		rows = s.systemSuggestLevel1(strings.ToLower(rest))
	}
	if len(rows) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderSearchSuggest(w, rows)
}

func (s *Server) systemSuggestLevel1(prefix string) []suggestItem {
	var rows []suggestItem
	for _, kw := range searchkw.Keywords {
		if !strings.HasPrefix(kw, prefix) {
			continue
		}
		rows = append(rows, suggestItem{
			Name:        kw + ":",
			Description: searchkw.Descriptions[kw],
		})
	}
	for _, cat := range s.systemCategoryRows() {
		if searchkw.IsKeyword(cat.Name) {
			continue
		}
		if !strings.HasPrefix(cat.Name, prefix) {
			continue
		}
		rows = append(rows, suggestItem{
			Name:        cat.Name + ":",
			Color:       cat.Color,
			Description: "tag category",
		})
	}
	return rows
}

func expansionRows(key, valPrefix string) []suggestItem {
	descs := searchkw.ExpansionDescriptions[key]
	var rows []suggestItem
	for _, exp := range searchkw.Expansions[key] {
		if !strings.HasPrefix(exp, valPrefix) {
			continue
		}
		rows = append(rows, suggestItem{
			Name:        key + ":" + exp,
			Description: descs[exp],
		})
	}
	return rows
}

// Quoted so a multi-word label stays one parser token.
func quotedSDLabelRows(key string, labels []string) []suggestItem {
	rows := make([]suggestItem, 0, len(labels))
	for _, lbl := range labels {
		rows = append(rows, suggestItem{
			Name: key + `:"` + search.QuoteValue(lbl) + `"`,
		})
	}
	return rows
}

func (s *Server) systemSuggestLevel2(key, valPrefix string) []suggestItem {
	switch key {
	case "cat":
		return s.categoryValueRows(valPrefix, func(cat systemCategoryRow) suggestItem {
			return suggestItem{Name: "cat:" + cat.Name, Color: cat.Color}
		})
	case "tagcount":
		return append(expansionRows(key, valPrefix), s.categoryValueRows(valPrefix, func(cat systemCategoryRow) suggestItem {
			return suggestItem{Name: "tagcount:" + cat.Name + "=", Color: cat.Color, Description: "number of " + cat.Name + " tags"}
		})...)
	case "collection":
		return quotedSDLabelRows("collection", s.queryCollectionLabels(valPrefix, 10))
	case "source":
		return append(expansionRows(key, valPrefix),
			quotedSDLabelRows("source", s.querySourceLabels(valPrefix, 10))...)
	// A label repeating an expansion (user) keeps only the expansion row.
	case "tagged", "autotagged":
		labels := s.queryTaggerLabels(valPrefix, 10, key == "autotagged")
		labels = slices.DeleteFunc(labels, func(l string) bool {
			return slices.Contains(searchkw.Expansions[key], strings.ToLower(l))
		})
		return append(expansionRows(key, valPrefix), quotedSDLabelRows(key, labels)...)
	case "name":
		if valPrefix == "" {
			return nil
		}
		return quotedSDLabelRows("name", s.queryNameBasenames(valPrefix, 10))
	case "model":
		return quotedSDLabelRows("model", s.querySDStringField("model", "model_checkpoint", valPrefix, 10, false))
	case "comfyui":
		return quotedSDLabelRows("comfyui", s.queryComfyTerms(valPrefix, 10))
	case "sampler":
		return quotedSDLabelRows("sampler", s.querySDStringField("sampler", "sampler", valPrefix, 10, false))
	// Empty prefix lists nothing: a DISTINCT over prompts scans every row.
	case "prompt":
		if valPrefix == "" {
			return nil
		}
		return quotedSDLabelRows("prompt", s.querySDStringField("prompt", "prompt", valPrefix, 10, true))
	}
	if _, ok := searchkw.Expansions[key]; ok {
		return expansionRows(key, valPrefix)
	}
	if searchkw.IsKeyword(key) {
		return nil
	}
	if s.categoryExists(key) {
		suggestions, _ := search.SuggestTagsWithFilter(s.source(), nil, valPrefix, key, 10)
		return tagSuggestItems(suggestions, key+":")
	}
	return nil
}

type systemCategoryRow struct {
	Name  string
	Color string
}

func (s *Server) categoryValueRows(prefix string, row func(systemCategoryRow) suggestItem) []suggestItem {
	var rows []suggestItem
	for _, cat := range s.systemCategoryRows() {
		if strings.HasPrefix(cat.Name, prefix) {
			rows = append(rows, row(cat))
		}
	}
	return rows
}

// A small table: filtering in Go spares a LIKE and escaping its underscores.
func (s *Server) systemCategoryRows() []systemCategoryRow {
	d := s.db()
	if d == nil {
		return nil
	}
	out, err := db.QueryAll(d.Read, func(rows *sql.Rows) (systemCategoryRow, error) {
		var name, color string
		err := rows.Scan(&name, &color)
		return systemCategoryRow{Name: name, Color: tags.SafeCategoryColor(color)}, err
	}, `SELECT name, color FROM tag_categories ORDER BY position, id`)
	if err != nil {
		logx.Warnf("system category rows: %v", err)
	}
	return out
}

func alreadyTypedTags(contextTokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(contextTokens))
	for _, tok := range contextTokens {
		t := strings.TrimPrefix(tok, "-")
		if t == "" {
			continue
		}
		if colonIdx := strings.IndexByte(t, ':'); colonIdx > 0 {
			if searchkw.IsKeyword(strings.ToLower(t[:colonIdx])) {
				continue
			}
		}
		set[t] = struct{}{}
	}
	return set
}

// Exclusive upper bound of prefix's BINARY range, for a >=, < scan in
// place of LIKE 'prefix%'.
func nextPrefix(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1])
		}
	}
	return prefix + "\xff"
}

// Lowercased first: NOCASE folds to lower case, so a bound built from "Z"
// would exclude every lower-case continuation.
func nocasePrefixRange(prefix string) (lo, hi string) {
	lo = db.ASCIILower(prefix)
	return lo, nextPrefix(lo)
}
