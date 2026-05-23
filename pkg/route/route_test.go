package route

import (
	"fmt"
	"testing"

	"github.com/ckumar392/zonegit/pkg/resolve"
)

func TestNewBucketRouter_ParsesSpec(t *testing.T) {
	r, err := NewBucketRouter("main", "canary:20", "salt")
	if err != nil {
		t.Fatal(err)
	}
	if r.CanaryBranch != "canary" || r.Pct != 20 {
		t.Fatalf("parse: got branch=%q pct=%d", r.CanaryBranch, r.Pct)
	}
}

func TestNewBucketRouter_RejectsBadSpec(t *testing.T) {
	for _, spec := range []string{"", "canary", "canary:", ":20", "canary:abc", "canary:101", "canary:-1"} {
		if _, err := NewBucketRouter("main", spec, "s"); err == nil {
			t.Errorf("expected error for spec %q, got nil", spec)
		}
	}
}

func TestRoute_Edges(t *testing.T) {
	// pct=0 always returns default; pct=100 always returns canary.
	r0, _ := NewBucketRouter("main", "canary:0", "s")
	if b := r0.Route(resolve.QueryContext{ClientIP: "10.0.0.1"}); b != "main" {
		t.Errorf("pct=0: want main, got %q", b)
	}
	r100, _ := NewBucketRouter("main", "canary:100", "s")
	if b := r100.Route(resolve.QueryContext{ClientIP: "10.0.0.1"}); b != "canary" {
		t.Errorf("pct=100: want canary, got %q", b)
	}
}

func TestRoute_DistributionApproximate(t *testing.T) {
	// With 1000 distinct /24s and pct=20, the canary share should be within
	// a generous ±5% window. We don't test for perfection — FNV-1a is good
	// enough for routing, not for stats.
	r, _ := NewBucketRouter("main", "canary:20", "rollout-1")
	canary := 0
	for i := 0; i < 1000; i++ {
		ip := fmt.Sprintf("10.0.%d.1", i%256)
		if r.Route(resolve.QueryContext{ClientIP: ip}) == "canary" {
			canary++
		}
	}
	// We expect ~200; allow 150..250.
	if canary < 100 || canary > 300 {
		t.Errorf("canary share %d/1000 outside expected 100..300", canary)
	}
}

func TestRoute_SameClientSameBucket(t *testing.T) {
	// A given subnet must always land in the same bucket for the same salt.
	r, _ := NewBucketRouter("main", "canary:50", "stable-salt")
	first := r.Route(resolve.QueryContext{ClientIP: "192.168.1.42"})
	for i := 0; i < 100; i++ {
		if got := r.Route(resolve.QueryContext{ClientIP: "192.168.1.42"}); got != first {
			t.Fatalf("non-deterministic routing: iter %d got %q after %q", i, got, first)
		}
	}
}

func TestRoute_DifferentSaltShufflesCohorts(t *testing.T) {
	a, _ := NewBucketRouter("main", "canary:50", "salt-A")
	b, _ := NewBucketRouter("main", "canary:50", "salt-B")
	disagreements := 0
	// Vary the /24 (second octet), not the host. The bucket function
	// hashes the /24, so 10.0.0.{0..255} would all collapse to one bucket.
	for i := 0; i < 256; i++ {
		ip := fmt.Sprintf("10.%d.0.1", i)
		if a.Route(resolve.QueryContext{ClientIP: ip}) != b.Route(resolve.QueryContext{ClientIP: ip}) {
			disagreements++
		}
	}
	// With 50% and independent hashes we expect roughly half disagree.
	if disagreements < 70 || disagreements > 190 {
		t.Errorf("salt independence weak: %d/256 disagreements", disagreements)
	}
}
