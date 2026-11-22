package tagger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestACallNoWorkerHoldsWholeIsSplitOverTheWorkersThatHoldItsParts(t *testing.T) {
	joytag := fakeModel{name: "joytag", labels: labelsOf("a")}
	a := &counting{f: newFake(t, wd())}
	b := &counting{f: newFake(t, joytag)}
	p := usePool(t, "",
		WorkerDef{Name: "a", URL: serve(t, a), Token: "tok"},
		WorkerDef{Name: "b", URL: serve(t, b), Token: "tok"})
	if n := p.Slots([][]string{{"wd", "joytag"}}, nil); n != 4 {
		t.Errorf("slots = %d, want a's 2 + b's 2", n)
	}
	resp, by, err := p.Tag(context.Background(), worker.TagRequest{Models: []string{"joytag", "wd"}, Priority: worker.PriorityBatch, Frames: worker.FrameSpec{Size: 8}}, [][]byte{make([]byte, 8*8*3)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.n() != 1 || b.n() != 1 || by == "" {
		t.Errorf("calls a %d, b %d, by %q, want one each", a.n(), b.n(), by)
	}
	var got []string
	for _, r := range resp.Results {
		got = append(got, r.Model)
	}
	if want := []string{"joytag", "wd"}; !slices.Equal(got, want) {
		t.Errorf("results = %v, want %v", got, want)
	}
}

func TestLoadCountsTheCallsWaitingAndTheSlotsFree(t *testing.T) {
	held := &counting{f: newFake(t, wd()), gate: make(chan struct{})}
	p := usePool(t, "", WorkerDef{Name: "a", URL: serve(t, held), Token: "tok"})
	if l := p.Load(); l.Busy || l.Waiting != 0 || l.Free != 2 {
		t.Fatalf("idle load = %+v, want two free slots and nothing waiting", l)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
				t.Error(err)
			}
		})
	}
	waitFor(t, func() bool { return p.Load().Waiting == 1 })
	if l := p.Load(); !l.Busy || l.Free != 0 {
		t.Errorf("load with three calls on two slots = %+v, want busy, none free, one waiting", l)
	}
	close(held.gate)
	wg.Wait()
	if l := p.Load(); l.Busy || l.Waiting != 0 || l.Free != 2 {
		t.Errorf("load after the calls = %+v", l)
	}
}

