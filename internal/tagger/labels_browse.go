package tagger

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
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
func BrowseLabels(modelPath, taggerName, tagsFile string, catIDs map[string]int64) ([]LabelView, error) {
	profile, err := ResolveProfile(modelPath, taggerName, tagsFile)
	if err != nil {
		return nil, err
	}
	labels, err := browsedLabels(filepath.Join(modelPath, taggerName, tagsFile), profile)
	if err != nil {
		return nil, err
	}
	embedded := compileEntries(DefaultDispatchRules(modelPath, taggerName), catIDs)
	overlay := compileEntries(OverlayDispatchRules(modelPath, taggerName), catIDs)
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

// The tag page and the labels panel read a tagger's whole label file on
// every view, so the parse is kept until the file or its profile changes.
var labelFiles = struct {
	sync.Mutex
	byPath map[string]parsedLabels
}{byPath: map[string]parsedLabels{}}

type parsedLabels struct {
	stamp  string
	labels []tagLabel
}

func browsedLabels(path string, profile Profile) ([]tagLabel, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	stamp := fi.ModTime().String() + "/" + strconv.FormatInt(fi.Size(), 10) + "/" + profile.fingerprint()
	labelFiles.Lock()
	cached, ok := labelFiles.byPath[path]
	labelFiles.Unlock()
	if ok && cached.stamp == stamp {
		return cached.labels, nil
	}
	labels, err := loadLabels(path, profile)
	if err != nil {
		return nil, err
	}
	labelFiles.Lock()
	labelFiles.byPath[path] = parsedLabels{stamp, labels}
	labelFiles.Unlock()
	return labels, nil
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
