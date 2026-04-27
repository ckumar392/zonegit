package repo_test

import (
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/repo"
)

var alice = object.Identity{Name: "alice", Email: "a@a"}

func commitSet(t *testing.T, r *repo.Repo, name, rdata, msg string) {
	t.Helper()
	if err := r.Set(ctx(), []dns.RR{mustRR(t, name+" 300 IN A "+rdata)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := r.Commit(ctx(), alice, msg); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestMerge_FastForward(t *testing.T) {
	r := newMemRepo(t)
	commitSet(t, r, "api.foo.com.", "1.2.3.4", "c1")

	_, c1, _ := r.Head(ctx())
	if err := r.Refs().CreateBranch(ctx(), "dev", c1); err != nil {
		t.Fatal(err)
	}
	if err := r.Refs().SetHEAD(ctx(), refs.BranchPrefix+"dev"); err != nil {
		t.Fatal(err)
	}
	commitSet(t, r, "www.foo.com.", "5.6.7.8", "c2")

	if err := r.Refs().SetHEAD(ctx(), refs.BranchPrefix+"main"); err != nil {
		t.Fatal(err)
	}
	res, err := r.Merge(ctx(), "dev", alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.FastForward {
		t.Fatalf("expected fast-forward, got %+v", res)
	}
	_, head, _ := r.Head(ctx())
	devTip, _ := r.Refs().GetBranch(ctx(), "dev")
	if head != devTip {
		t.Errorf("main not advanced to dev tip")
	}
}

func TestMerge_AlreadyUpToDate(t *testing.T) {
	r := newMemRepo(t)
	commitSet(t, r, "api.foo.com.", "1.2.3.4", "c1")
	_, c1, _ := r.Head(ctx())
	if err := r.Refs().CreateBranch(ctx(), "dev", c1); err != nil {
		t.Fatal(err)
	}
	commitSet(t, r, "www.foo.com.", "5.6.7.8", "c2")

	res, err := r.Merge(ctx(), "dev", alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyUpToDate {
		t.Errorf("expected already-up-to-date, got %+v", res)
	}
}

func TestMerge_ThreeWayClean(t *testing.T) {
	r := newMemRepo(t)
	commitSet(t, r, "api.foo.com.", "1.2.3.4", "base")
	_, base, _ := r.Head(ctx())

	if err := r.Refs().CreateBranch(ctx(), "dev", base); err != nil {
		t.Fatal(err)
	}
	if err := r.Refs().SetHEAD(ctx(), refs.BranchPrefix+"dev"); err != nil {
		t.Fatal(err)
	}
	commitSet(t, r, "www.foo.com.", "5.6.7.8", "dev: add www")

	if err := r.Refs().SetHEAD(ctx(), refs.BranchPrefix+"main"); err != nil {
		t.Fatal(err)
	}
	commitSet(t, r, "mail.foo.com.", "9.9.9.9", "main: add mail")

	res, err := r.Merge(ctx(), "dev", alice, "merge dev")
	if err != nil {
		t.Fatal(err)
	}
	if res.FastForward || res.AlreadyUpToDate || len(res.Conflicts) != 0 {
		t.Fatalf("expected clean 3-way merge, got %+v", res)
	}
	if res.Commit.IsZero() {
		t.Fatal("expected non-zero merge commit")
	}
	_, head, _ := r.Head(ctx())
	if _, err := r.Lookup(ctx(), head, "www", "A"); err != nil {
		t.Errorf("www should exist: %v", err)
	}
	if _, err := r.Lookup(ctx(), head, "mail", "A"); err != nil {
		t.Errorf("mail should exist: %v", err)
	}
}

func TestMerge_Conflict(t *testing.T) {
	r := newMemRepo(t)
	commitSet(t, r, "api.foo.com.", "1.2.3.4", "base")
	_, base, _ := r.Head(ctx())

	if err := r.Refs().CreateBranch(ctx(), "dev", base); err != nil {
		t.Fatal(err)
	}
	if err := r.Refs().SetHEAD(ctx(), refs.BranchPrefix+"dev"); err != nil {
		t.Fatal(err)
	}
	commitSet(t, r, "api.foo.com.", "9.9.9.9", "dev: change api")

	if err := r.Refs().SetHEAD(ctx(), refs.BranchPrefix+"main"); err != nil {
		t.Fatal(err)
	}
	commitSet(t, r, "api.foo.com.", "7.7.7.7", "main: change api")
	_, mainTip, _ := r.Head(ctx())

	res, err := r.Merge(ctx(), "dev", alice, "merge dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) == 0 {
		t.Fatalf("expected conflicts, got %+v", res)
	}
	_, head2, _ := r.Head(ctx())
	if head2 != mainTip {
		t.Errorf("main advanced despite conflicts")
	}
}

func TestRevert(t *testing.T) {
	r := newMemRepo(t)
	commitSet(t, r, "api.foo.com.", "1.2.3.4", "v1")
	commitSet(t, r, "api.foo.com.", "9.9.9.9", "v2")

	_, head, _ := r.Head(ctx())
	rev, err := r.Revert(ctx(), "HEAD", alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if rev.IsZero() {
		t.Fatal("zero hash")
	}
	_, newHead, _ := r.Head(ctx())
	if newHead == head {
		t.Fatal("HEAD did not advance")
	}
	rs, err := r.Lookup(ctx(), newHead, "api", "A")
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := rs.RRs[0].(*dns.A); !ok || a.A.String() != "1.2.3.4" {
		t.Errorf("after revert, api A = %v, want 1.2.3.4", rs.RRs[0])
	}
}

func TestResetHard(t *testing.T) {
	r := newMemRepo(t)
	commitSet(t, r, "api.foo.com.", "1.2.3.4", "v1")
	_, c1, _ := r.Head(ctx())
	commitSet(t, r, "api.foo.com.", "9.9.9.9", "v2")

	target, err := r.ResetHard(ctx(), c1.String(), alice)
	if err != nil {
		t.Fatal(err)
	}
	if target != c1 {
		t.Errorf("target = %s, want %s", target, c1)
	}
	_, head, _ := r.Head(ctx())
	if head != c1 {
		t.Errorf("HEAD = %s, want %s", head, c1)
	}
	rl, err := r.Refs().ReadReflog(ctx(), refs.BranchPrefix+"main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range rl {
		if e.Op == "reset" && strings.Contains(e.Message, "reset --hard") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("reset reflog entry not found; entries=%v", rl)
	}
}
