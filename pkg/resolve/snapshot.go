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

// StaticSnapshotter always returns the same Repo handle. Used in
// secondary daemons that hold the repo open writable so the replication
// client can land incoming objects in the same handle the resolver
// reads from — there's no separate writer process, so the polling /
// reopen dance is unnecessary.
//
// The handle is closed by Close. Callers must not close it themselves.
type StaticSnapshotter struct {
	R *repo.Repo
}

func (s *StaticSnapshotter) Snapshot() (*repo.Repo, error) {
	if s.R == nil {
		return nil, fmt.Errorf("StaticSnapshotter: nil repo")
	}
	return s.R, nil
}

func (s *StaticSnapshotter) Close() error {
	if s.R == nil {
		return nil
	}
	return s.R.Close()
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
//
// `refsToWatch` is a list of full ref paths (e.g. "refs/heads/foo.com./main").
// Multi-zone daemons pass one entry per (zone, branch) pair they serve.
type PollingSnapshotter struct {
	path         string
	pollInterval time.Duration

	cur atomic.Pointer[repo.Repo]

	// refsMu guards the watched-ref list. SetWatchedRefs may be called
	// from the daemon's zone-watcher goroutine concurrently with the
	// snapshotter's own polling goroutine.
	refsMu sync.RWMutex
	refs   []string

	// lastHashes tracks the hash we last saw for each watched ref. Mutated
	// only by the watcher goroutine, so no lock is needed.
	lastHashes map[string]store.Hash

	// graceClose is how long a swapped-out handle is kept open before it is
	// closed. An in-flight query (or a zone transfer, up to 30s) may still
	// hold the pointer a previous Snapshot() returned; closing it immediately
	// on swap would read from a closed store. Retired handles live in pending
	// and are swept on later ticks. Must exceed the 30s transfer context in
	// axfr.go/ixfr.go. (Reference-counted handles would be tighter; this is
	// the simple, safe version.) Mutated only by the watcher goroutine.
	graceClose time.Duration
	pending    []pendingClose

	stop chan struct{}
	wg   sync.WaitGroup
}

// pendingClose is a handle retired by a swap, awaiting its grace period.
type pendingClose struct {
	r  *repo.Repo
	at time.Time
}

// defaultCloseGrace must comfortably exceed the longest context a query can
// hold a snapshot handle — the 30s AXFR/IXFR transfer context.
const defaultCloseGrace = 45 * time.Second

// SetWatchedRefs replaces the list of refs whose hashes invalidate the
// cached snapshot. Used when a new zone is registered at runtime so its
// branches enter the watch set.
func (p *PollingSnapshotter) SetWatchedRefs(refsToWatch []string) {
	p.refsMu.Lock()
	p.refs = append(p.refs[:0:0], refsToWatch...)
	p.refsMu.Unlock()
}

// watchedRefs returns a copy of the current watch list. Cheap; called
// once per tick.
func (p *PollingSnapshotter) watchedRefs() []string {
	p.refsMu.RLock()
	out := append([]string(nil), p.refs...)
	p.refsMu.RUnlock()
	return out
}

// NewPollingSnapshotter opens an initial read-only handle and starts the
// watcher. refsToWatch must include every ref the resolver might serve
// from (default + canary branches across every zone).
func NewPollingSnapshotter(path string, refsToWatch []string, pollInterval time.Duration) (*PollingSnapshotter, error) {
	if pollInterval <= 0 {
		pollInterval = 200 * time.Millisecond
	}
	r, err := repo.Open(repo.Options{Path: path, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("snapshot: initial open: %w", err)
	}
	ps := &PollingSnapshotter{
		path:         path,
		refs:         append([]string(nil), refsToWatch...),
		pollInterval: pollInterval,
		lastHashes:   make(map[string]store.Hash),
		graceClose:   defaultCloseGrace,
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
	for _, pc := range p.pending {
		_ = pc.r.Close()
	}
	p.pending = nil
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
		p.sweepPending(time.Now())
		return
	}
	ctx := context.Background()
	watched := p.watchedRefs()
	changed := false
	freshHashes := make(map[string]store.Hash, len(watched))
	for _, ref := range watched {
		h, ok, err := fresh.Storage().GetRef(ctx, ref)
		if err != nil || !ok {
			h = store.ZeroHash
		}
		freshHashes[ref] = h
		if p.lastHashes[ref] != h {
			changed = true
		}
	}
	if !changed {
		_ = fresh.Close()
		p.sweepPending(time.Now())
		return
	}
	p.lastHashes = freshHashes
	if old := p.cur.Swap(fresh); old != nil {
		// Retire, don't close: an in-flight query may still hold `old` from
		// an earlier Snapshot(). It is closed once its grace period elapses.
		p.pending = append(p.pending, pendingClose{r: old, at: time.Now()})
	}
	p.sweepPending(time.Now())
}

// sweepPending closes any retired handle whose grace period has elapsed.
// Called on every tick (including no-change ticks) so retired handles still
// get closed on a quiet repo.
func (p *PollingSnapshotter) sweepPending(now time.Time) {
	kept := p.pending[:0]
	for _, pc := range p.pending {
		if now.Sub(pc.at) >= p.graceClose {
			_ = pc.r.Close()
		} else {
			kept = append(kept, pc)
		}
	}
	p.pending = kept
}

func (p *PollingSnapshotter) captureHashes(r *repo.Repo) {
	ctx := context.Background()
	for _, ref := range p.watchedRefs() {
		h, ok, err := r.Storage().GetRef(ctx, ref)
		if err != nil || !ok {
			h = store.ZeroHash
		}
		p.lastHashes[ref] = h
	}
}
