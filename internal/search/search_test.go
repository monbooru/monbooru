package search

import (
	"archive/zip"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/tags"
)

func buildWhere(expr Expr) (string, []any, bool) {
	return buildWhereDB(expr, Source{})
}

func TestParse(t *testing.T) {
	tag := func(name string) TagExpr { return TagExpr{Tag: name} }
	not := func(e Expr) NotExpr { return NotExpr{Expr: e} }
	filter := func(key, val string) FilterExpr { return FilterExpr{Key: key, Val: val} }
	for _, tc := range []struct {
		name, in string
		want     Expr
	}{
		{"BasicTag", "cute", tag("cute")},
		{"ImplicitAND", "a b", AndExpr{Left: tag("a"), Right: tag("b")}},
		{"OR", "cat OR dog", OrExpr{Left: tag("cat"), Right: tag("dog")}},
		{"NOT", "-blonde_hair", not(tag("blonde_hair"))},
		{"NOT_Keyword", "NOT blonde_hair", not(tag("blonde_hair"))},
		{"ORChain", "a OR b OR c", OrExpr{Left: OrExpr{Left: tag("a"), Right: tag("b")}, Right: tag("c")}},
		{"ORWithNegatedOperand", "1girl OR -solo", OrExpr{Left: tag("1girl"), Right: not(tag("solo"))}},
		{"NegatedOperandBeforeOR/-a OR b", "-a OR b", OrExpr{Left: not(tag("a")), Right: tag("b")}},
		{"NegatedOperandBeforeOR/NOT a OR b", "NOT a OR b", OrExpr{Left: not(tag("a")), Right: tag("b")}},
		{"NegatedOperandBeforeOR/-a OR -b", "-a OR -b", OrExpr{Left: not(tag("a")), Right: not(tag("b"))}},
		{"NegatedOperandBeforeOR/c -a OR b", "c -a OR b", AndExpr{Left: tag("c"), Right: OrExpr{Left: not(tag("a")), Right: tag("b")}}},
		{"NegatedOperandBeforeOR/NOT OR b", "NOT OR b", tag("b")},
		{"NegatedOperandBeforeOR/-a OR", "-a OR", not(tag("a"))},
		{"LeadingOR_DropsOperator", "OR 1girl", tag("1girl")},
		{"LeadingORChain_DropsOperators", "OR OR 1girl", tag("1girl")},
		{"AndKeyword_DropsOperator", "a AND b", AndExpr{Left: tag("a"), Right: tag("b")}},
		{"AndKeyword_CaseInsensitive/and", "a and b", AndExpr{Left: tag("a"), Right: tag("b")}},
		{"AndKeyword_CaseInsensitive/And", "a And b", AndExpr{Left: tag("a"), Right: tag("b")}},
		{"AndKeyword_CaseInsensitive/aNd", "a aNd b", AndExpr{Left: tag("a"), Right: tag("b")}},
		{"QuotedTag/underscore", `"long_hair"`, tag("long_hair")},
		{"QuotedTag/space", `"long hair"`, tag("long_hair")},
		{"QuotedTag/cased", `"Red Hair"`, tag("red_hair")},
		{"NegatedQuotedTag/underscore", `-"long_hair"`, not(tag("long_hair"))},
		{"NegatedQuotedTag/space", `-"long hair"`, not(tag("long_hair"))},
		{"QuotedTermWithAColonIsATag/re:zero", `"re:zero"`, tag("re:zero")},
		{"QuotedTermWithAColonIsATag/Re:Zero", `"Re:Zero"`, tag("re:zero")},
		{"QuotedTermWithAColonIsATag/source:code", `"source:code"`, tag("source:code")},
		{"QuotedTermWithAColonIsATag/artist:foo", `"artist:foo"`, tag("artist:foo")},
		{"Filters/plain", "fav:true", filter("fav", "true")},
		{"Filters/path value", "folder:2024/jan", filter("folder", "2024/jan")},
		{"Filters/escaped quote", `folder:"a\"b"`, filter("folder", `a"b`)},
		{"Filters/escaped backslash", `source:"a\\b"`, filter("source", `a\b`)},
		{"Filter_Inbox/true", "inbox:true", filter("inbox", "true")},
		{"Filter_Inbox/false", "inbox:false", filter("inbox", "false")},
		{"Filter_AI", "ai:sd", filter("ai", "sd")},
		{"Filter_Source", "source:my_label", filter("source", "my_label")},
		{"TagNormalized/folded", "Café", tag("café")},
		{"TagNormalized/quoted", `"red hair"`, tag("red_hair")},
		{"Filter_Type/image", "type:image", filter("type", "image")},
		{"Filter_Type/archive", "type:archive", filter("type", "archive")},
		{"Filter_Type/list", "type:image,archive", filter("type", "image,archive")},
		{"Filter_Collection", "collection:naruto", filter("collection", "naruto")},
		{"Filter_Pages/>=100", "pages:>=100", filter("pages", ">=100")},
		{"Filter_Pages/<200", "pages:<200", filter("pages", "<200")},
		{"Filter_Pages/=42", "pages:=42", filter("pages", "=42")},
		{"Filter_Pages/0", "pages:0", filter("pages", "0")},
		{"Wildcard_Prefix", "blue*", TagExpr{Tag: "blue", Wildcard: "prefix"}},
		{"Wildcard_Substring", "*blue*", TagExpr{Tag: "blue", Wildcard: "substring"}},
		{"Wildcard_Suffix", "*eyes", TagExpr{Tag: "eyes", Wildcard: "suffix"}},
		{"Empty", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in); got != tc.want {
				t.Errorf("Parse(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParse_QuoteAfterEqualsKeepsTheValueWhole(t *testing.T) {
	ipa := FilterExpr{Key: "comfyui", Val: "title=My IPA"}
	for _, tc := range []struct {
		in   string
		want Expr
	}{
		{`comfyui:title="My IPA"`, ipa},
		{`comfyui:"title=My IPA"`, ipa},
		{`comfyui:title="My IPA" solo`, AndExpr{Left: ipa, Right: TagExpr{Tag: "solo"}}},
		{`comfyui:title="a \"b\""`, FilterExpr{Key: "comfyui", Val: `title=a "b"`}},
	} {
		if got := Parse(tc.in); got != tc.want {
			t.Errorf("Parse(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestQuoteValueRoundTrip(t *testing.T) {
	for _, label := range []string{
		`saga`,
		`Tom & Jerry's "Big" <Adventure>`,
		`back\slash`,
		`日本語コレクション`,
		`a "b" c`,
	} {
		q := `collection:"` + QuoteValue(label) + `"`
		e := Parse(q)
		f, ok := e.(FilterExpr)
		if !ok {
			t.Fatalf("label %q: expected FilterExpr, got %T", label, e)
		}
		if f.Key != "collection" || f.Val != label {
			t.Errorf("label %q round-tripped to {Key:%q Val:%q}", label, f.Key, f.Val)
		}
	}
}

func TestParse_Filter_Stale(t *testing.T) {
	for _, in := range []string{"stale:foo", "stale:any", "stale:none"} {
		e := Parse(in)
		if f, ok := e.(FilterExpr); !ok || f.Key != "stale" {
			t.Errorf("parse %q: got %#v, want FilterExpr{stale}", in, e)
		}
	}
}

func TestParse_BareWildcard_NoMatch(t *testing.T) {
	for _, in := range []string{"*", "**"} {
		e := Parse(in)
		tag, ok := e.(TagExpr)
		if !ok {
			t.Fatalf("expected TagExpr for %q, got %T", in, e)
		}
		if tag.Tag != "" || tag.Wildcard == "prefix" {
			t.Errorf("Parse(%q) = %+v, want empty literal-no-match", in, tag)
		}
	}
}

func TestBuildWhere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expr    Expr
		args    []any
		in, out []string
	}{
		{name: "FolderFilter", expr: FilterExpr{Key: "folder", Val: "2024/jan"},
			args: []any{"2024/jan", "2024/jan/%"}, in: []string{"folder_path", "LIKE"}},

		{name: "FolderFilterEmpty", expr: FilterExpr{Key: "folder", Val: ""},
			out: []string{"folder_path"}},
		{name: "FolderOnlyFilter", expr: FilterExpr{Key: "folderonly", Val: "2024/jan"},
			args: []any{"2024/jan"}, in: []string{"folder_path = ?"}},
		{name: "FolderOnlyFilterEmpty", expr: FilterExpr{Key: "folderonly", Val: ""},
			in: []string{"folder_path = ''"}},
		{name: "TypeImage", expr: FilterExpr{Key: "type", Val: "image"},
			in: []string{"file_type IN ('avif', 'jpeg', 'jxl', 'png', 'webp')"}},
		{name: "TypeAnimated", expr: FilterExpr{Key: "type", Val: "animated"},
			in: []string{"file_type IN ('gif', 'mp4', 'webm')"}},
		{name: "TaggedTrue", expr: FilterExpr{Key: "tagged", Val: "true"},
			in: []string{"EXISTS (SELECT 1 FROM image_tags"}, out: []string{"NOT EXISTS"}},
		{name: "AutotaggedTrue", expr: FilterExpr{Key: "autotagged", Val: "true"},
			in: []string{"it.is_auto = 1"}, out: []string{"NOT EXISTS"}},
		{name: "TagExact", expr: TagExpr{Tag: "cute"},
			args: []any{"cute"}, in: []string{"name = ?", "COALESCE(canonical_tag_id, id)"}},
		{name: "TagPrefix", expr: TagExpr{Tag: "blue", Wildcard: "prefix"},
			args: []any{"blue%"}, in: []string{"LIKE ?"}},
		{name: "TagSubstring", expr: TagExpr{Tag: "hair", Wildcard: "substring"},
			args: []any{"%hair%"}},
		{name: "AIAny", expr: FilterExpr{Key: "ai", Val: "any"},
			in: []string{"'a1111'", "'comfyui'", "'a1111,comfyui'"}},
		{name: "AINone", expr: FilterExpr{Key: "ai", Val: "none"},
			in: []string{"i.source_type = 'none'"}, out: []string{"LIKE"}},
		{name: "SourceAnyMembership", expr: FilterExpr{Key: "source", Val: "Pixiv"},
			args: []any{"Pixiv"}, in: []string{"i.id IN (SELECT image_id FROM image_sources WHERE site = ? COLLATE NOCASE)"}},
		{name: "Cat", expr: FilterExpr{Key: "cat", Val: "character"},
			args: []any{"character"}, in: []string{"tc.name = ?"}},
		{name: "Width", expr: FilterExpr{Key: "width", Val: ">=1920"},
			args: []any{int64(1920)}, in: []string{"i.width"}},
		{name: "Height", expr: FilterExpr{Key: "height", Val: "<768"},
			args: []any{int64(768)}, in: []string{"i.height"}},
		{name: "WidthNonNumericRejected", expr: FilterExpr{Key: "width", Val: ">=abc"},
			in: []string{"1=0"}},
		{name: "Name", expr: FilterExpr{Key: "name", Val: "Vacation"},
			args: []any{`"vacation"`, `"vacation"`}, in: []string{"image_basename_canonical_fts MATCH", "image_basename_alias_fts MATCH"}},
		{name: "Ratio", expr: FilterExpr{Key: "ratio", Val: ">=1.5"},
			args: []any{1.5}, in: []string{"CAST(i.width AS REAL)"}},
		{name: "TagCount", expr: FilterExpr{Key: "tagcount", Val: ">=5"},
			args: []any{int64(5)}, in: []string{"i.tag_count >="}},
		{name: "TagCountOperatorFirstIsTheTotal", expr: FilterExpr{Key: "tagcount", Val: "=5"},
			args: []any{int64(5)}, in: []string{"i.tag_count = ?"}, out: []string{"GROUP BY"}},
		{name: "TagCountCategory", expr: FilterExpr{Key: "tagcount", Val: "Character>=2"},
			args: []any{"character", "character", int64(2)},
			in:   []string{"i.id IN (SELECT it.image_id FROM image_tags it", "HAVING COUNT(*) >= ?"}, out: []string{"i.tag_count", "NOT IN"}},
		{name: "TagCountCategoryZeroTakesTheComplement", expr: FilterExpr{Key: "tagcount", Val: "artist=0"},
			args: []any{"artist", "artist", int64(0)}, in: []string{"i.id NOT IN (", "HAVING NOT (COUNT(*) = ?)"}},
		{name: "TagCountCategoryRangeFromZero", expr: FilterExpr{Key: "tagcount", Val: "character=..1"},
			args: []any{"character", "character", int64(1)}, in: []string{"i.id NOT IN (", "NOT (COUNT(*) <= ?)"}},
		{name: "TagCountCategoryRangeAboveZero", expr: FilterExpr{Key: "tagcount", Val: "character=1..2"},
			args: []any{"character", "character", int64(1), int64(2)}, in: []string{"i.id IN (", "COUNT(*) BETWEEN ? AND ?"}, out: []string{"NOT IN"}},
		{name: "TagCountCategoryNonNumericRejected", expr: FilterExpr{Key: "tagcount", Val: "character=many"},
			in: []string{"1=0"}, out: []string{"image_tags"}},
		{name: "Duration", expr: FilterExpr{Key: "duration", Val: ">=30"},
			args: []any{30.0}, in: []string{"i.duration_seconds IS NOT NULL", "i.duration_seconds >="}},
		{name: "Via", expr: FilterExpr{Key: "via", Val: "upload"},
			args: []any{"upload"}, in: []string{"i.origin = ?", "COLLATE NOCASE"}},
		{name: "TypeEveryBucket", expr: FilterExpr{Key: "type", Val: "image,archive,animated"},
			in: []string{"1=1"}},
		{name: "TypeAnimatedNegated", expr: NotExpr{Expr: FilterExpr{Key: "type", Val: "animated"}},
			in: []string{"NOT", "file_type IN ('gif', 'mp4', 'webm')"}},
		{name: "TaggedFalse", expr: FilterExpr{Key: "tagged", Val: "false"},
			in: []string{"NOT EXISTS (SELECT 1 FROM image_tags"}},
		{name: "AutotaggedFalse", expr: FilterExpr{Key: "autotagged", Val: "false"},
			in: []string{"NOT EXISTS (SELECT 1 FROM image_tags", "it.is_auto = 1"}},
		{name: "NegatedMissingFalse", expr: NotExpr{Expr: FilterExpr{Key: "missing", Val: "false"}},
			in: []string{"NOT"}, out: []string{"AND i.is_missing = 0"}},
		{name: "NameShortFallback", expr: FilterExpr{Key: "name", Val: "im"},
			args: []any{"%im%", "%im%"}, in: []string{"i.basename_lower LIKE"}, out: []string{"image_basename_canonical_fts"}},
		{name: "OR", expr: OrExpr{Left: TagExpr{Tag: "cat"}, Right: TagExpr{Tag: "dog"}},
			args: []any{"cat", "dog"}, in: []string{"OR"}},
		{name: "NOT", expr: NotExpr{Expr: TagExpr{Tag: "ugly"}},
			args: []any{"ugly"}, in: []string{"NOT"}},
		{name: "AND", expr: AndExpr{Left: TagExpr{Tag: "cat"}, Right: TagExpr{Tag: "cute"}},
			args: []any{"cat", "cute"}, in: []string{"AND"}},
		{name: "AND_LeftUnknown", expr: AndExpr{Left: FilterExpr{Key: "bogus", Val: ""}, Right: TagExpr{Tag: "cute"}},
			args: []any{"bogus", "cute"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			where, args, _ := buildWhere(tc.expr)
			if !slices.Equal(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
			for _, s := range tc.in {
				wantIn(t, where, s)
			}
			for _, s := range tc.out {
				wantNotIn(t, where, s)
			}
		})
	}
}

func TestBuildWhere_MissingTrue(t *testing.T) {
	expr := FilterExpr{Key: "missing", Val: "true"}
	_, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingFilter = true")
	}
}

type searchEnv struct {
	db            *db.DB
	galleryDir    string
	thumbnailsDir string
	maxFileSizeMB int
}

func wantIn(t *testing.T, haystack, want string) {
	t.Helper()
	if !strings.Contains(haystack, want) {
		t.Errorf("missing %q in %q", want, haystack)
	}
}

func wantNotIn(t *testing.T, haystack, unwanted string) {
	t.Helper()
	if strings.Contains(haystack, unwanted) {
		t.Errorf("unexpected %q in %q", unwanted, haystack)
	}
}

func setupSearchDB(t *testing.T) (*db.DB, *searchEnv) {
	t.Helper()
	tmpDir := t.TempDir()

	t.Cleanup(func() {
		for i := 0; i < 50 && os.RemoveAll(tmpDir) != nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
	})
	galleryDir := filepath.Join(tmpDir, "gallery")
	if err := os.MkdirAll(galleryDir, 0755); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	return database, &searchEnv{
		db:            database,
		galleryDir:    galleryDir,
		thumbnailsDir: filepath.Join(tmpDir, "thumbs"),
		maxFileSizeMB: 100,
	}
}

func nameSpread(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32()
}

func ingestTestImage(t *testing.T, database *db.DB, env *searchEnv, name string) {
	t.Helper()
	spread := nameSpread(name)
	img := image.NewRGBA(image.Rect(0, 0, 10+int(spread%64), 10+int(spread/64%64)))
	path := filepath.Join(env.galleryDir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gallery.Ingest(testHandle(database, env.galleryDir, env.thumbnailsDir), path, ""); err != nil {
		t.Fatalf("ingest %q: %v", name, err)
	}
}

var testTallies, testCaches sync.Map

func testSource(database *db.DB) Source {
	t, _ := testTallies.LoadOrStore(database, counts.New(database))
	c, _ := testCaches.LoadOrStore(database, NewAdjacencyCache())
	return Source{DB: database, Tallies: t.(*counts.Tallies), Cache: c.(*AdjacencyCache)}
}

func seedVisibleRows(t *testing.T, database *db.DB, n int) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size) WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?) SELECT 'seed' || n, '/seed/' || n || '.png', 'png', 1 FROM seq`, n)
	testSource(database).Tallies.Invalidate()
}

func TestExecute_AZeroExactCountSkipsThePage(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "only.png")

	mustExec(t, database.Write, `DROP INDEX idx_images_ingested_visible`)

	res := mustExecute(t, database, Query{Expr: FilterExpr{Key: "type", Val: "archive"}, Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 0 || len(res.Results) != 0 {
		t.Errorf("Total = %d with %d rows, want an empty page", res.Total, len(res.Results))
	}
}

func TestExecute_DisjointPopularTagsAnswerZero(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedVisibleRows(t, database, 60000)
	generalID := categoryID(t, database, "general")

	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES ('left_tag', ?, 60000), ('right_tag', ?, 60000)`, generalID, generalID)

	for name, expr := range map[string]Expr{
		"plain":          Parse("left_tag right_tag"),
		"under a rating": ApplyCeiling(Parse("left_tag right_tag"), "general"),
	} {
		t.Run(name, func(t *testing.T) {
			for run := range 2 {
				res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40, Cached: true, Text: "disjoint " + name})
				if res.Total != 0 || len(res.Results) != 0 {
					t.Errorf("run %d: Total = %d with %d rows, want 0", run+1, res.Total, len(res.Results))
				}
			}
		})
	}
}

func TestExecute_BasicSearch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "test.png")

	q := Query{Sort: "newest", Page: 1, Limit: 40}
	result := mustExecute(t, database, q)
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
}

func TestExecute_APageTooLargeToMultiplyIsEmptyNotAPanic(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "test.png")

	q := Query{Sort: "newest", Page: 1, Limit: 40, Cached: true, Text: "huge-page"}
	if warm := mustExecute(t, database, q); warm.Total != 1 {
		t.Fatalf("Total = %d, want 1", warm.Total)
	}
	if _, ok := testSource(database).Cache.get(q.listKey(Source{})); !ok {
		t.Fatal("page 1 left the adjacency cache cold, so the sliced path is not under test")
	}

	for _, page := range []int{1 << 40, math.MaxInt/2 + 1, math.MaxInt} {
		q.Page = page
		res := mustExecute(t, database, q)
		if len(res.Results) != 0 {
			t.Errorf("page %d returned %d row(s), want none", page, len(res.Results))
		}
	}
}

func TestExecute_FolderFilter(t *testing.T) {
	database, env := setupSearchDB(t)

	subDir := filepath.Join(env.galleryDir, "2024")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	ingestTestImage(t, database, env, "root.png")
	ingestTestImage(t, database, env, "2024/sub.png")

	mustExecute(t, database, Query{
		Expr:  FilterExpr{Key: "folder", Val: ""},
		Page:  1,
		Limit: 40,
	})
}

func ingestTestManga(t *testing.T, database *db.DB, env *searchEnv, name string, series string) int64 {
	t.Helper()
	spread := nameSpread(name)
	pic := image.NewRGBA(image.Rect(0, 0, 8+int(spread%64), 8+int(spread/64%64)))
	cbzPath := filepath.Join(env.galleryDir, name)
	f, err := os.Create(cbzPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("01.png")
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := png.Encode(w, pic); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rec, _, err := gallery.Ingest(testHandle(database, env.galleryDir, env.thumbnailsDir), cbzPath, "")
	if err != nil {
		t.Fatalf("Ingest cbz: %v", err)
	}
	if series != "" {
		if err := gallery.SetHomeCollection(database, rec.ID, series, nil); err != nil {
			t.Fatal(err)
		}
	}
	return rec.ID
}

func TestExecute_Type(t *testing.T) {
	for _, tc := range []struct {
		name, val string
		cbz       bool
	}{
		{"ArchiveFindsCBZRow", "archive", true},
		{"ImageExcludesManga", "image", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, env := setupSearchDB(t)
			ingestTestImage(t, database, env, "regular.png")
			ingestTestManga(t, database, env, "m.cbz", "")

			res := mustExecute(t, database, Query{
				Expr: FilterExpr{Key: "type", Val: tc.val},
				Page: 1, Limit: 40,
			})
			if res.Total != 1 {
				t.Errorf("type:%s total = %d, want 1", tc.val, res.Total)
			}
			if len(res.Results) != 1 || (res.Results[0].FileType == "cbz") != tc.cbz {
				t.Errorf("type:%s returned %+v", tc.val, res.Results)
			}
		})
	}
}

func TestExecute_SystemFilter_NoMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	res := mustExecute(t, database, Query{
		Expr: FilterExpr{Key: "system", Val: ""},
		Page: 1, Limit: 40,
	})
	if res.Total != 0 {
		t.Errorf("system: total = %d, want 0 (autocomplete-only trigger)", res.Total)
	}
}

func TestExecute_CollectionExactMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestManga(t, database, env, "naruto.cbz", "Naruto")
	ingestTestManga(t, database, env, "bleach.cbz", "Bleach")

	res := mustExecute(t, database, Query{
		Expr: FilterExpr{Key: "collection", Val: "Naruto"},
		Page: 1, Limit: 40,
	})
	if res.Total != 1 {
		t.Errorf("collection:Naruto total = %d, want 1", res.Total)
	}
}

func TestExecute_CollectionAny(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestManga(t, database, env, "naruto.cbz", "Naruto")
	ingestTestManga(t, database, env, "bleach.cbz", "Bleach")
	ingestTestImage(t, database, env, "loose.png")

	res := mustExecute(t, database, Query{
		Expr: FilterExpr{Key: "collection", Val: "any"},
		Page: 1, Limit: 40,
	})
	if res.Total != 2 {
		t.Errorf("collection:any total = %d, want 2", res.Total)
	}
}

func TestExecute_CollectionMatchesExtraMembership(t *testing.T) {
	database, env := setupSearchDB(t)
	id := ingestTestManga(t, database, env, "naruto.cbz", "Naruto")
	mustAddCollection(t, database, id, "Shonen", nil)
	res := mustExecute(t, database, Query{
		Expr: FilterExpr{Key: "collection", Val: "Shonen"},
		Page: 1, Limit: 40,
	})
	if res.Total != 1 {
		t.Errorf("collection:Shonen total = %d, want 1 (matches an extra membership)", res.Total)
	}
}

func pinnedArcPair(t *testing.T, database *db.DB, env *searchEnv) (a, b int64) {
	t.Helper()
	a = ingestTestImageWithID(t, database, env, "a.png")
	b = ingestTestImageWithID(t, database, env, "b.png")
	mustAddCollection(t, database, a, "HomeA", nil)
	mustAddCollection(t, database, b, "HomeB", nil)
	o2, o1 := 2, 1
	mustAddCollection(t, database, a, "Arc", &o2)
	mustAddCollection(t, database, b, "Arc", &o1)
	return a, b
}

func TestExecute_CollectionPinnedOrder(t *testing.T) {
	database, env := setupSearchDB(t)
	a, b := pinnedArcPair(t, database, env)
	res := mustExecute(t, database, Query{
		Expr:            FilterExpr{Key: "collection", Val: "Arc"},
		Sort:            "order",
		Order:           "asc",
		OrderCollection: "Arc",
		Page:            1, Limit: 40,
	})
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(res.Results))
	}
	if res.Results[0].ID != b || res.Results[1].ID != a {
		t.Errorf("pinned order = [%d,%d], want [%d,%d] (Arc positions 1 then 2)",
			res.Results[0].ID, res.Results[1].ID, b, a)
	}
}

func TestScope_ViewOrderIsTheGallerysOrder(t *testing.T) {
	database, env := setupSearchDB(t)
	a, b := pinnedArcPair(t, database, env)
	pinned := NewScope(FilterExpr{Key: "collection", Val: "Arc"}, "").InViewOrder("order", "asc", 0)
	if ids, err := pinned.IDs(testSource(database)); err != nil || !slices.Equal(ids, []int64{b, a}) {
		t.Errorf("pinned collection scope = %v (%v), want [%d %d]", ids, err, b, a)
	}

	f := setupSimilarityFixture(t, database, env)
	similar := NewScope(Parse(fmt.Sprintf("similar:%d", f.seed)), "").InViewOrder("similarity", "", 0)
	if ids, err := similar.IDs(testSource(database)); err != nil || !slices.Equal(ids, []int64{f.near, f.far}) {
		t.Errorf("similarity scope = %v (%v), want [%d %d]", ids, err, f.near, f.far)
	}
}

func TestExecuteAdjacent_CollectionPinnedOrder(t *testing.T) {
	database, env := setupSearchDB(t)
	a, b := pinnedArcPair(t, database, env)
	q := Query{
		Expr:            FilterExpr{Key: "collection", Val: "Arc"},
		Sort:            "order",
		Order:           "asc",
		OrderCollection: "Arc",
	}
	prev, next := mustExecuteAdjacent(t, context.Background(), database, q, b)
	if prev != nil {
		t.Errorf("prev of b = %d, want nil (b is first in Arc order)", *prev)
	}
	if next == nil || *next != a {
		t.Errorf("next of b = %v, want %d", next, a)
	}
	prev, next = mustExecuteAdjacent(t, context.Background(), database, q, a)
	if prev == nil || *prev != b {
		t.Errorf("prev of a = %v, want %d", prev, b)
	}
	if next != nil {
		t.Errorf("next of a = %d, want nil (a is last in Arc order)", *next)
	}
	if rank, err := RankInQuery(context.Background(), testSource(database), q, b); err != nil || rank != 0 {
		t.Errorf("rank of b = %d (err %v), want 0", rank, err)
	}
	if rank, err := RankInQuery(context.Background(), testSource(database), q, a); err != nil || rank != 1 {
		t.Errorf("rank of a = %d (err %v), want 1", rank, err)
	}
}

