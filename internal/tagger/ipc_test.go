//go:build tagger

package tagger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

func TestFrameRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   ipcRequest
	}{
		{
			name: "status",
			in:   ipcRequest{Method: ipcMethodStatus},
		},
		{
			name: "release_idle",
			in:   ipcRequest{Method: ipcMethodReleaseIdle, IdleAfter: 600_000_000_000},
		},
		{
			name: "run_with_frames",
			in: ipcRequest{
				ID:     7,
				Method: ipcMethodRun,
				Run: &RunRequest{
					ModelPath:       "/models",
					DataPath:        "/data",
					SessionParallel: 4,
					Provider:        "cpu",
					Models:          []string{"wd-swinv2", "animetimm-eva02"},
					Size:            2,
					Frames:          [][]byte{bytes.Repeat([]byte{9}, 12)},
				},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFrame(&buf, c.in); err != nil {
				t.Fatalf("write: %v", err)
			}
			var got ipcRequest
			if err := readFrame(&buf, &got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Method != c.in.Method {
				t.Fatalf("method: got %d, want %d", got.Method, c.in.Method)
			}
			if got.ID != c.in.ID {
				t.Fatalf("ID: got %d, want %d", got.ID, c.in.ID)
			}
			if c.in.Run != nil {
				if got.Run == nil {
					t.Fatalf("Run round-trip lost the payload")
				}
				if fmt.Sprint(*got.Run) != fmt.Sprint(*c.in.Run) {
					t.Fatalf("Run: got %+v, want %+v", got.Run, c.in.Run)
				}
			}
		})
	}
}

func TestWorkerEnv_InheritsTheParentLevelUnlessSet(t *testing.T) {
	t.Cleanup(func() { logx.Set("warn") })
	logx.Set("info")

	t.Setenv(workerLogEnv, "")
	env := workerEnv("tok")
	if got := env[len(env)-1]; got != workerLogEnv+"=info" {
		t.Errorf("unset: last entry %q, want %s=info", got, workerLogEnv)
	}
	if !slices.Contains(env, ipcTokenEnv+"=tok") {
		t.Errorf("unset: token missing from %v", env)
	}

	t.Setenv(workerLogEnv, "debug")
	env = workerEnv("tok")
	var levels []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, workerLogEnv+"="); ok {
			levels = append(levels, v)
		}
	}
	if !slices.Equal(levels, []string{"debug"}) {
		t.Errorf("set: %s values %v, want [debug]", workerLogEnv, levels)
	}
}

