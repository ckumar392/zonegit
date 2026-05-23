package object

import (
	"crypto/sha256"
	"fmt"

	"github.com/ckumar392/zonegit/pkg/store"
)

// Kind is the type tag of a content-addressable object. The string values
// match the on-disk header tag in docs/OBJECT_MODEL.md section 3.
type Kind string

const (
	KindBlob   Kind = "blob"
	KindTree   Kind = "tree"
	KindCommit Kind = "commit"
	KindTag    Kind = "tag"
	// KindSymref is the object kind for symbolic refs (HEAD). Payload is
	// the raw UTF-8 target ref path (e.g. "refs/heads/foo.com./main").
	// Stored via the object store so HEAD targets are not constrained by
	// the 31-byte ref-value slot.
	KindSymref Kind = "symref"
)

// IsValid reports whether k is one of the canonical kinds.
func (k Kind) IsValid() bool {
	switch k {
	case KindBlob, KindTree, KindCommit, KindTag, KindSymref:
		return true
	}
	return false
}

// HashOf computes the content address for (kind, payload):
//
//	SHA256( "<kind> <decimal-len>\x00" || payload )
//
// This is the single source of truth for object identity.
func HashOf(kind Kind, payload []byte) store.Hash {
	h := sha256.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(payload))
	h.Write(payload)
	var out store.Hash
	copy(out[:], h.Sum(nil))
	return out
}

// Encode is a small convenience: returns both the hash and a store.Object
// ready to pass to PutObject.
func Encode(kind Kind, payload []byte) (store.Hash, store.Object) {
	return HashOf(kind, payload), store.Object{Kind: string(kind), Payload: payload}
}

// Verify checks that obj's bytes hash to h. It is the caller's only
// defense against silent on-disk corruption.
func Verify(h store.Hash, obj store.Object) error {
	if !Kind(obj.Kind).IsValid() {
		return fmt.Errorf("kind %q: %w", obj.Kind, store.ErrInvalidObject)
	}
	got := HashOf(Kind(obj.Kind), obj.Payload)
	if got != h {
		return fmt.Errorf("expected %s, got %s: %w", h.Short(), got.Short(), store.ErrCorrupt)
	}
	return nil
}

func errInvalid(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), store.ErrInvalidObject)
}
