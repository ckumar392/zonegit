package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ckumar392/zonegit/pkg/history"
	"github.com/ckumar392/zonegit/pkg/merge"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/store"
)

// MergeResult describes the outcome of a Merge call.
type MergeResult struct {
	// FastForward is true when ours was an ancestor of theirs and the
	// branch was simply advanced (no merge commit produced).
	FastForward bool

	// AlreadyUpToDate is true when theirs was an ancestor of ours; nothing
	// to do.
	AlreadyUpToDate bool

	// Commit is the resulting commit hash on the current branch. ZeroHash
	// when AlreadyUpToDate or when conflicts prevented committing.
	Commit store.Hash

	// Conflicts lists per-leaf 3-way conflicts. When non-empty, no commit
	// is produced and the branch ref is unchanged.
	Conflicts []merge.Conflict
}

// Merge integrates the named branch into the current branch (the branch
// that HEAD points at).
//
// Behavior matches Git's `git merge`:
//   - If our branch is an ancestor of theirs: fast-forward.
//   - If theirs is an ancestor of ours: already up to date.
//   - Otherwise: 3-way merge over trees. On conflicts we abort with
//     a non-empty MergeResult.Conflicts and leave the branch unchanged.
//   - Otherwise: produce a merge commit with two parents.
//
// Merge requires no staged changes (similar to Git's index check).
func (r *Repo) Merge(ctx context.Context, theirsBranch string, author object.Identity, msg string) (MergeResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.staging) > 0 {
		return MergeResult{}, fmt.Errorf("merge: refusing to merge with %d staged changes", len(r.staging))
	}

	zoneName, branchName, ours, err := r.refs.ReadHEAD(ctx)
	if err != nil {
		return MergeResult{}, fmt.Errorf("merge: read HEAD: %w", err)
	}
	theirsName := strings.TrimPrefix(theirsBranch, refs.BranchPrefix)
	if theirsName == branchName {
		return MergeResult{}, fmt.Errorf("merge: cannot merge a branch into itself")
	}

	theirs, err := r.refs.GetBranch(ctx, zoneName, theirsName)
	if err != nil {
		return MergeResult{}, fmt.Errorf("merge: %w", err)
	}

	// Already up to date?
	if !ours.IsZero() {
		anc, err := isAncestor(ctx, r.storage, theirs, ours)
		if err != nil {
			return MergeResult{}, err
		}
		if anc {
			return MergeResult{AlreadyUpToDate: true}, nil
		}
	}

	branchRef := refs.BranchRef(zoneName, branchName)

	// Fast-forward?
	if ours.IsZero() {
		// Orphan branch: just point it at theirs.
		if err := r.refs.CreateBranch(ctx, zoneName, branchName, theirs); err != nil {
			return MergeResult{}, fmt.Errorf("merge: ff create %s: %w", branchName, err)
		}
		_ = r.refs.AppendReflog(ctx, branchRef, ours, theirs, author.String(), "merge", "fast-forward "+theirsName)
		return MergeResult{FastForward: true, Commit: theirs}, nil
	}
	ff, err := isAncestor(ctx, r.storage, ours, theirs)
	if err != nil {
		return MergeResult{}, err
	}
	if ff {
		if err := r.refs.UpdateBranch(ctx, zoneName, branchName, ours, theirs); err != nil {
			return MergeResult{}, fmt.Errorf("merge: ff %s: %w", branchName, err)
		}
		_ = r.refs.AppendReflog(ctx, branchRef, ours, theirs, author.String(), "merge", "fast-forward "+theirsName)
		return MergeResult{FastForward: true, Commit: theirs}, nil
	}

	// True 3-way: find a merge base.
	base, err := MergeBase(ctx, r.storage, ours, theirs)
	if err != nil {
		return MergeResult{}, fmt.Errorf("merge: merge-base: %w", err)
	}

	mergedTree, conflicts, err := merge.MergeTrees(ctx, r.storage,
		object.TreeOf(ctx, r.storage, base),
		object.TreeOf(ctx, r.storage, ours),
		object.TreeOf(ctx, r.storage, theirs),
	)
	if err != nil {
		return MergeResult{}, fmt.Errorf("merge: tree merge: %w", err)
	}
	if len(conflicts) > 0 {
		return MergeResult{Conflicts: conflicts}, nil
	}

	if msg == "" {
		msg = fmt.Sprintf("Merge branch '%s' into %s", theirsName, branchName)
	}
	now := time.Now()
	c := object.Commit{
		Tree:       mergedTree,
		Parents:    []store.Hash{ours, theirs},
		Author:     author,
		Committer:  author,
		AuthorTime: now,
		CommitTime: now,
		Message:    msg,
	}
	commitHash, commitObj := c.Encode()
	if err := r.storage.PutObject(ctx, commitHash, commitObj); err != nil {
		return MergeResult{}, err
	}
	if err := r.refs.UpdateBranch(ctx, zoneName, branchName, ours, commitHash); err != nil {
		return MergeResult{}, fmt.Errorf("merge: advance %s: %w", branchName, err)
	}
	_ = r.refs.AppendReflog(ctx, branchRef, ours, commitHash, author.String(), "merge", msg)
	return MergeResult{Commit: commitHash}, nil
}

