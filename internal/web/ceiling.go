package web

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/tags"
)

const ratingCeilingCookieName = "monbooru_rating_ceiling"

func resolveCeiling(r *http.Request, cx *library.Gallery) *Ceiling {
	return library.NewCeiling(readRatingCookie(r), cx)
}

// The value reaches the search AST: anything outside the closed set is dropped.
func readRatingCookie(r *http.Request) string {
	if v := session.FromCookie(r, ratingCeilingCookieName); tags.IsCanonicalRating(v) {
		return v
	}
	return ""
}

// The top level is no ceiling at all, so it clears the cookie.
func writeRatingCookie(w http.ResponseWriter, r *http.Request, level string) {
	if rank := contract.RatingRank(level); rank >= 0 && rank < len(contract.RatingLevels)-1 {
		setPrefCookie(w, r, ratingCeilingCookieName, level)
		return
	}
	setPrefCookie(w, r, ratingCeilingCookieName, "")
}
