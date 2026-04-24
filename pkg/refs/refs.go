package refs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Well-known ref prefixes.
const (
	HeadRef      = "HEAD"
	BranchPrefix = "refs/heads/"
	TagPrefix    = "refs/tags/"
)

// DB wraps a store.Storage and provides higher-level ref operations:
// branches, HEAD, tags, reflog, and ref-ish resolution.
type DB struct {
	s store.Storage
}

// New returns a DB backed by s.
func New(s store.Storage) *DB {
	return &DB{s: s}
}

// --- HEAD ---

// ReadHEAD returns the current HEAD. HEAD is stored as a symbolic ref
// whose value is a branch name (e.g. "refs/heads/main"). We store
// it as a special ref whose "hash" is actually the branch ref name
// encoded. For v0, we store the branch name in a ref called "HEAD".
//
// Returns the branch name (e.g. "refs/heads/main") and the commit hash
// that branch points to.
func (db *DB) ReadHEAD(ctx context.Context) (branch string, commit store.Hash, err error) {
	branchBytes, ok, err := db.s.GetRef(ctx, HeadRef)
	if err != nil {
		return "", store.ZeroHash, fmt.Errorf("read HEAD: %w", err)
	}
	if !ok {
		return "", store.ZeroHash, fmt.Errorf("read HEAD: %w", store.ErrNotFound)
	}
	// HEAD stores the target branch name encoded in the hash bytes.
	// We use a simple scheme: first byte = length, rest = ASCII branch name.
	branch = decodeBranchFromHash(branchBytes)
	if branch == "" {
		return "", store.ZeroHash, fmt.Errorf("read HEAD: corrupt symbolic ref")
	}
	// Now dereference the branch to get the commit hash.
	commit, ok, err = db.s.GetRef(ctx, branch)
	if err != nil {
		return branch, store.ZeroHash, fmt.Errorf("read HEAD -> %s: %w", branch, err)
	}
	if !ok {
		// Branch exists symbolically but has no commits yet (orphan).
		return branch, store.ZeroHash, nil
	}
	return branch, commit, nil
}

// SetHEAD points HEAD at the given branch (must include BranchPrefix).
func (db *DB) SetHEAD(ctx context.Context, branch string) error {
	if !strings.HasPrefix(branch, BranchPrefix) {
		return fmt.Errorf("SetHEAD: branch must start with %q, got %q", BranchPrefix, branch)
	}
	encoded := encodeBranchToHash(branch)
	// Try create first, then update.
	old, ok, err := db.s.GetRef(ctx, HeadRef)
	if err != nil {
		return fmt.Errorf("SetHEAD: %w", err)
	}
	if !ok {
		return db.s.CASRef(ctx, HeadRef, store.ZeroHash, encoded)
	}
	return db.s.CASRef(ctx, HeadRef, old, encoded)
}

// encodeBranchToHash packs a branch name (up to 31 bytes) into a Hash.
// Format: [len][name bytes...][zero padding].
func encodeBranchToHash(branch string) store.Hash {
	var h store.Hash
	if len(branch) > store.HashSize-1 {
		// Truncate — in practice branch names are short.
		branch = branch[:store.HashSize-1]
	}
	h[0] = byte(len(branch))
	copy(h[1:], branch)
	return h
}

func decodeBranchFromHash(h store.Hash) string {
	n := int(h[0])
	if n == 0 || n > store.HashSize-1 {
		return ""
	}
	return string(h[1 : 1+n])
}

// --- Branches ---

// CreateBranch creates a new branch pointing at commit.
func (db *DB) CreateBranch(ctx context.Context, name string, commit store.Hash) error {
	ref := BranchPrefix + name
	if err := db.s.CASRef(ctx, ref, store.ZeroHash, commit); err != nil {
		return fmt.Errorf("create branch %s: %w", name, err)
	}
	return nil
}

// UpdateBranch moves branch from expected to next (atomic CAS).
func (db *DB) UpdateBranch(ctx context.Context, name string, expected, next store.Hash) error {
	ref := BranchPrefix + name
	return db.s.CASRef(ctx, ref, expected, next)
}

// DeleteBranch deletes a branch if it points at expected.
func (db *DB) DeleteBranch(ctx context.Context, name string, expected store.Hash) error {
	ref := BranchPrefix + name
	return db.s.DeleteRef(ctx, ref, expected)
}

// GetBranch returns the hash a branch points to.
func (db *DB) GetBranch(ctx context.Context, name string) (store.Hash, error) {
	ref := BranchPrefix + name
	h, ok, err := db.s.GetRef(ctx, ref)
	if err != nil {
		return h, err
	}
	if !ok {
		return h, fmt.Errorf("branch %s: %w", name, store.ErrNotFound)
	}
	return h, nil
}

// ListBranches returns all branch names (without the prefix), sorted.
func (db *DB) ListBranches(ctx context.Context) ([]string, error) {
	entries, err := db.s.ListRefs(ctx, BranchPrefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = strings.TrimPrefix(e.Name, BranchPrefix)
	}
	return names, nil
}

// --- Tags ---

// CreateTag creates a lightweight tag (name -> commit hash).
func (db *DB) CreateTag(ctx context.Context, name string, target store.Hash) error {
	ref := TagPrefix + name
	return db.s.CASRef(ctx, ref, store.ZeroHash, target)
}

