package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/miekg/dns"
	"github.com/spf13/cobra"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/zone"
)

// newSignZoneCmd implements `zonegit sign-zone --dry-run`.
//
// What this v0.5 milestone does:
//   - enumerates every RRset at HEAD
//   - generates an NSEC chain over the canonical sort of owner names
//   - generates placeholder DNSKEY records at the apex (KSK + ZSK)
//   - generates placeholder RRSIG records (one per RRset) with empty
//     signatures
//   - commits all of these in a single commit on the active branch
//
// What it does *not* do:
//   - actually compute cryptographic signatures
//   - validate against DNSSEC root trust anchors
//   - rotate keys
//
// The placeholders prove that DNSSEC records flow through the object
// model, the daemon's resolve path, and AXFR — without requiring a v0.6
// crypto implementation up front. A subsequent milestone replaces the
// empty Signature bytes with real Ed25519 / RSA / ECDSA output and adds
// key management.
func newSignZoneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sign-zone",
		Short: "Stage DNSSEC scaffolding (NSEC chain + DNSKEY + RRSIG placeholders) on the active branch",
		Long: `Stage a DNSSEC scaffold for the active zone.

v0.5 supports --dry-run only: signatures are placeholder bytes. The
records are DNSSEC-shaped and queryable via dig, but resolvers will
reject the signatures as invalid. v0.6 adds real signing.`,
		Example: "  zonegit sign-zone --dry-run -m 'add DNSSEC scaffold'",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				return fmt.Errorf("only --dry-run is supported in v0.5; live signing is a v0.6 milestone")
			}
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			if err := stageDNSSECScaffold(ctx, r); err != nil {
				return err
			}
			h, err := r.Commit(ctx, authorIdentity(), "DNSSEC scaffold (dry-run, unsigned)")
			if err != nil {
				return err
			}
			fmt.Printf("[%s %s] DNSSEC scaffold (dry-run, unsigned)\n", currentBranch(r), h.Short())
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "emit NSEC/DNSKEY/RRSIG records with placeholder signatures (required for v0.5)")
	return cmd
}

// stageDNSSECScaffold walks the current HEAD tree, gathers every
// (owner, []RRType), and stages NSEC + DNSKEY + RRSIG additions for one
// commit's worth of edits.
func stageDNSSECScaffold(ctx context.Context, r *repo.Repo) error {
	zoneName := r.ActiveZone()
	if zoneName == "" {
		return fmt.Errorf("sign-zone: no active zone")
	}
	_, _, head, err := r.Head(ctx)
	if err != nil {
		return err
	}
	if head.IsZero() {
		return fmt.Errorf("sign-zone: HEAD is empty; commit at least one RRset first")
	}
	headTree := treeOfCommit(ctx, r.Storage(), head)
	if headTree.IsZero() {
		return fmt.Errorf("sign-zone: HEAD has no tree")
	}

	// Collect owner → types[] across the entire zone.
	owners := map[string][]string{}
	err = object.WalkAllLeaves(ctx, r.Storage(), headTree, func(path []string, rrtype string, _ store.Hash) error {
		owner := ownerFQDN(zoneName, path)
		owners[owner] = append(owners[owner], rrtype)
		return nil
	})
	if err != nil {
		return fmt.Errorf("sign-zone: walk: %w", err)
	}
	if len(owners) == 0 {
		return fmt.Errorf("sign-zone: zone is empty")
	}

	// 1) DNSKEY at apex (KSK + ZSK, placeholder material).
	ksk, zsk := placeholderDNSKEYs(zoneName)
	if err := r.Set(ctx, []dns.RR{ksk, zsk}); err != nil {
		return fmt.Errorf("sign-zone: stage DNSKEY: %w", err)
	}
	owners[zoneName] = append(owners[zoneName], "DNSKEY")

	// 2) NSEC chain over the sorted owner names. Each NSEC also covers
	// itself and the RRSIG type that will be generated below, so we add
	// "NSEC" and "RRSIG" to every name's type set.
	names := make([]string, 0, len(owners))
	for n := range owners {
		owners[n] = append(owners[n], "NSEC", "RRSIG")
		names = append(names, n)
	}
	sort.Strings(names)

	for i, owner := range names {
		next := names[(i+1)%len(names)]
		// Sort + dedup the type bitmap (canonical order).
		typesUniq := uniqStrings(owners[owner])
		bitmap := make([]uint16, 0, len(typesUniq))
		for _, t := range typesUniq {
			if t == "" {
				continue
			}
			if code, ok := dns.StringToType[t]; ok {
				bitmap = append(bitmap, code)
			}
		}
		sort.Slice(bitmap, func(i, j int) bool { return bitmap[i] < bitmap[j] })
		nsec := &dns.NSEC{
			Hdr: dns.RR_Header{
				Name:   owner,
				Rrtype: dns.TypeNSEC,
				Class:  dns.ClassINET,
				Ttl:    300,
			},
			NextDomain: next,
			TypeBitMap: bitmap,
		}
		if err := r.Set(ctx, []dns.RR{nsec}); err != nil {
			return fmt.Errorf("sign-zone: stage NSEC %s: %w", owner, err)
		}
	}

	// 3) RRSIG per RRset. We must look up the actual RRsets to learn
	// their TTLs (RRSIG.OrigTtl mirrors the covered RRset's TTL).
	inception := uint32(time.Now().Unix())
	expiration := inception + 30*24*3600 // 30-day placeholder validity
	for owner, types := range owners {
		for _, t := range types {
			if t == "NSEC" || t == "RRSIG" || t == "DNSKEY" {
				// Always sign these too.
			}
			rsKey := stripZoneOwner(owner, zoneName)
			covered, err := r.Lookup(ctx, head, rsKey, t)
			if err != nil {
				// The DNSKEY/NSEC/RRSIG we just staged aren't in HEAD's
				// tree yet (they're in staging). Sign them via reasonable
				// defaults — TTL 300, the typical zonefile default.
				covered = zone.RRset{TTL: 300}
			}
			rrsig := placeholderRRSIG(owner, t, zoneName, covered.TTL, inception, expiration)
			if err := r.Set(ctx, []dns.RR{rrsig}); err != nil {
				return fmt.Errorf("sign-zone: stage RRSIG %s %s: %w", owner, t, err)
			}
		}
	}
	return nil
}