func TestCollectionAdjacencyWalksTheMembership(t *testing.T) {
	database, env := setupSearchDB(t)
	a, b := pinnedArcPair(t, database, env)
	c := ingestTestImageWithID(t, database, env, "c.png")
	mustAddCollection(t, database, c, "Arc", nil)
	mustExec(t, database.Write, `ANALYZE; DELETE FROM sqlite_stat1; DELETE FROM sqlite_stat4;
		INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES
			('image_collections', 'idx_image_collections_name', '751140 250000 1 1'),
			('image_collections', 'idx_image_collections_reading', '751140 250000 250000 1582 1'),
			('image_collections', 'sqlite_autoindex_image_collections_1', '751140 1 1'),
			('images', 'idx_images_missing', '1001364 401'),
			('images', 'sqlite_autoindex_images_1', '1001364 1');
		ANALYZE sqlite_schema`)
	for _, order := range []string{"asc", "desc"} {
		q := Query{
			Expr:            ApplyCeiling(AndExpr{Left: FilterExpr{Key: "collection", Val: "arc"}, Right: FilterExpr{Key: "collection", Val: "Arc"}}, "questionable"),
			Sort:            "order",
			Order:           order,
			OrderCollection: "Arc",
		}
		for _, current := range []int64{b, c} {
			p, err := buildAdjacencyPlan(context.Background(), testSource(database), q, current, nil, false, 0, 0)
			if err != nil || !p.ok {
				t.Fatalf("plan: %v", err)
			}
			wantNotIn(t, p.where, "image_collections")
			prevSQL, prevArgs := p.collectionNeighbour(p.collPrev)
			nextSQL, nextArgs := p.collectionNeighbour(p.collNext)
			countRows, countArgs := p.collectionPreceding()
			for _, w := range []struct {
				query string
				args  []any
				legs  []collectionLeg
			}{
				{prevSQL, prevArgs, p.collPrev},
				{nextSQL, nextArgs, p.collNext},
				{"SELECT COUNT(*) FROM (" + countRows + ")", countArgs, p.collPrev},
			} {
				plan := explainPlan(t, database, w.query, w.args)
				got := strings.Join(plan, "\n")
				var seeks []string
				for i, line := range plan {
					if strings.HasPrefix(line, "SEARCH pc ") {
						seeks = append(seeks, line)
						if i+1 >= len(plan) || !strings.HasPrefix(plan[i+1], "SEARCH i USING") {
							t.Errorf("%s: %s\nplans as\n%s\nwant the membership first", order, w.query, got)
						}
					}
				}
				if len(seeks) != len(w.legs) {
					t.Errorf("%s: %s\nplans as\n%s\nwant one membership seek per leg", order, w.query, got)
				}
				for i, leg := range w.legs {
					if i >= len(seeks) {
						break
					}
					seek := seeks[i]
					if !strings.HasPrefix(seek, "SEARCH pc USING COVERING INDEX idx_image_collections_reading (name=? AND <expr>=?") ||
						(len(leg.args) > 0 && !strings.ContainsAny(seek, "<>")) {
						t.Errorf("%s: leg %q seeks as %q, want the label's group and its cursor", order, leg.cmp, seek)
					}
				}
				wantNotIn(t, got, "TEMP B-TREE")
				wantNotIn(t, got, "SCAN pc")
				wantNotIn(t, got, "SCAN i")
			}
		}
	}
	q := Query{Expr: FilterExpr{Key: "collection", Val: "Arc"}, Sort: "order", Order: "asc", OrderCollection: "Arc"}
	if prev, next := mustExecuteAdjacent(t, context.Background(), database, q, b); prev != nil || next == nil || *next != a {
		t.Errorf("neighbours of b = %v, %v; want none, %d", prev, next, a)
	}
}

func explainPlan(t *testing.T, database *db.DB, query string, args []any) []string {
	t.Helper()
	rows, err := database.Write.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return plan
}

func TestCollectionOrderNeighboursFollowTheGalleryOrder(t *testing.T) {
	database, env := setupSearchDB(t)
	var ids []int64
	for i, pos := range []int{2, 1, 0, 0, 3, 2, 4} {
		id := ingestTestImageWithID(t, database, env, fmt.Sprintf("m%d.png", i))
		var order *int
		if pos > 0 {
			order = &pos
		}
		mustAddCollection(t, database, id, "Arc", order)
		ids = append(ids, id)
	}
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, ids[4])
	for _, order := range []string{"asc", "desc"} {
		q := Query{Expr: FilterExpr{Key: "collection", Val: "Arc"}, Sort: "order", Order: order, OrderCollection: "Arc", Page: 1, Limit: 40}
		res := mustExecute(t, database, q)
		var walk []int64
		for _, r := range res.Results {
			walk = append(walk, r.ID)
		}
		if len(walk) != 6 {
			t.Fatalf("%s: gallery order %v, want 6 visible members", order, walk)
		}
		for i, id := range walk {
			src := testSource(database)
			src.Cache = NewAdjacencyCache()
			prev, next, err := ExecuteAdjacent(context.Background(), src, q, id)
			if err != nil {
				t.Fatal(err)
			}
			var wantPrev, wantNext *int64
			if i > 0 {
				wantPrev = &walk[i-1]
			}
			if i+1 < len(walk) {
				wantNext = &walk[i+1]
			}
			if !sameID(prev, wantPrev) || !sameID(next, wantNext) {
				t.Errorf("%s: neighbours of %d (#%d in %v) = %v, %v; want %v, %v", order, id, i, walk, deref(prev), deref(next), deref(wantPrev), deref(wantNext))
			}
			if rank, err := RankInQuery(context.Background(), src, q, id); err != nil || rank != i {
				t.Errorf("%s: rank of %d = %d (%v), want %d", order, id, rank, err, i)
			}
		}
	}
}

func sameID(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestExecute_PagesComparison(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "regular.png")
	ingestTestManga(t, database, env, "small.cbz", "")
	mid := ingestTestManga(t, database, env, "mid.cbz", "")
	mustExec(t, database.Write, `UPDATE images SET page_count = 100 WHERE id = ?`, mid)

	res := mustExecute(t, database, Query{
		Expr: FilterExpr{Key: "pages", Val: ">=10"},
		Page: 1, Limit: 40,
	})
	if res.Total != 1 {
		t.Errorf("pages:>=10 total = %d, want 1 (only the 100-page manga)", res.Total)
	}
}

func TestExecute_NumericRange(t *testing.T) {
	database, env := setupSearchDB(t)
	mid := ingestTestManga(t, database, env, "mid.cbz", "")
	mustExec(t, database.Write, `UPDATE images SET page_count = 100 WHERE id = ?`, mid)

	cases := []struct {
		val   string
		total int
	}{
		{"50..200", 1},
		{"..50", 0},
		{"150..", 0},
		{"100..100", 1},
	}
	for _, c := range cases {
		res, err := Execute(context.Background(), testSource(database), Query{
			Expr: FilterExpr{Key: "pages", Val: c.val},
			Page: 1, Limit: 40,
		})
		if err != nil {
			t.Fatalf("%q: %v", c.val, err)
		}
		if res.Total != c.total {
			t.Errorf("pages:%s total = %d, want %d", c.val, res.Total, c.total)
		}
	}
}

func TestExecute_TagAliasResolvesToCanonical(t *testing.T) {
	database, env := setupSearchDB(t)
	imgID := ingestTestImageWithID(t, database, env, "alias_search.png")
	generalID := categoryID(t, database, "general")

	var canonID, aliasID int64
	canonID = insertTag(t, database, "feline", generalID, 0)
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES (?, ?, 1, ?) RETURNING id`,
		"cat", generalID, canonID,
	).Scan(&aliasID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, canonID)

	mustExec(t, database.Write, `UPDATE tags SET usage_count = 1 WHERE id = ?`, canonID)

	result := mustExecute(t, database, Query{
		Expr:  TagExpr{Tag: "cat"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1 (alias should resolve to canonical)", result.Total)
	}
}

func ratingTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

func getOrCreateTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	err := database.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id
	}
	return insertTag(t, database, name, categoryID(t, database, "general"), 0)
}

func attachTag(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imageID, tagID)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID)
}

func TestExecute_RatingHighestWins(t *testing.T) {
	database, env := setupSearchDB(t)
	idA := ingestTestImageWithID(t, database, env, "rating_a.png")
	idB := ingestTestImageWithID(t, database, env, "rating_b.png")

	attachTag(t, database, idA, ratingTagID(t, database, "general"))
	attachTag(t, database, idB, ratingTagID(t, database, "general"))
	attachTag(t, database, idB, ratingTagID(t, database, "explicit"))

	cases := []struct {
		val   string
		want  int64
		count int
	}{
		{"general", idA, 1},
		{"explicit", idB, 1},
		{"sensitive", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			result := mustExecute(t, database, Query{
				Expr:  FilterExpr{Key: "rating", Val: tc.val},
				Page:  1,
				Limit: 40,
			})
			if result.Total != tc.count {
				t.Fatalf("Total = %d, want %d", result.Total, tc.count)
			}
			if tc.count == 1 && result.Results[0].ID != tc.want {
				t.Errorf("matched id = %d, want %d", result.Results[0].ID, tc.want)
			}
		})
	}
}

func TestExecute_RatingCeilingHidesHigher(t *testing.T) {
	database, env := setupSearchDB(t)
	safeID := ingestTestImageWithID(t, database, env, "ceil_safe.png")
	expID := ingestTestImageWithID(t, database, env, "ceil_explicit.png")
	ingestTestImage(t, database, env, "ceil_untagged.png")

	attachTag(t, database, safeID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2 (general + untagged pass; explicit hidden)", result.Total)
	}
	for _, img := range result.Results {
		if img.ID == expID {
			t.Errorf("explicit image %d leaked through the ceiling", expID)
		}
	}
}

func TestHiddenByCeiling_RemembersAnOverrunUntilAWrite(t *testing.T) {
	database, env := setupSearchDB(t)
	expID := ingestTestImageWithID(t, database, env, "hidden_explicit.png")
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))
	src := testSource(database)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := HiddenByCeiling(expired, src, "rating:explicit", "sensitive"); err == nil {
		t.Fatal("a count past its deadline answered")
	}
	if n, err := HiddenByCeiling(context.Background(), src, "rating:explicit", "sensitive"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("after an overrun = %d, %v; want the overrun remembered", n, err)
	}
	src.Cache.Drop()
	if n, err := HiddenByCeiling(context.Background(), src, "rating:explicit", "sensitive"); err != nil || n != 1 {
		t.Errorf("after a write = %d, %v; want 1", n, err)
	}
}

func TestFastCountCeiling_MatchesSlowPath(t *testing.T) {
	database, env := setupSearchDB(t)
	genID := ingestTestImageWithID(t, database, env, "fc_general.png")
	expID := ingestTestImageWithID(t, database, env, "fc_explicit.png")
	ingestTestImage(t, database, env, "fc_untagged.png")

	attachTag(t, database, genID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	got, ok := fastCountCeiling(testSource(database), expr)
	if !ok {
		t.Fatal("fastCountCeiling should recognise the ceiling AST")
	}
	if got != 2 {
		t.Errorf("fastCountCeiling = %d, want 2", got)
	}
}

func TestFastCountCeiling_MultiRatingExact(t *testing.T) {
	database, env := setupSearchDB(t)
	multiID := ingestTestImageWithID(t, database, env, "mr_multi.png")
	genID := ingestTestImageWithID(t, database, env, "mr_general.png")
	ingestTestImage(t, database, env, "mr_untagged.png")

	attachTag(t, database, multiID, ratingTagID(t, database, "sensitive"))
	attachTag(t, database, multiID, ratingTagID(t, database, "explicit"))
	attachTag(t, database, genID, ratingTagID(t, database, "general"))

	expr := AndExpr{
		Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "sensitive"}},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	got, ok := fastCountCeiling(testSource(database), expr)
	if !ok {
		t.Fatal("fastCountCeiling should recognise the ceiling AST")
	}

	var exact int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM images
		WHERE is_missing = 0 AND id NOT IN (
			SELECT it.image_id FROM image_tags it
			JOIN tags t ON t.id = it.tag_id
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE tc.name = 'rating' AND t.name IN ('sensitive','questionable','explicit'))`).Scan(&exact); err != nil {
		t.Fatal(err)
	}
	if exact != 2 {
		t.Fatalf("fixture exact count = %d, want 2", exact)
	}
	if got != exact {
		t.Errorf("fastCountCeiling = %d, want %d (exact, not the usage-sum undercount)", got, exact)
	}
}

func TestRatingNegation_KeepsItsNotExistsMeaning(t *testing.T) {
	database, env := setupSearchDB(t)
	levels := contract.RatingLevels
	effective := map[int64]int{}
	for _, img := range []struct {
		name   string
		levels []string
	}{
		{"rn_general.png", []string{"general"}},
		{"rn_sensitive.png", []string{"sensitive"}},
		{"rn_questionable.png", []string{"questionable"}},
		{"rn_explicit.png", []string{"explicit"}},
		{"rn_multi.png", []string{"sensitive", "explicit"}},
		{"rn_unrated_a.png", nil},
		{"rn_unrated_b.png", nil},
	} {
		id := ingestTestImageWithID(t, database, env, img.name)
		effective[id] = -1
		for _, level := range img.levels {
			attachTag(t, database, id, ratingTagID(t, database, level))
			effective[id] = max(effective[id], contract.RatingRank(level))
		}
	}
	and := func(parts []Expr) Expr {
		e := parts[0]
		for _, p := range parts[1:] {
			e = AndExpr{Left: e, Right: p}
		}
		return e
	}
	sortedIDs := func(res []models.Image) []int64 {
		out := make([]int64, len(res))
		for i, im := range res {
			out[i] = im.ID
		}
		slices.Sort(out)
		return out
	}

	for mask := 1; mask < 1<<len(levels); mask++ {
		for ceiling := -1; ceiling < len(levels)-1; ceiling++ {
			var typed []Expr
			for i, level := range levels {
				if mask&(1<<i) != 0 {
					typed = append(typed, NotExpr{Expr: FilterExpr{Key: "rating", Val: level}})
				}
			}
			ceilingLevel := ""
			if ceiling >= 0 {
				ceilingLevel = levels[ceiling]
			}
			expr := ApplyCeiling(and(typed), ceilingLevel)
			var want []int64
			for id, rank := range effective {
				if (rank >= 0 && mask&(1<<rank) != 0) || (ceiling >= 0 && rank > ceiling) {
					continue
				}
				want = append(want, id)
			}
			slices.Sort(want)

			name := fmt.Sprintf("typed %04b under ceiling %d", mask, ceiling)
			res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
			if res.Total != len(want) || !slices.Equal(sortedIDs(res.Results), want) {
				t.Errorf("%s: Execute total %d rows %v, want %v", name, res.Total, sortedIDs(res.Results), want)
			}
			var paged []int64
			for page := 1; page <= len(effective); page++ {
				r := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: page, Limit: 2, Cached: true, Text: name})
				for _, im := range r.Results {
					paged = append(paged, im.ID)
				}
				if len(r.Results) < 2 {
					break
				}
			}
			slices.Sort(paged)
			if !slices.Equal(paged, want) {
				t.Errorf("%s: cached pages %v, want %v", name, paged, want)
			}
			scoped, err := NewScope(and(typed), ceilingLevel).IDs(testSource(database))
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(scoped)
			if !slices.Equal(scoped, want) {
				t.Errorf("%s: scope %v, want %v", name, scoped, want)
			}
		}
	}
}

func TestFastCountCeiling_WrappedUserExpr(t *testing.T) {
	database, env := setupSearchDB(t)
	safeBlueID := ingestTestImageWithID(t, database, env, "wc_safe_blue.png")
	safeRedID := ingestTestImageWithID(t, database, env, "wc_safe_red.png")
	expBlueID := ingestTestImageWithID(t, database, env, "wc_explicit_blue.png")
	ingestTestImage(t, database, env, "wc_untagged.png")

	generalID := categoryID(t, database, "general")
	blueID := insertTag(t, database, "blue_eyes", generalID, 0)
	attachTag(t, database, safeBlueID, blueID)
	attachTag(t, database, expBlueID, blueID)
	attachTag(t, database, safeBlueID, ratingTagID(t, database, "general"))
	attachTag(t, database, safeRedID, ratingTagID(t, database, "general"))
	attachTag(t, database, expBlueID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left: TagExpr{Tag: "blue_eyes"},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	if _, ok := fastCountCeiling(testSource(database), expr); ok {
		t.Fatal("fastCountCeiling should defer to slow COUNT for wrapped userExpr")
	}

	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1 (exact, not the loose min(2,3) upper bound)", result.Total)
	}
	if len(result.Results) != 1 || result.Results[0].ID != safeBlueID {
		t.Errorf("Execute results = %+v, want only safeBlueID=%d", result.Results, safeBlueID)
	}
}

func TestExecute_ACeilingWrappedLoneLiteralCountsExactly(t *testing.T) {
	t.Parallel()
	database, _ := setupSearchDB(t)
	seedVisibleRows(t, database, fastApproxThreshold+10)
	generalID := categoryID(t, database, "general")
	blueID := insertTag(t, database, "blue_eyes", generalID, 0)
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		attachTag(t, database, id, blueID)
	}
	attachTag(t, database, ids[2], ratingTagID(t, database, "explicit"))
	mustExec(t, database.Write, `UPDATE tags SET usage_count = ? WHERE id = ?`, fastApproxThreshold+1, blueID)

	var want int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE i.is_missing = 0 AND i.rating_rank <= 0
		 AND EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.tag_id = ?)`, blueID,
	).Scan(&want); err != nil {
		t.Fatal(err)
	}
	res := mustExecute(t, database, Query{Expr: ApplyCeiling(TagExpr{Tag: "blue_eyes"}, "general"), Sort: "newest", Page: 1, Limit: 40})
	if want != 2 || res.Total != want || len(res.Results) != want {
		t.Errorf("Total = %d with %d rows, want COUNT(*) = %d (2)", res.Total, len(res.Results), want)
	}
}

func TestCeilingWrapsLoneLiteral(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr Expr
		want bool
	}{
		{"a lone tag", ApplyCeiling(TagExpr{Tag: "blue_eyes"}, "general"), true},
		{"a category-qualified tag", ApplyCeiling(FilterExpr{Key: "character", Val: "hatsune_miku"}, "general"), true},
		{"a category-qualified wildcard", ApplyCeiling(FilterExpr{Key: "character", Val: "hatsune*"}, "general"), false},
		{"a keyword", ApplyCeiling(FilterExpr{Key: "type", Val: "image"}, "general"), false},
		{"a wildcard", ApplyCeiling(TagExpr{Tag: "hair", Wildcard: "substring"}, "general"), false},
		{"an AND", ApplyCeiling(AndExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, "sensitive"), false},
		{"a filter", ApplyCeiling(FilterExpr{Key: "inbox", Val: "true"}, "general"), false},
		{"the ceiling alone", ApplyCeiling(nil, "general"), false},
		{"no ceiling", TagExpr{Tag: "blue_eyes"}, false},
	} {
		if got := ceilingWrapsLoneLiteral(tc.expr); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFastTotalExact(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr Expr
		want bool
	}{
		{"a lone tag", TagExpr{Tag: "1girl"}, true},
		{"a wildcard sum", TagExpr{Tag: "hair", Wildcard: "substring"}, false},
		{"a negated tag", NotExpr{Expr: TagExpr{Tag: "1girl"}}, true},
		{"an AND's min", AndExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, false},
		{"an OR's sum", OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, false},
		{"the ceiling alone", ApplyCeiling(nil, "general"), true},
		{"a wildcard under a ceiling", ApplyCeiling(TagExpr{Tag: "hair", Wildcard: "substring"}, "general"), false},
		{"a category-qualified tag", FilterExpr{Key: "character", Val: "hatsune_miku"}, true},
		{"a category sum", FilterExpr{Key: "cat", Val: "character"}, false},
		{"the top rating", FilterExpr{Key: "rating", Val: "Explicit"}, true},
		{"a lower rating", FilterExpr{Key: "rating", Val: "general"}, false},
		{"a folder COUNT", FilterExpr{Key: "folder", Val: "a"}, true},
		{"visible minus untagged", FilterExpr{Key: "tagged", Val: "true"}, true},
		{"the inbox COUNT", FilterExpr{Key: "inbox", Val: "true"}, true},
	} {
		if got := fastTotalExact(tc.expr); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFastCountCeiling_WrappedNoFastBound(t *testing.T) {
	database, env := setupSearchDB(t)
	aID := ingestTestImageWithID(t, database, env, "nfb_a.png")
	bID := ingestTestImageWithID(t, database, env, "nfb_b.png")
	ingestTestImage(t, database, env, "nfb_c.png")

	attachTag(t, database, aID, ratingTagID(t, database, "general"))
	attachTag(t, database, bID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left:  FilterExpr{Key: "fav", Val: "true"},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	if _, ok := fastCountCeiling(testSource(database), expr); ok {
		t.Fatal("fastCountCeiling should defer to slow COUNT when userExpr can't be fast-bounded")
	}

	res := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if res.Total != 0 || len(res.Results) != 0 {
		t.Errorf("Execute total=%d results=%d, want 0/0", res.Total, len(res.Results))
	}
}

func TestExecute_CeilingWithMultiAndUserExpr(t *testing.T) {
	database, env := setupSearchDB(t)
	matchSafeID := ingestTestImageWithID(t, database, env, "ceil_match_safe.png")
	matchExpID := ingestTestImageWithID(t, database, env, "ceil_match_explicit.png")
	partialID := ingestTestImageWithID(t, database, env, "ceil_partial.png")
	ingestTestImage(t, database, env, "ceil_other.png")

	generalCatID := categoryID(t, database, "general")
	tagID := func(name string) int64 {
		id := insertTag(t, database, name, generalCatID, 0)
		return id
	}
	a, b, c, d := tagID("alpha"), tagID("bravo"), tagID("charlie"), tagID("delta")
	for _, id := range []int64{a, b, c, d} {
		attachTag(t, database, matchSafeID, id)
		attachTag(t, database, matchExpID, id)
	}

	for _, id := range []int64{a, b, c} {
		attachTag(t, database, partialID, id)
	}
	attachTag(t, database, matchSafeID, ratingTagID(t, database, "general"))
	attachTag(t, database, matchExpID, ratingTagID(t, database, "explicit"))

	user := AndExpr{Left: AndExpr{Left: AndExpr{Left: TagExpr{Tag: "alpha"}, Right: TagExpr{Tag: "bravo"}}, Right: TagExpr{Tag: "charlie"}}, Right: TagExpr{Tag: "delta"}}
	chain := AndExpr{Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "sensitive"}}, Right: AndExpr{Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}}, Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}}}}
	expr := AndExpr{Left: user, Right: chain}

	res := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40, Cached: true, Text: "ceiling-and4"})
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1 (only ceil_match_safe carries all four tags AND is sfw)", res.Total)
	}
	if len(res.Results) != 1 || res.Results[0].ID != matchSafeID {
		t.Errorf("Results = %+v, want only matchSafeID=%d", res.Results, matchSafeID)
	}
}

func TestExecute_CeilingCategoryQualifiedExact(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ccq_pop_safe.png")
	ingestTestImage(t, database, env, "ccq_pop_sens1.png")
	ingestTestImage(t, database, env, "ccq_pop_sens2.png")
	ingestTestImage(t, database, env, "ccq_pop_exp1.png")
	ingestTestImage(t, database, env, "ccq_pop_exp2.png")
	ingestTestImage(t, database, env, "ccq_other_safe.png")

	idOf := func(name string) int64 { return imageIDByName(t, database, name) }
	popSafe := idOf("ccq_pop_safe.png")
	popSens1 := idOf("ccq_pop_sens1.png")
	popSens2 := idOf("ccq_pop_sens2.png")
	popExp1 := idOf("ccq_pop_exp1.png")
	popExp2 := idOf("ccq_pop_exp2.png")
	otherSafe := idOf("ccq_other_safe.png")

	generalCatID := categoryID(t, database, "general")
	popID := insertTag(t, database, "popular", generalCatID, 0)
	for _, id := range []int64{popSafe, popSens1, popSens2, popExp1, popExp2} {
		attachTag(t, database, id, popID)
	}
	attachTag(t, database, popSafe, ratingTagID(t, database, "general"))
	attachTag(t, database, popSens1, ratingTagID(t, database, "sensitive"))
	attachTag(t, database, popSens2, ratingTagID(t, database, "sensitive"))
	attachTag(t, database, popExp1, ratingTagID(t, database, "explicit"))
	attachTag(t, database, popExp2, ratingTagID(t, database, "explicit"))
	attachTag(t, database, otherSafe, ratingTagID(t, database, "general"))

	user := FilterExpr{Key: "general", Val: "popular"}
	chain := AndExpr{
		Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "sensitive"}},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	expr := AndExpr{Left: user, Right: chain}

	if _, ok := fastCountCeiling(testSource(database), expr); ok {
		t.Fatal("fastCountCeiling should defer to slow COUNT for wrapped userExpr")
	}
	res := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40, Cached: true, Text: "ccq-popular"})
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1 (only popSafe is sfw); loose min(userCount=5, chainBound=2)=2 overshoots",
			res.Total)
	}
	if len(res.Results) != 1 || res.Results[0].ID != popSafe {
		t.Errorf("Results = %+v, want only popSafe=%d", res.Results, popSafe)
	}
}

func TestFastCountGenerated_HashMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	sdID := ingestTestImageWithID(t, database, env, "fg_sd.png")
	comfyID := ingestTestImageWithID(t, database, env, "fg_comfy.png")
	bothID := ingestTestImageWithID(t, database, env, "fg_both.png")
	otherID := ingestTestImageWithID(t, database, env, "fg_other.png")
	missingID := ingestTestImageWithID(t, database, env, "fg_missing.png")

	insertSD := func(id int64, hash string) {
		mustExec(t, database.Write, `INSERT INTO sd_metadata (image_id, generation_hash) VALUES (?, ?)`, id, hash)
	}
	insertComfy := func(id int64, hash string) {
		mustExec(t, database.Write, `INSERT INTO comfyui_metadata (image_id, generation_hash) VALUES (?, ?)`, id, hash)
	}
	insertSD(sdID, "abc123")
	insertComfy(comfyID, "abc123")
	insertSD(bothID, "abc123")
	insertComfy(bothID, "abc123")
	insertSD(otherID, "deadbeef")
	insertSD(missingID, "abc123")
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, missingID)

	got, ok := fastCountGenerated(testSource(database), FilterExpr{Key: "generated", Val: "abc123"})
	if !ok {
		t.Fatal("fastCountGenerated should answer for a known hash")
	}
	if got != 3 {
		t.Errorf("generated:abc123 = %d, want 3 (sd + comfy + both; missing excluded)", got)
	}

	got, ok = fastCountGenerated(testSource(database), FilterExpr{Key: "generated", Val: "true"})
	if !ok {
		t.Fatal("fastCountGenerated should answer for any value")
	}
	if got != 0 {
		t.Errorf("generated:true = %d, want 0 (literal hash 'true' matches nothing)", got)
	}
}

type fastCountCase struct {
	val  string
	want int
	ok   bool
}

func checkFastCount(t *testing.T, database *db.DB, count func(Source, FilterExpr) (int, bool), key string, cases []fastCountCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			got, ok := count(testSource(database), FilterExpr{Key: key, Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFastCountRating(t *testing.T) {
	database, env := setupSearchDB(t)
	genID := ingestTestImageWithID(t, database, env, "fr_general.png")
	expID := ingestTestImageWithID(t, database, env, "fr_explicit.png")
	ingestTestImage(t, database, env, "fr_untagged.png")

	attachTag(t, database, genID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))

	checkFastCount(t, database, fastCountRating, "rating", []fastCountCase{
		{"explicit", 1, true},
		{"general", 0, false},
		{"sensitive", 0, true},
		{"questionable", 0, true},

		{"not_a_level", 0, true},
	})
}

func TestFastCountFolder(t *testing.T) {
	database, env := setupSearchDB(t)

	for i, name := range []string{"root.png", "anime_a.png", "anime_b.png", "anime_sub.png", "deep_x.png", "deep_y.png", "anime_other.png"} {
		ingestTestImage(t, database, env, name)
		var folder string
		switch i {
		case 0:
			folder = ""
		case 1, 2:
			folder = "anime"
		case 3:
			folder = "anime/girls"
		case 4, 5:
			folder = "anime/girls/blue_eyes"
		case 6:
			folder = "anime_other"
		}
		mustExec(t, database.Write, `UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%' || ? || '%'`, folder, name)
	}

	checkFastCount(t, database, fastCountFolder, "folder", []fastCountCase{
		{"anime", 5, true},
		{"anime/girls", 3, true},
		{"missing_dir", 0, true},
	})
}

