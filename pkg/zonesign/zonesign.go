// Package zonesign is the DNSSEC signing engine: the orchestration layer
// that turns a plain zone held in a repo into a signed one.
//
// It sits above pkg/repo and uses pkg/dnssec for the cryptographic
// primitives (key generation, RRSIG production, key-tag math). Keeping it
// here -- rather than in package main -- lets the CLI, the daemon, and the
// CoreDNS plugin all sign zones, and makes the logic unit-testable.
//
// Two entry points:
//
//   - SignZone stages a full DNSSEC view of the active zone (DNSKEY at the
//     apex, an NSEC chain, and an RRSIG over every RRset). It does not
//     commit; the caller decides when to seal the change.
//   - AutoSign re-signs just the RRsets that were touched by a write, so a
//     signed zone stays valid after incremental edits.
package zonesign

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/dnssec"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Options tunes SignZone. The zero value is valid and uses the defaults
// below.
type Options struct {
	// TTL for the synthesized DNSKEY and NSEC records. Defaults to 300.
	TTL uint32
	// ValidityDays is the RRSIG validity window. Defaults to 30.
	ValidityDays uint32
}

const (
	defaultTTL          = 300
	defaultValidityDays = 30
)

func (o Options) ttl() uint32 {
	if o.TTL == 0 {
		return defaultTTL
	}
	return o.TTL
}

func (o Options) validityDays() uint32 {
	if o.ValidityDays == 0 {
		return defaultValidityDays
	}
	return o.ValidityDays
}

// SignZone walks HEAD's tree, collects every (owner, []rrtype), and stages
// DNSKEY + RRSIGs + NSEC chain in the repo's staging area. The records are
// staged, not committed -- the caller commits.
//
// When keys is nil, signatures are placeholder bytes (dry-run mode): the
// DNSSEC structure is complete but carries no real crypto. When keys is
// non-nil, RRSIGs carry real Ed25519 signatures produced by pkg/dnssec.
func SignZone(ctx context.Context, r *repo.Repo, keys *dnssec.ZoneKeys, opts Options) error {
	ttl := opts.ttl()
	validityDays := opts.validityDays()

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
	headTree := object.TreeOf(ctx, r.Storage(), head)
	if headTree.IsZero() {
		return fmt.Errorf("sign-zone: HEAD has no tree")
	}

	// Collect owner -> []rrtype across the entire zone, and remember which
	// blob each (owner, rrtype) pair points at so we can re-load the
	// RRset to sign it.
	type ownerEntry struct {
		types map[string]store.Hash
	}
	owners := map[string]*ownerEntry{}
	err = object.WalkAllLeaves(ctx, r.Storage(), headTree, func(path []string, rrtype string, blobHash store.Hash) error {
		owner := ownerFQDN(zoneName, path)
		e, ok := owners[owner]
		if !ok {
			e = &ownerEntry{types: map[string]store.Hash{}}
			owners[owner] = e
		}
		e.types[rrtype] = blobHash
		return nil
	})
	if err != nil {
		return fmt.Errorf("sign-zone: walk: %w", err)
	}
	if len(owners) == 0 {
		return fmt.Errorf("sign-zone: zone is empty")
	}

	// 1) Stage the DNSKEY RRset at the apex.
	var ksk, zsk *dns.DNSKEY
	if keys != nil {
		ksk, zsk = keys.DNSKEYs(zoneName, ttl)
	} else {
		ksk, zsk = placeholderDNSKEYs(zoneName, ttl)
	}
	dnskeySet := []dns.RR{ksk, zsk}
	if err := r.Set(ctx, dnskeySet); err != nil {
		return fmt.Errorf("sign-zone: stage DNSKEY: %w", err)
	}
	if owners[zoneName] == nil {
		owners[zoneName] = &ownerEntry{types: map[string]store.Hash{}}
	}
	owners[zoneName].types["DNSKEY"] = store.ZeroHash // marker (we just staged it)

	// 2) NSEC chain -- alphabetical, wrap-around. Each NSEC also covers
	// itself and RRSIG (added below), so include those in the bitmap.
	names := make([]string, 0, len(owners))
	for n := range owners {
		names = append(names, n)
	}
	sort.Strings(names)

	// Stage NSEC records.
	for i, owner := range names {
		next := names[(i+1)%len(names)]
		types := make([]string, 0, len(owners[owner].types)+2)
		for t := range owners[owner].types {
			types = append(types, t)
		}
		types = append(types, "NSEC", "RRSIG")
		types = uniqStrings(types)
		bitmap := typesToBitmap(types)
		nsec := &dns.NSEC{
			Hdr:        dns.RR_Header{Name: owner, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: ttl},
			NextDomain: next,
			TypeBitMap: bitmap,
		}
		if err := r.Set(ctx, []dns.RR{nsec}); err != nil {
			return fmt.Errorf("sign-zone: stage NSEC %s: %w", owner, err)
		}
		owners[owner].types["NSEC"] = store.ZeroHash
	}

	// 3) RRSIG per RRset, batched per owner.
	//
	// Multiple RRsets at the same owner each produce one RRSIG. They all
	// share Rrtype=RRSIG so they must be stored together as one RRset
	// in the object model -- otherwise each Set() would overwrite the
	// previous one at that owner.
	inception := uint32(time.Now().Unix())
	expiration := inception + validityDays*24*3600
	ownerSigs := map[string][]dns.RR{}
	for owner, e := range owners {
		for t := range e.types {
			if t == "RRSIG" {
				continue
			}
			var coveredRRs []dns.RR
			switch t {
			case "DNSKEY":
				coveredRRs = dnskeySet
			case "NSEC":
				idx := sort.SearchStrings(names, owner)
				next := names[(idx+1)%len(names)]
				types := []string{}
				for tt := range e.types {
					types = append(types, tt)
				}
				types = uniqStrings(append(types, "NSEC", "RRSIG"))
				coveredRRs = []dns.RR{&dns.NSEC{
					Hdr:        dns.RR_Header{Name: owner, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: ttl},
					NextDomain: next,
					TypeBitMap: typesToBitmap(types),
				}}
			default:
				rsKey := stripZoneOwner(owner, zoneName)
				rs, err := r.Lookup(ctx, head, rsKey, t)
				if err != nil {
					return fmt.Errorf("sign-zone: lookup %s %s: %w", owner, t, err)
				}
				coveredRRs = rs.RRs
			}

			var rrsig *dns.RRSIG
			if keys != nil {
				// KSK signs DNSKEY only; ZSK signs everything else.
				isKSK := t == "DNSKEY"
				signer := keys.ZSK
				if isKSK {
					signer = keys.KSK
				}
				rrsig, err = dnssec.SignRRset(coveredRRs, zoneName, signer, isKSK, inception, expiration)
				if err != nil {
					return fmt.Errorf("sign-zone: sign %s %s: %w", owner, t, err)
				}
			} else {
				rrsig = placeholderRRSIG(owner, t, zoneName, coveredRRs[0].Header().Ttl, inception, expiration)
			}
			ownerSigs[owner] = append(ownerSigs[owner], rrsig)
		}
	}
	for owner, sigs := range ownerSigs {
		if len(sigs) == 0 {
			continue
		}
		// All RRSIGs at an owner share owner/class/type and TTL (we set
		// them all to the same TTL above). EncodeRRset enforces that.
		// Normalise TTLs to the first one's value to satisfy the
		// homogeneity check.
		baseTTL := sigs[0].Header().Ttl
		for _, s := range sigs {
			s.Header().Ttl = baseTTL
		}
		if err := r.Set(ctx, sigs); err != nil {
			return fmt.Errorf("sign-zone: stage RRSIG batch for %s: %w", owner, err)
		}
	}
	return nil
}

