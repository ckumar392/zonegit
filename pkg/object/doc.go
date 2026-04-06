// Package object defines the four immutable content-addressable object
// kinds that constitute a dnsdb repository: Blob (RRset), Tree (zone
// snapshot), Commit (zone version), and Tag (named pointer).
//
// See docs/OBJECT_MODEL.md for the canonical encoding and the seven
// invariants this package must uphold.
package object

// Kind is the type tag of a content-addressable object.
type Kind string

const (
	KindBlob   Kind = "blob"
	KindTree   Kind = "tree"
	KindCommit Kind = "commit"
	KindTag    Kind = "tag"
)
