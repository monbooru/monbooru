// Package counts caches a gallery's whole-library tallies.
package counts

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

type Tallies struct {
	db *db.DB
	// A read stamps what it stores with the generation it read before its
	// query, so a count taken before an invalidation cannot outlive it.
	gen                 atomic.Uint64
	untaggedVisible     atomic.Pointer[stampedCount]
	autoUntaggedVisible atomic.Pointer[stampedCount]
	// Cheap, but the tag-pairs pass divides by it once per image.
	visibleCount     atomic.Pointer[stampedCount]
	countedTags      atomic.Pointer[CountedTags]
	inboxCount       atomic.Pointer[stampedCount]
	tagCount         atomic.Pointer[stampedCount]
	collectionsCount atomic.Pointer[stampedCount]
	phashGen         atomic.Uint64
	phashMissing     atomic.Pointer[stampedCount]
}

type stampedCount struct {
	n   int
	gen uint64
}

func New(database *db.DB) *Tallies { return &Tallies{db: database} }

func (c *Tallies) cachedCount(slot *atomic.Pointer[stampedCount], stamp *atomic.Uint64, sql string) (int, bool) {
	gen := stamp.Load()
	if p := slot.Load(); p != nil && p.gen == gen {
		return p.n, true
	}
	var n int
	if err := c.db.Read.QueryRow(sql).Scan(&n); err != nil {
		return 0, false
	}
	slot.Store(&stampedCount{n: n, gen: gen})
	return n, true
}

// UntaggedVisibleCount must leave out the derived meta rows exactly as
// the tagged: filter does, or the header count disagrees with the grid.
func (c *Tallies) UntaggedVisibleCount() (int, bool) {
	return c.cachedCount(&c.untaggedVisible, &c.gen,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id
		                    AND it.tagger_name IS NOT '`+models.TagSourceMonbooru+`')`)
}

func (c *Tallies) VisibleCount() (int, bool) {
	return c.cachedCount(&c.visibleCount, &c.gen, `SELECT COUNT(*) FROM images WHERE is_missing = 0`)
}

func (c *Tallies) AutoUntaggedVisibleCount() (int, bool) {
	return c.cachedCount(&c.autoUntaggedVisible, &c.gen,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (
		         SELECT 1 FROM image_tags it
		         WHERE it.image_id = i.id AND it.is_auto = 1
		       )`)
}

func (c *Tallies) InboxCount() (int, bool) {
	return c.cachedCount(&c.inboxCount, &c.gen, `SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = 1`)
}

func (c *Tallies) TagCount() (int, bool) {
	return c.cachedCount(&c.tagCount, &c.gen, `SELECT COUNT(*) FROM tags WHERE is_alias = 0 AND rejected = 0`)
}

func (c *Tallies) CollectionsCount() (int, bool) {
	return c.cachedCount(&c.collectionsCount, &c.gen, `SELECT COUNT(*) FROM collection_counts WHERE visible_count > 0`)
}

func (c *Tallies) PhashMissing() (int, bool) {
	return c.cachedCount(&c.phashMissing, &c.phashGen, `SELECT COUNT(*) FROM images WHERE phash IS NULL AND is_missing = 0`)
}

// InvalidatePhashMissing is for phash writes that leave image membership
// alone; anything else calls Invalidate.
func (c *Tallies) InvalidatePhashMissing() {
	c.phashGen.Add(1)
	c.phashMissing.Store(nil)
}

// CountedTags keeps parallel id-sorted slices rather than a map: a million
// images cost 12 MB here against about four times that in map buckets.
type CountedTags struct {
	gen      uint64
	maxUsage int64
	ids      []int64
	totals   []int32
	used     atomic.Int64
}

func (c *CountedTags) Total(id int64) int32 {
	if i, ok := slices.BinarySearch(c.ids, id); ok {
		return c.totals[i]
	}
	return 0
}

func (c *Tallies) CountedTagTotals(ctx context.Context, maxUsage int64) (*CountedTags, error) {
	gen := c.gen.Load()
	if t := c.countedTags.Load(); t != nil && t.gen == gen && t.maxUsage == maxUsage {
		t.used.Store(time.Now().UnixNano())
		return t, nil
	}
	rows, err := c.db.Read.QueryContext(ctx,
		`SELECT it.image_id, count(*)
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE tc.name != 'meta' AND t.usage_count <= ?
		  GROUP BY it.image_id`, maxUsage)
	if err != nil {
		return nil, fmt.Errorf("counted tag totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := &CountedTags{gen: gen, maxUsage: maxUsage}
	for rows.Next() {
		var id int64
		var total int32
		if err := rows.Scan(&id, &total); err != nil {
			return nil, err
		}
		totals.ids = append(totals.ids, id)
		totals.totals = append(totals.totals, total)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	totals.used.Store(time.Now().UnixNano())
	c.countedTags.Store(totals)
	return totals, nil
}

func (c *Tallies) ReleaseIdleCountedTags(after time.Duration) bool {
	t := c.countedTags.Load()
	if t == nil || time.Since(time.Unix(0, t.used.Load())) < after {
		return false
	}
	return c.countedTags.CompareAndSwap(t, nil)
}

// Invalidate must follow every write that changes image_tags membership.
func (c *Tallies) Invalidate() {
	c.gen.Add(1)
	c.phashGen.Add(1)
	c.countedTags.Store(nil)
}