func TestFastCountFolder_CaseInsensitive(t *testing.T) {
	database, env := setupSearchDB(t)

	for i, name := range []string{"a.png", "b.png", "c.png"} {
		ingestTestImage(t, database, env, name)
		var folder string
		switch i {
		case 0:
			folder = "Characters"
		case 1:
			folder = "Characters/asuka"
		case 2:
			folder = "Other"
		}
		mustExec(t, database.Write, `UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%' || ? || '%'`, folder, name)
	}

	checkFastCount(t, database, fastCountFolder, "folder", []fastCountCase{
		{"Characters", 2, true},
		{"characters", 2, true},
		{"CHARACTERS", 2, true},
	})

	result := mustExecute(t, database, Query{
		Expr:  FilterExpr{Key: "folder", Val: "CHARACTERS"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 2 {
		t.Errorf("Execute folder:CHARACTERS Total = %d, want 2", result.Total)
	}
}

func TestFastCountFolder_MatchesSlowPath(t *testing.T) {
	database, env := setupSearchDB(t)

	for _, name := range []string{"a.png", "b.png", "c.png", "d.png"} {
		ingestTestImage(t, database, env, name)
	}
	mustExec(t, database.Write, `UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%a.png'`, "movies")
	mustExec(t, database.Write, `UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%b.png'`, "movies/scifi")
	mustExec(t, database.Write, `UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%c.png'`, "movies_other")

	result := mustExecute(t, database, Query{
		Expr:  FilterExpr{Key: "folder", Val: "movies"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 2 {
		t.Errorf("Execute Total = %d, want 2 (movies + movies/scifi; movies_other excluded)", result.Total)
	}
}

func TestFastCountAI_Csv(t *testing.T) {
	database, env := setupSearchDB(t)

	for _, st := range []struct {
		name       string
		sourceType string
	}{
		{"src_a.png", "a1111"},
		{"src_b.png", "comfyui"},
		{"src_ab.png", "a1111,comfyui"},
		{"src_none.png", "none"},
	} {
		ingestTestImage(t, database, env, st.name)
		mustExec(t, database.Write, `UPDATE images SET source_type = ? WHERE canonical_path LIKE '%' || ? || '%'`, st.sourceType, st.name)
	}

	checkFastCount(t, database, fastCountAI, "ai", []fastCountCase{
		{"a1111,comfyui", 3, true},
		{"comfyui,none", 3, true},
		{"sd,a1111", 0, false},
		{"a1111", 0, false},

		{"any", 0, false},
		{"none", 0, false},
		{"sd", 0, false},
		{"x,y", 0, true},
	})
}

func TestFastCountTagged_PartitionsVisible(t *testing.T) {
	database, env := setupSearchDB(t)
	taggedID := ingestTestImageWithID(t, database, env, "ft_tagged.png")
	ingestTestImage(t, database, env, "ft_untagged.png")
	autoID := ingestTestImageWithID(t, database, env, "ft_auto.png")
	tagID := getOrCreateTagID(t, database, "blue")
	attachTag(t, database, taggedID, tagID)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 1)`, autoID, tagID)
	mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID)

	cases := []struct {
		key, val string
		want     int
		ok       bool
	}{
		{"tagged", "true", 2, true},
		{"autotagged", "true", 1, true},

		{"tagged", "false", 0, false},
		{"autotagged", "false", 0, false},
	}
	for _, tc := range cases {
		name := tc.key + ":" + tc.val
		t.Run(name, func(t *testing.T) {
			got, ok := fastCountTagged(testSource(database), FilterExpr{Key: tc.key, Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}

	tres := mustExecute(t, database, Query{Expr: FilterExpr{Key: "tagged", Val: "true"}, Page: 1, Limit: 40})
	fres := mustExecute(t, database, Query{Expr: FilterExpr{Key: "tagged", Val: "false"}, Page: 1, Limit: 40})
	if tres.Total+fres.Total != 3 {
		t.Errorf("tagged:true+tagged:false = %d, want 3 (no overlap)", tres.Total+fres.Total)
	}
	for _, img := range tres.Results {
		if img.ID != taggedID && img.ID != autoID {
			t.Errorf("tagged:true returned untagged image id=%d", img.ID)
		}
	}
}

func TestFastCountLookup_NeverCountsImagesNotRows(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"fl_a.png", "fl_b.png", "fl_c.png"} {
		ingestTestImage(t, database, env, name)
	}
	var looked int64
	if err := database.Read.QueryRow(`SELECT MIN(id) FROM images`).Scan(&looked); err != nil {
		t.Fatal(err)
	}
	mustExec(t, database.Write, `INSERT INTO image_lookups (image_id, backend) VALUES (?, 'booru'), (?, 'ptr')`, looked, looked)

	got, ok := fastCountLookup(testSource(database), FilterExpr{Key: "lookup", Val: "never"})
	if !ok || got != 2 {
		t.Fatalf("fast count = %d (ok = %v), want 2", got, ok)
	}
	if _, ok := fastCountLookup(testSource(database), FilterExpr{Key: "lookup", Val: "due"}); ok {
		t.Error("only never has a fast count; the recorded-history values stay on the slow path")
	}
	res := mustExecute(t, database, Query{Expr: FilterExpr{Key: "lookup", Val: "never"}, Page: 1, Limit: 40})
	if res.Total != 2 || len(res.Results) != 2 {
		t.Errorf("Execute reported %d total over %d rows, want 2 and 2", res.Total, len(res.Results))
	}
}

func TestFastCountRating_RoutesThroughExecute(t *testing.T) {
	database, env := setupSearchDB(t)
	aID := ingestTestImageWithID(t, database, env, "fre_a.png")
	bID := ingestTestImageWithID(t, database, env, "fre_b.png")
	attachTag(t, database, aID, ratingTagID(t, database, "explicit"))
	attachTag(t, database, bID, ratingTagID(t, database, "explicit"))

	result := mustExecute(t, database, Query{
		Expr:  FilterExpr{Key: "rating", Val: "explicit"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	if len(result.Results) != 2 {
		t.Errorf("len(Results) = %d, want 2", len(result.Results))
	}
}

func TestExtractCeilingShape(t *testing.T) {
	notRating := func(level string) Expr { return NotExpr{Expr: FilterExpr{Key: "rating", Val: level}} }
	tests := []struct {
		name     string
		expr     Expr
		wantUser Expr
		wantRank int
		wantOK   bool
	}{
		{name: "nil", expr: nil, wantOK: false},
		{name: "single rating not", expr: notRating("explicit"), wantRank: 2, wantOK: true},
		{
			name:     "pure chain",
			expr:     AndExpr{Left: notRating("questionable"), Right: notRating("explicit")},
			wantRank: 1,
			wantOK:   true,
		},
		{
			name:     "wrapped tag",
			expr:     AndExpr{Left: TagExpr{Tag: "blue_eyes"}, Right: notRating("explicit")},
			wantUser: TagExpr{Tag: "blue_eyes"},
			wantRank: 2,
			wantOK:   true,
		},
		{name: "no rating predicates", expr: AndExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, wantOK: false},
		{
			name:   "or-wrapped chain stays as user",
			expr:   OrExpr{Left: notRating("explicit"), Right: TagExpr{Tag: "a"}},
			wantOK: false,
		},
		{name: "a lower level alone is no ceiling", expr: notRating("questionable"), wantOK: false},
		{
			name:     "a lower level beside the chain stays as user",
			expr:     AndExpr{Left: notRating("general"), Right: AndExpr{Left: notRating("questionable"), Right: notRating("explicit")}},
			wantUser: notRating("general"),
			wantRank: 1,
			wantOK:   true,
		},
		{
			name:   "a misspelt level is no ceiling",
			expr:   notRating("nsfw"),
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, rank, ok := extractCeilingShape(tt.expr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !exprEqual(user, tt.wantUser) {
				t.Errorf("user = %+v, want %+v", user, tt.wantUser)
			}
			if rank != tt.wantRank {
				t.Errorf("rank = %d, want %d", rank, tt.wantRank)
			}
		})
	}
}

func TestApplyCeiling_RoundTripsThroughTheShape(t *testing.T) {
	top := len(contract.RatingLevels) - 1
	for _, user := range []Expr{nil, TagExpr{Tag: "blue_eyes"}, OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}} {
		for _, level := range contract.RatingLevels[:top] {
			got, rank, ok := extractCeilingShape(ApplyCeiling(user, level))
			if !ok || !exprEqual(got, user) || rank != contract.RatingRank(level) {
				t.Errorf("%s over %+v: shape (%+v, %d, %v), want (%+v, %d, true)", level, user, got, rank, ok, user, contract.RatingRank(level))
			}
		}
		for _, level := range []string{"", contract.RatingLevels[top], "nsfw"} {
			if got := ApplyCeiling(user, level); !exprEqual(got, user) {
				t.Errorf("%q over %+v = %+v, want it unchanged", level, user, got)
			}
		}
	}
}

func exprEqual(a, b Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

func TestIsPureTagExpr(t *testing.T) {
	tests := []struct {
		name string
		expr Expr
		want bool
	}{
		{"literal tag", TagExpr{Tag: "1girl"}, true},
		{"prefix tag", TagExpr{Tag: "blue", Wildcard: "prefix"}, true},
		{"substring tag", TagExpr{Tag: "blue", Wildcard: "substring"}, true},
		{"and of tags", AndExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, true},
		{"or of tags", OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, true},
		{"not tag", NotExpr{Expr: TagExpr{Tag: "a"}}, true},
		{"cat: filter", FilterExpr{Key: "cat", Val: "character"}, true},
		{"tagged: filter", FilterExpr{Key: "tagged", Val: "true"}, true},
		{"autotagged: filter", FilterExpr{Key: "autotagged", Val: "true"}, true},
		{"category-qualified (unknown key)", FilterExpr{Key: "character", Val: "miku"}, true},
		{"colon tag fallback (unknown key)", FilterExpr{Key: "nier", Val: "automata"}, true},
		{"fav filter", FilterExpr{Key: "fav", Val: "true"}, false},
		{"ai filter", FilterExpr{Key: "ai", Val: "any"}, false},
		{"source exact filter", FilterExpr{Key: "source", Val: "Pixiv"}, false},
		{"folder filter", FilterExpr{Key: "folder", Val: "anime"}, false},
		{"file_size filter", FilterExpr{Key: "size", Val: ">=10MB"}, false},
		{"mime filter", FilterExpr{Key: "mime", Val: "png"}, false},
		{"width filter", FilterExpr{Key: "width", Val: ">=1024"}, true},
		{"height filter", FilterExpr{Key: "height", Val: ">=1024"}, true},
		{"date filter", FilterExpr{Key: "date", Val: "2024"}, true},
		{"ratio filter", FilterExpr{Key: "ratio", Val: ">=1.5"}, true},
		{"duration filter", FilterExpr{Key: "duration", Val: ">=30"}, false},
		{"name filter", FilterExpr{Key: "name", Val: "img"}, false},
		{"prompt filter", FilterExpr{Key: "prompt", Val: "masterpiece"}, false},
		{"seed filter", FilterExpr{Key: "seed", Val: "42"}, false},
		{"tagcount filter", FilterExpr{Key: "tagcount", Val: ">=5"}, true},
		{"tag and fav", AndExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPureTagExpr(tt.expr); got != tt.want {
				t.Errorf("isPureTagExpr(%+v) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestFastTagTotal(t *testing.T) {
	type seedTag struct {
		name, cat string
		usage     int
		aliasOf   string
	}
	tag := func(name string) TagExpr { return TagExpr{Tag: name} }
	prefix := func(name string) TagExpr { return TagExpr{Tag: name, Wildcard: "prefix"} }
	for _, tc := range []struct {
		name    string
		images  []string
		visible int
		tags    []seedTag
		expr    Expr
		want    int
		ok      bool
		why     string
	}{
		{name: "NonexistentTag", expr: tag("no_such_tag"), ok: true, why: "confirmed empty"},
		{name: "SingleCanonical", tags: []seedTag{{"hot", "general", 549514, ""}},
			expr: tag("hot"), want: 549514, ok: true},
		{name: "AliasFollowsCanonical", tags: []seedTag{{"feline", "general", 7, ""}, {"cat", "general", 0, "feline"}},
			expr: tag("cat"), want: 7, ok: true, why: "the canonical's usage_count"},
		{name: "MultipleCanonicalsFallthrough", tags: []seedTag{{"cat", "general", 3, ""}, {"cat", "character", 5, ""}},
			expr: tag("cat")},
		{name: "WildcardBelowThresholdFallsThrough", tags: []seedTag{{"blue_eyes", "general", 5, ""}, {"blue_hair", "general", 3, ""}},
			expr: prefix("blue")},
		{name: "WildcardCollapsesAlias", tags: []seedTag{{"blueberry", "general", 7, ""}, {"bluebell", "general", 0, "blueberry"}},
			expr: prefix("blue"), want: 7, ok: true, why: "alias and canonical collapse via DISTINCT COALESCE"},
		{name: "WildcardNeverExceedsTheLibrary", visible: 50000, tags: []seedTag{{"blue_eyes", "general", 45000, ""}, {"blue_hair", "general", 40000, ""}},
			expr: prefix("blue"), want: 50000, ok: true, why: "the whole library, not the 85000 raw sum"},
		{name: "WildcardEmpty", expr: prefix("nomatch_zzzzz"), ok: true, why: "confirmed empty"},
		{name: "WildcardEscapesMetacharacters", tags: []seedTag{{"foo_bar", "general", 10, ""}, {"fooXbar", "general", 99, ""}},
			expr: prefix("foo_b"), want: 10, ok: true, why: "only foo_bar, not fooXbar"},
		{name: "NotSingleTag", images: []string{"n_a.png", "n_b.png", "n_c.png"}, tags: []seedTag{{"hot", "general", 2, ""}},
			expr: NotExpr{Expr: tag("hot")}, want: 1, ok: true, why: "3 visible - 2 hot"},
		{name: "NotMissingTag", images: []string{"nm_a.png", "nm_b.png"},
			expr: NotExpr{Expr: tag("no_such_tag")}, want: 2, ok: true, why: "all visible"},

		{name: "AndPositiveTags", tags: []seedTag{{"a", "general", 100000, ""}, {"b", "general", 60000, ""}, {"c", "general", 200000, ""}},
			expr: AndExpr{Left: tag("a"), Right: AndExpr{Left: tag("b"), Right: tag("c")}}, want: 60000, ok: true, why: "min upper bound"},
		{name: "AndBelowThresholdFallsThrough", tags: []seedTag{{"cute", "general", 3, ""}, {"dog", "general", 2, ""}},
			expr: AndExpr{Left: tag("cute"), Right: tag("dog")}},
		{name: "OrCappedAtVisibleCount", images: []string{"or_cap.png"}, tags: []seedTag{{"a", "general", 30000, ""}, {"b", "general", 30000, ""}},
			expr: OrExpr{Left: tag("a"), Right: tag("b")}, want: 1, ok: true, why: "sum=60000 capped at visible=1"},
		{name: "OrBelowThresholdFallsThrough", tags: []seedTag{{"a", "general", 5, ""}, {"b", "general", 3, ""}},
			expr: OrExpr{Left: tag("a"), Right: tag("b")}},

		{name: "CatFilter", tags: []seedTag{{"miku", "character", 40000, ""}, {"haku", "character", 20000, ""}, {"1girl", "general", 100000, ""}},
			expr: FilterExpr{Key: "cat", Val: "character"}, want: 60000, ok: true, why: "sum within character, general excluded"},
		{name: "CatFilterBelowThresholdFallsThrough", tags: []seedTag{{"miku", "character", 5, ""}, {"haku", "character", 3, ""}},
			expr: FilterExpr{Key: "cat", Val: "character"}},
		{name: "CategoryQualifiedTag", tags: []seedTag{{"miku", "character", 5, ""}},
			expr: FilterExpr{Key: "character", Val: "miku"}, want: 5, ok: true},
		{name: "CategoryQualifiedMixedCase/asuka", tags: []seedTag{{"asuka", "character", 3, ""}},
			expr: FilterExpr{Key: "character", Val: "asuka"}, want: 3, ok: true},
		{name: "CategoryQualifiedMixedCase/Asuka", tags: []seedTag{{"asuka", "character", 3, ""}},
			expr: FilterExpr{Key: "character", Val: "Asuka"}, want: 3, ok: true},
		{name: "CategoryQualifiedMixedCase/ASUKA", tags: []seedTag{{"asuka", "character", 3, ""}},
			expr: FilterExpr{Key: "character", Val: "ASUKA"}, want: 3, ok: true},
		{name: "CategoryQualifiedFollowsAlias", tags: []seedTag{{"feline", "character", 9, ""}, {"cat", "character", 0, "feline"}},
			expr: FilterExpr{Key: "character", Val: "cat"}, want: 9, ok: true, why: "the canonical's usage_count"},
		{name: "CategoryQualifiedMissingTag", expr: FilterExpr{Key: "character", Val: "no_such_char"}, ok: true},
		{name: "CategoryQualifiedUnknownCategory", expr: FilterExpr{Key: "nier", Val: "automata"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.visible > 0 {
				t.Parallel()
			}
			database, env := setupSearchDB(t)
			if tc.visible > 0 {
				seedVisibleRows(t, database, tc.visible)
			}
			for _, name := range tc.images {
				ingestTestImage(t, database, env, name)
			}
			ids := map[string]int64{}
			for _, st := range tc.tags {
				catID := categoryID(t, database, st.cat)
				if st.aliasOf != "" {
					mustExec(t, database.Write, `INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES (?, ?, 1, ?)`, st.name, catID, ids[st.aliasOf])
					continue
				}
				ids[st.name] = insertTag(t, database, st.name, catID, st.usage)
			}

			n, ok := fastTagTotal(testSource(database), tc.expr)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && n != tc.want {
				t.Errorf("count = %d, want %d (%s)", n, tc.want, tc.why)
			}
		})
	}
}

func TestFastTagTotal_AndWithAnUnknownTagIsZero(t *testing.T) {
	database, _ := setupSearchDB(t)
	generalID := categoryID(t, database, "general")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES ('popular', ?, 549514)`, generalID)

	for _, q := range []string{"popular nonexistent", "nonexistent popular", "nonexistent width:>100"} {
		t.Run(q, func(t *testing.T) {
			n, ok := fastTagTotal(testSource(database), Parse(q))
			if !ok || n != 0 {
				t.Errorf("fastTagTotal = (%d, %v), want (0, true)", n, ok)
			}
		})
	}
}

func TestFastTagTotal_Wildcards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tag, wildcard string

		tags [3]struct {
			name  string
			usage int
		}
	}{
		{"prefix", "blue", "prefix", [3]struct {
			name  string
			usage int
		}{{"blue_eyes", 40000}, {"blue_hair", 20000}, {"green_eyes", 30}}},
		{"substring", "blue", "substring", [3]struct {
			name  string
			usage int
		}{{"blue_eyes", 40000}, {"light_blue", 20000}, {"green_eyes", 30}}},
		{"suffix", "_eyes", "suffix", [3]struct {
			name  string
			usage int
		}{{"blue_eyes", 40000}, {"green_eyes", 20000}, {"blue_hair", 30}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database, _ := setupSearchDB(t)
			seedVisibleRows(t, database, 60000)
			generalID := categoryID(t, database, "general")
			for _, tag := range tc.tags {
				mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?)`, tag.name, generalID, tag.usage)
			}

			n, ok := fastTagTotal(testSource(database), TagExpr{Tag: tc.tag, Wildcard: tc.wildcard})
			if !ok {
				t.Fatalf("wildcard %s should hit fast path", tc.wildcard)
			}
			if n != 60000 {
				t.Errorf("count = %d, want 60000", n)
			}
		})
	}
}

func TestFastTagTotal_RejectsNonRecognisedShapes(t *testing.T) {
	database, _ := setupSearchDB(t)

	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES ('a', ?, 60000)`, categoryID(t, database, "general"))
	for _, key := range []string{"fav", "width", "date"} {
		if _, ok := fastTagTotal(testSource(database), FilterExpr{Key: key, Val: "true"}); ok {
			t.Errorf("FilterExpr{%q} should fall through to slow path", key)
		}
	}
	mixed := AndExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}
	if _, ok := fastTagTotal(testSource(database), mixed); ok {
		t.Error("AND with FilterExpr{fav} leaf should fall through")
	}
	mixedOr := OrExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}
	if _, ok := fastTagTotal(testSource(database), mixedOr); ok {
		t.Error("OR with FilterExpr{fav} leaf should fall through")
	}
	if _, ok := fastTagTotal(testSource(database), NotExpr{Expr: TagExpr{Tag: "blue", Wildcard: "prefix"}}); ok {
		t.Error("NOT with wildcard inner should fall through")
	}
	if _, ok := fastTagTotal(testSource(database), NotExpr{Expr: FilterExpr{Key: "fav", Val: "true"}}); ok {
		t.Error("NOT with non-tag inner should fall through")
	}
}

func TestExecute_FastTagTotal_EmptyShortCircuits(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fast_empty.png")

	result := mustExecute(t, database, Query{
		Expr:  TagExpr{Tag: "tag_that_does_not_exist"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 0 || len(result.Results) != 0 {
		t.Errorf("Total = %d, Results = %d, want both 0", result.Total, len(result.Results))
	}
}

func TestExecute_AndDriverMultiTag(t *testing.T) {
	database, env := setupSearchDB(t)
	matchID := ingestTestImageWithID(t, database, env, "and_match.png")
	pairID := ingestTestImageWithID(t, database, env, "and_pair_only.png")
	singleID := ingestTestImageWithID(t, database, env, "and_single.png")

	generalID := categoryID(t, database, "general")

	mkTag := func(name string) int64 { return insertTag(t, database, name, generalID, 0) }
	tagA := mkTag("alpha")
	tagB := mkTag("bravo")
	tagC := mkTag("charlie")

	attachTag(t, database, matchID, tagA)
	attachTag(t, database, matchID, tagB)
	attachTag(t, database, matchID, tagC)
	attachTag(t, database, pairID, tagA)
	attachTag(t, database, pairID, tagB)
	attachTag(t, database, singleID, tagA)

	expr := AndExpr{
		Left:  AndExpr{Left: TagExpr{Tag: "alpha"}, Right: TagExpr{Tag: "bravo"}},
		Right: TagExpr{Tag: "charlie"},
	}
	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1", result.Total)
	}
	if len(result.Results) != 1 || result.Results[0].ID != matchID {
		t.Errorf("matched = %v, want only %d", result.Results, matchID)
	}
}

func TestExecute_AndDriverPreservesOrAndNot(t *testing.T) {
	database, env := setupSearchDB(t)
	idA := ingestTestImageWithID(t, database, env, "or_a.png")
	idB := ingestTestImageWithID(t, database, env, "or_b.png")
	ingestTestImage(t, database, env, "or_c.png")

	generalID := categoryID(t, database, "general")

	mkTag := func(name string) int64 { return insertTag(t, database, name, generalID, 0) }
	tagX := mkTag("xray")
	tagY := mkTag("yankee")

	attachTag(t, database, idA, tagX)
	attachTag(t, database, idB, tagY)

	expr := OrExpr{Left: TagExpr{Tag: "xray"}, Right: TagExpr{Tag: "yankee"}}
	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
}

func TestExecute_DriverLeafRepeatedUnderNotOrOr(t *testing.T) {
	database, env := setupSearchDB(t)
	generalID := categoryID(t, database, "general")
	blue := insertTag(t, database, "blue", generalID, 0)
	red := insertTag(t, database, "red", generalID, 0)
	both := ingestTestImageWithID(t, database, env, "rep_both.png")
	onlyRed := ingestTestImageWithID(t, database, env, "rep_red.png")
	attachTag(t, database, both, blue)
	attachTag(t, database, both, red)
	attachTag(t, database, onlyRed, red)

	for q, want := range map[string]int{
		"blue red -blue":         0,
		"red blue -red":          0,
		"blue red blue OR green": 1,
		"red blue red OR x":      1,
	} {
		for _, sort := range []string{"newest", "random"} {
			res, err := Execute(context.Background(), testSource(database), Query{Expr: Parse(q), Sort: sort, RandomSeed: 7, Page: 1, Limit: 40})
			if err != nil {
				t.Errorf("%q (%s): %v", q, sort, err)
				continue
			}
			if res.Total != want || len(res.Results) != want {
				t.Errorf("%q (%s): total %d rows %d, want %d", q, sort, res.Total, len(res.Results), want)
			}
		}
	}
}

func TestPickAndDriverTag_SingleWildcard(t *testing.T) {
	database, _ := setupSearchDB(t)
	generalID := categoryID(t, database, "general")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES ('blue_eyes', ?, 12), ('blue_hair', ?, 7), ('red_hair',  ?, 5)`, generalID, generalID, generalID)

	legs, ok := pickAndDriverTag(database, TagExpr{Tag: "blue", Wildcard: "prefix"}, false)
	if !ok {
		t.Fatal("single wildcard should engage the driver")
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1 single-leaf path", len(legs))
	}
	leaf0, ok0 := legs[0].leaf.(TagExpr)
	if !ok0 || leaf0.Tag != "blue" || leaf0.Wildcard != "prefix" {
		t.Errorf("driver leaf = %+v, want TagExpr{blue, prefix}", legs[0].leaf)
	}
	if len(legs[0].ids) != 2 {
		t.Errorf("driver canonicals = %d, want 2 (blue_eyes + blue_hair)", len(legs[0].ids))
	}
}

func TestPickAndDriverTag_SingleLiteralStillSkips(t *testing.T) {
	database, _ := setupSearchDB(t)
	generalID := categoryID(t, database, "general")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES ('alpha',   ?, 5), ('popular', ?, ?)`, generalID, generalID, andDriverThreshold+10)
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "alpha"}, false); ok {
		t.Error("single literal at root should not engage the driver under indexed sort")
	}
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "alpha"}, true); !ok {
		t.Error("single literal at root should engage the driver under random sort")
	}
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "popular"}, true); !ok {
		t.Error("popular single literal under random sort should still engage the driver")
	}
}

func TestExecute_RandomSortSingleTagDriver(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"r_blue1.png", "r_blue2.png", "r_red.png"} {
		ingestTestImage(t, database, env, name)
	}
	blue1 := imageIDByName(t, database, "r_blue1.png")
	blue2 := imageIDByName(t, database, "r_blue2.png")
	red := imageIDByName(t, database, "r_red.png")

	generalID := categoryID(t, database, "general")
	blueID := insertTag(t, database, "blue_eyes", generalID, 0)
	redID := insertTag(t, database, "red_eyes", generalID, 0)
	attachTag(t, database, blue1, blueID)
	attachTag(t, database, blue2, blueID)
	attachTag(t, database, red, redID)

	result := mustExecute(t, database, Query{
		Expr:       TagExpr{Tag: "blue_eyes"},
		Sort:       "random",
		RandomSeed: 1234567890,
		Page:       1,
		Limit:      40,
	})
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	got := map[int64]bool{}
	for _, img := range result.Results {
		got[img.ID] = true
	}
	if !got[blue1] || !got[blue2] || got[red] {
		t.Errorf("results = %+v, want exactly {blue1, blue2}", got)
	}
}

