// Package route implements the per-query branch selection used by
// zonegitd's canary feature.
//
// This is intentionally a tiny subset of docs/SELECTORS.md — one rule
// shape only: "hash(client.subnet, salt) % 100 < pct -> branch". The
// full grammar is a v3 feature; what's here is what the demo actually
// needs to land UC5 ("send 20% of traffic to canary, snap it back on
// one ref move").
package route

import (
	"fmt"
	"hash/fnv"
	"net"
	"strings"

	"github.com/ckumar392/zonegit/pkg/resolve"
)

// BucketRouter splits traffic between two branches by hashing the client
// /24 (IPv4) or /48 (IPv6) into 100 buckets. Buckets 0..Pct-1 go to
// CanaryBranch; the rest go to DefaultBranch.
//
// Stable: the same client subnet always lands in the same bucket given
// the same salt. Operators rotate the salt to "reshuffle" cohorts.
type BucketRouter struct {
	DefaultBranch string
	CanaryBranch  string
	Pct           int    // 0..100; 0 disables, 100 sends everything to canary
	Salt          string // makes independent rollouts independent
}

// NewBucketRouter validates inputs and returns a router. canarySpec is
// "branch:pct" (e.g. "canary:20"). defaultBranch is what non-canary
// traffic resolves to.
func NewBucketRouter(defaultBranch, canarySpec, salt string) (*BucketRouter, error) {
	colon := strings.LastIndex(canarySpec, ":")
	if colon < 1 || colon == len(canarySpec)-1 {
		return nil, fmt.Errorf("route: canary spec %q must be \"branch:pct\"", canarySpec)
	}
	branch := canarySpec[:colon]
	var pct int
	if _, err := fmt.Sscanf(canarySpec[colon+1:], "%d", &pct); err != nil {
		return nil, fmt.Errorf("route: canary pct: %w", err)
	}
	if pct < 0 || pct > 100 {
		return nil, fmt.Errorf("route: canary pct %d out of range 0..100", pct)
	}
	if salt == "" {
		salt = "zonegit-default"
	}
	return &BucketRouter{
		DefaultBranch: defaultBranch,
		CanaryBranch:  branch,
		Pct:           pct,
		Salt:          salt,
	}, nil
}

// Route picks the branch for q. Implements the resolve.Router interface.
func (b *BucketRouter) Route(q resolve.QueryContext) string {
	if b.Pct <= 0 {
		return b.DefaultBranch
	}
	if b.Pct >= 100 {
		return b.CanaryBranch
	}
	bucket := b.bucket(q.ClientIP)
	if bucket < b.Pct {
		return b.CanaryBranch
	}
	return b.DefaultBranch
}

// bucket computes the 0..99 bucket for a client IP by hashing
// (subnet_bytes, salt) with FNV-1a 64-bit.
func (b *BucketRouter) bucket(clientIP string) int {
	subnet := subnetBytes(clientIP)
	h := fnv.New64a()
	_, _ = h.Write(subnet)
	_, _ = h.Write([]byte{0}) // separator
	_, _ = h.Write([]byte(b.Salt))
	return int(h.Sum64() % 100)
}

// subnetBytes returns the network portion of clientIP at /24 for IPv4
// and /48 for IPv6 (matching RFC 7871's typical ECS scope). Returns an
// empty slice for unparseable input; in that case the bucket function
// still produces a stable bucket (purely from the salt).
func subnetBytes(clientIP string) []byte {
	if clientIP == "" {
		return nil
	}
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[:3] // /24
	}
	return ip.To16()[:6] // /48
}

// String returns a one-line summary suitable for the metrics info gauge.
func (b *BucketRouter) String() string {
	return fmt.Sprintf("%s:%d -> %s (default %s, salt=%q)",
		b.CanaryBranch, b.Pct, b.CanaryBranch, b.DefaultBranch, b.Salt)
}
