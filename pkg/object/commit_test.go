package object

import (
	"strings"
	"testing"
	"time"

	"github.com/ckumar392/zonegit/pkg/store"
)

func TestCommitRoundTrip(t *testing.T) {
	t.Parallel()
	parent := mkHash(0xaa)
	tree := mkHash(0xbb)
	ts := time.Date(2026, 4, 25, 14, 30, 0, 0, time.FixedZone("PDT", -7*3600))

	c := Commit{
		Tree:       tree,
		Parents:    []store.Hash{parent},
		Author:     Identity{Name: "Alice Acme", Email: "alice@acme.example"},
		Committer:  Identity{Name: "Alice Acme", Email: "alice@acme.example"},
		AuthorTime: ts,
		CommitTime: ts,
		Message:    "promote new api",
	}
	_, obj := c.Encode()
	got, err := DecodeCommit(obj.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tree != c.Tree {
		t.Errorf("tree: got %s want %s", got.Tree.Short(), c.Tree.Short())
	}
	if len(got.Parents) != 1 || got.Parents[0] != parent {
		t.Errorf("parents: %v", got.Parents)
	}
	if got.Author != c.Author || got.Committer != c.Committer {
		t.Errorf("identity round-trip failed: %+v vs %+v", got.Author, c.Author)
	}
	if !got.AuthorTime.Equal(ts) || !got.CommitTime.Equal(ts) {
		t.Errorf("time round-trip: got %s, want %s", got.AuthorTime, ts)
	}
	if got.Message != c.Message {
		t.Errorf("message: got %q want %q", got.Message, c.Message)
	}
}

// Hash determinism for commits.
func TestCommitHashDeterministic(t *testing.T) {
	t.Parallel()
	ts := time.Unix(1700000000, 0).UTC()
	mk := func() Commit {
		return Commit{
			Tree:       mkHash(1),
			Parents:    []store.Hash{mkHash(2)},
			Author:     Identity{Name: "A", Email: "a@x"},
			Committer:  Identity{Name: "A", Email: "a@x"},
			AuthorTime: ts,
			CommitTime: ts,
			Message:    "m",
		}
	}
	h1, _ := mk().Encode()
	h2, _ := mk().Encode()
	if h1 != h2 {
		t.Fatalf("hash drift between identical commits: %s vs %s", h1, h2)
	}
}

// Multi-parent (merge) commits encode and decode their parent list in order.
func TestCommitMultiParent(t *testing.T) {
	t.Parallel()
	ts := time.Unix(0, 0).UTC()
	c := Commit{
		Tree:       mkHash(0),
		Parents:    []store.Hash{mkHash(1), mkHash(2), mkHash(3)},
		Author:     Identity{Name: "M", Email: "m@x"},
		Committer:  Identity{Name: "M", Email: "m@x"},
		AuthorTime: ts,
		CommitTime: ts,
		Message:    "octopus merge",
	}
	_, obj := c.Encode()
	got, err := DecodeCommit(obj.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parents) != 3 {
		t.Fatalf("parent count: got %d want 3", len(got.Parents))
	}
	for i, p := range got.Parents {
		if p != c.Parents[i] {
			t.Errorf("parent[%d]: got %s want %s", i, p.Short(), c.Parents[i].Short())
		}
	}
}

// The selector header round-trips (forward-compat for v2 canary).
func TestCommitSelectorHeader(t *testing.T) {
	t.Parallel()
	ts := time.Unix(0, 0).UTC()
	c := Commit{
		Tree:       mkHash(1),
		Author:     Identity{Name: "A", Email: "a@x"},
		Committer:  Identity{Name: "A", Email: "a@x"},
		AuthorTime: ts,
		CommitTime: ts,
		Selector:   "client.subnet=10.0.0.0/8",
		Message:    "canary",
	}
	_, obj := c.Encode()
	got, err := DecodeCommit(obj.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Selector != c.Selector {
		t.Errorf("selector: got %q want %q", got.Selector, c.Selector)
	}
}

// Decoder ignores unknown headers (forward-compat).
func TestCommitDecodeIgnoresUnknownHeader(t *testing.T) {
	t.Parallel()
	payload := []byte(strings.Join([]string{
		"version 1",
		"tree " + mkHash(1).String(),
		"author A <a@x> 0 +0000",
		"committer A <a@x> 0 +0000",
		"x-future-thing some-value",
		"",
		"hello",
	}, "\n"))
	c, err := DecodeCommit(payload)
	if err != nil {
		t.Fatalf("decode with unknown header: %v", err)
	}
	if c.Message != "hello" {
		t.Errorf("message: got %q want %q", c.Message, "hello")
	}
}

func TestCommitDecodeRejectsMissingHeaders(t *testing.T) {
	t.Parallel()
	payload := []byte("version 1\ntree " + mkHash(0).String() + "\n\nm")
	if _, err := DecodeCommit(payload); err == nil {
		t.Fatal("expected error: missing author/committer")
	}
}

// Note: zonegit has no annotated-tag *object* — lightweight tag refs
// (refs.TagRef + ref resolution) cover the use case. The object kind enum
// keeps KindTag reserved.
