package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// A part is a run of text in one colour: a class of the page's tokens,
// or a category's own colour.
type pathPart struct {
	Text  string
	Class string
	Color string
}

// A box's state: "" passes, stop ends the lane on ×, away ends it where a
// rule sends the name elsewhere, cond passes a rule that only sometimes
// stops it. Shared step-3 boxes of neighbouring lanes draw as one, the
// global setting they all read.
type pathBox struct {
	Parts  []pathPart
	State  string
	Href   string
	Right  string
	Note   string
	Shared bool
}

// A lane's kind decides its columns: site runs the three monloader steps,
// tagger the tagger's mappings and categories, span one box on the
// monbooru side, any the quiet spellings, implies a line straight to the
// gallery's step 6, note a line of text.
type pathLane struct {
	Kind      string
	Name      string
	NameClass string
	NameColor string
	NameHref  string
	Site      string
	LineNote  string
	Steps     [3]*pathBox
	Span      *pathBox
}

type pathBand struct {
	Label, Count string
	Lanes        []pathLane
}

// AliasInto names a tag the line offers to alias into this one; Focus is
// the input an add line puts the caret in.
type pathLine struct {
	Parts     []pathPart
	Href      string
	Action    string
	Focus     string
	AliasInto int64
	AliasName string
}

type pathGallery struct {
	Title string
	State string
	Lines []pathLine
	Href  string
}

type tagPath struct {
	Monloader   string
	Taggers     int
	GalleryName string
	Category    string
	CatColor    string
	Bands       []pathBand
	Gallery     [3]pathGallery
	Image       pathGallery
}

func part(text, class string) pathPart { return pathPart{Text: text, Class: class} }

// A row is a lane read across, for the narrow list that stands in for the
// drawing; a band row carries only its heading.
type pathRow struct {
	Band, Count    string
	Name, Mid, End template.HTML
	Note           string
	NoteClass      string
}

func partsHTML(parts []pathPart) template.HTML {
	var b strings.Builder
	for _, pt := range parts {
		if pt.Color != "" {
			fmt.Fprintf(&b, `<span style="color:%s">%s</span>`, pt.Color, html.EscapeString(pt.Text))
		} else {
			fmt.Fprintf(&b, `<span class="tl-%s">%s</span>`, pt.Class, html.EscapeString(pt.Text))
		}
	}
	return template.HTML(b.String())
}

// Settings, here or in monloader, open beside the tag page; its own
// sections and other tags open in place.
func newTab(href string) bool {
	return strings.HasPrefix(href, "/settings") || !strings.HasPrefix(href, "#") && !strings.HasPrefix(href, "/")
}

func linked(href string, inner template.HTML) template.HTML {
	if href == "" {
		return inner
	}
	target := ""
	if newTab(href) {
		target = ` target="_blank" rel="noopener"`
	}
	return template.HTML(fmt.Sprintf(`<a href="%s"%s>%s</a>`, html.EscapeString(href), target, inner))
}

var stateMark = map[string][]pathPart{"stop": {part(" ×", "err")}, "cond": {part(" ?", "warn")}}

