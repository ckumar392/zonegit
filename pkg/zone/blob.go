package zone

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/miekg/dns"
)

// blobVersion is the canonical-payload version byte. See OBJECT_MODEL.md §4.
const blobVersion uint8 = 1

// RRsetKey identifies an RRset by (owner, class, type). Owner is canonical
// (lowercase, fully-qualified, trailing dot).
type RRsetKey struct {
	Name  string
	Class uint16
	Type  uint16
}

// String renders the key in dig-style: "api.foo.com. IN A".
func (k RRsetKey) String() string {
	return fmt.Sprintf("%s %s %s", k.Name, dns.ClassToString[k.Class], dns.TypeToString[k.Type])
}

// RRset is a logical grouping of RRs sharing (owner, class, type) plus a
// shared TTL. miekg/dns RR.Header().Ttl is the source of truth at encode time;
// we collapse to the first RR's TTL (callers must homogenize beforehand).
type RRset struct {
	Key RRsetKey
	TTL uint32
	RRs []dns.RR // all share the same Header() name/class/type
}

// EncodeRRset serializes the canonical bytes of an RRset.
//
// Format (see docs/OBJECT_MODEL.md §4):
//
//	version  uint8
//	owner    DNS name in RFC 4034 canonical wire form (lowercased, length-prefixed)
//	class    uint16 BE
//	type     uint16 BE
//	ttl      uint32 BE
//	rr_count uint16 BE
//	rr_recs  sorted [count]{ length:uint16 BE, bytes:[]byte }
//
// Sorting is by raw rdata bytes, lexicographically. This makes
// {1.2.3.4, 5.6.7.8} and {5.6.7.8, 1.2.3.4} hash identically.
func EncodeRRset(rrs []dns.RR) ([]byte, error) {
	if len(rrs) == 0 {
		return nil, fmt.Errorf("zone: cannot encode empty RRset")
	}
	hdr := rrs[0].Header()
	owner := canonicalName(hdr.Name)
	class := hdr.Class
	rrtype := hdr.Rrtype
	ttl := hdr.Ttl

	// Validate homogeneity and pack each rdata.
	rdatas := make([][]byte, 0, len(rrs))
	for i, rr := range rrs {
		h := rr.Header()
		if canonicalName(h.Name) != owner {
			return nil, fmt.Errorf("zone: heterogeneous owner: %q vs %q", h.Name, hdr.Name)
		}
		if h.Class != class {
			return nil, fmt.Errorf("zone: heterogeneous class at idx %d", i)
		}
		if h.Rrtype != rrtype {
			return nil, fmt.Errorf("zone: heterogeneous type at idx %d", i)
		}
		if h.Ttl != ttl {
			return nil, fmt.Errorf("zone: heterogeneous TTL at idx %d (%d vs %d) — caller must normalize", i, h.Ttl, ttl)
		}
		rd, err := packRdata(rr)
		if err != nil {
			return nil, fmt.Errorf("zone: pack rdata idx %d: %w", i, err)
		}
		rdatas = append(rdatas, rd)
	}

	// Sort lexicographically by rdata bytes — canonical order.
	sort.Slice(rdatas, func(i, j int) bool { return bytes.Compare(rdatas[i], rdatas[j]) < 0 })

	// Encode owner as RFC 1035 wire-form labels (length-prefixed, terminating
	// zero label). dns.PackDomainName gives us exactly that.
	ownerWire := make([]byte, 256)
	n, err := dns.PackDomainName(owner, ownerWire, 0, nil, false)
	if err != nil {
		return nil, fmt.Errorf("zone: pack owner %q: %w", owner, err)
	}
	ownerWire = ownerWire[:n]

	var buf bytes.Buffer
	buf.WriteByte(blobVersion)
	buf.Write(ownerWire)
	_ = binary.Write(&buf, binary.BigEndian, class)
	_ = binary.Write(&buf, binary.BigEndian, rrtype)
	_ = binary.Write(&buf, binary.BigEndian, ttl)
	if len(rdatas) > 0xffff {
		return nil, fmt.Errorf("zone: too many RRs in RRset: %d", len(rdatas))
	}
	_ = binary.Write(&buf, binary.BigEndian, uint16(len(rdatas)))
	for _, rd := range rdatas {
		if len(rd) > 0xffff {
			return nil, fmt.Errorf("zone: rdata too long: %d", len(rd))
		}
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(rd)))
		buf.Write(rd)
	}
	return buf.Bytes(), nil
}

