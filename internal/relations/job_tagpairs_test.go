package relations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

type tagPairFixture struct {
	seed, near, far, stranger int64
}

func setupTagPairFixture(t *testing.T, database *db.DB) tagPairFixture {
	t.Helper()
	f := tagPairFixture{
		seed:     insertImage(t, database, "tp_seed", 100),
		near:     insertImage(t, database, "tp_near", 110),
		far:      insertImage(t, database, "tp_far", 120),
		stranger: insertImage(t, database, "tp_stranger", 130),
	}
	fillers := make([]int64, 0, 20)
	for i := 0; i < 20; i++ {
		fillers = append(fillers, insertImage(t, database, fmt.Sprintf("tp_filler_%02d", i), 90))
	}
	for i := 0; i < tagPairMinShared; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("shared_%02d", i))
		attachTagRow(t, database, f.seed, id)
		attachTagRow(t, database, f.near, id)
	}
	attachTagRow(t, database, f.far, insertTagRow(t, database, "shared_00"))
	for i := 0; i < tagPairMinShared; i++ {
		attachTagRow(t, database, f.far, insertTagRow(t, database, fmt.Sprintf("far_%02d", i)))
	}
	for i := 0; i < tagPairMinShared; i++ {
		attachTagRow(t, database, f.stranger, insertTagRow(t, database, fmt.Sprintf("lone_%02d", i)))
	}
	everywhere := insertTagRow(t, database, "everywhere")
	for _, id := range append([]int64{f.seed, f.near, f.far, f.stranger}, fillers...) {
		attachTagRow(t, database, id, everywhere)
	}
	return f
}

