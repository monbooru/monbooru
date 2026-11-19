//go:build tagger

package tagger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

func RunWorkerServer(ctx context.Context, addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("tagger-worker: dial %q: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := writeFrame(conn, ipcHello{Token: os.Getenv(ipcTokenEnv)}); err != nil {
		return fmt.Errorf("tagger-worker: handshake: %w", err)
	}
	logx.Infof("tagger-worker: connected to parent addr=%s", addr)
	return serveRequests(ctx, conn)
}

// serveRequests runs each call in its own goroutine; the backend bounds
// how many infer at once.
func serveRequests(ctx context.Context, conn net.Conn) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Every write goes through writeMu: replies come from the call
	// goroutines.
	var writeMu sync.Mutex
	var callsMu sync.Mutex
	calls := map[uint64]context.CancelFunc{}
	for {
		var req ipcRequest
		if err := readFrame(conn, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("tagger-worker: read: %w", err)
		}
		switch req.Method {
		case ipcMethodShutdown:
			if err := writeLocked(conn, &writeMu, ipcResponse{ID: req.ID}); err != nil {
				return fmt.Errorf("tagger-worker: shutdown response: %w", err)
			}
			return nil
		case ipcMethodCancel:
			callsMu.Lock()
			if stop, ok := calls[req.ID]; ok {
				stop()
			}
			callsMu.Unlock()
		case ipcMethodRun:
			callCtx, stop := context.WithCancel(ctx)
			callsMu.Lock()
			calls[req.ID] = stop
			callsMu.Unlock()
			wg.Go(func() {
				resp := handleRun(callCtx, req)
				callsMu.Lock()
				delete(calls, req.ID)
				callsMu.Unlock()
				stop()
				_ = writeLocked(conn, &writeMu, resp)
			})
		default:
			if err := writeLocked(conn, &writeMu, handle(req)); err != nil {
				return err
			}
		}
	}
}

func handle(req ipcRequest) ipcResponse {
	switch req.Method {
	case ipcMethodStatus:
		st := defaultBackend.Status()
		return ipcResponse{ID: req.ID, Status: &st}
	case ipcMethodReleaseIdle:
		return ipcResponse{ID: req.ID, Released: defaultBackend.ReleaseIdle(req.IdleAfter)}
	}
	return ipcResponse{ID: req.ID, Err: fmt.Sprintf("unknown method %d", req.Method)}
}

func handleRun(ctx context.Context, req ipcRequest) ipcResponse {
	if req.Run == nil {
		return ipcResponse{ID: req.ID, Err: "run: empty request"}
	}
	resp, err := defaultBackend.Run(ctx, *req.Run)
	if err != nil {
		return ipcResponse{ID: req.ID, Err: err.Error()}
	}
	return ipcResponse{ID: req.ID, Run: &resp}
}

func writeLocked(conn net.Conn, writeMu *sync.Mutex, resp ipcResponse) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return writeFrame(conn, resp)
}
