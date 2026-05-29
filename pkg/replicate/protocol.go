// Package replicate implements pull-mode replication of a zonegit
// repository from a primary daemon to one or more secondaries.
//
// The protocol is intentionally simple: it's a content-addressed
// Merkle-walk over plain HTTP. A secondary asks the primary "which
// branches do you have?", then for each branch where its local hash
// differs, asks "what objects do you have reachable from <root> that
// I don't have given I already have <known>?", then fetches each
// missing object by its content hash and finally moves the local ref
// to match.
//
// Why HTTP and not gRPC: gRPC pulls in ~150 transitive modules and a
// protobuf compiler step. Plain HTTP suffices here — every object body
// is identified by its SHA-256 hash, so transport correctness is
// verifiable end-to-end without a schema. A streaming-gRPC backend can
// be added behind the same Client interface if throughput becomes the
// constraint.
//
// Endpoint layout:
//
//	GET  /v0/refs                  → JSON: {zones, branches}
//	POST /v0/objects/walk          → JSON: {missing: []hash}
//	GET  /v0/objects/<hex-hash>    → binary object body; X-Object-Kind header
//
// The protocol is one-way: clients never push to primaries.
package replicate

import (
	"encoding/hex"

	"github.com/ckumar392/zonegit/pkg/store"
)

// BasePath is the URL prefix every replication endpoint sits under.
// Kept as a constant so client and server agree byte-for-byte.
const BasePath = "/v0"

// Branch identifies one (zone, name) pair plus the commit hash it
// points at. Exchanged in the /v0/refs response so the secondary can
// compute which branches have diverged.
type Branch struct {
	Zone string `json:"zone"`
	Name string `json:"name"`
	Hash string `json:"hash"` // hex-encoded
}

// RefsResponse is the body returned by GET /v0/refs.
type RefsResponse struct {
	Zones    []string `json:"zones"`
	Branches []Branch `json:"branches"`
}

// WalkRequest is the body POSTed to /v0/objects/walk.
//
// Roots are the object hashes the secondary wants to reach (typically
// the primary's branch tips after diffing /v0/refs).
//
// Known are object hashes the secondary already has in its local
// store. The server expands Known into its full reachable set, then
// walks Roots and returns whatever the secondary still needs.
type WalkRequest struct {
	Roots []string `json:"roots"` // hex
	Known []string `json:"known"` // hex
}

// WalkResponse is the body returned by /v0/objects/walk. Hashes are
// in arbitrary order; clients can fetch them in any order since each
// object is content-addressable on its own.
type WalkResponse struct {
	Missing []string `json:"missing"` // hex
}

func hashesToHex(hs []store.Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = hex.EncodeToString(h[:])
	}
	return out
}

func hexesToHashes(in []string) ([]store.Hash, error) {
	out := make([]store.Hash, len(in))
	for i, s := range in {
		h, err := store.ParseHash(s)
		if err != nil {
			return nil, err
		}
		out[i] = h
	}
	return out, nil
}