func TestAFarWorkerThatListsPNGGetsItsFramesCompressed(t *testing.T) {
	lists, plain := newFake(t, wd()), newFake(t, wd())
	lists.Info.Formats = []string{worker.FormatRGB8, worker.FormatPNG}
	lastFormat := func(f *worker.Fake) string {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		return f.Calls[len(f.Calls)-1].Frames.Format
	}
	for _, tc := range []struct {
		name string
		f    *worker.Fake
		far  bool
		want string
	}{
		{"near, lists png", lists, false, worker.FormatRGB8},
		{"far, lists png", lists, true, worker.FormatPNG},
		{"far, an older worker", plain, true, worker.FormatRGB8},
	} {
		p := usePool(t, "", WorkerDef{Name: "a", URL: serve(t, tc.f), Token: "tok"})
		p.far = func(context.Context, string) bool { return tc.far }
		p.Refresh(context.Background())
		if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := lastFormat(tc.f); got != tc.want {
			t.Errorf("%s: frames went as %q, want %q", tc.name, got, tc.want)
		}

		p.Set([]WorkerDef{{Name: "a", URL: p.Workers()[0].URL, Token: "tok"}})
		if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
			t.Fatalf("%s after a config write: %v", tc.name, err)
		}
		if got := lastFormat(tc.f); got != tc.want {
			t.Errorf("%s after a config write: frames went as %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFarAddress(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		"http://127.0.0.1:8457":       false,
		"http://[::1]:8457":           false,
		"http://192.168.1.20:8457":    false,
		"http://10.0.0.2:8457":        false,
		"http://169.254.3.4:8457":     false,
		"https://203.0.113.9":         true,
		"http://[2001:db8::1]:8457":   true,
		"http://no-such-host.invalid": false,
		"not a url":                   false,
	} {
		if got := farAddress(context.Background(), raw); got != want {
			t.Errorf("farAddress(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestAWorkerBehindAGatewayIsCalledWithBothTokens(t *testing.T) {
	f := newFake(t, wd())
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p := usePool(t, "", WorkerDef{Name: "rented", URL: srv.URL, Token: "tok", Gateway: "gateway-key"})
	if _, err := tagOne(t, p, worker.PriorityBatch); err != nil {
		t.Fatalf("the fake reads the worker's token from its header: %v", err)
	}
	if bearer != "Bearer gateway-key" {
		t.Errorf("Authorization = %q, want the gateway's key", bearer)
	}
}

type asleep struct {
	f    *worker.Fake
	mu   sync.Mutex
	hits int
	wake int
}

func (a *asleep) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.hits++
	up := a.hits >= a.wake
	a.mu.Unlock()
	if !up {
		http.Error(w, `{"error":"no workers available"}`, http.StatusServiceUnavailable)
		return
	}
	a.f.ServeHTTP(w, r)
}

func (a *asleep) seen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits
}

func TestAnOnDemandWorkerIsLeftAloneUntilAJobNamesIt(t *testing.T) {
	prev := wakeEvery
	wakeEvery = 5 * time.Millisecond
	t.Cleanup(func() { wakeEvery = prev })
	rented := &asleep{f: newFake(t, wd()), wake: 3}
	p := usePool(t, "",
		WorkerDef{Name: "nas", URL: serve(t, newFake(t, wd())), Token: "tok"},
		WorkerDef{Name: "rented", URL: serve(t, rented), Token: "tok", OnDemand: true})
	p.Refresh(context.Background())
	p.Glance(context.Background())
	p.Peek(context.Background())
	if rented.seen() != 0 {
		t.Fatalf("a refresh reached the on-demand worker %d times", rented.seen())
	}
	names := func() (out []string) {
		for _, w := range p.JobWorkers() {
			out = append(out, w.Name)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{"nas", "rented"}) {
		t.Errorf("job workers = %v, want the on-demand one offered though never seen", got)
	}
	if _, name, err := p.Tag(context.Background(), worker.TagRequest{Models: []string{"wd"}, Frames: worker.FrameSpec{Size: 8}}, [][]byte{make([]byte, 192)}, nil); err != nil || name != "nas" {
		t.Fatalf("a call that names no worker went to %q, %v", name, err)
	}
	if rented.seen() != 0 {
		t.Fatal("a call that names no worker reached the on-demand one")
	}

	only := []string{"rented"}
	if n := p.Slots([][]string{{"wd"}}, only); n != 0 {
		t.Errorf("slots before it was ever read = %d: nothing says it holds wd yet", n)
	}
	var ticks int
	if err := p.Wake(context.Background(), only, func(string, time.Duration) { ticks++ }); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if rented.seen() < 3 {
		t.Errorf("the worker was asked %d times; it answers at the third", rented.seen())
	}
	if n := p.Slots([][]string{{"wd"}}, only); n != 2 {
		t.Errorf("slots once it is up = %d, want its 2", n)
	}
	if _, name, err := p.Tag(context.Background(), worker.TagRequest{Models: []string{"wd"}, Frames: worker.FrameSpec{Size: 8}}, [][]byte{make([]byte, 192)}, only); err != nil || name != "rented" {
		t.Errorf("a call that names it went to %q, %v", name, err)
	}
	if !p.OnDemand("rented") || p.OnDemand("nas") {
		t.Error("OnDemand does not tell the two apart")
	}
	for _, v := range p.Workers() {
		if v.Local && v.Standby {
			t.Error("an on-demand worker made the local one stand by")
		}
	}
}

func TestWakeGivesUpOnAWorkerThatRefusesTheToken(t *testing.T) {
	f := newFake(t, wd())
	p := usePool(t, "", WorkerDef{Name: "rented", URL: serve(t, f), Token: "wrong", OnDemand: true})
	err := p.Wake(context.Background(), []string{"rented"}, nil)
	if err == nil || !strings.Contains(err.Error(), "worker rented did not start") {
		t.Fatalf("wake = %v", err)
	}
	if v := p.Workers()[0]; v.Up || !v.Checked {
		t.Errorf("after a failed start: %+v, want it down", v)
	}
}

func TestSpell(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{400 * time.Millisecond: "0 s", 34 * time.Second: "34 s", 72 * time.Second: "1 min 12 s", time.Hour: "60 min 0 s"} {
		if got := Spell(d); got != want {
			t.Errorf("Spell(%v) = %q, want %q", d, got, want)
		}
	}
}
