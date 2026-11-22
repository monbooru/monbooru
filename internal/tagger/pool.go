package tagger

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/worker"
)

// LocalWorker names the worker monbooru runs itself.
const LocalWorker = config.LocalWorkerName

type WorkerDef struct {
	Name   string
	URL    string
	Token  string
	Local  bool
	Paused bool
}

// What a worker last said of itself, kept on disk so the taggers list,
// Configure and the mappings open while it is off.
type workerCache struct {
	Info    worker.Info              `json:"info"`
	Models  []worker.Model           `json:"models"`
	Catalog []worker.CatalogEntry    `json:"catalog"`
	Labels  map[string]worker.Labels `json:"labels"`
	Seen    time.Time                `json:"seen"`
}

type poolWorker struct {
	def    WorkerDef
	client worker.Client
	cache  workerCache
	// up is the last health check's answer, or a call's; a worker never
	// checked is tried.
	up        bool
	checked   bool
	downSince time.Time
	err       string
	inUse     int
}

func (w *poolWorker) slots() int { return max(1, w.cache.Info.Slots) }

// Pool is every worker monbooru knows, the local one first. A call goes to
// one that holds its models; nothing waits for a worker that is down.
type Pool struct {
	mu      sync.Mutex
	free    *sync.Cond
	dir     string
	workers []*poolWorker
	// interactive counts the calls of a person waiting, which pass the
	// batch ones to a free slot.
	interactive int
	// waiting counts the calls waiting for a slot.
	waiting    int
	httpClient *http.Client
	// changed wakes Run when Set brings a worker it has not looked at.
	changed chan struct{}
}

// NewPool keeps each worker's descriptions in dir; "" keeps them in memory.
func NewPool(dir string) *Pool {
	p := &Pool{dir: dir, httpClient: &http.Client{Timeout: 10 * time.Minute}, changed: make(chan struct{}, 1)}
	p.free = sync.NewCond(&p.mu)
	return p
}

var (
	poolMu     sync.RWMutex
	activePool = NewPool("")
)

func SetPool(p *Pool) {
	poolMu.Lock()
	activePool = p
	poolMu.Unlock()
}

func currentPool() *Pool {
	poolMu.RLock()
	defer poolMu.RUnlock()
	return activePool
}

func CurrentPool() *Pool { return currentPool() }

func (p *Pool) cachePath(name string) string {
	if p.dir == "" {
		return ""
	}
	return filepath.Join(p.dir, name+".json")
}

// Set replaces the worker list; a worker that stays keeps what it said.
func (p *Pool) Set(defs []WorkerDef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make([]*poolWorker, 0, len(defs))
	for _, d := range defs {
		i := slices.IndexFunc(p.workers, func(w *poolWorker) bool { return w.def.Name == d.Name })
		var w *poolWorker
		if i >= 0 {
			w = p.workers[i]
			if w.def.URL != d.URL || w.def.Token != d.Token {
				w.checked = false
			}
		} else {
			w = &poolWorker{}
			w.cache = p.loadCache(d.Name)
		}
		w.def = d
		w.client = worker.Client{BaseURL: d.URL, Token: d.Token, HTTP: p.httpClient}
		next = append(next, w)
	}
	slices.SortStableFunc(next, func(a, b *poolWorker) int {
		switch {
		case a.def.Local == b.def.Local:
			return 0
		case a.def.Local:
			return -1
		}
		return 1
	})
	p.workers = next
	p.free.Broadcast()
	if slices.ContainsFunc(next, func(w *poolWorker) bool { return !w.checked }) {
		select {
		case p.changed <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) loadCache(name string) workerCache {
	var c workerCache
	path := p.cachePath(name)
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	if err := json.Unmarshal(data, &c); err != nil {
		logx.Warnf("tagger: %s: %v", path, err)
		return workerCache{}
	}
	return c
}

func (p *Pool) saveCache(name string, c workerCache) {
	path := p.cachePath(name)
	if path == "" {
		return
	}
	data, err := json.Marshal(c)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = fsx.WriteFile(path, data)
		}
	}
	if err != nil {
		logx.Warnf("tagger: keep what worker %s said: %v", name, err)
	}
}

