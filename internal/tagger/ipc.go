//go:build tagger

package tagger

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

type ipcMethod uint8

const (
	ipcMethodRun ipcMethod = iota + 1
	ipcMethodStatus
	ipcMethodReleaseIdle
	ipcMethodShutdown
	// ipcMethodCancel stops the call whose ID it carries.
	ipcMethodCancel
)

// Calls run concurrently over one connection; a reply carries its
// request's ID.
type ipcRequest struct {
	ID        uint64
	Method    ipcMethod
	Run       *RunRequest
	IdleAfter time.Duration
}

// The environment, not argv: /proc/<pid>/environ is owner-only while
// cmdline is world-readable.
const ipcTokenEnv = "MONBOORU_TAGGER_IPC_TOKEN"

const workerLogEnv = "MONBOORU_TAGGER_WORKER_LOG"

const ipcHandshakeTimeout = 5 * time.Second

// Any local process can reach the loopback listener, so a connection that
// cannot echo the spawn secret is dropped before it is sent frame paths.
type ipcHello struct{ Token string }

type ipcResponse struct {
	ID       uint64
	Run      *RunResponse
	Status   *CacheStatus
	Released bool
	Err      string
}

func writeFrame(w io.Writer, payload any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
		return fmt.Errorf("gob encode: %w", err)
	}
	body := buf.Bytes()
	if len(body) > int(^uint32(0)) {
		return fmt.Errorf("ipc frame too large: %d bytes", len(body))
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(body)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// Real frames stay well under a megabyte; the cap bounds what a corrupt
// length header can make readFrame allocate.
const maxFrameBytes uint32 = 64 << 20

func readFrame(r io.Reader, dst any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameBytes {
		return fmt.Errorf("frame body %d bytes exceeds cap %d", n, maxFrameBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("read frame body: %w", err)
	}
	return gob.NewDecoder(bytes.NewReader(body)).Decode(dst)
}

// ipcConn carries the calls to one child and routes each reply to its
// caller; once its reader stops, every call fails with err.
type ipcConn struct {
	net.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan ipcResponse
	err     error
}

func newIPCConn(c net.Conn) *ipcConn {
	ic := &ipcConn{Conn: c, pending: map[uint64]chan ipcResponse{}}
	go ic.readLoop()
	return ic
}

func (c *ipcConn) readLoop() {
	for {
		var resp ipcResponse
		if err := readFrame(c.Conn, &resp); err != nil {
			c.mu.Lock()
			c.err = fmt.Errorf("ipc read: %w", err)
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (c *ipcConn) send(req ipcRequest) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.Conn, req)
}

// call returns ctx's error on a cancel, which the child is told of, and
// any other error when the connection is gone.
func (c *ipcConn) call(ctx context.Context, req ipcRequest) (ipcResponse, error) {
	ch := make(chan ipcResponse, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return ipcResponse{}, c.err
	}
	c.nextID++
	req.ID = c.nextID
	c.pending[req.ID] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, req.ID)
		c.mu.Unlock()
	}
	if err := c.send(req); err != nil {
		forget()
		_ = c.Close()
		return ipcResponse{}, fmt.Errorf("ipc write: %w", err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			c.mu.Lock()
			defer c.mu.Unlock()
			return ipcResponse{}, c.err
		}
		return resp, nil
	case <-ctx.Done():
		forget()
		_ = c.send(ipcRequest{ID: req.ID, Method: ipcMethodCancel})
		return ipcResponse{}, ctx.Err()
	}
}

// ipcBackend runs inference in a child monbooru process: only the child's
// exit gives back the CUDA libraries and primary context it loaded.
type ipcBackend struct {
	// A call holds mu for reading; starting and stopping the child hold it
	// for writing.
	mu       sync.RWMutex
	cmd      *exec.Cmd
	conn     *ipcConn
	inFlight atomic.Int32
	childPID atomic.Int32
	// Overrides the cached snapshot's provider mid-batch: the snapshot
	// may predate a provider switch.
	runProvider atomic.Value
	lastStatus  atomic.Pointer[CacheStatus]
}

// Caller must hold b.mu.
func (b *ipcBackend) ensureRunning() error {
	if b.cmd != nil && b.conn != nil {
		return nil
	}
	token := rand.Text()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen tcp: %w", err)
	}
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	cmd := exec.Command(exe, "tagger-worker", "--addr="+addr)
	cmd.Env = workerEnv(token)
	cmd.Stdout = os.Stdout
	// The parent's writer, so a desktop launch's log file gets the
	// worker's lines too.
	cmd.Stderr = log.Writer()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn tagger-worker: %w", err)
	}
	logx.Infof("tagger-worker: spawned pid=%d addr=%s", cmd.Process.Pid, addr)

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		c, err := acceptWorker(listener, token)
		accepted <- acceptResult{c, err}
	}()

	select {
	case res := <-accepted:
		if res.err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("accept tagger-worker connection: %w", res.err)
		}
		b.cmd = cmd
		b.conn = newIPCConn(res.conn)
		b.childPID.Store(int32(cmd.Process.Pid))
		return nil
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("tagger-worker did not connect within 15s")
	}
}

func acceptWorker(l net.Listener, token string) (net.Conn, error) {
	for {
		c, err := l.Accept()
		if err != nil {
			return nil, err
		}
		if ipcHandshakeOK(c, token) {
			return c, nil
		}
		logx.Warnf("tagger-worker: dropped a connection that failed the handshake")
		_ = c.Close()
	}
}