// AutoSign re-signs the RRsets just staged via r.Set so resolvers continue
// to validate after this commit lands. The records are staged, not
// committed. If keys is nil it is a no-op (nothing to sign with).
//
// The wrinkle: at any given owner, all RRSIGs live together in a single
// RRSIG RRset (one blob per owner, indexed by Rrtype=RRSIG). If we just
// staged a fresh RRSIG for owner X covering type T, we'd wipe out the
// RRSIGs that cover X's other types. So this helper:
//
//  1. Groups touched RRs by (owner, rrtype).
//  2. For each affected owner, loads the existing RRSIG RRset at HEAD.
//  3. Drops any RRSIG whose TypeCovered matches one of the touched types
//     (those are about to be re-signed).
//  4. Appends a freshly-computed RRSIG for each touched (owner, type).
//  5. Stages the merged RRSIG set per owner.
func AutoSign(ctx context.Context, r *repo.Repo, keys *dnssec.ZoneKeys, touched []dns.RR) error {
	zoneName := r.ActiveZone()
	if zoneName == "" {
		return nil
	}
	if keys == nil {
		return nil
	}

	// Group touched RRs by (owner, rrtype).
	type key struct{ owner, rtype string }
	groups := map[key][]dns.RR{}
	for _, rr := range touched {
		h := rr.Header()
		owner := strings.ToLower(dns.Fqdn(h.Name))
		rtype := dns.TypeToString[h.Rrtype]
		if rtype == "" {
			continue
		}
		k := key{owner: owner, rtype: rtype}
		groups[k] = append(groups[k], rr)
	}

	// Per-owner accumulator. Pre-populate with existing RRSIGs at HEAD so
	// we preserve signatures for *untouched* RRsets at the same owner.
	ownerSigs := map[string][]dns.RR{}
	_, _, head, _ := r.Head(ctx)
	for k := range groups {
		if _, done := ownerSigs[k.owner]; done {
			continue
		}
		if head.IsZero() {
			ownerSigs[k.owner] = nil
			continue
		}
		rel := stripZoneOwner(k.owner, zoneName)
		existing, err := r.Lookup(ctx, head, rel, "RRSIG")
		if err == nil {
			ownerSigs[k.owner] = append(ownerSigs[k.owner], existing.RRs...)
		} else {
			ownerSigs[k.owner] = nil
		}
	}

	inception := uint32(time.Now().Unix())
	expiration := inception + 30*24*3600

	for k, rrs := range groups {
		coveredCode := dns.StringToType[k.rtype]
		// Drop existing RRSIGs that are about to be replaced.
		kept := ownerSigs[k.owner][:0]
		for _, e := range ownerSigs[k.owner] {
			if sig, ok := e.(*dns.RRSIG); ok && sig.TypeCovered == coveredCode {
				continue
			}
			kept = append(kept, e)
		}
		// Sign the touched RRset with the ZSK and add to the bucket.
		rrsig, err := dnssec.SignRRset(rrs, zoneName, keys.ZSK, false, inception, expiration)
		if err != nil {
			return err
		}
		kept = append(kept, rrsig)
		ownerSigs[k.owner] = kept
	}

	// Stage one RRSIG RRset per owner. TTLs must be homogeneous per the
	// EncodeRRset contract; we normalise to the first sig's TTL.
	for _, sigs := range ownerSigs {
		if len(sigs) == 0 {
			continue
		}
		baseTTL := sigs[0].Header().Ttl
		for _, s := range sigs {
			s.Header().Ttl = baseTTL
		}
		if err := r.Set(ctx, sigs); err != nil {
			return err
		}
	}
	return nil
}

