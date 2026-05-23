package refs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/store/memstore"
)

const testZone = "foo.com."

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

	if err := db.CreateBranch(ctx(), testZone, "main", h1); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetBranch(ctx(), testZone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != h1 {
		t.Fatalf("got %s, want %s", got.Short(), h1.Short())
	}

	names, err := db.ListBranches(ctx(), testZone)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "main" {
		t.Fatalf("branches = %v", names)
	}

	h2 := seedCommit(t, s, h1)
	if err := db.UpdateBranch(ctx(), testZone, "main", h1, h2); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetBranch(ctx(), testZone, "main")
	if got != h2 {
		t.Fatalf("after update got %s, want %s", got.Short(), h2.Short())
	}

	if err := db.DeleteBranch(ctx(), testZone, "main", h2); err != nil {
		t.Fatal(err)
	}
	_, err = db.GetBranch(ctx(), testZone, "main")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestTagCRUD(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	h := seedCommit(t, s)

	if err := db.CreateTag(ctx(), testZone, "v1.0", h); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTag(ctx(), testZone, "v1.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("got %s, want %s", got.Short(), h.Short())
	}
	names, _ := db.ListTags(ctx(), testZone)
	if len(names) != 1 || names[0] != "v1.0" {
		t.Fatalf("tags = %v", names)
	}
	if err := db.DeleteTag(ctx(), testZone, "v1.0", h); err != nil {
		t.Fatal(err)
	}
	_, err = db.GetTag(ctx(), testZone, "v1.0")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHEAD(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	h := seedCommit(t, s)

	if err := db.SetHEAD(ctx(), testZone, "main"); err != nil {
		t.Fatal(err)
	}
	zone, branch, commit, err := db.ReadHEAD(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if zone != testZone || branch != "main" {
		t.Fatalf("HEAD parts = (%q, %q)", zone, branch)
	}
	if !commit.IsZero() {
		t.Fatal("expected zero commit for orphan branch")
	}

	if err := db.CreateBranch(ctx(), testZone, "main", h); err != nil {
		t.Fatal(err)
	}
	zone, branch, commit, err = db.ReadHEAD(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if zone != testZone || branch != "main" || commit != h {
		t.Fatalf("HEAD = (%q, %q, %s)", zone, branch, commit.Short())
	}

	// Switch HEAD to another branch in the same zone.
	h2 := seedCommit(t, s, h)
	if err := db.CreateBranch(ctx(), testZone, "dev", h2); err != nil {
		t.Fatal(err)
	}
	if err := db.SetHEAD(ctx(), testZone, "dev"); err != nil {
		t.Fatal(err)
	}
	zone, branch, commit, _ = db.ReadHEAD(ctx())
	if zone != testZone || branch != "dev" || commit != h2 {
		t.Fatalf("HEAD after switch = (%q, %q, %s)", zone, branch, commit.Short())
	}
}

func TestResolve(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)

	c0 := seedCommit(t, s)
	c1 := seedCommit(t, s, c0)
	c2 := seedCommit(t, s, c1)

	_ = db.CreateBranch(ctx(), testZone, "main", c2)
	_ = db.CreateTag(ctx(), testZone, "v1", c0)
	_ = db.SetHEAD(ctx(), testZone, "main")

	tests := []struct {
		refish string
		want   store.Hash
	}{
		{c2.String(), c2},                   // full hex
		{"main", c2},                        // bare branch — uses active zone
		{"v1", c0},                          // bare tag — uses active zone
		{"foo.com./main", c2},               // zone-qualified branch
		{"foo.com./v1", c0},                 // zone-qualified tag
		{"HEAD", c2},                        // HEAD
		{"main~0", c2},                      // ~0 = self
		{"main~1", c1},                      // parent
		{"main~2", c0},                      // grandparent
		{"HEAD~1", c1},                      // HEAD ancestor
		{"refs/heads/foo.com./main", c2},    // full ref path
		{"refs/tags/foo.com./v1", c0},       // full tag path
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

	_, err := db.Resolve(ctx(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent")
	}

	c0 := seedCommit(t, s)
	_ = db.CreateBranch(ctx(), testZone, "main", c0)
	_ = db.SetHEAD(ctx(), testZone, "main")
	_, err = db.Resolve(ctx(), "main~1")
	if err == nil {
		t.Fatal("expected error for ancestor past root")
	}
}

func TestReflog(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)

	ref := refs.BranchRef(testZone, "main")
	if err := db.AppendReflog(ctx(), ref,
		store.ZeroHash, seedCommit(t, s), "me <me@me>", "commit", "initial"); err != nil {
		t.Fatal(err)
	}
	entries, err := db.ReadReflog(ctx(), ref)
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
	_ = db.CreateBranch(ctx(), testZone, "main", c1)
	_ = db.SetHEAD(ctx(), testZone, "main")

	got, err := db.Resolve(ctx(), "main~")
	if err != nil {
		t.Fatal(err)
	}
	if got != c0 {
		t.Fatalf("main~ = %s, want %s", got.Short(), c0.Short())
	}
}

func TestZoneRegistration(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)

	if err := db.RegisterZone(ctx(), "foo.com."); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterZone(ctx(), "bar.com."); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := db.RegisterZone(ctx(), "foo.com."); err != nil {
		t.Fatal(err)
	}

	zones, err := db.ListZones(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 2 || zones[0] != "bar.com." || zones[1] != "foo.com." {
		t.Fatalf("zones = %v, want [bar.com. foo.com.]", zones)
	}

	ok, _ := db.IsZoneRegistered(ctx(), "foo.com.")
	if !ok {
		t.Fatal("foo.com. should be registered")
	}
	ok, _ = db.IsZoneRegistered(ctx(), "missing.com.")
	if ok {
		t.Fatal("missing.com. should not be registered")
	}
}

func TestMultiZoneBranchIsolation(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	h1 := seedCommit(t, s)
	h2 := seedCommit(t, s, h1)

	// Two zones, each with a "main" branch pointing at different commits.
	if err := db.CreateBranch(ctx(), "foo.com.", "main", h1); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBranch(ctx(), "bar.com.", "main", h2); err != nil {
		t.Fatal(err)
	}

	got1, _ := db.GetBranch(ctx(), "foo.com.", "main")
	got2, _ := db.GetBranch(ctx(), "bar.com.", "main")
	if got1 != h1 || got2 != h2 {
		t.Fatalf("isolation violated: foo=%s bar=%s", got1.Short(), got2.Short())
	}
}
