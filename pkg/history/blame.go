package history

import (
	"context"
	"errors"
	"fmt"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
)

// BlameInfo identifies the commit that introduced the *current* value of
// an RRset and the value itself. "Introduced" = the commit whose parent
// either lacks the RRset or has a different blob.
type BlameInfo struct {
	Commit  store.Hash // commit hash that introduced the current value
	Author  object.Identity
	Message string
	Blob    store.Hash // current blob hash for (path, rrtype)
	Found   bool       // false if the RRset doesn't exist at head
}

// Blame walks the first-parent chain from head and returns the commit that
// last changed (path, rrtype). If the RRset is absent at head, Found=false.
func Blame(ctx context.Context, s store.Storage, head store.Hash, path []string, rrtype string) (BlameInfo, error) {
	if head.IsZero() {
		return BlameInfo{}, fmt.Errorf("blame: head is zero")
	}

	// 1. Find the current blob hash at head.
	headBlob, err := lookupBlob(ctx, s, head, path, rrtype)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BlameInfo{}, nil // not found at head
		}
		return BlameInfo{}, err
	}

	// 2. Walk first-parent chain. Track the deepest commit where the blob
	// is still headBlob; that is the introducing commit.
	cur := head
	var introducing store.Hash
	var introCommit object.Commit
	for !cur.IsZero() {
		curObj, err := s.GetObject(ctx, cur)
		if err != nil {
			return BlameInfo{}, err
		}
		c, err := object.DecodeCommit(curObj.Payload)
		if err != nil {
			return BlameInfo{}, err
		}
		// What is the blob at this commit?
		curBlob, lookupErr := lookupBlobInTree(ctx, s, c.Tree, path, rrtype)
		if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
			return BlameInfo{}, lookupErr
		}
		if errors.Is(lookupErr, store.ErrNotFound) || curBlob != headBlob {
			break // we've gone past the introducing commit
		}
		introducing = cur
		introCommit = c
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}

	if introducing.IsZero() {
		// Shouldn't happen: head itself has the blob.
		return BlameInfo{}, fmt.Errorf("blame: internal: introducing commit not found")
	}
	return BlameInfo{
		Commit:  introducing,
		Author:  introCommit.Author,
		Message: introCommit.Message,
		Blob:    headBlob,
		Found:   true,
	}, nil
}

// lookupBlob finds the blob hash at (path, rrtype) under the given commit.
func lookupBlob(ctx context.Context, s store.Storage, commit store.Hash, path []string, rrtype string) (store.Hash, error) {
	obj, err := s.GetObject(ctx, commit)
	if err != nil {
		return store.ZeroHash, err
	}
	c, err := object.DecodeCommit(obj.Payload)
	if err != nil {
		return store.ZeroHash, err
	}
	return lookupBlobInTree(ctx, s, c.Tree, path, rrtype)
}

func lookupBlobInTree(ctx context.Context, s store.Storage, tree store.Hash, path []string, rrtype string) (store.Hash, error) {
	if tree.IsZero() {
		return store.ZeroHash, fmt.Errorf("empty tree: %w", store.ErrNotFound)
	}
	return object.WalkTree(ctx, s, tree, path, rrtype)
}
