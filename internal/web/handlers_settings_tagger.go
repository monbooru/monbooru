package web

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// Install snippets are built in Go so the template never does shell quoting.
type taggerRow struct {
	Name                string
	Description         string
	Available           bool
	Reason              string
	Malformed           bool
	Enabled             bool
	ConfidenceThreshold float64
	ConfigSummary       string
	Differs             bool
	Installed           bool
	Supported           bool
	Gated               bool
	HostCommand         string
	// The browser fetches these itself, which keeps the server's
	// no-outbound-request promise.
	Files     []tagger.CatalogFile
	TargetDir string
}

func (s *Server) installedTaggerRow(t tagger.TaggerStatus, totalGalleries int, modelPath string, entry *tagger.CatalogEntry) taggerRow {
	ruleCount := len(tagger.OverlayDispatchRules(modelPath, t.Name))
	return taggerRow{
		Name:                t.Name,
		Available:           t.Available,
		Reason:              t.Reason,
		Malformed:           t.Malformed,
		Enabled:             t.Enabled,
		ConfidenceThreshold: t.ConfidenceThreshold,
		ConfigSummary:       taggerConfigSummary(t.TaggerInstance, totalGalleries, ruleCount),
		Differs:             taggerDiffersFromStock(t.TaggerInstance, modelPath, entry),
		Installed:           true,
	}
}

func taggerConfigSummary(inst config.TaggerInstance, totalGalleries, ruleCount int) string {
	out := taggerThresholdSummary(inst.ConfidenceThreshold, inst.CategoryThresholds, inst.DisabledCategories)
	if inst.Galleries != nil && len(inst.Galleries) != totalGalleries {
		if len(inst.Galleries) == 0 {
			out += ", no galleries"
		} else {
			out += ", galleries: " + strings.Join(inst.Galleries, ", ")
		}
	}
	switch {
	case ruleCount == 1:
		out += ", 1 rule"
	case ruleCount > 1:
		out += fmt.Sprintf(", %d rules", ruleCount)
	}
	return out
}

func taggerDiffersFromStock(inst config.TaggerInstance, modelPath string, entry *tagger.CatalogEntry) bool {
	seed := tagger.SeedTaggerInstance(inst.Name, inst.Enabled, entry)
	if inst.ConfidenceThreshold != seed.ConfidenceThreshold {
		return true
	}
	if !maps.Equal(inst.CategoryThresholds, seed.CategoryThresholds) {
		return true
	}
	if !maps.Equal(inst.PerCategoryTopK, seed.PerCategoryTopK) {
		return true
	}
	if !slices.Equal(slices.Sorted(slices.Values(inst.DisabledCategories)), slices.Sorted(slices.Values(seed.DisabledCategories))) {
		return true
	}
	if inst.Galleries != nil {
		return true
	}
	// An overlay that cannot be read differs too: Reset is how it goes.
	_, err := os.Stat(filepath.Join(modelPath, inst.Name, "dispatch.json"))
	return !errors.Is(err, fs.ErrNotExist)
}

type thresholdRow struct {
	Category         string
	Override         string
	MaxTags          string
	MaxDefault       int
	Color            string
	Disabled         bool
	ViaRules         bool // reached only through dispatch rules, not the model
	DefaultThreshold string
	DefaultMaxTags   string
}

type taggerGalleryRow struct {
	Name    string
	Checked bool
}