// placeholderDNSKEYs returns base64-clean placeholder DNSKEYs for
// dry-run mode (no real crypto).
func placeholderDNSKEYs(zoneName string, ttl uint32) (ksk, zsk *dns.DNSKEY) {
	ksk = &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zoneName, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: ttl},
		Flags:     257,
		Protocol:  3,
		Algorithm: dns.RSASHA256,
		PublicKey: "AwEAAcUlFV1vhmqx6NSOUOJgPLkNgmpC0c8oXdSnPp9LpvWPdcA3",
	}
	zsk = &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zoneName, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: ttl},
		Flags:     256,
		Protocol:  3,
		Algorithm: dns.RSASHA256,
		PublicKey: "AwEAAdVlGW2whnrx7OTPVPKhQMlOhnqD1d9pYeTpQq+MqwXQecB4",
	}
	return
}

// placeholderRRSIG returns an RRSIG with empty signature bytes for
// dry-run mode.
func placeholderRRSIG(owner, covered, zoneName string, origTTL, inception, expiration uint32) *dns.RRSIG {
	if origTTL == 0 {
		origTTL = 300
	}
	return &dns.RRSIG{
		Hdr:         dns.RR_Header{Name: owner, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: origTTL},
		TypeCovered: dns.StringToType[covered],
		Algorithm:   dns.RSASHA256,
		Labels:      uint8(dns.CountLabel(owner)),
		OrigTtl:     origTTL,
		Expiration:  expiration,
		Inception:   inception,
		KeyTag:      0,
		SignerName:  zoneName,
		Signature:   "AAAA",
	}
}

func typesToBitmap(types []string) []uint16 {
	out := make([]uint16, 0, len(types))
	seen := map[uint16]bool{}
	for _, t := range types {
		if code, ok := dns.StringToType[t]; ok && !seen[code] {
			out = append(out, code)
			seen[code] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ownerFQDN reconstructs the owner name from a tree path (apex-nearest
// label first) relative to zoneName.
func ownerFQDN(zoneName string, path []string) string {
	if len(path) == 0 {
		return zoneName
	}
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

// stripZoneOwner turns an owner FQDN into the repo's relative key ("@" for
// the apex, e.g. "api" for "api.foo.com." in zone "foo.com.").
func stripZoneOwner(owner, zoneName string) string {
	if owner == zoneName {
		return "@"
	}
	if len(owner) > len(zoneName)+1 && owner[len(owner)-len(zoneName)-1:] == "."+zoneName {
		return owner[:len(owner)-len(zoneName)-1]
	}
	return owner
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