func TestPickAndDriverTag_PopularIntersect(t *testing.T) {
	database, _ := setupSearchDB(t)
	generalID := categoryID(t, database, "general")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count) VALUES ('a_pop',  ?, ?), ('b_pop',  ?, ?), ('c_pop',  ?, ?), ('d_rare', ?, 5)`, generalID, andDriverThreshold+10, generalID, andDriverThreshold+20, generalID, andDriverThreshold+30, generalID)

	popular := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "a_pop"},
			Right: TagExpr{Tag: "b_pop"},
		},
		Right: TagExpr{Tag: "c_pop"},
	}
	legs, ok := pickAndDriverTag(database, popular, false)
	if !ok {
		t.Fatal("popular 3-AND should engage the driver via INTERSECT")
	}
	if len(legs) != 2 {
		t.Errorf("legs = %d, want 2 (cap at the two least-popular leaves)", len(legs))
	}
	picked := map[string]bool{}
	for _, l := range legs {
		tl, ok := l.leaf.(TagExpr)
		if !ok {
			t.Fatalf("leg leaf = %T, want TagExpr", l.leaf)
		}
		picked[tl.Tag] = true
	}
	if !picked["a_pop"] || !picked["b_pop"] {
		t.Errorf("picked = %v, want {a_pop, b_pop} (least-popular pair)", picked)
	}
	if picked["c_pop"] {
		t.Errorf("c_pop is the most popular leaf and should fall through to correlated EXISTS")
	}

	mixed := AndExpr{
		Left:  popular,
		Right: TagExpr{Tag: "d_rare"},
	}
	legs2, ok := pickAndDriverTag(database, mixed, false)
	if !ok {
		t.Fatal("rare-tag-wins shape should engage the single-leg driver")
	}
	if len(legs2) != 1 {
		t.Fatalf("legs = %d, want 1 single-leg path on the rare leaf", len(legs2))
	}
	tl2, ok2 := legs2[0].leaf.(TagExpr)
	if !ok2 || tl2.Tag != "d_rare" {
		t.Errorf("driver leaf = %+v, want TagExpr{d_rare}", legs2[0].leaf)
	}
}

func TestPickAndDriverTag_CategoryQualifiedFilterAtRoot(t *testing.T) {
	database, _ := setupSearchDB(t)
	charID := categoryID(t, database, "character")
	mikuID := insertTag(t, database, "miku", charID, 12)

	filt := FilterExpr{Key: "character", Val: "miku"}
	if _, ok := pickAndDriverTag(database, filt, false); ok {
		t.Error("single FilterExpr at root should not engage the driver under indexed sort")
	}
	legs, ok := pickAndDriverTag(database, filt, true)
	if !ok {
		t.Fatal("single FilterExpr at root should engage the driver under allowSingleLiteral")
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(legs))
	}
	fl, isFilter := legs[0].leaf.(FilterExpr)
	if !isFilter || fl.Key != "character" || fl.Val != "miku" {
		t.Errorf("driver leaf = %+v, want FilterExpr{character:miku}", legs[0].leaf)
	}
	if len(legs[0].ids) != 1 || legs[0].ids[0] != mikuID {
		t.Errorf("driver ids = %v, want [%d]", legs[0].ids, mikuID)
	}
}

func TestExecute_RandomSortCategoryFilterDriver(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"cm1.png", "cm2.png", "other.png"} {
		ingestTestImage(t, database, env, name)
	}
	cm1 := imageIDByName(t, database, "cm1.png")
	cm2 := imageIDByName(t, database, "cm2.png")
	other := imageIDByName(t, database, "other.png")

	var charID, generalID int64
	charID = categoryID(t, database, "character")
	generalID = categoryID(t, database, "general")
	mikuID := insertTag(t, database, "miku", charID, 0)
	redID := insertTag(t, database, "red", generalID, 0)
	attachTag(t, database, cm1, mikuID)
	attachTag(t, database, cm2, mikuID)
	attachTag(t, database, other, redID)

	result := mustExecute(t, database, Query{
		Expr:       FilterExpr{Key: "character", Val: "miku"},
		Sort:       "random",
		RandomSeed: 42,
		Page:       1,
		Limit:      40,
	})
	got := map[int64]bool{}
	for _, img := range result.Results {
		got[img.ID] = true
	}
	if !got[cm1] || !got[cm2] || got[other] {
		t.Errorf("results = %+v, want exactly {cm1, cm2}", got)
	}
}

func TestCeilingChainRewritesToColumnPredicate(t *testing.T) {
	database, _ := setupSearchDB(t)

	chain := AndExpr{
		Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "sensitive"}},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	where, args, _ := buildWhereDB(chain, testSource(database))
	wantIn(t, where, "i.rating_rank <= ?")
	wantNotIn(t, where, "NOT EXISTS")
	if len(args) != 1 || args[0] != 0 {
		t.Errorf("args = %v, want [0] for ceiling=general", args)
	}

	mixed := AndExpr{Left: TagExpr{Tag: "blue_eyes"}, Right: chain}
	whereMix, argsMix, _ := buildWhereDB(mixed, testSource(database))
	wantIn(t, whereMix, "i.rating_rank <= ?")
	wantIn(t, whereMix, "image_tags")
	if len(argsMix) != 2 || argsMix[0] != 0 || argsMix[1] != "blue_eyes" {
		t.Errorf("mixed args = %v, want [0, \"blue_eyes\"]", argsMix)
	}

	bogus := NotExpr{Expr: FilterExpr{Key: "rating", Val: "ultra"}}
	whereBogus, argsBogus, _ := buildWhereDB(bogus, testSource(database))
	wantNotIn(t, whereBogus, "rating_rank")
	if len(argsBogus) != 0 {
		t.Errorf("unknown-level args = %v, want []", argsBogus)
	}
}

func TestRatingRankTrigger_RoundTrip(t *testing.T) {
	database, env := setupSearchDB(t)
	imgID := ingestTestImageWithID(t, database, env, "r.png")

	read := func() int {
		t.Helper()
		var n int
		if err := database.Read.QueryRow(`SELECT rating_rank FROM images WHERE id = ?`, imgID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ratingID := func(name string) int64 { return ratingTagID(t, database, name) }

	if r := read(); r != -1 {
		t.Errorf("unrated image rating_rank = %d, want -1", r)
	}

	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, imgID, ratingID("sensitive"))
	if r := read(); r != 1 {
		t.Errorf("after insert sensitive: rating_rank = %d, want 1", r)
	}

	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, imgID, ratingID("explicit"))
	if r := read(); r != 3 {
		t.Errorf("after insert explicit: rating_rank = %d, want 3", r)
	}

	mustExec(t, database.Write, `DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, ratingID("explicit"))
	if r := read(); r != 1 {
		t.Errorf("after delete explicit: rating_rank = %d, want 1", r)
	}

	mustExec(t, database.Write, `DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, ratingID("sensitive"))
	if r := read(); r != -1 {
		t.Errorf("after delete last rating: rating_rank = %d, want -1", r)
	}
}

func TestExecute_IDListsPastTheVariableCap(t *testing.T) {
	t.Parallel()
	database, env := setupSearchDB(t)
	const n = 33000
	generalID := categoryID(t, database, "general")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id, usage_count)
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		SELECT printf('wide_%05d', i), ?, 1 FROM seq`, n, generalID)
	tagged := ingestTestImageWithID(t, database, env, "wide.png")
	attachTag(t, database, tagged, getOrCreateTagID(t, database, "wide_00001"))
	res, err := Execute(context.Background(), testSource(database), Query{Expr: Parse("wide_*"), Sort: "newest", Page: 1, Limit: 40})
	if err != nil || res.Total != 1 {
		t.Errorf("wide_* over %d tags: total %v, err %v", n, res, err)
	}

	seedVisibleRows(t, database, n)
	mustExec(t, database.Write, `UPDATE images SET phash = id`)
	res, err = Execute(context.Background(), Source{DB: database, Tree: relations.NewBKTree(), Tallies: testSource(database).Tallies, Cache: testSource(database).Cache}, Query{Expr: Parse("phash:0000000000000000~40"), Sort: "newest", Page: 1, Limit: 40})
	if err != nil || res.Total != n+1 {
		t.Errorf("phash within 40 of every row: total %v, err %v, want %d", res, err, n+1)
	}
}

func TestExecute_CategoryQualifiedQuotedValueFolds(t *testing.T) {
	database, env := setupSearchDB(t)
	id := ingestTestImageWithID(t, database, env, "john.png")
	attachTag(t, database, id, insertTag(t, database, "john_doe", categoryID(t, database, "artist"), 0))
	for _, q := range []string{`artist:"john doe"`, `artist:"John Doe"`, `artist:john_doe`} {
		res := mustExecute(t, database, Query{Expr: Parse(q), Sort: "newest", Page: 1, Limit: 40})
		if res.Total != 1 || len(res.Results) != 1 {
			t.Errorf("%s: total %d rows %d, want 1", q, res.Total, len(res.Results))
		}
	}
}

func TestExecute_TagCountPerCategory(t *testing.T) {
	database, env := setupSearchDB(t)
	character := categoryID(t, database, "character")
	reimu := insertTag(t, database, "reimu", character, 0)
	marisa := insertTag(t, database, "marisa", character, 0)
	swimsuit := insertTag(t, database, "reimu_(swimsuit)", character, 0)
	shrine := insertTag(t, database, "shrine", categoryID(t, database, "copyright"), 0)
	zun := insertTag(t, database, "zun", categoryID(t, database, "artist"), 0)
	mustExec(t, database.Write, `INSERT INTO tag_implications (parent_tag_id, implied_tag_id) VALUES (?, ?), (?, ?)`,
		swimsuit, reimu, shrine, reimu)
	implied := func(imageID, tagID int64) {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_implied) VALUES (?, ?, 1)`, imageID, tagID)
		mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID)
	}

	solo := ingestTestImageWithID(t, database, env, "solo.png")
	attachTag(t, database, solo, reimu)
	pair := ingestTestImageWithID(t, database, env, "pair.png")
	attachTag(t, database, pair, reimu)
	attachTag(t, database, pair, marisa)
	variant := ingestTestImageWithID(t, database, env, "variant.png")
	attachTag(t, database, variant, swimsuit)
	implied(variant, reimu)
	bothDirect := ingestTestImageWithID(t, database, env, "both.png")
	attachTag(t, database, bothDirect, swimsuit)
	attachTag(t, database, bothDirect, reimu)
	fromCopyright := ingestTestImageWithID(t, database, env, "copyright.png")
	attachTag(t, database, fromCopyright, shrine)
	implied(fromCopyright, reimu)
	none := ingestTestImageWithID(t, database, env, "none.png")
	attachTag(t, database, none, zun)

	for q, want := range map[string][]int64{
		"tagcount:character=1":                   {solo, variant, bothDirect, fromCopyright},
		"tagcount:character=2":                   {pair},
		"tagcount:character=0":                   {none},
		"tagcount:character>=1":                  {solo, pair, variant, bothDirect, fromCopyright},
		"tagcount:character<2":                   {solo, variant, bothDirect, fromCopyright, none},
		"tagcount:character=0..1":                {solo, variant, bothDirect, fromCopyright, none},
		"tagcount:character=2..":                 {pair},
		"-tagcount:character=1":                  {pair, none},
		"character:reimu tagcount:character=1":   {solo, variant, bothDirect, fromCopyright},
		"character:marisa tagcount:character=1":  nil,
		"tagcount:artist=0 tagcount:character=1": {solo, variant, bothDirect, fromCopyright},
		"tagcount:nosuch=0":                      nil,
		"tagcount:character=x":                   nil,
	} {
		res := mustExecute(t, database, Query{Expr: Parse(q), Page: 1, Limit: 40})
		got := resultIDs(res)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) || res.Total != len(want) {
			t.Errorf("%s: matched %v (total %d), want %v", q, got, res.Total, want)
		}
	}
}

func TestExecute_QuotedTermWithAColonFindsTheTag(t *testing.T) {
	database, env := setupSearchDB(t)
	id := ingestTestImageWithID(t, database, env, "rezero.png")
	general := categoryID(t, database, "general")
	attachTag(t, database, id, insertTag(t, database, "re:zero", general, 0))
	attachTag(t, database, id, insertTag(t, database, "source:code", general, 0))
	for q, want := range map[string]int{
		`"re:zero"`:      1,
		`"source:code"`:  1,
		`-"source:code"`: 0,
	} {
		res := mustExecute(t, database, Query{Expr: Parse(q), Sort: "newest", Page: 1, Limit: 40})
		if res.Total != want || len(res.Results) != want {
			t.Errorf("%s: total %d rows %d, want %d", q, res.Total, len(res.Results), want)
		}
	}
}

func TestExecute_NameMatchesAnAliasBasename(t *testing.T) {
	database, env := setupSearchDB(t)
	id := ingestTestImageWithID(t, database, env, "canonical_one.png")
	mustExec(t, database.Write, `INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, id, filepath.Join(env.galleryDir, "dupes", "twin_copy.png"))
	for _, q := range []string{"name:twin_copy", "name:win_co", "name:tw", "name:canonical"} {
		res := mustExecute(t, database, Query{Expr: Parse(q), Sort: "newest", Page: 1, Limit: 40})
		if res.Total != 1 {
			t.Errorf("%s: total %d, want 1", q, res.Total)
		}
	}
}

func TestExecute_AShortNameKeepsANonASCIICapital(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImageWithID(t, database, env, "\u00c9t\u00e9.png")
	res := mustExecute(t, database, Query{Expr: Parse("name:\u00c9t"), Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 1 {
		t.Errorf("name:\u00c9t total %d, want 1", res.Total)
	}
}

func TestPinnedCollectionName(t *testing.T) {
	for q, want := range map[string]string{
		"collection:Trip":                  "Trip",
		"blue collection:Trip":             "Trip",
		"collection:any":                   "",
		"collection:ANY":                   "",
		"collection:Trip collection:other": "",
		"collection:Trip OR red":           "",
	} {
		if got := PinnedCollectionName(Parse(q)); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
}

func TestApplyAndDriver_IntersectSQL(t *testing.T) {
	legs := []andDriverLeg{
		{leaf: TagExpr{Tag: "a"}, ids: []int64{1, 2}},
		{leaf: TagExpr{Tag: "b"}, ids: []int64{3, 4, 5}},
	}
	where, args := applyAndDriver("", nil, legs)
	wantPrefix := "i.id IN (SELECT image_id FROM image_tags WHERE tag_id IN (1,2) INTERSECT SELECT image_id FROM image_tags WHERE tag_id IN (3,4,5))"
	if where != wantPrefix {
		t.Errorf("multi-leg SQL = %q, want %q", where, wantPrefix)
	}
	if len(args) != 0 {
		t.Errorf("args len = %d, want 0", len(args))
	}

	single := []andDriverLeg{{leaf: TagExpr{Tag: "a"}, ids: []int64{1, 2}}}
	whereSingle, _ := applyAndDriver("", nil, single)
	wantNotIn(t, whereSingle, "INTERSECT")
}

func TestExecute_PopularAndIntersect(t *testing.T) {
	database, env := setupSearchDB(t)
	allID := ingestTestImageWithID(t, database, env, "all_three.png")
	abID := ingestTestImageWithID(t, database, env, "ab_only.png")
	aID := ingestTestImageWithID(t, database, env, "a_only.png")

	generalID := categoryID(t, database, "general")
	mkTag := func(name string, usage int) int64 { return insertTag(t, database, name, generalID, usage) }

	a := mkTag("a_int", andDriverThreshold+1)
	b := mkTag("b_int", andDriverThreshold+1)
	c := mkTag("c_int", andDriverThreshold+1)
	for _, p := range []struct {
		img int64
		tag int64
	}{
		{allID, a}, {allID, b}, {allID, c},
		{abID, a}, {abID, b},
		{aID, a},
	} {
		attachTag(t, database, p.img, p.tag)
	}

	expr := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "a_int"},
			Right: TagExpr{Tag: "b_int"},
		},
		Right: TagExpr{Tag: "c_int"},
	}
	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if len(result.Results) != 1 || result.Results[0].ID != allID {
		t.Errorf("Results = %+v, want only %d (all_three)", result.Results, allID)
	}
}

func TestExecute_RecentIDBoundSkipsAsc(t *testing.T) {
	database, _ := setupSearchDB(t)

	mustExec(t, database.Write, ` WITH RECURSIVE seq(n) AS ( SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 150 ) INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at) SELECT '/tmp/asc_' || n || '.png', 'png', 1024, 'asc_sha_' || n, datetime('now') FROM seq `)

	var oldestID, newestID int64
	if err := database.Read.QueryRow(
		`SELECT MIN(id), MAX(id) FROM images`,
	).Scan(&oldestID, &newestID); err != nil {
		t.Fatal(err)
	}

	generalID := categoryID(t, database, "general")
	mkTag := func(name string, usage int) int64 { return insertTag(t, database, name, generalID, usage) }

	lo := mkTag("lo_tag", andDriverThreshold+1)
	hi := mkTag("hi_tag", andDriverThreshold+1)
	for _, id := range []int64{oldestID, newestID} {
		attachTag(t, database, id, lo)
		attachTag(t, database, id, hi)
	}

	res := mustExecute(t, database, Query{
		Expr: AndExpr{
			Left:  TagExpr{Tag: "lo_tag"},
			Right: TagExpr{Tag: "hi_tag"},
		},
		Sort:  "newest",
		Order: "asc",
		Page:  1,
		Limit: 1,
	})
	if len(res.Results) != 1 || res.Results[0].ID != oldestID {
		t.Errorf("Results = %+v, want oldest carrier id=%d (asc order)", res.Results, oldestID)
	}
}

func TestExecute_AndDriverWildcardReplacesListSubquery(t *testing.T) {
	database, env := setupSearchDB(t)
	blue1ID := ingestTestImageWithID(t, database, env, "wd_blue1.png")
	blue2ID := ingestTestImageWithID(t, database, env, "wd_blue2.png")
	otherID := ingestTestImageWithID(t, database, env, "wd_other.png")
	ingestTestImage(t, database, env, "wd_none.png")

	generalID := categoryID(t, database, "general")
	mkTag := func(name string) int64 { return insertTag(t, database, name, generalID, 0) }
	blueEyes := mkTag("blue_eyes")
	blueHair := mkTag("blue_hair")
	redHair := mkTag("red_hair")
	attachTag(t, database, blue1ID, blueEyes)
	attachTag(t, database, blue2ID, blueHair)
	attachTag(t, database, otherID, redHair)

	result := mustExecute(t, database, Query{
		Expr:  TagExpr{Tag: "blue", Wildcard: "prefix"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2 (blue_eyes + blue_hair carriers)", result.Total)
	}
	matched := map[int64]bool{}
	for _, img := range result.Results {
		matched[img.ID] = true
	}
	if !matched[blue1ID] || !matched[blue2ID] {
		t.Errorf("results missing blue carriers: %v", result.Results)
	}
	if matched[otherID] {
		t.Errorf("non-blue image %d leaked into results", otherID)
	}
}

func TestExecute_RatingUnusedShortCircuits(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "rating_unused.png")

	result := mustExecute(t, database, Query{
		Expr:  FilterExpr{Key: "rating", Val: "explicit"},
		Page:  1,
		Limit: 40,
	})
	if result.Total != 0 || len(result.Results) != 0 {
		t.Errorf("Total = %d, Results = %d, want both 0", result.Total, len(result.Results))
	}
}

func TestExecute_FullSync(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	if _, err := gallery.Sync(context.Background(), testHandle(database, env.galleryDir, env.thumbnailsDir), env.maxFileSizeMB, gallery.Naming{}, func(int, int, string) {}, nil, nil); err != nil {
		t.Fatal(err)
	}

	result := mustExecute(t, database, Query{Page: 1, Limit: 40})
	if result.Total < 2 {
		t.Errorf("Total = %d, want >= 2", result.Total)
	}
}

func TestBuildOrder(t *testing.T) {
	for _, tc := range []struct {
		name, sort, order string
		seed              int64
		want              []string
	}{
		{"Newest", "newest", "", 0, []string{"DESC", "ingested_at"}},
		{"NewestAsc", "newest", "asc", 0, []string{"ASC"}},
		{"Filesize", "filesize", "", 0, []string{"file_size", "DESC"}},
		{"FilesizeAsc", "filesize", "asc", 0, []string{"file_size", "ASC"}},
		{"Order", "order", "", 0, []string{"i.series ASC", "i.series_order IS NULL", "i.series_order ASC", "i.id ASC"}},
		{"OrderDesc", "order", "desc", 0, []string{"i.series DESC", "i.series_order IS NULL", "i.series_order DESC", "i.id DESC"}},
		{"Unknown", "unknown_sort", "", 0, []string{"ingested_at", "DESC"}},
		{"Random", "random", "", 12345, []string{"random_key(i.id, 12345)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOrder(tc.sort, tc.order, tc.seed)
			for _, s := range tc.want {
				wantIn(t, got, s)
			}
		})
	}
}

func TestDefaultOrder(t *testing.T) {
	for sort, want := range map[string]string{
		"order": "asc", "newest": "desc", "filesize": "desc", "random": "desc", "": "desc",
	} {
		if got := DefaultOrder(sort); got != want {
			t.Errorf("DefaultOrder(%q) = %q, want %q", sort, got, want)
		}
	}
}

func TestBuildWhere_BoolColumns(t *testing.T) {
	for _, tc := range []struct {
		key, val, want string
	}{
		{"fav", "false", "is_favorited = 0"},
		{"inbox", "true", "is_inbox = 1"},
		{"inbox", "false", "is_inbox = 0"},
	} {
		t.Run(tc.key+":"+tc.val, func(t *testing.T) {
			where, args, _ := buildWhere(FilterExpr{Key: tc.key, Val: tc.val})
			if len(args) != 0 {
				t.Errorf("expected no args for %s:%s", tc.key, tc.val)
			}
			wantIn(t, where, tc.want)
		})
	}
}

func TestBuildWhere_BooleanAliases(t *testing.T) {
	for _, key := range []string{"fav", "inbox", "missing", "tagged", "autotagged"} {
		for _, side := range []struct {
			name    string
			aliases []string
		}{
			{"affirmative", []string{"true", "yes", "y", "on", "1", "YES", "On"}},
			{"negative", []string{"false", "no", "n", "off", "0", "NO", "Off"}},
		} {
			t.Run(key+"/"+side.name, func(t *testing.T) {
				ref, refArgs, _ := buildWhere(FilterExpr{Key: key, Val: side.aliases[0]})
				for _, alias := range side.aliases {
					got, args, _ := buildWhere(FilterExpr{Key: key, Val: alias})
					if got != ref {
						t.Errorf("%s:%s SQL = %q, want %q", key, alias, got, ref)
					}
					if len(args) != len(refArgs) {
						t.Errorf("%s:%s args shape = %v, want %v", key, alias, args, refArgs)
					}
				}
			})
		}

		if key == "tagged" || key == "autotagged" {
			continue
		}
		t.Run(key+"/garbage_returns_1=0", func(t *testing.T) {
			got, _, _ := buildWhere(FilterExpr{Key: key, Val: "maybe"})
			if !strings.Contains(got, "1=0") {
				t.Errorf("%s:maybe SQL = %q, want a 1=0 branch instead of a silent flip to false", key, got)
			}
		})
	}
}

func TestBuildWhere_AI(t *testing.T) {
	expr := FilterExpr{Key: "ai", Val: "sd"}
	where, args, _ := buildWhere(expr)
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %d: %v", len(args), args)
	}
	if len(args) > 0 && args[0] != "a1111" {
		t.Errorf("args[0] = %v, want a1111", args[0])
	}
	wantIn(t, where, "source_type")
}

func TestBuildWhere_AIListIsAUnion(t *testing.T) {
	where, args, _ := buildWhere(FilterExpr{Key: "ai", Val: "sd, comfyui,a1111"})
	if len(args) != 8 || args[0] != "a1111" || args[4] != "comfyui" {
		t.Errorf("args = %v, want the a1111 then the comfyui clause's four, sd folded into a1111", args)
	}
	wantIn(t, where, ") OR (")
}

func TestExecute_AIListMatchesEitherTool(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, st := range []struct{ name, sourceType string }{
		{"ail_a.png", "a1111"},
		{"ail_b.png", "comfyui"},
		{"ail_ab.png", "a1111,comfyui"},
		{"ail_none.png", "none"},
	} {
		ingestTestImage(t, database, env, st.name)
		mustExec(t, database.Write, `UPDATE images SET source_type = ? WHERE canonical_path LIKE '%' || ? || '%'`, st.sourceType, st.name)
	}
	for _, tc := range []struct {
		val  string
		want int
	}{{"a1111,comfyui", 3}, {"sd,none", 3}, {"comfyui,none", 3}, {"any,none", 4}} {
		expr := FilterExpr{Key: "ai", Val: tc.val}
		res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
		where, args, _ := buildWhere(expr)
		var slow int
		if err := database.Read.QueryRow(`SELECT COUNT(*) FROM images i WHERE i.is_missing = 0 AND `+where, args...).Scan(&slow); err != nil {
			t.Fatal(err)
		}
		fast, ok := fastCountAI(testSource(database), expr)
		if res.Total != tc.want || slow != tc.want || !ok || fast != tc.want {
			t.Errorf("ai:%s total = %d, slow = %d, fast = %d (ok %v), want %d", tc.val, res.Total, slow, fast, ok, tc.want)
		}
	}
}

func TestAIFilter_UnderscoreIsLiteral(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ai_both.png")
	mustExec(t, database.Write, `UPDATE images SET source_type = 'a1111,comfyui'`)
	for _, val := range []string{"a_111", "%"} {
		res := mustExecute(t, database, Query{Expr: FilterExpr{Key: "ai", Val: val}, Sort: "newest", Page: 1, Limit: 40})
		if res.Total != 0 {
			t.Errorf("ai:%s Total = %d, want 0", val, res.Total)
		}
	}
}

func TestExecute_SourceFilterMatchesAnyOrigin(t *testing.T) {
	database, env := setupSearchDB(t)
	hit := ingestTestImageWithID(t, database, env, "src_hit.png")
	ingestTestImage(t, database, env, "src_miss.png")

	mustExec(t, database.Write, `INSERT INTO image_sources (image_id, site, post_id) VALUES (?, 'gelbooru', '1'), (?, 'Pixiv', '2')`, hit, hit)
	expr := Parse("source:pixiv")
	res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 1 || len(res.Results) != 1 || res.Results[0].ID != hit {
		t.Fatalf("source:pixiv = %d matches %+v, want just image %d", res.Total, res.Results, hit)
	}

	for _, tc := range []struct {
		q    string
		want int64
	}{{"source:none", 0}, {"source:any", hit}} {
		e := Parse(tc.q)
		r := mustExecute(t, database, Query{Expr: e, Sort: "newest", Page: 1, Limit: 40})
		if r.Total != 1 || len(r.Results) != 1 || (tc.want != 0 && r.Results[0].ID != tc.want) {
			t.Errorf("%s = %d matches %+v, want exactly one", tc.q, r.Total, r.Results)
		}
		if tc.want == 0 && r.Results[0].ID == hit {
			t.Errorf("source:none returned the sourced image %d", hit)
		}
	}
}

func TestBuildWhere_SourceEmpty(t *testing.T) {
	for _, val := range []string{"", "none", "NONE"} {
		where, args, _ := buildWhere(FilterExpr{Key: "source", Val: val})
		if len(args) != 0 {
			t.Errorf("source:%q args = %v, want none", val, args)
		}
		if !strings.Contains(where, "i.id NOT IN (SELECT image_id FROM image_sources)") {
			t.Errorf("source:%q where = %q, expected a bare non-membership test", val, where)
		}
	}
	where, args, _ := buildWhere(FilterExpr{Key: "source", Val: "any"})
	if len(args) != 0 {
		t.Errorf("source:any args = %v, want none", args)
	}
	wantIn(t, where, "i.id IN (SELECT image_id FROM image_sources)")
}

