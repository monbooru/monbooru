package search

import (
	"slices"
	"testing"
	"time"
)

func TestAdjacencyCache_GetMissEmptyKey(t *testing.T) {
	c := NewAdjacencyCache()
	if _, ok := c.get(""); ok {
		t.Errorf("empty key should miss")
	}
}

func TestAdjacencyCache_SetGetRoundTrip(t *testing.T) {
	c := NewAdjacencyCache()
	key := "g\x00q\x00sort"
	want := []int64{10, 20, 30}
	c.set(key, want, c.gen())
	got, ok := c.get(key)
	if !ok {
		t.Fatalf("set+get: miss")
	}
	if !slices.Equal(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}
}

func TestAdjacencyCache_SetIsolatesCallerSlice(t *testing.T) {
	c := NewAdjacencyCache()
	key := "snapshot"
	src := []int64{1, 2, 3}
	c.set(key, src, c.gen())
	src[0] = 99
	got, _ := c.get(key)
	if got[0] != 1 {
		t.Errorf("cache aliased caller's slice: got[0] = %d, want 1", got[0])
	}
}

func TestAdjacencyCache_SetSkipsOversized(t *testing.T) {
	c := NewAdjacencyCache()
	key := "oversize"
	huge := make([]int64, adjacencyCacheMaxIDs+1)
	c.set(key, huge, c.gen())
	if _, ok := c.get(key); ok {
		t.Errorf("oversized list should not have been cached")
	}
}

func TestAdjacencyCache_LRUEviction(t *testing.T) {
	c := NewAdjacencyCache()
	for i := 0; i < adjacencyCacheMaxEntries; i++ {
		c.set(keyN(i), []int64{int64(i)}, c.gen())
	}
	c.set(keyN(adjacencyCacheMaxEntries), []int64{42}, c.gen())
	if _, ok := c.get(keyN(0)); ok {
		t.Errorf("oldest entry should have been evicted")
	}
	if _, ok := c.get(keyN(adjacencyCacheMaxEntries)); !ok {
		t.Errorf("newest entry should be present")
	}
}

func TestAdjacencyCache_ResetMovesToMostRecentSlot(t *testing.T) {
	c := NewAdjacencyCache()
	target := "refreshed"
	c.set(target, []int64{1}, c.gen())
	for i := 0; i < adjacencyCacheMaxEntries-1; i++ {
		c.set(keyN(i), []int64{int64(i + 100)}, c.gen())
	}

	backdate(c, target)
	c.set(target, []int64{2}, c.gen())

	c.set("trigger-eviction", []int64{999}, c.gen())
	if _, ok := c.get(target); !ok {
		t.Errorf("re-Set entry should have moved to the most-recent slot and survived the eviction")
	}
}

func TestAdjacencyCache_SetAfterDropIsRefused(t *testing.T) {
	c := NewAdjacencyCache()
	gen := c.gen()
	c.Drop()
	c.set("k", []int64{1, 2}, gen)
	if ids, ok := c.get("k"); ok {
		t.Errorf("a list read before the Drop was stored: %v", ids)
	}
	c.set("k", []int64{3}, c.gen())
	if _, ok := c.get("k"); !ok {
		t.Error("a list read after the Drop was refused")
	}
}

func TestAdjacencyCache_Expiry(t *testing.T) {
	c := NewAdjacencyCache()
	key := "expire"
	c.set(key, []int64{1, 2}, c.gen())
	backdate(c, key)
	if _, ok := c.get(key); ok {
		t.Errorf("expired entry should have missed")
	}
	c.mu.Lock()
	for _, k := range c.order {
		if k == key {
			t.Errorf("expired key still in order list after miss")
		}
	}
	c.mu.Unlock()
}

func TestAdjacencyCache_SweepDropsExpiredKeepsLive(t *testing.T) {
	c := NewAdjacencyCache()
	c.set("stale", []int64{1, 2}, c.gen())
	c.set("live", []int64{3}, c.gen())
	backdate(c, "stale")

	c.Sweep()

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries["stale"]; ok {
		t.Error("expired entry survived the sweep")
	}
	if _, ok := c.entries["live"]; !ok {
		t.Error("sweep dropped a live entry")
	}
	if len(c.order) != 1 || c.order[0] != "live" {
		t.Errorf("order list = %v, want [live]", c.order)
	}
}

func TestAdjacencyKey_DeterministicAndDistinct(t *testing.T) {
	a := adjacencyKey("q", "newest", "desc", 0, "", nil)
	b := adjacencyKey("q", "newest", "desc", 0, "", nil)
	if a != b {
		t.Errorf("same inputs produced different keys")
	}
	c := adjacencyKey("q2", "newest", "desc", 0, "", nil)
	if a == c {
		t.Errorf("different query should produce different key")
	}
	d := adjacencyKey("q", "random", "desc", 1234, "", nil)
	e := adjacencyKey("q", "random", "desc", 5678, "", nil)
	if d == e {
		t.Errorf("different random seeds should produce different keys")
	}
}