func (s *Server) settingsTaggerPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	newProvider := cmp.Or(strings.ToLower(strings.TrimSpace(r.FormValue("execution_provider"))), "cpu")
	if s.envPinned("tagger.execution_provider") != "" {
		newProvider = s.executionProvider()
	}
	if !config.IsValidExecutionProvider(newProvider) {
		writeInlineFlash(w, "err", "Invalid execution provider: "+newProvider)
		return
	}
	// ORT env init is not re-entrant, so no probe while a job runs; the lane
	// stays held through the release so no job starts on the old session.
	if newProvider != s.executionProvider() {
		res, err := s.jobs.Reserve()
		if err != nil {
			writeInlineFlash(w, "err", "A job is running; try again when it finishes.")
			return
		}
		defer res.Release()
		if newProvider != "cpu" {
			// SA4023: the stub build's probe always errors, the tagger build's
			// does not, and staticcheck only ever sees the stub.
			if err := tagger.CheckProviderAvailable(newProvider); err != nil { //nolint:staticcheck
				writeInlineFlash(w, "err", "Cannot enable "+newProvider+": "+err.Error())
				return
			}
		}
	}

	var providerChanged bool
	if err := s.withConfig(func(c *config.Config) error {
		providerChanged = c.Tagger.ExecutionProvider != newProvider
		c.Tagger.ExecutionProvider = newProvider
		if n, err := strconv.Atoi(r.FormValue("parallel")); err == nil && n >= 1 {
			c.Tagger.Parallel = n
		}
		if n, err := strconv.Atoi(r.FormValue("idle_release_after_minutes")); err == nil && n >= 0 {
			c.Tagger.IdleReleaseAfterMinutes = n
		}
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	// The old provider's session would otherwise hold its memory until
	// the idle release.
	if providerChanged {
		tagger.ReleaseAll()
	}
	logx.Infof("settings: tagger updated (execution_provider=%s)", newProvider)
	writeInlineFlash(w, "ok", "Saved.")
	s.renderTemplate(w, "partials/tagger_mode_badge.html", map[string]any{
		"Provider": newProvider,
		"OOB":      true,
	})
}

func (s *Server) settingsTaggerEnablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), true)
}

func (s *Server) settingsTaggerDisablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), false)
}

func (s *Server) updateTagger(name string, mutate func(*config.TaggerInstance)) error {
	modelPath := s.modelPath()
	return s.withConfig(func(c *config.Config) error {
		if i := slices.IndexFunc(c.Tagger.Taggers, func(t config.TaggerInstance) bool { return t.Name == name }); i >= 0 {
			mutate(&c.Tagger.Taggers[i])
		} else {
			catalog := catalogEntryByName(modelPath, name)
			seeded := tagger.SeedTaggerInstance(name, false, catalog)
			mutate(&seeded)
			c.Tagger.Taggers = append(c.Tagger.Taggers, seeded)
		}
		return nil
	})
}