func TestColumnFilterIndexHint_NegativeMembership(t *testing.T) {
	sortIdx := " INDEXED BY idx_images_ingested_visible"
	for _, tc := range []struct {
		key, val string
		want     string
	}{
		{"source", "none", sortIdx},
		{"source", "NONE", sortIdx},
		{"source", "any", ""},
		{"source", "gelbooru", ""},
		{"upgrade", "none", sortIdx},
		{"upgrade", "any", ""},
		{"lookup", "never", sortIdx},
		{"lookup", "off", ""},
		{"lookup", "due", ""},
	} {
		got := columnFilterIndexHint(FilterExpr{Key: tc.key, Val: tc.val}, "newest")
		if got != tc.want {
			t.Errorf("%s:%s hint = %q, want %q", tc.key, tc.val, got, tc.want)
		}
	}
	if got := columnFilterIndexHint(FilterExpr{Key: "source", Val: "none"}, "filesize"); got != " INDEXED BY idx_images_filesize_visible" {
		t.Errorf("source:none under filesize sort = %q", got)
	}
}

func TestColumnFilterIndexHint_NegatedWorkflowTerm(t *testing.T) {
	for _, tc := range []struct {
		expr Expr
		sort string
		want string
	}{
		{NotExpr{Expr: FilterExpr{Key: "comfyui", Val: "KSampler"}}, "newest", " INDEXED BY idx_images_ingested_visible"},
		{NotExpr{Expr: FilterExpr{Key: "comfyui", Val: "KSampler"}}, "filesize", " INDEXED BY idx_images_filesize_visible"},
		{FilterExpr{Key: "comfyui", Val: "KSampler"}, "newest", ""},
		{NotExpr{Expr: FilterExpr{Key: "comfyui", Val: ""}}, "newest", ""},
		{NotExpr{Expr: FilterExpr{Key: "source", Val: "none"}}, "newest", ""},
	} {
		if got := columnFilterIndexHint(tc.expr, tc.sort); got != tc.want {
			t.Errorf("%#v under %s: hint = %q, want %q", tc.expr, tc.sort, got, tc.want)
		}
	}
}

func TestExecute_CategoryQualifiedUnicode(t *testing.T) {
	database, env := setupSearchDB(t)
	img := ingestTestImageWithID(t, database, env, "uni.png")
	tagID := insertTag(t, database, "café_noir", categoryID(t, database, "character"), 0)
	attachTag(t, database, img, tagID)

	for _, q := range []string{"character:café_noir", "character:Café_Noir"} {
		expr := Parse(q)
		result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
		if result.Total != 1 {
			t.Errorf("query %q: Total = %d, want 1", q, result.Total)
		} else if result.Results[0].ID != img {
			t.Errorf("query %q: matched %d, want %d", q, result.Results[0].ID, img)
		}
	}
}

func TestBuildWhere_Stale(t *testing.T) {
	for _, val := range []string{"any", ""} {
		where, args, _ := buildWhere(FilterExpr{Key: "stale", Val: val})
		if len(args) != 0 || !strings.Contains(where, "EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.stale = 1)") {
			t.Errorf("stale:%q where=%q args=%v", val, where, args)
		}
	}
	if where, _, _ := buildWhere(FilterExpr{Key: "stale", Val: "none"}); !strings.Contains(where, "NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.stale = 1)") {
		t.Errorf("stale:none where=%q", where)
	}
	where, args, _ := buildWhere(FilterExpr{Key: "stale", Val: "Blue Eyes"})
	if len(args) != 1 || args[0] != "blue_eyes" {
		t.Errorf("stale:<tag> args=%v, want [blue_eyes]", args)
	}
	wantIn(t, where, "it.stale = 1 AND it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)")
}

func TestExecute_Stale(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "s1.png")
	ingestTestImage(t, database, env, "s2.png")
	var img1, img2 int64
	if err := database.Read.QueryRow(`SELECT MIN(id), MAX(id) FROM images`).Scan(&img1, &img2); err != nil {
		t.Fatal(err)
	}
	tagID := getOrCreateTagID(t, database, "blue_eyes")
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, stale) VALUES (?, ?, 0, 1)`, img1, tagID)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto, stale) VALUES (?, ?, 0, 0)`, img2, tagID)
	check := func(q string, want int64) {
		expr := Parse(q)
		res := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
		if res.Total != 1 || res.Results[0].ID != want {
			t.Errorf("query %q: Total=%d, want image %d", q, res.Total, want)
		}
	}
	check("stale:any", img1)
	check("stale:blue_eyes", img1)
	check("stale:none", img2)
	check("-stale:blue_eyes", img2)
}

func TestBuildWhere_TaggedSource(t *testing.T) {
	for _, val := range []string{"", "true"} {
		where, args, _ := buildWhere(FilterExpr{Key: "tagged", Val: val})
		if len(args) != 0 || !strings.Contains(where, "EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.tagger_name IS NOT 'monbooru')") {
			t.Errorf("tagged:%q where=%q args=%v", val, where, args)
		}
	}
	if where, _, _ := buildWhere(FilterExpr{Key: "autotagged", Val: ""}); !strings.Contains(where, "it.image_id = i.id AND it.is_auto = 1") {
		t.Errorf("autotagged: where=%q", where)
	}
	where, args, _ := buildWhere(FilterExpr{Key: "tagged", Val: "PTR"})
	if len(args) != 1 || args[0] != "PTR" {
		t.Errorf("tagged:<source> args=%v, want [PTR]", args)
	}
	wantIn(t, where, "FROM image_tag_sources s WHERE s.image_id = i.id AND s.source = ? COLLATE NOCASE")
	where, _, _ = buildWhere(FilterExpr{Key: "autotagged", Val: "wd14"})
	wantIn(t, where, "JOIN image_tags it ON it.image_id = s.image_id AND it.tag_id = s.tag_id")
	wantIn(t, where, "it.is_auto = 1 AND s.source = ? COLLATE NOCASE")
}