func TestHandshakeAcceptsOnlyTheSpawnToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		greet func(c net.Conn)
		want  bool
	}{
		{"spawn token", func(c net.Conn) { _ = writeFrame(c, ipcHello{Token: "tok"}) }, true},
		{"other token", func(c net.Conn) { _ = writeFrame(c, ipcHello{Token: "nope"}) }, false},
		{"closed before greeting", func(c net.Conn) { _ = c.Close() }, false},
		{"oversized header", func(c net.Conn) {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], maxFrameBytes+1)
			_, _ = c.Write(hdr[:])
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
			go tc.greet(client)
			if got := ipcHandshakeOK(server, "tok"); got != tc.want {
				t.Errorf("ipcHandshakeOK = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAcceptWorkerDropsABadGreeterAndKeepsListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	dial := func(token string) net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := writeFrame(c, ipcHello{Token: token}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	bad := dial("nope")
	good := dial("tok")

	c, err := acceptWorker(l, "tok")
	if err != nil {
		t.Fatalf("acceptWorker: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.RemoteAddr().String() != good.LocalAddr().String() {
		t.Errorf("accepted %s, want the good greeter %s", c.RemoteAddr(), good.LocalAddr())
	}
	_ = bad.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bad.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("bad greeter read: %v, want EOF (connection dropped)", err)
	}
}

func TestWorkerGreetsWithTheEnvironmentToken(t *testing.T) {
	t.Setenv(ipcTokenEnv, "tok")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	served := make(chan error, 1)
	go func() { served <- RunWorkerServer(context.Background(), l.Addr().String()) }()

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if !ipcHandshakeOK(c, "tok") {
		t.Fatal("the worker did not greet with the token from its environment")
	}
	_ = c.Close()
	if err := <-served; err != nil {
		t.Errorf("RunWorkerServer after the parent closed: %v", err)
	}
}

func TestRunCancelTellsTheChild(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	b := &ipcBackend{cmd: &exec.Cmd{}, conn: newIPCConn(client)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan ipcRequest, 2)
	go func() {

		for {
			var req ipcRequest
			if err := readFrame(server, &req); err != nil {
				return
			}
			seen <- req
			if req.Method == ipcMethodRun {
				cancel()
			}
		}
	}()

	start := time.Now()
	_, err := b.Run(ctx, RunRequest{Models: []string{"m"}})
	if err != context.Canceled {
		t.Fatalf("Run err: got %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run blocked for %v after cancel; expected sub-second", elapsed)
	}
	run, stop := <-seen, <-seen
	if stop.Method != ipcMethodCancel || stop.ID != run.ID {
		t.Errorf("after the cancel the child got %+v, want a cancel of call %d", stop, run.ID)
	}
	if b.conn == nil {
		t.Error("a cancelled call took the child down")
	}
}

func TestCallsInFlightGetTheirOwnReplies(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	b := &ipcBackend{cmd: &exec.Cmd{}, conn: newIPCConn(client)}

	go func() {

		var reqs []ipcRequest
		for len(reqs) < 2 {
			var req ipcRequest
			if err := readFrame(server, &req); err != nil {
				return
			}
			reqs = append(reqs, req)
		}
		for _, req := range slices.Backward(reqs) {
			_ = writeFrame(server, ipcResponse{ID: req.ID, Run: &RunResponse{Results: [][]FrameScores{{{Err: fmt.Sprint(req.Run.Size)}}}}})
		}
	}()

	errs := make(chan error, 2)
	for _, size := range []int{1, 2} {
		go func() {
			resp, err := b.Run(context.Background(), RunRequest{Size: size})
			if err == nil && resp.Results[0][0].Err != fmt.Sprint(size) {
				err = fmt.Errorf("call of size %d got the reply %q", size, resp.Results[0][0].Err)
			}
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestAChildGoneFailsTheCallAndIsDropped(t *testing.T) {
	server, client := net.Pipe()
	b := &ipcBackend{cmd: &exec.Cmd{}, conn: newIPCConn(client)}
	go func() {
		var req ipcRequest
		_ = readFrame(server, &req)
		_ = server.Close()
	}()
	if _, err := b.Run(context.Background(), RunRequest{}); err == nil {
		t.Fatal("Run: want an error once the child is gone")
	}
	if b.conn != nil {
		t.Error("the dead child's connection was kept")
	}
}

func TestStatusReportsInFlightProvider(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	b := &ipcBackend{
		cmd:  &exec.Cmd{},
		conn: newIPCConn(client),
	}

	stale := CacheStatus{Loaded: true, Provider: "cpu", Sessions: []string{"wd-swinv2"}}
	b.lastStatus.Store(&stale)

	b.inFlight.Add(1)
	b.runProvider.Store("cuda")
	defer b.inFlight.Add(-1)

	snap := b.Status()
	if !snap.Loaded {
		t.Fatalf("Loaded: got false, want true")
	}
	if snap.Provider != "cuda" {
		t.Fatalf("Provider: got %q, want %q (stale snapshot must be overridden)", snap.Provider, "cuda")
	}
	if !snap.InUse {
		t.Fatalf("InUse: got false, want true")
	}
}

func TestReleaseAllLeavesARunInFlightAlone(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	b := &ipcBackend{cmd: &exec.Cmd{}, conn: newIPCConn(client)}

	b.inFlight.Add(1)
	b.mu.RLock()
	defer func() {
		b.mu.RUnlock()
		b.inFlight.Add(-1)
	}()

	done := make(chan struct{})
	go func() {
		b.ReleaseAll()
		b.ReleaseIdle(0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a release waited on the call in flight")
	}
	if b.conn == nil {
		t.Error("a release dropped the connection of the call in flight")
	}
}

func TestResponseRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   ipcResponse
	}{
		{"run_ok", ipcResponse{ID: 3, Run: &RunResponse{Results: [][]FrameScores{
			{{SparseScores: SparseScores{I: []int32{3, 17}, S: []float32{0.91, 0.4}}}, {Err: "refused"}},
		}}}},
		{"status_loaded", ipcResponse{Status: &CacheStatus{Loaded: true, Provider: "cpu", InUse: false, Sessions: []string{"wd-swinv2"}}}},
		{"release_idle_true", ipcResponse{Released: true}},
		{"error", ipcResponse{Err: "model.onnx missing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFrame(&buf, c.in); err != nil {
				t.Fatalf("write: %v", err)
			}
			var got ipcResponse
			if err := readFrame(&buf, &got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Err != c.in.Err {
				t.Errorf("Err mismatch: got %q, want %q", got.Err, c.in.Err)
			}
			if got.Released != c.in.Released {
				t.Errorf("Released mismatch: got %v, want %v", got.Released, c.in.Released)
			}
			if c.in.Run != nil && fmt.Sprint(got.Run.Results) != fmt.Sprint(c.in.Run.Results) {
				t.Errorf("Results mismatch: got %v, want %v", got.Run.Results, c.in.Run.Results)
			}
		})
	}
}

func TestReapLogsAnAbnormalWorkerExit(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for _, tc := range []struct {
		name string
		args []string
		stop func(b *ipcBackend)
		warn string
	}{
		{"crash", []string{"sh", "-c", "exit 3"}, func(b *ipcBackend) { b.reapLocked(true, "terminated") }, "exit status 3"},
		{"clean exit", []string{"true"}, func(b *ipcBackend) { b.reapLocked(false, "released") }, ""},
		{"terminate", []string{"sleep", "30"}, func(b *ipcBackend) { b.terminate() }, ""},
	} {
		buf.Reset()
		cmd := exec.Command(tc.args[0], tc.args[1:]...)
		if err := cmd.Start(); err != nil {
			t.Fatalf("%s: start: %v", tc.name, err)
		}
		tc.stop(&ipcBackend{cmd: cmd})
		out := buf.String()
		switch {
		case tc.warn == "" && strings.Contains(out, "WARN"):
			t.Errorf("%s: logged %q, want no WARN", tc.name, out)
		case tc.warn != "" && (!strings.Contains(out, "WARN") || !strings.Contains(out, tc.warn)):
			t.Errorf("%s: logged %q, want a WARN naming %q", tc.name, out, tc.warn)
		}
	}
}
