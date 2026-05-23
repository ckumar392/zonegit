package object

import (
	"context"
	"fmt"

	"github.com/ckumar392/zonegit/pkg/store"
)

// LoadTree fetches a Tree by hash from a Storage.
func LoadTree(ctx context.Context, s store.Storage, h store.Hash) (Tree, error) {
	obj, err := s.GetObject(ctx, h)
	if err != nil {
		return Tree{}, fmt.Errorf("load tree %s: %w", h.Short(), err)
	}
	if Kind(obj.Kind) != KindTree {
		return Tree{}, fmt.Errorf("load tree %s: kind=%q: %w", h.Short(), obj.Kind, store.ErrInvalidObject)
	}
	if err := Verify(h, obj); err != nil {
		return Tree{}, fmt.Errorf("load tree %s: %w", h.Short(), err)
	}
	return DecodeTree(obj.Payload)
}

// PutTree encodes t and writes it to s. Returns the hash.
func PutTree(ctx context.Context, s store.Storage, t Tree) (store.Hash, error) {
	for _, e := range t.Entries {
		if err := validateEntry(e); err != nil {
			return store.Hash{}, err
		}
	}
	h, obj := t.Encode()
	if err := s.PutObject(ctx, h, obj); err != nil {
		return store.Hash{}, fmt.Errorf("put tree %s: %w", h.Short(), err)
	}
	return h, nil
}

// WalkTree traverses path through nested Tree objects starting at root,
// then returns the leaf entry whose Name matches rrtype. Each path element
// is a single DNS label (e.g. ["api"] for api.<zone>).
//
// Returns store.ErrNotFound if any intermediate label or the final
// rrtype leaf is absent.
func WalkTree(ctx context.Context, s store.Storage, root store.Hash, path []string, rrtype string) (store.Hash, error) {
	cur := root
	for i, label := range path {
		t, err := LoadTree(ctx, s, cur)
		if err != nil {
			return store.Hash{}, err
		}
		e, ok := t.Get(EntrySubtree, label)
		if !ok {
			return store.Hash{}, fmt.Errorf("walk tree at %q (depth %d): %w", label, i, store.ErrNotFound)
		}
		cur = e.Hash
	}
	leafTree, err := LoadTree(ctx, s, cur)
	if err != nil {
		return store.Hash{}, err
	}
	e, ok := leafTree.Get(EntryLeaf, rrtype)
	if !ok {
		return store.Hash{}, fmt.Errorf("walk tree leaf %q: %w", rrtype, store.ErrNotFound)
	}
	return e.Hash, nil
}

// WalkAllLeaves visits every leaf (RRset) reachable from root in
// depth-first, sorted order. The callback receives the labels path from
// the zone apex down to (but excluding) the leaf, plus the leaf's name
// (RR-type mnemonic) and blob hash.
//
// This is the enumeration AXFR and zone-export workflows need.
func WalkAllLeaves(ctx context.Context, s store.Storage, root store.Hash, fn func(path []string, rrtype string, blobHash store.Hash) error) error {
	if root.IsZero() {
		return nil
	}
	return walkLeavesAt(ctx, s, nil, root, fn)
}

func walkLeavesAt(ctx context.Context, s store.Storage, path []string, h store.Hash, fn func([]string, string, store.Hash) error) error {
	t, err := LoadTree(ctx, s, h)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		switch e.Kind {
		case EntryLeaf:
			if err := fn(path, e.Name, e.Hash); err != nil {
				return err
			}
		case EntrySubtree:
			sub := append(path[:len(path):len(path)], e.Name)
			if err := walkLeavesAt(ctx, s, sub, e.Hash, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// UpdateTree returns the new root hash of a tree where the leaf
// (path, rrtype) is set to leafHash.
//
// If leafHash is zero, the leaf is removed; if removing it leaves a Tree
// node empty, that subtree is also pruned (and so on recursively, except
// for the top-level zone root which is allowed to be empty).
//
// Pre-existing path elements that are unchanged are reused by hash —
// only the spine from leaf to root is rewritten. This is what makes
// committing a single-RR change cost ~O(zone-depth) writes regardless of
// total zone size.
//
// All new Tree objects are written to s; the caller is responsible for
// updating the ref pointing at the new root.
func UpdateTree(ctx context.Context, s store.Storage, root store.Hash, path []string, rrtype string, leafHash store.Hash) (store.Hash, error) {
	// Load the chain of trees from root to the deepest existing path element.
	chain := make([]Tree, 0, len(path)+1)
	{
		var cur store.Hash
		// If root is zero, start with an empty tree.
		if root.IsZero() {
			chain = append(chain, Tree{})
		} else {
			t, err := LoadTree(ctx, s, root)
			if err != nil {
				return store.Hash{}, err
			}
			chain = append(chain, t)
			cur = root
			_ = cur
		}
	}
	// Descend, creating empty trees along the way for missing labels.
	for _, label := range path {
		parent := chain[len(chain)-1]
		e, ok := parent.Get(EntrySubtree, label)
		if ok {
			child, err := LoadTree(ctx, s, e.Hash)
			if err != nil {
				return store.Hash{}, err
			}
			chain = append(chain, child)
		} else {
			chain = append(chain, Tree{})
		}
	}

	// Update the leaf at the deepest tree.
	leafTree := chain[len(chain)-1]
	leafTree = setLeaf(leafTree, rrtype, leafHash)
	chain[len(chain)-1] = leafTree

	// Walk back up: write each tree (or skip writing if it became empty
	// AND it's not the root), and update the parent to point at the new hash.
	var lastHash store.Hash
	for i := len(chain) - 1; i >= 0; i-- {
		t := chain[i]
		isRoot := i == 0
		empty := len(t.Entries) == 0

		if empty && !isRoot {
			// Drop this subtree: parent will get its label removed below.
			lastHash = store.ZeroHash
		} else {
			h, err := PutTree(ctx, s, t)
			if err != nil {
				return store.Hash{}, err
			}
			lastHash = h
		}
		// Update parent's pointer to this label.
		if !isRoot {
			label := path[i-1]
			parent := chain[i-1]
			parent = setSubtree(parent, label, lastHash)
			chain[i-1] = parent
		}
	}
	return lastHash, nil
}

// setLeaf returns a new Tree with the leaf (rrtype -> hash) set, or removed
// if hash is zero. The returned slice is a fresh allocation; the input is
// not mutated.
func setLeaf(t Tree, rrtype string, hash store.Hash) Tree {
	return setEntry(t, EntryLeaf, rrtype, hash)
}

// setSubtree returns a new Tree with the subtree (label -> hash) set, or
// removed if hash is zero.
func setSubtree(t Tree, label string, hash store.Hash) Tree {
	return setEntry(t, EntrySubtree, label, hash)
}

func setEntry(t Tree, kind EntryKind, name string, hash store.Hash) Tree {
	out := make([]TreeEntry, 0, len(t.Entries)+1)
	replaced := false
	for _, e := range t.Entries {
		if e.Kind == kind && e.Name == name {
			if !hash.IsZero() {
				out = append(out, TreeEntry{Kind: kind, Name: name, Hash: hash})
			}
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced && !hash.IsZero() {
		out = append(out, TreeEntry{Kind: kind, Name: name, Hash: hash})
	}
	sortEntries(out)
	return Tree{Entries: out}
}
