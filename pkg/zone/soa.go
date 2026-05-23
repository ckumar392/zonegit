package zone

import (
	"fmt"

	"github.com/miekg/dns"
)

// BumpSOASerial returns a new RRset identical to soa but with the SOA's
// Serial field incremented by 1 (mod 2^32 per RFC 1982 serial arithmetic).
//
// The input must be an apex SOA RRset (exactly one RR, type SOA). Anything
// else is an error: SOA is by definition single-record, and bumping a
// non-SOA RRset makes no sense.
//
// Callers re-encode the returned RRset via EncodeRRset and re-stage it on
// the next commit, which is what pkg/repo.Commit does automatically when
// other RRsets change.
func BumpSOASerial(soa RRset) (RRset, error) {
	if len(soa.RRs) != 1 {
		return RRset{}, fmt.Errorf("BumpSOASerial: expected exactly 1 RR, got %d", len(soa.RRs))
	}
	rec, ok := soa.RRs[0].(*dns.SOA)
	if !ok {
		return RRset{}, fmt.Errorf("BumpSOASerial: expected *dns.SOA, got %T", soa.RRs[0])
	}
	// Copy so we don't mutate the caller's RR.
	cp := *rec
	cp.Serial = cp.Serial + 1 // uint32 wraparound is the RFC 1982 expectation
	out := soa
	out.RRs = []dns.RR{&cp}
	return out, nil
}