// Forget drops a worker's kept descriptions.
func (p *Pool) Forget(name string) {
	if path := p.cachePath(name); path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logx.Warnf("tagger: %s: %v", path, err)
		}
	}
}

// Refresh asks every worker that is not paused what it holds; one that
// does not answer is down until the next refresh.
func (p *Pool) Refresh(ctx context.Context) { p.refresh(ctx, false) }

// Glance reads again what the workers that are up hold, so a page shows a
// model copied in a moment ago; one that does not answer in time keeps
// its state for the next Refresh to settle.
func (p *Pool) Glance(ctx context.Context) { p.refresh(ctx, true) }

// Peek reads again what the workers that are up say of themselves, for a
// block that polls: their state and the models loaded now. What they hold
// waits for the next Refresh, and one that does not answer keeps its state.
func (p *Pool) Peek(ctx context.Context) {
	p.mu.Lock()
	var ws []*poolWorker
	for _, w := range p.workers {
		if w.up && !w.def.Paused && w.def.URL != "" {
			ws = append(ws, w)
		}
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Go(func() {
			p.mu.Lock()
			client := w.client
			p.mu.Unlock()
			info, err := client.Info(ctx)
			if err != nil {
				return
			}
			p.mu.Lock()
			w.cache.Info = info
			p.mu.Unlock()
		})
	}
	wg.Wait()
}

func (p *Pool) refresh(ctx context.Context, glance bool) {
	p.mu.Lock()
	ws := slices.Clone(p.workers)
	up := map[*poolWorker]bool{}
	for _, w := range ws {
		up[w] = w.up
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, w := range ws {
		if w.def.Paused || w.def.URL == "" || glance && !up[w] {
			continue
		}
		wg.Go(func() { p.refreshOne(ctx, w, glance) })
	}
	wg.Wait()
}

func (p *Pool) refreshOne(ctx context.Context, w *poolWorker, glance bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	p.mu.Lock()
	client, prev := w.client, w.cache
	p.mu.Unlock()
	next, err := describe(ctx, client, prev)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil && glance {
		return
	}
	w.checked = true
	if err != nil {
		if w.up || w.downSince.IsZero() {
			w.downSince = time.Now()
			logx.Infof("tagger: worker %s is down: %v", w.def.Name, err)
		}
		w.up, w.err = false, err.Error()
		return
	}
	if !w.up && !w.downSince.IsZero() {
		logx.Infof("tagger: worker %s is up", w.def.Name)
	}
	w.up, w.err, w.downSince = true, "", time.Time{}
	w.cache = next
	p.free.Broadcast()
	go p.saveCache(w.def.Name, next)
}

// describe reads what a worker holds, fetching only the labels whose etag
// moved.
func describe(ctx context.Context, c worker.Client, prev workerCache) (workerCache, error) {
	next := workerCache{Labels: map[string]worker.Labels{}, Seen: time.Now()}
	var err error
	if next.Info, err = c.Info(ctx); err != nil {
		return prev, err
	}
	if next.Models, err = c.Models(ctx); err != nil {
		return prev, err
	}
	if next.Catalog, err = c.Catalog(ctx); err != nil {
		return prev, err
	}
	for _, m := range next.Models {
		if m.LabelsETag == "" {
			continue
		}
		old, ok := prev.Labels[m.Name]
		if ok && old.ETag == m.LabelsETag {
			next.Labels[m.Name] = old
			continue
		}
		l, changed, err := c.Labels(ctx, m.Name, old.ETag)
		if err != nil {
			return prev, err
		}
		if !changed {
			l = old
		}
		next.Labels[m.Name] = l
	}
	return next, nil
}

type WorkerView struct {
	Name      string
	URL       string
	Local     bool
	Paused    bool
	Up        bool
	Checked   bool
	Standby   bool
	InUse     bool
	DownSince time.Time
	Err       string
	Info      worker.Info
	Models    []worker.Model
	Seen      time.Time
}

func (p *Pool) Workers() []WorkerView {
	p.mu.Lock()
	defer p.mu.Unlock()
	remoteUp := slices.ContainsFunc(p.workers, func(w *poolWorker) bool { return !w.def.Local && !w.def.Paused && w.up })
	out := make([]WorkerView, 0, len(p.workers))
	for _, w := range p.workers {
		out = append(out, WorkerView{
			Name: w.def.Name, URL: w.def.URL, Local: w.def.Local, Paused: w.def.Paused,
			Up: w.up, Checked: w.checked, Standby: w.def.Local && remoteUp, InUse: w.inUse > 0,
			DownSince: w.downSince, Err: w.err, Info: w.cache.Info, Models: slices.Clone(w.cache.Models), Seen: w.cache.Seen,
		})
	}
	return out
}

// heldModel is a model as the pool sees it: the worker whose copy counts
// (the first that lists it), whether a worker that holds that copy can run
// it now, and whether another worker's copy differs.
type heldModel struct {
	model   worker.Model
	labels  worker.Labels
	owner   string
	usable  bool
	differs bool
	catalog *worker.CatalogEntry
}

// held lists every model a worker holds, in worker order. Caller holds
// p.mu.
func (p *Pool) held() ([]string, map[string]*heldModel) {
	var order []string
	out := map[string]*heldModel{}
	for _, w := range p.workers {
		for _, m := range w.cache.Models {
			h, ok := out[m.Name]
			if !ok {
				h = &heldModel{model: m, labels: w.cache.Labels[m.Name], owner: w.def.Name}
				out[m.Name] = h
				order = append(order, m.Name)
			} else if m.Digest != h.model.Digest {
				h.differs = true
				continue
			}
			if m.Ready && runnable(w) {
				h.usable = true
			}
		}
	}
	for _, w := range p.workers {
		for i, e := range w.cache.Catalog {
			if h, ok := out[e.Name]; ok && h.catalog == nil {
				h.catalog = &w.cache.Catalog[i]
			}
		}
	}
	return order, out
}

// runnable is a worker a call may go to now. Caller holds p.mu.
func runnable(w *poolWorker) bool {
	return !w.def.Paused && w.def.URL != "" && (w.up || !w.checked)
}

func (p *Pool) heldModel(name string) (heldModel, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, held := p.held()
	h, ok := held[name]
	if !ok {
		return heldModel{}, false
	}
	return *h, true
}

// Catalog is every worker's catalog, by name, the first worker's entry
// for a name the others repeat.
func (p *Pool) Catalog() []worker.CatalogEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []worker.CatalogEntry
	for _, w := range p.workers {
		for _, e := range w.cache.Catalog {
			if !slices.ContainsFunc(out, func(o worker.CatalogEntry) bool { return o.Name == e.Name }) {
				out = append(out, e)
			}
		}
	}
	return out
}

