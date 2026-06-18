package main

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/dnssec"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/zone"
)

// These tests pin the observable output of the DNSSEC signing engine
// (stageDNSSECScaffold + autoSignTouched) so it can be lifted out of
// package main into pkg/zonesign without changing behaviour. The
// invariants asserted here hold regardless of where the code lives.

const charZone = "foo.com."

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("NewRR(%q): %v", s, err)
	}
	return rr
}

// seedCharZone builds an in-memory repo holding a small but varied zone
// and commits it. The shape deliberately exercises the tricky cases:
//   - apex carries two types (SOA + NS) plus the synthesized DNSKEY,
//   - api carries two types (A + TXT) to force per-owner RRSIG batching,
//   - x.sub is multi-label to exercise NSEC ordering and owner-name
//     reconstruction from the tree path.
func seedCharZone(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	ctx := context.Background()
	if err := r.Init(ctx, charZone); err != nil {
		t.Fatal(err)
	}
	sets := [][]dns.RR{
		{mustRR(t, "foo.com. 300 IN SOA ns1.foo.com. admin.foo.com. 1 7200 3600 1209600 300")},
		{mustRR(t, "foo.com. 300 IN NS ns1.foo.com.")},
		{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")},
		{mustRR(t, `api.foo.com. 300 IN TXT "hello"`)},
		{mustRR(t, "www.foo.com. 300 IN AAAA 2001:db8::1")},
		{mustRR(t, "x.sub.foo.com. 300 IN A 5.6.7.8")},
	}
	for _, s := range sets {
		if err := r.Set(ctx, s); err != nil {
			t.Fatalf("Set %s: %v", s[0].Header().Name, err)
		}
	}
	if _, err := r.Commit(ctx, object.Identity{Name: "seed", Email: "s@s"}, "seed"); err != nil {
		t.Fatal(err)
	}
	return r
}

// signedZone maps owner FQDN -> rrtype -> RRs at HEAD.
type signedZone map[string]map[string][]dns.RR

func collectSignedZone(t *testing.T, r *repo.Repo) signedZone {
	t.Helper()
	ctx := context.Background()
	_, _, head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tree := object.TreeOf(ctx, r.Storage(), head)
	if tree.IsZero() {
		t.Fatal("HEAD has no tree")
	}
	zoneName := r.ActiveZone()
	out := signedZone{}
	err = object.WalkAllLeaves(ctx, r.Storage(), tree, func(path []string, rrtype string, blob store.Hash) error {
		owner := fqdnFromPath(zoneName, path)
		obj, err := r.Storage().GetObject(ctx, blob)
		if err != nil {
			return err
		}
		rrset, err := zone.DecodeRRset(object.DecodeBlob(obj.Payload).Payload)
		if err != nil {
			return err
		}
		if out[owner] == nil {
			out[owner] = map[string][]dns.RR{}
		}
		out[owner][rrtype] = rrset.RRs
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// fqdnFromPath reverses a tree path (apex-nearest label first) into an
// owner FQDN. It mirrors ownerFQDN but lives in the test so the
// assertions survive the engine moving packages.
func fqdnFromPath(zoneName string, path []string) string {
	if len(path) == 0 {
		return zoneName
	}
	labels := make([]string, len(path))
	for i := range path {
		labels[i] = path[len(path)-1-i]
	}
	return strings.Join(labels, ".") + "." + zoneName
}

// rrsigsCovering indexes an RRSIG RRset by the type each signature covers.
func rrsigsCovering(rrs []dns.RR) map[uint16]*dns.RRSIG {
	out := map[uint16]*dns.RRSIG{}
	for _, rr := range rrs {
		if sig, ok := rr.(*dns.RRSIG); ok {
			out[sig.TypeCovered] = sig
		}
	}
	return out
}

func sortedOwners(z signedZone) []string {
	owners := make([]string, 0, len(z))
	for owner := range z {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}

// assertScaffoldInvariants checks the structural DNSSEC properties that
// must hold no matter where the signing engine lives or whether real keys
// were used.
func assertScaffoldInvariants(t *testing.T, z signedZone, zoneName string) {
	t.Helper()

	// (1) apex DNSKEY: exactly one KSK (flags 257) and one ZSK (256).
	apex := z[zoneName]
	if apex == nil {
		t.Fatalf("no apex owner %q in signed zone", zoneName)
	}
	dnskeys := apex["DNSKEY"]
	if len(dnskeys) != 2 {
		t.Fatalf("apex DNSKEY count = %d, want 2", len(dnskeys))
	}
	var sawKSK, sawZSK bool
	for _, rr := range dnskeys {
		k, ok := rr.(*dns.DNSKEY)
		if !ok {
			t.Fatalf("apex DNSKEY contains non-DNSKEY %T", rr)
		}
		switch k.Flags {
		case 257:
			sawKSK = true
		case 256:
			sawZSK = true
		default:
			t.Errorf("unexpected DNSKEY flags %d", k.Flags)
		}
	}
	if !sawKSK || !sawZSK {
		t.Errorf("apex DNSKEY: KSK=%v ZSK=%v, want both", sawKSK, sawZSK)
	}

	// (2) every owner with records has an RRSIG set, and each non-RRSIG
	// type at the owner is covered by exactly one RRSIG.
	owners := sortedOwners(z)
	for _, owner := range owners {
		types := z[owner]
		sigs, ok := types["RRSIG"]
		if !ok {
			t.Errorf("owner %s has no RRSIG set", owner)
			continue
		}
		covered := rrsigsCovering(sigs)
		want := 0
		for rrtype := range types {
			if rrtype == "RRSIG" {
				continue
			}
			want++
			if _, ok := covered[dns.StringToType[rrtype]]; !ok {
				t.Errorf("owner %s: no RRSIG covers %s", owner, rrtype)
			}
		}
		if len(covered) != want {
			t.Errorf("owner %s: %d RRSIGs, want %d (one per non-RRSIG type)", owner, len(covered), want)
		}
	}

	// (3) NSEC chain: alphabetical with wrap-around, one per owner, each
	// bitmap includes NSEC and RRSIG.
	for i, owner := range owners {
		nsecRRs := z[owner]["NSEC"]
		if len(nsecRRs) != 1 {
			t.Errorf("owner %s: %d NSEC, want 1", owner, len(nsecRRs))
			continue
		}
		nsec, ok := nsecRRs[0].(*dns.NSEC)
		if !ok {
			t.Errorf("owner %s: NSEC is %T", owner, nsecRRs[0])
			continue
		}
		next := owners[(i+1)%len(owners)]
		if nsec.NextDomain != next {
			t.Errorf("owner %s: NSEC next = %s, want %s", owner, nsec.NextDomain, next)
		}
		bm := map[uint16]bool{}
		for _, tcode := range nsec.TypeBitMap {
			bm[tcode] = true
		}
		if !bm[dns.TypeNSEC] || !bm[dns.TypeRRSIG] {
			t.Errorf("owner %s: NSEC bitmap missing NSEC/RRSIG: %v", owner, nsec.TypeBitMap)
		}
	}
}

func TestStageDNSSECScaffold_Characterization(t *testing.T) {
	t.Run("dry-run", func(t *testing.T) {
		r := seedCharZone(t)
		ctx := context.Background()
		if err := stageDNSSECScaffold(ctx, r, nil, 300, 30); err != nil {
			t.Fatalf("stageDNSSECScaffold(dry-run): %v", err)
		}
		if _, err := r.Commit(ctx, object.Identity{Name: "s", Email: "s@s"}, "dnssec"); err != nil {
			t.Fatal(err)
		}
		assertScaffoldInvariants(t, collectSignedZone(t, r), r.ActiveZone())
	})

	t.Run("real-keys", func(t *testing.T) {
		r := seedCharZone(t)
		ctx := context.Background()
		keys, err := dnssec.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if err := stageDNSSECScaffold(ctx, r, keys, 300, 30); err != nil {
			t.Fatalf("stageDNSSECScaffold(real): %v", err)
		}
		if _, err := r.Commit(ctx, object.Identity{Name: "s", Email: "s@s"}, "dnssec"); err != nil {
			t.Fatal(err)
		}
		z := collectSignedZone(t, r)
		zoneName := r.ActiveZone()
		assertScaffoldInvariants(t, z, zoneName)

		// KSK signs the DNSKEY RRset; ZSK signs everything else.
		kskRR, zskRR := keys.DNSKEYs(zoneName, 300)
		kskTag, zskTag := kskRR.KeyTag(), zskRR.KeyTag()
		for owner, types := range z {
			for _, sig := range rrsigsCovering(types["RRSIG"]) {
				wantTag, role := zskTag, "ZSK"
				if sig.TypeCovered == dns.TypeDNSKEY {
					wantTag, role = kskTag, "KSK"
				}
				if sig.KeyTag != wantTag {
					t.Errorf("owner %s: RRSIG(%s) KeyTag=%d, want %s tag %d",
						owner, dns.TypeToString[sig.TypeCovered], sig.KeyTag, role, wantTag)
				}
			}
		}

		// Cryptographic verification: the published KSK validates the apex
		// DNSKEY RRset and the published ZSK validates the api A RRset.
		var pubKSK, pubZSK *dns.DNSKEY
		for _, rr := range z[zoneName]["DNSKEY"] {
			if k := rr.(*dns.DNSKEY); k.Flags == 257 {
				pubKSK = k
			} else {
				pubZSK = k
			}
		}
		if sig := rrsigsCovering(z[zoneName]["RRSIG"])[dns.TypeDNSKEY]; sig == nil {
			t.Error("apex: missing RRSIG over DNSKEY")
		} else if err := sig.Verify(pubKSK, z[zoneName]["DNSKEY"]); err != nil {
			t.Errorf("apex DNSKEY RRSIG.Verify(KSK): %v", err)
		}
		if sig := rrsigsCovering(z["api.foo.com."]["RRSIG"])[dns.TypeA]; sig == nil {
			t.Error("api: missing RRSIG over A")
		} else if err := sig.Verify(pubZSK, z["api.foo.com."]["A"]); err != nil {
			t.Errorf("api A RRSIG.Verify(ZSK): %v", err)
		}
	})
}

// TestAutoSignTouched_PreservesUntouchedSigs pins the subtle property that
// re-signing one type at an owner does not drop the RRSIGs covering that
// owner's other types (they all share a single RRSIG RRset).
func TestAutoSignTouched_PreservesUntouchedSigs(t *testing.T) {
	ctx := context.Background()

	// autoSignTouched and keysDir read the flagRepoPath global; point it at
	// a temp dir holding real keys for the zone.
	saved := flagRepoPath
	flagRepoPath = t.TempDir()
	defer func() { flagRepoPath = saved }()

	keys, err := dnssec.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.WriteToDir(keysDir(), charZone); err != nil {
		t.Fatal(err)
	}

	r := seedCharZone(t)

	// Fully sign once so api carries RRSIGs for A, TXT and NSEC.
	if err := stageDNSSECScaffold(ctx, r, keys, 300, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, object.Identity{Name: "s", Email: "s@s"}, "dnssec"); err != nil {
		t.Fatal(err)
	}

	// Change only api A, then auto-sign just that touched RRset.
	newA := mustRR(t, "api.foo.com. 300 IN A 9.9.9.9")
	if err := r.Set(ctx, []dns.RR{newA}); err != nil {
		t.Fatal(err)
	}
	if err := autoSignTouched(ctx, r, []dns.RR{newA}); err != nil {
		t.Fatalf("autoSignTouched: %v", err)
	}
	if _, err := r.Commit(ctx, object.Identity{Name: "s", Email: "s@s"}, "auto-sign"); err != nil {
		t.Fatal(err)
	}

	z := collectSignedZone(t, r)
	covered := rrsigsCovering(z["api.foo.com."]["RRSIG"])

	// The untouched TXT and NSEC signatures survive; A is re-signed.
	for _, want := range []uint16{dns.TypeA, dns.TypeTXT, dns.TypeNSEC} {
		if _, ok := covered[want]; !ok {
			t.Errorf("api RRSIG set lost coverage for %s", dns.TypeToString[want])
		}
	}

	// The fresh A signature validates against the published ZSK over the
	// new record value.
	var pubZSK *dns.DNSKEY
	for _, rr := range z[charZone]["DNSKEY"] {
		if k := rr.(*dns.DNSKEY); k.Flags == 256 {
			pubZSK = k
		}
	}
	if sig := covered[dns.TypeA]; sig != nil && pubZSK != nil {
		if err := sig.Verify(pubZSK, z["api.foo.com."]["A"]); err != nil {
			t.Errorf("re-signed api A RRSIG.Verify(ZSK): %v", err)
		}
	}
}
