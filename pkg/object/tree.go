package object

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/ckumar392/zonegit/pkg/store"
)

// EntryKind distinguishes a subtree pointer from a leaf RRset pointer
// inside a Tree. The byte values are part of the on-disk canonical form;
// do NOT renumber.
type EntryKind uint8

const (
	EntrySubtree EntryKind = 0 // points to another Tree object
	EntryLeaf    EntryKind = 1 // points to a Blob (RRset)
)

// IsValid reports whether k is a defined EntryKind.
func (k EntryKind) IsValid() bool { return k == EntrySubtree || k == EntryLeaf }

// TreeEntry is one row of a Tree node.
//
//   - For EntrySubtree, Name is a single DNS label (lowercase, byte-compared)
//     and Hash points to the child Tree.
//   - For EntryLeaf, Name is a DNS RR-type mnemonic ("A", "AAAA", "MX", ...)
//     in uppercase ASCII and Hash points to a Blob.
//
// Name is constrained to 0..255 bytes by the on-disk format.
type TreeEntry struct {
	Kind EntryKind
	Name string
	Hash store.Hash
}

// Tree is a sorted list of TreeEntry. Sorting is enforced at encode time;
// callers may build entries in any order and the encoded bytes will still
// canonicalize.
type Tree struct {
	Entries []TreeEntry
}

// treeVersion is the on-disk format version. Bump only on breaking changes.
const treeVersion uint8 = 1

// Encode produces (hash, store.Object) for this tree.
//
// On-disk layout per docs/OBJECT_MODEL.md section 5:
//
//	version    : uint8
//	entry_count: uvarint
//	for each entry (sorted):
//	    kind_byte : uint8
//	    name_len  : uint8
//	    name      : []byte
//	    hash      : [32]byte
//
// Sort order is (kind ASC, name ASC). All subtrees come before leaves at
// the same level because EntrySubtree (0) < EntryLeaf (1).
func (t Tree) Encode() (store.Hash, store.Object) {
	entries := append([]TreeEntry(nil), t.Entries...)
	sortEntries(entries)

	var buf bytes.Buffer
	buf.WriteByte(treeVersion)
	var n [binary.MaxVarintLen64]byte
	buf.Write(n[:binary.PutUvarint(n[:], uint64(len(entries)))])
	for _, e := range entries {
		buf.WriteByte(byte(e.Kind))
		// name_len is a single byte; entries with longer names are rejected
		// upstream by validateEntry.
		buf.WriteByte(byte(len(e.Name)))
		buf.WriteString(e.Name)
		buf.Write(e.Hash[:])
	}
	payload := buf.Bytes()
	return Encode(KindTree, payload)
}

// DecodeTree parses a tree payload. It validates structure and sort order:
// trees that decode but were not produced by Encode (e.g. unsorted) are
// rejected, since accepting them would let two trees with the same logical
// content have two different hashes.
func DecodeTree(payload []byte) (Tree, error) {
	if len(payload) < 1 {
		return Tree{}, errInvalid("tree: empty payload")
	}
	if payload[0] != treeVersion {
		return Tree{}, errInvalid("tree: unsupported version %d", payload[0])
	}
	rest := payload[1:]
	count64, n := binary.Uvarint(rest)
	if n <= 0 {
		return Tree{}, errInvalid("tree: bad entry count varint")
	}
	rest = rest[n:]
	entries := make([]TreeEntry, 0, count64)
	for i := uint64(0); i < count64; i++ {
		if len(rest) < 2 {
			return Tree{}, errInvalid("tree: truncated entry header at %d", i)
		}
		kind := EntryKind(rest[0])
		nameLen := int(rest[1])
		rest = rest[2:]
		if !kind.IsValid() {
			return Tree{}, errInvalid("tree: invalid entry kind %d at %d", kind, i)
		}
		if len(rest) < nameLen+store.HashSize {
			return Tree{}, errInvalid("tree: truncated entry body at %d", i)
		}
		name := string(rest[:nameLen])
		rest = rest[nameLen:]
		var h store.Hash
		copy(h[:], rest[:store.HashSize])
		rest = rest[store.HashSize:]
		entries = append(entries, TreeEntry{Kind: kind, Name: name, Hash: h})
	}
	if len(rest) != 0 {
		return Tree{}, errInvalid("tree: %d trailing bytes", len(rest))
	}
	if !isSorted(entries) {
		return Tree{}, errInvalid("tree: entries not in canonical sort order")
	}
	if dup := firstDuplicate(entries); dup != "" {
		return Tree{}, errInvalid("tree: duplicate entry %s", dup)
	}
	return Tree{Entries: entries}, nil
}

// Get returns the entry with (kind, name) and reports whether it exists.
// Lookup is O(log n) since entries are kept sorted by Encode/Decode.
func (t Tree) Get(kind EntryKind, name string) (TreeEntry, bool) {
	idx := sort.Search(len(t.Entries), func(i int) bool {
		return cmpEntry(t.Entries[i].Kind, t.Entries[i].Name, kind, name) >= 0
	})
	if idx < len(t.Entries) {
		e := t.Entries[idx]
		if e.Kind == kind && e.Name == name {
			return e, true
		}
	}
	return TreeEntry{}, false
}

// validateEntry enforces the on-disk format limits.
func validateEntry(e TreeEntry) error {
	if !e.Kind.IsValid() {
		return errInvalid("tree entry: invalid kind %d", e.Kind)
	}
	if len(e.Name) > 255 {
		return errInvalid("tree entry: name too long (%d > 255)", len(e.Name))
	}
	if len(e.Name) == 0 {
		return errInvalid("tree entry: empty name")
	}
	return nil
}

// sortEntries sorts entries in canonical order: kind ASC, then name ASC
// (compared as raw bytes).
func sortEntries(entries []TreeEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return cmpEntry(entries[i].Kind, entries[i].Name, entries[j].Kind, entries[j].Name) < 0
	})
}

// cmpEntry returns -1/0/+1 comparing (ka, na) to (kb, nb).
func cmpEntry(ka EntryKind, na string, kb EntryKind, nb string) int {
	switch {
	case ka < kb:
		return -1
	case ka > kb:
		return 1
	}
	switch {
	case na < nb:
		return -1
	case na > nb:
		return 1
	}
	return 0
}

func isSorted(entries []TreeEntry) bool {
	for i := 1; i < len(entries); i++ {
		if cmpEntry(entries[i-1].Kind, entries[i-1].Name, entries[i].Kind, entries[i].Name) > 0 {
			return false
		}
	}
	return true
}

func firstDuplicate(entries []TreeEntry) string {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Kind == entries[i].Kind && entries[i-1].Name == entries[i].Name {
			return entries[i].Name
		}
	}
	return ""
}