func (s *Server) applyTaggerEnabled(w http.ResponseWriter, name string, enabled bool) {
	if err := tagger.ValidateTaggerName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.Enabled = enabled
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	logx.Infof("settings: tagger %q %s", name, verb)
	// A header flash: the refresh discards an inline body before it paints.
	setFlashHeader(w, "Tagger "+name+" "+verb+".", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsTaggerConfigGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	rows, global := s.thresholdDialogData(inst)
	galRows, allChecked := s.galleryDialogData(inst)
	modelPath := s.modelPath()
	catIDs, catList := s.categoryChoices()
	labelCount := 0
	if views, err := tagger.BrowseLabels(modelPath, name, taggerTagsFile(inst), catIDs); err == nil {
		labelCount = len(views)
	}

	export, overlayErr := taggerExport(modelPath, name, inst)
	data := map[string]any{
		"Name":       name,
		"Global":     global,
		"Rows":       rows,
		"GalRows":    galRows,
		"AllChecked": allChecked,
		"Categories": catList,
		"LabelCount": labelCount,
		"OverlayErr": overlayErr,
	}
	maps.Copy(data, export)
	s.renderTemplate(w, "partials/tagger_config_dialog.html", data)
}

func taggerExport(modelPath, name string, inst config.TaggerInstance) (map[string]any, error) {
	embedded := tagger.DefaultDispatchRules(modelPath, name)
	overlay, overlayErr := tagger.ReadDispatchOverlay(modelPath, name)
	merged := tagger.MergedDispatchRules(modelPath, name)
	exportJSON := ""
	if b, err := tagger.MarshalDispatchDoc(merged); err == nil {
		exportJSON = string(b) + "\n"
	}
	profileJSON, profileLines := "", []exportLine(nil)
	sidecarFields := tagger.SidecarProfileFields(modelPath, name)
	if profile, err := tagger.ResolveProfile(modelPath, name, taggerTagsFile(inst)); err == nil {
		if b, err := tagger.ProfileExportDoc(profile); err == nil {
			profileJSON = string(b) + "\n"
			for _, line := range strings.Split(string(b), "\n") {
				mark := ""
				trimmed := strings.TrimSpace(line)
				if i := strings.Index(trimmed, `":`); strings.HasPrefix(trimmed, `"`) && i > 0 && slices.Contains(sidecarFields, trimmed[1:i]) {
					mark = "add"
				}
				profileLines = append(profileLines, exportLine{Text: line, Mark: mark})
			}
		}
	}
	return map[string]any{
		"Name":         name,
		"CustomRules":  len(overlay),
		"ExportRules":  exportRuleLines(embedded, overlay),
		"ExportJSON":   exportJSON,
		"RuleCount":    len(merged),
		"ProfileLines": profileLines,
		"ProfileJSON":  profileJSON,
		"ProfileStock": len(sidecarFields) == 0,
	}, overlayErr
}

type exportLine struct {
	Text string
	Mark string
}

func exportRuleLines(embedded, overlay []tagger.DispatchEntry) []exportLine {
	embByte := map[string]tagger.DispatchEntry{}
	for _, e := range embedded {
		embByte[e.Source] = e
	}
	ovr := map[string]tagger.DispatchEntry{}
	for _, e := range overlay {
		ovr[e.Source] = e
	}
	union := maps.Clone(embByte)
	maps.Copy(union, ovr)
	sources := slices.Sorted(maps.Keys(union))

	ruleText := func(e tagger.DispatchEntry) string {
		b, _ := json.Marshal(e)
		return "    " + string(b) + ","
	}
	lines := []exportLine{{Text: "{"}, {Text: `  "version": 1,`}, {Text: `  "rules": [`}}
	var run []tagger.DispatchEntry
	flushRun := func() {
		if len(run) <= 5 {
			for _, e := range run {
				lines = append(lines, exportLine{Text: ruleText(e)})
			}
		} else {
			lines = append(lines, exportLine{Text: fmt.Sprintf("    ... %d default rules ...", len(run)), Mark: "gap"})
		}
		run = nil
	}
	for _, src := range sources {
		o, hasOverlay := ovr[src]
		e, hasEmbedded := embByte[src]
		if !hasOverlay {
			run = append(run, e)
			continue
		}
		flushRun()
		if hasEmbedded {
			lines = append(lines, exportLine{Text: ruleText(e), Mark: "del"})
		}
		lines = append(lines, exportLine{Text: ruleText(o), Mark: "add"})
	}
	flushRun()
	lines = append(lines, exportLine{Text: "  ]"}, exportLine{Text: "}"})
	return lines
}

type taggerLabelRow struct {
	Source      string
	CatName     string
	TagName     string
	Color       string
	Muted       bool
	CategoryOff bool
	Rule        string
}

// Growing stops at taggerLabelMaxRows: past it the table costs more to
// render than a search costs to type.
const (
	taggerLabelPageSize = 50
	taggerLabelMaxRows  = 500
)

func (s *Server) settingsTaggerLabelsGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	s.renderTaggerLabels(w, r, inst)
}

