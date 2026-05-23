package dnssec

import (
	"testing"

	"github.com/miekg/dns"
)

func TestGenerateAndPersist(t *testing.T) {
	zk, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := zk.WriteToDir(dir, "foo.com."); err != nil {
		t.Fatal(err)
	}
	if !HasKeys(dir, "foo.com.") {
		t.Fatal("HasKeys = false after write")
	}
	loaded, err := LoadFromDir(dir, "foo.com.")
	if err != nil {
		t.Fatal(err)
	}
	// Bitwise equality on the public/private keys.
	if string(loaded.KSK.Public) != string(zk.KSK.Public) {
		t.Error("KSK public roundtrip diverged")
	}
	if string(loaded.ZSK.Private) != string(zk.ZSK.Private) {
		t.Error("ZSK private roundtrip diverged")
	}
}

func TestSignAndVerify_RRsetUnderZSK(t *testing.T) {
	zk, _ := Generate()
	rr1, _ := dns.NewRR("api.foo.com. 300 IN A 1.2.3.4")
	rr2, _ := dns.NewRR("api.foo.com. 300 IN A 5.6.7.8")
	rrs := []dns.RR{rr1, rr2}

	sig, err := SignRRset(rrs, "foo.com.", zk.ZSK, false, 0, 0)
	if err != nil {
		t.Fatalf("SignRRset: %v", err)
	}
	if sig.Signature == "" {
		t.Fatal("signature is empty")
	}

	_, zsk := zk.DNSKEYs("foo.com.", 300)
	if err := sig.Verify(zsk, rrs); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestSignAndVerify_DNSKEYUnderKSK(t *testing.T) {
	zk, _ := Generate()
	ksk, zsk := zk.DNSKEYs("foo.com.", 300)
	keyset := []dns.RR{ksk, zsk}

	sig, err := SignRRset(keyset, "foo.com.", zk.KSK, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := sig.Verify(ksk, keyset); err != nil {
		t.Fatalf("Verify(KSK over DNSKEY): %v", err)
	}
}

func TestVerify_RejectsTamperedRRset(t *testing.T) {
	zk, _ := Generate()
	rr, _ := dns.NewRR("api.foo.com. 300 IN A 1.2.3.4")
	rrs := []dns.RR{rr}
	sig, _ := SignRRset(rrs, "foo.com.", zk.ZSK, false, 0, 0)

	// Mutate the RR and re-verify — should fail.
	rrBad, _ := dns.NewRR("api.foo.com. 300 IN A 9.9.9.9")
	_, zskKey := zk.DNSKEYs("foo.com.", 300)
	if err := sig.Verify(zskKey, []dns.RR{rrBad}); err == nil {
		t.Fatal("Verify accepted tampered RRset")
	}
}

func TestDNSKEYs_HaveCorrectFlags(t *testing.T) {
	zk, _ := Generate()
	ksk, zsk := zk.DNSKEYs("foo.com.", 300)
	if ksk.Flags != 257 {
		t.Errorf("KSK flags = %d, want 257", ksk.Flags)
	}
	if zsk.Flags != 256 {
		t.Errorf("ZSK flags = %d, want 256", zsk.Flags)
	}
	if ksk.Algorithm != dns.ED25519 || zsk.Algorithm != dns.ED25519 {
		t.Error("expected Ed25519 algorithm")
	}
}
