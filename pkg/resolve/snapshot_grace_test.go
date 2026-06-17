package resolve

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// TestPollingSnapshotterKeepsOldHandleForInflightQueries pins the use-after-
// close bug: when a watched branch moves and the snapshotter swaps in a fresh
// handle, an in-flight query that already obtained the previous handle via
// Snapshot() must still be able to read from it. Closing the old handle
// immediately on swap (as the daemon does ~every 200ms on a live repo) would
// make concurrent lookups and 30s zone transfers fail or race.
func TestPollingSnapshotterKeepsOldHandleForInflightQueries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/repo"

	commit := func(rr string) {
		w, err := repo.Open(repo.Options{Path: dir})
		if err != nil {
			t.Fatalf("open writable: %v", err)
		}
		if _, _, _, err := w.Head(ctx); err != nil {
			if err := w.Init(ctx, "foo.com."); err != nil {
				t.Fatalf("init: %v", err)
			}
		}
		parsed, err := dns.NewRR(rr)
		if err != nil {
			t.Fatalf("NewRR: %v", err)
		}
		if err := w.Set(ctx, []dns.RR{parsed}); err != nil {
			t.Fatalf("set: %v", err)
		}
		if _, err := w.Commit(ctx, object.Identity{Name: "t", Email: "t@t"}, "x"); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_ = w.Close()
	}

	commit("api.foo.com. 300 IN A 1.2.3.4")

	// Long poll interval so the background watcher never ticks during the
	// test — we drive tick() by hand.
	ps, err := NewPollingSnapshotter(dir, []string{"refs/heads/foo.com./main"}, time.Hour)
	if err != nil {
		t.Fatalf("NewPollingSnapshotter: %v", err)
	}
	defer func() { _ = ps.Close() }()

	// An in-flight query grabs the current handle.
	old, err := ps.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := old.Lookup(ctx, store.ZeroHash, "api", "A"); err != nil {
		t.Fatalf("baseline lookup on snapshot handle: %v", err)
	}

	// The branch moves; the snapshotter notices and swaps in a fresh handle.
	commit("api.foo.com. 300 IN A 9.9.9.9")
	ps.tick()
	if fresh, _ := ps.Snapshot(); fresh == old {
		t.Fatal("tick did not swap in a fresh handle after the branch moved")
	}

	// The query still holds `old`. Immediate close on swap would make this fail.
	if _, err := old.Lookup(ctx, store.ZeroHash, "api", "A"); err != nil {
		t.Fatalf("old snapshot handle was closed out from under an in-flight query: %v", err)
	}
}
