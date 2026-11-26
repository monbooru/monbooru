package web

import (
	"crypto/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The supervisor's key for the local worker: the colon keeps it apart
// from every plugin's name.
const localWorkerName = "local:monworker"

// localWorker is the monworker monbooru runs itself: no pairing, a
// loopback port and a secret minted for this run, and the settings of
// [tagger] handed over in its environment.
type localWorker struct {
	// path is the binary, "" when none is beside monbooru or on PATH.
	path  string
	token string
	addr  string

	mu sync.Mutex
	// env is what the running worker started with; a change restarts it.
	env         []string
	dir         string
	saidMissing bool
}

func findLocalWorker() string {
	if p := desktop.BundledTool("monworker"); p != "" {
		return p
	}
	name := "monworker"
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return ""
}

func newLocalWorker() *localWorker {
	if os.Getenv("MONBOORU_TAGGER_BACKEND") != "" {
		logx.Warnf("MONBOORU_TAGGER_BACKEND is no longer read: the tagger runs on a monworker")
	}
	w := &localWorker{path: findLocalWorker(), token: rand.Text()}
	if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		w.addr = l.Addr().String()
		_ = l.Close()
	}
	return w
}

func (w *localWorker) url() string { return "http://" + w.addr }

func (w *localWorker) def() tagger.WorkerDef {
	return tagger.WorkerDef{Name: tagger.LocalWorker, Local: true, URL: w.url(), Token: w.token}
}

func (w *localWorker) envFor(cfg *config.Config) []string {
	env := []string{
		"MONWORKER_LISTEN=" + w.addr,
		"MONWORKER_TOKEN=" + w.token,
		"MONWORKER_NAME=" + tagger.LocalWorker,
		"MONWORKER_MODELS=" + cfg.Paths.ModelPath,
		"MONWORKER_CACHE=" + cfg.Paths.DataPath,
		"MONWORKER_CONFIG=" + filepath.Join(cfg.Paths.DataPath, "monworker.toml"),
		"MONWORKER_PROVIDER=" + cfg.Tagger.ExecutionProvider,
		"MONWORKER_PARALLEL=" + strconv.Itoa(max(1, cfg.Tagger.Parallel)),
		"MONWORKER_THREADS=" + strconv.Itoa(max(0, cfg.Tagger.CPUThreads)),
		"MONWORKER_IDLE_MINUTES=" + strconv.Itoa(max(0, cfg.Tagger.IdleReleaseAfterMinutes)),
		"MONWORKER_EXIT_ON_STDIN_EOF=1",
	}
	level := cfg.Log.Level
	if v := os.Getenv("MONBOORU_TAGGER_WORKER_LOG"); v != "" {
		level = v
	}
	if level != "" {
		env = append(env, "MONWORKER_LOG="+level)
	}
	if v := os.Getenv("MONBOORU_TAGGER_DIRECTML_DEVICE_ID"); v != "" {
		env = append(env, "MONWORKER_DIRECTML_DEVICE_ID="+v)
	}
	return env
}

func (w *localWorker) apply(sup *plugins.Supervisor, cfg *config.Config) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	want := cfg.Tagger.LocalWorker && w.path != "" && w.addr != ""
	if !want {
		if w.env != nil {
			sup.Stop(localWorkerName)
			w.env = nil
			logx.Infof("local worker: stopped")
		}
		if cfg.Tagger.LocalWorker && w.path == "" && !w.saidMissing {
			w.saidMissing = true
			logx.Infof("local worker: no monworker beside monbooru or on PATH")
		}
		return false
	}
	env := w.envFor(cfg)
	if slices.Equal(env, w.env) {
		return true
	}
	if w.env != nil {
		sup.Stop(localWorkerName)
	}
	w.env, w.dir = env, cfg.Paths.DataPath
	w.start(sup)
	return true
}

func (w *localWorker) start(sup *plugins.Supervisor) {
	sup.Start(plugins.Launch{Name: localWorkerName, Command: w.path, Dir: w.dir, Env: w.env, Label: "local worker"})
	// The pool's first look then finds it up rather than marking it down.
	listening(w.addr, 3*time.Second)
}

// A monworker has no call that unloads its models: restarting it is the
// way to drop them.
func (w *localWorker) restart(sup *plugins.Supervisor) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.env == nil {
		return false
	}
	sup.Stop(localWorkerName)
	w.start(sup)
	return true
}

func listening(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// restartsOn is whether applying cfg would restart the running worker.
func (w *localWorker) restartsOn(cfg *config.Config) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.env != nil && !slices.Equal(w.envFor(cfg), w.env)
}

func (w *localWorker) running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.env != nil
}
