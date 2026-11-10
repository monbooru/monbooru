package tags

import (
	"math"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func seedTagWeight(seed SimilaritySeed, tagID int64) float64 {
	for _, t := range seed.Tags {
		if int64(t.TagID) == tagID {
			return t.Weight
		}
	}
	return 0
}

func TestTagWeight_RarityAndCategory(t *testing.T) {
	const visible = 1000
	rare := tagWeight(visible, 10, "general")
	common := tagWeight(visible, 500, "general")
	if rare <= common {
		t.Fatalf("rare tag weight %v should exceed common %v", rare, common)
	}
	if want := math.Log(100); math.Abs(rare-want) > 1e-9 {
		t.Fatalf("general weight = %v, want %v", rare, want)
	}
	for _, tc := range []struct {
		category string
		mult     float64
	}{
		{"artist", 3},
		{"character", 1},
		{"copyright", 1},
		{"general", 1},
		{"medium", 1},
	} {
		got := tagWeight(visible, 10, tc.category)
		if want := math.Log(100) * tc.mult; math.Abs(got-want) > 1e-9 {
			t.Errorf("%s weight = %v, want %v", tc.category, got, want)
		}
	}
}

func TestTagWeight_UbiquitousTagWeighsNothing(t *testing.T) {
	if w := tagWeight(1000, 1000, "general"); w != 0 {
		t.Fatalf("tag on every visible image weighs %v, want 0", w)
	}
	if w := tagWeight(1000, 1200, "artist"); w != 0 {
		t.Fatalf("over-count usage weighs %v, want 0", w)
	}
	if w := tagWeight(1000, 0, "general"); w != 0 {
		t.Fatalf("unused tag weighs %v, want 0", w)
	}
}

func TestLoadSimilaritySeed_ExcludesMetaAndOverCap(t *testing.T) {
	database, svc := setupTestDB(t)
	for i := 0; i < 40; i++ {
		insertTestImage(t, database, "filler"+string(rune('a'+i)))
	}
	seedID := insertTestImage(t, database, "seed")

	general := categoryIDOf(t, svc, "general")
	meta := categoryIDOf(t, svc, "meta")
	rareID := insertTag(t, database, "plaid_scarf", general, 2)
	metaID := insertTag(t, database, "highres", meta, 3)
	popularID := insertTag(t, database, "everywhere", general, SimilarMaxTagUsage+1)
	for _, id := range []int64{rareID, metaID, popularID} {
		attachImageTag(t, database, seedID, id)
	}

	seed, err := LoadSimilaritySeed(database, seedID, visibleImages(t, database))
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.Tags) != 1 || int64(seed.Tags[0].TagID) != rareID {
		t.Fatalf("counted tags = %+v, want only the rare general tag %d", seed.Tags, rareID)
	}
	if seedTagWeight(seed, metaID) != 0 {
		t.Error("meta tag reached the counted set")
	}
	if seedTagWeight(seed, popularID) != 0 {
		t.Error("over-cap tag reached the counted set")
	}
	if w := seed.Tags[0].Weight; math.Abs(seed.Norm-w) > 1e-9 {
		t.Fatalf("norm = %v, want the single tag's weight %v", seed.Norm, w)
	}
}

func TestSimilaritySeed_CappedTagsScoreButDoNotSeed(t *testing.T) {
	seed := SimilaritySeed{
		Tags: []SimilarityTag{
			{TagID: 1, Weight: 6, Seeds: true},
			{TagID: 2, Weight: 2, Seeds: false},
		},
		Norm: 8,
	}
	if got := seed.TagIDs(); len(got) != 2 {
		t.Fatalf("counted ids = %v, want both tags", got)
	}
	if got := SimilarityScore(8, seed.Norm, 8); got != 1 {
		t.Fatalf("score = %v, want the capped tag counted in the shared weight", got)
	}
}

func TestEvidenceUsageCap_FollowsTheLibrary(t *testing.T) {
	for _, tc := range []struct {
		visible, want int64
	}{
		{0, 3},
		{24, 3},
		{3200, 320},
		{108000, SimilarMaxTagUsage},
		{1000000, SimilarMaxTagUsage},
	} {
		if got := evidenceUsageCap(tc.visible); got != tc.want {
			t.Errorf("evidenceUsageCap(%d) = %d, want %d", tc.visible, got, tc.want)
		}
	}
}

func TestLoadSimilaritySeed_CommonTagCarriesWeightButNotEvidence(t *testing.T) {
	database, svc := setupTestDB(t)
	for i := 0; i < 40; i++ {
		insertTestImage(t, database, "cap_filler"+string(rune('a'+i)))
	}
	seedID := insertTestImage(t, database, "cap_seed")
	general := categoryIDOf(t, svc, "general")

	commonID := insertTag(t, database, "brown_hair", general, 8)
	rareID := insertTag(t, database, "plaid_scarf", general, 2)
	for _, id := range []int64{commonID, rareID} {
		attachImageTag(t, database, seedID, id)
	}

	seed, err := LoadSimilaritySeed(database, seedID, visibleImages(t, database))
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.Tags) != 2 {
		t.Fatalf("counted tags = %+v, want both", seed.Tags)
	}
	for _, tag := range seed.Tags {
		want := int64(tag.TagID) == rareID
		if tag.Seeds != want {
			t.Errorf("tag %d seeds = %v, want %v", tag.TagID, tag.Seeds, want)
		}
	}
	if seedTagWeight(seed, commonID) <= 0 {
		t.Error("the common tag lost its weight instead of only its evidence")
	}
}

