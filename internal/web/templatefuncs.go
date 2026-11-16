package web

import (
	"cmp"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/webui"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

type originTagGroup struct {
	Origin string
	Stale  bool
	Tags   []models.Tag
}

type originImplicationGroup struct {
	Origin       string
	Stale        bool
	Implications []models.Implication
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"seq": func(start, end int) []int {
			r := make([]int, 0, end-start+1)
			for i := start; i <= end; i++ {
				r = append(r, i)
			}
			return r
		},
		// template.CSS to pass the style-attribute sanitizer;
		// categoryColor admits only a validated colour.
		"catColor":   func(color string) template.CSS { return template.CSS(categoryColor(color)) },
		"catDefault": tags.DefaultCategoryColor,
		// template.URL, or html/template would re-escape each % and
		// double-encode the link.
		"urlQ": func(s string) template.URL {
			return template.URL(url.QueryEscape(s))
		},
		"qval": search.QuoteValue,
		"pct":  func(f float64) int { return int(math.Round(f * 100)) },
		"list": func(vs ...any) []any { return vs },
		"groupByCategory": func(tagList []models.Tag) []tagGroup {
			return groupOrdered(tagList, nil,
				func(t models.Tag) string { return t.CategoryName },
				func(t models.Tag) *tagGroup { return &tagGroup{Name: t.CategoryName, Color: t.CategoryColor} },
				func(g *tagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"deref":    derefOr[int],
		"deref64":  derefOr[int64],
		"deref64f": derefOr[float64],
		// A seed past MaxInt64 is stored as its two's-complement int64.
		"seedText": func(p *int64) string { return strconv.FormatUint(uint64(*p), 10) },
		"elideHash": func(h string) string {
			if len(h) <= 19 {
				return h
			}
			return h[:8] + "..." + h[len(h)-8:]
		},
		"phashHex": func(p *int64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprintf("%016x", uint64(*p))
		},
		"upgradable":     upgrade.Eligible,
		"upgradeCompare": upgradeCompare,
		"isPTRSite": func(site string) bool {
			return strings.EqualFold(strings.TrimSpace(site), "ptr")
		},
		"groupTagsByOrigin": func(list []models.Tag) []originTagGroup {
			return groupByOriginStale(list,
				func(t models.Tag) string { return t.Origin },
				func(t models.Tag) bool { return t.Stale },
				func(t models.Tag) *originTagGroup { return &originTagGroup{Origin: t.Origin, Stale: t.Stale} },
				func(g *originTagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"groupImplicationsByOrigin": func(list []models.Implication) []originImplicationGroup {
			return groupByOriginStale(list,
				func(im models.Implication) string { return im.Origin },
				func(im models.Implication) bool { return im.Stale },
				func(im models.Implication) *originImplicationGroup {
					return &originImplicationGroup{Origin: im.Origin, Stale: im.Stale}
				},
				func(g *originImplicationGroup, im models.Implication) { g.Implications = append(g.Implications, im) })
		},
		"originLabel": func(kinds map[string]string, origin string) string {
			switch {
			case origin == "":
				return "an unrecorded source"
			case kinds[origin] == "user":
				return "the user"
			case kinds[origin] == "ptr" || strings.EqualFold(origin, "ptr"):
				return "the Public Tag Repository"
			default:
				return origin
			}
		},
		"cancelTitle": cancelTitle,
		"jobLabel":    jobLabel,
		"localTime": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"localTimePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"localDate":          localDay,
		"monloaderOffReason": monloaderOffReason,
		"lookupResultLabel": func(result string) string {
			if result == "hit" {
				return "tags applied"
			}
			return "no match"
		},
		"browseSortLabel": browseSortLabel,
		"tokenHint":       tokenHint,
		"addedViaLabel":   addedViaLabel,
		"isLongValue": func(s string) bool {
			return len(s) > 200 || strings.ContainsAny(s, "\n\r")
		},
		"schedDuration": func(d time.Duration) string {
			if d >= time.Second {
				return d.Round(time.Second).String()
			}
			return d.Round(time.Millisecond).String()
		},
		"minusDuration": func(a, b time.Duration) time.Duration {
			return a - b
		},
		"int64Duration": func(d time.Duration) int64 {
			return int64(d)
		},
		"plural":      models.Plural,
		"abbrevCount": abbrevCount,
		"searchTime":  searchTime,
		// Always quoted: a term may carry spaces the parser would split on.
		"comfySearch": func(term string) string {
			if term == "" {
				return ""
			}
			return `comfyui:"` + search.QuoteValue(term) + `"`
		},
		"comfyRefTarget": func(s string) string {
			return strings.TrimPrefix(s, "→ ")
		},
		"hasPrefix":     strings.HasPrefix,
		"providerLabel": config.ExecutionProviderLabel,
		"urlDomain": func(s string) string {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				return s
			}
			return strings.TrimPrefix(u.Host, "www.")
		},
		"truncate": truncateRunes,
		"hasFavFilter": func(query string) bool {
			return slices.ContainsFunc(strings.Fields(query), func(tok string) bool { return strings.EqualFold(tok, "fav:true") })
		},
		"pageLoadMs": func(t time.Time) int64 {
			if t.IsZero() {
				return 0
			}
			return time.Since(t).Milliseconds()
		},
	}
}

func localDay(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// Only job types whose worker observes ctx.Done() belong here: a missing
// entry renders no cancel button.
var cancelTitles = map[string]string{
	"autotag":         "Stop auto-tagging",
	"sync":            "Stop syncing",
	"delete":          "Stop deleting",
	"re-extract":      "Stop re-extraction",
	"rebuild-thumbs":  "Stop thumbnail rebuild",
	"prune-thumbs":    "Stop thumbnail prune",
	"hashes":          "Stop hash backfill",
	"meta-tags":       "Stop the meta tag pass",
	"index-workflows": "Stop workflow indexing",
	"relations":       "Stop find-pairs",
	"lookup":          "Stop the lookup",
	"move":            "Stop moving",
	"tag":             "Stop tagging",
	"transfer":        "Stop the transfer",
	"merge":           "Stop the merge",
	"check":           "Stop the check",
}

func cancelTitle(jobType string) string { return cancelTitles[jobType] }

var runningJobNames = map[string]string{
	"autotag":         "Auto-tagging",
	"sync":            "A gallery sync",
	"delete":          "A delete",
	"re-extract":      "A re-extraction",
	"rebuild-thumbs":  "A thumbnail rebuild",
	"prune-thumbs":    "A thumbnail prune",
	"prune-dirs":      "A folder prune",
	"hashes":          "A hash backfill",
	"meta-tags":       "A meta tag pass",
	"index-workflows": "A workflow indexing run",
	"relations":       "A find-pairs run",
	"lookup":          "A lookup",
	"move":            "A move",
	"tag":             "A tagging job",
	"transfer":        "A transfer",
	"merge":           "A merge",
	"vacuum":          "A vacuum",
	"free-memory":     "A memory reclaim",
	"fold":            "A fold",
	"check":           "A check",
}

func runningJobName(jobType string) string { return cmp.Or(runningJobNames[jobType], "A job") }

var jobLabels = map[string]string{
	"autotag":         "auto-tag",
	"tag":             "tagging",
	"rebuild-thumbs":  "thumbnails",
	"prune-thumbs":    "orphaned thumbnails",
	"prune-dirs":      "empty folders",
	"meta-tags":       "meta tags",
	"index-workflows": "workflow index",
	"relations":       "find pairs",
	"free-memory":     "free memory",
	"fold":            "old spellings",
	"check":           "name check",
}

func jobLabel(jobType string) string { return cmp.Or(jobLabels[jobType], jobType) }

var browseSortLabels = map[string]string{
	"recent":         "Recent",
	"size":           "Size",
	"original_added": "Original added",
	"length":         "Length",
	"newest_member":  "Newest member",
}

func browseSortLabel(s string) string { return cmp.Or(browseSortLabels[s], s) }

var tokenHints = map[string]string{
	"{name}":    "current file name, no extension",
	"{ext}":     "extension, e.g. png",
	"{id}":      "image ID, e.g. 1042",
	"{hash:8}":  "first 8 characters of the file hash, e.g. 3fa2c91b",
	"{md5}":     "MD5 of the file",
	"{date}":    "day the image was added, e.g. 2026-10-25",
	"{year}":    "year the image was added, e.g. 2026",
	"{month}":   "month the image was added, e.g. 10",
	"{gallery}": "gallery name, e.g. default",
	"{type}":    "detected type, e.g. png, gif, cbz",
	"{folder}":  "the image's current folder",
	"{n}":       "01, 02, ... in the order of the selection",
	"{source}":  "site the sender gave, e.g. danbooru",
	"{post_id}": "post number on that site, e.g. 4821337",
}

func tokenHint(token string) string { return tokenHints[token] }

var addedViaLabels = map[string]string{
	models.OriginIngest:   "found on disk",
	models.OriginExtract:  "extracted from an archive",
	models.OriginGenerate: "built from a collection",
}

func addedViaLabel(origin string) string { return cmp.Or(addedViaLabels[origin], origin) }

var monloaderOffReasons = map[string]string{
	peer.ConnPaused:   "monloader is paused",
	peer.ConnRejected: "monloader rejected the token",
}

func monloaderOffReason(conn string) string {
	return cmp.Or(monloaderOffReasons[conn], "monloader is not responding")
}

// At most four glyphs; the unit promotes at 999500 because anything
// higher rounds to "1000k".
func abbrevCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 999500:
		return abbrevUnit(n, 1000, "k")
	default:
		return abbrevUnit(n, 1000000, "M")
	}
}

// Three significant digits, as a cached search takes well under a
// millisecond; the unit promotes at 999.5 ms, which rounds to "1000".
func searchTime(d time.Duration) string {
	if d < 999500*time.Microsecond {
		return threeDigits(float64(d)/float64(time.Millisecond)) + " ms"
	}
	return threeDigits(d.Seconds()) + " s"
}

// Fixed-point rather than %g, which turns a long search into "1.23e+03".
func threeDigits(v float64) string {
	dec := 2
	for x := v; x >= 10 && dec > 0; x /= 10 {
		dec--
	}
	for x := v; x > 0 && x < 1; x *= 10 {
		dec++
	}
	s := strconv.FormatFloat(v, 'f', dec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func abbrevUnit(n, div int, suffix string) string {
	if tenths := (n*10 + div/2) / div; tenths < 100 {
		return fmt.Sprintf("%d.%d%s", tenths/10, tenths%10, suffix)
	}
	return fmt.Sprintf("%d%s", (n+div/2)/div, suffix)
}

func groupByOriginStale[T, G any](items []T, origin func(T) string, stale func(T) bool, newGroup func(T) *G, add func(*G, T)) []G {
	fresh := func(it T) bool { return !stale(it) }
	return append(groupOrdered(items, stale, origin, newGroup, add), groupOrdered(items, fresh, origin, newGroup, add)...)
}

// The result carries its own separator: a bare space when the post
// published nothing.
func upgradeCompare(s models.ImageSource, img models.Image) string {
	post := fileFacts(s.PostWidth, s.PostHeight, s.PostExt, s.PostSize)
	if post == "" {
		return " "
	}
	label := cmp.Or(s.Site, "the post")
	pad := max(len("yours"), len(label)) + 2
	return fmt.Sprintf("\n\n%-*s%s\n%-*s%s\n\n",
		pad, "yours", fileFacts(derefOr(img.Width), derefOr(img.Height), strings.ToLower(img.FileType), img.FileSize),
		pad, label, post)
}

func fileFacts(w, h int, ext string, size int64) string {
	var parts []string
	if w > 0 && h > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", w, h))
	}
	if ext != "" {
		parts = append(parts, ext)
	}
	line := strings.Join(parts, " ")
	if size > 0 {
		if line != "" {
			line += " - "
		}
		line += webui.HumanBytes(size)
	}
	return line
}

func derefOr[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// Keyed on the colour, not the category, so a theme can remap the shipped
// palette while an operator's own colour falls through.
func categoryColor(color string) string {
	if !tags.IsValidCategoryColor(color) {
		return tags.SafeCategoryColor(color)
	}
	return "var(--cat-" + strings.ToLower(strings.TrimPrefix(color, "#")) + "," + color + ")"
}