func TestAdjacencyKey_NormalisesNonRandomSeed(t *testing.T) {
	a := adjacencyKey("q", "newest", "desc", 0, "", nil)
	b := adjacencyKey("q", "newest", "desc", 1234, "", nil)
	if a != b {
		t.Errorf("seed should be ignored for non-random sort")
	}
}

func TestPageOf_ReadsTheCachedList(t *testing.T) {
	src := Source{Cache: NewAdjacencyCache()}
	q := Query{Cached: true, Text: "q", Sort: "newest", Order: "desc"}
	if _, listed := PageOf(src, q, 1, 2); listed {
		t.Error("an empty cache reported a list")
	}
	src.Cache.Seed(q, []int64{10, 20, 30})
	for _, tc := range []struct {
		id   int64
		page int
	}{{10, 1}, {20, 1}, {30, 2}, {99, 0}} {
		if page, listed := PageOf(src, q, tc.id, 2); !listed || page != tc.page {
			t.Errorf("PageOf(%d) = %d (listed %t), want %d", tc.id, page, listed, tc.page)
		}
	}
	q.Cached = false
	if _, listed := PageOf(src, q, 10, 2); listed {
		t.Error("an uncached query read the list")
	}
}

func TestFindInAdjacencyList(t *testing.T) {
	ids := []int64{10, 20, 30, 40, 50}
	ptr := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	for _, tc := range []struct{ id, prev, next int64 }{
		{30, 20, 40},
		{10, 0, 20},
		{50, 40, 0},
		{99, 0, 0},
	} {
		prev, next := findInAdjacencyList(ids, tc.id)
		if ptr(prev) != tc.prev || ptr(next) != tc.next {
			t.Errorf("around %d: prev, next = %d, %d, want %d, %d (0 is none)", tc.id, ptr(prev), ptr(next), tc.prev, tc.next)
		}
	}
}

func TestAdjacencyCache_EmptyFanHoldsOffTheNextAttempt(t *testing.T) {
	c := NewAdjacencyCache()
	key := "g\x00*blue*"
	if !c.tryAcquireFan(key) {
		t.Fatal("first attempt should win the gate")
	}
	c.releaseFan(key)
	c.markFanOverBudget(key)

	if c.tryAcquireFan(key) {
		t.Error("a fan that came back empty should hold the next attempt off")
	}
	if !c.tryAcquireFan("g\x001girl") {
		t.Error("the hold-off is per key; another query must still fan")
	}
}

func TestAdjacencyCache_DropLiftsTheFanHoldOff(t *testing.T) {
	c := NewAdjacencyCache()
	key := "g\x00*blue*"
	c.markFanOverBudget(key)
	c.Drop()
	if !c.tryAcquireFan(key) {
		t.Error("a write that changed membership should let the fan be tried again")
	}
}

func TestAdjacencyCache_BackgroundFansTakeTurns(t *testing.T) {
	c := NewAdjacencyCache()
	if !c.tryAcquireBackgroundFan("a") {
		t.Fatal("the first background fan should run")
	}
	if c.tryAcquireBackgroundFan("b") {
		t.Error("a second background fan ran beside the first")
	}
	if !c.tryAcquireFan("b") {
		t.Error("a fan in the request does not wait for the background slot")
	}
	c.releaseFan("b")
	c.releaseBackgroundFan("a")
	if !c.tryAcquireBackgroundFan("b") {
		t.Error("a key turned away by a busy slot should not be held off")
	}
}

func keyN(n int) string {
	return "k" + string(rune('0'+n%10)) + string(rune('a'+n/10))
}

func TestFanBudget_ScalesWithTheKnownTotal(t *testing.T) {
	for _, tc := range []struct {
		spent time.Duration
		total int
		want  time.Duration
	}{
		{0, 0, 750 * time.Millisecond},
		{100 * time.Millisecond, 1000, 750 * time.Millisecond},
		{100 * time.Millisecond, 434616, 1303848 * time.Microsecond},
		{100 * time.Millisecond, 1000000, 2 * time.Second},
		{time.Second, 0, 2 * time.Second},
		{1500 * time.Millisecond, 1000000, 3 * time.Second},
	} {
		if got := fanBudget(tc.spent, tc.total); got != tc.want {
			t.Errorf("fanBudget(%v, %d) = %v, want %v", tc.spent, tc.total, got, tc.want)
		}
	}
}

func backdate(c *AdjacencyCache, key string) {
	c.mu.Lock()
	entry := c.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	c.entries[key] = entry
	c.mu.Unlock()
}
