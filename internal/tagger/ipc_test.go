//go:build tagger

package tagger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
			name: "run_with_images",
			in: ipcRequest{
				Method: ipcMethodRun,
				Run: &RunRequest{
					ModelPath:          "/models",
					DataPath:           "/data",
					SessionParallel:    4,
					IdleReleaseMinutes: 15,
					Provider:           "cpu",
					Parallel:           2,
					MinHitFraction:     0.05,
					Images: []BackendImageRequest{
						{ID: 1, FramePaths: []string{"/tmp/a.png"}},
						{ID: 2, FramePaths: []string{"/tmp/b.png"}, MangaProgress: true},
					},
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
			if c.in.Run != nil {
				if got.Run == nil {
					t.Fatalf("Run round-trip lost the payload")
				}
				if got.Run.Parallel != c.in.Run.Parallel || got.Run.MinHitFraction != c.in.Run.MinHitFraction ||
					got.Run.ModelPath != c.in.Run.ModelPath || got.Run.DataPath != c.in.Run.DataPath ||
					got.Run.SessionParallel != c.in.Run.SessionParallel ||
					got.Run.IdleReleaseMinutes != c.in.Run.IdleReleaseMinutes {
					t.Fatalf("scalar mismatch: got %+v, want %+v", got.Run, c.in.Run)
				}
				if len(got.Run.Images) != len(c.in.Run.Images) {
					t.Fatalf("Images: got %d, want %d", len(got.Run.Images), len(c.in.Run.Images))
				}
				for i, im := range c.in.Run.Images {
					if got.Run.Images[i].ID != im.ID || got.Run.Images[i].MangaProgress != im.MangaProgress {
						t.Errorf("Images[%d]: got %+v, want %+v", i, got.Run.Images[i], im)
					}
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

func TestRunCancelDuringInFlight(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	b := &ipcBackend{
		cmd:  &exec.Cmd{},
		conn: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {

		var req ipcRequest
		_ = readFrame(server, &req)
		<-ctx.Done()
	}()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := b.Run(ctx, RunRequest{Images: []BackendImageRequest{{ID: 1, FramePaths: []string{"/tmp/x.png"}}}})
	elapsed := time.Since(start)
	if err != context.Canceled {
		t.Fatalf("Run err: got %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Run blocked for %v after cancel; expected sub-second", elapsed)
	}
}

func TestRunAtZeroIdleShutsTheWorkerDown(t *testing.T) {
	for _, tc := range []struct {
		idle int
		want []ipcMethod
	}{
		{0, []ipcMethod{ipcMethodRun, ipcMethodShutdown}},
		{15, []ipcMethod{ipcMethodRun}},
	} {
		server, client := net.Pipe()
		b := &ipcBackend{cmd: &exec.Cmd{}, conn: client}

		got := make(chan []ipcMethod, 1)
		go func() {
			var methods []ipcMethod
			for {
				var req ipcRequest
				if err := readFrame(server, &req); err != nil {
					got <- methods
					return
				}
				methods = append(methods, req.Method)
				reply := ipcResponse{}
				if req.Method == ipcMethodRun {
					reply.Run = &RunResponse{}
				}
				_ = writeFrame(server, reply)
				if req.Method == ipcMethodShutdown {
					_ = server.Close()
				}
			}
		}()

		if _, err := b.Run(context.Background(), RunRequest{IdleReleaseMinutes: tc.idle}); err != nil {
			t.Fatalf("idle %d: Run: %v", tc.idle, err)
		}
		if live := b.conn != nil; live != (tc.idle > 0) {
			t.Errorf("idle %d: worker connection kept = %v", tc.idle, live)
		}
		_ = client.Close()
		if methods := <-got; !slices.Equal(methods, tc.want) {
			t.Errorf("idle %d: child saw %v, want %v", tc.idle, methods, tc.want)
		}
	}
}

func TestStatusReportsInFlightProvider(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	b := &ipcBackend{
		cmd:  &exec.Cmd{},
		conn: client,
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
	b := &ipcBackend{cmd: &exec.Cmd{}, conn: client}

	b.inFlight.Add(1)
	b.mu.Lock()
	defer func() {
		b.mu.Unlock()
		b.inFlight.Add(-1)
	}()

	done := make(chan struct{})
	go func() {
		b.ReleaseAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReleaseAll waited on the run in flight")
	}
	if b.conn == nil {
		t.Error("ReleaseAll dropped the connection of the run in flight")
	}
}

func TestResponseRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   ipcResponse
	}{
		{"run_ok", ipcResponse{Run: &RunResponse{Results: []BackendImageResult{{ID: 7, Tags: map[TagKey]Scored{
			{Name: "blue_eyes", CatID: 0}: {Score: 0.91, TaggerName: "wd-swinv2"},
		}}}}}},
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
