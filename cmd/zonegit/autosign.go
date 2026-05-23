package main

import (
	"context"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/dnssec"
	"github.com/ckumar392/zonegit/pkg/repo"
)

// autoSignTouched re-signs the RRsets just staged via r.Set so resolvers
// continue to validate after this commit lands.
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
//
// If the zone has no DNSSEC keys, this is a silent no-op — the caller
// asked to auto-sign but nothing's configured, so we don't fail.
func autoSignTouched(ctx context.Context, r *repo.Repo, touched []dns.RR) error {
	zoneName := r.ActiveZone()
	if zoneName == "" {
		return nil
	}
	if !dnssec.HasKeys(keysDir(), zoneName) {
		return nil
	}
	keys, err := dnssec.LoadFromDir(keysDir(), zoneName)
	if err != nil {
		return err
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
		rel := stripZone(k.owner, zoneName)
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
