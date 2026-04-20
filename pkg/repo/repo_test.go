package repo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ckumar392/dnsdb/pkg/object"
	"github.com/ckumar392/dnsdb/pkg/repo"
	"github.com/miekg/dns"
)

func ctx() context.Context { return context.Background() }

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("NewRR(%q): %v", s, err)
	}
	return rr
}

func newMemRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Init(ctx(), "foo.com."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestInitAndCommit(t *testing.T) {
	r := newMemRepo(t)
	rrs := []dns.RR{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")}
	if err := r.Set(ctx(), rrs); err != nil {
		t.Fatal(err)
	}
	if r.StagedCount() != 1 {
		t.Fatalf("staged = %d", r.StagedCount())
	}
	c1, err := r.Commit(ctx(), object.Identity{Name: "alice", Email: "a@a"}, "init")
	if err != nil {
		t.Fatal(err)
	}
	if c1.IsZero() {
		t.Fatal("zero commit hash")
	}
	if r.StagedCount() != 0 {
		t.Fatal("staging not cleared")
	}

	// Lookup should return what we set.
	rs, err := r.Lookup(ctx(), c1, "api", "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.RRs) != 1 {
		t.Fatalf("len = %d", len(rs.RRs))
	}
	if rs.TTL != 300 {
		t.Errorf("ttl = %d", rs.TTL)
	}
	if a, ok := rs.RRs[0].(*dns.A); !ok || !a.A.Equal([]byte{1, 2, 3, 4}) {
		t.Errorf("rdata = %v", rs.RRs[0])
	}
}

func TestCommitNoChangesError(t *testing.T) {
	r := newMemRepo(t)
	_, err := r.Commit(ctx(), object.Identity{Name: "x"}, "empty")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestImportThenCommit(t *testing.T) {
	r := newMemRepo(t)
	zonefile := `$ORIGIN foo.com.
$TTL 300
api  IN  A  1.2.3.4
api  IN  A  5.6.7.8
www  IN  CNAME api
`
	n, err := r.Import(ctx(), strings.NewReader(zonefile))
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("imported %d rrsets", n)
	}
	c, err := r.Commit(ctx(), object.Identity{Name: "alice", Email: "a@a"}, "import")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := r.Lookup(ctx(), c, "api", "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.RRs) != 2 {
		t.Fatalf("api A: len = %d, want 2", len(rs.RRs))
	}
}

func TestLogDiffBlame(t *testing.T) {
	r := newMemRepo(t)

	// c1: api A = 1.2.3.4
	_ = r.Set(ctx(), []dns.RR{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")})
	c1, err := r.Commit(ctx(), object.Identity{Name: "alice", Email: "a@a"}, "init api")
	if err != nil {
		t.Fatal(err)
	}

	// c2: change api A
	_ = r.Set(ctx(), []dns.RR{mustRR(t, "api.foo.com. 300 IN A 9.9.9.9")})
	c2, err := r.Commit(ctx(), object.Identity{Name: "bob", Email: "b@b"}, "bump api")
	if err != nil {
		t.Fatal(err)
	}

	// Log returns 2 commits, newest first.
	entries, err := r.Log(ctx(), "HEAD", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("log len = %d", len(entries))
	}
	if entries[0].Hash != c2 || entries[1].Hash != c1 {
		t.Errorf("log order wrong")
	}

	// Diff between HEAD~1 and HEAD: one Modified change.
	changes, err := r.Diff(ctx(), "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("diff len = %d: %+v", len(changes), changes)
	}
	if changes[0].FQDN() != "api" || changes[0].RRType != "A" {
		t.Errorf("diff = %+v", changes[0])
	}

	// Blame: should attribute api A to bob (the bump commit).
	bi, err := r.Blame(ctx(), "api", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !bi.Found {
		t.Fatal("not found")
	}
	if bi.Author.Name != "bob" {
		t.Errorf("author = %s", bi.Author.Name)
	}
	if bi.Message != "bump api" {
		t.Errorf("msg = %s", bi.Message)
	}
}

func TestDelete(t *testing.T) {
	r := newMemRepo(t)
	_ = r.Set(ctx(), []dns.RR{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")})
	if _, err := r.Commit(ctx(), object.Identity{Name: "alice", Email: "a@a"}, "init"); err != nil {
		t.Fatal(err)
	}

	r.Delete("api", "A")
	c2, err := r.Commit(ctx(), object.Identity{Name: "alice", Email: "a@a"}, "delete api")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Lookup(ctx(), c2, "api", "A")
	if err == nil {
		t.Fatal("expected ErrNotFound after delete")
	}
}

func TestApexAndDeepNames(t *testing.T) {
	r := newMemRepo(t)
	_ = r.Set(ctx(), []dns.RR{mustRR(t, "foo.com. 300 IN NS ns1.foo.com.")})
	_ = r.Set(ctx(), []dns.RR{mustRR(t, "a.b.c.foo.com. 300 IN A 1.2.3.4")})
	c, err := r.Commit(ctx(), object.Identity{Name: "alice", Email: "a@a"}, "deep")
	if err != nil {
		t.Fatal(err)
	}

	// Apex.
	rs, err := r.Lookup(ctx(), c, "@", "NS")
	if err != nil {
		t.Fatalf("apex NS: %v", err)
	}
	if len(rs.RRs) != 1 {
		t.Errorf("apex NS len = %d", len(rs.RRs))
	}

	// Deep name.
	rs, err = r.Lookup(ctx(), c, "a.b.c", "A")
	if err != nil {
		t.Fatalf("a.b.c A: %v", err)
	}
	if len(rs.RRs) != 1 {
		t.Errorf("a.b.c A len = %d", len(rs.RRs))
	}
}
