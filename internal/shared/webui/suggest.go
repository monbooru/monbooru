package webui

import (
	"html/template"
	"regexp"
	"strings"
)

// SuggestItem is one row of a "webui/suggest_list"; Color is a category's
// hex colour, Alias the alias the row was found through.
type SuggestItem struct {
	Name        string
	Alias       string
	Color       string
	Description string
	UsageCount  int
	ShowCount   bool
}

// SuggestList is what "webui/suggest_list" renders. Attr names the data
// attribute that carries each row's name and OnClick what a click runs;
// both go out unescaped, so only ever set them from constants.
type SuggestList struct {
	Attr    template.HTMLAttr
	OnClick template.HTMLAttr
	Items   []SuggestItem
}

var categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// Keyed on the colour, not the category, so a theme can remap the shipped
// palette while an operator's own colour falls through. template.CSS to
// pass the style-attribute sanitizer: only a validated colour gets in.
func CategoryColor(color string) template.CSS {
	if !categoryColorRe.MatchString(color) {
		return "#888888"
	}
	return template.CSS("var(--cat-" + strings.ToLower(strings.TrimPrefix(color, "#")) + "," + color + ")")
}
