package db

import (
	"reflect"
	"testing"
)

func TestInPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int64
		body string
		args []any
	}{
		{"empty returns blank body and nil args", []int64{}, "", nil},
		{"nil slice returns blank body and nil args", nil, "", nil},
		{"single element has no trailing comma", []int64{7}, "?", []any{int64(7)}},
		{"many elements join with commas and no trailing comma", []int64{1, 2, 3}, "?,?,?", []any{int64(1), int64(2), int64(3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, args := InPlaceholders(tc.in)
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
			if !reflect.DeepEqual(args, tc.args) {
				t.Errorf("args = %#v, want %#v", args, tc.args)
			}
		})
	}

	t.Run("preserves element type and order with strings", func(t *testing.T) {
		body, args := InPlaceholders([]string{"a", "b"})
		if body != "?,?" {
			t.Errorf("body = %q, want %q", body, "?,?")
		}
		if !reflect.DeepEqual(args, []any{"a", "b"}) {
			t.Errorf("args = %#v, want [a b]", args)
		}
	})
}

func TestChunked(t *testing.T) {
	collect := func(xs []int, chunkSize int) [][]int {
		var got [][]int
		err := Chunked(xs, chunkSize, func(chunk []int) error {
			cp := append([]int(nil), chunk...)
			got = append(got, cp)
			return nil
		})
		if err != nil {
			t.Fatalf("Chunked: %v", err)
		}
		return got
	}

	for _, tc := range []struct {
		name string
		xs   []int
		size int
		want [][]int
	}{
		{"empty input makes no calls", []int{}, 3, nil},
		{"single element single chunk", []int{9}, 3, [][]int{{9}}},
		{"exact multiple of chunk size", []int{1, 2, 3, 4}, 2, [][]int{{1, 2}, {3, 4}}},
		{"non-multiple leaves a short trailing chunk", []int{1, 2, 3, 4, 5}, 2, [][]int{{1, 2}, {3, 4}, {5}}},
		{"chunk larger than input yields one chunk", []int{1, 2}, 10, [][]int{{1, 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collect(tc.xs, tc.size); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chunks = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("callback error short-circuits and propagates", func(t *testing.T) {
		wantErr := errSentinel
		calls := 0
		err := Chunked([]int{1, 2, 3, 4, 5}, 2, func(chunk []int) error {
			calls++
			return wantErr
		})
		if err != wantErr {
			t.Errorf("err = %v, want %v", err, wantErr)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (should stop on first error)", calls)
		}
	})
}

var errSentinel = sentinelErr("boom")

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

func TestQueryIDs(t *testing.T) {
	database := openTestDB(t)
	mustBootstrap(t, database)
	for _, id := range []int64{10, 20, 30} {
		mustExec(t, database.Write, `INSERT INTO images (id, sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 'png', 1)`, id, "sha"+string(rune('a'+id)), "/p/"+string(rune('a'+id)))
	}

	t.Run("empty result set returns nil slice", func(t *testing.T) {
		ids, err := QueryIDs(database.Read, `SELECT id FROM images WHERE id < 0 ORDER BY id`)
		if err != nil {
			t.Fatalf("QueryIDs: %v", err)
		}
		if ids != nil {
			t.Errorf("ids = %#v, want nil for empty result", ids)
		}
	})

	t.Run("single row", func(t *testing.T) {
		ids, err := QueryIDs(database.Read, `SELECT id FROM images WHERE id = 20`)
		if err != nil {
			t.Fatalf("QueryIDs: %v", err)
		}
		if !reflect.DeepEqual(ids, []int64{20}) {
			t.Errorf("ids = %v, want [20]", ids)
		}
	})

	t.Run("many rows in query order", func(t *testing.T) {
		ids, err := QueryIDs(database.Read, `SELECT id FROM images ORDER BY id`)
		if err != nil {
			t.Fatalf("QueryIDs: %v", err)
		}
		if !reflect.DeepEqual(ids, []int64{10, 20, 30}) {
			t.Errorf("ids = %v, want [10 20 30]", ids)
		}
	})

	t.Run("non-integer column surfaces a scan error", func(t *testing.T) {
		if _, err := QueryIDs(database.Read, `SELECT canonical_path FROM images ORDER BY id`); err == nil {
			t.Error("expected scan error on TEXT column, got nil")
		}
	})
}

func TestEscapeLike(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text untouched", "hello", "hello"},
		{"percent escaped", "50%", `50\%`},
		{"underscore escaped", "a_b", `a\_b`},
		{"backslash escaped", `a\b`, `a\\b`},
		{"all metacharacters together", `a_b%c\d`, `a\_b\%c\\d`},
		{"escape char doubled before metachar", `\%`, `\\\%`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EscapeLike(c.in); got != c.want {
				t.Errorf("EscapeLike(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	t.Run("escaped pattern matches literally under ESCAPE clause", func(t *testing.T) {
		database := openTestDB(t)
		mustBootstrap(t, database)

		for i, sha := range []string{"a_b", "axb"} {
			mustExec(t, database.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 1)`, sha, "/p/"+string(rune('a'+i)))
		}
		pattern := EscapeLike("a_b") + "%"
		got, err := QueryStrings(database.Read, `SELECT sha256 FROM images WHERE sha256 LIKE ? ESCAPE '\' ORDER BY sha256`, pattern)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"a_b"}) {
			t.Errorf("matched %v, want only [a_b] (underscore must be literal, not a wildcard)", got)
		}
	})
}
