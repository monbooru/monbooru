package tagger

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/worker"
)

func tagOne(t *testing.T, p *Pool, priority string) (worker.TagResponse, error) {
	t.Helper()
	resp, _, err := p.Tag(context.Background(), worker.TagRequest{Models: []string{"wd"}, Priority: priority, Frames: worker.FrameSpec{Size: 8}}, [][]byte{make([]byte, 8*8*3)}, nil)
	return resp, err
}

type counting struct {
	f     *worker.Fake
	mu    sync.Mutex
	calls int
	gate  chan struct{}
}

func (c *counting) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == worker.PathTag {
		c.mu.Lock()
		c.calls++
		gate := c.gate
		c.mu.Unlock()
		if gate != nil {
			<-gate
		}
	}
	c.f.ServeHTTP(w, r)
}

func (c *counting) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func wd() fakeModel { return fakeModel{name: "wd", labels: labelsOf("a")} }

func TestATransportFailureGoesToAnotherWorker(t *testing.T) {
	gone := &counting{f: newFake(t, wd())}
	dead := serve(t, gone)
	good := &counting{f: newFake(t, wd())}
	p := usePool(t, "", WorkerDef{Name: "a", URL: dead, Token: "tok"}, WorkerDef{Name: "b", URL: serve(t, good), Token: "tok"})

	p.mu.Lock()
	p.workers[0].client.BaseURL = "http://127.0.0.1:1"
	p.mu.Unlock()
	if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if good.n() != 1 {
		t.Errorf("b served %d calls, want 1", good.n())
	}
	for _, w := range p.Workers() {
		if w.Name == "a" && w.Up {
			t.Error("a is still up after its call failed")
		}
	}
}

func TestAWorkersRefusalIsFinal(t *testing.T) {
	f := newFake(t, wd())
	p := usePool(t, "", WorkerDef{Name: "a", URL: serve(t, f), Token: "tok"})
	f.Mu.Lock()
	f.Models = nil
	f.Mu.Unlock()
	var se *worker.StatusError
	if _, err := tagOne(t, p, worker.PriorityBatch); !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("tag: %v, want the worker's 404", err)
	}
}

func TestTheLocalWorkerStandsByWhileARemoteHoldsTheModel(t *testing.T) {
	local := &counting{f: newFake(t, wd())}
	remote := &counting{f: newFake(t, wd())}
	p := usePool(t, "",
		WorkerDef{Name: "gpu-box", URL: serve(t, remote), Token: "tok"},
		WorkerDef{Name: LocalWorker, Local: true, URL: serve(t, local), Token: "tok"})
	if p.Workers()[0].Name != LocalWorker || !p.Workers()[0].Standby {
		t.Fatalf("workers = %+v, want the local one first, standing by", p.Workers())
	}
	if _, err := tagOne(t, p, worker.PriorityBatch); err != nil || remote.n() != 1 || local.n() != 0 {
		t.Fatalf("with the remote up: %v, remote %d, local %d", err, remote.n(), local.n())
	}
	remote.f.Mu.Lock()
	remote.f.Token = "changed"
	remote.f.Mu.Unlock()
	p.Refresh(context.Background())
	if _, err := tagOne(t, p, worker.PriorityBatch); err != nil || local.n() != 1 {
		t.Errorf("with the remote down: %v, local %d", err, local.n())
	}
}

func TestOneNameWithTwoDigestsRunsOnTheFirst(t *testing.T) {
	first := &counting{f: newFake(t, wd())}
	other := newFake(t, wd())
	other.Models[0].Digest = "d-other"
	second := &counting{f: other}
	p := usePool(t, "",
		WorkerDef{Name: "a", URL: serve(t, first), Token: "tok"},
		WorkerDef{Name: "b", URL: serve(t, second), Token: "tok"})
	for range 3 {
		if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
			t.Fatal(err)
		}
	}
	if first.n() != 3 || second.n() != 0 {
		t.Errorf("calls a %d, b %d, want every call on a", first.n(), second.n())
	}
	for _, s := range DiscoverTaggers(&config.Config{}) {
		if s.Name == "wd" && !s.Differs {
			t.Errorf("wd = %+v, want it flagged as differing", s)
		}
	}
	if p.Slots([][]string{{"wd"}}, nil) != 2 {
		t.Errorf("slots = %d, want only a's 2", p.Slots([][]string{{"wd"}}, nil))
	}
}