// DecodeRRset reverses EncodeRRset.
func DecodeRRset(payload []byte) (RRset, error) {
	if len(payload) < 1 {
		return RRset{}, fmt.Errorf("zone: empty payload")
	}
	if payload[0] != blobVersion {
		return RRset{}, fmt.Errorf("zone: unsupported blob version %d", payload[0])
	}
	off := 1

	owner, n, err := dns.UnpackDomainName(payload, off)
	if err != nil {
		return RRset{}, fmt.Errorf("zone: unpack owner: %w", err)
	}
	off = n

	if len(payload) < off+10 {
		return RRset{}, fmt.Errorf("zone: payload truncated at header")
	}
	class := binary.BigEndian.Uint16(payload[off:])
	off += 2
	rrtype := binary.BigEndian.Uint16(payload[off:])
	off += 2
	ttl := binary.BigEndian.Uint32(payload[off:])
	off += 4
	count := binary.BigEndian.Uint16(payload[off:])
	off += 2

	rrs := make([]dns.RR, 0, count)
	for i := 0; i < int(count); i++ {
		if len(payload) < off+2 {
			return RRset{}, fmt.Errorf("zone: truncated rdata length at idx %d", i)
		}
		ln := binary.BigEndian.Uint16(payload[off:])
		off += 2
		if len(payload) < off+int(ln) {
			return RRset{}, fmt.Errorf("zone: truncated rdata at idx %d (need %d, have %d)", i, ln, len(payload)-off)
		}
		rdata := payload[off : off+int(ln)]
		off += int(ln)

		rr, err := unpackRdata(owner, rrtype, class, ttl, rdata)
		if err != nil {
			return RRset{}, fmt.Errorf("zone: unpack rdata idx %d: %w", i, err)
		}
		rrs = append(rrs, rr)
	}
	if off != len(payload) {
		return RRset{}, fmt.Errorf("zone: trailing bytes (%d unread)", len(payload)-off)
	}

	return RRset{
		Key: RRsetKey{Name: owner, Class: class, Type: rrtype},
		TTL: ttl,
		RRs: rrs,
	}, nil
}

// canonicalName returns the lowercase fully-qualified form (always ends in ".").
func canonicalName(s string) string {
	s = strings.ToLower(s)
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// packRdata returns just the rdata bytes (no owner / type / class / ttl / rdlen
// prefix) by building a full RR wire image and slicing off the header.
func packRdata(rr dns.RR) ([]byte, error) {
	// dns.PackRR writes the full RR; we slice off the header. The header
	// length depends on owner-name compression, so disable compression.
	buf := make([]byte, dns.Len(rr)+32) // some slack for safety
	off, err := dns.PackRR(rr, buf, 0, nil, false)
	if err != nil {
		return nil, err
	}
	full := buf[:off]

	// Re-walk the header to find rdata offset.
	_, hdrEnd, err := dns.UnpackDomainName(full, 0)
	if err != nil {
		return nil, fmt.Errorf("walk owner: %w", err)
	}
	// type(2) + class(2) + ttl(4) + rdlen(2) = 10
	hdrEnd += 10
	if hdrEnd > len(full) {
		return nil, fmt.Errorf("header walk past end")
	}
	return append([]byte(nil), full[hdrEnd:]...), nil
}

// unpackRdata reconstructs a single dns.RR from canonical rdata bytes plus
// its (owner, type, class, ttl). It does this by synthesizing a full wire
// RR image and feeding it to dns.UnpackRR.
func unpackRdata(owner string, rrtype, class uint16, ttl uint32, rdata []byte) (dns.RR, error) {
	// Build: <owner-wire> <type:2> <class:2> <ttl:4> <rdlen:2> <rdata...>
	ownerBuf := make([]byte, 256)
	n, err := dns.PackDomainName(owner, ownerBuf, 0, nil, false)
	if err != nil {
		return nil, fmt.Errorf("pack owner: %w", err)
	}
	wire := make([]byte, 0, n+10+len(rdata))
	wire = append(wire, ownerBuf[:n]...)
	var hdr [10]byte
	binary.BigEndian.PutUint16(hdr[0:], rrtype)
	binary.BigEndian.PutUint16(hdr[2:], class)
	binary.BigEndian.PutUint32(hdr[4:], ttl)
	binary.BigEndian.PutUint16(hdr[8:], uint16(len(rdata)))
	wire = append(wire, hdr[:]...)
	wire = append(wire, rdata...)

	rr, _, err := dns.UnpackRR(wire, 0)
	if err != nil {
		return nil, err
	}
	return rr, nil
}
