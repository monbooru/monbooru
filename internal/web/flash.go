package web

import (
	"encoding/json"
	"net/http"
)

// A refusal follows the caller: an htmx call that swaps its reply gets a 200 inline flash (hxErr, externalErr) or a 409, which main.js swaps;
// one that swaps nothing gets the flash header (flashHeaderErr, never with a 409); a native form or fetch gets plain text (http.Error, externalErr).
// flashStatus does not branch on the request mode, so only htmx-only routes call it.
func flashStatus(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeInlineFlash(w, "err", msg)
}

func flashHeaderErr(w http.ResponseWriter, code int, msg string) {
	setFlashHeader(w, msg, "err", nil)
	flashStatus(w, code, msg)
}

func hxErr(w http.ResponseWriter, r *http.Request, htmxMsg, plainMsg string, code int) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, htmxMsg)
		return
	}
	http.Error(w, plainMsg, code)
}

func externalErr(w http.ResponseWriter, r *http.Request, msg string, code int) {
	hxErr(w, r, msg, msg, code)
}

func setDialogSavedTrigger(w http.ResponseWriter, event, dialogID string) {
	payload, _ := json.Marshal(map[string]any{event: map[string]any{"dialog": dialogID}})
	w.Header().Set("HX-Trigger", string(payload))
}
