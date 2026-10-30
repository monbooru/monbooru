package lookup

import (
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// FlagColumn is per backend: the PTR is free and the boorus spend a quota,
// so an image can be worth one and not the other.
func FlagColumn(backend string) string {
	if backend == contract.LookupPTR {
		return "scheduled_lookup_ptr"
	}
	return "scheduled_lookup"
}

// CandidateClause expects images aliased as i. Only a source with a URL
// disqualifies: a PTR hit writes a url-less one that must not end the
// booru search. No index knows an archive's own hash.
func CandidateClause(backend string) string {
	return `i.is_missing = 0
	  AND i.` + FlagColumn(backend) + ` = 1
	  AND i.file_type <> 'cbz'
	  AND NOT EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')`
}

// DueClause expects images aliased as i. A PTR retry needs both its delay
// and an index move: the cursor alone re-pulls every miss most days, the
// delay alone re-asks an unchanged index.
func DueClause(backend string, now time.Time) (string, []any) {
	blocked := `l.queued_at IS NOT NULL OR l.next_due_at IS NULL OR l.next_due_at > ?`
	args := []any{backend, stamp(now)}
	if backend == contract.LookupPTR {
		// A cold cursor drops the index gate rather than block every row:
		// over-counting is recoverable, never looking again is not.
		if cursor := PTRCursor.Load(); cursor > 0 {
			blocked += ` OR l.ptr_cursor >= ?`
			args = append(args, int64(cursor))
		}
	}
	return `NOT EXISTS (SELECT 1 FROM image_lookups l
	          WHERE l.image_id = i.id AND l.backend = ? AND (` + blocked + `))`, args
}

type Candidate struct {
	ImageID int64
	SHA256  string
	MD5     string
}

// DueIDs snapshots the due set in the order a phase works it: never-tried
// rows first, then the oldest due. An inconclusive result is re-dated to
// now, so it sorts behind every older retry.
func DueIDs(database *db.DB, backend string, now time.Time) ([]int64, error) {
	query, args := dueSelect("i.id", backend, now)
	return db.QueryIDs(database.Read, query+dueOrder, args...)
}

// Due re-checks a slice of a DueIDs snapshot, so a row recorded, sourced
// or opted out since the snapshot drops out.
func Due(database *db.DB, backend string, now time.Time, ids []int64) ([]Candidate, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query, args := dueSelect("i.id, i.sha256, i.md5", backend, now)
	ph, idArgs := db.InPlaceholders(ids)
	return db.QueryAll(database.Read, db.ScanRow(func(c *Candidate) []any {
		return []any{&c.ImageID, &c.SHA256, &c.MD5}
	}), query+` AND i.id IN (`+ph+`)`+dueOrder, append(args, idArgs...)...)
}

const dueOrder = ` ORDER BY l.next_due_at IS NOT NULL, l.next_due_at, i.id`

func dueSelect(columns, backend string, now time.Time) (string, []any) {
	due, args := DueClause(backend, now)
	return `SELECT ` + columns + `
		 FROM images i
		 LEFT JOIN image_lookups l ON l.image_id = i.id AND l.backend = ?
		 WHERE ` + CandidateClause(backend) + ` AND ` + due, append([]any{backend}, args...)
}

func DueCount(database *db.DB, backend string, now time.Time) (int, error) {
	due, args := DueClause(backend, now)
	var n int
	err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+CandidateClause(backend)+` AND `+due, args...,
	).Scan(&n)
	return n, err
}

// Standing is an image's side of the lookup panel: its two opt-ins, and
// whether CandidateClause's file rules leave it anything to find.
type Standing struct {
	PTR, Booru, Candidate bool
}

func StandingOf(database *db.DB, imageID int64) (Standing, error) {
	var st Standing
	var sourced int
	var fileType string
	err := database.Read.QueryRow(
		`SELECT i.scheduled_lookup_ptr, i.scheduled_lookup, i.file_type,
		        EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')
		 FROM images i WHERE i.id = ?`, imageID,
	).Scan(&st.PTR, &st.Booru, &fileType, &sourced)
	st.Candidate = sourced == 0 && fileType != "cbz"
	return st, err
}
