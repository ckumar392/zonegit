package refs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ckumar392/dnsdb/pkg/object"
	"github.com/ckumar392/dnsdb/pkg/refs"
	"github.com/ckumar392/dnsdb/pkg/store"
	"github.com/ckumar392/dnsdb/pkg/store/memstore"
)

func ctx() context.Context { return context.Background() }

func seedCommit(t *testing.T, s store.Storage, parents ...store.Hash) store.Hash {
	t.Helper()
	c := object.Commit{
		Tree:       store.ZeroHash,
		Parents:    parents,
		Author:     object.Identity{Name: "test", Email: "t@t"},
		Committer:  object.Identity{Name: "test", Email: "t@t"},
		AuthorTime: time.Unix(1000000, 0),
		CommitTime: time.Unix(1000000, 0),
		Message:    "seed",
	}
	h, obj := c.Encode()
	if err := s.PutObject(ctx(), h, obj); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestBranchCRUD(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	h1 := seedCommit(t, s)

	// Create.
	if err := db.CreateBranch(ctx(), "main", h1); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBranch(ctx(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != h1 {
		t.Fatalf("got %s, want %s", got.Short(), h1.Short())
	}

	// List.
	names, err := db.ListBranches(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "main" {
		t.Fatalf("branches = %v", names)
	}

	// Update.
	h2 := seedCommit(t, s, h1)
	if err := db.UpdateBranch(ctx(), "main", h1, h2); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetBranch(ctx(), "main")
	if got != h2 {
		t.Fatalf("after update got %s, want %s", got.Short(), h2.Short())
	}

	// Delete.
	if err := db.DeleteBranch(ctx(), "main", h2); err != nil {
		t.Fatal(err)
	}
	_, err = db.GetBranch(ctx(), "main")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestTagCRUD(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	h := seedCommit(t, s)

	if err := db.CreateTag(ctx(), "v1.0", h); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTag(ctx(), "v1.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("got %s, want %s", got.Short(), h.Short())
	}
	names, _ := db.ListTags(ctx())
	if len(names) != 1 || names[0] != "v1.0" {
		t.Fatalf("tags = %v", names)
	}
	if err := db.DeleteTag(ctx(), "v1.0", h); err != nil {
		t.Fatal(err)
	}
	_, err = db.GetTag(ctx(), "v1.0")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHEAD(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	h := seedCommit(t, s)

	// Set HEAD to main (orphan — branch doesn't exist yet).
	if err := db.SetHEAD(ctx(), "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	branch, commit, err := db.ReadHEAD(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if branch != "refs/heads/main" {
		t.Fatalf("branch = %q", branch)
	}
	if !commit.IsZero() {
		t.Fatal("expected zero commit for orphan branch")
	}

	// Create the branch, then re-read HEAD.
	if err := db.CreateBranch(ctx(), "main", h); err != nil {
		t.Fatal(err)
	}
	branch, commit, err = db.ReadHEAD(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if branch != "refs/heads/main" || commit != h {
		t.Fatalf("HEAD = (%s, %s)", branch, commit.Short())
	}

	// Switch HEAD.
	h2 := seedCommit(t, s, h)
	if err := db.CreateBranch(ctx(), "dev", h2); err != nil {
		t.Fatal(err)
	}
	if err := db.SetHEAD(ctx(), "refs/heads/dev"); err != nil {
		t.Fatal(err)
	}
	branch, commit, _ = db.ReadHEAD(ctx())
	if branch != "refs/heads/dev" || commit != h2 {
		t.Fatalf("HEAD after switch = (%s, %s)", branch, commit.Short())
	}
}

func TestResolve(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)

	// Build a 3-commit chain: c0 <- c1 <- c2.
	c0 := seedCommit(t, s)
	c1 := seedCommit(t, s, c0)
	c2 := seedCommit(t, s, c1)

	_ = db.CreateBranch(ctx(), "main", c2)
	_ = db.CreateTag(ctx(), "v1", c0)
	_ = db.SetHEAD(ctx(), "refs/heads/main")

	tests := []struct {
		refish string
		want   store.Hash
	}{
		{c2.String(), c2},         // full hex
		{"main", c2},              // branch name
		{"v1", c0},                // tag name
		{"HEAD", c2},              // HEAD
		{"main~0", c2},            // ~0 = self
		{"main~1", c1},            // parent
		{"main~2", c0},            // grandparent
		{"HEAD~1", c1},            // HEAD ancestor
		{"refs/heads/main", c2},   // raw ref
		{"refs/tags/v1", c0},      // raw tag ref
	}
	for _, tt := range tests {
		t.Run(tt.refish, func(t *testing.T) {
			got, err := db.Resolve(ctx(), tt.refish)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.refish, err)
			}
			if got != tt.want {
				t.Fatalf("Resolve(%q) = %s, want %s", tt.refish, got.Short(), tt.want.Short())
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)

	// No refs at all.
	_, err := db.Resolve(ctx(), "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	// Ancestor past root.
	c0 := seedCommit(t, s)
	_ = db.CreateBranch(ctx(), "main", c0)
	_, err = db.Resolve(ctx(), "main~1")
	if err == nil {
		t.Fatal("expected error for ancestor past root")
	}
}

func TestReflog(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)

	if err := db.AppendReflog(ctx(), "refs/heads/main",
		store.ZeroHash, seedCommit(t, s), "me <me@me>", "commit", "initial"); err != nil {
		t.Fatal(err)
	}
	entries, err := db.ReadReflog(ctx(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d", len(entries))
	}
	if entries[0].Op != "commit" || entries[0].Message != "initial" {
		t.Errorf("entry = %+v", entries[0])
	}
}

func TestSplitAncestorEdgeCases(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	c0 := seedCommit(t, s)
	c1 := seedCommit(t, s, c0)
	_ = db.CreateBranch(ctx(), "main", c1)

	// "main~" with no number = ~1
	got, err := db.Resolve(ctx(), "main~")
	if err != nil {
		t.Fatal(err)
	}
	if got != c0 {
		t.Fatalf("main~ = %s, want %s", got.Short(), c0.Short())
	}
}
