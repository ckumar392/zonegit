package merge

import (
	"context"
	"testing"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/store/memstore"
	"github.com/ckumar392/zonegit/pkg/zone"
)

func putRRset(t *testing.T, ctx context.Context, s store.Storage, name string, rrtype uint16, rdata ...string) store.Hash {
	t.Helper()
	rrs := make([]dns.RR, 0, len(rdata))
	for _, rd := range rdata {
		rr, err := dns.NewRR(name + " 300 IN " + dns.TypeToString[rrtype] + " " + rd)
		if err != nil {
			t.Fatalf("parse RR: %v", err)
		}
		rrs = append(rrs, rr)
	}
	payload, err := zone.EncodeRRset(rrs)
	if err != nil {
		t.Fatalf("encode rrset: %v", err)
	}
	b := object.Blob{Payload: payload}
	h, obj := b.Encode()
	if err := s.PutObject(ctx, h, obj); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	return h
}

// buildTree constructs a tree from a flat key map. Keys are "fqdn|RRTYPE",
// where fqdn is dot-joined labels relative to the apex ("" for the apex).
func buildTree(t *testing.T, ctx context.Context, s store.Storage, leaves map[string]store.Hash) store.Hash {
	t.Helper()
	root := store.ZeroHash
	for k, v := range leaves {
		var fqdn, rrtype string
		for i := 0; i < len(k); i++ {
			if k[i] == '|' {
				fqdn = k[:i]
				rrtype = k[i+1:]
				break
			}
		}
		path := splitFQDN(fqdn)
		var err error
		root, err = object.UpdateTree(ctx, s, root, path, rrtype, v)
		if err != nil {
			t.Fatalf("update tree: %v", err)
		}
	}
	return root
}

func splitFQDN(fqdn string) []string {
	if fqdn == "" {
		return nil
	}
	parts := []string{}
	start := 0
	for i := 0; i <= len(fqdn); i++ {
		if i == len(fqdn) || fqdn[i] == '.' {
			parts = append(parts, fqdn[start:i])
			start = i + 1
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

func TestMerge_FastIdentical(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	a := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")
	root := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a})

	merged, conflicts, err := MergeTrees(ctx, s, root, root, root)
	if err != nil {
		t.Fatal(err)
	}
	if merged != root {
		t.Errorf("identical merge should return same hash; got %s want %s", merged.Short(), root.Short())
	}
	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", conflicts)
	}
}

func TestMerge_OneSidedChange(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	a1 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")
	a2 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "9.9.9.9")

	base := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a1})
	ours := base
	theirs := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a2})

	merged, conflicts, err := MergeTrees(ctx, s, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if merged != theirs {
		t.Errorf("merge of unchanged-ours should equal theirs; got %s want %s", merged.Short(), theirs.Short())
	}
}

func TestMerge_DisjointPaths(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()

	a := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")
	w := putRRset(t, ctx, s, "www.foo.com.", dns.TypeA, "5.6.7.8")
	a2 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "9.9.9.9")
	w2 := putRRset(t, ctx, s, "www.foo.com.", dns.TypeA, "8.8.8.8")

	base := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a, "www|A": w})
	ours := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a2, "www|A": w})
	theirs := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a, "www|A": w2})

	merged, conflicts, err := MergeTrees(ctx, s, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	apiH, err := object.WalkTree(ctx, s, merged, []string{"api"}, "A")
	if err != nil {
		t.Fatal(err)
	}
	if apiH != a2 {
		t.Errorf("api should be ours' value")
	}
	wwwH, err := object.WalkTree(ctx, s, merged, []string{"www"}, "A")
	if err != nil {
		t.Fatal(err)
	}
	if wwwH != w2 {
		t.Errorf("www should be theirs' value")
	}
}

func TestMerge_ConflictBothModified(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	a := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")
	a2 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "9.9.9.9")
	a3 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "7.7.7.7")

	base := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a})
	ours := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a2})
	theirs := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a3})

	_, conflicts, err := MergeTrees(ctx, s, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.Reason != ReasonBothModified {
		t.Errorf("reason: got %v want both-modified", c.Reason)
	}
	if c.FQDN() != "api" || c.RRType != "A" {
		t.Errorf("conflict path: got %s %s", c.FQDN(), c.RRType)
	}
}

func TestMerge_ConflictDeletedModified(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	a := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")
	a2 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "9.9.9.9")

	base := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a})
	ours := buildTree(t, ctx, s, map[string]store.Hash{})
	theirs := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a2})

	_, conflicts, err := MergeTrees(ctx, s, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Reason != ReasonDeletedModified {
		t.Fatalf("expected 1 deleted-modified conflict, got %v", conflicts)
	}
}

func TestMerge_AddAddIdentical(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	a := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")

	base := store.ZeroHash
	ours := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a})
	theirs := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a})

	merged, conflicts, err := MergeTrees(ctx, s, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("identical add-add should not conflict: %v", conflicts)
	}
	if merged != ours {
		t.Errorf("merged should equal ours")
	}
}

func TestMerge_AddAddDifferent(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	a1 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "1.2.3.4")
	a2 := putRRset(t, ctx, s, "api.foo.com.", dns.TypeA, "9.9.9.9")

	base := store.ZeroHash
	ours := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a1})
	theirs := buildTree(t, ctx, s, map[string]store.Hash{"api|A": a2})

	_, conflicts, err := MergeTrees(ctx, s, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Reason != ReasonAddAdd {
		t.Fatalf("expected 1 add-add conflict, got %v", conflicts)
	}
}
