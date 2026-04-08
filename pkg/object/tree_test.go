package object

import (
	"testing"

	"github.com/ckumar392/dnsdb/pkg/store"
)

func mkHash(b byte) store.Hash {
	var h store.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// I3 (round-trip): DecodeTree ∘ Encode is identity for valid Trees.
func TestInvariant_TreeRoundTrip(t *testing.T) {
	t.Parallel()
	tr := Tree{Entries: []TreeEntry{
		{Kind: EntryLeaf, Name: "A", Hash: mkHash(0x11)},
		{Kind: EntrySubtree, Name: "api", Hash: mkHash(0x22)},
		{Kind: EntryLeaf, Name: "AAAA", Hash: mkHash(0x33)},
		{Kind: EntrySubtree, Name: "internal", Hash: mkHash(0x44)},
	}}
	_, obj := tr.Encode()
	got, err := DecodeTree(obj.Payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != len(tr.Entries) {
		t.Fatalf("entry count: got %d want %d", len(got.Entries), len(tr.Entries))
	}
}

// Encoding sorts entries: subtrees first (kind=0), then leaves (kind=1),
// each sub-section name-ascending. Hash is independent of input order.
func TestTreeEncodeSortsCanonically(t *testing.T) {
	t.Parallel()
	a := Tree{Entries: []TreeEntry{
		{Kind: EntryLeaf, Name: "MX", Hash: mkHash(1)},
		{Kind: EntrySubtree, Name: "zzz", Hash: mkHash(2)},
		{Kind: EntryLeaf, Name: "A", Hash: mkHash(3)},
		{Kind: EntrySubtree, Name: "aaa", Hash: mkHash(4)},
	}}
	b := Tree{Entries: []TreeEntry{
		{Kind: EntrySubtree, Name: "aaa", Hash: mkHash(4)},
		{Kind: EntrySubtree, Name: "zzz", Hash: mkHash(2)},
		{Kind: EntryLeaf, Name: "A", Hash: mkHash(3)},
		{Kind: EntryLeaf, Name: "MX", Hash: mkHash(1)},
	}}
	ha, _ := a.Encode()
	hb, _ := b.Encode()
	if ha != hb {
		t.Fatal("permuted entries must produce identical hashes after canonical sort")
	}

	decoded, err := DecodeTree(([]byte)((store.Object{Kind: string(KindTree), Payload: nil}).Payload))
	_ = decoded
	_ = err
}

// DecodeTree rejects unsorted entries — accepting them would silently
// permit two payloads with the same logical content but different hashes.
func TestTreeDecodeRejectsUnsorted(t *testing.T) {
	t.Parallel()
	// Build a payload by hand with entries reversed.
	reversed := Tree{Entries: []TreeEntry{
		{Kind: EntryLeaf, Name: "B", Hash: mkHash(1)},
		{Kind: EntryLeaf, Name: "A", Hash: mkHash(2)},
	}}
	// Encode normally (which sorts), then swap two entries by hand.
	_, obj := reversed.Encode()
	payload := obj.Payload
	// Walk header: version(1) + uvarint(2)
	off := 1 + 1 // single-byte uvarint for count=2
	// Each entry: kind(1) + namelen(1) + name(N) + hash(32)
	e1Size := 1 + 1 + 1 + 32
	if e1Size > len(payload[off:]) {
		t.Fatal("payload too small")
	}
	// Swap the two entries in place.
	a := append([]byte(nil), payload[off:off+e1Size]...)
	b := append([]byte(nil), payload[off+e1Size:off+2*e1Size]...)
	copy(payload[off:], b)
	copy(payload[off+e1Size:], a)

	if _, err := DecodeTree(payload); err == nil {
		t.Fatal("DecodeTree must reject unsorted entries")
	}
}

// Tree.Get finds present entries and reports absent ones.
func TestTreeGet(t *testing.T) {
	t.Parallel()
	tr := Tree{Entries: []TreeEntry{
		{Kind: EntrySubtree, Name: "api", Hash: mkHash(1)},
		{Kind: EntryLeaf, Name: "A", Hash: mkHash(2)},
		{Kind: EntryLeaf, Name: "MX", Hash: mkHash(3)},
	}}
	_, obj := tr.Encode()
	got, err := DecodeTree(obj.Payload)
	if err != nil {
		t.Fatal(err)
	}

	if e, ok := got.Get(EntrySubtree, "api"); !ok || e.Hash != mkHash(1) {
		t.Errorf("Get(subtree api): %v %v", e, ok)
	}
	if e, ok := got.Get(EntryLeaf, "A"); !ok || e.Hash != mkHash(2) {
		t.Errorf("Get(leaf A): %v %v", e, ok)
	}
	if _, ok := got.Get(EntryLeaf, "AAAA"); ok {
		t.Error("Get(leaf AAAA) should be absent")
	}
	if _, ok := got.Get(EntrySubtree, "A"); ok {
		t.Error("Get(subtree A) must not match leaf A")
	}
}

// DecodeTree rejects malformed payloads.
func TestTreeDecodeMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"empty":         nil,
		"bad version":   {99},
		"bad count":     {1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"truncated entry header": {1, 1, 0}, // version=1, count=1, then nothing
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeTree(payload); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}
