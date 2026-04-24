// Package object defines the four immutable content-addressable object
// kinds that constitute a zonegit repository: Blob, Tree, Commit, Tag.
//
// See docs/OBJECT_MODEL.md for the canonical encoding and invariants.
// This package is DNS-unaware: a Blob's payload is opaque bytes that
// pkg/zone has already canonicalized.
package object
