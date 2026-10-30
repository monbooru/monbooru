package web

import (
	"context"
	"net/http"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// others are galleries the job writes besides the active one.
func (s *Server) startJob(w http.ResponseWriter, jobType string, others ...string) bool {
	if err := s.jobs.Start(jobType, append([]string{s.activeGallery()}, others...)...); err != nil {
		flashStatus(w, http.StatusConflict, "A job is already running.")
		return false
	}
	return true
}

// reserveJob holds the lane so writes can go first and the job start only
// once there is work for it; nil means the refusal is written. The caller
// defers the Release.
func (s *Server) reserveJob(w http.ResponseWriter) *jobs.Reservation {
	res, err := s.jobs.Reserve()
	if err != nil {
		flashStatus(w, http.StatusConflict, "A job is already running.")
		return nil
	}
	return res
}

func (s *Server) startReserved(res *jobs.Reservation, jobType string) bool {
	if err := res.Start(jobType, s.activeGallery()); err != nil {
		logx.Warnf("start %s job: %v", jobType, err)
		return false
	}
	return true
}

// goJobWork runs the started job's work once a watcher ingest already running
// in the active gallery has ended, when the job is one the watcher pauses for.
func (s *Server) goJobWork(jobType string, work func(ctx context.Context)) {
	cx := s.active()
	go func(ctx context.Context) {
		if gallery.SuppressesIngest(jobType) {
			cx.WaitIngests(ctx)
		}
		work(ctx)
	}(s.jobs.Context())
}

// goJob answers with the started flash once work runs on the job context.
func (s *Server) goJob(w http.ResponseWriter, jobType, started string, work func(ctx context.Context)) {
	if !s.startJob(w, jobType) {
		return
	}
	s.goJobWork(jobType, work)
	writeInlineFlash(w, "ok", started)
}

// No ids answers 202 with nothing started.
func (s *Server) startIDsJob(w http.ResponseWriter, jobType string, ids []int64, run func([]int64), others ...string) {
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, jobType, others...) {
		return
	}
	s.goJobWork(jobType, func(context.Context) { run(ids) })
	w.WriteHeader(http.StatusAccepted)
}
