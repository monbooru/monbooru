package search

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Worst-case memory per gallery: 4 entries x 1M ids x 8 bytes = 32 MB.
const (
	adjacencyCacheTTL        = 5 * time.Minute
	adjacencyCacheMaxEntries = 4
	adjacencyCacheMaxIDs     = 1000000
	// The fan has no LIMIT to stop at. It gets twice what the request has
	// spent, and at least the floor, raised with an exact total up to a
	// cap: a query slow once repays its fan, a large match set gets the
	// time its walk needs, and an overrun only leaves the entry unset.
	adjacencyFanBudget            = 750 * time.Millisecond
	adjacencyFanCostRatio         = 2
	adjacencyFanPerID             = 3 * time.Microsecond
	adjacencyFanMaxFloor          = 2 * time.Second
	similarityFanBackgroundBudget = 10 * time.Second
)

var fanBudget = func(spent time.Duration, total int) time.Duration {
	return max(adjacencyFanBudget, spent*adjacencyFanCostRatio, min(time.Duration(total)*adjacencyFanPerID, adjacencyFanMaxFloor))
}

type adjacencyCacheEntry struct {
	ids       []int64
	expiresAt time.Time
}

type hiddenCountEntry struct {
	n         int
	overran   bool
	expiresAt time.Time
}

// AdjacencyCache holds one gallery's recent match lists, so a detail page
// finds its neighbours without re-running the search. A nil cache holds
// nothing.
type AdjacencyCache struct {
	mu         sync.Mutex
	entries    map[string]adjacencyCacheEntry
	order      []string
	generation uint64
	hidden     map[string]hiddenCountEntry

	// One fan per key at a time: concurrent misses would each run the
	// same full SELECT.
	fanMu       sync.Mutex
	fanInFlight map[string]bool
	// Keys whose last fan came back empty, until they may try again:
	// without the hold-off such a query pays the whole budget on every
	// page-1 hit.
	fanOverBudget map[string]time.Time
}

func NewAdjacencyCache() *AdjacencyCache {
	return &AdjacencyCache{
		entries:       map[string]adjacencyCacheEntry{},
		hidden:        map[string]hiddenCountEntry{},
		fanInFlight:   map[string]bool{},
		fanOverBudget: map[string]time.Time{},
	}
}

// tryAcquireFan's winner must call releaseFan, whatever the outcome.
func (c *AdjacencyCache) tryAcquireFan(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.fanMu.Lock()
	defer c.fanMu.Unlock()
	if c.fanInFlight[key] {
		return false
	}
	if retryAt, held := c.fanOverBudget[key]; held {
		if time.Now().Before(retryAt) {
			return false
		}
		delete(c.fanOverBudget, key)
	}
	c.fanInFlight[key] = true
	return true
}

func (c *AdjacencyCache) markFanOverBudget(key string) {
	if c == nil || key == "" {
		return
	}
	c.fanMu.Lock()
	c.fanOverBudget[key] = time.Now().Add(adjacencyCacheTTL)
	c.fanMu.Unlock()
}

func (c *AdjacencyCache) releaseFan(key string) {
	if c == nil {
		return
	}
	c.fanMu.Lock()
	delete(c.fanInFlight, key)
	c.fanMu.Unlock()
}

// get returns the cached array itself: callers must not modify it.
func (c *AdjacencyCache) get(key string) ([]int64, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		c.removeFromOrder(key)
		return nil, false
	}
	return entry.ids, true
}

// Seed stores ids as q's list; it exists for other packages' tests.
func (c *AdjacencyCache) Seed(q Query, ids []int64) {
	c.set(q.listKey(Source{}), ids, c.gen())
}

