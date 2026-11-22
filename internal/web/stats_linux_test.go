//go:build linux

package web

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

func TestAutotagPeakIsTheJobs(t *testing.T) {

	spike := make([]byte, 256<<20)
	for i := 0; i < len(spike); i += 4096 {
		spike[i] = 1
	}
	runtime.KeepAlive(spike)
	runtime.GC()
	debug.FreeOSMemory()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	prev := logx.Get()
	logx.Set("info")
	t.Cleanup(func() { logx.Set(prev) })

	peak := autotagMemoryMark()
	peak("test job")
	if out := buf.String(); strings.Contains(out, "MiB") || strings.Contains(out, "GiB") {
		t.Errorf("an idle job logged %q: the peak predates it", strings.TrimSpace(out))
	}
}

func TestProcChildrenFindsAChildOfAnyThread(t *testing.T) {
	release := make(chan struct{})
	started := make(chan *exec.Cmd, 1)
	var spawn func()
	spawn = func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if syscall.Gettid() == os.Getpid() {
			go spawn()
			<-release
			return
		}
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Error(err)
			started <- nil
			return
		}
		started <- cmd
		<-release
	}
	go spawn()
	cmd := <-started
	t.Cleanup(func() { close(release) })
	if cmd == nil {
		return
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if got := procChildren(os.Getpid()); !slices.Contains(got, cmd.Process.Pid) {
		t.Errorf("procChildren = %v, want it to hold %d", got, cmd.Process.Pid)
	}
}
