package jobs

import (
	"context"
	"errors"
	"testing"
)

func TestChunkedWalksEveryChunk(t *testing.T) {
	var seen [][]int64
	processed, cancelled, err := Chunked(context.Background(), nil, []int64{1, 2, 3, 4, 5}, 2, "counting",
		func(chunk []int64) error {
			seen = append(seen, append([]int64(nil), chunk...))
			return nil
		})
	if err != nil || cancelled {
		t.Fatalf("Chunked = (%d, %v, %v), want (5, false, nil)", processed, cancelled, err)
	}
	if processed != 5 {
		t.Errorf("processed = %d, want 5", processed)
	}
	if len(seen) != 3 || len(seen[2]) != 1 {
		t.Errorf("chunks = %v, want three ending in one id", seen)
	}
}

func TestChunkedStopsBetweenChunksOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	processed, cancelled, err := Chunked(ctx, nil, []int64{1, 2, 3, 4, 5, 6}, 2, "counting",
		func([]int64) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Chunked err = %v, want nil", err)
	}
	if !cancelled {
		t.Error("cancelled = false, want true")
	}
	if processed != 4 {
		t.Errorf("processed = %d, want 4 (the chunk that was mid-flight still counts)", processed)
	}
	if calls != 2 {
		t.Errorf("op ran %d times, want 2", calls)
	}
}

func TestChunkedReportsTheFailingChunk(t *testing.T) {
	boom := errors.New("boom")
	processed, cancelled, err := Chunked(context.Background(), nil, []int64{1, 2, 3, 4}, 2, "counting",
		func(chunk []int64) error {
			if chunk[0] == 3 {
				return boom
			}
			return nil
		})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if cancelled {
		t.Error("cancelled = true on a failure, want false")
	}
	if processed != 2 {
		t.Errorf("processed = %d, want 2", processed)
	}
}

func TestChunkedReportsProgress(t *testing.T) {
	m := NewManager()
	mustStart(t, m, "tag")
	if _, _, err := Chunked(context.Background(), m, []int64{1, 2, 3}, 2, "counting",
		func([]int64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	state := m.Get()
	if state.Processed != 3 || state.Total != 3 {
		t.Errorf("progress = %d/%d, want 3/3", state.Processed, state.Total)
	}
	if state.Message != "counting…" {
		t.Errorf("message = %q, want %q", state.Message, "counting…")
	}
}
