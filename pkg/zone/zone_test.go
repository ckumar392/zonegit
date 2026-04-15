package zone

import (
	"bytes"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("NewRR(%q): %v", s, err)
	}
	return rr
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	rrs := []dns.RR{
		mustRR(t, "api.foo.com. 300 IN A 1.2.3.4"),
		mustRR(t, "api.foo.com. 300 IN A 5.6.7.8"),
	}
	payload, err := EncodeRRset(rrs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRRset(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key.Name != "api.foo.com." {
		t.Errorf("name = %q", got.Key.Name)
	}
	if got.Key.Type != dns.TypeA {
		t.Errorf("type = %d", got.Key.Type)
	}
	if got.TTL != 300 {
		t.Errorf("ttl = %d", got.TTL)
	}
	if len(got.RRs) != 2 {
		t.Fatalf("len = %d", len(got.RRs))
	}
}

func TestEncodeIsCanonical(t *testing.T) {
	// {1.2.3.4, 5.6.7.8} and {5.6.7.8, 1.2.3.4} must hash identically.
	a := []dns.RR{
		mustRR(t, "api.foo.com. 300 IN A 1.2.3.4"),
		mustRR(t, "api.foo.com. 300 IN A 5.6.7.8"),
	}
	b := []dns.RR{
		mustRR(t, "api.foo.com. 300 IN A 5.6.7.8"),
		mustRR(t, "api.foo.com. 300 IN A 1.2.3.4"),
	}
	pa, err := EncodeRRset(a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := EncodeRRset(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pa, pb) {
		t.Fatalf("permutation produced different bytes:\n%x\n%x", pa, pb)
	}
}

func TestEncodeCaseInsensitiveOwner(t *testing.T) {
	a := []dns.RR{mustRR(t, "API.Foo.Com. 300 IN A 1.2.3.4")}
	b := []dns.RR{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")}
	pa, _ := EncodeRRset(a)
	pb, _ := EncodeRRset(b)
	if !bytes.Equal(pa, pb) {
		t.Fatal("case difference produced different bytes")
	}
}

func TestEncodeTTLAffectsHash(t *testing.T) {
	a := []dns.RR{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")}
	b := []dns.RR{mustRR(t, "api.foo.com. 600 IN A 1.2.3.4")}
	pa, _ := EncodeRRset(a)
	pb, _ := EncodeRRset(b)
	if bytes.Equal(pa, pb) {
		t.Fatal("different TTL must produce different bytes")
	}
}

func TestEncodeRejectsHeterogeneous(t *testing.T) {
	cases := [][]dns.RR{
		{
			mustRR(t, "a.foo.com. 300 IN A 1.2.3.4"),
			mustRR(t, "b.foo.com. 300 IN A 1.2.3.4"),
		},
		{
			mustRR(t, "a.foo.com. 300 IN A 1.2.3.4"),
			mustRR(t, "a.foo.com. 600 IN A 1.2.3.4"),
		},
	}
	for i, rrs := range cases {
		if _, err := EncodeRRset(rrs); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestEncodeRejectsEmpty(t *testing.T) {
	if _, err := EncodeRRset(nil); err == nil {
		t.Fatal("expected error for empty RRset")
	}
}

func TestDecodeMalformed(t *testing.T) {
	cases := [][]byte{
		{}, // empty
		{99}, // bad version
		{1, 0}, // owner unpack will fail (just root + nothing)
	}
	for i, p := range cases {
		if _, err := DecodeRRset(p); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestEncodeMxAndAaaa(t *testing.T) {
	cases := [][]dns.RR{
		{mustRR(t, "foo.com. 300 IN MX 10 mail.foo.com.")},
		{mustRR(t, "foo.com. 300 IN AAAA 2001:db8::1")},
		{mustRR(t, "foo.com. 300 IN TXT \"hello world\"")},
	}
	for i, rrs := range cases {
		payload, err := EncodeRRset(rrs)
		if err != nil {
			t.Fatalf("case %d encode: %v", i, err)
		}
		got, err := DecodeRRset(payload)
		if err != nil {
			t.Fatalf("case %d decode: %v", i, err)
		}
		if len(got.RRs) != 1 {
			t.Fatalf("case %d: len = %d", i, len(got.RRs))
		}
		// String() round trip should preserve the rdata.
		want := rrs[0].String()
		gotStr := got.RRs[0].String()
		if want != gotStr {
			t.Errorf("case %d:\n want: %s\n  got: %s", i, want, gotStr)
		}
	}
}

func TestImportZonefile(t *testing.T) {
	zonefile := `$ORIGIN foo.com.
$TTL 300
@         IN  SOA  ns1.foo.com. admin.foo.com. (1 3600 600 86400 300)
@         IN  NS   ns1.foo.com.
api       IN  A    1.2.3.4
api       IN  A    5.6.7.8
api       600 IN A    9.9.9.9
www       IN  CNAME api
`
	rrsets, err := ImportZonefile(strings.NewReader(zonefile), "foo.com.")
	if err != nil {
		t.Fatal(err)
	}
	// Find the api A RRset.
	var apiA *RRset
	for i := range rrsets {
		if rrsets[i].Key.Name == "api.foo.com." && rrsets[i].Key.Type == dns.TypeA {
			apiA = &rrsets[i]
			break
		}
	}
	if apiA == nil {
		t.Fatal("api.foo.com. A not found")
	}
	if len(apiA.RRs) != 3 {
		t.Errorf("api.foo.com. A: len = %d, want 3", len(apiA.RRs))
	}
	// Smallest TTL (300) should win.
	if apiA.TTL != 300 {
		t.Errorf("api.foo.com. A: TTL = %d, want 300", apiA.TTL)
	}
	// Encode should succeed (TTLs are normalized).
	if _, err := EncodeRRset(apiA.RRs); err != nil {
		t.Errorf("EncodeRRset after import failed: %v", err)
	}
}

func TestImportEncodeIsDeterministic(t *testing.T) {
	zonefile := `$ORIGIN foo.com.
$TTL 300
api  IN  A  5.6.7.8
api  IN  A  1.2.3.4
`
	zonefile2 := `$ORIGIN foo.com.
$TTL 300
api  IN  A  1.2.3.4
api  IN  A  5.6.7.8
`
	r1, _ := ImportZonefile(strings.NewReader(zonefile), "foo.com.")
	r2, _ := ImportZonefile(strings.NewReader(zonefile2), "foo.com.")
	p1, _ := EncodeRRset(r1[0].RRs)
	p2, _ := EncodeRRset(r2[0].RRs)
	if !bytes.Equal(p1, p2) {
		t.Fatal("zonefile order changed canonical bytes")
	}
}
