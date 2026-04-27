package merge

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
)

// ConflictReason classifies why a 3-way merge could not auto-resolve a leaf.
type ConflictReason int

const (
	// ReasonBothModified: base, ours, and theirs are three distinct leaves.
	// At v1 we do not attempt member-level RRset merge; any divergent
	// modification of the same leaf is a conflict.
	ReasonBothModified ConflictReason = iota + 1

	// ReasonDeletedModified: one branch deleted the leaf, the other modified
	// it. The user must choose which intent wins.
	ReasonDeletedModified

	// ReasonAddAdd: leaf was absent in base and both branches added it with
	// different content (different blob hashes).
	ReasonAddAdd
)

// String renders the reason in lowercase, dash-separated form.
func (r ConflictReason) String() string {
	switch r {
	case ReasonBothModified:
		return "both-modified"
	case ReasonDeletedModified:
		return "deleted-modified"
	case ReasonAddAdd:
		return "add-add"
	default:
		return "unknown"
	}
}

// Conflict identifies a leaf that the 3-way merge could not auto-resolve.
//
// Path is the sequence of DNS labels from the zone apex down to the
// containing tree (i.e. the same coordinate space as history.Change.Path).
// RRType is the leaf name (the RR type mnemonic such as "A", "MX").
//
// Hashes are provided for diagnostics; ZeroHash means "absent on this side".
type Conflict struct {
	Path   []string
	RRType string
	Reason ConflictReason

	Base   store.Hash
	Ours   store.Hash
	Theirs store.Hash
}

// FQDN returns the conflict's owner name relative to the zone apex
// ("@" for the apex itself).
func (c Conflict) FQDN() string {
	if len(c.Path) == 0 {
		return "@"
	}
	return strings.Join(c.Path, ".")
}

// String renders the conflict as "<fqdn> <type>: <reason>".
func (c Conflict) String() string {
	return fmt.Sprintf("%s %s: %s", c.FQDN(), c.RRType, c.Reason)
}

// MergeTrees performs a 3-way merge of three trees rooted at base, ours,
// and theirs and returns the hash of the merged tree.
//
// The merge is structural: corresponding sub-trees and leaves are matched
// by (kind, name). Per-leaf resolution rules are:
//
//	ours == theirs               -> take that value
//	ours == base                 -> take theirs (theirs changed)
//	theirs == base               -> take ours   (ours changed)
//	otherwise                    -> conflict, ours wins for the merged tree
//
// Conflicts are reported in the returned slice (sorted by FQDN, RRType).
// The returned tree always has *some* value at every conflicted leaf
// (currently "ours") so that the caller can still write a merged tree to
// disk for inspection; callers should refuse to advance a branch when
// len(conflicts) > 0.
//
// Any of base, ours, theirs may be ZeroHash (meaning "empty tree").
func MergeTrees(ctx context.Context, s store.Storage, base, ours, theirs store.Hash) (store.Hash, []Conflict, error) {
	var conflicts []Conflict
	merged, err := mergeTreesAt(ctx, s, nil, base, ours, theirs, &conflicts)
	if err != nil {
		return store.ZeroHash, nil, err
	}
	sortConflicts(conflicts)
	return merged, conflicts, nil
}