func TestWhatAWorkerSaidSurvivesARestartWithItOff(t *testing.T) {
	dir := t.TempDir()
	f := newFake(t, fakeModel{name: "wd", labels: labelsOf("1girl", "solo"), mappings: []worker.Mapping{{Source: "solo", Category: ""}}})
	url := serve(t, f)
	usePool(t, dir, WorkerDef{Name: "a", URL: url, Token: "tok"})
	f.Mu.Lock()
	f.Token = "changed"
	f.Mu.Unlock()
	time.Sleep(50 * time.Millisecond)

	usePool(t, dir, WorkerDef{Name: "a", URL: url, Token: "tok"})
	if !WorkersDescribed() {
		t.Error("the restarted pool does not know what a holds")
	}
	views, err := BrowseLabels(t.TempDir(), "wd", canonicalCatIDs)
	if err != nil || len(views) != 2 {
		t.Fatalf("labels with the worker off: %+v, %v", views, err)
	}
	for _, s := range DiscoverTaggers(&config.Config{}) {
		if s.Name == "wd" && (s.Available || s.Missing) {
			t.Errorf("wd = %+v, want held but unavailable", s)
		}
	}
}

func TestAnInteractiveCallPassesTheBatchOnes(t *testing.T) {
	c := &counting{f: newFake(t, wd()), gate: make(chan struct{})}
	c.f.Info.Slots = 1
	p := usePool(t, "", WorkerDef{Name: "a", URL: serve(t, c), Token: "tok"})

	order := make(chan string, 3)
	var wg sync.WaitGroup
	call := func(name, priority string) {
		wg.Go(func() {
			if _, err := tagOne(t, p, priority); err != nil {
				t.Error(err)
			}
			order <- name
		})
	}
	call("first", worker.PriorityBatch)
	waitFor(t, func() bool { return c.n() == 1 })
	call("batch", worker.PriorityBatch)
	time.Sleep(20 * time.Millisecond)
	call("interactive", worker.PriorityInteractive)
	time.Sleep(20 * time.Millisecond)
	close(c.gate)
	wg.Wait()
	close(order)
	var got []string
	for n := range order {
		got = append(got, n)
	}
	if strings.Join(got, " ") != "first interactive batch" {
		t.Errorf("served %v", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoWorkerHoldingTheModelIsAnErrorAtOnce(t *testing.T) {
	p := usePool(t, "")
	start := time.Now()
	if _, err := tagOne(t, p, worker.PriorityBatch); err == nil || time.Since(start) > time.Second {
		t.Errorf("tag with no worker: %v after %s", err, time.Since(start))
	}
}

func TestAGlanceShowsANewModelAndLeavesTheStateAlone(t *testing.T) {
	f := newFake(t, wd())
	slow := newFake(t, wd())
	hold := make(chan struct{})
	var held atomic.Bool
	p := usePool(t, "",
		WorkerDef{Name: "a", URL: serve(t, f), Token: "tok"},
		WorkerDef{Name: "slow", URL: serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if held.Load() {
				<-hold
			}
			slow.ServeHTTP(w, r)
		})), Token: "tok"})
	t.Cleanup(func() { close(hold) })
	f.Mu.Lock()
	f.Models = append(f.Models, worker.Model{Name: "joytag", Kind: worker.KindTagger, Digest: "d-joytag", Ready: true})
	f.Mu.Unlock()
	held.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p.Glance(ctx)
	ws := p.Workers()
	if len(ws[0].Models) != 2 {
		t.Errorf("a holds %d models after a glance, want the new one too", len(ws[0].Models))
	}
	if !ws[1].Up {
		t.Error("a glance that timed out marked the worker down")
	}
}

func TestAPeekReadsTheLoadedModelsAndOnlyThat(t *testing.T) {
	f := newFake(t, wd())
	p := usePool(t, "", WorkerDef{Name: "a", URL: serve(t, f), Token: "tok"})
	f.Mu.Lock()
	f.Info.Loaded, f.Info.Busy = []string{"wd-swinv2"}, true
	f.Models = append(f.Models, worker.Model{Name: "joytag", Kind: worker.KindTagger, Digest: "d-joytag", Ready: true})
	f.Mu.Unlock()

	p.Peek(context.Background())
	w := p.Workers()[0]
	if !slices.Equal(w.Info.Loaded, []string{"wd-swinv2"}) || !w.Info.Busy {
		t.Errorf("after a peek: loaded %v busy %v, want wd-swinv2 and busy", w.Info.Loaded, w.Info.Busy)
	}
	if len(w.Models) != 1 {
		t.Errorf("a peek read the models too: %d", len(w.Models))
	}
}

func TestAWorkerAddedWhileRunningIsLookedAtAtOnce(t *testing.T) {
	p := usePool(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx, time.Hour)
	p.Set([]WorkerDef{{Name: "a", URL: serve(t, newFake(t, wd())), Token: "tok"}})
	waitFor(t, func() bool { ws := p.Workers(); return len(ws) == 1 && ws[0].Up })
}

