package plugins

import (
	"strings"
	"testing"
	"time"
)

func TestManagedRunErrorCarriesTheLastOutputLine(t *testing.T) {
	m := &managedPlugin{
		name:    "boom",
		command: "/bin/sh",
		args:    []string{"-c", "echo first; echo the reason it died; exit 1"},
	}
	err := m.run()
	if err == nil {
		t.Fatal("run of a failing command returned no error")
	}
	if !strings.Contains(err.Error(), "the reason it died") {
		t.Errorf("error = %q, want it to carry the plugin's last line", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error = %q, want it to keep the exit status", err)
	}
}

func TestRunRefusesToLaunchAfterAStop(t *testing.T) {
	m := &managedPlugin{
		name:    "late",
		command: "/bin/sh",
		args:    []string{"-c", "exit 0"},
		stopped: true,
	}
	if err := m.run(); err != nil {
		t.Fatalf("run after a stop = %v, want nil", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proc != nil || m.running {
		t.Error("run started a process after the plugin had been stopped")
	}
}

func newTestSupervisor(t *testing.T) (*Supervisor, chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})
	return New(func() string { return "http://127.0.0.1:0" }, done), done
}

func waitForState(t *testing.T, s *Supervisor, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.State(name); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("plugin %s state = %q, want %q", name, s.State(name), want)
}

func TestStartRunsThePluginAndStopEndsIt(t *testing.T) {
	s, _ := newTestSupervisor(t)
	s.Start(Launch{Name: "sleeper", Command: "/bin/sh", Args: []string{"-c", "cat"}})
	waitForState(t, s, "sleeper", "running")

	s.Stop("sleeper")
	waitForState(t, s, "sleeper", "stopped")
}

func TestStopKeepsTheSupervisorFromRestarting(t *testing.T) {
	s, _ := newTestSupervisor(t)
	s.Start(Launch{Name: "sleeper", Command: "/bin/sh", Args: []string{"-c", "cat"}})
	waitForState(t, s, "sleeper", "running")
	s.Stop("sleeper")
	waitForState(t, s, "sleeper", "stopped")

	time.Sleep(2 * BackoffMin)
	if got := s.State("sleeper"); got != "stopped" {
		t.Errorf("state after the stop settled = %q, want stopped", got)
	}
}

func TestACrashIsRestartedWithBackoff(t *testing.T) {
	s, _ := newTestSupervisor(t)
	s.Start(Launch{Name: "crasher", Command: "/bin/sh", Args: []string{"-c", "exit 1"}})
	waitForState(t, s, "crasher", "restarting (1)")
	s.Stop("crasher")
}

func TestStartTwiceKeepsOneSupervisor(t *testing.T) {
	s, _ := newTestSupervisor(t)
	launch := Launch{Name: "sleeper", Command: "/bin/sh", Args: []string{"-c", "cat"}}
	s.Start(launch)
	waitForState(t, s, "sleeper", "running")
	s.Start(launch)
	waitForState(t, s, "sleeper", "running")

	s.StopAll()
	waitForState(t, s, "sleeper", "stopped")
}

func TestStopAllEndsEveryPlugin(t *testing.T) {
	s, _ := newTestSupervisor(t)
	for _, name := range []string{"one", "two"} {
		s.Start(Launch{Name: name, Command: "/bin/sh", Args: []string{"-c", "cat"}})
		waitForState(t, s, name, "running")
	}
	s.StopAll()
	for _, name := range []string{"one", "two"} {
		waitForState(t, s, name, "stopped")
	}
}

func TestStateOfAnUnknownPluginIsStopped(t *testing.T) {
	s, _ := newTestSupervisor(t)
	if got := s.State("never-enabled"); got != "stopped" {
		t.Errorf("state = %q, want stopped", got)
	}
}

func TestStopEndsARunWhoseGrandchildHoldsTheOutput(t *testing.T) {
	s, _ := newTestSupervisor(t)
	s.Start(Launch{Name: "forker", Command: "/bin/sh", Args: []string{"-c", "sleep 30 & echo up; cat"}})
	waitForState(t, s, "forker", "running")

	s.Stop("forker")
	waitForState(t, s, "forker", "stopped")
}
