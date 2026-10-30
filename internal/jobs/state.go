// Package jobs runs one background job at a time: two jobs writing the
// same SQLite file would only contend.
package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/models"
)

var ErrJobRunning = errors.New("a job is already running")

const (
	dismissDelay       = 30 * time.Second
	viewedDismissDelay = 6 * time.Second
)

type Manager struct {
	mu     sync.Mutex
	state  *models.JobState
	timer  *time.Timer
	ctx    context.Context
	cancel context.CancelFunc
	// The exclusive lane: Start refuses while it is held, also where no
	// job runs, such as between a scheduled run's phases.
	lane     *Reservation
	viewed   bool
	finished uint64
}

// Reservation is a hold on the job lane; only its own Release frees it.
type Reservation struct {
	m         *Manager
	cancelled bool
}

func NewManager() *Manager { return &Manager{} }

func (m *Manager) clearStateLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.ctx, m.cancel = nil, nil
	m.state = nil
	m.viewed = false
}

func (m *Manager) Start(jobType string, galleries ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lane != nil {
		return ErrJobRunning
	}
	return m.startLocked(jobType, galleries)
}

func (m *Manager) startLocked(jobType string, galleries []string) error {
	if m.state != nil && m.state.Running {
		return ErrJobRunning
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.viewed = false
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.state = &models.JobState{
		Running:   true,
		JobType:   jobType,
		Galleries: galleries,
		StartedAt: time.Now().UTC(),
		Message:   "Starting...",
	}
	return nil
}

// Reserve makes Start refuse until the returned Reservation's Release,
// which the caller must defer.
func (m *Manager) Reserve() (*Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lane != nil || (m.state != nil && m.state.Running) {
		return nil, ErrJobRunning
	}
	m.lane = &Reservation{m: m}
	return m.lane, nil
}

func (r *Reservation) Release() {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.lane == r {
		r.m.lane = nil
	}
}

func (r *Reservation) Cancelled() bool {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return r.cancelled
}

func (r *Reservation) Start(jobType string, galleries ...string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.lane != r {
		return ErrJobRunning
	}
	return r.m.startLocked(jobType, galleries)
}

func (m *Manager) Context() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return context.Background()
	}
	return m.ctx
}

// Cancel also marks the lane's reservation cancelled, so a scheduled run
// stops at its next phase; the lane stays held until its holder releases it.
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	if m.lane != nil {
		m.lane.cancelled = true
	}
}

func (m *Manager) Update(processed, total int, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	m.state.Processed = processed
	m.state.Total = total
	m.state.Message = message
}

func (m *Manager) Complete(summary string) {
	m.end(func(st *models.JobState) { st.Summary, st.Message = summary, "" })
}

func (m *Manager) CompletePartial(summary string) {
	m.end(func(st *models.JobState) { st.Summary, st.Message, st.Partial = summary, "", true })
}

func (m *Manager) Fail(errMsg string) {
	m.end(func(st *models.JobState) { st.Error = errMsg })
}

func (m *Manager) end(set func(*models.JobState)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	now := time.Now().UTC()
	m.state.Running = false
	m.state.FinishedAt = &now
	set(m.state)
	m.finished++
	m.armDismiss(dismissDelay)
}

func (m *Manager) Get() *models.JobState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return nil
	}
	copy := *m.state
	return &copy
}

// IsRunning is also true while the lane is reserved.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lane != nil || (m.state != nil && m.state.Running)
}

// WaitIdle reports false when the lane is still held after d.
func (m *Manager) WaitIdle(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for m.IsRunning() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// Finished only grows, so a watcher sampling less often than the
// auto-dismiss still sees a job end.
func (m *Manager) Finished() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.finished
}

// MarkViewed shortens the auto-dismiss, except after a failure: any open
// tab's poll counts as a view, and the status bar is the only report the
// error gets.
func (m *Manager) MarkViewed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.Running || m.viewed || m.state.Error != "" {
		return
	}
	m.viewed = true
	m.armDismiss(viewedDismissDelay)
}

func (m *Manager) Dismiss() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.Running {
		return
	}
	m.clearStateLocked()
}

func (m *Manager) NoteChange() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.Running {
		m.state.Changes++
	}
}

func (m *Manager) SetWatcherMessage(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.Running {
		m.state.Changes++
		return
	}
	if m.state != nil && m.state.Error != "" {
		return
	}
	now := time.Now().UTC()
	m.state = &models.JobState{
		Running:    false,
		JobType:    models.JobTypeWatcher,
		Summary:    msg,
		FinishedAt: &now,
	}
	m.armDismiss(dismissDelay)
}

// Caller must hold m.mu.
func (m *Manager) armDismiss(d time.Duration) {
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(d, m.Dismiss)
}