func mergeTreesAt(ctx context.Context, s store.Storage, path []string, base, ours, theirs store.Hash, out *[]Conflict) (store.Hash, error) {
	// Identical-tree fast paths (structural sharing => no work).
	switch {
	case ours == theirs:
		return ours, nil
	case base == ours:
		return theirs, nil
	case base == theirs:
		return ours, nil
	}

	bt, err := loadOrEmpty(ctx, s, base)
	if err != nil {
		return store.ZeroHash, err
	}
	ot, err := loadOrEmpty(ctx, s, ours)
	if err != nil {
		return store.ZeroHash, err
	}
	tt, err := loadOrEmpty(ctx, s, theirs)
	if err != nil {
		return store.ZeroHash, err
	}

	// Merge-walk three sorted entry lists by (kind, name).
	merged := make([]object.TreeEntry, 0, max3(len(bt.Entries), len(ot.Entries), len(tt.Entries)))
	bi, oi, ti := 0, 0, 0
	for bi < len(bt.Entries) || oi < len(ot.Entries) || ti < len(tt.Entries) {
		// Compute the smallest (kind, name) across the three cursors.
		key, kind, ok := minKey(bt.Entries, bi, ot.Entries, oi, tt.Entries, ti)
		if !ok {
			break
		}

		var bh, oh, th store.Hash
		if bi < len(bt.Entries) && bt.Entries[bi].Name == key && bt.Entries[bi].Kind == kind {
			bh = bt.Entries[bi].Hash
			bi++
		}
		if oi < len(ot.Entries) && ot.Entries[oi].Name == key && ot.Entries[oi].Kind == kind {
			oh = ot.Entries[oi].Hash
			oi++
		}
		if ti < len(tt.Entries) && tt.Entries[ti].Name == key && tt.Entries[ti].Kind == kind {
			th = tt.Entries[ti].Hash
			ti++
		}

		if kind == object.EntryLeaf {
			resolved, conflict := resolveLeaf(bh, oh, th)
			if conflict != nil {
				conflict.Path = slices.Clone(path)
				conflict.RRType = key
				*out = append(*out, *conflict)
			}
			if !resolved.IsZero() {
				merged = append(merged, object.TreeEntry{
					Kind: object.EntryLeaf,
					Name: key,
					Hash: resolved,
				})
			}
			continue
		}

		// Subtree: recurse.
		sub := append(path[:len(path):len(path)], key)
		mh, err := mergeTreesAt(ctx, s, sub, bh, oh, th, out)
		if err != nil {
			return store.ZeroHash, err
		}
		if !mh.IsZero() {
			merged = append(merged, object.TreeEntry{
				Kind: object.EntrySubtree,
				Name: key,
				Hash: mh,
			})
		}
	}

	if len(merged) == 0 {
		return store.ZeroHash, nil
	}
	return object.PutTree(ctx, s, object.Tree{Entries: merged})
}

// resolveLeaf applies the per-leaf 3-way rules. Either of the input hashes
// may be ZeroHash to mean "absent on that side". The returned hash is the
// chosen winner (ZeroHash if the merged result has no leaf there); a
// non-nil conflict means the caller should record an unresolved conflict.
func resolveLeaf(base, ours, theirs store.Hash) (store.Hash, *Conflict) {
	switch {
	// All three identical (including all absent or all present).
	case ours == theirs:
		return ours, nil

	// Only one side changed relative to base.
	case ours == base:
		return theirs, nil
	case theirs == base:
		return ours, nil

	// All three differ. Classify by absence pattern.
	case base.IsZero():
		// Add-add with different content.
		return ours, &Conflict{
			Reason: ReasonAddAdd,
			Base:   base, Ours: ours, Theirs: theirs,
		}

	case ours.IsZero() || theirs.IsZero():
		return ours, &Conflict{
			Reason: ReasonDeletedModified,
			Base:   base, Ours: ours, Theirs: theirs,
		}

	default:
		return ours, &Conflict{
			Reason: ReasonBothModified,
			Base:   base, Ours: ours, Theirs: theirs,
		}
	}
}

func loadOrEmpty(ctx context.Context, s store.Storage, h store.Hash) (object.Tree, error) {
	if h.IsZero() {
		return object.Tree{}, nil
	}
	return object.LoadTree(ctx, s, h)
}

// minKey picks the smallest pending (kind, name) among the three cursor
// positions. It returns (name, kind, true) or ("", 0, false) when all
// cursors are exhausted. Sub-trees come before leaves at the same level
// because EntrySubtree (0) < EntryLeaf (1).
func minKey(b []object.TreeEntry, bi int, o []object.TreeEntry, oi int, t []object.TreeEntry, ti int) (string, object.EntryKind, bool) {
	type cursor struct {
		kind object.EntryKind
		name string
		ok   bool
	}
	c := [3]cursor{}
	if bi < len(b) {
		c[0] = cursor{b[bi].Kind, b[bi].Name, true}
	}
	if oi < len(o) {
		c[1] = cursor{o[oi].Kind, o[oi].Name, true}
	}
	if ti < len(t) {
		c[2] = cursor{t[ti].Kind, t[ti].Name, true}
	}
	have := false
	var best cursor
	for _, x := range c {
		if !x.ok {
			continue
		}
		if !have || lessKN(x.kind, x.name, best.kind, best.name) {
			best = x
			have = true
		}
	}
	if !have {
		return "", 0, false
	}
	return best.name, best.kind, true
}

func lessKN(ka object.EntryKind, na string, kb object.EntryKind, nb string) bool {
	if ka != kb {
		return ka < kb
	}
	return na < nb
}

func max3(a, b, c int) int {
	if a < b {
		a = b
	}
	if a < c {
		a = c
	}
	return a
}

func sortConflicts(cs []Conflict) {
	slices.SortFunc(cs, func(a, b Conflict) int {
		af, bf := a.FQDN(), b.FQDN()
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		}
		switch {
		case a.RRType < b.RRType:
			return -1
		case a.RRType > b.RRType:
			return 1
		}
		return 0
	})
}
