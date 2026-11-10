package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/monbooru/monbooru/internal/gallery"
)

func TestFindRelationPairsPost_ReplaceNeedsTheConfirm(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "pair_a.png", 10, 10)
	b := seedImage(t, srv, "pair_b.png", 11, 11)
	queueRow(t, srv.db(), a, b, 2)

	w := postForm(t, srv, "/relations/find-pairs", url.Values{"replace": {"true"}})
	wantCode(t, w, http.StatusBadRequest)
	wantIn(t, w.Body.String(), "replace=true requires confirm=REBUILD.")
	if n := countRows(t, srv.db(), "potential_relation_pairs"); n != 1 {
		t.Errorf("queue holds %d row(s) after a refused rebuild, want the 1 it had", n)
	}
	if srv.jobs.IsRunning() {
		t.Error("the refused rebuild took the job lane")
	}
}

func TestFindRelationPairsPost_ReplaceDropsTheSkipDecisions(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "pair_a.png", 10, 10)
	b := seedImage(t, srv, "pair_b.png", 40, 40)
	queueRow(t, srv.db(), a, b, 2)
	mustExec(t, srv.db().Write, `UPDATE potential_relation_pairs SET skipped_at = datetime('now')`)

	w := postForm(t, srv, "/relations/find-pairs", url.Values{
		"replace": {"true"}, "confirm": {"REBUILD"},
	})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), "Find-pairs started.")
	awaitJobsDrain(t, srv)

	if st := srv.jobs.Get(); st.Error != "" {
		t.Fatalf("the rebuild failed: %s", st.Error)
	}
	if n := countRows(t, srv.db(), "potential_relation_pairs WHERE skipped_at IS NOT NULL"); n != 0 {
		t.Errorf("%d skipped row(s) survived the rebuild", n)
	}
}

func TestFindPairsJob_DropsThePhashMissingCount(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "unhashed.png", 32, 32)
	cx := srv.active()
	mustExec(t, srv.db().Write, `UPDATE images SET phash = NULL`)
	cx.InvalidateCaches()
	if n, err := cx.PhashMissingUnder(nil); err != nil || n != 1 {
		t.Fatalf("test premise broken: %d missing (%v), want 1", n, err)
	}

	mustCode(t, postForm(t, srv, "/relations/find-pairs", url.Values{}), http.StatusOK)
	awaitJobsDrain(t, srv)

	if n, err := cx.PhashMissingUnder(nil); err != nil || n != 0 {
		t.Errorf("phash missing after find-pairs = %d (%v), want 0", n, err)
	}
}

func TestRecomputePhashPost_RestoresTheStoredHash(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "phash_subject.png", 32, 32)
	cx := srv.active()
	if _, err := gallery.RecomputeAndStorePhash(t.Context(), cx.DB, id, cx.ThumbnailsPath); err != nil {
		t.Fatalf("seed the phash: %v", err)
	}
	var want sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if !want.Valid {
		t.Fatal("test premise broken: the row has no phash to restore")
	}
	mustExec(t, srv.db().Write, `UPDATE images SET phash = NULL WHERE id = ?`, id)

	w := postForm(t, srv, fmt.Sprintf("/relations/phash/%d/recompute", id), url.Values{})
	mustCode(t, w, http.StatusSeeOther)

	var got sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("phash = %v, want the %v Generate produces for those thumbnail bytes", got, want)
	}
}

func TestRecomputePhashPost_KeepsTheHashWhenTheThumbnailIsGone(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "phash_gone.png", 32, 32)
	cx := srv.active()
	if _, err := gallery.RecomputeAndStorePhash(t.Context(), cx.DB, id, cx.ThumbnailsPath); err != nil {
		t.Fatalf("seed the phash: %v", err)
	}
	var want sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gallery.ThumbnailPath(cx.ThumbnailsPath, id)); err != nil {
		t.Fatal(err)
	}

	w := postForm(t, srv, fmt.Sprintf("/relations/phash/%d/recompute", id), url.Values{})
	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Header().Get("HX-Trigger"), "phash recompute failed")

	var got sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("phash = %v, want the previous %v kept", got, want)
	}
}

func TestSettingsRelationsPost_FlashesTheSavedValues(t *testing.T) {
	srv := newTestServer(t)

	w := postForm(t, srv, "/settings/relations", url.Values{
		"default_distance":      {"4"},
		"default_session_order": {"random"},
		"tag_pairs":             {"on"},
		"tag_pair_threshold":    {"90"},
	})

	mustCode(t, w, http.StatusOK)
	wantIn(t, w.Body.String(), `<div class="flash flash-ok">Saved. distance=4 order=random tag pairs=true (90%)</div>`)
}
