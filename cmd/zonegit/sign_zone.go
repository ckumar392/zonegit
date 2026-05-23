package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/miekg/dns"
	"github.com/spf13/cobra"

	"github.com/ckumar392/zonegit/pkg/dnssec"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// keysDir returns the directory where DNSSEC keys for this repo live.
// Default: <repo>/keys/. The directory is created on first use.
func keysDir() string { return filepath.Join(flagRepoPath, "keys") }

// newZoneKeygenCmd implements `zonegit zone-keygen [zone]`.
//
// Generates a fresh Ed25519 KSK + ZSK and writes them under
// <repo>/keys/<zone>.{ksk,zsk}.{key,pub}. Without an argument, it uses
// the active zone.
func newZoneKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "zone-keygen [zone]",
		Short: "Generate a DNSSEC keypair (KSK + ZSK, Ed25519) for a zone",
		Args:  cobra.MaximumNArgs(1),
		Example: "  zonegit zone-keygen foo.com.",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			z := r.ActiveZone()
			if len(args) == 1 {
				z = args[0]
			}
			if z == "" {
				return fmt.Errorf("zone-keygen: no zone (active zone is empty; pass one explicitly)")
			}
			if dnssec.HasKeys(keysDir(), z) {
				return fmt.Errorf("zone-keygen: keys already exist for %s (delete them manually to regenerate)", z)
			}
			zk, err := dnssec.Generate()
			if err != nil {
				return err
			}
			if err := zk.WriteToDir(keysDir(), z); err != nil {
				return err
			}
			fmt.Printf("generated KSK + ZSK for %s in %s\n", z, keysDir())
			return nil
		},
	}
}

// newSignZoneCmd implements `zonegit sign-zone [--dry-run]`.
//
// Default behaviour (no --dry-run): loads the zone's KSK + ZSK from
// <repo>/keys/ and emits real RRSIGs that resolvers will validate.
// Falls back to placeholder mode automatically if no keys are present
// and --dry-run is set.
func newSignZoneCmd() *cobra.Command {
	var dryRun bool
	var ttl uint32
	var validityDays uint32
	cmd := &cobra.Command{
		Use:   "sign-zone",
		Short: "Stage DNSSEC records (DNSKEY + RRSIG over every RRset + NSEC chain) on the active branch",
		Long: `Stage a DNSSEC-signed view of the active zone.

Without --dry-run: loads the zone's KSK and ZSK from <repo>/keys/ and
emits real RRSIGs that resolvers will validate end-to-end. Run
zone-keygen first.

With --dry-run: emits placeholder signatures (zero crypto). Useful for
demos and tests that don't want to roll keys.`,
		Example: "  zonegit zone-keygen foo.com.\n  zonegit sign-zone -m 'add DNSSEC'",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()

			zname := r.ActiveZone()
			if zname == "" {
				return fmt.Errorf("sign-zone: no active zone")
			}

			var keys *dnssec.ZoneKeys
			if !dryRun {
				if !dnssec.HasKeys(keysDir(), zname) {
					return fmt.Errorf("sign-zone: no DNSSEC keys for %s in %s. Run `zonegit zone-keygen %s` first, or pass --dry-run for unsigned placeholders", zname, keysDir(), zname)
				}
				keys, err = dnssec.LoadFromDir(keysDir(), zname)
				if err != nil {
					return fmt.Errorf("sign-zone: load keys: %w", err)
				}
			}

			if err := stageDNSSECScaffold(ctx, r, keys, ttl, validityDays); err != nil {
				return err
			}
			msg := "DNSSEC signed"
			if dryRun {
				msg = "DNSSEC scaffold (dry-run, unsigned)"
			}
			h, err := r.Commit(ctx, authorIdentity(), msg)
			if err != nil {
				return err
			}
			fmt.Printf("[%s %s] %s\n", currentBranch(r), h.Short(), msg)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "emit placeholder signatures instead of real ones (no keys required)")
	cmd.Flags().Uint32Var(&ttl, "ttl", 300, "TTL for DNSKEY / NSEC records")
	cmd.Flags().Uint32Var(&validityDays, "validity-days", 30, "RRSIG validity window in days")
	return cmd
}

// stageDNSSECScaffold walks HEAD's tree, collects every (owner, []rrtype),
// and stages DNSKEY + RRSIGs + NSEC chain in the repo's staging area.
//
// When keys is nil, signatures are placeholder bytes (dry-run mode).
// When keys is non-nil, RRSIGs carry real Ed25519 signatures produced
// by miekg/dns's RRSIG.Sign.
func stageDNSSECScaffold(ctx context.Context, r *repo.Repo, keys *dnssec.ZoneKeys, ttl uint32, validityDays uint32) error {
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

	// Collect owner → []rrtype across the entire zone, and remember which
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

	// 2) NSEC chain — alphabetical, wrap-around. Each NSEC also covers
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
	// in the object model — otherwise each Set() would overwrite the
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
			if t == "DNSKEY" {
				coveredRRs = dnskeySet
			} else if t == "NSEC" {
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
			} else {
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

// placeholderDNSKEYs returns base64-clean placeholder DNSKEYs for
// --dry-run mode (no real crypto).
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
// --dry-run mode.
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

