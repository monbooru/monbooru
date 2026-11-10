package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestDeleteAlias_RemovesTheCopyAndRefusesTheCanonical(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "kept.png", 10, 10)
	canonical := canonicalPathOf(t, srv, id)
	pathID, aliasPath := seedAliasCopy(t, srv, id, "spare.png")

	del := func(pathID int64) *httptest.ResponseRecorder {
		w := deleteWithCSRF(t, srv, fmt.Sprintf("/images/%d/aliases/%d", id, pathID))
		return w
	}

	var canonicalPathID int64
	if err := srv.db().Read.QueryRow(
		`SELECT id FROM image_paths WHERE image_id = ? AND is_canonical = 1`, id,
	).Scan(&canonicalPathID); err != nil {
		t.Fatal(err)
	}
	refused := del(canonicalPathID)
	mustCode(t, refused, http.StatusBadRequest)
	wantIn(t, refused.Header().Get("HX-Trigger"), `"action-flash":{"kind":"err","text":"The canonical path cannot be deleted."}`)
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("the refused delete removed the canonical file anyway: %v", err)
	}

	w := del(pathID)
	mustCode(t, w, http.StatusOK)
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q, want empty so the swap removes the row", body)
	}
	if _, err := os.Stat(aliasPath); !os.IsNotExist(err) {
		t.Error("the alias file is still on disk")
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("the canonical file went with the alias: %v", err)
	}
	if got := canonicalPathOf(t, srv, id); got != canonical {
		t.Errorf("canonical_path = %q, want it unchanged at %q", got, canonical)
	}
}
