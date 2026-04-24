package object

import (
	"strings"
	"testing"

	"github.com/ckumar392/zonegit/pkg/store"
)

// I1 (determinism): HashOf is a pure function of (kind, payload).
func TestInvariant_HashOfDeterministic(t *testing.T) {
	t.Parallel()
	a := HashOf(KindBlob, []byte("hello world"))
	b := HashOf(KindBlob, []byte("hello world"))
	if a != b {
		t.Fatalf("HashOf not deterministic: %s vs %s", a, b)
	}
}

// Hash differs across kinds for the same payload (the header is part of
// the hashed bytes, by design — prevents trivial cross-kind collisions).
func TestHashSeparatesKinds(t *testing.T) {
	t.Parallel()
	payload := []byte("payload")
	hb := HashOf(KindBlob, payload)
	ht := HashOf(KindTree, payload)
	hc := HashOf(KindCommit, payload)
	hg := HashOf(KindTag, payload)
	seen := map[store.Hash]Kind{hb: KindBlob, ht: KindTree, hc: KindCommit, hg: KindTag}
	if len(seen) != 4 {
		t.Fatalf("expected 4 distinct hashes for 4 kinds, got %d", len(seen))
	}
}

// Empty payload still hashes deterministically and is non-zero.
func TestHashEmptyPayload(t *testing.T) {
	t.Parallel()
	h := HashOf(KindBlob, nil)
	if h.IsZero() {
		t.Fatal("hash of empty blob must not be zero")
	}
	if HashOf(KindBlob, []byte{}) != h {
		t.Fatal("nil and empty slice must hash equally")
	}
}

// Verify accepts well-formed objects and rejects hash mismatches.
func TestVerify(t *testing.T) {
	t.Parallel()
	payload := []byte("rrset bytes")
	h, obj := Encode(KindBlob, payload)
	if err := Verify(h, obj); err != nil {
		t.Fatalf("Verify on good object: %v", err)
	}

	// Tamper with the payload.
	bad := store.Object{Kind: obj.Kind, Payload: []byte("tampered")}
	if err := Verify(h, bad); err == nil {
		t.Fatal("Verify must reject tampered payload")
	} else if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt error, got %v", err)
	}

	// Invalid kind.
	weird := store.Object{Kind: "weird", Payload: payload}
	if err := Verify(h, weird); err == nil {
		t.Fatal("Verify must reject invalid kind")
	}
}

// Kind.IsValid covers the four kinds and rejects anything else.
func TestKindIsValid(t *testing.T) {
	t.Parallel()
	for _, k := range []Kind{KindBlob, KindTree, KindCommit, KindTag} {
		if !k.IsValid() {
			t.Errorf("expected %q valid", k)
		}
	}
	if Kind("nope").IsValid() {
		t.Error("expected 'nope' invalid")
	}
}
