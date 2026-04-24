package history_test

import (
	"context"
	"testing"
	"time"

	"github.com/ckumar392/zonegit/pkg/history"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/store/memstore"
)

func ctx() context.Context { return context.Background() }

// makeBlob writes a blob containing arbitrary bytes and returns its hash.
func makeBlob(t *testing.T, s store.Storage, payload string) store.Hash {
	t.Helper()
	b := object.Blob{Payload: []byte(payload)}
	h, obj := b.Encode()
	if err := s.PutObject(ctx(), h, obj); err != nil {
		t.Fatal(err)
	}
	return h
}

// makeCommit builds a commit on top of `parent` whose tree has `(path, rrtype)`
// pointing at `blob`. parent may be ZeroHash for root commits. If `prev` is
// non-zero, the new tree is built by updating prev's tree.
func makeCommit(t *testing.T, s store.Storage, parent store.Hash, prevTree store.Hash, path []string, rrtype string, blob store.Hash, when time.Time, msg string) store.Hash {
	t.Helper()
	newTree, err := object.UpdateTree(ctx(), s, prevTree, path, rrtype, blob)
	if err != nil {
		t.Fatal(err)
	}
	parents := []store.Hash{}
	if !parent.IsZero() {
		parents = append(parents, parent)
	}
	c := object.Commit{
		Tree:       newTree,
		Parents:    parents,
		Author:     object.Identity{Name: "test", Email: "t@t"},
		Committer:  object.Identity{Name: "test", Email: "t@t"},
		AuthorTime: when,
		CommitTime: when,
		Message:    msg,
	}
	h, obj := c.Encode()
	if err := s.PutObject(ctx(), h, obj); err != nil {
		t.Fatal(err)
	}
	return h
}