// JobWorker is a worker a job can be given to now: up, not paused, with
// the models it can run.
type JobWorker struct {
	Name, Provider string
	Local          bool
	Slots          int
	Holds          []string
}

func (p *Pool) JobWorkers() []JobWorker {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, held := p.held()
	var out []JobWorker
	for _, w := range p.workers {
		if !runnable(w) || !w.up {
			continue
		}
		jw := JobWorker{Name: w.def.Name, Provider: w.cache.Info.Provider, Local: w.def.Local, Slots: w.slots()}
		for _, m := range w.cache.Models {
			if h := held[m.Name]; h != nil && m.Ready && m.Digest == h.model.Digest {
				jw.Holds = append(jw.Holds, m.Name)
			}
		}
		out = append(out, jw)
	}
	return out
}

// Answers is whether the paired worker named name answers with the token
// monbooru holds, and the address it was called at.
func (p *Pool) Answers(ctx context.Context, name string) (string, bool) {
	p.mu.Lock()
	i := slices.IndexFunc(p.workers, func(w *poolWorker) bool { return w.def.Name == name && !w.def.Local })
	if i < 0 {
		p.mu.Unlock()
		return "", false
	}
	client, at := p.workers[i].client, p.workers[i].def.URL
	p.mu.Unlock()
	_, err := client.Info(ctx)
	return at, err == nil
}

// HasWorker is false while no worker is enabled or paired.
func (p *Pool) HasWorker() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers) > 0
}

var errNoWorker = errors.New("no worker")