func TestAJobNamingItsWorkersSharesThemTheLocalOneIncluded(t *testing.T) {
	local := &counting{f: newFake(t, wd())}
	left := &counting{f: newFake(t, wd())}
	named := &counting{f: newFake(t, wd()), gate: make(chan struct{})}
	p := usePool(t, "",
		WorkerDef{Name: LocalWorker, Local: true, URL: serve(t, local), Token: "tok"},
		WorkerDef{Name: "left-out", URL: serve(t, left), Token: "tok"},
		WorkerDef{Name: "gpu-box", URL: serve(t, named), Token: "tok"})
	only := []string{LocalWorker, "gpu-box"}
	if n := p.Slots([][]string{{"wd"}}, only); n != 4 {
		t.Errorf("slots of the named workers = %d, want 2 + 2", n)
	}
	if n := p.Slots([][]string{{"wd"}}, nil); n != 4 {
		t.Errorf("slots with no name = %d, want the two remote workers' only", n)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, _, err := p.Tag(context.Background(), worker.TagRequest{Models: []string{"wd"}, Priority: worker.PriorityBatch, Frames: worker.FrameSpec{Size: 8}}, [][]byte{make([]byte, 8*8*3)}, only); err != nil {
				t.Error(err)
			}
		})
	}
	waitFor(t, func() bool { return named.n() == 2 && local.n() == 1 })
	close(named.gate)
	wg.Wait()
	if left.n() != 0 {
		t.Errorf("the worker left out took %d calls", left.n())
	}
}

func TestAJobWhoseWorkersHoldNoneOfItsModelsFails(t *testing.T) {
	p := usePool(t, "",
		WorkerDef{Name: "a", URL: serve(t, newFake(t, wd())), Token: "tok"},
		WorkerDef{Name: "b", URL: serve(t, newFake(t, fakeModel{name: "joytag", labels: labelsOf("a")})), Token: "tok"})
	_, _, err := p.Tag(context.Background(), worker.TagRequest{Models: []string{"wd"}, Frames: worker.FrameSpec{Size: 8}}, [][]byte{make([]byte, 8*8*3)}, []string{"b"})
	if !errors.Is(err, errNoWorker) {
		t.Errorf("tag on b only: %v, want no worker", err)
	}
}

func TestJobWorkersAreTheOnesUpWithWhatTheyCanRun(t *testing.T) {
	gone := serve(t, newFake(t, wd()))
	p := usePool(t, "",
		WorkerDef{Name: LocalWorker, Local: true, URL: serve(t, newFake(t, wd())), Token: "tok"},
		WorkerDef{Name: "both", URL: serve(t, newFake(t, wd(), fakeModel{name: "joytag", labels: labelsOf("a")})), Token: "tok"},
		WorkerDef{Name: "paused", URL: serve(t, newFake(t, wd())), Token: "tok", Paused: true},
		WorkerDef{Name: "gone", URL: gone, Token: "tok"})
	p.markDown(p.workers[3], errors.New("gone"))
	got := p.JobWorkers()
	var names []string
	for _, w := range got {
		names = append(names, w.Name+":"+strings.Join(w.Holds, "+"))
	}
	if want := []string{"local:wd", "both:wd+joytag"}; !slices.Equal(names, want) {
		t.Errorf("job workers = %v, want %v", names, want)
	}
	if !got[0].Local || got[1].Slots != 2 {
		t.Errorf("job workers = %+v", got)
	}
}

func TestACallGoesToTheWorkerWithTheMostFreeSlots(t *testing.T) {
	busy := &counting{f: newFake(t, wd()), gate: make(chan struct{})}
	other := &counting{f: newFake(t, wd()), gate: make(chan struct{})}
	p := usePool(t, "",
		WorkerDef{Name: "a", URL: serve(t, busy), Token: "tok"},
		WorkerDef{Name: "b", URL: serve(t, other), Token: "tok"})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
				t.Error(err)
			}
		})
	}
	waitFor(t, func() bool { return busy.n()+other.n() == 2 })
	if busy.n() != 1 || other.n() != 1 {
		t.Errorf("calls a %d, b %d, want one each", busy.n(), other.n())
	}
	close(busy.gate)
	close(other.gate)
	wg.Wait()
}

func TestAJobsSlotsCountEveryWorkerThatTakesOneOfItsCalls(t *testing.T) {
	joytag := fakeModel{name: "joytag", labels: labelsOf("a")}
	pixai := fakeModel{name: "pixai", size: 16, labels: labelsOf("a")}
	p := usePool(t, "",
		WorkerDef{Name: "all", URL: serve(t, newFake(t, wd(), joytag, pixai)), Token: "tok"},
		WorkerDef{Name: "two", URL: serve(t, newFake(t, wd(), joytag)), Token: "tok"})
	if n := p.Slots([][]string{{"wd", "joytag"}, {"pixai"}}, nil); n != 4 {
		t.Errorf("slots = %d, want both workers' 2 + 2", n)
	}
}
