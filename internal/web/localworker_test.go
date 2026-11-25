package web

import (
	"bytes"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/plugins"
)

func standIn(t *testing.T) (path, envFile string) {
	t.Helper()
	dir := t.TempDir()
	envFile = filepath.Join(dir, "env")
	path = filepath.Join(dir, "monworker")
	mustWriteFile(t, path, []byte("#!/bin/sh\nenv > "+envFile+".tmp && mv "+envFile+".tmp "+envFile+"\ncat > /dev/null\n"), 0o755)
	return path, envFile
}

func waitEnv(t *testing.T, file, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(file); err == nil && strings.Contains(string(data), want) {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the worker never started with %s", want)
	return ""
}

func TestTheLocalWorkerRunsAsTheConfigSays(t *testing.T) {
	path, envFile := standIn(t)
	sup := plugins.New(func() string { return "http://127.0.0.1:8455" }, make(chan struct{}))
	t.Cleanup(sup.StopAll)
	w := &localWorker{path: path, token: "secret", addr: "127.0.0.1:47000"}
	cfg := &config.Config{}
	cfg.Paths.ModelPath, cfg.Paths.DataPath = t.TempDir(), t.TempDir()
	cfg.Tagger.ExecutionProvider, cfg.Tagger.Parallel, cfg.Tagger.IdleReleaseAfterMinutes = "cpu", 4, 15

	if w.apply(sup, cfg) || w.running() {
		t.Fatal("the worker runs while local_worker is off")
	}
	cfg.Tagger.LocalWorker = true
	if !w.apply(sup, cfg) {
		t.Fatal("the worker does not run once local_worker is on")
	}
	env := waitEnv(t, envFile, "MONWORKER_PARALLEL=4")
	for _, want := range []string{
		"MONWORKER_LISTEN=127.0.0.1:47000", "MONWORKER_TOKEN=secret", "MONWORKER_MODELS=" + cfg.Paths.ModelPath,
		"MONWORKER_CACHE=" + cfg.Paths.DataPath, "MONWORKER_PROVIDER=cpu", "MONWORKER_EXIT_ON_STDIN_EOF=1", "MONWORKER_NAME=local", "MONWORKER_THREADS=0",
	} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("environment lacks %s", want)
		}
	}
	if d := w.def(); !d.Local || d.URL != "http://127.0.0.1:47000" || d.Token != "secret" {
		t.Errorf("pool entry = %+v", d)
	}

	cfg.Tagger.Parallel = 2
	w.apply(sup, cfg)
	waitEnv(t, envFile, "MONWORKER_PARALLEL=2")

	cfg.Tagger.LocalWorker = false
	if w.apply(sup, cfg) || sup.State(localWorkerName) != "stopped" {
		t.Errorf("after the switch off the worker is %s", sup.State(localWorkerName))
	}
}

func TestNoLocalWorkerWithoutTheBinary(t *testing.T) {
	sup := plugins.New(func() string { return "" }, make(chan struct{}))
	w := &localWorker{addr: "127.0.0.1:47001"}
	cfg := &config.Config{}
	cfg.Tagger.LocalWorker = true
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for range 3 {
		if w.apply(sup, cfg) {
			t.Error("a worker runs with no monworker found")
		}
	}
	if n := strings.Count(buf.String(), "no monworker beside monbooru or on PATH"); n != 1 {
		t.Errorf("three config writes logged the missing monworker %d times, want once:\n%s", n, buf.String())
	}
}

func TestTheServerGivesThePoolItsLocalWorker(t *testing.T) {
	path, envFile := standIn(t)
	t.Setenv("PATH", filepath.Dir(path)+string(os.PathListSeparator)+os.Getenv("PATH"))
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Tagger.LocalWorker = true })
	waitEnv(t, envFile, "MONWORKER_TOKEN=")
	if names := workerNames(srv); !slices.Equal(names, []string{"local"}) {
		t.Errorf("pool = %v, want the local worker", names)
	}
	if err := srv.withConfig(func(c *config.Config) error {
		c.Tagger.LocalWorker = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if names := workerNames(srv); len(names) != 0 {
		t.Errorf("pool = %v after the switch off", names)
	}
}

func TestFreeMemoryRestartsTheLocalWorker(t *testing.T) {
	path, envFile := standIn(t)
	t.Setenv("PATH", filepath.Dir(path)+string(os.PathListSeparator)+os.Getenv("PATH"))
	srv := newTestServerWith(t, false, func(c *config.Config) { c.Tagger.LocalWorker = true })
	waitEnv(t, envFile, "MONWORKER_TOKEN=")
	before := srv.peers.PID(localWorkerName)

	if w := postForm(t, srv, "/settings/maintenance/free-memory", url.Values{}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "flash-ok") {
		t.Fatalf("free memory: %d %s", w.Code, w.Body)
	}
	if after := srv.peers.PID(localWorkerName); after == 0 || after == before {
		t.Errorf("the worker's pid is %d after, %d before: it did not restart", after, before)
	}
}

func TestFreeMemoryStartsNoWorkerWhileItIsOff(t *testing.T) {
	path, _ := standIn(t)
	t.Setenv("PATH", filepath.Dir(path)+string(os.PathListSeparator)+os.Getenv("PATH"))
	srv := newTestServerWith(t, false, nil)

	if w := postForm(t, srv, "/settings/maintenance/free-memory", url.Values{}); w.Code != http.StatusOK {
		t.Fatalf("free memory: %d %s", w.Code, w.Body)
	}
	if state := srv.peers.State(localWorkerName); state != "stopped" {
		t.Errorf("the worker is %s after a free memory with local_worker off", state)
	}
}

func workerNames(srv *Server) []string {
	var out []string
	for _, w := range srv.pool.Workers() {
		out = append(out, w.Name)
	}
	return out
}
