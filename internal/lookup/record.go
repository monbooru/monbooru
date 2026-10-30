package lookup

import (
	"database/sql"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

type Row struct {
	Backend    string
	Attempts   int
	QueuedAt   time.Time
	LastAt     time.Time
	LastResult string
	NextDueAt  time.Time
}

type InFlight struct {
	ImageID int64
	Backend string
	JobID   int64
}

// A zero jobID still records the attempt, but only the grace sweep can
// resolve it.
func Enqueued(database *db.DB, imageID int64, backends []string, jobID int64, now time.Time) error {
	for _, backend := range backends {
		if _, err := database.Write.Exec(
			`INSERT INTO image_lookups (image_id, backend, queued_at, job_id)
			 VALUES (?, ?, ?, NULLIF(?, 0))
			 ON CONFLICT(image_id, backend) DO UPDATE SET queued_at = excluded.queued_at, job_id = excluded.job_id`,
			imageID, backend, stamp(now), jobID,
		); err != nil {
			return err
		}
	}
	return nil
}

// Record advances the ladder only on a hit or a miss. Any other result clears
// the in-flight state and leaves the image due now, so an outage cannot walk
// it to "nothing found". cursor is for a PTR result; pass 0 elsewhere.
func Record(database *db.DB, imageID int64, backend, result string, cursor uint64, now time.Time) error {
	if result != ResultHit && result != ResultMiss {
		_, err := database.Write.Exec(
			`INSERT INTO image_lookups (image_id, backend, next_due_at) VALUES (?, ?, ?)
			 ON CONFLICT(image_id, backend) DO UPDATE SET
			   queued_at = NULL, job_id = NULL, next_due_at = excluded.next_due_at`,
			imageID, backend, stamp(now))
		return err
	}
	attempts, err := nextAttempts(database, imageID, backend, result)
	if err != nil {
		return err
	}
	var due any
	if result == ResultMiss {
		if t, ok := NextDue(now, backend, attempts); ok {
			due = stamp(t)
		}
	}
	var ptrCursor any
	if backend == contract.LookupPTR && cursor > 0 {
		ptrCursor = int64(cursor)
	}
	_, err = database.Write.Exec(
		`INSERT INTO image_lookups (image_id, backend, attempts, last_at, last_result, next_due_at, ptr_cursor)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(image_id, backend) DO UPDATE SET
		   queued_at = NULL, job_id = NULL, attempts = excluded.attempts,
		   last_at = excluded.last_at, last_result = excluded.last_result,
		   next_due_at = excluded.next_due_at, ptr_cursor = excluded.ptr_cursor`,
		imageID, backend, attempts, stamp(now), result, due, ptrCursor)
	return err
}

func nextAttempts(database *db.DB, imageID int64, backend, result string) (int, error) {
	if result == ResultHit {
		return 0, nil
	}
	var attempts int
	err := database.Read.QueryRow(
		`SELECT attempts FROM image_lookups WHERE image_id = ? AND backend = ?`, imageID, backend).Scan(&attempts)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return attempts + 1, nil
}

// RecordInFlight concludes only attempts in flight, so a plain source refetch
// cannot conclude a lookup that never ran. An empty backend matches any.
func RecordInFlight(database *db.DB, imageID int64, backend, result string, now time.Time) error {
	backends, err := db.QueryStrings(database.Read,
		`SELECT backend FROM image_lookups
		 WHERE image_id = ? AND queued_at IS NOT NULL AND (? = '' OR backend = ?)`,
		imageID, backend, backend)
	if err != nil {
		return err
	}
	for _, b := range backends {
		if err := Record(database, imageID, b, result, 0, now); err != nil {
			return err
		}
	}
	return nil
}

func Waiting(database *db.DB, cutoff time.Time) ([]InFlight, error) {
	return db.QueryAll(database.Read, db.ScanRow(func(f *InFlight) []any {
		return []any{&f.ImageID, &f.Backend, &f.JobID}
	}), `SELECT image_id, backend, COALESCE(job_id, 0) FROM image_lookups
		 WHERE queued_at IS NOT NULL AND queued_at <= ?`, stamp(cutoff))
}

// Reset keeps last_at and last_result so the detail page still says when
// the image was last looked up.
func Reset(database *db.DB, imageID int64, backend string, now time.Time) error {
	return ResetFor(database.Write, "?", []any{imageID}, []string{backend}, now)
}

// ResetFor is Reset for a set of images and backends; whereIDs is the
// placeholder list for image_id IN (...) and args its binds.
func ResetFor(e db.Execer, whereIDs string, args []any, backends []string, now time.Time) error {
	in, backendArgs := db.InPlaceholders(backends)
	_, err := e.Exec(
		`UPDATE image_lookups SET attempts = 0, next_due_at = ?, ptr_cursor = NULL
		 WHERE image_id IN (`+whereIDs+`) AND backend IN (`+in+`)`,
		append(append([]any{stamp(now)}, args...), backendArgs...)...)
	return err
}

// DeleteForImage must run whenever an image's bytes change: its misses
// were about the old bytes.
func DeleteForImage(e db.Execer, imageID int64) error {
	_, err := e.Exec(`DELETE FROM image_lookups WHERE image_id = ?`, imageID)
	return err
}

func ForImage(database *db.DB, imageID int64) (map[string]Row, error) {
	rows, err := database.Read.Query(
		`SELECT backend, attempts, queued_at, last_at, last_result, next_due_at
		 FROM image_lookups WHERE image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Row{}
	for rows.Next() {
		var r Row
		var queued, last, due sql.NullString
		if err := rows.Scan(&r.Backend, &r.Attempts, &queued, &last, &r.LastResult, &due); err != nil {
			return nil, err
		}
		r.QueuedAt, r.LastAt, r.NextDueAt = parseStamp(queued), parseStamp(last), parseStamp(due)
		out[r.Backend] = r
	}
	return out, rows.Err()
}

// Exhausted is derived rather than stored, so a schedule that never ran
// cannot show "nothing found".
func (r Row) Exhausted() bool {
	return r.Backend == contract.LookupBooru && r.LastResult == ResultMiss && r.NextDueAt.IsZero()
}

func parseStamp(v sql.NullString) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05Z", v.String)
	return t
}