func TestExecute_TaggedSource(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"t1.png", "t2.png", "t3.png", "t4.png"} {
		ingestTestImage(t, database, env, name)
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	implied, anon, shared, auto := ids[0], ids[1], ids[2], ids[3]

	svc := tags.New(database, nil)
	parent := getOrCreateTagID(t, database, "parent_tag")
	child := getOrCreateTagID(t, database, "child_tag")
	if _, err := svc.AddImplication(parent, child); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTagToImageReportingDup(implied, parent, false, nil, "danbooru"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTagToImageReportingDup(anon, parent, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"danbooru", "ptr"} {
		if _, err := svc.AddTagToImageReportingDup(shared, child, false, nil, src); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddTagsToImageFromTagger(auto, []int64{child}, true, "wd14"); err != nil {
		t.Fatal(err)
	}

	match := func(q string, want ...int64) { checkSearchSet(t, database, q, want...) }

	match("tagged:user", anon)
	match("tagged:USER", anon)
	match("tagged:danbooru", implied, shared)
	match("tagged:ptr", shared)
	match("tagged:PTR", shared)
	match("tagged:wd14", auto)
	match("tagged:nobody")

	match("autotagged:wd14", auto)
	match("autotagged:danbooru")
	match("autotagged:user")
	match("tagged:", implied, anon, shared, auto)
	match("autotagged:", auto)
}

func TestBuildWhere_ComparisonTolerable(t *testing.T) {
	for _, tc := range []struct {
		key, val string
		want     any
	}{
		{"width", ">= 1920", int64(1920)},
		{"ratio", "> 1.5", 1.5},
		{"size", ">= 10MB", int64(10 * 1024 * 1024)},
	} {
		where, args, _ := buildWhere(FilterExpr{Key: tc.key, Val: tc.val})
		if len(args) != 1 || args[0] != tc.want {
			t.Errorf("%s:%q args = %v, want [%v]", tc.key, tc.val, args, tc.want)
		}
		if strings.Contains(where, "1=0") {
			t.Errorf("%s:%q where = %q", tc.key, tc.val, where)
		}
	}
}

func TestBuildWhere_MissingFalse(t *testing.T) {
	expr := FilterExpr{Key: "missing", Val: "false"}
	where, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingFilter = true for missing:false")
	}
	wantIn(t, where, "is_missing = 0")
}

func TestBuildWhere_RejectedValues(t *testing.T) {
	for _, expr := range []FilterExpr{
		{Key: "name", Val: ""},
		{Key: "size", Val: ">=10ZB"},
		{Key: "ratio", Val: ">=abc"},
		{Key: "seed", Val: "notanumber"},
	} {
		t.Run(expr.Key, func(t *testing.T) {
			where, _, _ := buildWhere(expr)
			if !strings.Contains(where, "1=0") {
				t.Errorf("%s:%q where = %q, want 1=0", expr.Key, expr.Val, where)
			}
		})
	}
}

func TestBuildWhere_Size(t *testing.T) {
	cases := []struct {
		in     string
		wantOp string
		wantN  int64
	}{
		{">=10MB", ">=", 10 * 1024 * 1024},
		{"<2gb", "<", 2 * 1024 * 1024 * 1024},
		{"512", "=", 512},
		{">100kb", ">", 100 * 1024},
		{">9000000TB", ">", math.MaxInt64},
		{">99999999999999999999", ">", math.MaxInt64},
		{"<-9000000TB", "<", math.MinInt64},
	}
	for _, tc := range cases {
		expr := FilterExpr{Key: "size", Val: tc.in}
		where, args, _ := buildWhere(expr)
		if !strings.Contains(where, "i.file_size") {
			t.Errorf("size %q: where = %q", tc.in, where)
		}
		if !strings.Contains(where, tc.wantOp) {
			t.Errorf("size %q: op missing in %q", tc.in, where)
		}
		if len(args) != 1 || args[0] != tc.wantN {
			t.Errorf("size %q: args = %v, want [%d]", tc.in, args, tc.wantN)
		}
	}
}

func TestBuildWhere_SizeUnitless(t *testing.T) {
	expr := FilterExpr{Key: "size", Val: ">=1024"}
	_, args, _ := buildWhere(expr)
	if args[0] != int64(1024) {
		t.Errorf("args[0] = %v, want 1024 bytes", args[0])
	}
}

func TestBuildWhere_Mime(t *testing.T) {
	cases := []struct {
		in    string
		wants []string
	}{
		{"png", []string{"'png'"}},
		{"image/jpeg", []string{"'jpeg'"}},
		{"png,jpeg", []string{"'jpeg'", "'png'"}},
		{"image/png,image/jpeg", []string{"'jpeg'", "'png'"}},
		{"image/png, video/mp4", []string{"'mp4'", "'png'"}},
		{"avif,jxl", []string{"'avif'", "'jxl'"}},
		{"image/jxl", []string{"'jxl'"}},
		{"bogus", []string{"1=0"}},
	}
	for _, tc := range cases {
		expr := FilterExpr{Key: "mime", Val: tc.in}
		where, _, _ := buildWhere(expr)
		for _, want := range tc.wants {
			if !strings.Contains(where, want) {
				t.Errorf("mime %q: where missing %q (got %q)", tc.in, want, where)
			}
		}
	}
}

func TestBuildWhere_Hash(t *testing.T) {
	sha := strings.Repeat("AB", 32)
	sum := strings.Repeat("CD", 16)

	for _, c := range []struct{ val, col, arg string }{
		{sha, "i.sha256 = ?", strings.ToLower(sha)},
		{sum, "i.md5 = ?", strings.ToLower(sum)},
	} {
		where, args, _ := buildWhere(FilterExpr{Key: "hash", Val: c.val})
		if !strings.Contains(where, c.col) {
			t.Errorf("hash:%s where = %q, want %q", c.val, where, c.col)
		}
		if len(args) != 1 || args[0] != c.arg {
			t.Errorf("hash:%s args = %v, want the lowercase digest", c.val, args)
		}
	}
	for _, val := range []string{"", "abcdef01", strings.Repeat("z", 32)} {
		where, args, _ := buildWhere(FilterExpr{Key: "hash", Val: val})
		if !strings.Contains(where, "1=0") {
			t.Errorf("hash:%q where = %q, want 1=0", val, where)
		}
		if len(args) != 0 {
			t.Errorf("hash:%q args = %v, want none bound", val, args)
		}
	}
}

func TestBuildWhere_MD5(t *testing.T) {
	sum := strings.Repeat("AB", 16)
	where, args, _ := buildWhere(FilterExpr{Key: "md5", Val: sum})
	wantIn(t, where, "i.md5 = ?")
	wantIn(t, where, "i.md5 != ''")
	if len(args) != 1 || args[0] != strings.ToLower(sum) {
		t.Errorf("args = %v, want the lowercase digest", args)
	}
	if where, _, _ := buildWhere(FilterExpr{Key: "md5", Val: ""}); !strings.Contains(where, "i.md5 != ''") {
		t.Errorf("bare md5: where = %q, want the has-one test", where)
	}
	for _, val := range []string{"abcd", strings.Repeat("z", 32), strings.Repeat("ab", 32)} {
		if where, _, _ := buildWhere(FilterExpr{Key: "md5", Val: val}); !strings.Contains(where, "1=0") {
			t.Errorf("md5:%q where = %q, want 1=0", val, where)
		}
	}
}

func TestBuildWhere_Prompt(t *testing.T) {
	expr := FilterExpr{Key: "prompt", Val: "1girl"}
	where, args, _ := buildWhere(expr)
	wantIn(t, where, "FROM sd_metadata WHERE prompt LIKE ?")
	wantIn(t, where, "FROM comfyui_metadata WHERE prompt LIKE ?")
	wantNotIn(t, where, "image_id = i.id")
	if len(args) != 2 {
		t.Errorf("args len = %d, want 2 (one per table)", len(args))
	}
}

func TestBuildWhere_Seed(t *testing.T) {
	expr := FilterExpr{Key: "seed", Val: "12345"}
	where, args, _ := buildWhere(expr)
	wantIn(t, where, "FROM sd_metadata WHERE seed = ?")
	wantIn(t, where, "FROM comfyui_metadata WHERE seed = ?")
	if len(args) != 2 || args[0] != int64(12345) {
		t.Errorf("args = %v, want both seed slots set to 12345", args)
	}
}

func TestBuildWhere_UnknownFilter(t *testing.T) {
	expr := FilterExpr{Key: "bogus", Val: "val"}
	where, args, _ := buildWhere(expr)
	wantIn(t, where, "EXISTS")
	if len(args) != 2 {
		t.Errorf("expected 2 args (tag name + category name), got %d: %v", len(args), args)
	}

	expr2 := FilterExpr{Key: "bogus", Val: ""}
	where2, args2, _ := buildWhere(expr2)
	wantIn(t, where2, "tc.name = ?")
	if len(args2) != 1 || args2[0] != "bogus" {
		t.Errorf("expected single-arg category-name, got %v", args2)
	}
}

func TestBuildWhereDB_ColonFallsBackToLiteral(t *testing.T) {
	database, _ := setupSearchDB(t)

	expr := FilterExpr{Key: "nier", Val: "automata"}
	where, args, _ := buildWhereDB(expr, testSource(database))
	wantNotIn(t, where, "tc.name")
	if len(args) != 1 || args[0] != "nier:automata" {
		t.Errorf("args = %v, want [nier:automata]", args)
	}
}

func TestBuildWhereDB_ColonUsesCategoryWhenPrefixExists(t *testing.T) {
	database, _ := setupSearchDB(t)
	artistID := categoryID(t, database, "artist")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id) VALUES ('foo', ?)`, artistID)
	expr := FilterExpr{Key: "artist", Val: "foo"}
	where, _, _ := buildWhereDB(expr, testSource(database))
	wantIn(t, where, "EXISTS (SELECT 1 FROM image_tags it")
	wantIn(t, where, "it.tag_id IN (")
}

func TestBuildWhereDB_SpeciesIsBuiltinCategory(t *testing.T) {
	database, _ := setupSearchDB(t)
	speciesID := categoryID(t, database, "species")
	mustExec(t, database.Write, `INSERT INTO tags (name, category_id) VALUES ('fox', ?)`, speciesID)
	expr := FilterExpr{Key: "species", Val: "fox"}
	where, _, _ := buildWhereDB(expr, testSource(database))
	wantIn(t, where, "EXISTS (SELECT 1 FROM image_tags it")
}

func withDisplayZone(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func TestBuildDateFilter(t *testing.T) {
	withDisplayZone(t, time.UTC)
	for _, tc := range []struct {
		name, in, op, arg string
	}{
		{"After", ">2024-01-01", "> ?", "2024-01-01T23:59:59Z"},
		{"Before", "<2024-12-31", "< ?", "2024-12-31T00:00:00Z"},
		{"AfterOrEqual", ">=2024-01-01", ">= ?", "2024-01-01T00:00:00Z"},
		{"BeforeOrEqual", "<=2024-12-31", "<= ?", "2024-12-31T23:59:59Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &whereBuilder{}
			clause := b.buildDateFilter(tc.in)
			if !strings.Contains(clause, tc.op) || b.args[0] != tc.arg {
				t.Errorf("clause = %q, args = %v", clause, b.args)
			}
		})
	}
}

func TestBuildDateFilter_DisplayTimezone(t *testing.T) {
	withDisplayZone(t, time.FixedZone("UTC+2", 2*3600))

	b := &whereBuilder{}
	b.buildDateFilter("2026-05-22")
	if b.args[0] != "2026-05-21T22:00:00Z" || b.args[1] != "2026-05-22T21:59:59Z" {
		t.Errorf("local day window = %v", b.args)
	}

	b = &whereBuilder{}
	b.buildDateFilter("2026-05-22T06")
	if b.args[0] != "2026-05-22T04:00:00Z" || b.args[1] != "2026-05-22T04:59:59Z" {
		t.Errorf("local hour window = %v", b.args)
	}

	b = &whereBuilder{}
	b.buildDateFilter(">=2026-05-22T19:23")
	if b.args[0] != "2026-05-22T17:23:00Z" {
		t.Errorf("local minute lower bound = %v", b.args)
	}
}

func TestBuildDateFilter_Range(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("2024-01-01..2024-12-31")
	wantIn(t, clause, "BETWEEN")
	if len(b.args) != 2 {
		t.Errorf("expected 2 args, got %d", len(b.args))
	}
}

func TestBuildDateFilter_Exact(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("2024-06-15")
	wantIn(t, clause, "BETWEEN")
}

func TestParseCompOp(t *testing.T) {
	for _, tc := range []struct {
		name, in, op, val string
	}{
		{"GTE", ">=1920", ">=", "1920"},
		{"LTE", "<=768", "<=", "768"},
		{"GT", ">100", ">", "100"},
		{"LT", "<200", "<", "200"},
		{"EQ", "=1024", "=", "1024"},
		{"Default", "512", "=", "512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if op, val := parseCompOp(tc.in); op != tc.op || val != tc.val {
				t.Errorf("op=%q val=%q", op, val)
			}
		})
	}
}

func TestSidebarTagsWithGlobalCount_Empty(t *testing.T) {
	database, _ := setupSearchDB(t)
	tags, err := SidebarTagsWithGlobalCount(testSource(database), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tags != nil {
		t.Error("expected nil for empty image IDs")
	}
}

func TestSidebarTagsWithGlobalCount_WithImages(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	tagID := insertTag(t, database, "shared", categoryID(t, database, "general"), 2)

	result := mustExecute(t, database, Query{Page: 1, Limit: 40})
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 images, got %d", len(result.Results))
	}
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img.ID, tagID)
	}

	got, err := SidebarTagsWithGlobalCount(testSource(database), ids)
	if err != nil {
		t.Fatal(err)
	}
	var found *int
	for i, tag := range got {
		if tag.Name == "shared" {
			found = &i
			break
		}
	}
	if found == nil {
		t.Fatalf("expected tag 'shared' in sidebar aggregator, got %+v", got)
	}
	if got[*found].UsageCount != 2 {
		t.Errorf("shared tag usage = %d, want 2", got[*found].UsageCount)
	}
}

func TestExecute_SortFilesize(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "s1.png")
	ingestTestImage(t, database, cfg, "s2.png")

	result := mustExecute(t, database, Query{Sort: "filesize", Page: 1, Limit: 40})
	if result.Total < 2 {
		t.Errorf("Total = %d, want >= 2", result.Total)
	}
}

func TestExecute_SortRandom(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "r1.png")

	mustExecute(t, database, Query{Sort: "random", Page: 1, Limit: 40})
}

func TestExecute_SortOrder_GroupsBySeriesThenOrder(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "a1.png")
	ingestTestImage(t, database, cfg, "a2.png")
	ingestTestImage(t, database, cfg, "a3.png")
	ingestTestImage(t, database, cfg, "b1.png")
	ingestTestImage(t, database, cfg, "none.png")
	imageID := func(name string) int64 { return imageIDByName(t, database, name) }
	a1, a2, a3, b1, none := imageID("a1.png"), imageID("a2.png"), imageID("a3.png"), imageID("b1.png"), imageID("none.png")
	for _, set := range []struct {
		id    int64
		ser   string
		order interface{}
	}{
		{a1, "A", 2},
		{a2, "A", 1},
		{a3, "A", nil},
		{b1, "B", 1},
	} {
		mustExec(t, database.Write, `UPDATE images SET series=?, series_order=? WHERE id=?`, set.ser, set.order, set.id)
	}

	result := mustExecute(t, database, Query{Sort: "order", Order: "asc", Page: 1, Limit: 40})
	if len(result.Results) != 5 {
		t.Fatalf("len(results) = %d, want 5", len(result.Results))
	}
	gotOrder := resultIDs(result)
	want := []int64{none, a2, a1, a3, b1}
	for i, id := range want {
		if gotOrder[i] != id {
			t.Errorf("position %d: id=%d, want %d (full order: %v)", i, gotOrder[i], id, gotOrder)
		}
	}
}

func TestExecute_FavFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "fav1.png")

	expr := FilterExpr{Key: "fav", Val: "true"}
	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if result.Total != 0 {
		t.Errorf("expected 0 favorited images, got %d", result.Total)
	}
}

func TestExecute_InboxFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "inbox1.png")

	resTrue := mustExecute(t, database, Query{Expr: FilterExpr{Key: "inbox", Val: "true"}, Page: 1, Limit: 40})
	if resTrue.Total != 1 {
		t.Errorf("inbox:true Total = %d, want 1", resTrue.Total)
	}
	if len(resTrue.Results) != 1 || !resTrue.Results[0].IsInbox {
		t.Errorf("inbox:true result didn't surface IsInbox=true: %+v", resTrue.Results)
	}

	resFalse := mustExecute(t, database, Query{Expr: FilterExpr{Key: "inbox", Val: "false"}, Page: 1, Limit: 40})
	if resFalse.Total != 0 {
		t.Errorf("inbox:false Total = %d, want 0", resFalse.Total)
	}
}

func TestExecute_TagSearch(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "tagged.png")

	expr := TagExpr{Tag: "nonexistent_tag_xyz"}
	result := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	if result.Total != 0 {
		t.Errorf("expected 0 results for nonexistent tag, got %d", result.Total)
	}
}

func TestExecute_SkipCount(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "sc1.png")
	ingestTestImage(t, database, cfg, "sc2.png")

	result := mustExecute(t, database, Query{Page: 1, Limit: 40, SkipCount: true})
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0 (skip-count)", result.Total)
	}
	if len(result.Results) != 2 {
		t.Errorf("Results = %d, want 2", len(result.Results))
	}
}

func TestExecute_DeferCountLeavesOnlyAFullPageUnknown(t *testing.T) {
	database, cfg := setupSearchDB(t)
	for i := range 5 {
		ingestTestImage(t, database, cfg, "dc"+strconv.Itoa(i)+".png")
	}

	q := Query{Expr: Parse("width:>0"), Page: 1, Limit: 2, DeferCount: true}

	full := mustExecute(t, database, q)
	if !full.TotalUnknown || len(full.Results) != 2 {
		t.Fatalf("full page: unknown=%v rows=%d, want unknown with 2 rows", full.TotalUnknown, len(full.Results))
	}
	if n, err := Count(context.Background(), testSource(database), q); err != nil || n != 5 {
		t.Errorf("Count = %d, %v; want 5", n, err)
	}

	q.Page = 3
	if short := mustExecute(t, database, q); short.TotalUnknown || short.Total != 5 {
		t.Errorf("short last page: unknown=%v total=%d, want known 5", short.TotalUnknown, short.Total)
	}
	q.Page = 9
	if past := mustExecute(t, database, q); past.TotalUnknown || past.Total != 5 {
		t.Errorf("page past the end: unknown=%v total=%d, want known 5", past.TotalUnknown, past.Total)
	}
	q.Page, q.Expr = 1, Parse("width:<0")
	if none := mustExecute(t, database, q); none.TotalUnknown || none.Total != 0 {
		t.Errorf("no match: unknown=%v total=%d, want known 0", none.TotalUnknown, none.Total)
	}
}

func TestExecute_DeferCountKeepsAFastCount(t *testing.T) {
	database, env := setupSearchDB(t)
	tag := getOrCreateTagID(t, database, "deferred_fast")
	for i := range 3 {
		attachTag(t, database, ingestTestImageWithID(t, database, env, "dfc"+strconv.Itoa(i)+".png"), tag)
	}
	res := mustExecute(t, database, Query{Page: 1, Limit: 2, DeferCount: true, Expr: Parse("deferred_fast")})
	if res.TotalUnknown || res.Total != 3 {
		t.Errorf("unknown=%v total=%d, want the fast count 3", res.TotalUnknown, res.Total)
	}
}

func pageIDs(res *models.SearchResult) []int64 {
	ids := make([]int64, len(res.Results))
	for i, img := range res.Results {
		ids[i] = img.ID
	}
	return ids
}

func TestExecute_ACursorPageHoldsStillWhileImagesLandOnTop(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := range 5 {
		ingestTestImage(t, database, env, "cur"+strconv.Itoa(i)+".png")
	}
	q := Query{Sort: "newest", Order: "desc", Page: 1, Limit: 2, Expr: Parse("width:>0")}
	page1 := pageIDs(mustExecute(t, database, q))
	q.Page = 2
	page2 := pageIDs(mustExecute(t, database, q))

	ingestTestImage(t, database, env, "cur-new.png")
	q.After = page1[1]
	if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, page2) {
		t.Errorf("after=%d: %v, want the page as it was drawn %v", q.After, got, page2)
	}
	q.After, q.Before, q.Page = 0, page2[0], 1
	if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, page1) {
		t.Errorf("before=%d: %v, want %v in view order", q.Before, got, page1)
	}
}

func TestExecute_ADeletedCursorFallsBackToTheOffset(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := range 5 {
		ingestTestImage(t, database, env, "curdel"+strconv.Itoa(i)+".png")
	}
	q := Query{Sort: "filesize", Order: "desc", Page: 2, Limit: 2, Expr: Parse("width:>0")}
	want := pageIDs(mustExecute(t, database, q))
	q.After = 999999
	if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, want) {
		t.Errorf("after a missing id: %v, want the offset page %v", got, want)
	}
}

func TestExecute_ACachedListPagesFromTheCursor(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := range 5 {
		ingestTestImage(t, database, env, "curcache"+strconv.Itoa(i)+".png")
	}
	src := testSource(database)
	q := Query{Sort: "newest", Order: "desc", Page: 2, Limit: 2, Cached: true, Text: "cursor-cache"}
	list := pageIDs(mustExecute(t, database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 5}))
	src.Cache.Seed(q, list)

	q.After = list[2]
	if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, list[3:5]) {
		t.Errorf("after=%d off the cached list: %v, want %v", q.After, got, list[3:5])
	}
	q.After, q.Before = 0, list[3]
	if got := pageIDs(mustExecute(t, database, q)); !slices.Equal(got, list[1:3]) {
		t.Errorf("before=%d off the cached list: %v, want %v", q.Before, got, list[1:3])
	}
}

func TestExecute_Page1FansAfterTheAnswer(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := range 5 {
		ingestTestImage(t, database, env, "bgfan"+strconv.Itoa(i)+".png")
	}
	src := testSource(database)
	q := Query{Sort: "newest", Order: "desc", Page: 1, Limit: 2, Cached: true, Text: "bg-fan", Expr: Parse("width:>0"), DeferCount: true}
	if res := mustExecute(t, database, q); !res.TotalUnknown {
		t.Errorf("page 1 total = %d, want it left to the count: the fan no longer runs in the request", res.Total)
	}
	waitForFan(t, src.Cache, q.listKey(Source{}))
	if ids, ok := src.Cache.get(q.listKey(Source{})); !ok || len(ids) != 5 {
		t.Fatalf("cached ids = %v (ok=%v), want all 5", ids, ok)
	}
	if res := mustExecute(t, database, q); res.TotalUnknown || res.Total != 5 {
		t.Errorf("the next visit: unknown=%v total=%d, want the cached 5", res.TotalUnknown, res.Total)
	}
}

func TestExecute_AnUnknownTotalPastTheCapIsNotCached(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := range 5 {
		ingestTestImage(t, database, env, "capfan"+strconv.Itoa(i)+".png")
	}
	old := adjacencyCacheMaxIDs
	adjacencyCacheMaxIDs = 3
	t.Cleanup(func() { adjacencyCacheMaxIDs = old })
	src := testSource(database)
	q := Query{Sort: "newest", Order: "desc", Page: 1, Limit: 2, Cached: true, Text: "cap-fan", Expr: Parse("width:>0"), DeferCount: true}
	if res := mustExecute(t, database, q); !res.TotalUnknown {
		t.Fatalf("page 1 total = %d, want it left to the count", res.Total)
	}
	key := q.listKey(Source{})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		src.Cache.fanMu.Lock()
		_, heldOff := src.Cache.fanOverBudget[key]
		src.Cache.fanMu.Unlock()
		if heldOff {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fan never refused the list past the cap")
		}
	}
	if ids, ok := src.Cache.get(key); ok {
		t.Errorf("cached %v as the whole match set of 5", ids)
	}
}

func TestExecute_DefaultPagination(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "p1.png")

	result := mustExecute(t, database, Query{Page: 0, Limit: 0})
	if result.Page != 1 {
		t.Errorf("default page = %d, want 1", result.Page)
	}
	if result.Limit != 40 {
		t.Errorf("default limit = %d, want 40", result.Limit)
	}
}

func assertAdjacencyChain(t *testing.T, database *db.DB, q Query, ids []int64) {
	t.Helper()
	show := func(p *int64) string {
		if p == nil {
			return "nil"
		}
		return strconv.FormatInt(*p, 10)
	}
	for i, id := range ids {
		prev, next := mustExecuteAdjacent(t, context.Background(), database, q, id)
		wantPrev, wantNext := "nil", "nil"
		if i > 0 {
			wantPrev = strconv.FormatInt(ids[i-1], 10)
		}
		if i < len(ids)-1 {
			wantNext = strconv.FormatInt(ids[i+1], 10)
		}
		if show(prev) != wantPrev || show(next) != wantNext {
			t.Errorf("%s %s pos %d id %d: prev=%s next=%s, want %s %s (chain %v)", q.Sort, q.Order, i, id, show(prev), show(next), wantPrev, wantNext, ids)
		}
	}
}

func TestExecuteAdjacent_Newest(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "adj_a.png")
	ingestTestImage(t, database, cfg, "adj_b.png")
	ingestTestImage(t, database, cfg, "adj_c.png")

	result, err := Execute(context.Background(), testSource(database), Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	assertAdjacencyChain(t, database, Query{Sort: "newest", Order: "desc"}, resultIDs(result))
}

func TestExecuteAdjacent_Order(t *testing.T) {
	database, cfg := setupSearchDB(t)
	for _, n := range []string{"o_a1.png", "o_a2.png", "o_a3.png", "o_b1.png", "o_none.png"} {
		ingestTestImage(t, database, cfg, n)
	}
	for _, set := range []struct {
		name  string
		ser   string
		order interface{}
	}{
		{"o_a1.png", "A", 2},
		{"o_a2.png", "A", 1},
		{"o_a3.png", "A", nil},
		{"o_b1.png", "B", 1},
	} {
		mustExec(t, database.Write, `UPDATE images SET series=?, series_order=? WHERE canonical_path LIKE '%' || ?`, set.ser, set.order, set.name)
	}

	for _, order := range []string{"asc", "desc"} {
		q := Query{Sort: "order", Order: order, Page: 1, Limit: 40}
		result, err := Execute(context.Background(), testSource(database), q)
		if err != nil || len(result.Results) != 5 {
			t.Fatalf("[%s] setup Execute: err=%v len=%d", order, err, len(result.Results))
		}
		ids := resultIDs(result)
		assertAdjacencyChain(t, database, q, ids)
		for i := range ids {
			rank, err := RankInQuery(context.Background(), testSource(database), q, ids[i])
			if err != nil {
				t.Fatalf("[%s] rank %d: %v", order, ids[i], err)
			}
			if rank != i {
				t.Errorf("[%s] id %d rank=%d want %d (order %v)", order, ids[i], rank, i, ids)
			}
		}
	}
}

func TestExecuteAdjacent_Random(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rnd_a.png")
	ingestTestImage(t, database, cfg, "rnd_b.png")
	ingestTestImage(t, database, cfg, "rnd_c.png")

	const seed int64 = 1234567
	q := Query{Sort: "random", RandomSeed: seed, Page: 1, Limit: 40}
	result, err := Execute(context.Background(), testSource(database), q)
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	assertAdjacencyChain(t, database, q, resultIDs(result))
}

func TestExecuteAdjacent_RandomNoSeed(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rnd_no_seed.png")
	prev, next, err := ExecuteAdjacent(context.Background(), testSource(database), Query{Sort: "random"}, 1)
	if err != nil || prev != nil || next != nil {
		t.Errorf("random adjacency without seed must be nil/nil, got prev=%v next=%v err=%v", prev, next, err)
	}
}

func TestRankInQuery_RandomNoSeed(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rank_rnd.png")
	q := Query{Sort: "random", RandomSeed: 0, Page: 1, Limit: 40}
	rank, err := RankInQuery(context.Background(), testSource(database), q, 1)
	if err != nil || rank != -1 {
		t.Errorf("random sort with seed=0 must be (-1, nil), got (%d, %v)", rank, err)
	}
}

func TestExecute_RandomSmallSeedShuffles(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 8; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("rnd_small_%d.png", i))
	}

	q := Query{Sort: "random", RandomSeed: 1, Page: 1, Limit: 40}
	res := mustExecute(t, database, q)
	if len(res.Results) < 8 {
		t.Fatalf("got %d rows, want 8", len(res.Results))
	}
	asc := true
	for i := 1; i < len(res.Results); i++ {
		if res.Results[i].ID <= res.Results[i-1].ID {
			asc = false
			break
		}
	}
	if asc {
		t.Errorf("seed=1 returned strict id-ASC; expected a shuffle: %+v", res.Results)
	}
}

func TestExecute_RandomSeedStableAcrossCalls(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 6; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("rnd_stable_%d.png", i))
	}

	q := Query{Sort: "random", RandomSeed: 42, Page: 1, Limit: 40}
	first := mustExecute(t, database, q)
	second := mustExecute(t, database, q)
	if len(first.Results) != len(second.Results) {
		t.Fatalf("call returned %d vs %d rows", len(first.Results), len(second.Results))
	}
	for i := range first.Results {
		if first.Results[i].ID != second.Results[i].ID {
			t.Errorf("position %d: first %d, second %d (order not stable)", i, first.Results[i].ID, second.Results[i].ID)
		}
	}
}

func TestExecute_RandomSmallSeedNoArithmeticStride(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 30; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("stride_%02d.png", i))
	}

	for _, seed := range []int64{1, 2, 10, 42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			q := Query{Sort: "random", RandomSeed: seed, Page: 1, Limit: 40}
			res := mustExecute(t, database, q)
			if len(res.Results) < 30 {
				t.Fatalf("got %d rows, want >= 30", len(res.Results))
			}
			ids := resultIDs(res)
			first := ids[1] - ids[0]
			constantStride := true
			for i := 2; i < len(ids); i++ {
				if ids[i]-ids[i-1] != first {
					constantStride = false
					break
				}
			}
			if constantStride {
				t.Errorf("seed=%d produced a constant id stride %d: %v", seed, first, ids)
			}
			if ids[0] < ids[len(ids)-1] && slices.IsSorted(ids) {
				t.Errorf("seed=%d returned a monotonic id sequence: %v", seed, ids)
			}
			if ids[0] > ids[len(ids)-1] && slices.IsSortedFunc(ids, func(a, b int64) int { return cmp.Compare(b, a) }) {
				t.Errorf("seed=%d returned a reversed monotonic id sequence: %v", seed, ids)
			}
		})
	}
}

func TestRankInQuery_RandomSeeded(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rank_rnd_seeded_a.png")
	ingestTestImage(t, database, cfg, "rank_rnd_seeded_b.png")
	ingestTestImage(t, database, cfg, "rank_rnd_seeded_c.png")
	q := Query{Sort: "random", RandomSeed: 1234567, Page: 1, Limit: 40}
	res, err := Execute(context.Background(), testSource(database), q)
	if err != nil || len(res.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(res.Results))
	}
	for i, img := range res.Results {
		rank, err := RankInQuery(context.Background(), testSource(database), q, img.ID)
		if err != nil {
			t.Fatalf("rank for id=%d: %v", img.ID, err)
		}
		if rank != i {
			t.Errorf("id=%d: rank = %d, want %d (matches Execute order)", img.ID, rank, i)
		}
	}
}

func TestRankInQuery_HonorsContextDeadline(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rank_ctx.png")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rank, err := RankInQuery(ctx, testSource(database), Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40}, 1)
	if err == nil || rank != -1 {
		t.Errorf("cancelled context must surface err and rank=-1, got (%d, %v)", rank, err)
	}
}

func TestExecute_StopsOnACancelledContext(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "exec_ctx.png")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cached := Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40, Cached: true, Text: "exec ctx"}
	mustExecute(t, database, cached)
	for name, q := range map[string]Query{
		"counted":    {Sort: "newest", Order: "desc", Page: 1, Limit: 40},
		"skip count": {Sort: "newest", Order: "desc", Page: 1, Limit: 40, SkipCount: true},
		"cached ids": cached,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Execute(ctx, testSource(database), q); !errors.Is(err, context.Canceled) {
				t.Errorf("Execute on a cancelled context = %v, want context.Canceled", err)
			}
		})
	}
}

func TestExecuteAdjacent_WithTagPredicate(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "adj_tag_a.png")
	ingestTestImage(t, database, env, "adj_tag_b.png")
	ingestTestImage(t, database, env, "adj_tag_c.png")

	result, err := Execute(context.Background(), testSource(database), Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	newest, _, oldest := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	generalID := categoryID(t, database, "general")
	tagID := insertTag(t, database, "blue", generalID, 2)
	for _, id := range []int64{newest, oldest} {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID)
	}

	assertAdjacencyChain(t, database, Query{Expr: TagExpr{Tag: "blue"}, Sort: "newest", Order: "desc"}, []int64{newest, oldest})
}

func TestExecuteAdjacent_RandomBucketBound(t *testing.T) {

	database, nearA, nearB, far := seedFarBucketTrio(t, "rb", int64(randomAdjacencyBucketSize)+1)

	generalID := categoryID(t, database, "general")
	tagID := insertTag(t, database, "rndtag", generalID, fastApproxThreshold)
	for _, id := range []int64{nearA, nearB, far} {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID)
	}

	q := Query{Expr: TagExpr{Tag: "rndtag"}, Sort: "random", RandomSeed: 1234567}

	assertBucketBound(t, database, q, nearA, nearB, far)
}

func TestExecuteAdjacent_NewestSparseAndBucketBound(t *testing.T) {
	database, nearA, nearB, far := seedFarBucketTrio(t, "ad", int64(andAdjacencyBucketSize)*2)

	generalID := categoryID(t, database, "general")
	mkTag := func(name string) int64 { return insertTag(t, database, name, generalID, fastApproxThreshold) }
	a := mkTag("ada")
	b := mkTag("adb")
	c := mkTag("adc")
	for _, img := range []int64{nearA, nearB, far} {
		for _, tag := range []int64{a, b, c} {
			mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, tag)
		}
	}

	expr := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "ada"},
			Right: TagExpr{Tag: "adb"},
		},
		Right: TagExpr{Tag: "adc"},
	}
	q := Query{Expr: expr, Sort: "filesize", Order: "desc"}

	assertBucketBound(t, database, q, nearA, nearB, far)

	q.Sort = "newest"
	if prev, next := mustExecuteAdjacent(t, context.Background(), database, q, far); prev != nil || next == nil || *next != nearB {
		t.Errorf("newest from the far image: prev=%v next=%v, want nil and %d", prev, next, nearB)
	}

	expr2 := AndExpr{Left: TagExpr{Tag: "ada"}, Right: TagExpr{Tag: "adb"}}
	q2 := Query{Expr: expr2, Sort: "newest", Order: "desc"}
	prev, next, _ := ExecuteAdjacent(context.Background(), testSource(database), q2, nearA)
	if prev == nil && next == nil {
		t.Errorf("2-AND adjacency on nearA returned nothing; expected to reach a peer")
	}
}

func TestExecuteAdjacent_NewestPopularWildcardBucketBound(t *testing.T) {
	database, nearA, nearB, far := seedFarBucketTrio(t, "wp", int64(andAdjacencyBucketSize)*2)

	generalID := categoryID(t, database, "general")
	tagID := insertTag(t, database, "wildpop_eyes", generalID, andDriverThreshold+1)
	for _, img := range []int64{nearA, nearB, far} {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, tagID)
	}

	expr := TagExpr{Tag: "wildpop", Wildcard: "prefix"}
	legs, ok := pickAndDriverTag(database, expr, true)
	if !ok || len(legs) != 1 {
		t.Fatalf("popular single wildcard with allowSingleLiteral: legs=%d ok=%v, want 1 leg", len(legs), ok)
	}

	q := Query{Expr: expr, Sort: "filesize", Order: "desc"}
	assertBucketBound(t, database, q, nearA, nearB, far)
}

func TestExecuteAdjacent_RandomSparseSkipsBucket(t *testing.T) {
	for _, ceiling := range []string{"", "general", "sensitive"} {
		t.Run(cmp.Or(ceiling, "none"), func(t *testing.T) {
			database, nearA, nearB, far := seedFarBucketTrio(t, "rs", int64(randomAdjacencyBucketSize)*2)

			generalID := categoryID(t, database, "general")

			tagID := insertTag(t, database, "rstag", generalID, 3)
			for _, id := range []int64{nearA, nearB, far} {
				mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID)
			}

			q := Query{Expr: ApplyCeiling(TagExpr{Tag: "rstag"}, ceiling), Sort: "random", RandomSeed: 1234567}

			reached := walkAdjacency(t, database, q, nearA)
			for _, want := range []int64{nearA, nearB, far} {
				if !reached[want] {
					t.Errorf("sparse random adjacency did not reach %d; reached=%v", want, reached)
				}
			}
		})
	}
}

func TestExecuteAdjacent_NewestSparseAndSkipsBucket(t *testing.T) {
	database, nearA, nearB, far := seedFarBucketTrio(t, "ns", int64(andAdjacencyBucketSize)*2)

	generalID := categoryID(t, database, "general")
	mkTag := func(name string) int64 { return insertTag(t, database, name, generalID, 3) }
	a := mkTag("nsa")
	b := mkTag("nsb")
	c := mkTag("nsc")
	for _, img := range []int64{nearA, nearB, far} {
		for _, tag := range []int64{a, b, c} {
			mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, tag)
		}
	}

	expr := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "nsa"},
			Right: TagExpr{Tag: "nsb"},
		},
		Right: TagExpr{Tag: "nsc"},
	}
	q := Query{Expr: expr, Sort: "newest", Order: "desc"}

	reached := walkAdjacency(t, database, q, nearA)
	for _, want := range []int64{nearA, nearB, far} {
		if !reached[want] {
			t.Errorf("sparse 3-AND adjacency did not reach %d; reached=%v", want, reached)
		}
	}
}

func walkAdjacency(t *testing.T, database *db.DB, q Query, start int64) map[int64]bool {
	t.Helper()
	reached := map[int64]bool{start: true}
	queue := []int64{start}
	for len(queue) > 0 {
		cursor := queue[0]
		queue = queue[1:]
		prev, next := mustExecuteAdjacent(t, context.Background(), database, q, cursor)
		for _, neighbour := range []*int64{prev, next} {
			if neighbour == nil || reached[*neighbour] {
				continue
			}
			reached[*neighbour] = true
			queue = append(queue, *neighbour)
		}
	}
	return reached
}

func TestExecuteAdjacent_RatingCeiling(t *testing.T) {
	database, env := setupSearchDB(t)
	safeID := ingestTestImageWithID(t, database, env, "adj_safe.png")
	expID := ingestTestImageWithID(t, database, env, "adj_explicit.png")
	safe2ID := ingestTestImageWithID(t, database, env, "adj_safe2.png")

	attachTag(t, database, safeID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))
	attachTag(t, database, safe2ID, ratingTagID(t, database, "general"))

	expr := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	q := Query{Expr: expr, Sort: "newest", Order: "desc"}

	prev, next := mustExecuteAdjacent(t, context.Background(), database, q, safeID)
	if prev == nil {
		t.Fatal("ceiling adjacency: prev = nil, want safe2ID")
	}
	if *prev == expID {
		t.Errorf("ceiling adjacency leaked the explicit image: prev = %d", *prev)
	}
	if *prev != safe2ID {
		t.Errorf("ceiling adjacency: prev = %d, want %d", *prev, safe2ID)
	}
	if next != nil {
		t.Errorf("oldest next under ceiling: got %v, want nil", next)
	}
}

func TestExecuteAdjacent_FolderHalfOpenRange(t *testing.T) {
	database, env := setupSearchDB(t)
	mkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(env.galleryDir, rel), 0755); err != nil {
			t.Fatal(err)
		}
	}
	ingestAt := func(rel, name string) int64 {
		t.Helper()
		return ingestTestImageWithID(t, database, env, rel+"/"+name)
	}

	mkdir("anime")
	mkdir("anime/girls")
	mkdir("animes")
	mkdir("anime-2024")
	rootImg := ingestAt(".", "fol_root.png")
	folderImg := ingestAt("anime", "fol_a.png")

	siblingDashImg := ingestAt("anime-2024", "fol_d.png")
	subImg := ingestAt("anime/girls", "fol_b.png")
	siblingImg := ingestAt("animes", "fol_c.png")

	q := Query{
		Expr:  FilterExpr{Key: "folder", Val: "anime"},
		Sort:  "newest",
		Order: "desc",
	}

	prev, next := mustExecuteAdjacent(t, context.Background(), database, q, folderImg)
	for _, p := range []*int64{prev, next} {
		if p == nil {
			continue
		}
		if *p == rootImg {
			t.Errorf("folder adjacency leaked root image %d", rootImg)
		}
		if *p == siblingImg {
			t.Errorf("folder adjacency leaked sibling-prefix image %d (animes/ outside anime/)", siblingImg)
		}
		if *p == siblingDashImg {
			t.Errorf("folder adjacency leaked low-ASCII sibling image %d (anime-2024/ outside anime/)", siblingDashImg)
		}
	}
	reachedSub := (prev != nil && *prev == subImg) || (next != nil && *next == subImg)
	if !reachedSub {
		t.Errorf("folder adjacency did not reach subfolder peer %d (prev=%v next=%v)", subImg, prev, next)
	}
}

func TestBuildWhere_DateFilter(t *testing.T) {
	where, args, _ := buildWhere(FilterExpr{Key: "date", Val: ">2024-01-01"})
	wantIn(t, where, "ingested_at")
	if len(args) == 0 {
		t.Error("expected args for date filter")
	}
}

func TestBuildWhere_DateFilter_HalfOpenRanges(t *testing.T) {
	withDisplayZone(t, time.UTC)
	upper, upperArgs, _ := buildWhere(FilterExpr{Key: "date", Val: "..2026-05-19"})
	if !strings.Contains(upper, "<= ?") || strings.Contains(upper, "BETWEEN") {
		t.Errorf("`date:..X` should produce <= bound, got %q", upper)
	}
	if len(upperArgs) != 1 || upperArgs[0] != "2026-05-19T23:59:59Z" {
		t.Errorf("upper bound args = %v, want [\"2026-05-19T23:59:59Z\"]", upperArgs)
	}

	lower, lowerArgs, _ := buildWhere(FilterExpr{Key: "date", Val: "2026-05-19.."})
	if !strings.Contains(lower, ">= ?") || strings.Contains(lower, "BETWEEN") {
		t.Errorf("`date:X..` should produce >= bound, got %q", lower)
	}
	if len(lowerArgs) != 1 || lowerArgs[0] != "2026-05-19T00:00:00Z" {
		t.Errorf("lower bound args = %v, want [\"2026-05-19T00:00:00Z\"]", lowerArgs)
	}

	emptyWhere, _, _ := buildWhere(FilterExpr{Key: "date", Val: ".."})
	wantIn(t, emptyWhere, "1=0")
}

func TestBuildWhere_DateFilter_EqualsOperator(t *testing.T) {
	bareWhere, bareArgs, _ := buildWhere(FilterExpr{Key: "date", Val: "2026-05-19"})
	equalsWhere, equalsArgs, _ := buildWhere(FilterExpr{Key: "date", Val: "=2026-05-19"})
	if bareWhere != equalsWhere {
		t.Errorf("`=date` should produce the same SQL as bare date\n bare: %q\nequal: %q", bareWhere, equalsWhere)
	}
	if len(bareArgs) != len(equalsArgs) {
		t.Errorf("arg shape mismatch: bare=%v, equals=%v", bareArgs, equalsArgs)
	}
	for i := range bareArgs {
		if bareArgs[i] != equalsArgs[i] {
			t.Errorf("arg[%d]: bare=%v, equals=%v", i, bareArgs[i], equalsArgs[i])
		}
	}
}

func TestBuildWhere_DateFilter_RejectsBadInput(t *testing.T) {
	for _, in := range []string{"abcd", "abcd..xyz", "2024-01..bogus", "2024/01/01", ">notadate", "<not", "20240101"} {
		where, args, _ := buildWhere(FilterExpr{Key: "date", Val: in})
		if !strings.Contains(where, "1=0") {
			t.Errorf("bad date %q: expected 1=0, got %q", in, where)
		}
		if len(args) != 0 {
			t.Errorf("bad date %q: expected no args, got %v", in, args)
		}
	}
}

func TestBuildWhere_DateFilter_AcceptsYearMonth(t *testing.T) {
	for _, in := range []string{"2024", "2024-06", "2024-06-15", ">2024-01", "<2099-12", "2000-01..2099-12"} {
		where, _, _ := buildWhere(FilterExpr{Key: "date", Val: in})
		if strings.Contains(where, "1=0") {
			t.Errorf("good date %q rejected: %q", in, where)
		}
	}
}

func TestBuildWhere_DateFilter_AcceptsMinutePrecision(t *testing.T) {
	withDisplayZone(t, time.UTC)
	for _, tc := range []struct {
		name, val string
		args      []any
	}{
		{"range upper bound extends to end of minute", "2026-05-22T19:23..2026-05-22T19:30", []any{"2026-05-22T19:23:00Z", "2026-05-22T19:30:59Z"}},
		{"inclusive upper operator extends to end of minute", "<=2026-05-22T19:23", []any{"2026-05-22T19:23:59Z"}},
		{"inclusive lower operator starts at the minute", ">=2026-05-22T19:23", []any{"2026-05-22T19:23:00Z"}},
		{"second precision is an exact-second BETWEEN", "=2026-05-22T19:23:42", []any{"2026-05-22T19:23:42Z", "2026-05-22T19:23:42Z"}},
		{"hour precision extends to end of hour", "2026-05-22T06", []any{"2026-05-22T06:00:00Z", "2026-05-22T06:59:59Z"}},
		{"hour range form", "2026-05-22T06..2026-05-22T08", []any{"2026-05-22T06:00:00Z", "2026-05-22T08:59:59Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			where, args, _ := buildWhere(FilterExpr{Key: "date", Val: tc.val})
			if len(args) == 2 {
				wantIn(t, where, "BETWEEN")
			}
			if !slices.Equal(args, tc.args) {
				t.Errorf("%s args = %v, want %v", tc.val, args, tc.args)
			}
		})
	}
	t.Run("malformed time precision still rejected", func(t *testing.T) {
		for _, in := range []string{"2026-05-22T19:", "2026-05-22T", "T19:23"} {
			where, _, _ := buildWhere(FilterExpr{Key: "date", Val: in})
			if !strings.Contains(where, "1=0") {
				t.Errorf("bad %q: expected 1=0, got %q", in, where)
			}
		}
	})
}

func TestExecute_WithAutoTaggedAt(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "autotagged.png")

	mustExec(t, database.Write, `UPDATE images SET auto_tagged_at = '2024-01-15T12:00:00Z'`)

	result := mustExecute(t, database, Query{Page: 1, Limit: 40})
	if result.Total == 0 {
		t.Fatal("Execute returned no rows after ingest")
	}
	found := false
	for _, img := range result.Results {
		if img.AutoTaggedAt != nil {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one image with AutoTaggedAt set")
	}
}

func TestSuggestTagsWithFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "combo1.png")
	ingestTestImage(t, database, cfg, "combo2.png")
	ingestTestImage(t, database, cfg, "combo3.png")

	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}

	catID := categoryID(t, database, "general")
	tagA := insertTag(t, database, "alpha", catID, 0)
	tagB := insertTag(t, database, "beta", catID, 0)
	tagBet := insertTag(t, database, "betula", catID, 0)

	add := func(img, tag int64) {
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, tag)
		mustExec(t, database.Write, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tag)
	}
	add(ids[0], tagA)
	add(ids[0], tagB)
	add(ids[1], tagA)
	add(ids[1], tagB)
	add(ids[2], tagBet)

	got, err := SuggestTagsWithFilter(testSource(database), nil, "be", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("plain suggest: expected 2, got %+v", got)
	}
	expr := Parse("alpha")
	got, err = SuggestTagsWithFilter(testSource(database), expr, "be", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("context suggest: expected only beta, got %+v", got)
	}
	if got[0].UsageCount != 2 {
		t.Errorf("expected combo count 2 for alpha+beta, got %d", got[0].UsageCount)
	}
}

func TestSuggestTagsWithFilter_ARareTagPastTheContextCapCountsExactly(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedVisibleRows(t, database, suggestContextCap+200)
	general := categoryID(t, database, "general")
	broad := insertTag(t, database, "broad", general, suggestContextCap+200)
	rare := insertTag(t, database, "rare_one", general, 5)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) SELECT id, ? FROM images`, broad)

	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) SELECT id, ? FROM images ORDER BY id DESC LIMIT 5`, rare)

	got, err := SuggestTagsWithFilter(testSource(database), Parse("broad"), "rare", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "rare_one" || got[0].UsageCount != 5 {
		t.Fatalf("broad context: got %+v, want rare_one counted 5", got)
	}
}

func TestSuggestTagsWithFilter_ATagCountContextPastTheCapCountsPerImage(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedVisibleRows(t, database, suggestContextCap+200)
	character := categoryID(t, database, "character")
	solo := insertTag(t, database, "solo_girl", character, suggestContextCap+200)
	rare := insertTag(t, database, "rare_girl", character, 5)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) SELECT id, ? FROM images`, solo)
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) SELECT id, ? FROM images ORDER BY id DESC LIMIT 5`, rare)

	for q, want := range map[string]int{"tagcount:character=2": 5, "tagcount:character<3": 5, "tagcount:character=1": 0} {
		got, err := SuggestTagsWithFilter(testSource(database), Parse(q), "rare", "character", 10)
		if err != nil {
			t.Fatal(err)
		}
		if want == 0 && len(got) != 0 || want > 0 && (len(got) != 1 || got[0].UsageCount != want) {
			t.Errorf("%s: got %+v, want rare_girl counted %d", q, got, want)
		}
	}
}

func TestBuildWherePerRow_TagCountTestsEachImage(t *testing.T) {
	where, args := buildWherePerRow(FilterExpr{Key: "tagcount", Val: "character=0"}, Source{})
	wantIn(t, where, "it.image_id = i.id")
	wantNotIn(t, where, "GROUP BY")
	if !slices.Equal(args, []any{"character", int64(0)}) {
		t.Errorf("args = %v", args)
	}
}

func TestSuggestTagsWithFilterEscapesLikeMetachars(t *testing.T) {
	database, cfg := setupSearchDB(t)
	imgID := ingestTestImageWithID(t, database, cfg, "esc1.png")

	catID := categoryID(t, database, "general")
	seed := func(name string) int64 {
		tid := insertTag(t, database, name, catID, 1)
		mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, tid)
		return tid
	}
	seed("alpha")
	seed("100%_orange_juice")
	seed("100x_orange_juice")

	expr := Parse("alpha")
	got, err := SuggestTagsWithFilter(testSource(database), expr, "100%", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "100%_orange_juice" {
		t.Fatalf("literal %% suggest: expected only 100%%_orange_juice, got %+v", got)
	}
}

func TestExecuteAdjacent_ATagCountMissFansTheList(t *testing.T) {
	database, env := setupSearchDB(t)
	character := categoryID(t, database, "character")
	a := insertTag(t, database, "char_a", character, 0)
	b := insertTag(t, database, "char_b", character, 0)
	var ids []int64
	for _, name := range []string{"tc1.png", "tc2.png", "tc3.png", "tc4.png"} {
		ids = append(ids, ingestTestImageWithID(t, database, env, name))
	}
	for _, id := range ids {
		attachTag(t, database, id, a)
	}
	attachTag(t, database, ids[2], b)

	src := testSource(database)
	q := Query{Expr: Parse("tagcount:character=1"), Sort: "newest", Order: "desc", Cached: true, Text: "tagcount:character=1"}
	prev, next, err := ExecuteAdjacent(context.Background(), src, q, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != ids[3] || next == nil || *next != ids[0] {
		t.Errorf("prev/next = %v/%v, want %d/%d", prev, next, ids[3], ids[0])
	}
	if cached, ok := src.Cache.get(q.listKey(src)); !ok || !slices.Equal(cached, []int64{ids[3], ids[1], ids[0]}) {
		t.Errorf("cached list = %v (%v), want the three single-character images newest first", cached, ok)
	}

	src.Cache.Drop()
	prev, next, err = ExecuteAdjacent(context.Background(), src, q, ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != ids[3] || next == nil || *next != ids[1] {
		t.Errorf("off-list prev/next = %v/%v, want %d/%d", prev, next, ids[3], ids[1])
	}
}

func TestExecuteAdjacent_CacheReuse(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "cr_a.png")
	ingestTestImage(t, database, env, "cr_b.png")
	ingestTestImage(t, database, env, "cr_c.png")

	q := Query{
		Sort:   "newest",
		Order:  "desc",
		Page:   1,
		Limit:  40,
		Cached: true,
		Text:   "test-cache-reuse",
	}
	res, err := Execute(context.Background(), testSource(database), q)
	if err != nil || len(res.Results) != 3 {
		t.Fatalf("Execute: err=%v len=%d", err, len(res.Results))
	}

	cached, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok {
		t.Fatalf("Execute did not populate the cache")
	}
	if len(cached) != 3 {
		t.Fatalf("cache size = %d, want 3", len(cached))
	}
	for i, img := range res.Results {
		if cached[i] != img.ID {
			t.Errorf("cache[%d] = %d, want %d", i, cached[i], img.ID)
		}
	}

	middle := res.Results[1].ID
	prev, next, err := ExecuteAdjacent(context.Background(), Source{Cache: testSource(database).Cache}, q, middle)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != res.Results[0].ID {
		t.Errorf("prev = %v, want %d", prev, res.Results[0].ID)
	}
	if next == nil || *next != res.Results[2].ID {
		t.Errorf("next = %v, want %d", next, res.Results[2].ID)
	}
}

func TestExecute_CachesFullMatchListOnMultiPage(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 5; i++ {
		ingestTestImage(t, database, env, "multi_"+string(rune('a'+i))+".png")
	}

	q := Query{
		Sort:   "newest",
		Order:  "desc",
		Page:   1,
		Limit:  2,
		Cached: true,
		Text:   "test-cache-multi",
	}
	res := mustExecute(t, database, q)
	if res.Total != 5 || len(res.Results) != 2 {
		t.Fatalf("page 1 result: total=%d len=%d, want 5/2", res.Total, len(res.Results))
	}
	var cached []int64
	var ok bool
	for i := 0; i < 100; i++ {
		if cached, ok = testSource(database).Cache.get(q.listKey(Source{})); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("multi-page render did not populate the cache")
	}
	if len(cached) != 5 {
		t.Fatalf("cached len = %d, want 5", len(cached))
	}
	if cached[0] != res.Results[0].ID || cached[1] != res.Results[1].ID {
		t.Errorf("cache prefix [%d %d] does not match page 1 [%d %d]",
			cached[0], cached[1], res.Results[0].ID, res.Results[1].ID)
	}
}

func TestExecute_RecentIDBoundFansTheFullMatchSet(t *testing.T) {
	t.Parallel()
	database, _ := setupSearchDB(t)

	const imageCount = (1 * 40) * driverIDBoundMargin * 2
	const matchCount = imageCount / 10
	seedBoundedANDFixture(t, database, imageCount, imageCount)

	expr := Parse("bound_a bound_b")
	q := Query{Expr: expr, Sort: "newest", Order: "desc", Page: 1, Limit: 40, Cached: true, Text: "test-bound-seed"}
	mustExecute(t, database, q)
	waitForFan(t, testSource(database).Cache, q.listKey(Source{}))
	ids, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok {
		t.Fatal("bounded page-1 render did not seed the adjacency cache")
	}
	if len(ids) != matchCount {
		t.Errorf("cached ids = %d, want the full match set %d", len(ids), matchCount)
	}
	page1 := mustExecute(t, database, q)
	if page1.Total != matchCount {
		t.Errorf("page 1 Total = %d, want the fanned count %d", page1.Total, matchCount)
	}

	q.Page = 2
	page2 := mustExecute(t, database, q)
	if page2.Total != page1.Total {
		t.Errorf("Total collapsed between pages: %d -> %d", page1.Total, page2.Total)
	}
	if len(page2.Results) != 40 {
		t.Errorf("page 2 results = %d, want 40", len(page2.Results))
	}
}

func TestExecute_RecentIDBoundFallsBackWhenTheWindowHoldsTooFew(t *testing.T) {
	t.Parallel()
	database, _ := setupSearchDB(t)

	seedBoundedANDFixture(t, database, 3000, 1000)

	q := Query{Expr: Parse("bound_a bound_b"), Sort: "newest", Order: "desc", Page: 1, Limit: 10, Cached: true, Text: "test-bound-fallback"}
	page1 := mustExecute(t, database, q)
	if len(page1.Results) != 10 {
		t.Fatalf("page 1 results = %d (Total %d), want 10", len(page1.Results), page1.Total)
	}
	var newest int64
	if err := database.Read.QueryRow(`SELECT MAX(image_id) FROM image_tags`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if page1.Results[0].ID != newest {
		t.Errorf("page 1 first id = %d, want the newest carrier %d", page1.Results[0].ID, newest)
	}

	q.Page = 2
	q.Cached = false
	page2 := mustExecute(t, database, q)
	if len(page2.Results) != 10 {
		t.Fatalf("page 2 results = %d, want 10", len(page2.Results))
	}
	if page2.Results[0].ID >= page1.Results[9].ID {
		t.Errorf("page 2 first id = %d, want below page 1's last %d", page2.Results[0].ID, page1.Results[9].ID)
	}
}

func seedBoundedANDFixture(t *testing.T, database *db.DB, n, lastTagged int) {
	t.Helper()
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var generalID int64
	if err := tx.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var tagIDs []int64
	for _, name := range []string{"bound_a", "bound_b"} {
		var id int64
		if err := tx.QueryRow(
			`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`, name, generalID,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		tagIDs = append(tagIDs, id)
	}
	for i := 1; i <= n; i++ {
		sha := fmt.Sprintf("bound%06d", i)
		var imgID int64
		if err := tx.QueryRow(
			`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 100) RETURNING id`,
			sha, "/gallery/"+sha+".png",
		).Scan(&imgID); err != nil {
			t.Fatal(err)
		}
		if i%10 != 0 || i > lastTagged {
			continue
		}
		for _, tagID := range tagIDs {
			if _, err := tx.Exec(
				`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, tagID,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tagID := range tagIDs {
		if _, err := tx.Exec(`UPDATE tags SET usage_count = ? WHERE id = ?`, fastApproxThreshold+1, tagID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_CacheServesGalleryReload(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 3; i++ {
		ingestTestImage(t, database, env, "reload_"+string(rune('a'+i))+".png")
	}

	q := Query{
		Sort:   "newest",
		Order:  "desc",
		Page:   1,
		Limit:  40,
		Cached: true,
		Text:   "test-cache-reload",
	}
	first := mustExecute(t, database, q)
	if len(first.Results) != 3 {
		t.Fatalf("first Results = %d, want 3", len(first.Results))
	}
	cached, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok || len(cached) != 3 {
		t.Fatalf("cache state: ok=%v len=%d", ok, len(cached))
	}

	mustExec(t, database.Write, `UPDATE images SET is_favorited = 1 WHERE id = ?`, first.Results[0].ID)

	second := mustExecute(t, database, q)
	if second.Total != 3 || len(second.Results) != 3 {
		t.Fatalf("second result: total=%d len=%d, want 3/3", second.Total, len(second.Results))
	}
	for i := range first.Results {
		if first.Results[i].ID != second.Results[i].ID {
			t.Errorf("Results[%d].ID = %d, want %d", i, second.Results[i].ID, first.Results[i].ID)
		}
	}
	if !second.Results[0].IsFavorited {
		t.Errorf("cache hit returned stale is_favorited; row mutation should be visible")
	}
}

func TestExecute_CacheSlicesByPage(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 5; i++ {
		ingestTestImage(t, database, env, "slice_"+string(rune('a'+i))+".png")
	}

	q := Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40, Cached: true, Text: "test-cache-slice"}
	populate := mustExecute(t, database, q)
	if len(populate.Results) != 5 {
		t.Fatalf("populate Results = %d, want 5", len(populate.Results))
	}
	cached, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok || len(cached) != 5 {
		t.Fatalf("cache state: ok=%v len=%d", ok, len(cached))
	}

	q.Page, q.Limit = 2, 2
	res := mustExecute(t, database, q)
	if res.Total != 5 {
		t.Errorf("Total = %d, want 5", res.Total)
	}
	if len(res.Results) != 2 {
		t.Fatalf("page 2 Results = %d, want 2", len(res.Results))
	}
	if res.Results[0].ID != cached[2] || res.Results[1].ID != cached[3] {
		t.Errorf("page 2 ids = [%d %d], want [%d %d]",
			res.Results[0].ID, res.Results[1].ID, cached[2], cached[3])
	}

	q.Page = 99
	pastEnd := mustExecute(t, database, q)
	if pastEnd.Total != 5 || len(pastEnd.Results) != 0 {
		t.Errorf("past-end result: total=%d len=%d, want 5/0", pastEnd.Total, len(pastEnd.Results))
	}
}

func TestExecute_PhashExact(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")
	const want = int64(0x123456789ABCDEF0)
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = 1`, want)
	q := Parse("phash:123456789abcdef0")
	res := mustExecute(t, database, Query{Expr: q, Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 1 || res.Results[0].ID != 1 {
		t.Fatalf("phash exact: total=%d ids=%v, want 1/[1]", res.Total, res.Results)
	}
}

func TestExecute_PhashBareIsNotNull(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")
	mustExec(t, database.Write, `UPDATE images SET phash = NULL WHERE id = 2`)
	q := Parse("phash:")
	res := mustExecute(t, database, Query{Expr: q, Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 1 {
		t.Fatalf("phash:bare: total=%d, want 1 (one row has NULL phash)", res.Total)
	}
}

func TestExecute_PhashDistanceMatchesViaPopcountFallback(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")
	ingestTestImage(t, database, env, "c.png")
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = 1`, int64(0xFF00))
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = 2`, int64(0xFF01))
	mustExec(t, database.Write, `UPDATE images SET phash = ? WHERE id = 3`, int64(0x0000))
	q := Parse("phash:000000000000ff00~1")
	res := mustExecute(t, database, Query{Expr: q, Sort: "newest", Page: 1, Limit: 40})
	if res.Total != 2 {
		t.Fatalf("phash:~1 total=%d, want 2", res.Total)
	}
}

func TestExecute_RelationVocabulary(t *testing.T) {
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "a.png")
	b := ingestTestImageWithID(t, database, env, "b.png")
	c := ingestTestImageWithID(t, database, env, "c.png")
	d := ingestTestImageWithID(t, database, env, "d.png")
	_ = d

	mustExec(t, database.Write, `INSERT INTO dup_groups (id, original_image_id) VALUES (1, ?)`, a)
	mustExec(t, database.Write, `INSERT INTO dup_group_members (image_id, group_id) VALUES (?, 1), (?, 1)`, a, b)
	mustExec(t, database.Write, `INSERT INTO derivative_edges (derivative_image_id, source_image_id) VALUES (?, ?)`, c, a)

	check := func(query string, wantIDs []int64) { checkSearchIDs(t, database, query, wantIDs) }
	check("relation:duplicate", []int64{a, b})
	check("relation:original", []int64{a})
	check("relation:derivative", []int64{c})
	check("relation:source", []int64{a})
	check("relation:any", []int64{a, b, c})
	check("relation:none", []int64{d})

	mustExec(t, database.Write, `DELETE FROM dup_group_members; DELETE FROM dup_groups; DELETE FROM alt_group_members; DELETE FROM alt_groups; DELETE FROM derivative_edges; DELETE FROM version_edges`)
	check("relation:duplicate", []int64{})
	check("relation:alternate", []int64{})
	check("relation:version", []int64{})
	check("relation:derivative", []int64{})
	check("relation:source", []int64{})
	check("relation:none", []int64{a, b, c, d})

	mustExec(t, database.Write, `UPDATE images SET series = 'My Set' WHERE id = ?`, d)
	check("relation:collection", []int64{d})
	check("relation:any", []int64{d})
	check("relation:none", []int64{a, b, c})
}

func TestExecute_RelationAnyDropsMissing(t *testing.T) {
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "a.png")
	b := ingestTestImageWithID(t, database, env, "b.png")
	c := ingestTestImageWithID(t, database, env, "c.png")

	mustExec(t, database.Write, `INSERT INTO dup_groups (id, original_image_id) VALUES (1, ?)`, a)
	mustExec(t, database.Write, `INSERT INTO dup_group_members (image_id, group_id) VALUES (?, 1), (?, 1)`, a, b)
	mustExec(t, database.Write, `INSERT INTO derivative_edges (derivative_image_id, source_image_id) VALUES (?, ?)`, c, a)
	mustExec(t, database.Write, `UPDATE images SET is_missing = 1 WHERE id = ?`, b)

	checkSearchIDs(t, database, "relation:any", []int64{a, c})
}

func TestExecute_IDFilter(t *testing.T) {
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "a.png")
	b := ingestTestImageWithID(t, database, env, "b.png")
	_ = b

	expr := Parse("id:" + strconv.FormatInt(a, 10))
	res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
	if len(res.Results) != 1 || res.Results[0].ID != a {
		t.Fatalf("id:%d got %v, want [%d]", a, res.Results, a)
	}

	bad := Parse("id:nope")
	bres := mustExecute(t, database, Query{Expr: bad, Sort: "newest", Page: 1, Limit: 40})
	if bres.Total != 0 {
		t.Fatalf("id:nope total=%d, want 0", bres.Total)
	}
}

func TestExecute_BatchFilter(t *testing.T) {
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "a.png")
	b := ingestTestImageWithID(t, database, env, "b.png")
	c := ingestTestImageWithID(t, database, env, "c.png")
	mustExec(t, database.Write, `UPDATE images SET upload_batch = CASE id WHEN ? THEN 100 WHEN ? THEN 100 ELSE 200 END`, a, b)

	check := func(q string, want []int64) { checkSearchSet(t, database, q, want...) }
	check("batch:100", []int64{a, b})
	check("batch:200", []int64{c})
	check("batch:nope", nil)
}

func ingestTestImageWithID(t *testing.T, database *db.DB, env *searchEnv, name string) int64 {
	t.Helper()
	ingestTestImage(t, database, env, name)
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestExecute_PhashMalformed(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	for _, q := range []string{"phash:nothex", "phash:abc~99", "phash:abcd"} {
		expr := Parse(q)
		res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
		if res.Total != 0 {
			t.Fatalf("%q matched %d rows, want 0", q, res.Total)
		}
	}
}

func TestExecute_ModelSamplerFilters(t *testing.T) {
	database, env := setupSearchDB(t)
	sdID := ingestTestImageWithID(t, database, env, "ms_sd.png")
	comfyID := ingestTestImageWithID(t, database, env, "ms_comfy.png")

	mustExec(t, database.Write, `INSERT INTO sd_metadata (image_id, model, sampler) VALUES (?, ?, ?)`, sdID, "dreamshaper_v8", "euler_a")
	mustExec(t, database.Write, `INSERT INTO comfyui_metadata (image_id, model_checkpoint, sampler) VALUES (?, ?, ?)`, comfyID, "sdxl_base", "dpmpp_2m")

	check := func(query string, wantIDs []int64) { checkSearchIDs(t, database, query, wantIDs) }

	check("model:dreamshaper_v8", []int64{sdID})
	check("model:sdxl_base", []int64{comfyID})
	check("sampler:euler_a", []int64{sdID})
	check("sampler:dpmpp_2m", []int64{comfyID})

	check("-model:dreamshaper_v8", []int64{comfyID})
	check("-sampler:dpmpp_2m", []int64{sdID})
}

func TestExecute_SeedPastInt64(t *testing.T) {
	database, env := setupSearchDB(t)
	sdID := ingestTestImageWithID(t, database, env, "seed_sd.png")
	comfyID := ingestTestImageWithID(t, database, env, "seed_comfy.png")
	ingestTestImageWithID(t, database, env, "seed_none.png")

	mustExec(t, database.Write, `INSERT INTO sd_metadata (image_id, seed) VALUES (?, ?)`, sdID, int64(-1))
	mustExec(t, database.Write, `INSERT INTO comfyui_metadata (image_id, seed) VALUES (?, ?)`, comfyID, int64(-9223372036854775808))

	check := func(query string, wantIDs []int64) { checkSearchIDs(t, database, query, wantIDs) }

	check("seed:18446744073709551615", []int64{sdID})
	check("seed:9223372036854775808", []int64{comfyID})
	check("seed:18446744073709551616", nil)
}

func TestExecute_CatFilterIgnoresCase(t *testing.T) {
	database, env := setupSearchDB(t)
	charImg := ingestTestImageWithID(t, database, env, "cat_char.png")
	generalImg := ingestTestImageWithID(t, database, env, "cat_general.png")
	charTag := insertTag(t, database, "miku", categoryID(t, database, "character"), 0)
	generalTag := insertTag(t, database, "sky", categoryID(t, database, "general"), 0)
	attachTag(t, database, charImg, charTag)
	attachTag(t, database, generalImg, generalTag)

	checkSearchIDs(t, database, "cat:Character", []int64{charImg})

	mustExec(t, database.Write, `UPDATE tags SET usage_count = ? WHERE id = ?`, fastApproxThreshold, charTag)
	if n, ok := fastTagTotal(testSource(database), FilterExpr{Key: "cat", Val: "CHARACTER"}); !ok || n != fastApproxThreshold {
		t.Errorf("fast count = %d, %v; want %d from the character usages", n, ok, fastApproxThreshold)
	}
}

func TestExecute_SizeRatioRange(t *testing.T) {
	database, env := setupSearchDB(t)
	smallID := ingestTestImageWithID(t, database, env, "sr_small.png")
	midID := ingestTestImageWithID(t, database, env, "sr_mid.png")
	bigID := ingestTestImageWithID(t, database, env, "sr_big.png")

	mustExec(t, database.Write, `UPDATE images SET file_size = ? WHERE id = ?`, 1*1024, smallID)
	mustExec(t, database.Write, `UPDATE images SET file_size = ? WHERE id = ?`, 3*1024, midID)
	mustExec(t, database.Write, `UPDATE images SET file_size = ? WHERE id = ?`, 9*1024, bigID)

	mustExec(t, database.Write, `UPDATE images SET width = 50, height = 100 WHERE id = ?`, smallID)
	mustExec(t, database.Write, `UPDATE images SET width = 150, height = 100 WHERE id = ?`, midID)
	mustExec(t, database.Write, `UPDATE images SET width = 300, height = 100 WHERE id = ?`, bigID)

	check := func(query string, wantIDs []int64) { checkSearchIDs(t, database, query, wantIDs) }

	check("size:2kb..5kb", []int64{midID})
	check("size:>9000000TB", nil)
	check("size:<9000000TB", []int64{smallID, midID, bigID})
	check("ratio:1.2..1.8", []int64{midID})
}

func TestExecute_NegatedOperandBeforeORReturnsTheUnion(t *testing.T) {
	database, env := setupSearchDB(t)
	sketchOnly := ingestTestImageWithID(t, database, env, "nor_sketch.png")
	favOnly := ingestTestImageWithID(t, database, env, "nor_fav.png")
	both := ingestTestImageWithID(t, database, env, "nor_both.png")
	neither := ingestTestImageWithID(t, database, env, "nor_neither.png")
	sketch := getOrCreateTagID(t, database, "sketch")
	fav := getOrCreateTagID(t, database, "fav_tag")
	attachTag(t, database, sketchOnly, sketch)
	attachTag(t, database, both, sketch)
	attachTag(t, database, favOnly, fav)
	attachTag(t, database, both, fav)

	checkSearchIDs(t, database, "-sketch OR fav_tag", []int64{favOnly, both, neither})
}

func TestExecute_GeneratedCompound(t *testing.T) {
	database, env := setupSearchDB(t)
	tagID := ingestTestImageWithID(t, database, env, "gc_tag.png")
	genID := ingestTestImageWithID(t, database, env, "gc_gen.png")
	_ = ingestTestImageWithID(t, database, env, "gc_other.png")

	attachTag(t, database, tagID, getOrCreateTagID(t, database, "sometag"))
	mustExec(t, database.Write, `INSERT INTO sd_metadata (image_id, generation_hash) VALUES (?, ?)`, genID, "abc123")

	expr := Parse("sometag OR generated:abc123")
	res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Order: "asc", Page: 1, Limit: 40})
	gotIDs := resultIDs(res)
	wantIDs := []int64{tagID, genID}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got ids %v, want %v", gotIDs, wantIDs)
	}
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("at %d: got %d, want %d", i, gotIDs[i], wantIDs[i])
		}
	}
}

func TestExecute_RelationVocabulary_PartialPresence(t *testing.T) {
	database, env := setupSearchDB(t)
	dupA := ingestTestImageWithID(t, database, env, "pp_dup_a.png")
	dupB := ingestTestImageWithID(t, database, env, "pp_dup_b.png")
	verChild := ingestTestImageWithID(t, database, env, "pp_ver_child.png")
	verParent := ingestTestImageWithID(t, database, env, "pp_ver_parent.png")
	coll := ingestTestImageWithID(t, database, env, "pp_coll.png")
	none := ingestTestImageWithID(t, database, env, "pp_none.png")

	mustExec(t, database.Write, `INSERT INTO dup_groups (id, original_image_id) VALUES (1, ?)`, dupA)
	mustExec(t, database.Write, `INSERT INTO dup_group_members (image_id, group_id) VALUES (?, 1), (?, 1)`, dupA, dupB)
	mustExec(t, database.Write, `INSERT INTO version_edges (child_image_id, parent_image_id) VALUES (?, ?)`, verChild, verParent)
	mustExec(t, database.Write, `UPDATE images SET series = 'My Set' WHERE id = ?`, coll)

	check := func(query string, wantIDs []int64) { checkSearchIDs(t, database, query, wantIDs) }

	check("relation:duplicate", []int64{dupA, dupB})
	check("relation:version", []int64{verChild, verParent})
	check("relation:collection", []int64{coll})
	check("relation:alternate", []int64{})
	check("relation:derivative", []int64{})

	check("relation:any", []int64{dupA, dupB, verChild, verParent, coll})
	check("relation:none", []int64{none})
}

func TestExecute_FolderEscapedQuote(t *testing.T) {
	database, env := setupSearchDB(t)

	subDir := filepath.Join(env.galleryDir, `a"b`)
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	ingestTestImage(t, database, env, "root.png")

	wantID := ingestTestImageWithID(t, database, env, `a"b/foo.png`)

	expr := Parse(`folder:"a\"b"`)
	if f, ok := expr.(FilterExpr); !ok || f.Key != "folder" || f.Val != `a"b` {
		t.Fatalf("parsed to %#v, want FilterExpr{folder, a\"b}", expr)
	}
	res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Order: "asc", Page: 1, Limit: 40})
	if len(res.Results) != 1 || res.Results[0].ID != wantID {
		gotIDs := resultIDs(res)
		t.Fatalf(`folder:"a\"b" got ids %v, want [%d]`, gotIDs, wantID)
	}
}

