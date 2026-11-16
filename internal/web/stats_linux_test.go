//go:build linux

package web

import (
	"bytes"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
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
