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

// RunRequest carries pre-extracted frames: the backend never reads the
// gallery database or unpacks archives.
type RunRequest struct {
	ModelPath string
	DataPath  string
	// SessionParallel is the configured tagger.parallel, which sizes the
	// sessions' intra-op threads; Parallel is this batch's worker count.
	SessionParallel    int
	IdleReleaseMinutes int
	Taggers            []TaggerStatus
	Provider           string
	CatIDs             map[string]int64
	GeneralCatID       int64
	InferredCats       map[string]int64
	MinHitFraction     float64
	Parallel           int
	Images             []BackendImageRequest
	// OnProgress may be nil. An empty msg marks an image done; any other
	// is page status.
	OnProgress func(workerIdx int, msg string)
}

// MangaProgress asks the backend to report each page through OnProgress.
type BackendImageRequest struct {
	ID            int64
	FramePaths    []string
	MangaProgress bool
	FileType      string
}

// RunResponse.Results mirrors RunRequest.Images in order.
type RunResponse struct {
	Results []BackendImageResult
}

// Err is a string because gob cannot encode an error interface over IPC.
type BackendImageResult struct {
	ID   int64
	Tags map[TagKey]Scored
	Err  string
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
