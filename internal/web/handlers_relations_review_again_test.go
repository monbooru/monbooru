package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/monbooru/monbooru/internal/relations"
)

func insertRawImage(t *testing.T, srv *Server, path, sha string) int64 {
	t.Helper()
	if sha == "" {
		sum := sha256.Sum256([]byte(path))
		sha = hex.EncodeToString(sum[:])
	}
	id, _ := mustExec(t, srv.db().Write,
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, source_type, origin)
		 VALUES (?, ?, '', 'png', 1024, 'image', 'test')`,
		sha, path).LastInsertId()
	return id
}

func TestReviewAgainPost(t *testing.T) {
	for _, c := range []struct {
		name, kind, table string
		add               func(svc *relations.Service, a, b int64) error
	}{
		{"duplicate", "duplicate", "dup_groups",
			func(svc *relations.Service, a, b int64) error { return svc.AddDuplicate(a, b) }},
		{"alternate", "alternate", "alt_groups",
			func(svc *relations.Service, a, b int64) error { return svc.AddAlternate(a, b) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t)
			a := insertRawImage(t, srv, "a.png", "")
			b := insertRawImage(t, srv, "b.png", "")
			if err := c.add(srv.active().RelationsSvc, a, b); err != nil {
				t.Fatal(err)
			}
			var gid int64
			if err := srv.db().Read.QueryRow(`SELECT id FROM ` + c.table + ` LIMIT 1`).Scan(&gid); err != nil {
				t.Fatal(err)
			}

			form := url.Values{
				"type":     {"review-again"},
				"kind":     {c.kind},
				"group_id": {strconv.FormatInt(gid, 10)},
			}
			w := postForm(t, srv, "/relations/remove", form)
			mustCode(t, w, http.StatusSeeOther)
			wantLoc := "/relations/session?a=" + strconv.FormatInt(a, 10) + "&b=" + strconv.FormatInt(b, 10)
			if loc := w.Header().Get("Location"); loc != wantLoc {
				t.Errorf("Location = %q, want %q", loc, wantLoc)
			}

			if n := countRows(t, srv.db(), c.table); n != 0 {
				t.Errorf("%s remaining = %d, want 0", c.table, n)
			}
			queued := countRows(t, srv.db(), "potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?", a, b)
			if queued != 1 {
				t.Errorf("potential_relation_pairs entries = %d, want 1", queued)
			}
		})
	}
}

func TestReviewAgainPost_DerivativeEdge(t *testing.T) {
	srv := newTestServer(t)
	source := insertRawImage(t, srv, "a.png", "")
	deriv := insertRawImage(t, srv, "b.png", "")
	if err := srv.active().RelationsSvc.AddDerivativeEdge(source, deriv); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"type": {"review-again"},
		"kind": {"derivative"},
		"a":    {strconv.FormatInt(source, 10)},
		"b":    {strconv.FormatInt(deriv, 10)},
	}
	w := postForm(t, srv, "/relations/remove", form)
	mustCode(t, w, http.StatusSeeOther)
	wantLoc := "/relations/session?a=" + strconv.FormatInt(source, 10) + "&b=" + strconv.FormatInt(deriv, 10)
	if loc := w.Header().Get("Location"); loc != wantLoc {
		t.Errorf("Location = %q, want %q", loc, wantLoc)
	}

	n := countRows(t, srv.db(), "derivative_edges")
	if n != 0 {
		t.Errorf("derivative_edges remaining = %d, want 0", n)
	}
	queued := countRows(t, srv.db(), "potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?", source, deriv)
	if queued != 1 {
		t.Errorf("potential_relation_pairs entries = %d, want 1", queued)
	}
}

func TestReviewAgainPost_UnknownKind(t *testing.T) {
	srv := newTestServer(t)
	form := url.Values{
		"type":     {"review-again"},
		"kind":     {"nonsense"},
		"group_id": {"1"},
	}
	w := postForm(t, srv, "/relations/remove", form)
	mustCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "Unknown review-again kind")
}

func TestReviewAgainPost_RefusesNonPairGroup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		build      func(t *testing.T, srv *Server) int64
		wantGroups int
	}{
		{"dissolved before the click", func(t *testing.T, srv *Server) int64 {
			a := insertRawImage(t, srv, "a.png", "")
			b := insertRawImage(t, srv, "b.png", "")
			if err := srv.active().RelationsSvc.AddAlternate(a, b); err != nil {
				t.Fatal(err)
			}
			gid := firstAltGroupID(t, srv)
			if err := srv.active().RelationsSvc.DissolveAltGroup(gid); err != nil {
				t.Fatal(err)
			}
			return gid
		}, 0},
		{"wider than a pair", func(t *testing.T, srv *Server) int64 {
			a := insertRawImage(t, srv, "a.png", "")
			b := insertRawImage(t, srv, "b.png", "")
			c := insertRawImage(t, srv, "c.png", "")
			if err := srv.active().RelationsSvc.AddAlternate(a, b); err != nil {
				t.Fatal(err)
			}
			if err := srv.active().RelationsSvc.AddAlternate(b, c); err != nil {
				t.Fatal(err)
			}
			return firstAltGroupID(t, srv)
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			gid := tc.build(t, srv)

			form := url.Values{
				"type":     {"review-again"},
				"kind":     {"alternate"},
				"group_id": {strconv.FormatInt(gid, 10)},
			}
			w := postForm(t, srv, "/relations/remove", form)

			mustCode(t, w, http.StatusOK)
			wantIn(t, w.Body.String(), "Group must have exactly two members")
			groups := countRows(t, srv.db(), "alt_groups")
			if groups != tc.wantGroups {
				t.Errorf("alt_groups = %d, want %d", groups, tc.wantGroups)
			}
			queued := countRows(t, srv.db(), "potential_relation_pairs")
			if queued != 0 {
				t.Errorf("potential_relation_pairs = %d, want 0", queued)
			}
		})
	}
}

func firstAltGroupID(t *testing.T, srv *Server) int64 {
	t.Helper()
	var gid int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM alt_groups LIMIT 1`).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	return gid
}
