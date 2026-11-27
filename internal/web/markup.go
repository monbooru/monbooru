package web

import (
	"html/template"
	"maps"
	"net/url"
	"slices"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/markup"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

// One batched read per kind, not one per reference: a page of labelled
// boxes would otherwise cost a query each.
func (s *Server) resolveMarkup(refs markup.Refs) markup.Resolver {
	var res markup.Resolver
	if len(refs.Tags) > 0 {
		names := slices.Collect(maps.Keys(refs.Tags))
		res.Tags = make(map[string]markup.TagRef, len(names))
		for name, target := range gallery.ResolveTagRefs(s.db(), names) {
			ref := markup.TagRef{
				Href:  "/?q=" + url.QueryEscape(name),
				Known: target.Found,
			}
			if target.Found {
				ref.Color = string(webui.CategoryColor(target.Color))
			}
			res.Tags[name] = ref
		}
	}
	if len(refs.Images) > 0 {
		res.Images = gallery.ExistingImageIDs(s.db(), slices.Collect(maps.Keys(refs.Images)))
	}
	if len(refs.URLs) > 0 {
		res.Links = gallery.ImageIDsBySourceURL(s.db(), slices.Collect(maps.Keys(refs.URLs)))
	}
	return res
}

func (s *Server) renderMarkup(body string) template.HTML {
	doc := markup.Parse(body)
	refs := markup.NewRefs()
	doc.Collect(refs)
	return doc.Render(s.resolveMarkup(refs))
}