func (s *Server) renderTaggerLabels(w http.ResponseWriter, r *http.Request, inst config.TaggerInstance) {
	name := inst.Name
	modelPath := s.modelPath()
	catIDs, _ := s.categoryChoices()
	colors := s.categoryColors()
	views, err := tagger.BrowseLabels(modelPath, name, taggerTagsFile(inst), catIDs)
	custom := len(tagger.OverlayDispatchRules(modelPath, name))
	if err != nil {
		s.renderTemplate(w, "partials/tagger_labels_rows.html", map[string]any{
			"Name":        name,
			"CustomRules": custom,
			"Err":         "Cannot read the label file: " + err.Error(),
		})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.FormValue("q")))
	filter := r.FormValue("filter")
	limit := taggerLabelPageSize
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > limit {
		limit = min(n, taggerLabelMaxRows)
	}
	disabled := map[string]bool{}
	for _, cat := range inst.DisabledCategories {
		disabled[cat] = true
	}
	var rows []taggerLabelRow
	more := 0
	for _, v := range views {
		if q != "" && !strings.Contains(strings.ToLower(v.Source), q) && !strings.Contains(strings.ToLower(v.TagName), q) {
			continue
		}
		catOff := !v.Muted && disabled[v.CatName]
		switch filter {
		case "customized":
			if v.Rule != "custom" {
				continue
			}
		case "muted":
			if !v.Muted && !catOff {
				continue
			}
		}
		if len(rows) >= limit {
			more++
			continue
		}
		rows = append(rows, taggerLabelRow{
			Source:      v.Source,
			CatName:     v.CatName,
			TagName:     v.TagName,
			Color:       colors[v.CatName],
			Muted:       v.Muted,
			CategoryOff: catOff,
			Rule:        v.Rule,
		})
	}
	s.renderTemplate(w, "partials/tagger_labels_rows.html", map[string]any{
		"Name":        name,
		"Rows":        rows,
		"More":        more,
		"NextLimit":   limit + taggerLabelPageSize,
		"CanGrow":     limit < taggerLabelMaxRows,
		"CustomRules": custom,
	})
}

func taggerTagsFile(inst config.TaggerInstance) string {
	return cmp.Or(inst.TagsFile, tagger.DefaultTagsFile)
}

func (s *Server) categoryChoices() (map[string]int64, []string) {
	ids := map[string]int64{}
	var names []string
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		return ids, names
	}
	for _, c := range cats {
		ids[c.Name] = c.ID
		names = append(names, c.Name)
	}
	return ids, names
}

// Every row posts its category, so an absent Enable box means muted.
func parseThresholdForm(r *http.Request) (global float64, overrides map[string]float64, topK map[string]int, disabled []string, errMsg string) {
	globalRaw := strings.TrimSpace(r.FormValue("global_threshold"))
	global, err := strconv.ParseFloat(globalRaw, 64)
	if err != nil || math.IsNaN(global) || global < 0 || global > 1 {
		return 0, nil, nil, nil, "Global threshold must be between 0 and 1."
	}
	overrides = map[string]float64{}
	topK = map[string]int{}
	for _, cat := range r.Form["category"] {
		cat = strings.TrimSpace(cat)
		if cat == "" {
			continue
		}
		raw := strings.TrimSpace(r.FormValue("threshold_" + cat))
		if raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(v) || v < 0 || v > 1 {
				return 0, nil, nil, nil, "Threshold for " + cat + " must be between 0 and 1."
			}
			overrides[cat] = v
		}
		rawK := strings.TrimSpace(r.FormValue("maxtags_" + cat))
		if rawK != "" {
			n, err := strconv.Atoi(rawK)
			if err != nil || n < 0 {
				return 0, nil, nil, nil, "Max tags for " + cat + " must be 0 or higher."
			}
			topK[cat] = n
		}
		if r.FormValue("enable_"+cat) == "" {
			disabled = append(disabled, cat)
		}
	}
	return global, overrides, topK, disabled, ""
}

// nil means every gallery; a non-nil empty slice means none and must stay
// non-nil so the TOML keeps galleries = [].
func (s *Server) parseGalleriesForm(r *http.Request) []string {
	if r.FormValue("all") == "on" {
		return nil
	}
	galleries := []string{}
	known := s.galleries()
	for _, n := range r.Form["gallery_names"] {
		n = strings.TrimSpace(n)
		if n == "" || !slices.ContainsFunc(known, func(g config.Gallery) bool { return g.Name == n }) {
			continue
		}
		galleries = append(galleries, n)
	}
	return galleries
}

