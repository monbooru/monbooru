package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestTagNote_SavedNoteAndLinksRenderOnTheTagPage(t *testing.T) {
	srv := newTestServer(t)
	general := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "ahoge", general)
	mustTag(t, srv.tagSvc(), "antenna_hair", general)
	page := fmt.Sprintf("/tags/%d", tag.ID)

	empty := do(srv, "GET", page).Body.String()
	wantIn(t, empty, "[+ add note]")
	wantIn(t, empty, "[+ add links]")
	note, usage := strings.Index(empty, `id="tag-note"`), strings.Index(empty, page+`/usage"`)
	if note < 0 || usage < 0 || note > usage {
		t.Errorf("the note section (at %d) should sit above the usage panel (at %d)", note, usage)
	}

	w := postHTMXForm(t, srv, page+"/note", url.Values{
		"note":  {"[b]one strand[/b], see [tag]antenna_hair[/tag]"},
		"links": {"https://www.pixiv.net/users/4461829/\n-https://kurobane-i.tumblr.com/\npixiv id 4461829"},
	})
	mustCode(t, w, http.StatusNoContent)
	wantIn(t, w.Header().Get("HX-Refresh"), "true")

	body := do(srv, "GET", page).Body.String()
	for _, want := range []string{
		"<strong>one strand</strong>",
		`href="/?q=antenna_hair"`,
		`href="https://www.pixiv.net/users/4461829/"`,
		">pixiv.net/users/4461829<",
		"<s>kurobane-i.tumblr.com</s>",
		"dead link: https://kurobane-i.tumblr.com/",
		"pixiv id 4461829",
		"[edit]",
	} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, "[+ add links]")

	mustCode(t, postHTMXForm(t, srv, page+"/note", url.Values{"note": {""}, "links": {""}}), http.StatusNoContent)
	wantIn(t, do(srv, "GET", page).Body.String(), "[+ add note]")
}

func TestTagNote_RefusalsLeaveTheNoteAlone(t *testing.T) {
	srv := newTestServer(t)
	general := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "refused", general)
	alias, err := srv.tagSvc().CreateAlias("refused_alias", general, tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	links := make([]string, 101)
	for i := range links {
		links[i] = fmt.Sprintf("https://a.example/%d", i)
	}
	for _, c := range []struct {
		path string
		form url.Values
		want string
	}{
		{fmt.Sprintf("/tags/%d/note", tag.ID), url.Values{"note": {strings.Repeat("x", 10001)}}, "note too long (max 10000 chars)"},
		{fmt.Sprintf("/tags/%d/note", tag.ID), url.Values{"links": {strings.Join(links, "\n")}}, "too many links (max 100)"},
		{fmt.Sprintf("/tags/%d/note", tag.ID), url.Values{"links": {"https://a.example/" + strings.Repeat("x", 2048)}}, "link too long (max 2048 chars)"},
		{fmt.Sprintf("/tags/%d/note", alias.ID), url.Values{"note": {"text"}}, "an alias carries no note"},
	} {
		w := postHTMXForm(t, srv, c.path, c.form)
		mustCode(t, w, http.StatusOK)
		wantIn(t, w.Body.String(), c.want)
	}
	mustCode(t, postForm(t, srv, "/tags/999999/note", url.Values{"note": {"text"}}), http.StatusNotFound)
	gone := postHTMXForm(t, srv, "/tags/999999/note", url.Values{"note": {"text"}})
	mustCode(t, gone, http.StatusOK)
	wantIn(t, gone.Body.String(), "Tag not found.")
	if n, _ := srv.tagSvc().TagNote(tag.ID); n.Body != "" || len(n.Links) != 0 {
		t.Errorf("a refused save stored %+v", n)
	}
}

func TestTagNote_AliasPageHasNoNoteSection(t *testing.T) {
	srv := newTestServer(t)
	general := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "canon_note", general)
	alias, err := srv.tagSvc().CreateAlias("alias_note", general, tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantIn(t, do(srv, "GET", fmt.Sprintf("/tags/%d", tag.ID)).Body.String(), `id="tag-note-dialog"`)
	body := do(srv, "GET", fmt.Sprintf("/tags/%d", alias.ID)).Body.String()
	wantNotIn(t, body, `id="tag-note"`)
	wantNotIn(t, body, `id="tag-note-dialog"`)
}

func TestTagNote_PreviewRendersTheMarkup(t *testing.T) {
	srv := newTestServer(t)
	tag := mustTag(t, srv.tagSvc(), "previewed", srv.active().GeneralCategoryID)
	w := postHTMXForm(t, srv, fmt.Sprintf("/tags/%d/markup/preview", tag.ID), url.Values{"body": {"[i]draft[/i]"}})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "<em>draft</em>")
}

func TestLinkLabel(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.pixiv.net/users/4461829":   "pixiv.net/users/4461829",
		"https://x.com/kurobane_i/":             "x.com/kurobane_i",
		"https://www.pixiv.net/member.php?id=5": "pixiv.net/member.php?id=5",
		"https://example.com":                   "example.com",
		"not a url":                             "not a url",
	} {
		if got := linkLabel(raw); got != want {
			t.Errorf("linkLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestDeleteTag_AMissingTagReachesTheFlashHeader(t *testing.T) {
	srv := newTestServer(t)
	w := deleteWithCSRF(t, srv, "/tags/999999")
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Header().Get("HX-Trigger"), `"action-flash":{"kind":"err","text":"Tag not found."}`)
}
