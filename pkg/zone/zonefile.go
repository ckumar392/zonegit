package zone

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/miekg/dns"
)

// ImportZonefile parses an RFC 1035 zonefile and groups its RRs into RRsets.
// `origin` is the default owner (e.g. "foo.com.") used for relative names.
//
// All RRs sharing (owner, class, type) are merged into one RRset. If their
// TTLs differ, the smallest is chosen (matching common DNS server behavior).
func ImportZonefile(r io.Reader, origin string) ([]RRset, error) {
	if !strings.HasSuffix(origin, ".") {
		origin += "."
	}
	groups := make(map[RRsetKey][]dns.RR)

	zp := dns.NewZoneParser(r, origin, "")
	zp.SetDefaultTTL(3600)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		h := rr.Header()
		key := RRsetKey{
			Name:  canonicalName(h.Name),
			Class: h.Class,
			Type:  h.Rrtype,
		}
		groups[key] = append(groups[key], rr)
	}
	if err := zp.Err(); err != nil {
		return nil, fmt.Errorf("zone: parse: %w", err)
	}

	out := make([]RRset, 0, len(groups))
	for k, rrs := range groups {
		// Pick the smallest TTL and rewrite all RR headers to match so
		// EncodeRRset's homogeneity check passes.
		minTTL := rrs[0].Header().Ttl
		for _, rr := range rrs[1:] {
			if rr.Header().Ttl < minTTL {
				minTTL = rr.Header().Ttl
			}
		}
		for _, rr := range rrs {
			rr.Header().Ttl = minTTL
			rr.Header().Name = k.Name // canonical form
		}
		out = append(out, RRset{Key: k, TTL: minTTL, RRs: rrs})
	}

	// Stable order for callers (helps tests and diffs).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Name != out[j].Key.Name {
			return out[i].Key.Name < out[j].Key.Name
		}
		if out[i].Key.Class != out[j].Key.Class {
			return out[i].Key.Class < out[j].Key.Class
		}
		return out[i].Key.Type < out[j].Key.Type
	})
	return out, nil
}

// ExportZonefile writes RRsets as a textual zonefile.
func ExportZonefile(w io.Writer, rrsets []RRset) error {
	for _, rs := range rrsets {
		for _, rr := range rs.RRs {
			if _, err := fmt.Fprintln(w, rr.String()); err != nil {
				return err
			}
		}
	}
	return nil
}