func (s *Server) settingsTaggerMappingPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	modelPath := s.modelPath()
	if errMsg := s.applyMappingRule(name, modelPath, r); errMsg != "" {
		w.Header().Set("HX-Retarget", "#flash-tagger-config-"+name)
		writeInlineFlash(w, "err", errMsg)
		return
	}
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		w.Header().Set("HX-Retarget", "#flash-tagger-config-"+name)
		writeInlineFlash(w, "err", "Tagger "+name+" is not installed any more.")
		return
	}
	writeFlashOOB(w, "flash-tagger-config-"+name, "", "")
	s.renderTaggerLabels(w, r, inst)
	export, _ := taggerExport(modelPath, name, inst)
	export["OOB"] = true
	s.renderTemplate(w, "tagger_export_panel", export)
	summary := taggerConfigSummary(inst, len(s.galleries()), len(tagger.OverlayDispatchRules(modelPath, name)))
	_, _ = fmt.Fprintf(w, `<span id="tagger-config-summary-%s" class="field-hint" hx-swap-oob="true">%s</span>`, name, html.EscapeString(summary))
	s.renderTemplate(w, "tagger_reset_button", map[string]any{"Name": name, "Differs": taggerDiffersFromStock(inst, modelPath, catalogEntryByName(modelPath, inst.Name)), "OOB": true})
}

func (s *Server) applyMappingRule(name, modelPath string, r *http.Request) (errMsg string) {
	source := strings.TrimSpace(r.FormValue("rule_source"))
	if source == "" {
		return "Malformed mapping rule."
	}
	// Validated before the overlay lock, so the locked section is only
	// read, swap, write.
	reset := r.FormValue("rule_reset") != ""
	entry := tagger.DispatchEntry{Source: source}
	if !reset && r.FormValue("rule_mute") == "" {
		category := r.FormValue("rule_category")
		catIDs, _ := s.categoryChoices()
		if _, ok := catIDs[category]; !ok {
			return "Unknown category " + category + " for label " + source + "."
		}
		rename := ""
		if n := strings.TrimSpace(r.FormValue("rule_name")); n != "" && n != source {
			valid, err := tags.ValidateTagName(n)
			if err != nil {
				return "Invalid rename for label " + source + ": " + err.Error()
			}
			rename = valid
		}
		if target := cmp.Or(rename, source); category == "rating" && !tags.IsCanonicalRating(target) {
			last := len(contract.RatingLevels) - 1
			return "A rating label must become " + strings.Join(contract.RatingLevels[:last], ", ") + " or " + contract.RatingLevels[last] + ", not " + target + "."
		}
		entry = tagger.DispatchEntry{Source: source, Category: category, Name: rename}
	}
	if err := tagger.UpdateDispatchOverlay(modelPath, name, func(overlay map[string]tagger.DispatchEntry) error {
		if reset {
			delete(overlay, source)
		} else {
			overlay[source] = entry
		}
		return nil
	}); err != nil {
		return "Could not save dispatch.json: " + err.Error()
	}
	logx.Infof("settings: tagger %q mapping rule for %q updated", name, source)
	return ""
}

