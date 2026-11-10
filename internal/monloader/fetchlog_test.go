package monloader

import (
	"testing"
	"time"
)

func TestPruneFetchStatus_DropsPastTTL(t *testing.T) {
	f := newFetchLog()
	g := "default"

	f.Record(g, 7, "ok", "done")
	f.Record(g, 8, "ok", "done")
	f.mu.Lock()
	entry := f.m[fetchKey(g, 7)]
	entry.At = time.Now().Add(-fetchTTL - time.Minute)
	f.m[fetchKey(g, 7)] = entry
	f.mu.Unlock()

	f.Prune()

	if _, ok := f.Load(g, 7); ok {
		t.Error("entry past the TTL survived the prune")
	}
	if _, ok := f.Load(g, 8); !ok {
		t.Error("prune dropped a live entry")
	}
}
