package zone

import (
	"testing"

	"github.com/miekg/dns"
)

func TestBumpSOASerial(t *testing.T) {
	soaRR, err := dns.NewRR("foo.com. 300 IN SOA ns1.foo.com. admin.foo.com. 7 7200 3600 1209600 300")
	if err != nil {
		t.Fatal(err)
	}
	in := RRset{
		Key: RRsetKey{Name: "foo.com.", Class: dns.ClassINET, Type: dns.TypeSOA},
		TTL: 300,
		RRs: []dns.RR{soaRR},
	}
	out, err := BumpSOASerial(in)
	if err != nil {
		t.Fatalf("BumpSOASerial: %v", err)
	}
	got := out.RRs[0].(*dns.SOA).Serial
	if got != 8 {
		t.Fatalf("serial: want 8, got %d", got)
	}
	// Input must not be mutated.
	if in.RRs[0].(*dns.SOA).Serial != 7 {
		t.Fatalf("input was mutated: want 7, got %d", in.RRs[0].(*dns.SOA).Serial)
	}
}

func TestBumpSOASerial_WrapsAt32Bits(t *testing.T) {
	soaRR, _ := dns.NewRR("foo.com. 300 IN SOA ns1.foo.com. admin.foo.com. 4294967295 7200 3600 1209600 300")
	out, err := BumpSOASerial(RRset{RRs: []dns.RR{soaRR}})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.RRs[0].(*dns.SOA).Serial; got != 0 {
		t.Fatalf("uint32 wraparound: want 0, got %d", got)
	}
}

func TestBumpSOASerial_RejectsNonSOA(t *testing.T) {
	a, _ := dns.NewRR("api.foo.com. 300 IN A 1.2.3.4")
	if _, err := BumpSOASerial(RRset{RRs: []dns.RR{a}}); err == nil {
		t.Fatal("expected error on non-SOA, got nil")
	}
}