func (s *Server) settingsTaggerConfigPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	global, overrides, topK, disabled, errMsg := parseThresholdForm(r)
	if errMsg != "" {
		writeInlineFlash(w, "err", errMsg)
		return
	}
	galleries := s.parseGalleriesForm(r)
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = global
		if len(overrides) > 0 {
			t.CategoryThresholds = overrides
		} else {
			t.CategoryThresholds = nil
		}
		if len(topK) > 0 {
			t.PerCategoryTopK = topK
		} else {
			t.PerCategoryTopK = nil
		}
		t.DisabledCategories = disabled
		t.Galleries = galleries
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q config updated (global=%.2f, %d threshold overrides, %d top-K overrides, %d disabled, all_galleries=%t)",
		name, global, len(overrides), len(topK), len(disabled), galleries == nil)
	setFlashHeader(w, "Tagger "+name+" configuration saved.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsTaggerResetPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(s.modelPath(), name))
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = defaults.ConfidenceThreshold
		t.CategoryThresholds = defaults.CategoryThresholds
		t.PerCategoryTopK = defaults.PerCategoryTopK
		t.DisabledCategories = defaults.DisabledCategories
		t.Galleries = nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	overlay := filepath.Join(s.modelPath(), name, "dispatch.json")
	if err := os.Remove(overlay); err != nil && !os.IsNotExist(err) {
		logx.Warnf("reset tagger %q: remove %q: %v", name, overlay, err)
		writeInlineFlash(w, "err", "Reset saved but could not delete dispatch.json: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q reset to stock", name)
	setFlashHeader(w, "Tagger "+name+" reset to stock.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

// Through discovery: only it fills ModelFile and TagsFile from disk, and
// a seeded entry assumes tags.csv, which some taggers do not ship.
func (s *Server) resolveTaggerInstance(name string) (config.TaggerInstance, bool) {
	for _, t := range tagger.DiscoverTaggers(s.cfgSnapshot()) {
		if t.Name == name {
			return t.TaggerInstance, true
		}
	}
	return config.TaggerInstance{}, false
}

// Every gallery category is listed, so one no tagger reaches yet can be
// tuned before a rule routes into it.
func (s *Server) thresholdDialogData(inst config.TaggerInstance) (rows []thresholdRow, global float64) {
	name := inst.Name
	modelPath := s.modelPath()
	global = inst.ConfidenceThreshold

	emit := tagger.EmittedCategories(modelPath, name, taggerTagsFile(inst))

	colors := s.categoryColors()

	// Seeded as the tagger Reset seeds, so a category's Reset lands on
	// the same values.
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(modelPath, name))

	seen := map[string]bool{}
	appendRow := func(cat string, viaRules bool) {
		if seen[cat] {
			return
		}
		seen[cat] = true
		rows = append(rows, thresholdRow{
			Category:         cat,
			Override:         formatOverride(inst.CategoryThresholds, cat),
			MaxTags:          formatTopKOverride(inst.PerCategoryTopK, cat),
			MaxDefault:       tagger.ResolveTopK(nil, cat),
			Color:            colors[cat],
			Disabled:         slices.Contains(inst.DisabledCategories, cat),
			ViaRules:         viaRules,
			DefaultThreshold: formatOverride(defaults.CategoryThresholds, cat),
			DefaultMaxTags:   formatTopKOverride(defaults.PerCategoryTopK, cat),
		})
	}
	appendSorted := func(cats []string, viaRules bool) {
		for _, cat := range slices.Sorted(slices.Values(cats)) {
			appendRow(cat, viaRules)
		}
	}
	appendSorted(emit, false)
	var dispatchTargets []string
	for _, cat := range tagger.DispatchTargetCategories(modelPath, name) {
		if _, ok := colors[cat]; ok {
			dispatchTargets = append(dispatchTargets, cat)
		}
	}
	appendSorted(dispatchTargets, true)
	appendSorted(slices.Collect(maps.Keys(colors)), false)
	// Overrides for categories the gallery no longer has still render, so
	// they can be cleared.
	appendSorted(slices.Concat(
		slices.Collect(maps.Keys(inst.CategoryThresholds)),
		slices.Collect(maps.Keys(inst.PerCategoryTopK)),
		inst.DisabledCategories,
	), false)
	return rows, global
}

func formatOverride(m map[string]float64, key string) string {
	if v, ok := m[key]; ok {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return ""
}

// An explicit zero stays "0": it is the operator's opt-out of the cap.
func formatTopKOverride(m map[string]int, key string) string {
	if v, ok := m[key]; ok {
		return strconv.Itoa(v)
	}
	return ""
}

func taggerThresholdSummary(global float64, overrides map[string]float64, disabled []string) string {
	out := fmt.Sprintf("global %.2f", global)
	for _, k := range slices.Sorted(maps.Keys(overrides)) {
		out += fmt.Sprintf(", %s %.2f", k, overrides[k])
	}
	if len(disabled) > 0 {
		out += " (disabled: " + strings.Join(slices.Sorted(slices.Values(disabled)), ", ") + ")"
	}
	return out
}

func (s *Server) galleryDialogData(inst config.TaggerInstance) (rows []taggerGalleryRow, allChecked bool) {
	galleries := s.galleries()
	allChecked = inst.Galleries == nil
	for _, g := range galleries {
		rows = append(rows, taggerGalleryRow{
			Name:    g.Name,
			Checked: allChecked || slices.Contains(inst.Galleries, g.Name),
		})
	}
	return rows, allChecked
}

func catalogEntryByName(modelPath, name string) *tagger.CatalogEntry {
	return catalogEntry(tagger.LoadCatalog(modelPath), name)
}

func catalogEntry(catalog []tagger.CatalogEntry, name string) *tagger.CatalogEntry {
	for _, e := range catalog {
		if e.Name == name {
			return &e
		}
	}
	return nil
}

// Persisted, so a re-downloaded model has to be re-enabled deliberately.
// An unmounted model_path would read as every file gone, so it disables
// nothing.
func (s *Server) reconcileTaggers(catalog []tagger.CatalogEntry) {
	cfg := s.cfgSnapshot()
	fi, err := os.Stat(cfg.Paths.ModelPath)
	mounted := err == nil && fi.IsDir()
	known := func(c *config.Config, name string) bool {
		return slices.ContainsFunc(c.Tagger.Taggers, func(t config.TaggerInstance) bool { return t.Name == name })
	}
	missing := map[string]bool{}
	var fresh []tagger.TaggerStatus
	for _, d := range tagger.DiscoverTaggers(cfg) {
		missing[d.Name] = mounted && d.Missing
		if d.Available && !known(cfg, d.Name) {
			fresh = append(fresh, d)
		}
	}
	gone := func(t config.TaggerInstance) bool { return t.Enabled && missing[t.Name] }
	if len(fresh) == 0 && !slices.ContainsFunc(cfg.Tagger.Taggers, gone) {
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		for i, t := range c.Tagger.Taggers {
			if gone(t) {
				c.Tagger.Taggers[i].Enabled = false
				logx.Infof("settings: auto-disabled tagger %q (files missing)", t.Name)
			}
		}
		for _, d := range fresh {
			if known(c, d.Name) {
				continue
			}
			c.Tagger.Taggers = append(c.Tagger.Taggers, tagger.SeedTaggerInstance(d.Name, true, catalogEntry(catalog, d.Name)))
			logx.Infof("settings: auto-enabled discovered tagger %q", d.Name)
		}
		return nil
	}); err != nil {
		logx.Warnf("reconcile taggers: save config: %v", err)
	}
}

func (s *Server) settingsTaggerDeletePost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	s.cfgMu.RLock()
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name && t.Enabled {
			s.cfgMu.RUnlock()
			writeInlineFlash(w, "err", "Disable tagger "+name+" before deleting it.")
			return
		}
	}
	// Not through modelPath: a second RLock deadlocks behind a waiting
	// writer.
	dir := filepath.Join(s.cfg.Paths.ModelPath, name)
	s.cfgMu.RUnlock()
	// A running job may still name this tagger for its next chunk, and the
	// release below would kill the worker under it.
	res, err := s.jobs.Reserve()
	if err != nil {
		writeInlineFlash(w, "err", "A job is running; try again when it finishes.")
		return
	}
	defer res.Release()
	// The folder goes first: an entry dropped before a failed removal
	// leaves memory and the TOML disagreeing.
	if err := os.RemoveAll(dir); err != nil {
		logx.Warnf("delete tagger %q: remove %q: %v", name, dir, err)
		writeInlineFlash(w, "err", "Could not delete the tagger folder: "+err.Error())
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Tagger.Taggers = slices.DeleteFunc(c.Tagger.Taggers, func(t config.TaggerInstance) bool { return t.Name == name })
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	// The deleted tagger's session would otherwise hold its memory until
	// the idle release.
	tagger.ReleaseAll()
	logx.Infof("settings: tagger %q deleted (folder %s removed)", name, dir)
	setFlashHeader(w, "Tagger "+name+" deleted.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}
