package globaltags

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

type gallery struct {
	Side
	t       *testing.T
	general int64
	images  int
}

func open(t *testing.T, name string) *gallery {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/" + name + ".db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	general, _, err := tags.CategoryIDByName(database, "general")
	if err != nil {
		t.Fatal(err)
	}
	return &gallery{Side: Side{Name: name, DB: database, Tags: tags.New(database, nil)}, t: t, general: general}
}

func (g *gallery) category(name string) int64 {
	g.t.Helper()
	id, ok, err := tags.CategoryIDByName(g.DB, name)
	if err != nil {
		g.t.Fatal(err)
	}
	if !ok {
		cat, err := g.Tags.CreateCategory(name, "#123456")
		if err != nil {
			g.t.Fatal(err)
		}
		id = cat.ID
	}
	return id
}

func split(spec string) (category, name string) {
	if c, n, ok := strings.Cut(spec, ":"); ok {
		return c, n
	}
	return "general", spec
}

func (g *gallery) tag(spec string) int64 {
	g.t.Helper()
	category, name := split(spec)
	tag, err := g.Tags.GetOrCreateTag(name, g.category(category))
	if err != nil {
		g.t.Fatal(err)
	}
	return tag.ID
}

func (g *gallery) alias(name, of string) {
	g.t.Helper()
	category, n := split(name)
	if _, err := g.Tags.CreateAlias(n, g.category(category), g.tag(of)); err != nil {
		g.t.Fatal(err)
	}
}

func (g *gallery) implies(parent, implied string) {
	g.t.Helper()
	if _, err := g.Tags.AddImplication(g.tag(parent), g.tag(implied)); err != nil {
		g.t.Fatal(err)
	}
}

