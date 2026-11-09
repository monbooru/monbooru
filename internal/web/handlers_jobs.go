package web

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/models"
)

func (s *Server) jobDismissPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Dismiss()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobCancelPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

type jobStatusView struct {
	Job       *models.JobState
	Downloads []*activeDownload
	Mascot    string
	PairKey   string
}

func (s *Server) footerCountsHandler(w http.ResponseWriter, r *http.Request) {
	cx := s.active()
	if cx == nil {
		return
	}
	visible, _ := cx.VisibleCount()
	tagCount, _ := cx.TagCount()
	collections, _ := cx.CollectionsCount()
	s.renderTemplate(w, "footer_counts", map[string]int{"VisibleCount": visible, "TagCount": tagCount, "CollectionsCount": collections})
}

func (s *Server) jobStatusHandler(w http.ResponseWriter, r *http.Request) {
	// MarkViewed first: a finished job's first render starts its dismiss timer.
	s.jobs.MarkViewed()
	n := s.pairNotice()
	s.renderTemplate(w, "partials/job_status.html", jobStatusView{Job: s.jobs.Get(), Downloads: s.downloads.snapshot(), Mascot: themeMascots(s.activeTheme())["sparkle-head"], PairKey: n.Key()})
	// The poll sends the key it last rendered, so an unchanged notice is
	// not swapped under a pointer reading its title.
	if r.FormValue("pair") != n.Key() {
		n.OOB = true
		s.renderTemplate(w, "webui/pair_notice", n)
	}
}

func (s *Server) syncTrigger(w http.ResponseWriter, r *http.Request) {
	if cx := s.active(); cx == nil || cx.Degraded {
		hxRefuse(w, r, http.StatusServiceUnavailable, "Sync unavailable: gallery path is unreadable.")
		return
	}
	if !s.startJob(w, models.JobTypeSync) {
		return
	}
	// Resolved under the request's RLock; the gallery mutations refuse
	// while a job runs, so cx stays open for the sync.
	cx := s.active()
	maxFileSizeMB := s.maxFileSizeMB()
	go func() {
		ctx := s.jobs.Context()
		result, err := cx.Sync(ctx, maxFileSizeMB, s.ingestNaming(cx.Name), s.jobs.Update, s.jobs.NoteChange)
		_ = s.jobs.Settle(ctx, err, "sync cancelled", result.Summary())
	}()

	redirectTo := sameOriginReferer(r)
	if isHTMXRequest(r) {
		w.Header().Set("HX-Trigger", "syncStarted")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}
