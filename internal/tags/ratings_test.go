package tags

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestRatingLevelsAgreeEverywhere(t *testing.T) {
	if got := searchkw.Expansions["rating"]; !slices.Equal(got, contract.RatingLevels) {
		t.Errorf("rating: offers %v, want %v", got, contract.RatingLevels)
	}
	for file, pattern := range map[string]string{
		"../db/schema.sql":   `\('(\w+)',\s+\(SELECT id FROM tag_categories WHERE name = 'rating'\)\)`,
		"../db/bootstrap.go": `WHEN '(\w+)' THEN \d`,
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(string(src), -1) {
			names = append(names, m[1])
		}
		if !slices.Equal(names, contract.RatingLevels) {
			t.Errorf("%s spells the ratings %v, want %v", file, names, contract.RatingLevels)
		}
	}
}
