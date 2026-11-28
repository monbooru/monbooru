// Package plugins launches the plugin binaries an operator put on disk;
// it never downloads or installs anything.
package plugins

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type Launch struct {
	Name    string
	Command string
	Args    []string
	Dir     string
	// Env joins MONBOORU_URL in the process's environment.
	Env []string
	// Label names the process in the log; "" is "plugin <name>".
	Label string
}

type Supervisor struct {
	CallbackURL func() string
	Done        <-chan struct{}

	mu      sync.Mutex
	managed map[string]*managedPlugin
}

func New(callbackURL func() string, done <-chan struct{}) *Supervisor {
	return &Supervisor{
		CallbackURL: callbackURL,
		Done:        done,
		managed:     map[string]*managedPlugin{},
	}
}

const (
	// Counted from stdin closing: no signals, so it works the same on Windows.
	managedStopGrace    = 5 * time.Second
	managedHealthyAfter = 30 * time.Second
	BackoffMin          = time.Second
	managedBackoffMax   = time.Minute
	// Above bufio's 64 KiB default, which a traceback with a large repr
	// or JSON dump exceeds.
	LogLineMax = 1 << 20
)

type managedPlugin struct {
	name    string
	label   string
	command string
	args    []string
	dir     string
	env     []string

	mu          sync.Mutex
	proc        *exec.Cmd
	stdin       io.WriteCloser
	running     bool
	supervising bool
	stopped     bool
	restarts    int
	stop        chan struct{}
}

func (s *Supervisor) Start(p Launch) {
	s.mu.Lock()
	m, ok := s.managed[p.Name]
	if !ok {
		m = &managedPlugin{name: p.Name}
		s.managed[p.Name] = m
	}
	s.mu.Unlock()

	env := append([]string{"MONBOORU_URL=" + s.CallbackURL()}, p.Env...)

	m.mu.Lock()
	m.label = p.Label
	if m.label == "" {
		m.label = "plugin " + p.Name
	}
	m.command, m.args = p.Command, p.Args
	m.dir, m.env = p.Dir, env
	m.stopped, m.restarts = false, 0
	// Fresh even when a supervisor runs: it would read the closed old
	// channel at its next backoff and quit with the plugin enabled, and a
	// later Disable would close it twice.
	m.stop = make(chan struct{})
	if m.supervising {
		m.mu.Unlock()
		return
	}
	m.supervising = true
	m.mu.Unlock()
	go s.supervise(m)
}

func (s *Supervisor) Stop(name string) {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.stop)
	}
	m.mu.Unlock()
	m.terminate()
}

func (s *Supervisor) StopAll() {
	s.mu.Lock()
	names := slices.Collect(maps.Keys(s.managed))
	s.mu.Unlock()
	for _, name := range names {
		s.Stop(name)
	}
}

func (s *Supervisor) supervise(m *managedPlugin) {
	defer func() {
		m.mu.Lock()
		m.supervising = false
		m.mu.Unlock()
	}()
	for {
		// The flag, not the channel, is the authority: the backoff select
		// can miss a stop, so it is read again before each run.
		m.mu.Lock()
		stopped, label := m.stopped, m.label
		m.mu.Unlock()
		if stopped {
			return
		}
		started := time.Now()
		if err := m.run(); err != nil {
			logx.Errorf("%s: %v", label, err)
		}
		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		if time.Since(started) >= managedHealthyAfter {
			m.restarts = 0
		}
		m.restarts++
		stop := m.stop
		delay := min(BackoffMin<<min(m.restarts-1, 6), managedBackoffMax)
		m.mu.Unlock()
		logx.Warnf("%s: exited, restarting in %s", label, delay)
		select {
		case <-time.After(delay):
		case <-stop:
			// An Enable may have followed the stop that closed this; the
			// flag decides.
			m.mu.Lock()
			stopped := m.stopped
			m.mu.Unlock()
			if stopped {
				return
			}
		case <-s.Done:
			return
		}
	}
}

func (m *managedPlugin) run() error {
	m.mu.Lock()
	// Checked again under the lock that publishes m.proc: a Stop after
	// the loop's read had no child to terminate.
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	label := m.label
	cmd := exec.Command(m.command, m.args...)
	desktop.HideConsole(cmd)
	cmd.Dir = m.dir
	cmd.Env = append(os.Environ(), m.env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	out, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.proc, m.stdin, m.running = cmd, stdin, true
	m.mu.Unlock()
	logx.Infof("%s: started (pid %d)", label, cmd.Process.Pid)

	lastLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(nil, LogLineMax)
		var last string
		for scanner.Scan() {
			last = scanner.Text()
			logx.Infof("%s: %s", label, last)
		}
		if scanner.Err() != nil {
			// A line past the cap stops the scan; undrained, the child blocks
			// on a full pipe and Wait never returns.
			logx.Warnf("%s: output dropped: %v", label, scanner.Err())
			_, _ = io.Copy(io.Discard, out)
		}
		lastLine <- last
	}()
	err = cmd.Wait()
	_ = pw.Close()
	last := <-lastLine
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}

	m.mu.Lock()
	m.proc, m.stdin, m.running = nil, nil, false
	m.mu.Unlock()
	// Output logs at info, below the default threshold; without the last
	// line a crash reads as a bare exit status.
	if err != nil && last != "" {
		return fmt.Errorf("%w: %s", err, last)
	}
	return err
}

// Closing stdin is the exit signal a managed plugin must honour.
func (m *managedPlugin) terminate() {
	m.mu.Lock()
	cmd, stdin, label := m.proc, m.stdin, m.label
	m.mu.Unlock()
	if cmd == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	deadline := time.Now().Add(managedStopGrace)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		running := m.running
		m.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		logx.Warnf("%s: killed after %s", label, managedStopGrace)
	}
}

func (s *Supervisor) State(name string) string {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return "stopped"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.running:
		return "running"
	case m.stopped || !m.supervising:
		return "stopped"
	case m.restarts > 0:
		return "restarting (" + strconv.Itoa(m.restarts) + ")"
	default:
		return "starting"
	}
}

func (s *Supervisor) PID(name string) int {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proc == nil || m.proc.Process == nil {
		return 0
	}
	return m.proc.Process.Pid
}