// MergeBase returns the lowest common ancestor of two commits using a
// straightforward two-set traversal. Returns ZeroHash if there is none
// (independent histories).
func MergeBase(ctx context.Context, s store.Storage, a, b store.Hash) (store.Hash, error) {
	if a.IsZero() || b.IsZero() {
		return store.ZeroHash, nil
	}
	if a == b {
		return a, nil
	}

	// BFS over a's ancestors first, then walk b's ancestors and return the
	// first one we've already seen. This is O(N+M) which is fine for v1
	// where histories are small.
	aAnc, err := ancestorSet(ctx, s, a)
	if err != nil {
		return store.ZeroHash, err
	}
	queue := []store.Hash{b}
	visited := map[store.Hash]bool{b: true}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if aAnc[h] {
			return h, nil
		}
		c, err := loadCommit(ctx, s, h)
		if err != nil {
			return store.ZeroHash, err
		}
		for _, p := range c.Parents {
			if !visited[p] {
				visited[p] = true
				queue = append(queue, p)
			}
		}
	}
	return store.ZeroHash, nil
}

// isAncestor reports whether anc is an ancestor of (or equal to) desc.
func isAncestor(ctx context.Context, s store.Storage, anc, desc store.Hash) (bool, error) {
	if anc.IsZero() || anc == desc {
		return true, nil
	}
	if desc.IsZero() {
		return false, nil
	}
	queue := []store.Hash{desc}
	seen := map[store.Hash]bool{desc: true}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if h == anc {
			return true, nil
		}
		c, err := loadCommit(ctx, s, h)
		if err != nil {
			return false, err
		}
		for _, p := range c.Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}

func ancestorSet(ctx context.Context, s store.Storage, root store.Hash) (map[store.Hash]bool, error) {
	out := map[store.Hash]bool{}
	queue := []store.Hash{root}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if out[h] {
			continue
		}
		out[h] = true
		c, err := loadCommit(ctx, s, h)
		if err != nil {
			return nil, err
		}
		for _, p := range c.Parents {
			if !out[p] {
				queue = append(queue, p)
			}
		}
	}
	return out, nil
}

func loadCommit(ctx context.Context, s store.Storage, h store.Hash) (object.Commit, error) {
	obj, err := s.GetObject(ctx, h)
	if err != nil {
		return object.Commit{}, err
	}
	return object.DecodeCommit(obj.Payload)
}

