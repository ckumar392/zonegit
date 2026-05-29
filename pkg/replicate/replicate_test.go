package replicate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/replicate"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

func ctx() context.Context { return context.Background() }

// TestEndToEndPull seeds a primary repo with one commit, spins up a
// Server in front of it, pulls into an empty secondary, and asserts
// the secondary now resolves the same record.
func TestEndToEndPull(t *testing.T) {
	// Primary: init, import a record, commit.
	primary, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	if err := primary.Init(ctx(), "foo.com."); err != nil {
		t.Fatal(err)
	}
	rr, _ := dns.NewRR("api.foo.com. 300 IN A 1.2.3.4")
	if err := primary.Set(ctx(), []dns.RR{rr}); err != nil {
		t.Fatal(err)
	}
	primaryHead, err := primary.Commit(ctx(), object.Identity{Name: "t", Email: "t@t"}, "primary init")
	if err != nil {
		t.Fatal(err)
	}

	// Spin up the replication server.
	srv := &replicate.Server{
		SnapshotFn: func() (*repo.Repo, error) { return primary, nil },
	}
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Empty secondary.
	secondary, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer secondary.Close()

	client := replicate.NewClient(ts.URL, secondary)
	if err := client.Pull(ctx()); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// After pull: secondary should have foo.com. registered with a
	// main branch pointing at the same commit.
	zones, _ := secondary.Zones(ctx())
	if len(zones) != 1 || zones[0] != "foo.com." {
		t.Fatalf("secondary zones = %v, want [foo.com.]", zones)
	}
	secondaryHead, err := secondary.Refs().GetBranch(ctx(), "foo.com.", "main")
	if err != nil {
		t.Fatalf("secondary missing main branch: %v", err)
	}
	if secondaryHead != primaryHead {
		t.Fatalf("secondary head = %s, want %s", secondaryHead.Short(), primaryHead.Short())
	}

	// Cross-check by resolving the actual record.
	rs, err := secondary.Lookup(ctx(), secondaryHead, "api", "A")
	if err != nil {
		t.Fatalf("secondary lookup api A: %v", err)
	}
	if len(rs.RRs) != 1 {
		t.Fatalf("expected 1 RR, got %d", len(rs.RRs))
	}
	if a, ok := rs.RRs[0].(*dns.A); !ok || a.A.String() != "1.2.3.4" {
		t.Fatalf("RR = %v, want api A 1.2.3.4", rs.RRs[0])
	}
}

// TestIncrementalPull simulates the common case: secondary is already
// in sync, then primary gets a new commit. Secondary should only
// fetch the delta — not re-fetch the full history.
func TestIncrementalPull(t *testing.T) {
	primary, _ := repo.Open(repo.Options{Memory: true})
	defer primary.Close()
	_ = primary.Init(ctx(), "foo.com.")
	rr1, _ := dns.NewRR("api.foo.com. 300 IN A 1.2.3.4")
	_ = primary.Set(ctx(), []dns.RR{rr1})
	_, _ = primary.Commit(ctx(), object.Identity{Name: "t", Email: "t@t"}, "c1")

	srv := &replicate.Server{SnapshotFn: func() (*repo.Repo, error) { return primary, nil }}
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	secondary, _ := repo.Open(repo.Options{Memory: true})
	defer secondary.Close()
	client := replicate.NewClient(ts.URL, secondary)

	// First pull: catches up fully.
	if err := client.Pull(ctx()); err != nil {
		t.Fatal(err)
	}
	objsBefore := countObjects(t, secondary)

	// Primary makes a second commit.
	rr2, _ := dns.NewRR("api.foo.com. 300 IN A 5.6.7.8")
	_ = primary.Set(ctx(), []dns.RR{rr2})
	primaryHead2, _ := primary.Commit(ctx(), object.Identity{Name: "t", Email: "t@t"}, "c2")

	// Second pull: should fetch only the delta.
	if err := client.Pull(ctx()); err != nil {
		t.Fatal(err)
	}
	objsAfter := countObjects(t, secondary)

	// Delta should be small — new blob (api A 5.6.7.8), new tree spine
	// from leaf up to root (one tree per label level), new commit,
	// plus the auto-bumped SOA blob. ~5 objects, not the whole
	// 8-ish-object history.
	delta := objsAfter - objsBefore
	if delta == 0 {
		t.Fatal("no new objects after second pull")
	}
	if delta > 7 {
		t.Errorf("incremental pull fetched %d new objects; expected ≤7 (full history was %d)", delta, objsBefore)
	}

	// And the secondary's head matches the primary's new head.
	got, _ := secondary.Refs().GetBranch(ctx(), "foo.com.", "main")
	if got != primaryHead2 {
		t.Errorf("secondary head after second pull = %s, want %s", got.Short(), primaryHead2.Short())
	}
}

// countObjects walks the in-memory store and returns the number of
// distinct content-addressed objects present.
func countObjects(t *testing.T, r *repo.Repo) int {
	t.Helper()
	n := 0
	if err := r.Storage().IterObjects(ctx(), func(_ store.Hash, _ store.Object) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("IterObjects: %v", err)
	}
	return n
}
