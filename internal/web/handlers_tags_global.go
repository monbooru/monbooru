package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/globaltags"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type globalTagState struct {
	stalled atomic.Bool
	mu      sync.Mutex
	pulls   []string
}

func (s *Server) globalSides() []globaltags.Side {
	var sides []globaltags.Side
	for _, g := range s.set.All() {
		if g.DB != nil && g.TagSvc != nil {
			sides = append(sides, globaltags.Side{Name: g.Name, DB: g.DB, Tags: g.TagSvc})
		}
	}
	return sides
}

func sideNames(sides []globaltags.Side, except string) []string {
	var names []string
	for _, side := range sides {
		if side.Name != except {
			names = append(names, side.Name)
		}
	}
	slices.Sort(names)
	return names
}

func (s *Server) queueGlobalPull(name string) {
	s.global.mu.Lock()
	if !slices.Contains(s.global.pulls, name) {
		s.global.pulls = append(s.global.pulls, name)
	}
	s.global.mu.Unlock()
	s.applyGlobalChanges(false)
}

func (s *Server) takeGlobalPulls() []string {
	s.global.mu.Lock()
	defer s.global.mu.Unlock()
	pulls := s.global.pulls
	s.global.pulls = nil
	return pulls
}

func (s *Server) hasGlobalPulls() bool {
	s.global.mu.Lock()
	defer s.global.mu.Unlock()
	return len(s.global.pulls) > 0
}

func (s *Server) applyGlobalChanges(afterJob bool) {
	if afterJob && s.global.stalled.Load() {
		return
	}
	sides := s.globalSides()
	if len(sides) < 2 {
		s.takeGlobalPulls()
		return
	}
	if s.hasGlobalPulls() && !globaltags.Any(sides) {
		s.takeGlobalPulls()
	}
	if !s.hasGlobalPulls() && !globaltags.Pending(sides) {
		return
	}
	if err := s.jobs.Start(models.JobTypeTag, sideNames(sides, "")...); err != nil {
		return
	}
	go func(ctx context.Context) {
		s.jobs.Update(0, 0, "applying global tags…")
		var skipped []globaltags.Skip
		var err error
		for _, name := range s.takeGlobalPulls() {
			var skips []globaltags.Skip
			if skips, err = globaltags.Pull(ctx, sides, name); err != nil {
				break
			}
			skipped = append(skipped, skips...)
		}
		if err == nil {
			var skips []globaltags.Skip
			skips, err = globaltags.Push(ctx, sides)
			skipped = append(skipped, skips...)
		}
		s.global.stalled.Store(err != nil)
		_ = s.jobs.Settle(ctx, err, "Global tags: stopped, the rest is applied at the next change.",
			"Global tags applied to the other galleries."+skippedGlobal(skipped))
	}(s.jobs.Context())
}

func skippedGlobal(skipped []globaltags.Skip) string {
	if len(skipped) == 0 {
		return ""
	}
	first := skipped[0]
	out := fmt.Sprintf(" %d left out: %s in %s, %s", len(skipped), first.Tag, first.Gallery, first.Reason)
	if len(skipped) > 1 {
		out += fmt.Sprintf(", and %d more", len(skipped)-1)
	}
	return out + "."
}

func (s *Server) globalTagsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			s.applyGlobalChanges(false)
		}
	})
}

