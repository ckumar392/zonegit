// Package memstore is an in-memory implementation of store.Storage used
// for tests, demos, and embedded scenarios where durability is not
// required. It is fully thread-safe.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ckumar392/dnsdb/pkg/store"
)

// Store is an in-memory store.Storage. The zero value is NOT usable; call
// New() to construct one with initialized maps.
type Store struct {
	mu      sync.RWMutex
	objects map[store.Hash]store.Object
	refs    map[string]store.Hash
	reflog  map[string][]store.ReflogEntry
}

// New returns a fresh, empty Store.
func New() *Store {
	return &Store{
		objects: make(map[store.Hash]store.Object),
		refs:    make(map[string]store.Hash),
		reflog:  make(map[string][]store.ReflogEntry),
	}
}

// Compile-time interface check.
var _ store.Storage = (*Store)(nil)

// PutObject stores obj under h. Re-puts of an existing hash are accepted
// silently (the bytes are guaranteed identical by the CAS property of
// content addressing).
func (s *Store) PutObject(_ context.Context, h store.Hash, obj store.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := store.Object{
		Kind:    obj.Kind,
		Payload: append([]byte(nil), obj.Payload...),
	}
	s.objects[h] = cp
	return nil
}

// GetObject returns the object stored under h, or store.ErrNotFound.
func (s *Store) GetObject(_ context.Context, h store.Hash) (store.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[h]
	if !ok {
		return store.Object{}, fmt.Errorf("object %s: %w", h.Short(), store.ErrNotFound)
	}
	cp := store.Object{
		Kind:    obj.Kind,
		Payload: append([]byte(nil), obj.Payload...),
	}
	return cp, nil
}

// HasObject reports whether h is present.
func (s *Store) HasObject(_ context.Context, h store.Hash) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.objects[h]
	return ok, nil
}

// IterObjects calls fn for every (hash, object). Iteration order is
// deterministic (hash-ascending) for test reproducibility.
func (s *Store) IterObjects(_ context.Context, fn func(store.Hash, store.Object) error) error {
	s.mu.RLock()
	hashes := make([]store.Hash, 0, len(s.objects))
	for h := range s.objects {
		hashes = append(hashes, h)
	}
	s.mu.RUnlock()
	sort.Slice(hashes, func(i, j int) bool {
		// Compare byte-by-byte.
		for k := 0; k < store.HashSize; k++ {
			if hashes[i][k] != hashes[j][k] {
				return hashes[i][k] < hashes[j][k]
			}
		}
		return false
	})
	for _, h := range hashes {
		s.mu.RLock()
		obj, ok := s.objects[h]
		s.mu.RUnlock()
		if !ok {
			continue // concurrent removal — currently impossible
		}
		cp := store.Object{Kind: obj.Kind, Payload: append([]byte(nil), obj.Payload...)}
		if err := fn(h, cp); err != nil {
			return err
		}
	}
	return nil
}

// GetRef returns the hash for name and whether it exists.
func (s *Store) GetRef(_ context.Context, name string) (store.Hash, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.refs[name]
	return h, ok, nil
}

// CASRef updates name iff its current value equals expected.
//
// A zero expected hash means "create only if absent". A zero next hash is
// rejected (use DeleteRef instead).
func (s *Store) CASRef(_ context.Context, name string, expected, next store.Hash) error {
	if next.IsZero() {
		return fmt.Errorf("CASRef: next hash is zero (use DeleteRef): %w", store.ErrInvalidObject)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.refs[name]
	switch {
	case !ok && expected.IsZero():
		// Creating a new ref; OK.
	case ok && cur == expected:
		// Updating an existing ref; OK.
	default:
		return fmt.Errorf("CASRef %s: %w", name, store.ErrConflict)
	}
	s.refs[name] = next
	return nil
}

// DeleteRef removes name iff its current value equals expected.
func (s *Store) DeleteRef(_ context.Context, name string, expected store.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.refs[name]
	if !ok {
		return fmt.Errorf("DeleteRef %s: %w", name, store.ErrNotFound)
	}
	if cur != expected {
		return fmt.Errorf("DeleteRef %s: %w", name, store.ErrConflict)
	}
	delete(s.refs, name)
	return nil
}

// ListRefs returns refs whose name has the given prefix, sorted by name.
func (s *Store) ListRefs(_ context.Context, prefix string) ([]store.RefEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.RefEntry, 0)
	for name, h := range s.refs {
		if strings.HasPrefix(name, prefix) {
			out = append(out, store.RefEntry{Name: name, Hash: h})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AppendReflog appends e to the named reflog.
func (s *Store) AppendReflog(_ context.Context, name string, e store.ReflogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reflog[name] = append(s.reflog[name], e)
	return nil
}

// ReadReflog returns a copy of the reflog entries for name.
func (s *Store) ReadReflog(_ context.Context, name string) ([]store.ReflogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.reflog[name]
	out := make([]store.ReflogEntry, len(src))
	copy(out, src)
	return out, nil
}

// Close is a no-op for the memory store.
func (s *Store) Close() error { return nil }
