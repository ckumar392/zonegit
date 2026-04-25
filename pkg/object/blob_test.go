package object

import (
	"bytes"
	"testing"
)

// I3 (round-trip): DecodeBlob ∘ Encode is identity over Blob.Payload.
func TestInvariant_BlobRoundTrip(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		nil,
		{},
		[]byte("a"),
		bytes.Repeat([]byte{0xff}, 4096),
		[]byte("\x00\x01\x02\x03\x00"),
	}
	for _, payload := range cases {
		b := DecodeBlob(payload)
		_, obj := b.Encode()
		if !bytes.Equal(obj.Payload, payload) && (payload != nil || len(obj.Payload) != 0) {
			t.Fatalf("round-trip mismatch: in=%x out=%x", payload, obj.Payload)
		}
	}
}

// DecodeBlob returns a defensive copy so the caller cannot mutate our state.
func TestBlobDecodeIsDefensiveCopy(t *testing.T) {
	t.Parallel()
	orig := []byte{1, 2, 3}
	b := DecodeBlob(orig)
	orig[0] = 0xff
	if b.Payload[0] == 0xff {
		t.Fatal("Blob.Payload aliased input slice")
	}
}

// Two blobs with identical payloads hash identically; differing payloads
// hash differently (deduplication property).
func TestBlobDedup(t *testing.T) {
	t.Parallel()
	b1 := DecodeBlob([]byte("same"))
	b2 := DecodeBlob([]byte("same"))
	b3 := DecodeBlob([]byte("diff"))
	h1, _ := b1.Encode()
	h2, _ := b2.Encode()
	h3, _ := b3.Encode()
	if h1 != h2 {
		t.Fatal("identical payloads must hash equally")
	}
	if h1 == h3 {
		t.Fatal("different payloads must not hash equally")
	}
}
