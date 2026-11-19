package tagger

import (
	"context"
	"sync"
	"time"
)

type Backend interface {
	// Per-image failures go on the matching result's Err, not the
	// returned error.
	Run(ctx context.Context, req RunRequest) (RunResponse, error)
	Status() CacheStatus
	// ReleaseIdle never tears down while a run is in flight.
	ReleaseIdle(after time.Duration) bool
	ReleaseAll()
}

func WorkerPID() (int, bool) {
	if b := activeBackend(); b != nil {
		if w, ok := b.(workerPIDer); ok {
			return w.WorkerPID()
		}
	}
	return 0, false
}

type workerPIDer interface {
	WorkerPID() (int, bool)
}

type TagKey struct {
	Name  string
	CatID int64
}

type Scored struct {
	Score      float32
	TaggerName string
}

// CacheStatus.Sessions is sorted so a refresh doesn't reorder the list.
type CacheStatus struct {
	Loaded   bool
	Provider string
	InUse    bool
	Sessions []string
	LastUsed time.Time
}

// RunRequest is one call: Frames, each a Size x Size RGB8 square, through
// each of Models, which share that input. Taggers is the run's whole set,
// what the session cache holds; the backend never reads the gallery.
type RunRequest struct {
	ModelPath string
	DataPath  string
	// SessionParallel is the configured tagger.parallel, which sizes the
	// sessions' intra-op threads and the calls inferring at once.
	SessionParallel int
	Taggers         []TaggerStatus
	Provider        string
	Models          []string
	Size            int
	Frames          [][]byte
}

// FrameScores is one frame through one model; a frame the model refused
// carries Err, a string because gob cannot encode an error interface.
type FrameScores struct {
	SparseScores
	Err string
}

// RunResponse.Results follows RunRequest.Models, each with one entry per
// frame.
type RunResponse struct {
	Results [][]FrameScores
}

var (
	backendMu      sync.RWMutex
	currentBackend Backend
)

func SetBackend(b Backend) {
	backendMu.Lock()
	currentBackend = b
	backendMu.Unlock()
}

func activeBackend() Backend {
	backendMu.RLock()
	defer backendMu.RUnlock()
	return currentBackend
}
