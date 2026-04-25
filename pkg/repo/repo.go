package repo

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/history"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/store/badger"
	"github.com/ckumar392/zonegit/pkg/store/memstore"
	"github.com/ckumar392/zonegit/pkg/zone"
)

// DefaultBranch is the branch v0 uses for new repos.
const DefaultBranch = "main"

// Repo is the public zonegit handle.
//
// A Repo is bound to one Storage and one zone (the zone name is part of
// init metadata so that label paths in trees can be relative to the zone
// apex). All write paths take a per-Repo write lock to avoid useless CAS
// retries against ourselves; concurrent readers are unblocked.
type Repo struct {
	storage store.Storage
	refs    *refs.DB

	// staging holds in-memory edits made via Set/Delete that have not yet
	// been Commit-ted. Keys are (path joined by ".", rrtype). Value of nil
	// means "delete this rrset on commit".
	mu      sync.Mutex
	staging map[stagingKey]stagingValue
	zone    string // canonical zone name with trailing dot
}

type stagingKey struct {
	fqdn   string // labels joined by "." (no trailing dot, "" = zone apex)
	rrtype string // "A", "AAAA", ...
}

type stagingValue struct {
	tombstone bool
	blob      store.Hash
}

// Options for opening a Repo.
type Options struct {
	// Storage to use. If non-nil, takes precedence over Path.
	Storage store.Storage
	// Path to a Badger directory. Used only if Storage is nil.
	Path string
	// Memory uses an in-memory store. Used only if Storage and Path are both empty.
	Memory bool
	// ReadOnly opens the underlying Badger store without acquiring the
	// directory lock, allowing multiple readers (and one concurrent writer)
	// to coexist. Only meaningful with Path. Mutating calls (Set/Delete/
	// Commit/Init) will fail with an error from BadgerDB.
	ReadOnly bool
}

// Open opens an existing repo or creates one if not present (when using a
// path-based Storage). The branch HEAD points at is loaded into memory.
func Open(opts Options) (*Repo, error) {
	var s store.Storage
	switch {
	case opts.Storage != nil:
		s = opts.Storage
	case opts.Path != "":
		var (
			bs  *badger.Store
			err error
		)
		if opts.ReadOnly {
			bs, err = badger.OpenReadOnly(opts.Path)
		} else {
			bs, err = badger.Open(opts.Path)
		}
		if err != nil {
			return nil, err
		}
		s = bs
	default:
		s = memstore.New()
	}
	return &Repo{
		storage: s,
		refs:    refs.New(s),
		staging: make(map[stagingKey]stagingValue),
	}, nil
}

// Close releases the underlying storage.
func (r *Repo) Close() error {
	return r.storage.Close()
}

// Storage returns the underlying store.Storage. Useful for tests and
// for embedders that need lower-level access.
func (r *Repo) Storage() store.Storage { return r.storage }

// Refs returns the underlying refs.DB.
func (r *Repo) Refs() *refs.DB { return r.refs }

// --- Init / Zone metadata ---

// Init creates an empty repo on the default branch with the given zone
// name. It is a no-op if HEAD is already set.
func (r *Repo) Init(ctx context.Context, zoneName string) error {
	zoneName = canonZone(zoneName)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.zone = zoneName

	if _, _, err := r.refs.ReadHEAD(ctx); err == nil {
		return nil // already initialized
	}
	// Set HEAD to refs/heads/main; branch itself is not created until the
	// first commit lands.
	return r.refs.SetHEAD(ctx, refs.BranchPrefix+DefaultBranch)
}

// Zone returns the canonical zone name (with trailing dot).
func (r *Repo) Zone() string { return r.zone }

// SetZone overrides the in-memory zone name (used by Open after restoring
// from disk; eventually we'll persist this).
func (r *Repo) SetZone(z string) { r.zone = canonZone(z) }

// --- Mutation API ---

// Set stages an RRset write to be applied on the next Commit. RRs are
// homogenized (same name/class/type/ttl) per pkg/zone rules.
func (r *Repo) Set(ctx context.Context, rrs []dns.RR) error {
	if len(rrs) == 0 {
		return fmt.Errorf("Set: empty RRset")
	}
	payload, err := zone.EncodeRRset(rrs)
	if err != nil {
		return fmt.Errorf("Set: encode: %w", err)
	}
	b := object.Blob{Payload: payload}
	h, obj := b.Encode()
	if err := r.storage.PutObject(ctx, h, obj); err != nil {
		return err
	}
	hdr := rrs[0].Header()
	owner := strings.ToLower(hdr.Name)
	if !strings.HasSuffix(owner, ".") {
		owner += "."
	}
	fqdn := stripZoneSuffix(owner, r.zone)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.staging[stagingKey{fqdn: fqdn, rrtype: dns.TypeToString[hdr.Rrtype]}] = stagingValue{blob: h}
	return nil
}