// commitTree returns the tree hash referenced by a commit.
func commitTree(t *testing.T, s store.Storage, h store.Hash) store.Hash {
	t.Helper()
	obj, err := s.GetObject(ctx(), h)
	if err != nil {
		t.Fatal(err)
	}
	c, err := object.DecodeCommit(obj.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return c.Tree
}

func TestLog(t *testing.T) {
	s := memstore.New()
	t0 := time.Unix(1000000, 0)
	b1 := makeBlob(t, s, "v1")
	c1 := makeCommit(t, s, store.ZeroHash, store.ZeroHash, []string{"api"}, "A", b1, t0, "first")
	b2 := makeBlob(t, s, "v2")
	c2 := makeCommit(t, s, c1, commitTree(t, s, c1), []string{"api"}, "A", b2, t0.Add(time.Hour), "second")
	b3 := makeBlob(t, s, "v3")
	c3 := makeCommit(t, s, c2, commitTree(t, s, c2), []string{"api"}, "A", b3, t0.Add(2*time.Hour), "third")

	entries, err := history.Log(ctx(), s, c3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[0].Hash != c3 || entries[2].Hash != c1 {
		t.Errorf("order wrong")
	}
	if entries[0].Commit.Message != "third" || entries[2].Commit.Message != "first" {
		t.Errorf("messages wrong")
	}

	// Limited.
	entries, _ = history.Log(ctx(), s, c3, 2)
	if len(entries) != 2 {
		t.Fatalf("limited len = %d", len(entries))
	}
}

func TestWalkAt(t *testing.T) {
	s := memstore.New()
	t0 := time.Unix(1000000, 0)
	b1 := makeBlob(t, s, "v1")
	c1 := makeCommit(t, s, store.ZeroHash, store.ZeroHash, []string{"api"}, "A", b1, t0, "first")
	b2 := makeBlob(t, s, "v2")
	c2 := makeCommit(t, s, c1, commitTree(t, s, c1), []string{"api"}, "A", b2, t0.Add(time.Hour), "second")

	// Exact match on c2.
	got, _, err := history.WalkAt(ctx(), s, c2, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != c2 {
		t.Errorf("got %s, want c2", got.Short())
	}

	// Between commits — should pick c1.
	got, _, err = history.WalkAt(ctx(), s, c2, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got != c1 {
		t.Errorf("got %s, want c1", got.Short())
	}

	// Before all commits.
	_, _, err = history.WalkAt(ctx(), s, c2, t0.Add(-time.Hour))
	if err == nil {
		t.Fatal("expected error for time before all commits")
	}
}

func TestDiff_AddRemoveModify(t *testing.T) {
	s := memstore.New()
	t0 := time.Unix(1000000, 0)

	// c1: api A = v1
	bAv1 := makeBlob(t, s, "Av1")
	c1 := makeCommit(t, s, store.ZeroHash, store.ZeroHash, []string{"api"}, "A", bAv1, t0, "init")

	// c2: change api A to v2, add www CNAME, remove nothing
	bAv2 := makeBlob(t, s, "Av2")
	tree2a, err := object.UpdateTree(ctx(), s, commitTree(t, s, c1), []string{"api"}, "A", bAv2)
	if err != nil {
		t.Fatal(err)
	}
	bCNAME := makeBlob(t, s, "cname-blob")
	tree2b, err := object.UpdateTree(ctx(), s, tree2a, []string{"www"}, "CNAME", bCNAME)
	if err != nil {
		t.Fatal(err)
	}

	changes, err := history.Diff(ctx(), s, commitTree(t, s, c1), tree2b)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(changes), changes)
	}
	// Sorted by FQDN: api < www.
	if changes[0].FQDN() != "api" || changes[0].Op != history.OpModified {
		t.Errorf("changes[0] = %+v", changes[0])
	}
	if changes[1].FQDN() != "www" || changes[1].Op != history.OpAdded {
		t.Errorf("changes[1] = %+v", changes[1])
	}
}

func TestDiff_RemovedSubtree(t *testing.T) {
	s := memstore.New()
	bA := makeBlob(t, s, "A")
	bB := makeBlob(t, s, "B")
	old, err := object.UpdateTree(ctx(), s, store.ZeroHash, []string{"foo"}, "A", bA)
	if err != nil {
		t.Fatal(err)
	}
	old, err = object.UpdateTree(ctx(), s, old, []string{"foo"}, "AAAA", bB)
	if err != nil {
		t.Fatal(err)
	}
	// Removing both leaves should prune the "foo" subtree entirely.
	new1, err := object.UpdateTree(ctx(), s, old, []string{"foo"}, "A", store.ZeroHash)
	if err != nil {
		t.Fatal(err)
	}
	new1, err = object.UpdateTree(ctx(), s, new1, []string{"foo"}, "AAAA", store.ZeroHash)
	if err != nil {
		t.Fatal(err)
	}

	changes, err := history.Diff(ctx(), s, old, new1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(changes), changes)
	}
	for _, c := range changes {
		if c.Op != history.OpRemoved {
			t.Errorf("op = %s, want removed: %+v", c.Op, c)
		}
		if c.FQDN() != "foo" {
			t.Errorf("fqdn = %s", c.FQDN())
		}
	}
}

func TestDiff_IdenticalTreesNoLoad(t *testing.T) {
	s := memstore.New()
	bA := makeBlob(t, s, "A")
	tree, err := object.UpdateTree(ctx(), s, store.ZeroHash, []string{"foo"}, "A", bA)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := history.Diff(ctx(), s, tree, tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("identical trees produced %d changes", len(changes))
	}
}

func TestBlame(t *testing.T) {
	s := memstore.New()
	t0 := time.Unix(1000000, 0)

	b1 := makeBlob(t, s, "v1")
	c1 := makeCommit(t, s, store.ZeroHash, store.ZeroHash, []string{"api"}, "A", b1, t0, "intro")

	// c2 changes a different RRset — api A unchanged
	bUnrelated := makeBlob(t, s, "ns1")
	c2 := makeCommit(t, s, c1, commitTree(t, s, c1), []string{"@"}, "NS", bUnrelated, t0.Add(time.Hour), "add NS")
	_ = c2

	// c3 changes api A to v2
	b2 := makeBlob(t, s, "v2")
	c3 := makeCommit(t, s, c2, commitTree(t, s, c2), []string{"api"}, "A", b2, t0.Add(2*time.Hour), "bump api")

	// Blame at HEAD=c3 for api A: should be c3 (introduced v2).
	info, err := history.Blame(ctx(), s, c3, []string{"api"}, "A")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Found {
		t.Fatal("not found")
	}
	if info.Commit != c3 {
		t.Fatalf("commit = %s, want c3 %s", info.Commit.Short(), c3.Short())
	}
	if info.Message != "bump api" {
		t.Errorf("message = %q", info.Message)
	}

	// Blame at HEAD=c2 for api A: still v1 — should be c1.
	info, err = history.Blame(ctx(), s, c2, []string{"api"}, "A")
	if err != nil {
		t.Fatal(err)
	}
	if info.Commit != c1 {
		t.Fatalf("commit = %s, want c1 %s", info.Commit.Short(), c1.Short())
	}

	// Blame for nonexistent RRset.
	info, err = history.Blame(ctx(), s, c3, []string{"nope"}, "A")
	if err != nil {
		t.Fatal(err)
	}
	if info.Found {
		t.Fatal("should not be found")
	}
}
