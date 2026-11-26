// Package taggertest gives other packages' tests a fake monworker in the
// tagger pool.
package taggertest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tagger"
)

// Worker is a fake monworker holding one tagger per name, each taking
// 8 px frames, which scores every one of its labels 0.9 on each frame.
type Worker struct {
	fake *worker.Fake
	// stop releases the blocked calls when the test ends, ahead of the
	// server's close, which waits for them.
	stop    chan struct{}
	mu      sync.Mutex
	calls   int
	blocked chan struct{}
	block   bool
}

// Use makes the pool one worker holding taggers, a name and its labels
// each, until the test ends.
func Use(t testing.TB, taggers map[string][]string) *Worker {
	t.Helper()
	return UsePool(t, taggers, tagger.WorkerDef{Name: "fake"})[0]
}

// UsePool makes the pool one worker per def, each holding the same
// taggers; a def's URL and token are its worker's.
func UsePool(t testing.TB, taggers map[string][]string, defs ...tagger.WorkerDef) []*Worker {
	t.Helper()
	out := make([]*Worker, len(defs))
	for i := range defs {
		out[i] = newWorker(t, taggers)
		srv := httptest.NewServer(out[i])
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(out[i].stop) })
		defs[i].URL, defs[i].Token = srv.URL, "tok"
	}
	p := tagger.NewPool("")
	p.Set(defs)
	p.Refresh(context.Background())
	tagger.SetPool(p)
	t.Cleanup(func() { tagger.SetPool(tagger.NewPool("")) })
	return out
}

func newWorker(t testing.TB, taggers map[string][]string) *Worker {
	f := &worker.Fake{Token: "tok", Info: worker.Info{Name: "fake", Slots: 2}, Labels: map[string]worker.Labels{}}
	for name, labels := range taggers {
		etag := fmt.Sprintf("%q", t.Name()+"/"+name+fmt.Sprint(labels))
		f.Models = append(f.Models, worker.Model{
			Name: name, Kind: worker.KindTagger, Digest: "d-" + name, ModelFile: "model.onnx", LabelFile: "tags.csv",
			Input: worker.Input{Size: 8, Fill: [3]uint8{255, 255, 255}}, Labels: len(labels), LabelsETag: etag, Ready: true,
		})
		l := worker.Labels{Model: name, ETag: etag, CategoryScheme: "wd14_numeric"}
		for _, n := range labels {
			l.Labels = append(l.Labels, worker.Label{Name: n})
		}
		f.Labels[name] = l
	}
	f.Score = func(model string, _ []byte) worker.FrameScores {
		out := worker.FrameScores{}
		for i := range f.Labels[model].Labels {
			out.I = append(out.I, int32(i))
			out.S = append(out.S, 0.9)
		}
		return out
	}
	return &Worker{fake: f, stop: make(chan struct{})}
}

func (w *Worker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == worker.PathTag {
		w.mu.Lock()
		w.calls++
		block, blocked := w.block, w.blocked
		w.mu.Unlock()
		if block {
			select {
			case <-blocked:
			default:
				close(blocked)
			}
			select {
			case <-r.Context().Done():
			case <-w.stop:
				http.Error(rw, `{"error":"stopped"}`, http.StatusServiceUnavailable)
			}
			return
		}
	}
	w.fake.ServeHTTP(rw, r)
}

// Block makes every later tag call wait for its cancel; the channel
// closes once one is waiting.
func (w *Worker) Block() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.block, w.blocked = true, make(chan struct{})
	return w.blocked
}

// Refuse answers every frame with err, as a model that cannot read it.
func (w *Worker) Refuse(err string) {
	w.fake.Mu.Lock()
	w.fake.Score = func(string, []byte) worker.FrameScores { return worker.FrameScores{Err: err} }
	w.fake.Mu.Unlock()
}

func (w *Worker) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// Fake is the worker behind w, for a test to change what it holds under
// its Mu; Refresh then makes the pool read it again.
func (w *Worker) Fake() *worker.Fake { return w.fake }

func (w *Worker) Refresh() {
	p := tagger.CurrentPool()
	p.Refresh(context.Background())
}
