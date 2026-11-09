package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) findRelationPairsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		externalErr(w, r, "No active gallery.", http.StatusServiceUnavailable)
		return
	}
	s.cfgMu.RLock()
	distance := s.cfg.Relations.DefaultDistance
	tagPairs := s.cfg.Relations.TagPairs
	tagPairThreshold := s.cfg.Relations.TagPairThreshold
	s.cfgMu.RUnlock()
	if n, err := strconv.Atoi(r.FormValue("distance")); err == nil && n >= 0 && n <= config.MaxPhashDistance {
		distance = n
	}
	replace := r.FormValue("replace") == "true"
	// A typed confirm, so a bare replace=true from curl or a bookmark
	// cannot wipe the queue.
	if replace && r.FormValue("confirm") != "REBUILD" {
		externalErr(w, r, "replace=true requires confirm=REBUILD.", http.StatusBadRequest)
		return
	}

	opts := relations.FindPairsOptions{
		Distance:         distance,
		Replace:          replace,
		TagPairs:         tagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(tagPairThreshold),
	}
	s.goJob(w, models.JobTypeRelations, "Find-pairs started.", func(ctx context.Context) {
		added, err := cx.FindPairs(ctx, opts, func(processed, total int, phase string) {
			s.jobs.Update(processed, total, fmt.Sprintf("find-pairs: %s", phase))
		})
		_ = s.jobs.Settle(ctx, err,
			fmt.Sprintf("find-pairs cancelled (%d added)", added),
			fmt.Sprintf("find-pairs added %d candidate%s.", added, plural(added)))
	})
}

func (s *Server) resetSkippedPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		externalErr(w, r, "No active gallery.", http.StatusServiceUnavailable)
		return
	}
	n, err := cx.RelationsSvc.ResetSkipped(r.Context())
	if err != nil {
		logx.Warnf("reset skipped: %v", err)
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Reset %d skipped pair%s.", n, plural(int(n))))
}