// Delete stages removal of (fqdn, rrtype). fqdn must be relative to the zone
// apex ("" or "@" for the apex itself).
func (r *Repo) Delete(fqdn, rrtype string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staging[stagingKey{fqdn: normFQDN(fqdn), rrtype: strings.ToUpper(rrtype)}] = stagingValue{tombstone: true}
}

// StagedCount returns the number of pending edits (used for `zonegit status`).
func (r *Repo) StagedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.staging)
}

// --- Import ---

// Import reads a zonefile and stages a Set for every RRset it contains.
func (r *Repo) Import(ctx context.Context, src io.Reader) (int, error) {
	if r.zone == "" {
		return 0, fmt.Errorf("Import: zone not set; call Init first")
	}
	rrsets, err := zone.ImportZonefile(src, r.zone)
	if err != nil {
		return 0, err
	}
	for _, rs := range rrsets {
		if err := r.Set(ctx, rs.RRs); err != nil {
			return 0, fmt.Errorf("Import: stage %s: %w", rs.Key, err)
		}
	}
	return len(rrsets), nil
}

// --- Commit ---

// Commit applies all staged edits as a single new commit on the current
// branch, advances HEAD, appends a reflog entry, and clears staging.
//
// Returns the new commit hash.
func (r *Repo) Commit(ctx context.Context, author object.Identity, msg string) (store.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.staging) == 0 {
		return store.ZeroHash, fmt.Errorf("Commit: no staged changes")
	}

	// Read HEAD (may be orphan).
	branch, parent, err := r.refs.ReadHEAD(ctx)
	if err != nil {
		return store.ZeroHash, fmt.Errorf("Commit: read HEAD: %w", err)
	}
	var parentTree store.Hash
	if !parent.IsZero() {
		obj, err := r.storage.GetObject(ctx, parent)
		if err != nil {
			return store.ZeroHash, err
		}
		pc, err := object.DecodeCommit(obj.Payload)
		if err != nil {
			return store.ZeroHash, err
		}
		parentTree = pc.Tree
	}

	// Apply each staged edit in deterministic order.
	keys := sortedKeys(r.staging)
	tree := parentTree
	for _, k := range keys {
		v := r.staging[k]
		path := splitFQDN(k.fqdn)
		var leaf store.Hash
		if !v.tombstone {
			leaf = v.blob
		}
		newRoot, err := object.UpdateTree(ctx, r.storage, tree, path, k.rrtype, leaf)
		if err != nil {
			return store.ZeroHash, fmt.Errorf("Commit: update %s %s: %w", k.fqdn, k.rrtype, err)
		}
		tree = newRoot
	}

	now := time.Now()
	parents := []store.Hash{}
	if !parent.IsZero() {
		parents = append(parents, parent)
	}
	c := object.Commit{
		Tree:       tree,
		Parents:    parents,
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

	// Advance branch ref via CAS (handles both create and update).
	branchName := strings.TrimPrefix(branch, refs.BranchPrefix)
	if parent.IsZero() {
		if err := r.refs.CreateBranch(ctx, branchName, commitHash); err != nil {
			return store.ZeroHash, err
		}
	} else {
		if err := r.refs.UpdateBranch(ctx, branchName, parent, commitHash); err != nil {
			return store.ZeroHash, err
		}
	}

	// Reflog.
	_ = r.refs.AppendReflog(ctx, branch, parent, commitHash, author.String(), "commit", msg)

	// Clear staging.
	r.staging = make(map[stagingKey]stagingValue)
	return commitHash, nil
}

// --- Read API ---

// Head returns (branch, commit) where branch is "refs/heads/X". commit may
// be ZeroHash for an orphan branch.
func (r *Repo) Head(ctx context.Context) (string, store.Hash, error) {
	return r.refs.ReadHEAD(ctx)
}

// Resolve parses a ref-ish string (see refs.DB.Resolve for forms).
func (r *Repo) Resolve(ctx context.Context, refish string) (store.Hash, error) {
	return r.refs.Resolve(ctx, refish)
}

// Log returns up to max commits in first-parent order from refish (default
// HEAD if "").
func (r *Repo) Log(ctx context.Context, refish string, max int) ([]history.Entry, error) {
	if refish == "" {
		refish = "HEAD"
	}
	h, err := r.refs.Resolve(ctx, refish)
	if err != nil {
		return nil, err
	}
	return history.Log(ctx, r.storage, h, max)
}

// Diff computes the RRset-level changes between two ref-ish points.
func (r *Repo) Diff(ctx context.Context, fromRefish, toRefish string) ([]history.Change, error) {
	a, err := r.refs.Resolve(ctx, fromRefish)
	if err != nil {
		return nil, err
	}
	b, err := r.refs.Resolve(ctx, toRefish)
	if err != nil {
		return nil, err
	}
	return history.Diff(ctx, r.storage, treeOf(ctx, r.storage, a), treeOf(ctx, r.storage, b))
}