type similarityFixture struct {
	seed, near, far, stranger int64
}

func setupSimilarityFixture(t *testing.T, database *db.DB, env *searchEnv) similarityFixture {
	t.Helper()
	for i := 0; i < 20; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("sim_filler_%02d.png", i))
	}
	for _, name := range []string{"sim_seed.png", "sim_near.png", "sim_far.png", "sim_stranger.png"} {
		ingestTestImage(t, database, env, name)
	}
	id := func(name string) int64 { return imageIDByName(t, database, name) }
	f := similarityFixture{
		seed:     id("sim_seed.png"),
		near:     id("sim_near.png"),
		far:      id("sim_far.png"),
		stranger: id("sim_stranger.png"),
	}
	scarf := getOrCreateTagID(t, database, "plaid_scarf")
	wings := getOrCreateTagID(t, database, "ice_wings")
	boots := getOrCreateTagID(t, database, "knee_boots")
	other := getOrCreateTagID(t, database, "unrelated_thing")
	attachTag(t, database, f.seed, scarf)
	attachTag(t, database, f.seed, wings)
	attachTag(t, database, f.near, scarf)
	attachTag(t, database, f.near, wings)
	attachTag(t, database, f.far, scarf)
	attachTag(t, database, f.far, boots)
	attachTag(t, database, f.stranger, other)
	return f
}

func similarityIDs(t *testing.T, database *db.DB, query, sort string) []int64 {
	t.Helper()
	expr := Parse(query)
	res := mustExecute(t, database, Query{Expr: expr, Sort: sort, Page: 1, Limit: 40})
	ids := resultIDs(res)
	return ids
}

