package history

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ckumar392/dnsdb/pkg/object"
	"github.com/ckumar392/dnsdb/pkg/store"
)

// ChangeOp describes the nature of an RRset change between two trees.
type ChangeOp int

const (
	OpAdded ChangeOp = iota + 1
	OpRemoved
	OpModified
)

func (op ChangeOp) String() string {
	switch op {
	case OpAdded:
		return "added"
	case OpRemoved:
		return "removed"
	case OpModified:
		return "modified"
	default:
		return "?"
	}
}

// Change is one RRset-level change between two trees.
type Change struct {
	Path    []string // labels from zone root down (e.g. ["api"] for api.<zone>)
	RRType  string   // "A", "AAAA", "MX", ...
	Op      ChangeOp
	OldBlob store.Hash // ZeroHash for OpAdded
	NewBlob store.Hash // ZeroHash for OpRemoved
}

// FQDN returns the labels joined dot-style (without trailing zone suffix).
func (c Change) FQDN() string {
	if len(c.Path) == 0 {
		return "@"
	}
	return strings.Join(c.Path, ".")
}

// Diff walks two trees in lockstep and returns the set of RRset-level
// changes. If oldTree == newTree (by hash), it returns no changes without
// loading anything. Identical sub-trees are skipped (structural sharing
// makes diff cost O(changed-paths), not O(zone-size)).
//
// Either hash may be ZeroHash to mean "empty tree".
//
// Output order: lexicographic by FQDN, then RRType.
func Diff(ctx context.Context, s store.Storage, oldTree, newTree store.Hash) ([]Change, error) {
	if oldTree == newTree {
		return nil, nil
	}
	var out []Change
	if err := diffWalk(ctx, s, nil, oldTree, newTree, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].FQDN(), out[j].FQDN(); a != b {
			return a < b
		}
		return out[i].RRType < out[j].RRType
	})
	return out, nil
}

func diffWalk(ctx context.Context, s store.Storage, path []string, oldH, newH store.Hash, out *[]Change) error {
	if oldH == newH {
		return nil
	}

	var oldTree, newTree object.Tree
	var err error
	if !oldH.IsZero() {
		oldTree, err = object.LoadTree(ctx, s, oldH)
		if err != nil {
			return fmt.Errorf("diff: load old %s: %w", oldH.Short(), err)
		}
	}
	if !newH.IsZero() {
		newTree, err = object.LoadTree(ctx, s, newH)
		if err != nil {
			return fmt.Errorf("diff: load new %s: %w", newH.Short(), err)
		}
	}

	// Merge-walk the two sorted entry lists.
	i, j := 0, 0
	oa, na := oldTree.Entries, newTree.Entries
	for i < len(oa) || j < len(na) {
		var have, take int // 1=old, 2=new, 3=both
		switch {
		case i >= len(oa):
			have = 2
		case j >= len(na):
			have = 1
		default:
			c := cmpEntry(oa[i], na[j])
			switch {
			case c < 0:
				have = 1
			case c > 0:
				have = 2
			default:
				have = 3
			}
		}
		take = have

		switch take {
		case 1: // present only in old
			if err := emitOnlySide(ctx, s, append(path[:len(path):len(path)]), oa[i], OpRemoved, out); err != nil {
				return err
			}
			i++
		case 2: // present only in new
			if err := emitOnlySide(ctx, s, append(path[:len(path):len(path)]), na[j], OpAdded, out); err != nil {
				return err
			}
			j++
		case 3: // present in both
			if oa[i].Hash != na[j].Hash {
				if oa[i].Kind == object.EntryLeaf {
					*out = append(*out, Change{
						Path:    append(path[:len(path):len(path)]),
						RRType:  oa[i].Name,
						Op:      OpModified,
						OldBlob: oa[i].Hash,
						NewBlob: na[j].Hash,
					})
				} else {
					sub := append(path[:len(path):len(path)], oa[i].Name)
					if err := diffWalk(ctx, s, sub, oa[i].Hash, na[j].Hash, out); err != nil {
						return err
					}
				}
			}
			i++
			j++
		}
	}
	return nil
}

// emitOnlySide records changes for an entry present only on one side. For
// subtrees, this means recursing to enumerate every leaf as added/removed.
func emitOnlySide(ctx context.Context, s store.Storage, path []string, e object.TreeEntry, op ChangeOp, out *[]Change) error {
	if e.Kind == object.EntryLeaf {
		ch := Change{Path: path, RRType: e.Name, Op: op}
		if op == OpAdded {
			ch.NewBlob = e.Hash
		} else {
			ch.OldBlob = e.Hash
		}
		*out = append(*out, ch)
		return nil
	}
	// Subtree: recurse into it as either all-added or all-removed.
	t, err := object.LoadTree(ctx, s, e.Hash)
	if err != nil {
		return fmt.Errorf("diff: load %s subtree %s: %w", op, e.Hash.Short(), err)
	}
	sub := append(path[:len(path):len(path)], e.Name)
	for _, child := range t.Entries {
		if err := emitOnlySide(ctx, s, sub, child, op, out); err != nil {
			return err
		}
	}
	return nil
}

func cmpEntry(a, b object.TreeEntry) int {
	if a.Kind != b.Kind {
		if a.Kind < b.Kind {
			return -1
		}
		return 1
	}
	switch {
	case a.Name < b.Name:
		return -1
	case a.Name > b.Name:
		return 1
	}
	return 0
}
