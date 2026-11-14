package monloader

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

// How this catalog holds a spelling the PTR named.
type Standing int

const (
	StandingFree Standing = iota
	StandingSelf
	StandingHeld
	StandingAliasElsewhere
	StandingTag
	StandingInvalid
)

// Adds is whether the pull, or for the looked-up spelling the alias,
// would adopt it.
type LookupRow struct {
	Name      string
	Standing  Standing
	Canonical string
	Usage     int
	Adds      bool
}

type LookupGroupKind int

const (
	LookupAliases LookupGroupKind = iota
	LookupImplies
	LookupImpliedBy
)

type LookupGroup struct {
	Kind LookupGroupKind
	Rows []LookupRow
	Adds int
}

func ptrLookupAliasRow(tagSvc func() *tags.Service, tagID int64, name string) LookupRow {
	if !tagSvc().FormValid(name) {
		return LookupRow{Name: name, Standing: StandingInvalid}
	}
	row := formStanding(tagSvc, name, tagID)
	row.Adds = row.Standing == StandingFree
	return row
}

// An alias of self is held; self 0 reads every alias as filed elsewhere.
func formStanding(tagSvc func() *tags.Service, name string, self int64) LookupRow {
	row := LookupRow{Name: name}
	held, exists := tagSvc().FormRow(name)
	switch {
	case !exists:
		row.Standing = StandingFree
	case held.IsAlias && held.CanonicalID == self:
		row.Standing = StandingHeld
	case held.IsAlias:
		row.Standing, row.Canonical = StandingAliasElsewhere, held.CanonicalName
	default:
		row.Standing, row.Usage = StandingTag, held.Usage
	}
	return row
}

func ptrLookupImplRow(tagSvc func() *tags.Service, name string, declared map[string]bool) LookupRow {
	if declared[localForm(tagSvc, name)] {
		return LookupRow{Name: name, Standing: StandingHeld}
	}
	if !tagSvc().FormValid(name) {
		return LookupRow{Name: name, Standing: StandingInvalid}
	}
	row := formStanding(tagSvc, name, 0)
	row.Adds = row.Standing == StandingFree || row.Standing == StandingTag
	return row
}

func LookupGroups(tagSvc func() *tags.Service, id int64, tagForm, spelling string, info contract.PTRTagInfo) ([]LookupGroup, error) {
	implications, err := tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	localImplied, localImpliedBy := map[string]bool{}, map[string]bool{}
	for _, im := range implications {
		localImplied[tags.FormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	for _, im := range impliedBy {
		localImpliedBy[tags.FormName(im.ParentCategoryName, im.ParentName)] = true
	}
	seen := map[string]bool{tagForm: true}
	aliases := LookupGroup{Kind: LookupAliases}
	for _, name := range append([]string{spelling, info.Ideal}, info.Aliases...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		row := ptrLookupAliasRow(tagSvc, id, name)
		if row.Adds {
			aliases.Adds++
		}
		aliases.Rows = append(aliases.Rows, row)
	}
	implies := ptrLookupImplGroup(tagSvc, LookupGroup{Kind: LookupImplies},
		info.Implications, localImplied, tagForm, spelling)
	implied := ptrLookupImplGroup(tagSvc, LookupGroup{Kind: LookupImpliedBy},
		info.ImpliedBy, localImpliedBy, tagForm, spelling)
	var groups []LookupGroup
	for _, g := range []LookupGroup{aliases, implies, implied} {
		if len(g.Rows) > 0 {
			groups = append(groups, g)
		}
	}
	return groups, nil
}

func ptrLookupImplGroup(tagSvc func() *tags.Service, g LookupGroup, names []string, declared map[string]bool, tagForm, spelling string) LookupGroup {
	for _, name := range names {
		if name == "" || name == tagForm || name == spelling {
			continue
		}
		row := ptrLookupImplRow(tagSvc, name, declared)
		if row.Adds {
			g.Adds++
		}
		g.Rows = append(g.Rows, row)
	}
	return g
}

func LookupLocalStatus(tagSvc func() *tags.Service, id int64, tagForm, spelling string) LookupRow {
	if spelling == tagForm {
		return LookupRow{Name: spelling, Standing: StandingSelf}
	}
	row := formStanding(tagSvc, spelling, id)
	row.Adds = row.Standing != StandingHeld
	return row
}

// Longest first: an operator's spelling is usually the repository's with
// something added.
func ptrSpellingStems(form string) []string {
	cat, name, qualified := strings.Cut(form, ":")
	if !qualified {
		cat, name = "", form
	}
	var out []string
	seen := map[string]bool{}
	push := func(n string) {
		if utf8.RuneCountInString(n) < 4 || seen[n] {
			return
		}
		seen[n] = true
		if cat != "" {
			n = cat + ":" + n
		}
		out = append(out, n)
	}
	push(name)
	stem := name
	for strings.HasSuffix(stem, ")") && strings.Contains(stem, "_(") {
		stem = stem[:strings.LastIndex(stem, "_(")]
		push(stem)
	}
	for parts := strings.Split(stem, "_"); len(parts) > 1; {
		parts = parts[:len(parts)-1]
		push(strings.Join(parts, "_"))
	}
	return out
}

// The tag's own name often sits in the index as an orphan, so a stem
// whose clusters carry relations beats the first stem that answers;
// ownHeld says the tag's own name answered all the same.
func (l *Link) PTRSeedSearch(ctx context.Context, tagForm string) (clusters []contract.PTRCluster, stem string, truncated, ownHeld bool, err error) {
	for _, candidate := range ptrSpellingStems(tagForm) {
		got, cut, err := l.PTRSpellingSearch(ctx, candidate, "", contract.PTRSearchLimit)
		if err != nil {
			return nil, "", false, false, err
		}
		if len(got) == 0 {
			continue
		}
		ownHeld = ownHeld || candidate == tagForm
		if clusters == nil {
			clusters, stem, truncated = got, candidate, cut
		}
		for _, c := range got {
			if c.Aliases+c.Implications+c.ImpliedBy > 0 {
				return got, candidate, cut, ownHeld, nil
			}
		}
	}
	return clusters, stem, truncated, ownHeld, nil
}