// eligible lists the workers a call for models may go to, in order: those
// up and holding each model as the pool's copy. Once a remote worker
// qualifies the local one stands by, unless the job names its workers in
// only. Caller holds p.mu.
func (p *Pool) eligible(models []string, skip map[*poolWorker]bool, only []string) []*poolWorker {
	_, held := p.held()
	var local, remote []*poolWorker
	for _, w := range p.workers {
		if skip[w] || !runnable(w) || only != nil && !slices.Contains(only, w.def.Name) {
			continue
		}
		ok := true
		for _, name := range models {
			h := held[name]
			i := slices.IndexFunc(w.cache.Models, func(m worker.Model) bool { return m.Name == name })
			if h == nil || i < 0 || !w.cache.Models[i].Ready || w.cache.Models[i].Digest != h.model.Digest {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if w.def.Local {
			local = append(local, w)
		} else {
			remote = append(remote, w)
		}
	}
	if only != nil {
		return append(remote, local...)
	}
	if len(remote) > 0 {
		return remote
	}
	return local
}

// Slots is how many calls the workers that may take a job's calls run at
// once, each call carrying one group of models; 0 when none may. A non-nil
// only keeps the workers it names.
func (p *Pool) Slots(groups [][]string, only []string) int {
	n, _ := p.JobSlots(groups, only)
	return n
}

// JobSlots is Slots, and whether a worker other than the local one is
// among those that may take the calls.
func (p *Pool) JobSlots(groups [][]string, only []string) (n int, remote bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[*poolWorker]bool{}
	for _, models := range groups {
		ws := p.eligible(models, nil, only)
		if len(ws) == 0 {
			for _, m := range models {
				ws = append(ws, p.eligible([]string{m}, nil, only)...)
			}
		}
		for _, w := range ws {
			if !seen[w] {
				seen[w] = true
				n += w.slots()
				remote = remote || !w.def.Local
			}
		}
	}
	return n, remote
}

// Tag runs frames through models on a worker that holds them all, waiting
// for a free slot, and names the worker that answered; a non-nil only keeps
// the workers it names. A call that fails in transport goes to another
// worker, three tries at most, and that worker is down until its next
// refresh; a worker's refusal is final.
func (p *Pool) Tag(ctx context.Context, req worker.TagRequest, frames [][]byte, only []string) (worker.TagResponse, string, error) {
	if parts := p.split(req.Models, only); len(parts) > 1 {
		return p.tagParts(ctx, req, frames, only, parts)
	}
	interactive := req.Priority == worker.PriorityInteractive
	skip := map[*poolWorker]bool{}
	var lastErr error
	for range 3 {
		w, err := p.acquire(ctx, req.Models, interactive, skip, only)
		if err != nil {
			if lastErr != nil && errors.Is(err, errNoWorker) {
				return worker.TagResponse{}, "", lastErr
			}
			return worker.TagResponse{}, "", err
		}
		p.mu.Lock()
		client := w.client
		p.mu.Unlock()
		resp, err := client.Tag(ctx, req, frames)
		p.release(w)
		if err == nil {
			return resp, w.def.Name, nil
		}
		if !worker.IsTransport(err) {
			return worker.TagResponse{}, "", fmt.Errorf("worker %s: %w", w.def.Name, err)
		}
		lastErr = fmt.Errorf("worker %s: %w", w.def.Name, err)
		p.markDown(w, err)
		skip[w] = true
	}
	return worker.TagResponse{}, "", lastErr
}

// split cuts models no worker holds together into parts that each one
// does, the worker holding the most of what is left taking its share
// first; nil when one worker holds them all or one holds none.
func (p *Pool) split(models, only []string) [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(models) < 2 || len(p.eligible(models, nil, only)) > 0 {
		return nil
	}
	holders := map[string][]*poolWorker{}
	for _, m := range models {
		if holders[m] = p.eligible([]string{m}, nil, only); len(holders[m]) == 0 {
			return nil
		}
	}
	var parts [][]string
	left := slices.Clone(models)
	for len(left) > 0 {
		var best *poolWorker
		var bestPart []string
		for _, w := range p.workers {
			var part []string
			for _, m := range left {
				if slices.Contains(holders[m], w) {
					part = append(part, m)
				}
			}
			if len(part) > len(bestPart) {
				best, bestPart = w, part
			}
		}
		if best == nil {
			return nil
		}
		parts = append(parts, bestPart)
		left = slices.DeleteFunc(left, func(m string) bool { return slices.Contains(bestPart, m) })
	}
	return parts
}

// tagParts runs each part as its own call and answers in req's model
// order, naming the worker of the first part.
func (p *Pool) tagParts(ctx context.Context, req worker.TagRequest, frames [][]byte, only []string, parts [][]string) (worker.TagResponse, string, error) {
	got := map[string]worker.ModelScores{}
	var by string
	for _, part := range parts {
		sub := req
		sub.Models = part
		resp, name, err := p.Tag(ctx, sub, frames, only)
		if err != nil {
			return worker.TagResponse{}, "", err
		}
		by = cmp.Or(by, name)
		for i, m := range part {
			got[m] = resp.Results[i]
		}
	}
	var out worker.TagResponse
	for _, m := range req.Models {
		out.Results = append(out.Results, got[m])
	}
	return out, by, nil
}

func (p *Pool) markDown(w *poolWorker, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w.up || w.downSince.IsZero() {
		logx.Warnf("tagger: worker %s is down: %v", w.def.Name, err)
		w.downSince = time.Now()
	}
	w.up, w.checked, w.err = false, true, err.Error()
	p.free.Broadcast()
}

func (p *Pool) acquire(ctx context.Context, models []string, interactive bool, skip map[*poolWorker]bool, only []string) (*poolWorker, error) {
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.free.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	if interactive {
		p.interactive++
		defer func() { p.interactive-- }()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ws := p.eligible(models, skip, only)
		if len(ws) == 0 {
			return nil, fmt.Errorf("%w holds %s", errNoWorker, strings.Join(models, ", "))
		}
		if interactive || p.interactive == 0 {
			// The one with the most free slots, so a worker that also
			// takes the job's other calls leaves this one to the rest.
			var best *poolWorker
			for _, w := range ws {
				if free := w.slots() - w.inUse; free > 0 && (best == nil || free > best.slots()-best.inUse) {
					best = w
				}
			}
			if best != nil {
				best.inUse++
				return best, nil
			}
		}
		p.waiting++
		p.free.Wait()
		p.waiting--
	}
}

// Load is where the calls stand now: those ready and waiting for a slot,
// and the slots free on the workers that take calls by themselves, the
// local one only when no paired worker is up.
type Load struct {
	Busy    bool
	Waiting int
	Free    int
}

func (p *Pool) Load() Load {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := Load{Waiting: p.waiting}
	remoteUp := slices.ContainsFunc(p.workers, func(w *poolWorker) bool { return !w.def.Local && runnable(w) && w.up })
	for _, w := range p.workers {
		l.Busy = l.Busy || w.inUse > 0
		if runnable(w) && w.up && (!w.def.Local || !remoteUp) {
			l.Free += max(0, w.slots()-w.inUse)
		}
	}
	l.Busy = l.Busy || l.Waiting > 0
	return l
}

func (p *Pool) release(w *poolWorker) {
	p.mu.Lock()
	w.inUse--
	p.free.Broadcast()
	p.mu.Unlock()
}

// Run refreshes the pool every interval until ctx ends, and again within
// seconds while a worker is still reading a model's weights or went down
// moments ago, as one just started does.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	for {
		p.Refresh(ctx)
		wait := interval
		if p.reading() || p.justDown(30*time.Second) {
			wait = min(interval, 2*time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-p.changed:
		}
	}
}

func (p *Pool) justDown(within time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.ContainsFunc(p.workers, func(w *poolWorker) bool {
		return !w.def.Paused && !w.up && !w.downSince.IsZero() && time.Since(w.downSince) < within
	})
}

func (p *Pool) reading() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workers {
		if w.up && slices.ContainsFunc(w.cache.Models, func(m worker.Model) bool { return m.Reason == reasonReading }) {
			return true
		}
	}
	return false
}

// WorkersDescribed is whether every worker has said once what it holds,
// now or in a run before: until then a tagger it holds reads as missing.
func WorkersDescribed() bool {
	p := currentPool()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.workers) == 0 {
		return false
	}
	for _, w := range p.workers {
		if w.cache.Seen.IsZero() {
			return false
		}
	}
	return true
}