func workerEnv(token string) []string {
	env := append(os.Environ(), ipcTokenEnv+"="+token)
	if os.Getenv(workerLogEnv) == "" {
		env = append(env, workerLogEnv+"="+logx.Get())
	}
	return env
}

func ipcHandshakeOK(c net.Conn, want string) bool {
	_ = c.SetReadDeadline(time.Now().Add(ipcHandshakeTimeout))
	var hello ipcHello
	if err := readFrame(c, &hello); err != nil {
		return false
	}
	_ = c.SetReadDeadline(time.Time{})
	return subtle.ConstantTimeCompare([]byte(hello.Token), []byte(want)) == 1
}

// Caller must hold b.mu.
func (b *ipcBackend) terminate() {
	if b.cmd == nil {
		return
	}
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Signal(syscall.SIGTERM)
	}
	b.reapLocked(true, "terminated")
}

// dropConnFirst is for an unresponsive child; a graceful shutdown keeps
// the socket open until the child has exited.
func (b *ipcBackend) reapLocked(dropConnFirst bool, verb string) {
	if dropConnFirst && b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	done := make(chan struct{})
	go func() {
		if b.cmd != nil {
			_ = b.cmd.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
		<-done
	}
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	pid := 0
	var ps *os.ProcessState
	if b.cmd != nil && b.cmd.ProcessState != nil {
		ps = b.cmd.ProcessState
		pid = ps.Pid()
	}
	b.cmd = nil
	b.childPID.Store(0)
	b.lastStatus.Store(nil)
	if ps != nil && !ps.Success() {
		if ws, ok := ps.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			logx.Warnf("tagger-worker: %s pid=%d: %s", verb, pid, ps)
			return
		}
	}
	logx.Infof("tagger-worker: %s pid=%d", verb, pid)
}

// Well above a teardown with mallocTrim (a few seconds), short enough
// that a wedged worker can't stall the reclaim ticker or shutdown.
const shortCallTimeout = 30 * time.Second

// Caller must hold b.mu. A failed short call takes the child down.
func (b *ipcBackend) shortCall(req ipcRequest) (ipcResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := b.conn.call(ctx, req)
	if err != nil {
		b.terminate()
	}
	return resp, err
}

func (b *ipcBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	b.runProvider.Store(req.Provider)

	conn, err := b.acquire()
	if err != nil {
		return RunResponse{}, err
	}
	resp, err := conn.call(ctx, ipcRequest{Method: ipcMethodRun, Run: &req})
	b.mu.RUnlock()
	if err != nil {
		if ctx.Err() != nil {
			return RunResponse{}, ctx.Err()
		}
		b.drop(conn)
		return RunResponse{}, err
	}
	if resp.Err != "" {
		return RunResponse{}, errors.New(resp.Err)
	}
	if resp.Run == nil {
		return RunResponse{}, errors.New("tagger-worker returned empty response")
	}
	return *resp.Run, nil
}

// acquire returns the live connection with b.mu held for reading,
// starting the child first when there is none.
func (b *ipcBackend) acquire() (*ipcConn, error) {
	for {
		b.mu.RLock()
		if b.conn != nil {
			return b.conn, nil
		}
		b.mu.RUnlock()
		b.mu.Lock()
		err := b.ensureRunning()
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

// drop takes down a child whose connection failed, unless another call
// already replaced it.
func (b *ipcBackend) drop(conn *ipcConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == conn {
		b.terminate()
	}
}

// While a call is in flight Status answers from the last snapshot
// instead of asking the busy child.
func (b *ipcBackend) Status() CacheStatus {
	if b.inFlight.Load() > 0 {
		provider, _ := b.runProvider.Load().(string)
		provider = cmp.Or(provider, "cpu")
		if cached := b.lastStatus.Load(); cached != nil {
			snap := *cached
			snap.InUse = true
			snap.Provider = provider
			return snap
		}
		return CacheStatus{Loaded: true, InUse: true, Provider: provider}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return CacheStatus{}
	}
	resp, err := b.shortCall(ipcRequest{Method: ipcMethodStatus})
	if err != nil || resp.Status == nil {
		return CacheStatus{}
	}
	snap := *resp.Status
	b.lastStatus.Store(&snap)
	return snap
}

func (b *ipcBackend) ReleaseIdle(after time.Duration) bool {
	if b.inFlight.Load() > 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return false
	}
	resp, err := b.shortCall(ipcRequest{Method: ipcMethodReleaseIdle, IdleAfter: after})
	if err != nil {
		return false
	}
	if resp.Released {
		b.terminate()
	}
	return resp.Released
}

// A call in flight keeps its child: waiting would hang the caller and then
// kill the worker the next call needs. Idle release takes the child once
// the run ends.
func (b *ipcBackend) ReleaseAll() {
	if b.inFlight.Load() > 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.shutdownLocked()
}

// Caller must hold b.mu.
func (b *ipcBackend) shutdownLocked() {
	if b.conn == nil {
		return
	}
	resp, err := b.shortCall(ipcRequest{Method: ipcMethodShutdown})
	if err != nil {
		return
	}
	if resp.Err != "" {
		b.terminate()
		return
	}

	b.reapLocked(false, "released")
}

func (b *ipcBackend) WorkerPID() (int, bool) {
	pid := b.childPID.Load()
	if pid == 0 {
		return 0, false
	}
	return int(pid), true
}