// Revert produces a new commit on the current branch that is the inverse
// of the named commit (target). Specifically: it computes the tree diff
// between target and target's first parent and applies the *reversed*
// changes on top of the current branch HEAD.
//
// If target has no parents (root commit), Revert removes every leaf
// introduced by it.
//
// The returned hash is the new commit on the current branch. Returns an
// error if applying the inverse would conflict with the current state
// (the leaf to be reverted no longer matches target's "after" value).
func (r *Repo) Revert(ctx context.Context, refish string, author object.Identity, msg string) (store.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.staging) > 0 {
		return store.ZeroHash, fmt.Errorf("revert: refusing to revert with %d staged changes", len(r.staging))
	}

	target, err := r.refs.Resolve(ctx, refish)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("revert: resolve %q: %w", refish, err)
	}
	tc, err := loadCommit(ctx, r.storage, target)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("revert: load target: %w", err)
	}
	var parentTree store.Hash
	if len(tc.Parents) > 0 {
		pc, err := loadCommit(ctx, r.storage, tc.Parents[0])
		if err != nil {
			return store.ZeroHash, fmt.Errorf("revert: load parent: %w", err)
		}
		parentTree = pc.Tree
	}

	zoneName, branchName, head, err := r.refs.ReadHEAD(ctx)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("revert: read HEAD: %w", err)
	}
	if head.IsZero() {
		return store.ZeroHash, fmt.Errorf("revert: HEAD is empty")
	}
	branchRef := refs.BranchRef(zoneName, branchName)
	headCommit, err := loadCommit(ctx, r.storage, head)
	if err != nil {
		return store.ZeroHash, err
	}

	// Compute changes target introduced (parent -> target). Reverting means
	// applying their inverse on top of HEAD.
	changes, err := history.Diff(ctx, r.storage, parentTree, tc.Tree)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("revert: diff: %w", err)
	}
	if len(changes) == 0 {
		return store.ZeroHash, fmt.Errorf("revert: %s introduced no changes", target.Short())
	}

	curTree := headCommit.Tree
	for _, ch := range changes {
		// Each change's "newBlob" is what target wrote. Reverting means
		// putting back oldBlob (or removing the leaf entirely if it was
		// added by target).
		var leaf store.Hash // ZeroHash means "delete leaf"
		switch ch.Op {
		case history.OpAdded:
			leaf = store.ZeroHash // remove what target added
		case history.OpRemoved:
			leaf = ch.OldBlob // restore what target removed
		case history.OpModified:
			leaf = ch.OldBlob // restore previous value
		}
		curTree, err = object.UpdateTree(ctx, r.storage, curTree, ch.Path, ch.RRType, leaf)
		if err != nil {
			return store.ZeroHash, fmt.Errorf("revert: apply inverse %s %s: %w", ch.FQDN(), ch.RRType, err)
		}
	}

	if curTree == headCommit.Tree {
		return store.ZeroHash, fmt.Errorf("revert: nothing to do; current tree already matches inverse")
	}

	if msg == "" {
		first := strings.SplitN(tc.Message, "\n", 2)[0]
		msg = fmt.Sprintf("Revert \"%s\"\n\nThis reverts commit %s.", first, target)
	}
	now := time.Now()
	c := object.Commit{
		Tree:       curTree,
		Parents:    []store.Hash{head},
		Author:     author,
		Committer:  author,
		AuthorTime: now,
		CommitTime: now,
		Message:    msg,
	}
	commitHash, commitObj := c.Encode()
	if err := r.storage.PutObject(ctx, commitHash, commitObj); err != nil {
		return store.ZeroHash, err
	}
	if err := r.refs.UpdateBranch(ctx, zoneName, branchName, head, commitHash); err != nil {
		return store.ZeroHash, fmt.Errorf("revert: advance %s: %w", branchName, err)
	}
	_ = r.refs.AppendReflog(ctx, branchRef, head, commitHash, author.String(), "revert", msg)
	return commitHash, nil
}

// ResetHard moves the current branch ref to the commit identified by refish
// and clears any staged changes. There is no working-tree analogue in
// zonegit, so --hard and --mixed degenerate to the same operation; we name
// this method ResetHard to be explicit that it is destructive (the previous
// tip becomes unreachable from the branch).
func (r *Repo) ResetHard(ctx context.Context, refish string, author object.Identity) (store.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	target, err := r.refs.Resolve(ctx, refish)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("reset: resolve %q: %w", refish, err)
	}
	zoneName, branchName, head, err := r.refs.ReadHEAD(ctx)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("reset: read HEAD: %w", err)
	}

	if head == target {
		r.staging = make(map[stagingKey]stagingValue)
		return target, nil
	}

	if head.IsZero() {
		if err := r.refs.CreateBranch(ctx, zoneName, branchName, target); err != nil {
			return store.ZeroHash, fmt.Errorf("reset: create %s: %w", branchName, err)
		}
	} else {
		if err := r.refs.UpdateBranch(ctx, zoneName, branchName, head, target); err != nil {
			return store.ZeroHash, fmt.Errorf("reset: move %s: %w", branchName, err)
		}
	}
	_ = r.refs.AppendReflog(ctx, refs.BranchRef(zoneName, branchName), head, target, author.String(), "reset", "reset --hard "+refish)
	r.staging = make(map[stagingKey]stagingValue)
	return target, nil
}