// Blame returns the commit that introduced the current value of (fqdn, rrtype)
// at HEAD.
func (r *Repo) Blame(ctx context.Context, fqdn, rrtype string) (history.BlameInfo, error) {
	_, head, err := r.refs.ReadHEAD(ctx)
	if err != nil {
		return history.BlameInfo{}, err
	}
	if head.IsZero() {
		return history.BlameInfo{}, fmt.Errorf("Blame: HEAD is empty")
	}
	return history.Blame(ctx, r.storage, head, splitFQDN(normFQDN(fqdn)), strings.ToUpper(rrtype))
}

// Lookup returns the canonical-form blob payload for (fqdn, rrtype) at the
// given commit (or HEAD if commit is zero). Returns ErrNotFound if absent.
func (r *Repo) Lookup(ctx context.Context, commit store.Hash, fqdn, rrtype string) (zone.RRset, error) {
	if commit.IsZero() {
		_, h, err := r.refs.ReadHEAD(ctx)
		if err != nil {
			return zone.RRset{}, err
		}
		commit = h
	}
	if commit.IsZero() {
		return zone.RRset{}, fmt.Errorf("Lookup: empty repo")
	}
	obj, err := r.storage.GetObject(ctx, commit)
	if err != nil {
		return zone.RRset{}, err
	}
	c, err := object.DecodeCommit(obj.Payload)
	if err != nil {
		return zone.RRset{}, err
	}
	blobHash, err := object.WalkTree(ctx, r.storage, c.Tree, splitFQDN(normFQDN(fqdn)), strings.ToUpper(rrtype))
	if err != nil {
		return zone.RRset{}, err
	}
	bobj, err := r.storage.GetObject(ctx, blobHash)
	if err != nil {
		return zone.RRset{}, err
	}
	return zone.DecodeRRset(bobj.Payload)
}

// --- helpers ---

func canonZone(z string) string {
	z = strings.ToLower(z)
	if !strings.HasSuffix(z, ".") {
		z += "."
	}
	return z
}

// stripZoneSuffix turns "api.foo.com." into "api" given zone "foo.com.".
// Returns "" for the apex.
func stripZoneSuffix(owner, zoneName string) string {
	if owner == zoneName {
		return ""
	}
	if strings.HasSuffix(owner, "."+zoneName) {
		return strings.TrimSuffix(owner, "."+zoneName)
	}
	if strings.HasSuffix(owner, zoneName) {
		return strings.TrimSuffix(owner, zoneName)
	}
	// Out-of-zone — store under a synthetic path so we don't lose data.
	return strings.TrimSuffix(owner, ".")
}

// splitFQDN turns "api.web" into ["web", "api"] (deepest-label-last;
// the tree walker descends label-by-label so we want labels in
// path-order from zone-apex down).
//
// "" or "@" means apex (empty path).
func splitFQDN(fqdn string) []string {
	fqdn = normFQDN(fqdn)
	if fqdn == "" {
		return nil
	}
	parts := strings.Split(fqdn, ".")
	// Reverse so deepest is last? No — for api.foo.com. with zone foo.com.,
	// fqdn becomes "api" and path is ["api"]. For "api.web", we want
	// ["web", "api"] so that the tree under web/ contains api as a subtree.
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

func normFQDN(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".")
	if s == "@" {
		return ""
	}
	return s
}

func sortedKeys(m map[stagingKey]stagingValue) []stagingKey {
	out := make([]stagingKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Lexicographic on (fqdn, rrtype) for deterministic commit order.
	sortKeys(out)
	return out
}

func sortKeys(ks []stagingKey) {
	// Insertion sort — count is tiny in v0.
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0; j-- {
			if lessKey(ks[j], ks[j-1]) {
				ks[j], ks[j-1] = ks[j-1], ks[j]
			} else {
				break
			}
		}
	}
}

func lessKey(a, b stagingKey) bool {
	if a.fqdn != b.fqdn {
		return a.fqdn < b.fqdn
	}
	return a.rrtype < b.rrtype
}

// treeOf returns the tree hash referenced by a commit, or ZeroHash on error
// (callers must have already validated commit existence via Resolve).
func treeOf(ctx context.Context, s store.Storage, commit store.Hash) store.Hash {
	if commit.IsZero() {
		return store.ZeroHash
	}
	obj, err := s.GetObject(ctx, commit)
	if err != nil {
		return store.ZeroHash
	}
	c, err := object.DecodeCommit(obj.Payload)
	if err != nil {
		return store.ZeroHash
	}
	return c.Tree
}
