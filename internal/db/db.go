// Package db opens a gallery's SQLite pools and owns its schema.
package db

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)

// Both separators: a Windows library's canonical_path has backslashes.
// Indexes store the result, so changing it needs a REINDEX migration.
func basenameSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	s, ok := args[0].(string)
	if !ok {
		return nil, nil
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		return s[i+1:], nil
	}
	return s, nil
}

func hammingDistSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	a, aOk := args[0].(int64)
	b, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(bits.OnesCount64(uint64(a) ^ uint64(b))), nil
}

func randomKeySQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	id, aOk := args[0].(int64)
	seed, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(RandomSortKey(id, seed)), nil
}

// RandomSortKey is random_key() in Go: a SplitMix64 mix of (id, seed).
func RandomSortKey(id, seed int64) uint64 {
	x := uint64(id)*0x9E3779B97F4A7C15 ^ uint64(seed)*0xBF58476D1CE4E5B9
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x & 0x7FFFFFFFFFFFFFFF
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("basename", 1, basenameSQL)
	sqlite.MustRegisterDeterministicScalarFunction("hammingdist", 2, hammingDistSQL)
	sqlite.MustRegisterDeterministicScalarFunction("random_key", 2, randomKeySQL)
}

//go:embed schema.sql
var schemaSQL string

// NormalizeWindowsFolderPathSQL skips any row whose canonical_path has a
// "/": on POSIX a backslash in a name is literal.
const NormalizeWindowsFolderPathSQL = `UPDATE images SET folder_path = ltrim(replace(folder_path, '\', '/'), '/')
	WHERE instr(folder_path, '\') > 0
	  AND instr(canonical_path, '\') > 0
	  AND instr(canonical_path, '/') = 0`

// Bump bootstrapSchemaVersion for a new one-shot or an index the planner
// needs stats for: ANALYZE runs once per bump.
const bootstrapSchemaVersion = 16

// A library below MinUpgradeUserVersion was last opened outside the
// two-minor upgrade window; UpgradeVia is a release that still upgrades
// it. Raising the floor is what lets a one-shot under it be deleted.
const (
	MinUpgradeUserVersion = 13
	UpgradeVia            = "v1.22.1"
)

var ErrLibraryTooOld = errors.New("library too old to upgrade directly")

// WAL serialises writers, so Write holds one connection and Read a pool.
type DB struct {
	Read  *sql.DB
	Write *sql.DB
	// Two passes each holding part of one pool would wait on each other
	// forever.
	eachConn sync.Mutex
}

// Only these are URI syntax in a path; a URL encoding breaks relative and drive-letter paths.
var uriPathEscaper = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

func Open(path string) (*DB, error) {
	dsn := "file:" + uriPathEscaper.Replace(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-1024)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=mmap_size(67108864)"

	rd, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening read pool: %w", err)
	}
	rd.SetMaxOpenConns(8)
	rd.SetMaxIdleConns(8)
	rd.SetConnMaxIdleTime(5 * time.Minute)

	// Immediate: a transaction that reads before it writes gets SQLITE_BUSY at
	// once, busy_timeout or not, when another connection holds the write
	// lock, and the readers' ANALYZE takes it.
	wr, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		_ = rd.Close()
		return nil, fmt.Errorf("opening write pool: %w", err)
	}
	wr.SetMaxOpenConns(1)
	wr.SetMaxIdleConns(1)
	wr.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{Read: rd, Write: wr}

	if err := rd.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging read pool: %w", err)
	}
	if err := wr.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging write pool: %w", err)
	}

	return db, nil
}

// Each connection has its own page cache, so ShrinkMemory holds them all
// at once to reach every one.
func (db *DB) ShrinkMemory(ctx context.Context) error {
	if err := db.execEachConn(ctx, db.Read, `PRAGMA shrink_memory`); err != nil {
		return err
	}
	return db.execEachConn(ctx, db.Write, `PRAGMA shrink_memory`)
}

// Optimize refreshes the planner statistics a library outgrows. A
// connection reads them only when it opens, so every pooled reader is
// made to reload them; 0x10002 also covers tables the writer never read.
func (db *DB) Optimize(ctx context.Context) error {
	if _, err := db.Write.ExecContext(ctx, `PRAGMA optimize=0x10002`); err != nil {
		return err
	}
	return db.execEachConn(ctx, db.Read, `ANALYZE sqlite_schema`)
}

// ErrPoolBusy is a pass skipped because a long read kept a connection past
// eachConnWait: holding the rest meanwhile would stall every other read.
var ErrPoolBusy = errors.New("database busy; pass skipped")

const eachConnWait = 2 * time.Second

func (db *DB) execEachConn(ctx context.Context, pool *sql.DB, stmt string) error {
	db.eachConn.Lock()
	defer db.eachConn.Unlock()
	n := pool.Stats().MaxOpenConnections
	if n <= 0 {
		n = 1
	}
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	acquire, cancel := context.WithTimeout(ctx, eachConnWait)
	defer cancel()
	for i := 0; i < n; i++ {
		c, err := pool.Conn(acquire)
		if err != nil {
			if ctx.Err() == nil && acquire.Err() != nil {
				return ErrPoolBusy
			}
			return err
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if _, err := c.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

var ErrCheckpoint = errors.New("wal checkpoint")

// Vacuum's failed checkpoint wraps ErrCheckpoint: the vacuum itself is
// done. In WAL mode VACUUM writes into the -wal file; only a truncating
// checkpoint gives the space back.
func (db *DB) Vacuum(ctx context.Context) error {
	if _, err := db.Write.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if _, err := db.Write.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("%w: %v", ErrCheckpoint, err)
	}
	return nil
}

func (db *DB) Close() error {
	return cmp.Or(db.Read.Close(), db.Write.Close())
}

const DefaultCategoryOrder = `CASE name
	WHEN 'rating' THEN 0 WHEN 'year' THEN 1 WHEN 'meta' THEN 2 WHEN 'medium' THEN 3
	WHEN 'person' THEN 4 WHEN 'artist' THEN 5 WHEN 'copyright' THEN 6 WHEN 'character' THEN 7
	WHEN 'species' THEN 8 WHEN 'general' THEN 10 ELSE 9 END, name`

const ResetCategoryPositions = `UPDATE tag_categories SET position = r.pos
	FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY ` + DefaultCategoryOrder + `) - 1 AS pos FROM tag_categories) r
	WHERE r.id = tag_categories.id`
