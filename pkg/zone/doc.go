// Package zone bridges miekg/dns RR types and pkg/object Blob payloads.
//
// It owns:
//   - The canonical RRset byte format (per docs/OBJECT_MODEL.md §4).
//   - RFC 1035 zonefile import/export via miekg/dns.
//
// Everything DNS-aware lives here. pkg/object is intentionally kept
// DNS-unaware so that the canonical-encoding rules can evolve without
// touching the hash/object machinery.
package zone
