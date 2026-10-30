package web

import (
	"net/http"
	"strings"
)

func parseFormOK(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		externalErr(w, r, "Bad form data: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func requiredFormFlash(w http.ResponseWriter, r *http.Request, field, msg string) (string, bool) {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return "", false
	}
	return v, true
}

func requiredFormExternal(w http.ResponseWriter, r *http.Request, field, msg string) (string, bool) {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		externalErr(w, r, msg, http.StatusBadRequest)
		return "", false
	}
	return v, true
}

var sameOrigin http.CrossOriginProtection

func isPluginMount(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, pluginMountPrefix) }

// A mounted plugin's forms carry no monbooru token, and with login off no
// session gates the mount either, so the CSRF check lets it by and only its
// origin is held.
func pluginMountOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPluginMount(r) && sameOrigin.Check(r) != nil {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