func (s *Server) makeTagsGlobalPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, err := s.resolveTagScope(r)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	sides := s.globalSides()
	from := s.activeGallery()
	if len(sides) < 2 {
		flashStatus(w, http.StatusBadRequest, "A global tag needs a second gallery.")
		return
	}
	merge := r.FormValue("merge") == "1"
	s.startIDsJob(w, models.JobTypeTag, ids, func(ids []int64) {
		ctx := s.jobs.Context()
		s.jobs.Update(0, 0, "making tags global…")
		rep, skipped, err := globaltags.MakeGlobal(ctx, sides, from, ids, merge)
		summary := ""
		if rep != nil {
			summary = fmt.Sprintf("Made %d tag%s global", len(rep.Global), plural(len(rep.Global)))
			if n := len(rep.ComesAlong); n > 0 {
				summary += fmt.Sprintf(", %d of them implied", n)
			}
			if n := localTags(rep.Local); n > 0 {
				summary += fmt.Sprintf("; %d stay%s local", n, map[bool]string{true: "s"}[n == 1])
			}
			summary += "." + skippedGlobal(skipped)
		}
		_ = s.jobs.Settle(ctx, err, "Make global: stopped. Run it again to finish.", summary)
	}, sideNames(sides, from)...)
}

const globalCheckListed = 12

type globalCheckData struct {
	Error      string
	Report     *globaltags.Report
	Changes    bool
	ComesAlong []globaltags.Key
	MoreAlong  int
	Local      []globaltags.Conflict
	MoreLocal  int
	LocalTags  int
	Mergeable  bool
	Merge      bool
}

func (s *Server) makeTagsGlobalCheck(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	data := globalCheckData{Merge: r.FormValue("merge") == "1"}
	ids, err := s.resolveTagScope(r)
	if err == nil {
		data.Report, err = globaltags.Check(s.globalSides(), s.activeGallery(), ids, data.Merge)
	}
	if err != nil {
		data.Error = err.Error()
		s.renderTemplate(w, "partials/tag_global_check.html", data)
		return
	}
	rep := data.Report
	for _, g := range rep.Galleries {
		data.Changes = data.Changes || g.Any()
	}
	data.ComesAlong, data.MoreAlong = rep.ComesAlong, 0
	if len(rep.ComesAlong) > globalCheckListed {
		data.ComesAlong, data.MoreAlong = rep.ComesAlong[:globalCheckListed], len(rep.ComesAlong)-globalCheckListed
	}
	data.Local = rep.Local
	if len(rep.Local) > globalCheckListed {
		data.Local, data.MoreLocal = rep.Local[:globalCheckListed], len(rep.Local)-globalCheckListed
	}
	data.LocalTags = localTags(rep.Local)
	for _, c := range rep.Local {
		data.Mergeable = data.Mergeable || c.Mergeable
	}
	s.renderTemplate(w, "partials/tag_global_check.html", data)
}

func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func globalCounts(c globaltags.Counts) string {
	var parts []string
	add := func(n int, one, many string) {
		if n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+models.Plural(n, one, many))
		}
	}
	add(c.Tags, "tag", "tags")
	add(c.Aliases, "alias", "aliases")
	add(c.Implications, "implication", "implications")
	add(c.Notes, "note", "notes")
	out := joinAnd(parts)
	if c.Merged > 0 {
		if out != "" {
			out += ", and "
		}
		out += strconv.Itoa(c.Merged) + " " + models.Plural(c.Merged, "tag", "tags") + " merged"
	}
	return out
}

func localTags(conflicts []globaltags.Conflict) int {
	seen := map[globaltags.Key]bool{}
	for _, c := range conflicts {
		seen[c.Tag] = true
	}
	return len(seen)
}

func (s *Server) makeTagLocalPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.active()
	tag, err := cx.TagSvc.GetTag(id)
	if err == nil {
		err = globaltags.MakeLocal(globaltags.Side{Name: cx.Name, DB: cx.DB, Tags: cx.TagSvc}, []int64{id})
	}
	var implied *globaltags.ErrImplied
	switch {
	case errors.Is(err, tags.ErrTagNotFound):
		flashHeaderErr(w, http.StatusNotFound, "Tag not found.")
		return
	case errors.As(err, &implied):
		flashHeaderErr(w, http.StatusConflict, implied.Error()+".")
		return
	case err != nil:
		flashHeaderErr(w, http.StatusInternalServerError, "Error: "+err.Error())
		return
	}
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/tags/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	setFlashHeader(w, tag.Name+" is local again. Every gallery keeps what it has.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}