func insertTagRow(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id); err == nil {
		return id
	}
	var catID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0) RETURNING id`, name, catID,
	).Scan(&id); err != nil {
		t.Fatalf("insert tag %q: %v", name, err)
	}
	return id
}

func attachTagRow(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imageID, tagID)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID)
}

func attachImpliedTagRow(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	attachTagRow(t, database, imageID, tagID)
	mustExec(t, database.Write, `UPDATE image_tags SET is_implied = 1 WHERE image_id = ? AND tag_id = ?`, imageID, tagID)
}

func queueRow(t *testing.T, database *db.DB, a, b int64) (source string, score *float64, distance int, found bool) {
	t.Helper()
	lo, hi := canonicalPair(a, b)
	err := database.Read.QueryRow(
		`SELECT source, score, distance FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`,
		lo, hi,
	).Scan(&source, &score, &distance)
	if err != nil {
		return "", nil, 0, false
	}
	return source, score, distance, true
}

func TestTagPairDistance_BandedAbovePhash(t *testing.T) {
	if got := TagPairDistance(1); got != tagPairDistanceBase {
		t.Fatalf("perfect score maps to %d, want %d", got, tagPairDistanceBase)
	}
	if got := TagPairDistance(0.85); got <= tagPairDistanceBase {
		t.Fatalf("0.85 maps to %d, want above the band floor", got)
	}
	if TagPairDistance(1) <= 12 {
		t.Fatal("tag band overlaps the phash distance range")
	}
	if got := TagPairDistance(2); got != tagPairDistanceBase {
		t.Fatalf("out-of-range score maps to %d, want the band floor", got)
	}
}

func TestFindTagPairs_QueuesStrongMatchesOnly(t *testing.T) {
	database, _ := setupTestDB(t)
	f := setupTagPairFixture(t, database)

	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the saturating pair)", added)
	}
	source, score, distance, found := queueRow(t, database, f.seed, f.near)
	if !found {
		t.Fatal("the strong pair never reached the queue")
	}
	if source != SourceTags {
		t.Fatalf("source = %q, want %q", source, SourceTags)
	}
	if score == nil || *score < 0.85 {
		t.Fatalf("stored score = %v, want the admission score", score)
	}
	if distance != TagPairDistance(*score) {
		t.Fatalf("distance = %d, want %d", distance, TagPairDistance(*score))
	}
	if _, _, _, ok := queueRow(t, database, f.seed, f.far); ok {
		t.Fatal("the weak match was queued")
	}
	if _, _, _, ok := queueRow(t, database, f.seed, f.stranger); ok {
		t.Fatal("an image sharing only a ubiquitous tag was queued")
	}
}

func TestFindTagPairs_BulkTaggedBlockIsNotEvidence(t *testing.T) {
	database, _ := setupTestDB(t)
	const block = 30
	shared := make([]int64, 0, 12)
	for i := 0; i < 12; i++ {
		shared = append(shared, insertTagRow(t, database, fmt.Sprintf("bulk_%02d", i)))
	}
	for i := 0; i < block; i++ {
		id := insertImage(t, database, fmt.Sprintf("bulk_%02d", i), 100)
		for _, tagID := range shared {
			attachTagRow(t, database, id, tagID)
		}
	}
	for i := 0; i < 30; i++ {
		insertImage(t, database, fmt.Sprintf("bulk_filler_%02d", i), 90)
	}

	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 from a block sharing only library-wide tags", added)
	}

	a := insertImage(t, database, "bulk_pair_a", 100)
	b := insertImage(t, database, "bulk_pair_b", 110)
	for i := 0; i < 12; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("bulkrare_%02d", i))
		attachTagRow(t, database, a, id)
		attachTagRow(t, database, b, id)
	}
	added = mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 1 {
		t.Fatalf("added = %d, want the rare-tag pair", added)
	}
}

func TestFindTagPairs_ImpliedRowsAreNotEvidence(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "imp_a", 100)
	b := insertImage(t, database, "imp_b", 110)
	for i := 0; i < 30; i++ {
		insertImage(t, database, fmt.Sprintf("imp_filler_%02d", i), 90)
	}
	parent := insertTagRow(t, database, "character_soandso")
	attachTagRow(t, database, a, parent)
	attachTagRow(t, database, b, parent)
	for i := 0; i < 12; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("imp_%02d", i))
		attachImpliedTagRow(t, database, a, id)
		attachImpliedTagRow(t, database, b, id)
	}

	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 from one tag's fan-out", added)
	}

	mustExec(t, database.Write, `UPDATE image_tags SET is_implied = 0`)
	added = mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 1 {
		t.Fatalf("added = %d, want the hand-tagged pair", added)
	}
}

func TestFindTagPairs_SkipsSparselyTaggedImages(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "sparse_a", 100)
	b := insertImage(t, database, "sparse_b", 110)
	for i := 0; i < 20; i++ {
		insertImage(t, database, fmt.Sprintf("sparse_filler_%02d", i), 90)
	}
	for _, name := range []string{"rare_one", "rare_two", "rare_three"} {
		id := insertTagRow(t, database, name)
		attachTagRow(t, database, a, id)
		attachTagRow(t, database, b, id)
	}
	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 below the tag floor", added)
	}
}

func TestFindTagPairs_SubsetScoresByNormRatio(t *testing.T) {
	database, _ := setupTestDB(t)
	dense := insertImage(t, database, "asym_dense", 100)
	sparse := insertImage(t, database, "asym_sparse", 110)
	for i := 0; i < 20; i++ {
		insertImage(t, database, fmt.Sprintf("asym_filler_%02d", i), 90)
	}
	for i := 0; i < tagPairMinShared; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("sh_%02d", i))
		attachTagRow(t, database, dense, id)
		attachTagRow(t, database, sparse, id)
	}
	for i := 0; i < 12; i++ {
		attachTagRow(t, database, dense, insertTagRow(t, database, fmt.Sprintf("own_%02d", i)))
	}

	seedDense, err := tags.LoadSimilaritySeed(database, dense, visibleImages(t, database))
	if err != nil {
		t.Fatal(err)
	}
	seedSparse, err := tags.LoadSimilaritySeed(database, sparse, visibleImages(t, database))
	if err != nil {
		t.Fatal(err)
	}

	want := math.Sqrt(seedSparse.Norm / seedDense.Norm)
	if want >= 0.85 || want < 0.5 {
		t.Fatalf("fixture drifted: subset score = %v, want inside [0.5, 0.85)", want)
	}

	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 above the size-ratio score", added)
	}
	added = mustFindTagPairs(t, context.Background(), database, 0.5, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 once the threshold sits under the size ratio", added)
	}
	source, score, _, found := queueRow(t, database, dense, sparse)
	if !found {
		t.Fatal("the subset pair never reached the queue")
	}
	if source != SourceTags {
		t.Fatalf("source = %q, want %q", source, SourceTags)
	}
	if score == nil || math.Abs(*score-want) > 1e-9 {
		t.Fatalf("stored score = %v, want the size-ratio score %v", score, want)
	}
}

func TestFindTagPairs_MetaPaddedImageIsStillACandidate(t *testing.T) {
	database, _ := setupTestDB(t)
	seed := insertImage(t, database, "meta_seed", 100)
	padded := insertImage(t, database, "meta_padded", 110)
	for i := 0; i < 20; i++ {
		insertImage(t, database, fmt.Sprintf("meta_filler_%02d", i), 90)
	}
	for i := 0; i < tagPairMinShared; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("mp_%02d", i))
		attachTagRow(t, database, seed, id)
		attachTagRow(t, database, padded, id)
	}
	attachTagRow(t, database, seed, insertTagRow(t, database, "seed_own"))
	var metaCat int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'meta'`).Scan(&metaCat); err != nil {
		t.Fatal(err)
	}
	var metaTag int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('mp_meta', ?, 0) RETURNING id`, metaCat,
	).Scan(&metaTag); err != nil {
		t.Fatal(err)
	}
	attachTagRow(t, database, padded, metaTag)

	added := mustFindTagPairs(t, context.Background(), database, 0.75, nil)
	if added != 1 {
		t.Fatalf("added = %d, want the meta-padded candidate admitted", added)
	}
	source, score, _, found := queueRow(t, database, seed, padded)
	if !found {
		t.Fatal("the meta-padded pair never reached the queue")
	}
	if source != SourceTags {
		t.Fatalf("source = %q, want %q", source, SourceTags)
	}
	if score == nil || *score <= 0.75 || *score >= 1 {
		t.Fatalf("stored score = %v, want inside (0.75, 1)", score)
	}
}

func TestFindTagPairs_HalfOverlapStaysBelowDefault(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "ho_a", 100)
	b := insertImage(t, database, "ho_b", 110)
	twinA := insertImage(t, database, "ho_twin_a", 120)
	twinB := insertImage(t, database, "ho_twin_b", 130)
	for i := 0; i < 20; i++ {
		insertImage(t, database, fmt.Sprintf("ho_filler_%02d", i), 90)
	}
	for i := 0; i < tagPairMinShared; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("ho_shared_%02d", i))
		attachTagRow(t, database, a, id)
		attachTagRow(t, database, b, id)
	}
	for i := 0; i < 14; i++ {
		attachTagRow(t, database, a, insertTagRow(t, database, fmt.Sprintf("ho_a_%02d", i)))
		attachTagRow(t, database, b, insertTagRow(t, database, fmt.Sprintf("ho_b_%02d", i)))
	}
	for i := 0; i < tagPairMinShared; i++ {
		id := insertTagRow(t, database, fmt.Sprintf("tw_%02d", i))
		attachTagRow(t, database, twinA, id)
		attachTagRow(t, database, twinB, id)
	}

	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 1 {
		t.Fatalf("added = %d, want only the identical-set pair", added)
	}
	if _, _, _, ok := queueRow(t, database, a, b); ok {
		t.Fatal("a five-of-twelve overlap reached the queue at the default threshold")
	}
	if _, _, _, ok := queueRow(t, database, twinA, twinB); !ok {
		t.Fatal("the identical-set pair missed the queue")
	}
}

func TestFindTagPairs_RespectsDeclaredRelations(t *testing.T) {
	database, svc := setupTestDB(t)
	f := setupTagPairFixture(t, database)
	if err := svc.AddNotRelated(f.seed, f.near); err != nil {
		t.Fatal(err)
	}
	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 once the pair is marked not related", added)
	}
}

func TestFindTagPairs_UpgradesAPhashPairToBoth(t *testing.T) {
	database, _ := setupTestDB(t)
	f := setupTagPairFixture(t, database)
	lo, hi := canonicalPair(f.seed, f.near)
	mustExec(t, database.Write, `INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, source) VALUES (?, ?, 3, '2026-01-01T00:00:00Z', ?)`, lo, hi, SourcePhash)
	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0: the pair was already queued", added)
	}
	source, score, distance, _ := queueRow(t, database, f.seed, f.near)
	if source != SourceBoth {
		t.Fatalf("source = %q, want %q", source, SourceBoth)
	}
	if score == nil {
		t.Fatal("the upgrade dropped the tag score")
	}
	if distance != 3 {
		t.Fatalf("distance = %d, want the phash distance 3 kept", distance)
	}
}

func TestFindTagPairs_SkipsPairsInAPrivateCollection(t *testing.T) {
	database, _ := setupTestDB(t)
	f := setupTagPairFixture(t, database)
	for _, id := range []int64{f.seed, f.near} {
		mustExec(t, database.Write, `INSERT INTO image_collections (image_id, name) VALUES (?, 'doujin')`, id)
	}
	added := mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 0 {
		t.Fatalf("added = %d, want 0 while the collection has not opted in", added)
	}
	mustExec(t, database.Write, `INSERT INTO collection_find_relations (name) VALUES ('doujin')`)
	added = mustFindTagPairs(t, context.Background(), database, 0.85, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 after the collection opts in", added)
	}
}

func TestFindPairs_TagPassIsOptIn(t *testing.T) {
	database, _ := setupTestDB(t)
	f := setupTagPairFixture(t, database)
	tree := NewBKTree()

	if _, err := FindPairs(context.Background(), database, tree, FindPairsOptions{Distance: 2}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := queueRow(t, database, f.seed, f.near); ok {
		t.Fatal("the tag pass ran with TagPairs off")
	}
	added := mustFindPairs(t, context.Background(), database, tree, FindPairsOptions{Distance: 2, TagPairs: true, TagPairThreshold: 0.85, VisibleCount: counts.New(database).VisibleCount}, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 with the tag pass on", added)
	}
	if _, _, _, ok := queueRow(t, database, f.seed, f.near); !ok {
		t.Fatal("the tag pass queued nothing")
	}
}

func TestQueueTagPair_KeepsTheStrongerDirection(t *testing.T) {
	database, _ := setupTestDB(t)
	a := insertImage(t, database, "qtp_a", 100)
	b := insertImage(t, database, "qtp_b", 110)

	queue := func(seed, other int64, score float64) bool {
		t.Helper()
		p, ok, err := admitTagPair(context.Background(), database, seed, tagPairCandidate{imageID: other, score: score})
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("the pair was refused admission")
		}
		tx, err := database.Write.Begin()
		if err != nil {
			t.Fatal(err)
		}
		landed, err := storeTagPairTx(tx, p)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return landed
	}

	if !queue(a, b, 0.86) {
		t.Fatal("the first direction should have filed a new row")
	}
	if queue(b, a, 0.94) {
		t.Error("the second direction must not count as a new row")
	}

	_, score, distance, found := queueRow(t, database, a, b)
	if !found {
		t.Fatal("the pair left the queue")
	}
	if score == nil || math.Abs(*score-0.94) > 1e-9 {
		t.Errorf("stored score = %v, want the stronger 0.94", score)
	}
	if want := TagPairDistance(0.94); distance != want {
		t.Errorf("distance = %d, want %d (derived from the stored score)", distance, want)
	}

	queue(a, b, 0.87)
	if _, score, _, _ = queueRow(t, database, a, b); score == nil || math.Abs(*score-0.94) > 1e-9 {
		t.Errorf("stored score = %v after a weaker pass, want 0.94 kept", score)
	}
}

type cancelAfter struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	n      int
}

func (c *cancelAfter) Err() error {
	if pc, _, _, ok := runtime.Caller(1); ok && strings.HasSuffix(runtime.FuncForPC(pc).Name(), ".findTagPairs") {
		c.calls++
		if c.calls == c.n {
			c.cancel()
		}
	}
	return c.Context.Err()
}

func TestFindTagPairs_CancelKeepsTheAdmittedPairs(t *testing.T) {
	database, _ := setupTestDB(t)
	everywhere := insertTagRow(t, database, "everywhere")
	for i := range 20 {
		attachTagRow(t, database, insertImage(t, database, fmt.Sprintf("cp_filler_%02d", i), 90), everywhere)
	}
	const pairs = 6
	for p := range pairs {
		a := insertImage(t, database, fmt.Sprintf("cp_%d_a", p), 100)
		b := insertImage(t, database, fmt.Sprintf("cp_%d_b", p), 100)
		for i := range tagPairMinShared {
			id := insertTagRow(t, database, fmt.Sprintf("cp_%d_%02d", p, i))
			attachTagRow(t, database, a, id)
			attachTagRow(t, database, b, id)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	added, err := findTagPairs(&cancelAfter{Context: ctx, cancel: cancel, n: 2*pairs + 4}, database, visibleImages(t, database), 0.85, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if added == 0 {
		t.Fatal("added = 0, want the pairs admitted before the cancel")
	}
	if n := countRows(t, database, "potential_relation_pairs"); n != added {
		t.Fatalf("queue holds %d pairs, the run reported %d", n, added)
	}
}