// gen must be read before the query whose ids go to set.
func (c *AdjacencyCache) gen() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// set takes a re-set key out of its old LRU slot, whose eviction would
// otherwise delete the fresh entry.
func (c *AdjacencyCache) set(key string, ids []int64, gen uint64) {
	if c == nil || key == "" || len(ids) == 0 || len(ids) > adjacencyCacheMaxIDs {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.generation {
		return
	}
	if _, exists := c.entries[key]; exists {
		c.removeFromOrder(key)
	}
	c.order = append(c.order, key)
	snapshot := make([]int64, len(ids))
	copy(snapshot, ids)
	c.entries[key] = adjacencyCacheEntry{
		ids:       snapshot,
		expiresAt: time.Now().Add(adjacencyCacheTTL),
	}
	for len(c.order) > adjacencyCacheMaxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

func (c *AdjacencyCache) hiddenCount(key string) (hiddenCountEntry, bool) {
	if c == nil {
		return hiddenCountEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.hidden[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return hiddenCountEntry{}, false
	}
	return entry, true
}

func (c *AdjacencyCache) setHiddenCount(key string, entry hiddenCountEntry, gen uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen == c.generation {
		entry.expiresAt = time.Now().Add(adjacencyCacheTTL)
		c.hidden[key] = entry
	}
}

func (c *AdjacencyCache) Drop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.generation++
	clear(c.entries)
	clear(c.hidden)
	c.order = nil
	c.mu.Unlock()
	c.dropFanHoldOffs(func(string, time.Time) bool { return true })
}

// Sweep exists because get and set evict only what they touch: an idle
// gallery would keep expired lists, and the hold-offs of keys nobody asks
// for again would pile up.
func (c *AdjacencyCache) Sweep() {
	if c == nil {
		return
	}
	now := time.Now()
	c.drop(func(_ string, entry adjacencyCacheEntry) bool {
		return now.After(entry.expiresAt)
	})
	c.mu.Lock()
	maps.DeleteFunc(c.hidden, func(_ string, entry hiddenCountEntry) bool {
		return now.After(entry.expiresAt)
	})
	c.mu.Unlock()
	c.dropFanHoldOffs(func(_ string, retryAt time.Time) bool {
		return now.After(retryAt)
	})
}

func (c *AdjacencyCache) dropFanHoldOffs(drop func(string, time.Time) bool) {
	c.fanMu.Lock()
	maps.DeleteFunc(c.fanOverBudget, drop)
	c.fanMu.Unlock()
}

func (c *AdjacencyCache) drop(drop func(string, adjacencyCacheEntry) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.entries, drop)
	c.order = slices.DeleteFunc(c.order, func(k string) bool {
		_, kept := c.entries[k]
		return !kept
	})
}

func (c *AdjacencyCache) removeFromOrder(key string) {
	if i := slices.Index(c.order, key); i >= 0 {
		c.order = slices.Delete(c.order, i, i+1)
	}
}

// adjacencyKey joins on NUL so no component can run into the next. The
// seed counts only under random sort, so a leftover seed param can't split
// a newest or filesize entry.
func adjacencyKey(query, sort, order string, seed int64, ceiling string, lookups []string) string {
	seedStr := ""
	if sort == "random" && seed != 0 {
		seedStr = strconv.FormatInt(seed, 10)
	}
	return strings.Join([]string{query, sort, order, seedStr, ceiling, strings.Join(lookups, ",")}, "\x00")
}

// PageOf reads the page holding id off q's cached list: listed is false
// when none is cached, and page is 0 when the list does not hold id.
func PageOf(src Source, q Query, id int64, pageSize int) (page int, listed bool) {
	ids, ok := src.Cache.get(q.listKey(src))
	if !ok {
		return 0, false
	}
	i := slices.Index(ids, id)
	if i < 0 {
		return 0, true
	}
	return i/pageSize + 1, true
}

func findInAdjacencyList(ids []int64, currentID int64) (*int64, *int64) {
	i := slices.Index(ids, currentID)
	if i < 0 {
		return nil, nil
	}
	var prev, next *int64
	if i > 0 {
		p := ids[i-1]
		prev = &p
	}
	if i < len(ids)-1 {
		n := ids[i+1]
		next = &n
	}
	return prev, next
}
