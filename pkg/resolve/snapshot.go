package resolve

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Snapshotter returns a (read-only) *repo.Repo that reflects the latest
// committed state of an on-disk repo. Implementations may cache and only
// reopen when the underlying state has actually changed; the contract is
// that callers can call Snapshot() per query without worrying about cost.
type Snapshotter interface {
	Snapshot() (*repo.Repo, error)
	Close() error
}

// PollingSnapshotter keeps a single read-only Repo open and reopens it
// only when one of the watched refs changes its hash. This replaces the
// v0 behaviour of opening Badger per DNS query (which capped throughput
// at single-digit hundreds of QPS) with a single Open at startup plus
// occasional reopens — typically zero per second on a quiet repo.
//
// The watcher runs in a background goroutine, polling at PollInterval
// (default 200ms). On detected change it opens a fresh Repo, then atomically
// swaps and closes the old one. Snapshot() never blocks on disk I/O.
type PollingSnapshotter struct {
	path         string
	branches     []string // branches whose hashes invalidate the cache
	pollInterval time.Duration

	cur atomic.Pointer[repo.Repo]

	// lastHashes tracks the hash we last saw for each watched ref. Mutated
	// only by the watcher goroutine, so no lock is needed.
	lastHashes map[string]store.Hash

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewPollingSnapshotter opens an initial read-only handle and starts the
// watcher. branches must include every branch the resolver might serve
// (default branch + canary branch, if any).
func NewPollingSnapshotter(path string, branches []string, pollInterval time.Duration) (*PollingSnapshotter, error) {
	if pollInterval <= 0 {
		pollInterval = 200 * time.Millisecond
	}
	r, err := repo.Open(repo.Options{Path: path, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("snapshot: initial open: %w", err)
	}
	ps := &PollingSnapshotter{
		path:         path,
		branches:     branches,
		pollInterval: pollInterval,
		lastHashes:   make(map[string]store.Hash),
		stop:         make(chan struct{}),
	}
	ps.cur.Store(r)
	ps.captureHashes(r)
	ps.wg.Add(1)
	go ps.watch()
	return ps, nil
}

// Snapshot returns the current cached Repo. The pointer is safe to use
// concurrently for reads. Writers (Set/Delete/Commit/Init) will fail
// because the underlying store is opened read-only.
func (p *PollingSnapshotter) Snapshot() (*repo.Repo, error) {
	if r := p.cur.Load(); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("snapshot: no current repo")
}

// Close stops the watcher and releases the cached handle.
func (p *PollingSnapshotter) Close() error {
	close(p.stop)
	p.wg.Wait()
	if r := p.cur.Swap(nil); r != nil {
		return r.Close()
	}
	return nil
}

func (p *PollingSnapshotter) watch() {
	defer p.wg.Done()
	t := time.NewTicker(p.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.tick()
		}
	}
}

// tick opens a fresh read-only handle to discover the current ref state,
// and either swaps it in (if any watched branch moved) or discards it.
//
// Badger's read-only handle is frozen at open time: it does NOT see writes
// made by the separate writer process. To detect updates we must open a
// new handle. We pay one Open per tick (~10–50ms on a small repo) instead
// of one Open per DNS query (which is what the v0 daemon did). On a quiet
// repo the fresh handle is closed and the cached one keeps serving — so
// per-query cost stays at one atomic pointer load.
func (p *PollingSnapshotter) tick() {
	fresh, err := repo.Open(repo.Options{Path: p.path, ReadOnly: true})
	if err != nil {
		// Writer mid-commit or FS hiccup — keep the existing handle.
		return
	}
	ctx := context.Background()
	changed := false
	freshHashes := make(map[string]store.Hash, len(p.branches))
	for _, b := range p.branches {
		h, err := fresh.Refs().GetBranch(ctx, b)
		if err != nil {
			h = store.ZeroHash
		}
		freshHashes[b] = h
		if p.lastHashes[b] != h {
			changed = true
		}
	}
	if !changed {
		_ = fresh.Close()
		return
	}
	p.lastHashes = freshHashes
	old := p.cur.Swap(fresh)
	if old != nil {
		_ = old.Close()
	}
}

func (p *PollingSnapshotter) captureHashes(r *repo.Repo) {
	ctx := context.Background()
	for _, b := range p.branches {
		h, err := r.Refs().GetBranch(ctx, b)
		if err != nil {
			h = store.ZeroHash
		}
		p.lastHashes[b] = h
	}
}