func TestOverlapScore_RewardsBreadth(t *testing.T) {
	if got := OverlapScore(10, 10, 10); math.Abs(got-1) > 1e-9 {
		t.Fatalf("identical sets = %v, want 1", got)
	}
	wide := OverlapScore(12, 15, 21)
	narrow := OverlapScore(3, 3, 21)
	if wide <= narrow {
		t.Fatalf("wide overlap %v did not beat the sparse match %v", wide, narrow)
	}
	if got := OverlapScore(0, 10, 10); got != 0 {
		t.Fatalf("nothing shared = %v, want 0", got)
	}
	if got := OverlapScore(1, 0, 0); got != 0 {
		t.Fatalf("untagged pair = %v, want 0", got)
	}
}

func TestMinShared_NeverDropsAReachableScore(t *testing.T) {
	seed := OverlapSeed{TagIDs: []int64{1, 2, 3, 4, 5}}
	for _, threshold := range []float64{0, 0.1, 0.25, 0.5, 0.8, 1} {
		need := seed.MinShared(threshold)
		for n := 0; n <= len(seed.TagIDs); n++ {
			for c := n; c <= 40; c++ {
				scored := OverlapScore(n, len(seed.TagIDs), c) >= threshold
				if scored && n < need {
					t.Fatalf("threshold %v: %d shared of %d counted scores in but the bound needs %d",
						threshold, n, c, need)
				}
			}
		}
	}
}

func TestLoadSimilaritySeed_UntaggedSeedHasNothingToMatch(t *testing.T) {
	database, _ := setupTestDB(t)
	seedID := insertTestImage(t, database, "bare")
	seed, err := LoadSimilaritySeed(database, seedID, visibleImages(t, database))
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.Tags) != 0 || seed.Norm != 0 {
		t.Fatalf("untagged seed = %+v, want empty", seed)
	}
	if got := SimilarityScore(1, seed.Norm, 5); got != 0 {
		t.Fatalf("score against an empty seed = %v, want 0", got)
	}
}

func TestSimilaritySeed_ScoreBounds(t *testing.T) {
	seed := SimilaritySeed{Norm: 4, Tags: []SimilarityTag{{TagID: 1, Weight: 2}}}
	if got := SimilarityScore(4, seed.Norm, 4); got != 1 {
		t.Fatalf("identical-set score = %v, want 1", got)
	}
	if got := SimilarityScore(2, seed.Norm, 4); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("score = %v, want 0.5", got)
	}
	if got := SimilarityScore(0, seed.Norm, 4); got != 0 {
		t.Fatalf("score with nothing shared = %v, want 0", got)
	}
	if got := SimilarityScore(1, seed.Norm, 0); got != 0 {
		t.Fatalf("score against an empty candidate = %v, want 0", got)
	}
}

func insertTag(t *testing.T, database *db.DB, name string, categoryID, usage int64) int64 {
	t.Helper()
	var id int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?) RETURNING id`,
		name, categoryID, usage,
	).Scan(&id); err != nil {
		t.Fatalf("insert tag %q: %v", name, err)
	}
	return id
}

func attachImageTag(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imageID, tagID)
}

func TestOverlapPercentsAgainst_ScoresOnlyTheGivenIDs(t *testing.T) {
	database, svc := setupTestDB(t)
	for i := 0; i < 20; i++ {
		insertTestImage(t, database, "sp_filler"+string(rune('a'+i)))
	}
	seedID := insertTestImage(t, database, "sp_seed")
	twin := insertTestImage(t, database, "sp_twin")
	partial := insertTestImage(t, database, "sp_partial")
	stranger := insertTestImage(t, database, "sp_stranger")

	general := categoryIDOf(t, svc, "general")
	shared := make([]int64, 0, 4)
	for _, name := range []string{"plaid_scarf", "ice_wings", "knee_boots", "winter_coat"} {
		shared = append(shared, insertTag(t, database, name, general, 3))
	}
	for _, id := range shared {
		attachImageTag(t, database, seedID, id)
		attachImageTag(t, database, twin, id)
	}
	attachImageTag(t, database, partial, shared[0])
	attachImageTag(t, database, stranger, insertTag(t, database, "unrelated", general, 1))

	got, err := OverlapPercentsAgainst(database, seedID, []int64{twin, partial, stranger}, visibleImages(t, database))
	if err != nil {
		t.Fatal(err)
	}
	if got[twin] != 100 {
		t.Errorf("twin scored %d%%, want a full overlap of 100", got[twin])
	}
	if got[partial] <= 0 || got[partial] >= got[twin] {
		t.Errorf("partial scored %d%%, want between 0 and the twin's %d", got[partial], got[twin])
	}
	if _, ok := got[stranger]; ok {
		t.Errorf("an image sharing nothing got a score: %d", got[stranger])
	}
	empty, err := OverlapPercentsAgainst(database, seedID, nil, visibleImages(t, database))
	if err != nil || empty != nil {
		t.Fatalf("empty id set = %v, %v; want nil, nil", empty, err)
	}
}

func visibleImages(t *testing.T, database *db.DB) int {
	t.Helper()
	return countRows(t, database, "images WHERE is_missing = 0")
}
