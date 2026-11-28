package tagger

import (
	"fmt"
	"sort"
)

// Rule is "" for the model's own routing, "default" for an embedded rule
// and "custom" for an overlay rule.
type LabelView struct {
	Source  string
	CatName string // effective category; "" when Muted
	TagName string // effective tag name (rename applied)
	Muted   bool
	Rule    string
}

// The single_general inferred-category lift needs a job's database, so
// those labels show their static routing.
func BrowseLabels(dataPath, taggerName string, catIDs map[string]int64) ([]LabelView, error) {
	h, ok := CurrentPool().heldModel(taggerName)
	if !ok {
		return nil, fmt.Errorf("no worker holds tagger %q", taggerName)
	}
	if len(h.labels.Labels) == 0 {
		return nil, fmt.Errorf("its worker has not sent the labels of %q", taggerName)
	}
	profile := profileOf(h.labels)
	labels := toTagLabels(h.labels)
	embedded := compileEntries(stockRules(h.labels), catIDs)
	overlay := compileEntries(OverlayDispatchRules(dataPath, taggerName), catIDs)
	empty := &DispatchTable{}

	out := make([]LabelView, 0, len(labels))
	for _, l := range labels {
		if l.placeholder {
			continue
		}
		v := LabelView{Source: l.name, TagName: l.name}
		rule, ok := overlay[l.name]
		if ok {
			v.Rule = "custom"
		} else if rule, ok = embedded[l.name]; ok {
			v.Rule = "default"
		}
		if ok {
			if rule.Drop {
				v.Muted = true
			} else {
				v.CatName = rule.CatName
				if rule.Name != "" {
					v.TagName = rule.Name
				}
			}
		} else {
			v.CatName = resolveCategory(profile, l, catIDs, empty).catName
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

func compileEntries(entries []DispatchEntry, catIDs map[string]int64) map[string]DispatchRule {
	out := map[string]DispatchRule{}
	for _, e := range entries {
		if rule, ok := compileDispatchRule(e, catIDs); ok {
			out[e.Source] = rule
		}
	}
	return out
}
