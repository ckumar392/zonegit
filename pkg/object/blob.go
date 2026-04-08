package object

import "github.com/ckumar392/dnsdb/pkg/store"

// Blob is the content-addressable form of one RRset.
//
// Payload is the canonical RRset bytes per docs/OBJECT_MODEL.md section 4.
// pkg/object treats Payload as opaque: it is the responsibility of pkg/zone
// to produce the canonical byte form (lowercase owner, sorted rdata, etc.).
// This split keeps pkg/object DNS-unaware and trivially testable.
type Blob struct {
	Payload []byte
}

// Encode returns the hash and store.Object for this blob.
func (b Blob) Encode() (store.Hash, store.Object) {
	return Encode(KindBlob, b.Payload)
}

// DecodeBlob lifts a payload back into a Blob. It cannot fail because the
// payload is opaque; format errors surface only when pkg/zone tries to
// parse it as an RRset.
func DecodeBlob(payload []byte) Blob {
	// Defensive copy so the caller can mutate `payload` without affecting us.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	return Blob{Payload: cp}
}