func TestExecute_SimilarityRanksByOverlapNotRarity(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 30; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("ov_filler_%02d.png", i))
	}
	for _, name := range []string{"ov_seed.png", "ov_broad.png", "ov_rare.png"} {
		ingestTestImage(t, database, env, name)
	}
	id := func(name string) int64 { return imageIDByName(t, database, name) }
	seed, broad, rare := id("ov_seed.png"), id("ov_broad.png"), id("ov_rare.png")

	scarce := getOrCreateTagID(t, database, "ov_scarce")
	attachTag(t, database, seed, scarce)
	attachTag(t, database, rare, scarce)
	for i := 0; i < 8; i++ {
		tag := getOrCreateTagID(t, database, fmt.Sprintf("ov_common_%02d", i))
		attachTag(t, database, seed, tag)
		attachTag(t, database, broad, tag)
		for k := 0; k < 10; k++ {
			attachTag(t, database, id(fmt.Sprintf("ov_filler_%02d.png", k)), tag)
		}
	}

	got := similarityIDs(t, database, fmt.Sprintf("similar:%d", seed), "similarity")
	if len(got) == 0 || got[0] != broad {
		t.Fatalf("similarity order = %v, want the eight-tag sharer %d first", got, broad)
	}
	for _, gotID := range got {
		if gotID == rare {
			return
		}
	}
	t.Fatal("the single-rare-tag sharer left the result set")
}

func TestExecute_SimilarBareMatchesSharersOnly(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	got := similarityIDs(t, database, fmt.Sprintf("similar:%d", f.seed), "newest")
	want := map[int64]bool{f.near: true, f.far: true}
	if len(got) != 2 {
		t.Fatalf("similar:%d matched %v, want the two tag sharers", f.seed, got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("similar:%d matched %d, which shares no counted tag", f.seed, id)
		}
	}
}

func TestExecute_SimilarNeverReturnsTheSeed(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	for _, id := range similarityIDs(t, database, fmt.Sprintf("similar:%d", f.seed), "newest") {
		if id == f.seed {
			t.Fatal("similar: returned the seed image itself")
		}
	}
}

func TestExecute_SimilaritySortRanksClosestFirst(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	got := similarityIDs(t, database, fmt.Sprintf("similar:%d", f.seed), "similarity")
	if len(got) != 2 || got[0] != f.near || got[1] != f.far {
		t.Fatalf("similarity order = %v, want [%d %d]", got, f.near, f.far)
	}
}

func TestExecute_SimilarityWithoutSeedFallsBackToNewest(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	got := similarityIDs(t, database, "plaid_scarf", "similarity")
	if len(got) != 3 {
		t.Fatalf("plaid_scarf under similarity sort = %v, want three carriers", got)
	}
	if got[0] != f.far {
		t.Fatalf("fallback order = %v, want newest-first starting at %d", got, f.far)
	}
}

func TestExecute_SimilarThresholdIsInclusive(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	visible, _ := testSource(database).Tallies.VisibleCount()
	seed, err := tags.LoadSimilaritySeed(database, f.seed, visible)
	if err != nil {
		t.Fatal(err)
	}

	if got := tags.SimilarityScore(seed.Norm, seed.Norm, seed.Norm); got != 1 {
		t.Fatalf("fixture score = %v, want the identical-set bound for the boundary check", got)
	}
	atScore := similarityIDs(t, database, fmt.Sprintf("similar:%d~1", f.seed), "similarity")
	if len(atScore) != 1 || atScore[0] != f.near {
		t.Fatalf("similar:%d~1 = %v, want the saturating match %d", f.seed, atScore, f.near)
	}
}

func TestExecute_SimilarThresholdDropsWeakerMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	got := similarityIDs(t, database, fmt.Sprintf("similar:%d~0.9", f.seed), "similarity")
	if len(got) != 1 || got[0] != f.near {
		t.Fatalf("similar:%d~0.9 = %v, want only the two-tag sharer %d", f.seed, got, f.near)
	}
}

func TestExecute_SimilarThresholdKeepsTheBoundaryMatch(t *testing.T) {
	database, env := setupSearchDB(t)

	for i := 0; i < 20; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("bnd_filler_%02d.png", i))
	}
	for _, name := range []string{"bnd_seed.png", "bnd_exact.png", "bnd_padded.png", "bnd_thin.png"} {
		ingestTestImage(t, database, env, name)
	}
	id := func(name string) int64 { return imageIDByName(t, database, name) }
	seedTags := make([]int64, 3)
	for i := range seedTags {
		seedTags[i] = getOrCreateTagID(t, database, fmt.Sprintf("bnd_shared_%d", i))
		attachTag(t, database, id("bnd_seed.png"), seedTags[i])
	}

	attachTag(t, database, id("bnd_exact.png"), seedTags[0])
	attachTag(t, database, id("bnd_exact.png"), seedTags[1])

	attachTag(t, database, id("bnd_padded.png"), seedTags[0])
	attachTag(t, database, id("bnd_padded.png"), seedTags[1])
	attachTag(t, database, id("bnd_padded.png"), getOrCreateTagID(t, database, "bnd_own"))

	attachTag(t, database, id("bnd_thin.png"), seedTags[0])

	query := fmt.Sprintf("similar:%d~0.8", id("bnd_seed.png"))
	got := similarityIDs(t, database, query, "similarity")
	if len(got) != 1 || got[0] != id("bnd_exact.png") {
		t.Fatalf("%s = %v, want only the boundary match %d", query, got, id("bnd_exact.png"))
	}
}

func TestExecute_SimilarComposesWithNegation(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	got := similarityIDs(t, database, fmt.Sprintf("similar:%d -knee_boots", f.seed), "similarity")
	if len(got) != 1 || got[0] != f.near {
		t.Fatalf("similar with exclusion = %v, want [%d]", got, f.near)
	}
	excluded := similarityIDs(t, database, fmt.Sprintf("-similar:%d", f.seed), "newest")
	for _, id := range excluded {
		if id == f.near || id == f.far {
			t.Fatalf("-similar:%d still matched sharer %d", f.seed, id)
		}
	}
}

func TestExecute_SimilarDegenerateSeedMatchesNothing(t *testing.T) {
	database, env := setupSearchDB(t)
	setupSimilarityFixture(t, database, env)
	bare := ingestTestImageWithID(t, database, env, "sim_untagged.png")
	if got := similarityIDs(t, database, fmt.Sprintf("similar:%d", bare), "similarity"); len(got) != 0 {
		t.Fatalf("similar: against an untagged seed = %v, want no matches", got)
	}
}

func TestExecute_SimilarMalformedMatchesNothing(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	for _, q := range []string{
		"similar:abc",
		"similar:0",
		"similar:-4",
		fmt.Sprintf("similar:%d~", f.seed),
		fmt.Sprintf("similar:%d~1.5", f.seed),
		fmt.Sprintf("similar:%d~junk", f.seed),
	} {
		if got := similarityIDs(t, database, q, "newest"); len(got) != 0 {
			t.Errorf("%s matched %v, want nothing", q, got)
		}
	}
}

func TestExecute_SimilarLeftmostSeedDrivesTheRanking(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	expr := Parse(fmt.Sprintf("similar:%d similar:%d", f.far, f.seed))
	id, ok := SimilaritySeedID(expr)
	if !ok || id != f.far {
		t.Fatalf("leftmost seed = %d (ok=%v), want %d", id, ok, f.far)
	}
	if !HasSimilarTerm(expr) {
		t.Fatal("HasSimilarTerm missed a positive similar: term")
	}
	negated := Parse(fmt.Sprintf("-similar:%d", f.seed))
	if HasSimilarTerm(negated) {
		t.Fatal("HasSimilarTerm counted a negated term")
	}
}

func TestExecute_SimilarityFanMatchesTheScoredOrder(t *testing.T) {
	database, env := setupSearchDB(t)
	names := []string{"fan_seed.png", "fan_a.png", "fan_b.png", "fan_c.png", "fan_d.png", "fan_e.png"}
	for _, name := range names {
		ingestTestImage(t, database, env, name)
	}
	id := func(name string) int64 { return imageIDByName(t, database, name) }
	shared := make([]int64, 4)
	for i := range shared {
		shared[i] = getOrCreateTagID(t, database, fmt.Sprintf("fan_shared_%d", i))
		attachTag(t, database, id("fan_seed.png"), shared[i])
	}
	attachTag(t, database, id("fan_a.png"), shared[0])
	attachTag(t, database, id("fan_a.png"), shared[1])
	attachTag(t, database, id("fan_a.png"), shared[2])
	for _, tagID := range shared {
		attachTag(t, database, id("fan_b.png"), tagID)
	}
	for i := 0; i < 6; i++ {
		attachTag(t, database, id("fan_b.png"), getOrCreateTagID(t, database, fmt.Sprintf("fan_pad_%d", i)))
	}
	attachTag(t, database, id("fan_c.png"), shared[0])
	attachTag(t, database, id("fan_d.png"), shared[0])
	attachTag(t, database, id("fan_d.png"), shared[1])

	seed := id("fan_seed.png")
	expr := Parse(fmt.Sprintf("similar:%d OR fan_pad_0", seed))
	run := func(q Query) []int64 {
		t.Helper()
		res := mustExecute(t, database, q)
		ids := resultIDs(res)
		return ids
	}
	for _, order := range []string{"desc", "asc"} {
		testSource(database).Cache.Drop()
		fanned := run(Query{Expr: expr, Sort: "similarity", Order: order, Page: 1, Limit: 40, Cached: true, Text: "fan"})
		scored := run(Query{Expr: expr, Sort: "similarity", Order: order, Page: 1, Limit: 40})
		if !slices.Equal(fanned, scored) {
			t.Fatalf("%s: fanned order %v, scored order %v", order, fanned, scored)
		}
	}
}

func TestExecute_SimilarityRankingFollowsATagWrite(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Order: "desc", Page: 1, Limit: 40, Cached: true, Text: fmt.Sprintf("similar:%d", f.seed)}
	fan := func() []int64 {
		t.Helper()
		testSource(database).Cache.Drop()
		mustExecute(t, database, q)
		ids, ok := testSource(database).Cache.get(q.listKey(Source{}))
		if !ok {
			t.Fatal("the fan left no list behind")
		}
		return ids
	}

	got := fan()
	if len(got) != 2 || got[0] != f.near {
		t.Fatalf("similarity order = %v, want %d first", got, f.near)
	}

	for i := 0; i < 6; i++ {
		attachTag(t, database, f.near, getOrCreateTagID(t, database, fmt.Sprintf("sim_extra_%d", i)))
	}
	testSource(database).Tallies.Invalidate()

	got = fan()
	if len(got) != 2 || got[0] != f.far {
		t.Fatalf("post-write similarity order = %v, want %d first", got, f.far)
	}
}

func TestExecute_SimilarityRidesTheAdjacencyCache(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Page: 1, Limit: 40, Cached: true, Text: fmt.Sprintf("similar:%d", f.seed)}
	mustExecute(t, database, q)
	ids, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok || len(ids) != 2 || ids[0] != f.near {
		t.Fatalf("cached ids = %v (ok=%v), want the ranked pair starting at %d", ids, ok, f.near)
	}
	prev, next := mustExecuteAdjacent(t, context.Background(), database, q, f.near)
	if prev != nil || next == nil || *next != f.far {
		t.Fatalf("adjacent to %d = (prev %v, next %v), want (nil, %d)", f.near, prev, next, f.far)
	}
}

func TestExecute_SimilarityFanThatOverranFillsTheCacheAfterwards(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	testSource(database).Cache.Drop()
	old := fanBudget
	fanBudget = func(time.Duration, int) time.Duration { return 0 }
	t.Cleanup(func() { fanBudget = old })

	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Order: "desc", Page: 1, Limit: 40, Cached: true, Text: fmt.Sprintf("similar:%d", f.seed)}
	res := mustExecute(t, database, q)
	if len(res.Results) != 2 || res.Results[0].ID != f.near {
		t.Fatalf("scored page = %v, want the ranked pair starting at %d", res.Results, f.near)
	}
	cache, key := testSource(database).Cache, q.listKey(Source{})
	for deadline := time.Now().Add(5 * time.Second); !cache.tryAcquireFan(key); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the background fan never let go of the fan lock")
		}
	}
	cache.releaseFan(key)
	if ids, ok := cache.get(key); !ok || len(ids) != 2 || ids[0] != f.near || ids[1] != f.far {
		t.Fatalf("cached ids = %v (ok=%v), want [%d %d]", ids, ok, f.near, f.far)
	}
}

func TestExecute_SimilarityPageTwoSeedsTheCache(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	testSource(database).Cache.Drop()

	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Order: "desc", Page: 2, Limit: 1, Cached: true, Text: fmt.Sprintf("similar:%d", f.seed)}
	res := mustExecute(t, database, q)
	if res.Total != 2 || len(res.Results) != 1 || res.Results[0].ID != f.far {
		t.Fatalf("page 2 of 2 = %v (total %d), want just the second-ranked %d", res.Results, res.Total, f.far)
	}
	ids, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok || len(ids) != 2 || ids[0] != f.near || ids[1] != f.far {
		t.Fatalf("cached ids = %v (ok=%v), want [%d %d]", ids, ok, f.near, f.far)
	}
}

func TestExecute_SimilarityColdAdjacentAndRank(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)

	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Page: 1, Limit: 40}
	prev, next := mustExecuteAdjacent(t, context.Background(), database, q, f.far)
	if next != nil || prev == nil || *prev != f.near {
		t.Fatalf("cold adjacent to %d = (prev %v, next %v), want (%d, nil)", f.far, prev, next, f.near)
	}
	rank, err := RankInQuery(context.Background(), testSource(database), q, f.far)
	if err != nil {
		t.Fatal(err)
	}
	if rank != 1 {
		t.Fatalf("cold rank of %d = %d, want 1", f.far, rank)
	}
}

func TestSimilarityColdPathsHonourTheDeadline(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	testSource(database).Cache.Drop()

	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Order: "desc", Page: 1, Limit: 40}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prev, next := mustExecuteAdjacent(t, ctx, database, q, f.far)
	if prev != nil || next != nil {
		t.Errorf("cancelled adjacent = (prev %v, next %v), want both nil", prev, next)
	}
	if rank, err := RankInQuery(ctx, testSource(database), q, f.far); err != nil || rank != -1 {
		t.Errorf("cancelled rank = %d (err %v), want -1 so the caller keeps the URL's back_page", rank, err)
	}
}

func TestRankInQuery_AnswersWithinTheCapAndRefusesPastIt(t *testing.T) {
	database, _ := setupSearchDB(t)
	seedVisibleRows(t, database, rankInQueryMaxRank+60)

	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40}

	for _, want := range []int{0, 1, 40, rankInQueryMaxRank} {
		rank, err := RankInQuery(context.Background(), testSource(database), q, ids[want])
		if err != nil {
			t.Fatalf("rank of the row at %d: %v", want, err)
		}
		if rank != want {
			t.Errorf("rank of the row at %d = %d, want %d", want, rank, want)
		}
	}

	deep := ids[rankInQueryMaxRank+1]
	rank, err := RankInQuery(context.Background(), testSource(database), q, deep)
	if err != nil {
		t.Fatal(err)
	}
	if rank != -1 {
		t.Errorf("rank past the cap = %d, want -1 so the caller keeps the page the URL carries", rank)
	}
}

func TestFetchSortedMatchIDs_SpentBudgetKeepsNoPrefix(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"fan1.png", "fan2.png", "fan3.png"} {
		ingestTestImage(t, database, env, name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if ids := fetchSortedMatchIDs(ctx, database, "", "1=1", nil, "ORDER BY i.id", nil, 3); ids != nil {
		t.Errorf("fan past its budget = %v, want nil", ids)
	}
	if ids := fetchSortedMatchIDs(context.Background(), database, "", "1=1", nil, "ORDER BY i.id", nil, 3); len(ids) != 3 {
		t.Errorf("fan inside its budget = %v, want 3 ids", ids)
	}
}

func TestExecuteAdjacent_SimilarityColdFanSeedsTheCache(t *testing.T) {
	database, env := setupSearchDB(t)
	f := setupSimilarityFixture(t, database, env)
	testSource(database).Cache.Drop()

	expr := Parse(fmt.Sprintf("similar:%d", f.seed))
	q := Query{Expr: expr, Sort: "similarity", Order: "desc", Page: 1, Limit: 40, Cached: true, Text: fmt.Sprintf("similar:%d", f.seed)}
	if _, _, err := ExecuteAdjacent(context.Background(), testSource(database), q, f.far); err != nil {
		t.Fatal(err)
	}
	ids, ok := testSource(database).Cache.get(q.listKey(Source{}))
	if !ok || len(ids) != 2 || ids[0] != f.near || ids[1] != f.far {
		t.Fatalf("cached ids = %v (ok=%v), want [%d %d]", ids, ok, f.near, f.far)
	}
	rank, err := RankInQuery(context.Background(), testSource(database), q, f.far)
	if err != nil || rank != 1 {
		t.Fatalf("rank of %d off the seeded list = %d (err %v), want 1", f.far, rank, err)
	}
}

func TestExecute_Lookup(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"l1.png", "l2.png", "l3.png", "l4.png"} {
		ingestTestImage(t, database, env, name)
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	never, missed, exhausted, off := ids[0], ids[1], ids[2], ids[3]

	mustExec(t, database.Write, `INSERT INTO image_lookups (image_id, backend, attempts, last_at, last_result, next_due_at) VALUES (?, 'booru', 1, '2026-07-01T00:00:00Z', 'miss', '2099-01-01T00:00:00Z'), (?, 'booru', 6, '2026-07-01T00:00:00Z', 'miss', NULL)`, missed, exhausted)
	mustExec(t, database.Write, `UPDATE images SET scheduled_lookup = 0 WHERE id = ?`, off)

	check := func(q string, want ...int64) { checkSearchSet(t, database, q, want...) }
	due := func(backends []string, want ...int64) {
		t.Helper()
		src := testSource(database)
		src.LookupBackends = backends
		res, err := Execute(context.Background(), src, Query{Expr: Parse("lookup:due"), Page: 1, Limit: 40})
		if err != nil {
			t.Fatal(err)
		}
		got := resultIDs(res)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("lookup:due under %v matched %v, want %v", backends, got, want)
		}
	}
	both := []string{contract.LookupPTR, contract.LookupBooru}
	check("lookup:never", never, off)
	check("lookup:missed", missed)
	check("lookup:exhausted", exhausted)
	check("lookup:off", off)

	due(both, never, missed, exhausted, off)
	due([]string{contract.LookupBooru}, never)
	due(nil)
	mustExec(t, database.Write, `UPDATE images SET scheduled_lookup_ptr = 0 WHERE id = ?`, off)
	due(both, never, missed, exhausted)
	check("lookup:off", off)
}

func TestLookupBackendsSplitTheCachedList(t *testing.T) {
	q := Query{Cached: true, Text: "lookup:due"}
	if q.listKey(Source{}) == q.listKey(Source{LookupBackends: []string{contract.LookupPTR}}) {
		t.Error("a schedule change must not serve the list the old schedule cached")
	}
}

func TestExecute_Upgrade(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"u1.png", "u2.png", "u3.png", "u4.png", "u5.png", "u6.png"} {
		ingestTestImage(t, database, env, name)
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	differ, similar, verified, unknown, kept, nosource := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]

	mustExec(t, database.Write, ` INSERT INTO image_sources (image_id, site, post_id, url, md5, similarity, upgrade_kept) VALUES (?, 'danbooru', '1', 'https://d/1', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 0,  0), (?, 'gelbooru', '2', 'https://g/2', '',                                 92, 0), (?, 'danbooru', '3', 'https://d/3', (SELECT md5 FROM images WHERE id = ?), 0, 0), (?, 'danbooru', '4', 'https://d/4', '',                                 0,  0), (?, 'danbooru', '5', 'https://d/5', 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee', 0,  1)`, differ, similar, verified, verified, unknown, kept)
	for id, want := range map[int64]string{differ: "differ", verified: "match", kept: "differ"} {
		var got string
		if err := database.Read.QueryRow(
			`SELECT md5_match FROM image_sources WHERE image_id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("image %d verdict = %q, want %q", id, got, want)
		}
	}

	check := func(q string, want ...int64) { checkSearchSet(t, database, q, want...) }
	check("upgrade:any", differ, similar)
	check("upgrade:", differ, similar)
	check("upgrade:none", verified, unknown, kept, nosource)
	check("upgrade:unknown", unknown)
	check("upgrade:kept", kept)
	check("upgrade:danbooru", differ)
	check("upgrade:GELBOORU", similar)
	check("upgrade:nosuchsite")

	mustExec(t, database.Write, `UPDATE image_sources SET upgrade_kept = 0 WHERE image_id = ?`, kept)
	check("upgrade:any", differ, similar, kept)
}

func TestUpgradeFilterRidesItsPartialIndex(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "plan.png")
	expr := Parse("upgrade:any")
	where, args, _ := buildWhere(expr)
	rows, err := database.Read.Query(`EXPLAIN QUERY PLAN SELECT i.id FROM images i WHERE `+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	wantIn(t, plan, "idx_image_sources_upgradable")
}

func TestExecute_UpgradeSample(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"sample_abc.png", "art_master1200.png", "plain.png", "verified_sample_x.png", "sourceless_sample_y.png"} {
		ingestTestImage(t, database, env, name)
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	prefixed, master, plain, verified := ids[0], ids[1], ids[2], ids[3]
	mustExec(t, database.Write, ` INSERT INTO image_sources (image_id, site, post_id, url, md5_match) VALUES (?, 'danbooru', '1', 'https://d/1', ''), (?, 'pixiv',    '2', 'https://p/2', ''), (?, 'danbooru', '3', 'https://d/3', ''), (?, 'danbooru', '4', 'https://d/4', 'match')`, prefixed, master, plain, verified)

	check := func(q string, want ...int64) { checkSearchSet(t, database, q, want...) }

	check("upgrade:sample", prefixed, master)

	mustExec(t, database.Write, `UPDATE image_sources SET md5_match = 'differ' WHERE image_id = ?`, prefixed)
	check("upgrade:sample", master)
	check("upgrade:any", prefixed)
}

func TestExecute_UpgradeBigger(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"b1.png", "b2.png", "b3.png", "b4.png"} {
		ingestTestImage(t, database, env, name)
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	larger, smaller, byBytes, unknown := ids[0], ids[1], ids[2], ids[3]
	mustExec(t, database.Write, `UPDATE images SET width = 100, height = 100, file_size = 1000`)
	mustExec(t, database.Write, ` INSERT INTO image_sources (image_id, site, post_id, url, md5_match, post_width, post_height, post_size) VALUES (?, 'danbooru', '1', 'https://d/1', 'differ', 400, 400, 0), (?, 'danbooru', '2', 'https://d/2', 'differ', 50,  50,  9000), (?, 'danbooru', '3', 'https://d/3', 'differ', 0,   0,   9000), (?, 'danbooru', '4', 'https://d/4', 'differ', 0,   0,   0)`, larger, smaller, byBytes, unknown)

	expr := Parse("upgrade:bigger")
	res := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	got := resultIDs(res)
	slices.Sort(got)

	want := []int64{larger, byBytes}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("upgrade:bigger matched %v, want %v", got, want)
	}
}

func assertBucketBound(t *testing.T, database *db.DB, q Query, nearA, nearB, far int64) {
	t.Helper()
	prev, next := mustExecuteAdjacent(t, context.Background(), database, q, nearA)
	for _, p := range []*int64{prev, next} {
		if p != nil && *p == far {
			t.Errorf("nearA reached far image %d across bucket boundary", far)
		}
	}
	reachedNearB := (prev != nil && *prev == nearB) || (next != nil && *next == nearB)
	if !reachedNearB {
		t.Errorf("nearA did not reach in-bucket peer %d (prev=%v next=%v)", nearB, prev, next)
	}

	prev, next, _ = ExecuteAdjacent(context.Background(), testSource(database), q, far)
	if prev != nil || next != nil {
		t.Errorf("far alone in bucket: want nil/nil, got prev=%v next=%v", prev, next)
	}
}

func seedFarBucketTrio(t *testing.T, prefix string, far int64) (*db.DB, int64, int64, int64) {
	t.Helper()
	database, env := setupSearchDB(t)
	for _, suffix := range []string{"a", "b", "c"} {
		ingestTestImage(t, database, env, prefix+"_"+suffix+".png")
	}
	ids, err := db.QueryIDs(database.Read, `SELECT id FROM images ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE images SET id = ? WHERE id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"image_paths", "image_tags", "image_tag_sources"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET image_id = ? WHERE image_id = ?`, far, ids[2]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return database, ids[0], ids[1], far
}

func categoryID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("category %q: %v", name, err)
	}
	return id
}

func waitForFan(t *testing.T, cache *AdjacencyCache, key string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cache.tryAcquireFan(key); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the background fan never let go of the fan lock")
		}
	}
	cache.releaseFan(key)
}

func mustExecute(t *testing.T, database *db.DB, q Query) *models.SearchResult {
	t.Helper()
	res, err := Execute(context.Background(), testSource(database), q)
	if err != nil {
		t.Fatalf("Execute(%+v): %v", q, err)
	}
	return res
}

func checkSearchIDs(t *testing.T, database *db.DB, query string, wantIDs []int64) {
	t.Helper()
	expr := Parse(query)
	res := mustExecute(t, database, Query{Expr: expr, Sort: "newest", Order: "asc", Page: 1, Limit: 40})
	gotIDs := resultIDs(res)
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("%q: got ids %v, want %v", query, gotIDs, wantIDs)
	}
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("%q at %d: got %d, want %d", query, i, gotIDs[i], wantIDs[i])
		}
	}
}

func imageIDByName(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var out int64
	if err := database.Read.QueryRow(
		`SELECT id FROM images WHERE canonical_path LIKE ?`, "%"+name).Scan(&out); err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return out
}

func resultIDs(res *models.SearchResult) []int64 {
	ids := make([]int64, len(res.Results))
	for i, r := range res.Results {
		ids[i] = r.ID
	}
	return ids
}

func checkSearchSet(t *testing.T, database *db.DB, q string, want ...int64) {
	t.Helper()
	expr := Parse(q)
	res := mustExecute(t, database, Query{Expr: expr, Page: 1, Limit: 40})
	got := resultIDs(res)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("query %q matched %v, want %v", q, got, want)
	}
}

func insertTag(t *testing.T, database *db.DB, name string, categoryID int64, usage int) int64 {
	t.Helper()
	var id int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?) RETURNING id`,
		name, categoryID, usage,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mustExecuteAdjacent(t *testing.T, ctx context.Context, database *db.DB, q Query, currentID int64) (*int64, *int64) {
	t.Helper()
	prev, next, err := ExecuteAdjacent(ctx, testSource(database), q, currentID)
	if err != nil {
		t.Fatalf("ExecuteAdjacent(%d): %v", currentID, err)
	}
	return prev, next
}

func mustAddCollection(t *testing.T, database *db.DB, imageID int64, name string, order *int) {
	t.Helper()
	if err := gallery.AddCollectionMembership(database, imageID, name, order); err != nil {
		t.Fatalf("AddCollectionMembership(%d, %q): %v", imageID, name, err)
	}
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func TestExecute_TaggedSkipsTheDerivedTags(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"derived.png", "manual.png"} {
		ingestTestImage(t, database, env, name)
	}
	var derived, manual int64
	if err := database.Read.QueryRow(`SELECT MIN(id), MAX(id) FROM images`).Scan(&derived, &manual); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{derived, manual} {
		var n int
		if err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM image_tag_sources WHERE image_id = ? AND source = 'monbooru'`, id,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("image %d carries no derived tag to test against", id)
		}
	}
	mustExec(t, database.Write, `INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`,
		manual, getOrCreateTagID(t, database, "blue_eyes"))

	check := func(query string, want ...int64) { checkSearchSet(t, database, query, want...) }
	check("tagged:true", manual)
	check("tagged:false", derived)
	check("tagged:monbooru", derived, manual)
}

func testHandle(database *db.DB, galleryPath, thumbnailsPath string) gallery.Handle {
	return gallery.Handle{GalleryPath: galleryPath, ThumbnailsPath: thumbnailsPath, DB: database, TagSvc: tags.New(database, nil)}
}
