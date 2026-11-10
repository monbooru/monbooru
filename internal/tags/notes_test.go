package tags

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func mustTagNote(t *testing.T, svc *Service, id int64) TagNote {
	t.Helper()
	n, err := svc.TagNote(id)
	if err != nil {
		t.Fatalf("TagNote(%d): %v", id, err)
	}
	return n
}

func mustSetTagNote(t *testing.T, svc *Service, id int64, body string, links ...string) {
	t.Helper()
	if err := svc.SetTagNote(id, body, links); err != nil {
		t.Fatalf("SetTagNote(%d): %v", id, err)
	}
}

func TestSetTagNote_RoundTripsAndClears(t *testing.T) {
	database, svc := setupTestDB(t)
	tag := mustTag(t, svc, "ahoge", generalCategoryID(t, svc))

	mustSetTagNote(t, svc, tag.ID, "a strand of hair", "https://a.example/1", "-https://b.example/2")
	want := TagNote{Body: "a strand of hair", Links: []string{"https://a.example/1", "-https://b.example/2"}}
	if got := mustTagNote(t, svc, tag.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("TagNote = %+v, want %+v", got, want)
	}

	if err := svc.RenameTag(tag.ID, "ahoge_renamed"); err != nil {
		t.Fatal(err)
	}
	if got := mustTagNote(t, svc, tag.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("after a rename, TagNote = %+v", got)
	}

	mustSetTagNote(t, svc, tag.ID, "")
	if n := countRows(t, database, "tag_notes"); n != 0 {
		t.Errorf("an empty note and no links left %d rows", n)
	}
	if got := mustTagNote(t, svc, tag.ID); !reflect.DeepEqual(got, TagNote{}) {
		t.Errorf("cleared TagNote = %+v", got)
	}
}

func TestSetTagNote_RefusesAnAliasAndAnUnknownTag(t *testing.T) {
	_, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	canon := mustTag(t, svc, "canon", general)
	alias := mustCreateAlias(t, svc, "spelling", general, canon.ID)
	if err := svc.SetTagNote(alias.ID, "text", nil); !errors.Is(err, ErrAliasNote) {
		t.Errorf("alias: err = %v, want ErrAliasNote", err)
	}
	if err := svc.SetTagNote(999999, "text", nil); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("unknown id: err = %v, want ErrTagNotFound", err)
	}
}

func TestParseTagLinks(t *testing.T) {
	got, err := ParseTagLinks(" https://a.example \r\n\n-https://b.example\n- https://c.example\nhttps://b.example\n-https://a.example\nplain handle\n-\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example", "-https://b.example", "-https://c.example", "plain handle"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseTagLinks = %q, want %q", got, want)
	}

	long := strings.Repeat("髪", MaxTagLinkLen)
	if _, err := ParseTagLinks(long); err != nil {
		t.Errorf("a %d-character link: %v", MaxTagLinkLen, err)
	}
	if _, err := ParseTagLinks("-" + long + "x"); err == nil {
		t.Error("a link over the limit was accepted")
	}
	var many []string
	for i := range MaxTagLinks + 1 {
		many = append(many, fmt.Sprintf("https://a.example/%d", i))
	}
	if _, err := ParseTagLinks(strings.Join(many, "\n")); err == nil {
		t.Error("a list over the limit was accepted")
	}
}

func TestAliasingATagMovesItsNoteToTheCanonical(t *testing.T) {
	database, svc := setupTestDB(t)
	general := generalCategoryID(t, svc)
	canon := mustTag(t, svc, "kurobane_itsuki", general)

	old := mustTag(t, svc, "kurobane", general)
	mustSetTagNote(t, svc, old.ID, "same person?", "https://p.example/1")
	mustMerge(t, svc, old.ID, canon.ID)
	want := TagNote{Body: "same person?", Links: []string{"https://p.example/1"}}
	if got := mustTagNote(t, svc, canon.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("a canonical with no note got %+v, want %+v", got, want)
	}
	if n := countRows(t, database, "tag_notes WHERE tag_id = ?", old.ID); n != 0 {
		t.Errorf("the alias kept %d note rows", n)
	}

	second := mustTag(t, svc, "kurobane_i", general)
	mustSetTagNote(t, svc, second.ID, "signs as 黒羽", "-https://p.example/1", "https://x.example/k")
	mustMerge(t, svc, second.ID, canon.ID)
	both := mustTagNote(t, svc, canon.ID)
	want = TagNote{
		Body:  "same person?\n\nFrom kurobane_i:\nsigns as 黒羽",
		Links: []string{"https://p.example/1", "https://x.example/k"},
	}
	if !reflect.DeepEqual(both, want) {
		t.Errorf("two noted tags merged into %+v, want %+v", both, want)
	}

	twin := mustTag(t, svc, "kurobane_twin", general)
	mustSetTagNote(t, svc, twin.ID, both.Body)
	mustMerge(t, svc, twin.ID, canon.ID)
	if got := mustTagNote(t, svc, canon.ID); got.Body != both.Body {
		t.Errorf("an identical note was appended: %q", got.Body)
	}

	spare := mustTag(t, svc, "kurobane_old", general)
	mustSetTagNote(t, svc, spare.ID, "", "https://s.example/1")
	mustCreateAlias(t, svc, "kurobane_old", general, canon.ID)
	if got := mustTagNote(t, svc, canon.ID); !slices.Contains(got.Links, "https://s.example/1") {
		t.Errorf("Create alias over a zero-usage tag left its link behind: %q", got.Links)
	}

	artist, ok, err := CategoryIDByName(database, "artist")
	if err != nil || !ok {
		t.Fatalf("artist category: %v %v", ok, err)
	}
	mover := mustTag(t, svc, "kurobane_itsuki", artist)
	mustSetTagNote(t, svc, mover.ID, "moved here")
	merged, err := svc.ChangeTagCategoryMerge(mover.ID, general, false)
	if err != nil || !merged {
		t.Fatalf("ChangeTagCategoryMerge: merged=%v err=%v", merged, err)
	}
	if got := mustTagNote(t, svc, canon.ID); !strings.HasSuffix(got.Body, "From kurobane_itsuki:\nmoved here") {
		t.Errorf("a category change that merges dropped the note: %q", got.Body)
	}
}

func TestDeletingATagDropsItsNote(t *testing.T) {
	database, svc := setupTestDB(t)
	plain := mustTag(t, svc, "plain", generalCategoryID(t, svc))
	mustSetTagNote(t, svc, plain.ID, "a note")
	if err := svc.DeleteTag(plain.ID); err != nil {
		t.Fatal(err)
	}
	cat := mustCreateCategory(t, svc, "doomed", "#aabbcc")
	inCat := mustTag(t, svc, "in_doomed", cat.ID)
	mustSetTagNote(t, svc, inCat.ID, "another")
	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "delete_all", 0); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, "tag_notes"); n != 0 {
		t.Errorf("%d note rows outlived their tags", n)
	}

	var explicit int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE c.name = 'rating' AND t.name = 'explicit'`,
	).Scan(&explicit); err != nil {
		t.Fatal(err)
	}
	mustSetTagNote(t, svc, explicit, "what counts as explicit here")
	if err := svc.DeleteTag(explicit); err != nil {
		t.Fatal(err)
	}
	if got := mustTagNote(t, svc, explicit); got.Body == "" {
		t.Error("deleting a rating tag keeps its row, and its note with it")
	}
}
