package web

import (
	"net/http"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type lookupView struct {
	ImageID int64
	// Candidate excludes an archive: no booru or repository indexes its
	// own hash, only its pages'.
	Candidate bool
	Backends  []lookupBackendView
}

type lookupBackendView struct {
	Backend string
	Label   string
	// Enabled is the image's own opt-in; ScheduleOn is the phase in Settings.
	Enabled    bool
	ScheduleOn bool
	GalleryOff bool
	Exhausted  bool
	Attempts   int
	QueuedAt   time.Time
	LastAt     time.Time
	LastResult string
	NextDueAt  time.Time
}

func (v lookupBackendView) Tried() bool { return !v.LastAt.IsZero() || !v.QueuedAt.IsZero() }

// NextDueAt outlives the schedule, gallery and image opt-outs.
func (v lookupBackendView) Retrying() bool {
	return v.ScheduleOn && !v.GalleryOff && v.Enabled && !v.NextDueAt.IsZero()
}

type lookupStatus struct {
	Text string
	Tone string
}

// Case order matters: an in-flight attempt outranks a spent ladder, and both
// outrank the due date.
func (v lookupBackendView) Status() lookupStatus {
	switch {
	case !v.ScheduleOn:
		return lookupStatus{Text: "schedule off"}
	case v.GalleryOff:
		return lookupStatus{Text: "off for this gallery"}
	case !v.Enabled:
		return lookupStatus{Text: "not scheduled"}
	case !v.QueuedAt.IsZero():
		return lookupStatus{Text: "queued", Tone: "queued"}
	case v.Exhausted:
		return lookupStatus{Text: "nothing found", Tone: "verdict"}
	case v.LastResult == lookup.ResultHit:
		return lookupStatus{Text: "matched " + localDay(v.LastAt), Tone: "hit"}
	case v.NextDueAt.After(time.Now()):
		return lookupStatus{Text: "due " + localDay(v.NextDueAt)}
	}
	// Not "due tonight": a budget-bound run reaches it only once the
	// backlog ahead of it clears.
	return lookupStatus{Text: "due now"}
}

func (s *Server) lookupViewFor(cx *library.Gallery, imageID int64) lookupView {
	v := lookupView{ImageID: imageID}
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()

	st, err := lookup.StandingOf(cx.DB, imageID)
	if err != nil {
		logx.Warnf("lookup view for image %d: %v", imageID, err)
		return v
	}
	v.Candidate = st.Candidate

	rows, err := lookup.ForImage(cx.DB, imageID)
	if err != nil {
		logx.Warnf("lookup view for image %d: %v", imageID, err)
		return v
	}
	for _, b := range []struct {
		backend, label, action string
		optedIn                bool
	}{
		{contract.LookupPTR, "PTR", config.ActionLookupPTR, st.PTR},
		{contract.LookupBooru, "boorus", config.ActionLookupBooru, st.Booru},
	} {
		r := rows[b.backend]
		on := sched.Enabled(b.action)
		v.Backends = append(v.Backends, lookupBackendView{
			Backend: b.backend, Label: b.label, Enabled: b.optedIn,
			ScheduleOn: on, GalleryOff: on && !sched.RunsOn(b.action, cx.Name),
			Exhausted: r.Exhausted(), Attempts: r.Attempts, QueuedAt: r.QueuedAt,
			LastAt: r.LastAt, LastResult: r.LastResult, NextDueAt: r.NextDueAt,
		})
	}
	return v
}

func scheduledLookupBackend(r *http.Request) string {
	if r.FormValue("backend") == contract.LookupPTR {
		return contract.LookupPTR
	}
	return contract.LookupBooru
}

func (s *Server) scheduledLookupPost(w http.ResponseWriter, r *http.Request) {
	id, cx, _, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	if err := cx.SetScheduledLookup([]int64{id}, []string{scheduledLookupBackend(r)}, r.FormValue("on") == "1"); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderScheduledLookup(w, r, cx, id)
}

func (s *Server) scheduledLookupResetPost(w http.ResponseWriter, r *http.Request) {
	id, cx, _, ok := s.imageAndGallery(w, r)
	if !ok {
		return
	}
	if err := cx.Edit(func(d *db.DB) error { return lookup.Reset(d, id, scheduledLookupBackend(r), time.Now()) }); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderScheduledLookup(w, r, cx, id)
}

func (s *Server) renderScheduledLookup(w http.ResponseWriter, r *http.Request, cx *library.Gallery, id int64) {
	view := s.lookupViewFor(cx, id)
	s.renderTemplate(w, "partials/scheduled_lookup.html", map[string]any{
		"Lookup": view,
	})
	// The history, in the other column, carries [look again], so it
	// re-renders with the ladder.
	s.renderTemplate(w, "partials/lookup_history.html", map[string]any{
		"Lookup": view, "OOB": true,
	})
}