func (p tagPath) rows() []pathRow {
	var rows []pathRow
	for _, band := range p.Bands {
		rows = append(rows, pathRow{Band: band.Label, Count: band.Count})
		for _, l := range band.Lanes {
			row := pathRow{Name: linked(l.NameHref, partsHTML([]pathPart{{Text: l.Name, Class: l.NameClass, Color: l.NameColor}}))}
			switch l.Kind {
			case "site", "tagger":
				first := l.Steps[0]
				var num []pathPart
				if first.State != "off" {
					num = []pathPart{part("1 ", "dim")}
				}
				row.Mid = linked(first.Href, partsHTML(slices.Concat(num, first.Parts, stateMark[first.State])))
				var end []pathPart
				href := ""
				for i, st := range l.Steps[1:] {
					if st == nil {
						break
					}
					n := i + 2
					if l.Kind == "tagger" {
						n = 3
					}
					end = append(end, part(fmt.Sprint(n)+" ", "dim"))
					end = slices.Concat(end, st.Parts, stateMark[st.State], []pathPart{part(" ", "fg")})
					href = st.Href
					if st.Note != "" {
						row.Note, row.NoteClass = st.Note, "dim"
						if st.State == "cond" {
							row.NoteClass = "warn"
						}
					}
				}
				row.End = linked(href, partsHTML(end))
			case "implies":
				row.Mid = partsHTML([]pathPart{part(l.LineNote, "dim")})
			default:
				row.Mid = linked(l.Span.Href, partsHTML(l.Span.Parts))
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func (p tagPath) galleryRows() []template.HTML {
	var out []template.HTML
	for _, g := range append(p.Gallery[:], p.Image) {
		for i, ln := range g.Lines {
			parts := ln.Parts
			if i == 0 {
				parts = append([]pathPart{part(g.Title+": ", "fg")}, parts...)
			}
			inner := partsHTML(parts)
			switch {
			case ln.Focus != "":
				inner = template.HTML(fmt.Sprintf(`<a href="%s" onclick="document.getElementById('%s').focus();return false">%s</a>`, ln.Href, ln.Focus, inner))
			case ln.AliasInto != 0:
				inner = template.HTML(fmt.Sprintf(`<a href="#" data-alias-into="%d" data-alias-name="%s">%s</a>`, ln.AliasInto, html.EscapeString(ln.AliasName), inner))
			case ln.Action != "":
				inner = template.HTML(fmt.Sprintf(`<a href="#" onclick="document.getElementById('%s').click();return false">%s</a>`, ln.Action, inner))
			default:
				inner = linked(cmp.Or(ln.Href, g.Href), inner)
			}
			out = append(out, inner)
		}
	}
	return out
}

func (p tagPath) hasTaggerLane() bool {
	for _, b := range p.Bands {
		for _, l := range b.Lanes {
			if l.Kind == "tagger" {
				return true
			}
		}
	}
	return false
}

func colored(text, color string) pathPart { return pathPart{Text: text, Color: color} }

// A free route, for the monloader wait: the reads run under the gallery
// lock on either side of it, and a gallery switched meanwhile ends the
// render rather than mixing two galleries' tags.
func (s *Server) tagRulesPanel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags/%d#tag-path", id), http.StatusSeeOther)
		return
	}
	s.set.RLock()
	cx := s.active()
	if pageGalleryStale(w, r, s.activeGallery()) {
		s.set.RUnlock()
		return
	}
	tag, err := s.tagSvc().GetTag(id)
	var aliases []models.Tag
	if err == nil {
		byCanon, _ := s.tagSvc().AliasesForTagIDs([]int64{tag.ID})
		aliases = byCanon[tag.ID]
	}
	s.set.RUnlock()
	if errors.Is(err, tags.ErrTagNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.IsAlias || tag.CategoryName == "rating" {
		w.WriteHeader(http.StatusOK)
		return
	}
	answers, state := s.tagRulesAnswers(r.Context(), tag, aliases)
	s.set.RLock()
	defer s.set.RUnlock()
	if s.active() != cx {
		w.Header().Set("X-Monbooru-Gallery", s.activeGallery())
		http.Error(w, "The active gallery changed; reload the page.", http.StatusConflict)
		return
	}
	// The page sends its width; the frame's padding and border come off it.
	avail, _ := strconv.ParseFloat(r.URL.Query().Get("w"), 64)
	path := s.buildTagPath(tag, aliases, answers, state)
	s.renderTemplate(w, "partials/tag_rules.html", map[string]any{
		"SVG":       tagPathSVG(tag, path, avail-30),
		"Rows":      path.rows(),
		"Gallery":   path.galleryRows(),
		"Monloader": path.Monloader,
		"Taggers":   path.Taggers,
	})
}

// tagRulesAnswers asks monloader about the tag and its first aliases; state
// is unpaired, paused, old, unreachable or live.
func (s *Server) tagRulesAnswers(ctx context.Context, tag *models.Tag, aliases []models.Tag) ([]monloader.TagRules, string) {
	switch {
	case !s.pairedWith(monloader.App):
		return nil, "unpaired"
	case s.ml.Paused():
		return nil, "paused"
	case !s.ml.Usable():
		return nil, "unreachable"
	}
	names := []monloader.TagName{{Tag: tag.Name, Category: tag.CategoryName}}
	for _, a := range aliases[:min(len(aliases), maxRulesAliases)] {
		names = append(names, monloader.TagName{Tag: a.Name, Category: a.CategoryName})
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	answers, err := s.ml.TagRulesFor(ctx, names)
	if errors.Is(err, monloader.ErrNoRules) {
		return nil, "old"
	}
	if err != nil || len(answers) == 0 {
		logx.Warnf("monloader rules for %s: %v", tag.Name, err)
		return nil, "unreachable"
	}
	return answers, "live"
}

func (s *Server) buildTagPath(tag *models.Tag, aliases []models.Tag, answers []monloader.TagRules, state string) tagPath {
	colors := s.categoryColors()
	catColor := func(cat string) string { return categoryColor(colors[cat]) }
	p := tagPath{Monloader: state, GalleryName: s.activeGallery(), Category: tag.CategoryName, CatColor: catColor(tag.CategoryName)}
	web := s.monloaderWebBase()

	var own, spelled []pathLane
	quiet := aliases
	if state == "live" {
		own = siteLanes(tag.Name, tag.CategoryName, answers[0], web, catColor)
		spelled, quiet = aliasLanes(tag.CategoryName, aliases, answers[1:], web, catColor)
	}
	if p.Monloader != "live" {
		note := pathLane{Kind: "note", Name: "monloader", NameClass: "dim",
			Span: &pathBox{Parts: []pathPart{part("not paired, no site lanes  ", "dim"), part("[pair…]", "link")}, Href: "/settings#plugins"}}
		switch p.Monloader {
		case "paused":
			note.Span = &pathBox{Parts: []pathPart{part("paused, no site lanes", "dim")}}
		case "unreachable":
			note.Span = &pathBox{Parts: []pathPart{part("unreachable, no site lanes", "dim")}}
		case "old":
			note.Span = &pathBox{Parts: []pathPart{part("monloader v1.14.0 or newer shows site lanes", "dim")}}
		}
		own = []pathLane{note}
	}
	ownTaggers, aliasTaggers := s.taggerLanes(tag, aliases, catColor)
	for _, l := range ownTaggers {
		if l.Steps[0].State != "off" {
			p.Taggers++
		}
	}
	own = append(own, ownTaggers...)
	own = append(own, pathLane{Kind: "span", Name: "you", NameClass: "fg", Span: &pathBox{Parts: []pathPart{part("pages, batch, API, import", "fg")}}})
	p.Bands = []pathBand{{Label: "as " + tag.Name, Lanes: own}}

	if n := len(aliases); n > 0 {
		band := pathBand{Label: "as an alias", Count: fmt.Sprintf(" · %d other name%s", n, plural(n))}
		if shared := sharedCategory(aliases); shared != "" {
			band.Count += ", all " + shared
		}
		band.Lanes = append(spelled, aliasTaggers...)
		if len(quiet) > 0 {
			var list []pathPart
			for i, a := range quiet {
				if i > 0 {
					list = append(list, part(", ", "fg"))
				}
				list = append(list, colored(spelling(a, tag.CategoryName), catColor(a.CategoryName)))
			}
			band.Lanes = append(band.Lanes, pathLane{Kind: "any", Name: "any source", NameClass: "dim", Span: &pathBox{Parts: list}})
		}
		p.Bands = append(p.Bands, band)
		p.Gallery[0] = pathGallery{Title: "4 aliases", State: "on", Href: "#tag-aliases",
			Lines: []pathLine{{Parts: []pathPart{part(fmt.Sprintf("%d name%s → here  ", n, plural(n)), "fg"), part("[add alias]", "link")}, Href: "#tag-aliases", Focus: "alias-add-input"}}}
	} else {
		p.Gallery[0] = pathGallery{Title: "4 aliases", Href: "#tag-aliases",
			Lines: []pathLine{{Parts: []pathPart{part("none  ", "dim"), part("[add alias]", "link")}, Href: "#tag-aliases", Focus: "alias-add-input"}}}
	}
	if !tag.Rejected {
		p.Gallery[0].Lines = append(p.Gallery[0].Lines, s.elsewhereLines(tag, aliases)...)
	}
	if tag.Rejected {
		p.Gallery[1] = pathGallery{Title: "5 rejected", State: "stop",
			Lines: []pathLine{{Parts: []pathPart{part(fmt.Sprintf("yes · %d dropped  ", tag.RejectedDrops), "fg"), part("[un-reject]", "link")}, Action: "btn-detail-unreject"}}}
	} else {
		p.Gallery[1] = pathGallery{Title: "5 rejected",
			Lines: []pathLine{{Parts: []pathPart{part("no  ", "fg"), part("[reject]", "link")}, Action: "btn-detail-delete"}}}
	}
	parents, _ := s.tagSvc().ImpliedBy(tag.ID)
	if n := len(parents); n > 0 {
		adds, implies := "add", "imply"
		if n == 1 {
			adds, implies = "adds", "implies"
		}
		band := pathBand{Label: "through an implication", Count: fmt.Sprintf(" · %d tag%s that %s this one", n, plural(n), adds)}
		for _, im := range parents[:min(n, maxImplicationLanes)] {
			lane := pathLane{Kind: "implies", Name: im.ParentName, NameColor: catColor(im.ParentCategoryName), NameHref: fmt.Sprintf("/tags/%d", im.ParentID), LineNote: "no image yet"}
			if parent, err := s.tagSvc().GetTag(im.ParentID); err == nil && parent.UsageCount > 0 {
				lane.LineNote = fmt.Sprintf("%d image%s", parent.UsageCount, plural(parent.UsageCount))
			}
			band.Lanes = append(band.Lanes, lane)
		}
		if n > maxImplicationLanes {
			band.Lanes = append(band.Lanes, pathLane{Kind: "implies", Name: fmt.Sprintf("+%d more", n-maxImplicationLanes), NameClass: "link", NameHref: "#tag-implied-by"})
		}
		p.Bands = append(p.Bands, band)
		p.Gallery[2] = pathGallery{Title: "6 implications", State: "on", Href: "#tag-implied-by",
			Lines: []pathLine{{Parts: []pathPart{part(fmt.Sprintf("%d tag%s %s it  ", n, plural(n), implies), "fg"), part("[add]", "link")}, Href: "#tag-implied-by", Focus: "implied-by-add-input"}}}
	} else {
		p.Gallery[2] = pathGallery{Title: "6 implications", Href: "#tag-implied-by",
			Lines: []pathLine{{Parts: []pathPart{part("nothing implies it  ", "dim"), part("[add]", "link")}, Href: "#tag-implied-by", Focus: "implied-by-add-input"}}}
	}

	if tag.Rejected {
		// A rejected tag takes no alias or implication, and its page has
		// neither editor to focus.
		for _, i := range []int{0, 2} {
			p.Gallery[i].Href = "#tag-rejected"
			for j := range p.Gallery[i].Lines {
				l := &p.Gallery[i].Lines[j]
				l.Parts = slices.DeleteFunc(l.Parts, func(pp pathPart) bool { return pp.Class == "link" })
				l.Href, l.Focus = "", ""
			}
		}
	}

	p.Image = pathGallery{Title: "the image", State: "on", Lines: []pathLine{{Parts: []pathPart{part(fmt.Sprintf("%d image%s", tag.UsageCount, plural(tag.UsageCount)), "dim")}}}}
	if tag.UsageCount == 0 || tag.Rejected {
		p.Image.State = "off"
	}
	if imps, err := s.tagSvc().ListImplications(tag.ID); err == nil && len(imps) > 0 {
		var also []pathPart
		for i, im := range imps {
			if i > 0 {
				also = append(also, part(", ", "fg"))
			}
			also = append(also, colored(im.ImpliedName, catColor(im.ImpliedCategoryName)))
		}
		p.Image.Lines = append(p.Image.Lines, pathLine{Parts: append([]pathPart{part("also gets ", "dim")}, also...), Href: "#implications-editor"})
	}
	return p
}

// Sites with nothing naming the tag fold into one lane; the PTR never does.
func siteLanes(name, cat string, rules monloader.TagRules, web string, catColor func(string) string) []pathLane {
	var lanes []pathLane
	var folded []monloader.TagRule
	for _, r := range rules.Sites {
		if r.Rule == "none" && len(r.MappedFrom) == 0 && len(r.RenamedFrom) == 0 && r.Blacklist == "" && r.Kept && r.Kind != "lookup" {
			folded = append(folded, r)
			continue
		}
		lanes = append(lanes, siteLane(name, cat, r, web, catColor))
	}
	switch len(folded) {
	case 0:
	case 1:
		lanes = append(lanes, siteLane(name, cat, folded[0], web, catColor))
	default:
		lane := siteLane(name, cat, folded[0], web, catColor)
		lane.Name, lane.NameClass, lane.Site = fmt.Sprintf("%d other sites", len(folded)), "dim", ""
		lane.Steps[0].Href = ""
		lanes = append(lanes, lane)
	}
	if len(lanes) == 0 {
		lanes = []pathLane{{Kind: "note", Name: "monloader", NameClass: "dim", Span: &pathBox{Parts: []pathPart{part("no site configured", "dim")}}}}
	}
	return lanes
}

func siteLane(name, cat string, r monloader.TagRule, web string, catColor func(string) string) pathLane {
	lane := pathLane{Kind: "site", Name: r.Site, NameClass: "ml", Site: r.Kind}
	tagCat := cat
	isPTR := r.Kind == "lookup"
	ruleHref, rulesHref := "", ""
	if web != "" {
		ruleHref = web + "/settings?site=" + url.QueryEscape(r.Site) + "&tab=mapping&q=" + url.QueryEscape(name)
		rulesHref = web + "/settings#rules"
	}
	step1 := &pathBox{Href: ruleHref}
	if r.Builtin {
		step1.Right = "built-in"
	}
	switch r.Rule {
	case "drop":
		step1.Parts, step1.State = []pathPart{part("tag rule: ", "fg"), part("drop", "err")}, "stop"
		lane.Steps[0] = step1
		return lane
	case "rename", "move":
		targetCat, _, _ := strings.Cut(r.Target, ":")
		if !strings.Contains(r.Target, ":") {
			targetCat = cat
		}
		step1.Parts, step1.State = []pathPart{part("tag rule → ", "fg"), colored(r.Target, catColor(targetCat))}, "away"
		lane.Steps[0] = step1
		return lane
	}
	switch {
	case len(r.RenamedFrom) > 0:
		step1.Parts = []pathPart{part("tag rule: ", "fg"), colored(strings.Join(r.RenamedFrom, ", "), catColor(cat)), part(" → here", "dim")}
	case len(r.MappedFrom) > 0:
		step1.Parts = []pathPart{part("category map: "+strings.Join(r.MappedFrom, ", ")+" → ", "fg"), colored(cat, catColor(cat))}
	default:
		step1.Parts = []pathPart{part("no rule", "dim")}
	}
	lane.Steps[0] = step1

	step2 := &pathBox{Parts: []pathPart{part("passes", "dim")}, Href: rulesHref}
	if r.Blacklist != "" {
		list := "global list"
		if r.Blacklist == "site" {
			list = r.Site + "'s own list"
			if web != "" {
				step2.Href = web + "/settings?site=" + url.QueryEscape(r.Site) + "&tab=mapping"
			}
		}
		if others := blacklistCondition(r.BlacklistLine, name); others != "" {
			step2.Parts, step2.State = []pathPart{part("skip", "warn")}, "cond"
			step2.Note = "only with " + others
		} else {
			step2.Parts, step2.State = []pathPart{part("skip", "err")}, "stop"
			step2.Note = list + ": post skipped"
			if isPTR {
				step2.Note = list + ": answer skipped"
			}
			lane.Steps[1] = step2
			return lane
		}
	}
	lane.Steps[1] = step2

	step3 := &pathBox{Parts: []pathPart{colored(cat+" ", catColor(cat)), part("kept", "fg")}, Href: rulesHref, Shared: cat == tagCat}
	if !r.Kept {
		step3.Parts, step3.State = []pathPart{colored(cat+" ", catColor(cat)), part("off", "err")}, "stop"
		step3.Shared = false
		step3.Note = "post sent without it"
		if isPTR {
			step3.Note = "answer sent without it"
		}
	}
	lane.Steps[2] = step3
	return lane
}

// maxRulesAliases keeps the batched call under monloader's limit of 50.
const maxRulesAliases = 49

const maxImplicationLanes = 6

// A spelling carries its category only when it is not the tag's.
func spelling(a models.Tag, cat string) string {
	if a.CategoryName == cat {
		return a.Name
	}
	return a.CategoryName + ":" + a.Name
}

func sharedCategory(aliases []models.Tag) string {
	for _, a := range aliases[1:] {
		if a.CategoryName != aliases[0].CategoryName {
			return ""
		}
	}
	return aliases[0].CategoryName
}

// A spelling no site does anything with joins the quiet list; one a site
// drops, renames or skips gets that site's lane, the others folded beside it.
func aliasLanes(cat string, aliases []models.Tag, answers []monloader.TagRules, web string, catColor func(string) string) (lanes []pathLane, quiet []models.Tag) {
	for i, a := range aliases {
		if i >= len(answers) {
			quiet = append(quiet, aliases[i:]...)
			break
		}
		var loud []pathLane
		var folded []monloader.TagRule
		for _, r := range answers[i].Sites {
			if r.Rule == "none" && r.Blacklist == "" && r.Kept && len(r.RenamedFrom) == 0 {
				folded = append(folded, r)
				continue
			}
			loud = append(loud, siteLane(a.Name, a.CategoryName, r, web, catColor))
		}
		if len(loud) == 0 {
			quiet = append(quiet, a)
			continue
		}
		if len(folded) > 0 {
			lane := siteLane(a.Name, a.CategoryName, folded[0], web, catColor)
			if len(folded) > 1 {
				lane.Name, lane.NameClass, lane.Site = fmt.Sprintf("%d other sites", len(folded)), "dim", ""
				lane.Steps[0].Href = ""
			}
			loud = append(loud, lane)
		}
		lead := []pathPart{colored(spelling(a, cat), catColor(a.CategoryName)), part(": ", "fg")}
		for _, l := range loud {
			l.Steps[0].Parts = append(slices.Clone(lead), l.Steps[0].Parts...)
			if a.CategoryName != cat && l.Steps[2] != nil {
				l.Steps[2].Shared = false
			}
			lanes = append(lanes, l)
		}
	}
	return lanes, quiet
}

// A plain tag of another category under this tag's name or one of its
// spellings is where a site filing it there lands instead.
func (s *Server) elsewhereLines(tag *models.Tag, aliases []models.Tag) []pathLine {
	names := []string{tag.Name}
	for _, a := range aliases {
		names = append(names, a.Name)
	}
	found, err := s.tagSvc().NamedTags(names)
	if err != nil {
		return nil
	}
	distinct, _ := s.tagSvc().DistinctPartners([]int64{tag.ID})
	var lines []pathLine
	for _, t := range found {
		if t.ID == tag.ID || slices.ContainsFunc(distinct[tag.ID], func(d tags.DistinctPartner) bool { return d.ID == t.ID }) {
			continue
		}
		name := t.CategoryName + ":" + t.Name
		lines = append(lines, pathLine{Parts: []pathPart{part(name, "warn"), part(" exists  ", "dim"), part("[alias it]", "link")}, AliasInto: t.ID, AliasName: name})
	}
	return lines
}

// A blacklist line skips a post only when all its tags match, so a line
// naming more than the tag is a condition: the rest of the line.
func blacklistCondition(line, name string) string {
	tokens := strings.Fields(line)
	if len(tokens) < 2 {
		return ""
	}
	for i, t := range tokens {
		_, bare, found := strings.Cut(t, ":")
		if !found {
			bare = t
		}
		if ok, _ := path.Match(bare, name); ok {
			return strings.Join(slices.Delete(tokens, i, i+1), " ")
		}
	}
	return strings.Join(tokens, " ")
}

// One lane per tagger whose label file reaches the name, and one more when
// a label lands on a spelling of it; a tagger off for this gallery shows
// so on the name's own lane, and one that knows neither has no lane.
func (s *Server) taggerLanes(tag *models.Tag, aliases []models.Tag, catColor func(string) string) (own, spelled []pathLane) {
	cfg := s.cfgSnapshot()
	modelPath := s.modelPath()
	catIDs, _ := s.categoryChoices()
	active := s.activeGallery()
	reaches := func(v tagger.LabelView, name, cat string) bool {
		return v.TagName == name && (v.Muted || v.CatName == cat)
	}
	for _, t := range tagger.DiscoverTaggers(cfg) {
		if t.Malformed {
			continue
		}
		views, err := tagger.BrowseLabels(modelPath, t.Name, taggerTagsFile(t.TaggerInstance), catIDs)
		if err != nil {
			continue
		}
		i := slices.IndexFunc(views, func(v tagger.LabelView) bool { return reaches(v, tag.Name, tag.CategoryName) })
		if !t.Enabled || !t.AppliesToGallery(active) {
			if i >= 0 {
				own = append(own, offTaggerLane(t))
			}
			continue
		}
		if i >= 0 {
			own = append(own, taggerLane(t, views[i], tag.Name, tag.CategoryName, catColor))
		}
	spellings:
		for _, a := range aliases {
			for _, v := range views {
				if reaches(v, a.Name, a.CategoryName) {
					lane := taggerLane(t, v, a.Name, a.CategoryName, catColor)
					lane.Steps[0].Parts = append([]pathPart{colored(spelling(a, tag.CategoryName), catColor(a.CategoryName)), part(": ", "fg")}, lane.Steps[0].Parts...)
					spelled = append(spelled, lane)
					break spellings
				}
			}
		}
	}
	return own, spelled
}

func taggerLane(t tagger.TaggerStatus, v tagger.LabelView, name, cat string, catColor func(string) string) pathLane {
	lane := pathLane{Kind: "tagger", Name: t.Name, NameClass: "tg"}
	configure := "/settings?tagger=" + url.QueryEscape(t.Name) + "&panel="
	layer := map[string]string{"custom": "yours", "default": "bundled"}[v.Rule]
	maps := &pathBox{Href: configure + "mappings&q=" + url.QueryEscape(v.Source), Right: layer}
	switch {
	case v.Muted:
		maps.Parts, maps.State = []pathPart{part("drop", "err")}, "stop"
		lane.Steps[0] = maps
		return lane
	case v.Source != name:
		maps.Parts = []pathPart{part(v.Source, "dim"), part(" → here", "fg")}
	default:
		maps.Parts = []pathPart{part("no rule", "dim")}
	}
	lane.Steps[0] = maps
	threshold := tagger.CategoryThreshold(t.ConfidenceThreshold, t.CategoryThresholds, cat)
	cats := &pathBox{Href: configure + "thresholds", Parts: []pathPart{colored(cat+" ", catColor(cat)), part(fmt.Sprintf("≥ %.2f", threshold), "fg")}}
	if slices.Contains(t.DisabledCategories, cat) {
		cats.Parts, cats.State = []pathPart{colored(cat+" ", catColor(cat)), part("off", "err")}, "stop"
	}
	lane.Steps[1] = cats
	return lane
}

func offTaggerLane(t tagger.TaggerStatus) pathLane {
	off := &pathBox{State: "off", Parts: []pathPart{part("off for this gallery ", "dim"), part("›", "link")},
		Href: "/settings?tagger=" + url.QueryEscape(t.Name) + "&panel=galleries"}
	if !t.Enabled {
		off.Parts, off.Href = []pathPart{part("turned off ", "dim"), part("›", "link")}, "/settings#tagger"
	}
	return pathLane{Kind: "tagger", Name: t.Name, NameClass: "dim", Steps: [3]*pathBox{off}}
}

// The drawing's columns: the source, monloader's three steps, the tagger's
// two at the widths of steps 1 and 3, the bus, the gallery's steps over the
// image. Without monloader its three columns go and the monbooru side moves
// left. Width past the narrowest goes to the step-1 boxes and the gallery.
const (
	tpPitch, tpBH  = 36, 26
	tpSX, tpSW     = 8, 120
	tpGap          = 12
	tp1Min, tpGMin = 180, 230
	tp2W, tp3W     = 76, 150
	tpMaxW         = 2000
	tpCharW        = 0.6
)

type tpCols struct{ x1, w1, x2, x3, div, tx, tcx, bus, gx, gw, w float64 }

func tpColumns(ml bool, avail float64) tpCols {
	c := tpCols{w1: tp1Min, gw: tpGMin}
	c.place(ml)
	if extra := math.Floor(min(avail, tpMaxW)) - c.w; extra > 0 {
		grow := math.Floor(extra * 0.6)
		c.gw += extra - grow
		if ml {
			grow = math.Floor(extra * 0.35)
			c.gw = tpGMin + extra - 2*grow
		}
		c.w1 += grow
		c.place(ml)
	}
	return c
}

func (c *tpCols) place(ml bool) {
	c.x1 = tpSX + tpSW + 14
	c.x2 = c.x1 + c.w1 + tpGap
	c.x3 = c.x2 + tp2W + tpGap
	c.div = c.x3 + tp3W + tpGap
	c.tx = c.div + tpGap
	if !ml {
		c.tx = tpSX + tpSW + 24
	}
	c.tcx = c.tx + c.w1 + 14
	c.bus = c.tcx + tp3W + 14
	c.gx = c.bus + 22
	c.w = c.gx + c.gw + 8
}

// cells is a run's width in cells of the monospace face; a wide glyph
// is an em, five thirds of a cell.
func cells(s string) float64 {
	n := 0.0
	for _, r := range s {
		n++
		if wideRune(r) {
			n += 2.0 / 3
		}
	}
	return n
}

func wideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1FAFF, r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

// fitParts cuts a run to the box, in cells of the monospace face, and
// returns the whole text for a tooltip when it had to.
func fitParts(parts []pathPart, width, size float64) ([]pathPart, string) {
	maxc := width / (size * tpCharW)
	total := 0.0
	for _, p := range parts {
		total += cells(p.Text)
	}
	if total <= maxc {
		return parts, ""
	}
	var full strings.Builder
	for _, p := range parts {
		full.WriteString(p.Text)
	}
	var out []pathPart
	n := 0.0
	for _, p := range parts {
		if n+cells(p.Text) > maxc-1 {
			var cut strings.Builder
			for _, r := range p.Text {
				if n+cells(string(r)) > maxc-1 {
					break
				}
				cut.WriteRune(r)
				n += cells(string(r))
			}
			p.Text = cut.String() + "…"
			out = append(out, p)
			break
		}
		out = append(out, p)
		n += cells(p.Text)
	}
	return out, full.String()
}

// wrapParts breaks a run onto lines of the box's width, between parts; a
// comma stays with the name before it, its space dropped at a line's end.
func wrapParts(parts []pathPart, width, size float64) [][]pathPart {
	maxc := width / (size * tpCharW)
	comma := func(i int) bool { return i < len(parts) && strings.TrimSpace(parts[i].Text) == "," }
	var lines [][]pathPart
	var cur []pathPart
	n := 0.0
	for i, p := range parts {
		need := cells(p.Text)
		if comma(i + 1) {
			need++
		}
		if !comma(i) && n+need > maxc && len(cur) > 0 {
			if last := &cur[len(cur)-1]; comma(i - 1) {
				last.Text = strings.TrimRight(last.Text, " ")
			}
			lines = append(lines, cur)
			cur, n = nil, 0
		}
		cur = append(cur, p)
		n += cells(p.Text)
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

type svgWriter struct{ strings.Builder }

func (b *svgWriter) f(format string, args ...any) { fmt.Fprintf(b, format, args...) }

func (b *svgWriter) text(x, y float64, s, class string, size float64, anchor string, bold bool) {
	weight := ""
	if bold {
		weight = ` font-weight="bold"`
	}
	b.f(`<text x="%g" y="%g" class="tp-%s" font-size="%g" text-anchor="%s"%s>%s</text>`, x, y, class, size, anchor, weight, html.EscapeString(s))
}

func (b *svgWriter) spans(x, y float64, parts []pathPart, size, width float64) {
	title := ""
	if width > 0 {
		parts, title = fitParts(parts, width, size)
	}
	// A browser rounding each glyph's advance would run a fitted line past
	// its box; its length is pinned to the one it was fitted to.
	fit := ""
	if width > 0 {
		n := 0.0
		for _, pt := range parts {
			n += cells(pt.Text)
		}
		fit = fmt.Sprintf(` textLength="%.1f" lengthAdjust="spacing"`, n*size*tpCharW)
	}
	b.f(`<text x="%g" y="%g" font-size="%g"%s>`, x, y, size, fit)
	if title != "" {
		b.f(`<title>%s</title>`, html.EscapeString(title))
	}
	for _, pt := range parts {
		if pt.Color != "" {
			b.f(`<tspan style="fill:%s">%s</tspan>`, pt.Color, html.EscapeString(pt.Text))
		} else {
			b.f(`<tspan class="tp-%s">%s</tspan>`, pt.Class, html.EscapeString(pt.Text))
		}
	}
	b.f(`</text>`)
}

func (b *svgWriter) rect(x, y, w, h float64, class string, dashed bool, width float64) {
	extra := ""
	if dashed {
		extra += ` stroke-dasharray="3 3"`
	}
	if width > 0 {
		extra += fmt.Sprintf(` stroke-width="%g"`, width)
	}
	b.f(`<rect x="%g" y="%g" width="%g" height="%g" fill="none" class="tp-%s"%s/>`, x, y, w, h, class, extra)
}

func (b *svgWriter) line(x1, y1, x2, y2 float64, arrow, dashed bool) {
	extra := ""
	if arrow {
		extra += ` marker-end="url(#tp-arrow)"`
	}
	if dashed {
		extra += ` stroke-dasharray="3 3"`
	}
	b.f(`<line x1="%g" y1="%g" x2="%g" y2="%g" class="tp-sdim"%s/>`, x1, y1, x2, y2, extra)
}

func (b *svgWriter) open(href, action string) bool {
	switch {
	case action != "":
		b.f(`<a href="#" onclick="document.getElementById('%s').click();return false">`, action)
	case href == "":
		return false
	case newTab(href):
		b.f(`<a href="%s" target="_blank" rel="noopener">`, html.EscapeString(href))
	default:
		b.f(`<a href="%s">`, html.EscapeString(href))
	}
	return true
}

func (b *svgWriter) close(opened bool) {
	if opened {
		b.f(`</a>`)
	}
}

var boxStroke = map[string]string{"": "sborder", "stop": "serr", "away": "sdim", "cond": "swarn"}

func (b *svgWriter) box(x, w, cy float64, num string, bx *pathBox) {
	top := cy - tpBH/2
	opened := b.open(bx.Href, "")
	width := 0.0
	if bx.State == "cond" {
		width = 1.5
	}
	b.rect(x, top, w, tpBH, boxStroke[bx.State], bx.State == "away", width)
	parts := bx.Parts
	if num != "" {
		parts = append([]pathPart{part(num+" ", "dim")}, parts...)
	}
	room := w - 16
	if bx.State == "stop" || bx.State == "cond" {
		room -= 16
	}
	if bx.Right != "" {
		room -= cells(bx.Right)*11*tpCharW + 8
	}
	b.spans(x+8, cy+4, parts, 12, room)
	b.close(opened)
	switch {
	case bx.State == "stop":
		b.text(x+w-8, cy+5, "×", "err", 14, "end", true)
	case bx.State == "cond":
		b.text(x+w-8, cy+4, "?", "warn", 12, "end", true)
	case bx.Right != "":
		b.text(x+w-8, cy+4, bx.Right, "dim", 11, "end", false)
	}
	if bx.Note != "" {
		class := "dim"
		if bx.State == "cond" {
			class = "warn"
		}
		b.text(x, cy+tpBH/2+11, bx.Note, class, 9.5, "start", false)
	}
}

func tagPathSVG(tag *models.Tag, p tagPath, avail float64) template.HTML {
	ml := p.Monloader == "live"
	c := tpColumns(ml, avail)
	var b svgWriter
	b.f(`<defs><marker id="tp-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0,0 L10,5 L0,10 z" class="tp-dim"/></marker></defs>`)
	if ml {
		b.f(`<line x1="%g" y1="6" x2="%g" y2="6" class="tp-sml" stroke-width="2"/>`, c.x1, c.x3+tp3W)
		b.text(c.x1, 20, "set in monloader", "ml", 11, "start", true)
		b.text(c.x3+tp3W, 20, "Settings → Sites, Rules", "dim", 10, "end", false)
	}
	b.f(`<line x1="%g" y1="6" x2="%g" y2="6" class="tp-slink" stroke-width="2"/>`, c.tx, c.gx+c.gw)
	b.text(c.tx, 20, "set in monbooru", "link", 11, "start", true)
	where := "this page"
	if p.hasTaggerLane() {
		where = "Settings → Auto-Tagger · this page"
	}
	b.text(c.gx+c.gw, 20, where, "dim", 10, "end", false)
	type head struct {
		x         float64
		name, sub string
	}
	heads := []head{{tpSX, "source", ""}}
	if ml {
		heads = append(heads, head{c.x1, "1 mappings", "per site: its tags tab"}, head{c.x2, "2 blacklist", "global + site"}, head{c.x3, "3 categories kept", "global"})
	}
	if p.hasTaggerLane() {
		heads = append(heads, head{c.tx, "1 mappings", "Configure → mappings"}, head{c.tcx, "3 categories kept", "Configure → thresholds"})
	}
	heads = append(heads, head{c.gx, "4-6 gallery rules · " + p.GalleryName, "this gallery"})
	for _, h := range heads {
		b.text(h.x, 40, h.name, "fg", 11, "start", false)
		if h.sub != "" {
			b.text(h.x, 53, h.sub, "dim", 10, "start", false)
		}
	}
	b.f(`<line x1="0" y1="60" x2="%g" y2="60" class="tp-sborder"/>`, c.w)

	y := 84.0
	var pass, passC, noteYs []float64
	var siteYs, taggerYs [][]float64
	var run []float64
	runHref := ""
	flush := func(cat string) {
		if len(run) == 0 {
			return
		}
		top, bot := run[0]-tpBH/2, run[len(run)-1]+tpBH/2
		opened := b.open(runHref, "")
		b.rect(c.x3, top, tp3W, bot-top, "sborder", false, 0)
		b.spans(c.x3+8, (top+bot)/2+4, []pathPart{part("3 ", "dim"), colored(cat+" ", p.CatColor), part("kept", "fg")}, 12, tp3W-16)
		b.close(opened)
		for _, cy := range run {
			b.line(c.x3+tp3W, cy, c.bus, cy, false, false)
			pass = append(pass, cy)
		}
		run = nil
	}
	source := func(cy float64, l pathLane) {
		opened := b.open(l.NameHref, "")
		b.rect(tpSX, cy-tpBH/2, tpSW, tpBH, "sborder", l.Kind == "note" || l.Kind == "tagger" && l.Steps[0].State == "off", 0)
		name := []pathPart{{Text: l.Name, Class: l.NameClass, Color: l.NameColor}}
		if strings.HasPrefix(l.NameHref, "/") {
			name = append(name, part(" ›", "link"))
		}
		room := float64(tpSW - 16)
		if l.Site != "" {
			room -= cells(l.Site)*6 + 8
			b.text(tpSX+tpSW-8, cy+4, l.Site, "dim", 10, "end", false)
		}
		b.spans(tpSX+8, cy+4, name, 12, room)
		b.close(opened)
	}
	for _, band := range p.Bands {
		b.spans(tpSX, y, []pathPart{part(band.Label, "fg"), part(band.Count, "dim")}, 11, 0)
		y += 22
		siteYs, taggerYs = append(siteYs, nil), append(taggerYs, nil)
		bi := len(siteYs) - 1
		for _, l := range band.Lanes {
			cy := y
			s3 := l.Steps[2]
			if l.Kind != "site" || s3 == nil || !s3.Shared || len(run) > 0 && run[len(run)-1] != cy-tpPitch {
				flush(p.Category)
			}
			source(cy, l)
			extra := 0.0
			switch l.Kind {
			case "site":
				siteYs[bi] = append(siteYs[bi], cy)
				b.line(tpSX+tpSW, cy, c.x1, cy, true, false)
				b.box(c.x1, c.w1, cy, "1", l.Steps[0])
				if l.Steps[1] == nil {
					break
				}
				b.line(c.x1+c.w1, cy, c.x2, cy, true, false)
				b.box(c.x2, tp2W, cy, "2", l.Steps[1])
				if l.Steps[1].Note != "" {
					extra, noteYs = 10, append(noteYs, cy)
				}
				if s3 == nil {
					break
				}
				b.line(c.x2+tp2W, cy, c.x3, cy, true, false)
				if s3.Shared {
					run, runHref = append(run, cy), s3.Href
					break
				}
				b.box(c.x3, tp3W, cy, "3", s3)
				if s3.Note != "" {
					extra, noteYs = 10, append(noteYs, cy)
				}
				if s3.State == "" {
					b.line(c.x3+tp3W, cy, c.bus, cy, false, false)
					pass = append(pass, cy)
				}
			case "tagger":
				b.line(tpSX+tpSW, cy, c.tx, cy, true, ml)
				if l.Steps[0].State == "off" {
					opened := b.open(l.Steps[0].Href, "")
					b.rect(c.tx, cy-tpBH/2, c.tcx+tp3W-c.tx, tpBH, "sdim", true, 0)
					b.spans(c.tx+8, cy+4, l.Steps[0].Parts, 12, 0)
					b.close(opened)
					break
				}
				taggerYs[bi] = append(taggerYs[bi], cy)
				b.box(c.tx, c.w1, cy, "1", l.Steps[0])
				if l.Steps[1] == nil {
					break
				}
				b.line(c.tx+c.w1, cy, c.tcx, cy, true, false)
				b.box(c.tcx, tp3W, cy, "3", l.Steps[1])
				if l.Steps[1].State == "" {
					b.line(c.tcx+tp3W, cy, c.bus, cy, false, false)
					pass = append(pass, cy)
				}
			case "span":
				b.line(tpSX+tpSW, cy, c.tx, cy, true, ml)
				opened := b.open(l.Span.Href, "")
				b.rect(c.tx, cy-tpBH/2, c.tcx+tp3W-c.tx, tpBH, "sborder", true, 0)
				b.spans(c.tx+8, cy+4, l.Span.Parts, 12, c.tcx+tp3W-c.tx-16)
				b.close(opened)
				b.line(c.tcx+tp3W, cy, c.bus, cy, false, false)
				pass = append(pass, cy)
			case "any":
				bx, bw, lead := c.tx, c.tcx+tp3W-c.tx, ""
				if ml {
					bx, bw, lead = c.x1, c.x3+tp3W-c.x1, "1-3 "
				}
				lines := wrapParts(l.Span.Parts, bw-16-float64(len(lead))*12*tpCharW, 12)
				b.line(tpSX+tpSW, cy, bx, cy, true, false)
				b.rect(bx, cy-tpBH/2, bw, tpBH+16*float64(len(lines)-1), "sborder", true, 0)
				for i, ln := range lines {
					indent := strings.Repeat(" ", len(lead))
					if i == 0 {
						indent = lead
					}
					b.spans(bx+8, cy+4+16*float64(i), append([]pathPart{part(indent, "dim")}, ln...), 12, bw-16)
				}
				b.line(bx+bw, cy, c.bus, cy, false, false)
				pass = append(pass, cy)
				extra = 16 * float64(len(lines)-1)
			case "implies":
				b.line(tpSX+tpSW, cy, c.bus+8, cy, false, ml)
				if l.LineNote != "" {
					b.text(tpSX+tpSW+10, cy-4, l.LineNote, "dim", 9.5, "start", false)
				}
				passC = append(passC, cy)
			case "note":
				opened := b.open(l.Span.Href, "")
				b.spans(tpSX+tpSW+14, cy+4, l.Span.Parts, 11, 0)
				b.close(opened)
			}
			y += tpPitch + extra
		}
		flush(p.Category)
		y += 16
	}
	lanesBottom := y - 10

	groups := func(x0, x1 float64, ys []float64, class string) {
		if len(ys) == 0 {
			return
		}
		top, bot := ys[0]-tpBH/2-5, ys[len(ys)-1]+tpBH/2+5
		if slices.Contains(noteYs, ys[len(ys)-1]) {
			bot += 12
		}
		b.f(`<rect x="%g" y="%g" width="%g" height="%g" fill="none" class="tp-s%s" stroke-dasharray="2 3" opacity="0.55"/>`, x0-7, top, x1-x0+14, bot-top, class)
	}
	for i := range siteYs {
		groups(c.x1, c.x3+tp3W, siteYs[i], "ml")
		groups(c.tx, c.tcx+tp3W, taggerYs[i], "tg")
	}

	gbox := func(gy float64, g pathGallery, dim bool) float64 {
		h := 26 + 15*float64(len(g.Lines)) + 4
		stroke, width := "sborder", 0.0
		switch g.State {
		case "on":
			stroke, width = "sfg", 1.5
		case "stop":
			stroke, width = "serr", 2
		}
		opened := b.open(g.Href, "")
		b.rect(c.gx, gy, c.gw, h, stroke, dim, width)
		title := "fg"
		if dim {
			title = "dim"
		}
		b.text(c.gx+8, gy+17, g.Title, title, 12, "start", true)
		b.close(opened)
		for i, ln := range g.Lines {
			o := false
			if ln.Focus != "" {
				b.f(`<a href="%s" onclick="document.getElementById('%s').focus();return false">`, ln.Href, ln.Focus)
				o = true
			} else {
				o = b.open(ln.Href, ln.Action)
			}
			if ln.AliasInto != 0 {
				b.f(`<a href="#" data-alias-into="%d" data-alias-name="%s">`, ln.AliasInto, html.EscapeString(ln.AliasName))
				o = true
			}
			room := float64(c.gw - 16)
			if g.State == "stop" {
				room -= 16
			}
			b.spans(c.gx+8, gy+34+float64(i)*15, ln.Parts, 11, room)
			b.close(o)
		}
		if g.State == "stop" {
			b.text(c.gx+c.gw-8, gy+19, "×", "err", 16, "end", true)
		}
		return h
	}
	stopped := p.Gallery[1].State == "stop"
	alY := 70.0
	if len(pass) > 0 {
		alY = max(70, (pass[0]+pass[len(pass)-1])/2-50)
	}
	alH := gbox(alY, p.Gallery[0], false)
	rjY := alY + alH + 26
	rjH := gbox(rjY, p.Gallery[1], false)
	imY := rjY + rjH + 26
	if len(passC) > 0 {
		imY = max(imY, (passC[0]+passC[len(passC)-1])/2-30)
	}
	imH := gbox(imY, p.Gallery[2], stopped)
	cx := c.gx + c.gw/2
	b.line(cx, alY+alH, cx, rjY, true, false)
	if !stopped {
		b.line(cx, rjY+rjH, cx, imY, true, false)
	}
	if len(pass) > 0 {
		mid := alY + alH/2
		b.line(c.bus, min(pass[0], mid), c.bus, max(pass[len(pass)-1], mid), false, false)
		b.line(c.bus, mid, c.gx, mid, true, false)
	}
	if len(passC) > 0 {
		mid := imY + imH/2
		b.line(c.bus+8, min(passC[0], mid), c.bus+8, max(passC[len(passC)-1], mid), false, false)
		b.line(c.bus+8, mid, c.gx, mid, true, false)
	}
	// Within the 8px the drawing leaves right of the gallery column.
	b.f(`<rect x="%g" y="%g" width="%g" height="%g" fill="none" class="tp-slink" stroke-dasharray="2 3" opacity="0.55"/>`, c.gx-6, alY-12, c.gw+12, imY+imH+12-(alY-12))
	iy := imY + imH + 30
	if !stopped {
		b.line(cx, imY+imH, cx, iy, true, false)
	}
	img := p.Image
	var imgLines []pathLine
	for _, ln := range img.Lines {
		for _, w := range wrapParts(ln.Parts, c.gw-16, 11) {
			imgLines = append(imgLines, pathLine{Parts: w, Href: ln.Href})
		}
	}
	ih := 26 + 15*float64(len(imgLines)) + 4
	if img.State == "on" {
		b.rect(c.gx, iy, c.gw, ih, "sfg", false, 0)
	} else {
		b.rect(c.gx, iy, c.gw, ih, "sdim", true, 0)
	}
	titleClass := "fg"
	if stopped {
		titleClass = "dim"
	}
	b.text(c.gx+8, iy+17, img.Title, titleClass, 12, "start", true)
	for i, ln := range imgLines {
		o := b.open(ln.Href, "")
		b.spans(c.gx+8, iy+34+float64(i)*15, ln.Parts, 11, c.gw-16)
		b.close(o)
	}
	h := max(lanesBottom, iy+ih+10)
	if ml {
		b.f(`<line x1="%g" y1="0" x2="%g" y2="%g" class="tp-sborder" stroke-dasharray="1 4"/>`, c.div, c.div, h)
	}
	return template.HTML(fmt.Sprintf(`<svg viewBox="0 0 %g %g" width="%g" role="img" aria-label="Where %s comes from">`,
		c.w, h, c.w, html.EscapeString(tag.Name)) + b.String() + `</svg>`)
}
