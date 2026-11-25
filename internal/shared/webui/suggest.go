package webui

import (
	"html/template"
	"regexp"
	"strings"
)

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
