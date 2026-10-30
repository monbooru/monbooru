package db

import (
	"context"
	"database/sql"
	"strings"
)

// fn gets a view of xs's backing array; copy anything it keeps.
func Chunked[T any](xs []T, chunkSize int, fn func(chunk []T) error) error {
	for start := 0; start < len(xs); start += chunkSize {
		if err := fn(xs[start:min(start+chunkSize, len(xs))]); err != nil {
			return err
		}
	}
	return nil
}

// ScanAll leaves closing rows to the caller, and on a scan error returns
// nothing rather than a partial set.
func ScanAll[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) ([]T, error) {
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ScanRow's fields returns pointers into its argument in column order.
func ScanRow[T any](fields func(*T) []any) func(*sql.Rows) (T, error) {
	return func(rows *sql.Rows) (T, error) {
		var v T
		err := rows.Scan(fields(&v)...)
		return v, err
	}
}

func scanOne[T any](rows *sql.Rows) (T, error) {
	var v T
	err := rows.Scan(&v)
	return v, err
}

func QueryAll[T any](q Querier, scan func(*sql.Rows) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return ScanAll(rows, scan)
}

func QueryAllContext[T any](ctx context.Context, q CtxQuerier, scan func(*sql.Rows) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return ScanAll(rows, scan)
}

func QueryStrings(q Querier, query string, args ...any) ([]string, error) {
	return QueryAll(q, scanOne[string], query, args...)
}

func InWriteTx(w *sql.DB, work func(*sql.Tx) error) error {
	tx, err := w.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := work(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type Querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type RowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

type CtxQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func QueryIDs(q Querier, query string, args ...any) ([]int64, error) {
	return QueryAll(q, scanOne[int64], query, args...)
}

// QueryIDsFunc stops the scan as soon as visit returns false.
func QueryIDsFunc(q Querier, visit func(int64) bool, query string, args ...any) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !visit(id) {
			return nil
		}
	}
	return rows.Err()
}

func QueryIDsContext(ctx context.Context, q CtxQuerier, query string, args ...any) ([]int64, error) {
	return QueryAllContext(ctx, q, scanOne[int64], query, args...)
}

// InPlaceholders returns "" for no input, which SQLite reads as an empty IN list (false; NOT IN true).
func InPlaceholders[T any](xs []T) (string, []any) {
	if len(xs) == 0 {
		return "", nil
	}
	args := make([]any, len(xs))
	for i, x := range xs {
		args[i] = x
	}
	return strings.Repeat("?,", len(xs)-1) + "?", args
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)

// EscapeLike's output needs ESCAPE '\' on the LIKE.
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}

// ASCIILower folds A-Z only, as SQLite's lower(), NOCASE and LIKE do, so a
// bound folding "É" to "é" would miss the stored "É".
func ASCIILower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}