// GetTag returns the hash a tag points to.
func (db *DB) GetTag(ctx context.Context, name string) (store.Hash, error) {
	ref := TagPrefix + name
	h, ok, err := db.s.GetRef(ctx, ref)
	if err != nil {
		return h, err
	}
	if !ok {
		return h, fmt.Errorf("tag %s: %w", name, store.ErrNotFound)
	}
	return h, nil
}

// ListTags returns all tag names (without the prefix), sorted.
func (db *DB) ListTags(ctx context.Context) ([]string, error) {
	entries, err := db.s.ListRefs(ctx, TagPrefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = strings.TrimPrefix(e.Name, TagPrefix)
	}
	return names, nil
}

// DeleteTag deletes a tag if it points at expected.
func (db *DB) DeleteTag(ctx context.Context, name string, expected store.Hash) error {
	ref := TagPrefix + name
	return db.s.DeleteRef(ctx, ref, expected)
}

// --- Reflog ---

// AppendReflog records a ref movement.
func (db *DB) AppendReflog(ctx context.Context, ref string, old, new store.Hash, who string, op, msg string) error {
	e := store.ReflogEntry{
		Old:      old,
		New:      new,
		Author:   who,
		UnixTime: time.Now().Unix(),
		TZOffset: 0,
		Op:       op,
		Message:  msg,
	}
	return db.s.AppendReflog(ctx, ref, e)
}

// ReadReflog returns the reflog for a given ref.
func (db *DB) ReadReflog(ctx context.Context, ref string) ([]store.ReflogEntry, error) {
	return db.s.ReadReflog(ctx, ref)
}

// --- Resolve ref-ish ---

// Resolve parses a ref-ish string and returns the commit hash it points to.
//
// Supported forms:
//   - Full hex hash (64 chars)
//   - Branch name: "main" -> looks up refs/heads/main
//   - Tag name: "v1.0" -> looks up refs/tags/v1.0
//   - "HEAD" -> dereferences HEAD
//   - Ancestor: "main~3" or "HEAD~1" -> walks N parents
//   - Raw ref path: "refs/heads/main" or "refs/tags/v1.0"
func (db *DB) Resolve(ctx context.Context, refish string) (store.Hash, error) {
	// Split off ~N suffix.
	base, ancestor := splitAncestor(refish)

	h, err := db.resolveBase(ctx, base)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("resolve %q: %w", refish, err)
	}

	// Walk ancestors.
	for i := 0; i < ancestor; i++ {
		obj, err := db.s.GetObject(ctx, h)
		if err != nil {
			return store.ZeroHash, fmt.Errorf("resolve %q: walking parent %d: %w", refish, i, err)
		}
		if obj.Kind != "commit" {
			return store.ZeroHash, fmt.Errorf("resolve %q: expected commit at %s, got %s", refish, h.Short(), obj.Kind)
		}
		c, err := object.DecodeCommit(obj.Payload)
		if err != nil {
			return store.ZeroHash, fmt.Errorf("resolve %q: decode commit %s: %w", refish, h.Short(), err)
		}
		if len(c.Parents) == 0 {
			return store.ZeroHash, fmt.Errorf("resolve %q: commit %s has no parent (at ~%d)", refish, h.Short(), i+1)
		}
		h = c.Parents[0] // follow first parent
	}

	return h, nil
}

func (db *DB) resolveBase(ctx context.Context, base string) (store.Hash, error) {
	// 1) Full hex hash?
	if len(base) == 2*store.HashSize {
		return store.ParseHash(base)
	}

	// 2) HEAD?
	if base == HeadRef {
		_, commit, err := db.ReadHEAD(ctx)
		if err != nil {
			return store.ZeroHash, err
		}
		if commit.IsZero() {
			return store.ZeroHash, fmt.Errorf("HEAD: orphan branch (no commits)")
		}
		return commit, nil
	}

	// 3) Raw ref path?
	if strings.HasPrefix(base, "refs/") {
		h, ok, err := db.s.GetRef(ctx, base)
		if err != nil {
			return store.ZeroHash, err
		}
		if !ok {
			return store.ZeroHash, fmt.Errorf("ref %s: %w", base, store.ErrNotFound)
		}
		return h, nil
	}

	// 4) Try as branch name.
	h, ok, err := db.s.GetRef(ctx, BranchPrefix+base)
	if err != nil {
		return store.ZeroHash, err
	}
	if ok {
		return h, nil
	}

	// 5) Try as tag name.
	h, ok, err = db.s.GetRef(ctx, TagPrefix+base)
	if err != nil {
		return store.ZeroHash, err
	}
	if ok {
		return h, nil
	}

	return store.ZeroHash, fmt.Errorf("%s: %w", base, store.ErrNotFound)
}

// splitAncestor parses "foo~3" into ("foo", 3). No tilde means 0.
func splitAncestor(s string) (string, int) {
	idx := strings.LastIndex(s, "~")
	if idx < 0 {
		return s, 0
	}
	base := s[:idx]
	nStr := s[idx+1:]
	if nStr == "" {
		return base, 1
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 {
		// Not a valid ancestor spec — treat the whole thing as a name.
		return s, 0
	}
	return base, n
}