// placeholderDNSKEYs returns a KSK and ZSK with non-cryptographic key
// material. Algorithm 8 (RSA-SHA256) is chosen because it's the most
// common algorithm secondaries recognize without complaining about
// unknown codes; the actual key bytes are placeholder data and will not
// validate signatures. v0.6 replaces these with real keypair output.
func placeholderDNSKEYs(zoneName string) (ksk, zsk *dns.DNSKEY) {
	ksk = &dns.DNSKEY{
		Hdr: dns.RR_Header{
			Name:   zoneName,
			Rrtype: dns.TypeDNSKEY,
			Class:  dns.ClassINET,
			Ttl:    300,
		},
		Flags:     257, // KSK (256 = ZSK)
		Protocol:  3,
		Algorithm: dns.RSASHA256,
		// Placeholder base64. miekg/dns parses PublicKey as base64; we
		// pick a 52-char base64-clean string so it decodes cleanly. The
		// resulting bytes are not a real RSA key — they won't validate
		// against any signature. v0.6 replaces with real KSK material.
		PublicKey: "AwEAAcUlFV1vhmqx6NSOUOJgPLkNgmpC0c8oXdSnPp9LpvWPdcA3",
	}
	zsk = &dns.DNSKEY{
		Hdr: dns.RR_Header{
			Name:   zoneName,
			Rrtype: dns.TypeDNSKEY,
			Class:  dns.ClassINET,
			Ttl:    300,
		},
		Flags:     256, // ZSK
		Protocol:  3,
		Algorithm: dns.RSASHA256,
		PublicKey: "AwEAAdVlGW2whnrx7OTPVPKhQMlOhnqD1d9pYeTpQq+MqwXQecB4",
	}
	return
}

// placeholderRRSIG returns an RRSIG with empty signature bytes,
// covering the given (owner, type). Validity period is 30 days from now.
func placeholderRRSIG(owner, covered, zoneName string, origTTL, inception, expiration uint32) *dns.RRSIG {
	if origTTL == 0 {
		origTTL = 300
	}
	coveredCode := dns.StringToType[covered]
	return &dns.RRSIG{
		Hdr: dns.RR_Header{
			Name:   owner,
			Rrtype: dns.TypeRRSIG,
			Class:  dns.ClassINET,
			Ttl:    origTTL,
		},
		TypeCovered: coveredCode,
		Algorithm:   dns.RSASHA256,
		Labels:      uint8(countLabels(owner)),
		OrigTtl:     origTTL,
		Expiration:  expiration,
		Inception:   inception,
		KeyTag:      0,
		SignerName:  zoneName,
		Signature:   "AAAA", // placeholder — v0.6 replaces with real bytes
	}
}

func ownerFQDN(zoneName string, path []string) string {
	if len(path) == 0 {
		return zoneName
	}
	// path is [zone-down], with last element being the deepest label.
	// Reverse + join + zone suffix.
	out := ""
	for i := len(path) - 1; i >= 0; i-- {
		if out == "" {
			out = path[i]
		} else {
			out = out + "." + path[i]
		}
	}
	return out + "." + zoneName
}

// stripZoneOwner turns "api.foo.com." into "api" given zone "foo.com.";
// the apex becomes "@" (Repo.Set normalises that back to "").
func stripZoneOwner(owner, zoneName string) string {
	if owner == zoneName {
		return "@"
	}
	if len(owner) > len(zoneName)+1 && owner[len(owner)-len(zoneName)-1:] == "."+zoneName {
		return owner[:len(owner)-len(zoneName)-1]
	}
	return owner
}

// countLabels returns the number of non-empty labels in an FQDN, used
// for RRSIG.Labels per RFC 4034.
func countLabels(name string) int {
	if name == "" || name == "." {
		return 0
	}
	n := 0
	for _, b := range name {
		if b == '.' {
			n++
		}
	}
	// trailing dot is counted as a separator only — if the name ends
	// with ".", we've over-counted by one.
	if len(name) > 0 && name[len(name)-1] == '.' {
		return n
	}
	return n + 1
}

func uniqStrings(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// treeOfCommit returns the tree hash inside the commit at h, or
// ZeroHash on error.
func treeOfCommit(ctx context.Context, s store.Storage, h store.Hash) store.Hash {
	if h.IsZero() {
		return store.ZeroHash
	}
	obj, err := s.GetObject(ctx, h)
	if err != nil {
		return store.ZeroHash
	}
	c, err := object.DecodeCommit(obj.Payload)
	if err != nil {
		return store.ZeroHash
	}
	return c.Tree
}
