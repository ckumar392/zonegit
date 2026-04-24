// Package store defines the only persistence seam in zonegit.
//
// Every zonegit storage backend (in-memory, Badger, Postgres, ...) implements
// the Storage interface. Nothing above this package knows or cares which
// backend is in use. See docs/ARCHITECTURE.md section 2 for the design rationale.
package store

import (
	"context"
	"errors"
)

// HashSize is the length of an object hash in bytes (SHA-256).
const HashSize = 32

// Hash is the content address of an immutable object.
type Hash [HashSize]byte

// Object is the kind+payload of a content-addressable object.
//
// Payload is the canonical bytes WITHOUT the "<kind> <length>\x00" header
// described in docs/OBJECT_MODEL.md section 3. The header is regenerated on
// the fly during hashing so the on-disk representation can be optimized per
// backend without changing the hash.
type Object struct {
	Kind    string // "blob" | "tree" | "commit" | "tag"
	Payload []byte
}

// RefEntry is a single (name -> hash) ref mapping.
type RefEntry struct {
	Name string
	Hash Hash
}

// ReflogEntry records one ref movement for forensic recovery.
type ReflogEntry struct {
	Old      Hash
	New      Hash
	Author   string // "name <email>"
	UnixTime int64
	TZOffset int    // minutes east of UTC
	Op       string // "commit" | "merge" | "revert" | "reset" | "branch" | ...
	Message  string
}

// Storage is the contract every backend implements.
//
// Implementations MUST be safe for concurrent use. Object writes are
// idempotent: writing the same hash twice is not an error. Ref updates are
// atomic via CAS.
type Storage interface {
	// Object CAS (immutable).
	PutObject(ctx context.Context, h Hash, o Object) error
	GetObject(ctx context.Context, h Hash) (Object, error)
	HasObject(ctx context.Context, h Hash) (bool, error)
	IterObjects(ctx context.Context, fn func(Hash, Object) error) error

	// Refs (mutable, atomic).
	GetRef(ctx context.Context, name string) (h Hash, ok bool, err error)
	// CASRef updates name from expected to next iff the current value still
	// equals expected. A zero expected Hash means "create only if absent".
	CASRef(ctx context.Context, name string, expected, next Hash) error
	DeleteRef(ctx context.Context, name string, expected Hash) error
	ListRefs(ctx context.Context, prefix string) ([]RefEntry, error)

	// Reflog (append-only).
	AppendReflog(ctx context.Context, name string, e ReflogEntry) error
	ReadReflog(ctx context.Context, name string) ([]ReflogEntry, error)

	Close() error
}

// Sentinel errors. Implementations MUST return these (wrapped with
// fmt.Errorf("...: %w", ...)) so callers can errors.Is against them.
var (
	// ErrNotFound is returned by Get* when the object or ref does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned by CASRef when the expected value did not match.
	ErrConflict = errors.New("ref cas conflict")
	// ErrCorrupt is returned when a stored object's bytes do not match its hash.
	ErrCorrupt = errors.New("corrupt object")
	// ErrInvalidObject is returned when an object's kind or payload is malformed.
	ErrInvalidObject = errors.New("invalid object")
)
