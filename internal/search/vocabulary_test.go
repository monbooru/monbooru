package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestMimeOffersEveryFileType(t *testing.T) {
	var names []string
	for _, ft := range models.FileTypes {
		names = append(names, ft.Name)
	}
	if got := searchkw.Expansions["mime"]; !slices.Equal(got, names) {
		t.Errorf("mime: offers %v, the file types are %v", got, names)
	}
}

func TestOrdinalKeysAreTheOnesOfferingARange(t *testing.T) {
	for key, vals := range searchkw.Expansions {
		if got := slices.Contains(vals, ".."); got != searchkw.IsOrdinal(key) {
			t.Errorf("%s: offers a range %v, ordinal %v", key, got, searchkw.IsOrdinal(key))
		}
	}
}

func TestEveryKeywordHasAFilterBuilder(t *testing.T) {
	var built []string
	for k := range filterBuilders {
		if k != "system" {
			built = append(built, k)
		}
	}
	slices.Sort(built)
	want := slices.Sorted(slices.Values(searchkw.Keywords))
	if !slices.Equal(built, want) {
		t.Errorf("filter builders %v, keywords %v", built, want)
	}
}

func TestEveryOfferedValueBuildsAPredicate(t *testing.T) {
	operators := []string{">=", "<=", ">", "<", "=", ".."}
	for key, vals := range searchkw.Expansions {
		for _, val := range vals {
			if slices.Contains(operators, val) {
				continue
			}
			b := &whereBuilder{
				relPresence:         relationPresence{dup: true, alt: true, version: true, derivative: true, series: true},
				relPresenceResolved: true,
				ratingIDs:           map[string]int64{},
				ratingUsage:         map[string]int64{},
				ratingResolved:      true,
				lookupBackends:      []string{contract.LookupPTR, contract.LookupBooru},
			}
			for i, level := range contract.RatingLevels {
				b.ratingIDs[level] = int64(i + 1)
				b.ratingUsage[level] = 1
			}
			if got := b.buildFilterExpr(FilterExpr{Key: key, Val: val}); strings.Contains(got, "1=0") {
				t.Errorf("%s:%s is offered but builds %q", key, val, got)
			}
		}
	}
}