func (g *gallery) image(tagged ...string) int64 {
	g.t.Helper()
	g.images++
	var id int64
	if err := g.DB.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 100) RETURNING id`,
		fmt.Sprintf("%s-%064d", g.Name, g.images)[:64], fmt.Sprintf("/%s/%d.png", g.Name, g.images),
	).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	for _, spec := range tagged {
		if err := g.Tags.AddTagToImage(id, g.tag(spec), false, nil); err != nil {
			g.t.Fatal(err)
		}
	}
	return id
}

func (g *gallery) strings(query string, args ...any) []string {
	g.t.Helper()
	rows, err := g.DB.Read.Query(query, args...)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

const keyOf = `CASE c.name WHEN 'general' THEN t.name ELSE c.name || ':' || t.name END`

func (g *gallery) global() []string {
	return g.strings(`SELECT ` + keyOf + ` FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE t.is_global = 1 ORDER BY 1`)
}

func (g *gallery) aliases(of string) []string {
	return g.strings(`SELECT `+keyOf+` FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE t.canonical_tag_id = ? ORDER BY 1`, g.tag(of))
}

func (g *gallery) implied(by string) []string {
	return g.strings(`SELECT `+keyOf+` FROM tag_implications i JOIN tags t ON t.id = i.implied_tag_id JOIN tag_categories c ON c.id = t.category_id
		WHERE i.parent_tag_id = ? ORDER BY 1`, g.tag(by))
}

func (g *gallery) row(spec string) int64 {
	g.t.Helper()
	category, name := split(spec)
	var id int64
	if err := g.DB.Read.QueryRow(`SELECT t.id FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE c.name = ? AND t.name = ?`, category, name).Scan(&id); err != nil {
		g.t.Fatalf("%s: %v", spec, err)
	}
	return id
}

func (g *gallery) has(spec string) bool {
	category, name := split(spec)
	return len(g.strings(`SELECT t.name FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE c.name = ? AND t.name = ?`, category, name)) == 1
}

func (g *gallery) tagsOn(image int64) []string {
	return g.strings(`SELECT `+keyOf+` FROM image_tags it JOIN tags t ON t.id = it.tag_id JOIN tag_categories c ON c.id = t.category_id
		WHERE it.image_id = ? ORDER BY 1`, image)
}

func (g *gallery) pending() []string {
	return g.strings(`SELECT name FROM global_tag_changes ORDER BY 1`)
}

func want(t *testing.T, label string, got []string, wanted ...string) {
	t.Helper()
	if wanted == nil {
		wanted = []string{}
	}
	if !slices.Equal(got, wanted) {
		t.Errorf("%s = %v, want %v", label, got, wanted)
	}
}

func sides(gs ...*gallery) []Side {
	out := make([]Side, len(gs))
	for i, g := range gs {
		out[i] = g.Side
	}
	return out
}

func makeGlobal(t *testing.T, from *gallery, all []Side, merge bool, specs ...string) (*Report, []Skip) {
	t.Helper()
	ids := make([]int64, len(specs))
	for i, s := range specs {
		ids[i] = from.tag(s)
	}
	rep, skipped, err := MakeGlobal(context.Background(), all, from.Name, ids, merge)
	if err != nil {
		t.Fatal(err)
	}
	return rep, skipped
}

func push(t *testing.T, all []Side) []Skip {
	t.Helper()
	skipped, err := Push(context.Background(), all)
	if err != nil {
		t.Fatal(err)
	}
	return skipped
}

func TestMakeGlobalSharesATagItsAliasesAndWhatItImplies(t *testing.T) {
	drawn, movies := open(t, "drawn"), open(t, "movies")
	all := sides(drawn, movies)
	drawn.alias("anime", "medium:animation")
	drawn.implies("medium:animation", "medium:video")
	drawn.tag("medium:watercolor")
	movies.implies("medium:music_video", "medium:video")
	clip := movies.image("medium:animation")

	rep, skipped := makeGlobal(t, drawn, all, false, "medium:animation")
	if len(skipped) != 0 || len(rep.Local) != 0 {
		t.Fatalf("skipped %v, local %v", skipped, rep.Local)
	}
	want(t, "comes along", []string{rep.ComesAlong[0].String()}, "medium:video")
	for _, g := range []*gallery{drawn, movies} {
		want(t, g.Name+" global", g.global(), "medium:animation", "medium:video")
		want(t, g.Name+" aliases", g.aliases("medium:animation"), "anime")
		want(t, g.Name+" implied", g.implied("medium:animation"), "medium:video")
		want(t, g.Name+" pending", g.pending())
	}
	want(t, "movies image", movies.tagsOn(clip), "medium:animation", "medium:video")
	if movies.has("medium:watercolor") || drawn.has("medium:music_video") {
		t.Error("a local tag crossed galleries")
	}
	want(t, "movies keeps its own rule", movies.implied("medium:music_video"), "medium:video")
	want(t, "drawn did not take it", drawn.strings(`SELECT name FROM tags WHERE name = 'music_video'`))
}

func TestMakeGlobalTakesWhatTheOtherGalleryAlreadySaysOfTheTag(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.tag("cat")
	b.alias("kitty", "cat")
	b.implies("cat", "animal")

	makeGlobal(t, a, sides(a, b), false, "cat")
	want(t, "a aliases", a.aliases("cat"), "kitty")
	want(t, "a implied", a.implied("cat"), "animal")
	want(t, "a global", a.global(), "animal", "cat")
}

func TestAConflictingTagStaysLocalAndIsListed(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	all := sides(a, b)
	a.alias("tag_foo", "tag_bar")
	b.alias("tag_bar", "tag_foo")
	a.alias("anime", "animation")
	b.image("anime")
	a.implies("parent", "tag_bar")
	a.tag("fine")

	rep, _ := makeGlobal(t, a, all, false, "tag_bar", "animation", "parent", "fine")
	var local []string
	for _, c := range rep.Local {
		local = append(local, c.Tag.String()+" in "+c.Gallery+": "+c.Reason)
	}
	want(t, "local", local,
		"animation in b: its alias anime is a tag on 1 image in b",
		"parent in b: it implies tag_bar, an alias of tag_foo in b",
		"tag_bar in b: tag_bar is an alias of tag_foo in b",
	)
	want(t, "a global", a.global(), "fine")
	want(t, "b global", b.global(), "fine")
	if !rep.Local[0].Mergeable {
		t.Error("an alias that is a tag with images is not offered a merge")
	}

	makeGlobal(t, a, all, true, "animation")
	want(t, "b aliases after the merge", b.aliases("animation"), "anime")
	want(t, "b global after the merge", b.global(), "animation", "fine")
	want(t, "b image", b.tagsOn(1), "animation")
}

func TestAnEditToAGlobalTagReachesTheOtherGalleries(t *testing.T) {
	a, b, c := open(t, "a"), open(t, "b"), open(t, "c")
	all := sides(a, b, c)
	a.alias("anime", "animation")
	a.implies("animation", "video")
	makeGlobal(t, a, all, false, "animation")
	pic := c.image("animation")
	want(t, "c image", c.tagsOn(pic), "animation", "video")

	b.alias("animated", "animation")
	b.implies("animation", "moving")
	if err := b.Tags.SetTagNote(b.tag("animation"), "Moving pictures.", []string{"https://example.org/a"}); err != nil {
		t.Fatal(err)
	}
	want(t, "b pending", b.pending(), "animation")
	if !Pending(all) {
		t.Error("Pending is false with a change recorded")
	}
	if skipped := push(t, all); len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	for _, g := range []*gallery{a, c} {
		want(t, g.Name+" aliases", g.aliases("animation"), "animated", "anime")
		want(t, g.Name+" implied", g.implied("animation"), "moving", "video")
		want(t, g.Name+" global", g.global(), "animation", "moving", "video")
		note, err := g.Tags.TagNote(g.tag("animation"))
		if err != nil || note.Body != "Moving pictures." || !slices.Equal(note.Links, []string{"https://example.org/a"}) {
			t.Errorf("%s note = %+v, %v", g.Name, note, err)
		}
		want(t, g.Name+" pending", g.pending())
	}
	want(t, "b pending after the push", b.pending())
	want(t, "c image after the push", c.tagsOn(pic), "animation", "moving", "video")
	if Pending(all) {
		t.Error("Pending is true after the push")
	}

	if err := c.Tags.DeleteTag(c.row("anime")); err != nil {
		t.Fatal(err)
	}
	if err := c.Tags.RemoveImplication(c.tag("animation"), c.tag("moving")); err != nil {
		t.Fatal(err)
	}
	push(t, all)
	for _, g := range []*gallery{a, b} {
		want(t, g.Name+" aliases after the removal", g.aliases("animation"), "animated")
		want(t, g.Name+" implied after the removal", g.implied("animation"), "video")
	}
	if a.has("anime") {
		t.Error("the removed alias is still a name in a")
	}
}

func TestARenameAndACategoryMoveFollow(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	all := sides(a, b)
	a.implies("animation", "video")
	makeGlobal(t, a, all, false, "animation")
	pic := b.image("animation")

	if err := a.Tags.RenameTagKeepAlias(a.tag("video"), "moving_picture"); err != nil {
		t.Fatal(err)
	}
	if err := a.Tags.ChangeTagCategory(a.tag("animation"), a.category("medium")); err != nil {
		t.Fatal(err)
	}
	if skipped := push(t, all); len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	want(t, "b global", b.global(), "medium:animation", "moving_picture")
	want(t, "b alias of the old name", b.aliases("moving_picture"), "video")
	want(t, "b implied", b.implied("medium:animation"), "moving_picture")
	want(t, "b image", b.tagsOn(pic), "medium:animation", "moving_picture")
	if b.has("animation") {
		t.Error("the old category still holds the tag in b")
	}
}

func TestAMergeOfAGlobalTagIsMergedEverywhere(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	all := sides(a, b)
	a.tag("kitty")
	a.tag("cat")
	makeGlobal(t, a, all, false, "kitty")
	pic := b.image("kitty")

	if err := a.Tags.MergeTags(a.tag("kitty"), a.tag("cat")); err != nil {
		t.Fatal(err)
	}
	if skipped := push(t, all); len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	want(t, "b global", b.global(), "cat")
	want(t, "b aliases", b.aliases("cat"), "kitty")
	want(t, "b image", b.tagsOn(pic), "cat")
	want(t, "a global", a.global(), "cat")
}

func TestMakeLocalStopsTheSharingAndKeepsEveryGallery(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	all := sides(a, b)
	a.alias("anime", "animation")
	a.implies("animation", "video")
	makeGlobal(t, a, all, false, "animation")

	err := MakeLocal(b.Side, []int64{b.tag("video")})
	if _, ok := err.(*ErrImplied); !ok {
		t.Fatalf("making an implied tag local = %v, want ErrImplied", err)
	}
	if err := MakeLocal(b.Side, []int64{b.tag("animation")}); err != nil {
		t.Fatal(err)
	}
	push(t, all)
	want(t, "a global", a.global(), "video")
	want(t, "a keeps the alias", a.aliases("animation"), "anime")
	want(t, "a keeps the rule", a.implied("animation"), "video")

	a.alias("animated", "animation")
	push(t, all)
	want(t, "b after a local edit in a", b.aliases("animation"), "anime")
}

func TestADeletedGlobalTagGoesLocalElsewhere(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	all := sides(a, b)
	a.implies("animation", "video")
	makeGlobal(t, a, all, false, "animation")
	pic := b.image("animation")

	if err := a.Tags.DeleteTag(a.tag("video")); err != tags.ErrTagGlobal {
		t.Fatalf("deleting a global tag = %v, want ErrTagGlobal", err)
	}
	if _, err := a.DB.Write.Exec(`DELETE FROM tags WHERE id = ?`, a.tag("video")); err != nil {
		t.Fatal(err)
	}
	push(t, all)
	want(t, "b global", b.global(), "animation")
	want(t, "b implied", b.implied("animation"))
	if !b.has("video") {
		t.Error("b lost the tag a deleted")
	}
	want(t, "b image keeps what it had", b.tagsOn(pic), "animation")
}

func TestACancelledPushResumes(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	all := sides(a, b)
	makeGlobal(t, a, all, false, "animation")
	a.alias("anime", "animation")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Push(ctx, all); err == nil {
		t.Fatal("a cancelled push reported no error")
	}
	want(t, "a pending after the cancel", a.pending(), "animation")
	want(t, "b left alone", b.aliases("animation"))
	want(t, "b not left applying", b.strings(`SELECT 'x' FROM global_tag_applying`))

	push(t, all)
	want(t, "b after the resume", b.aliases("animation"), "anime")
}

func TestPullBringsANewGalleryUpToDate(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.alias("anime", "medium:animation")
	a.implies("medium:animation", "video")
	makeGlobal(t, a, sides(a, b), false, "medium:animation")

	fresh := open(t, "c")
	fresh.tag("stale")
	if err := fresh.Tags.SetGlobal([]int64{fresh.tag("stale")}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Pull(context.Background(), sides(a, b, fresh), "c"); err != nil {
		t.Fatal(err)
	}
	want(t, "c global", fresh.global(), "medium:animation", "stale", "video")
	want(t, "a takes what the gallery held", a.global(), "medium:animation", "stale", "video")
	want(t, "c aliases", fresh.aliases("medium:animation"), "anime")
	want(t, "c pending", fresh.pending())
	want(t, "a pending", a.pending())
}

func TestCheckWritesNothingAndCountsWhatEachGalleryGets(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.alias("anime", "animation")
	a.implies("animation", "video")
	b.tag("animation")

	rep, err := Check(sides(a, b), "a", []int64{a.tag("animation")}, false)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "a global", a.global())
	want(t, "b aliases", b.aliases("animation"))
	if got := rep.Galleries[1]; got.Name != "b" || got.Tags != 1 || got.Aliases != 1 || got.Implications != 1 {
		t.Errorf("b gets %+v, want 1 tag, 1 alias, 1 implication", got)
	}
	if got := rep.Galleries[0]; got.Any() {
		t.Errorf("a gets %+v, want nothing", got)
	}
}

func TestAnImplicationThatWouldLoopIsRefused(t *testing.T) {
	a, b := open(t, "a"), open(t, "b")
	a.implies("x", "y")
	b.implies("y", "x")
	rep, _ := makeGlobal(t, a, sides(a, b), false, "x")
	if len(rep.Global) != 0 || len(rep.Local) == 0 {
		t.Errorf("global %v, local %v; want both tags kept local", rep.Global, rep.Local)
	}
	want(t, "a global", a.global())
}
