package api

import (
	"net/http"
	"time"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

type jobResponse struct {
	Type       string     `json:"type"`
	Processed  int        `json:"processed"`
	Total      int        `json:"total"`
	Message    string     `json:"message,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	Error      string     `json:"error,omitempty"`
	Partial    bool       `json:"partial"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Galleries  []string   `json:"galleries"`
}

type currentJobResponse struct {
	Running  bool         `json:"running"`
	Finished uint64       `json:"finished"`
	Queued   int          `json:"queued"`
	Job      *jobResponse `json:"job,omitempty"`
}

func (h *Handler) currentJob(w http.ResponseWriter, _ *http.Request) {
	h.autotagMu.Lock()
	queued := len(h.autotagQueue)
	h.autotagMu.Unlock()
	out := currentJobResponse{Running: h.jobs.IsRunning(), Finished: h.jobs.Finished(), Queued: queued}
	if st := h.jobs.Get(); st != nil {
		out.Job = &jobResponse{
			Type:       st.JobType,
			Processed:  st.Processed,
			Total:      st.Total,
			Message:    st.Message,
			Summary:    st.Summary,
			Error:      st.Error,
			Partial:    st.Partial,
			StartedAt:  st.StartedAt,
			FinishedAt: st.FinishedAt,
			Galleries:  append([]string{}, st.Galleries...),
		}
	}
	contract.WriteJSON(w, http.StatusOK, out)
}
