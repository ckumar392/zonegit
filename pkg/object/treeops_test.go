package object

import (
	"context"
	"errors"
	"testing"

	"github.com/ckumar392/dnsdb/pkg/store"
	"github.com/ckumar392/dnsdb/pkg/store/memstore"
)

// I4 (NotFound): walking a missing path returns store.ErrNotFound.
func TestInvariant_WalkMissingPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := memstore.New()

	// Build: root -> "api" -> {A: leaf}
	leaf := DecodeBlob([]byte("rrset"))
	leafHash, leafObj := leaf.Encode()
	if err := s.PutObject(ctx, leafHash, leafObj); err != nil {
		t.Fatal(err)
	}

	apiTree := Tree{Entries: []TreeEntry{
		{Kind: EntryLeaf, Name: "A", Hash: leafHash},
	}}
	apiHash, err := PutTree(ctx, s, apiTree)
	if err != nil {
		t.Fatal(err)
	}

	root := Tree{Entries: []TreeEntry{
		{Kind: EntrySubtree, Name: "api", Hash: apiHash},
	}}
	rootHash, err := PutTree(ctx, s, root)
	if err != nil {
		t.Fatal(err)
	}

	// Existing: root/api A -> found.
	if got, err := WalkTree(ctx, s, rootHash, []string{"api"}, "A"); err != nil || got != leafHash {
		t.Errorf("walk existing: %v %v", got, err)
	}

	// Missing label.
	_, err = WalkTree(ctx, s, rootHash, []string{"missing"}, "A")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing label: want ErrNotFound, got %v", err)
	}

	// Missing leaf.
	_, err = WalkTree(ctx, s, rootHash, []string{"api"}, "AAAA")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing leaf: want ErrNotFound, got %v", err)
	}
}

// UpdateTree adds a leaf to a fresh (zero-root) tree, creating any missing
// intermediates, and the resulting root resolves the leaf via WalkTree.
func TestUpdateTree_FromEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := memstore.New()

	leaf := DecodeBlob([]byte("the rrset"))
	leafHash, leafObj := leaf.Encode()
	if err := s.PutObject(ctx, leafHash, leafObj); err != nil {
		t.Fatal(err)
	}

	root, err := UpdateTree(ctx, s, store.ZeroHash, []string{"internal", "db"}, "A", leafHash)
	if err != nil {
		t.Fatalf("UpdateTree: %v", err)
	}
	if root.IsZero() {
		t.Fatal("expected non-zero root")
	}

	got, err := WalkTree(ctx, s, root, []string{"internal", "db"}, "A")
	if err != nil {
		t.Fatalf("walk after update: %v", err)
	}
	if got != leafHash {
		t.Fatalf("walk hash: got %s want %s", got.Short(), leafHash.Short())
	}
}

// Updating a leaf produces a new root, but unchanged sibling subtrees are
// reused by hash (structural sharing — the property that makes commits cheap).
func TestUpdateTree_StructuralSharing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := memstore.New()

	leafA := DecodeBlob([]byte("v1"))
	hA, oA := leafA.Encode()
	if err := s.PutObject(ctx, hA, oA); err != nil {
		t.Fatal(err)
	}
	leafB := DecodeBlob([]byte("static"))
	hB, oB := leafB.Encode()
	if err := s.PutObject(ctx, hB, oB); err != nil {
		t.Fatal(err)
	}

	// Build root with two siblings: api/A and stable/A.
	r1, err := UpdateTree(ctx, s, store.ZeroHash, []string{"api"}, "A", hA)
	if err != nil {
		t.Fatal(err)
	}
	r1, err = UpdateTree(ctx, s, r1, []string{"stable"}, "A", hB)
	if err != nil {
		t.Fatal(err)
	}

	// Capture the unchanged sibling subtree's hash.
	r1Tree, err := LoadTree(ctx, s, r1)
	if err != nil {
		t.Fatal(err)
	}
	stableEntry, ok := r1Tree.Get(EntrySubtree, "stable")
	if !ok {
		t.Fatal("stable subtree missing")
	}
	stableHashBefore := stableEntry.Hash

	// Change api/A to a new value.
	leafA2 := DecodeBlob([]byte("v2"))
	hA2, oA2 := leafA2.Encode()
	if err := s.PutObject(ctx, hA2, oA2); err != nil {
		t.Fatal(err)
	}
	r2, err := UpdateTree(ctx, s, r1, []string{"api"}, "A", hA2)
	if err != nil {
		t.Fatal(err)
	}
	if r2 == r1 {
		t.Fatal("changing a leaf must produce a new root hash")
	}

	r2Tree, err := LoadTree(ctx, s, r2)
	if err != nil {
		t.Fatal(err)
	}
	stableEntry2, ok := r2Tree.Get(EntrySubtree, "stable")
	if !ok {
		t.Fatal("stable subtree missing in r2")
	}
	if stableEntry2.Hash != stableHashBefore {
		t.Fatal("unchanged sibling subtree hash must be preserved (structural sharing)")
	}
}

// Setting a leaf to ZeroHash deletes it; deleting the last leaf prunes the
// subtree all the way up — but the zone root is allowed to be empty.
func TestUpdateTree_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := memstore.New()

	leaf := DecodeBlob([]byte("doomed"))
	h, o := leaf.Encode()
	if err := s.PutObject(ctx, h, o); err != nil {
		t.Fatal(err)
	}

	r, err := UpdateTree(ctx, s, store.ZeroHash, []string{"internal", "db"}, "A", h)
	if err != nil {
		t.Fatal(err)
	}

	// Walk should find it.
	if _, err := WalkTree(ctx, s, r, []string{"internal", "db"}, "A"); err != nil {
		t.Fatal(err)
	}

	// Delete the leaf.
	r2, err := UpdateTree(ctx, s, r, []string{"internal", "db"}, "A", store.ZeroHash)
	if err != nil {
		t.Fatal(err)
	}
	// internal/db should be pruned (empty); internal should also be pruned.
	// The root tree should have no "internal" subtree.
	rootTree, err := LoadTree(ctx, s, r2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rootTree.Get(EntrySubtree, "internal"); ok {
		t.Fatal("expected 'internal' to be pruned after deleting its only leaf")
	}
}
