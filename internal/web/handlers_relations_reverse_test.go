package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestReverseRelation_FlipsAVersionEdgeInPlace(t *testing.T) {
	srv := newTestServer(t)
	parent := seedImage(t, srv, "v1.png", 10, 10)
	child := seedImage(t, srv, "v2.png", 11, 11)
	cx := srv.active()
	if err := cx.RelationsSvc.AddVersionEdge(parent, child); err != nil {
		t.Fatalf("AddVersionEdge: %v", err)
	}

	w := postForm(t, srv, "/relations/reverse", url.Values{
		"type": {"version"},
		"a":    {strconv.FormatInt(parent, 10)},
		"b":    {strconv.FormatInt(child, 10)},
	})
	mustCode(t, w, http.StatusOK)

	var gotParent, gotChild int64
	edges := countRows(t, srv.db(), "version_edges")
	if edges != 1 {
		t.Fatalf("version_edges rows = %d, want the one edge flipped in place", edges)
	}
	if err := srv.db().Read.QueryRow(
		`SELECT parent_image_id, child_image_id FROM version_edges`).Scan(&gotParent, &gotChild); err != nil {
		t.Fatal(err)
	}
	if gotParent != child || gotChild != parent {
		t.Errorf("edge = %d -> %d, want %d -> %d", gotParent, gotChild, child, parent)
	}
}

func TestReverseRelation_RefusesAnUnknownType(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "r1.png", 10, 10)
	b := seedImage(t, srv, "r2.png", 11, 11)

	w := postForm(t, srv, "/relations/reverse", url.Values{
		"type": {"duplicate"},
		"a":    {strconv.FormatInt(a, 10)},
		"b":    {strconv.FormatInt(b, 10)},
	})
	wantCode(t, w, http.StatusBadRequest)
}

func TestImageRelationsPage_OffersReverseOnlyOnATwoImageChain(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "c1.png", 10, 10)
	b := seedImage(t, srv, "c2.png", 11, 11)
	c := seedImage(t, srv, "c3.png", 12, 12)
	svc := srv.active().RelationsSvc
	if err := svc.AddVersionEdge(a, b); err != nil {
		t.Fatal(err)
	}
	reverse := `hx-post="/relations/reverse"`
	page := func(id int64) string {
		t.Helper()
		w := do(srv, "GET", "/images/"+strconv.FormatInt(id, 10)+"/relations")
		mustCode(t, w, http.StatusOK)
		return w.Body.String()
	}
	if !strings.Contains(page(b), reverse) {
		t.Error("a two-image chain offers no [reverse]")
	}
	if err := svc.AddVersionEdge(b, c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a, b, c} {
		if strings.Contains(page(id), reverse) {
			t.Errorf("image %d of a three-image chain offers [reverse]", id)
		}
	}
}
