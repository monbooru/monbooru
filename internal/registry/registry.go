package registry

import "embed"

//go:embed plugins/themes/light plugins/themes/legacy-dark plugins/themes/legacy-light
var FS embed.FS

const (
	LightThemeDir       = "plugins/themes/light"
	LegacyDarkThemeDir  = "plugins/themes/legacy-dark"
	LegacyLightThemeDir = "plugins/themes/legacy-light"
)
