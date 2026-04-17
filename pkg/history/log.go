package history

import (
	"context"
	"fmt"
	"time"

	"github.com/ckumar392/dnsdb/pkg/object"
	"github.com/ckumar392/dnsdb/pkg/store"
)

// Entry is one commit in a log listing.
type Entry struct {
	Hash   store.Hash
	Commit object.Commit
}

// Log walks the first-parent chain starting at head and returns up to max
// commits (oldest last). max <= 0 means unlimited.
//
// First-parent walking matches git log's default behavior — for v0 we
// don't visit merge sub-history. Multi-parent traversal is a v1+ concern.
func Log(ctx context.Context, s store.Storage, head store.Hash, max int) ([]Entry, error) {
	out := make([]Entry, 0, 16)
	cur := head
	for !cur.IsZero() {
		if max > 0 && len(out) >= max {
			break
		}
		obj, err := s.GetObject(ctx, cur)
		if err != nil {
			return nil, fmt.Errorf("log: load %s: %w", cur.Short(), err)
		}
		if obj.Kind != "commit" {
			return nil, fmt.Errorf("log: %s is %s, not commit", cur.Short(), obj.Kind)
		}
		c, err := object.DecodeCommit(obj.Payload)
		if err != nil {
			return nil, fmt.Errorf("log: decode %s: %w", cur.Short(), err)
		}
		out = append(out, Entry{Hash: cur, Commit: c})
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return out, nil
}

// WalkAt returns the most recent commit at or before t, walking the
// first-parent chain from head. Returns ErrNotFound if every reachable
// commit is newer than t.
func WalkAt(ctx context.Context, s store.Storage, head store.Hash, t time.Time) (store.Hash, object.Commit, error) {
	cur := head
	for !cur.IsZero() {
		obj, err := s.GetObject(ctx, cur)
		if err != nil {
			return store.ZeroHash, object.Commit{}, err
		}
		c, err := object.DecodeCommit(obj.Payload)
		if err != nil {
			return store.ZeroHash, object.Commit{}, err
		}
		if !c.CommitTime.After(t) {
			return cur, c, nil
		}
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return store.ZeroHash, object.Commit{}, fmt.Errorf("walk-at %s: %w", t.Format(time.RFC3339), store.ErrNotFound)
}
