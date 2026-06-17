package repo

import (
	"context"
	"errors"
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

// DefaultBranch is the branch new zones get on first init.
const DefaultBranch = "main"

// ErrReadOnlyMigrationNeeded is returned by Open when a v0.3 repo is
// opened read-only — migration is a write operation, so the user must
// re-open without ReadOnly to convert it. This keeps the daemon from
// silently mutating a repo a separate writer process is also holding.
var ErrReadOnlyMigrationNeeded = errors.New("legacy v0.3 repo opened read-only; re-open writable to auto-migrate")

// Repo is the public zonegit handle.
//
// In v0.4 a repo can hold multiple zones. Mutations (Set, Delete, Commit)
// operate on the active zone — the zone HEAD currently points at. The
// CLI's `--zone` flag and the SwitchZone API move HEAD between zones.
// All write paths take a per-Repo write lock to avoid useless CAS
// retries against ourselves; concurrent readers are unblocked.
type Repo struct {
	storage store.Storage
	refs    *refs.DB

	mu      sync.Mutex
	staging map[stagingKey]stagingValue

	// activeZone is the zone derived from HEAD at Open / SwitchZone time.
	// Read paths that need the zone (Set, Import, Lookup) consult this
	// field instead of re-parsing HEAD on every call.
	activeZone string
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
// path-based Storage).
//
// If the repo uses the v0.3 single-zone layout, Open returns
// ErrLegacyRepo with the legacy zone name in the wrapped message; the
// caller is expected to surface a migration instruction.
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
	r := &Repo{
		storage: s,
		refs:    refs.New(s),
		staging: make(map[stagingKey]stagingValue),
	}

	ctx := context.Background()
	if legacy, _, err := r.refs.IsLegacyV03(ctx); err == nil && legacy {
		if opts.ReadOnly {
			_ = r.storage.Close()
			return nil, ErrReadOnlyMigrationNeeded
		}
		if _, _, err := r.refs.MigrateLegacyV03(ctx); err != nil {
			_ = r.storage.Close()
			return nil, fmt.Errorf("auto-migrate legacy repo: %w", err)
		}
	}

	// Populate activeZone from HEAD if HEAD is set. Brand-new repos have
	// no HEAD yet — that's fine; Init will set it.
	if zoneName, _, _, err := r.refs.ReadHEAD(ctx); err == nil && zoneName != "" {
		r.activeZone = zoneName
	}
	return r, nil
}

// Close releases the underlying storage.
func (r *Repo) Close() error { return r.storage.Close() }

// Storage returns the underlying store.Storage. Useful for tests and
// for embedders that need lower-level access.
func (r *Repo) Storage() store.Storage { return r.storage }

// Refs returns the underlying refs.DB.
func (r *Repo) Refs() *refs.DB { return r.refs }

// --- Init / zones ---

// Init creates an empty repo registering zone with a default branch and
// pointing HEAD at it. If the zone already exists, Init is a no-op for
// that zone and leaves HEAD alone.
//
// Multi-zone repos call Init for the first zone and AddZone for each
// subsequent zone; behaviour is equivalent except Init also sets HEAD
// if it isn't already set.
func (r *Repo) Init(ctx context.Context, zoneName string) error {
	zoneName = canonZone(zoneName)
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.refs.RegisterZone(ctx, zoneName); err != nil {
		return fmt.Errorf("Init: register zone: %w", err)
	}
	// If HEAD is not set yet, point it at this zone's default branch.
	if _, _, _, err := r.refs.ReadHEAD(ctx); err != nil {
		if err := r.refs.SetHEAD(ctx, zoneName, DefaultBranch); err != nil {
			return fmt.Errorf("Init: set HEAD: %w", err)
		}
		r.activeZone = zoneName
	}
	return nil
}

// AddZone registers an additional zone without moving HEAD. Use this to
// host a second zone in an existing repo.
func (r *Repo) AddZone(ctx context.Context, zoneName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refs.RegisterZone(ctx, canonZone(zoneName))
}

// Zones returns all registered zones in the repo, sorted.
func (r *Repo) Zones(ctx context.Context) ([]string, error) {
	return r.refs.ListZones(ctx)
}

// ActiveZone returns the zone HEAD currently points at, or "" if HEAD
// is unset.
func (r *Repo) ActiveZone() string { return r.activeZone }

// SwitchZone moves HEAD to (zone, branch) and refreshes the cached
// active zone. The zone must already be registered; branch may or may
// not exist (orphan branches are allowed).
func (r *Repo) SwitchZone(ctx context.Context, zoneName, branch string) error {
	zoneName = canonZone(zoneName)
	r.mu.Lock()
	defer r.mu.Unlock()
	ok, err := r.refs.IsZoneRegistered(ctx, zoneName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("SwitchZone: zone %q is not registered", zoneName)
	}
	if err := r.refs.SetHEAD(ctx, zoneName, branch); err != nil {
		return err
	}
	r.activeZone = zoneName
	return nil
}

// --- Mutation API (operates on the active zone) ---

// Set stages an RRset write to be applied on the next Commit. RRs are
// homogenized (same name/class/type/ttl) per pkg/zone rules.
//
// The RR owner name is interpreted relative to the active zone.
func (r *Repo) Set(ctx context.Context, rrs []dns.RR) error {
	if len(rrs) == 0 {
		return fmt.Errorf("Set: empty RRset")
	}
	if r.activeZone == "" {
		return fmt.Errorf("Set: no active zone; call Init or SwitchZone first")
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
	fqdn := stripZoneSuffix(owner, r.activeZone)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.staging[stagingKey{fqdn: fqdn, rrtype: dns.TypeToString[hdr.Rrtype]}] = stagingValue{blob: h}
	return nil
}

// Delete stages removal of (fqdn, rrtype). fqdn must be relative to the
// active zone apex ("" or "@" for the apex itself).
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
// The zonefile is parsed against the active zone's apex.
func (r *Repo) Import(ctx context.Context, src io.Reader) (int, error) {
	if r.activeZone == "" {
		return 0, fmt.Errorf("Import: no active zone; call Init or SwitchZone first")
	}
	rrsets, err := zone.ImportZonefile(src, r.activeZone)
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

// Commit applies all staged edits as a single new commit on the active
// branch (the branch HEAD points at), advances the branch ref, appends
// a reflog entry, and clears staging.
func (r *Repo) Commit(ctx context.Context, author object.Identity, msg string) (store.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.staging) == 0 {
		return store.ZeroHash, fmt.Errorf("Commit: no staged changes")
	}

	zoneName, branch, parent, err := r.refs.ReadHEAD(ctx)
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

	if err := r.maybeBumpSOA(ctx, parentTree); err != nil {
		return store.ZeroHash, fmt.Errorf("Commit: auto-bump SOA: %w", err)
	}

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

	if parent.IsZero() {
		if err := r.refs.CreateBranch(ctx, zoneName, branch, commitHash); err != nil {
			return store.ZeroHash, err
		}
	} else {
		if err := r.refs.UpdateBranch(ctx, zoneName, branch, parent, commitHash); err != nil {
			return store.ZeroHash, err
		}
	}

	_ = r.refs.AppendReflog(ctx, refs.BranchRef(zoneName, branch), parent, commitHash, author.String(), "commit", msg)
	r.staging = make(map[stagingKey]stagingValue)
	return commitHash, nil
}

// --- Read API ---

// Head returns (zone, branch, commit) where (zone, branch) is what HEAD
// points at. commit may be ZeroHash for an orphan branch.
func (r *Repo) Head(ctx context.Context) (zoneName, branch string, commit store.Hash, err error) {
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
	return history.Diff(ctx, r.storage, object.TreeOf(ctx, r.storage, a), object.TreeOf(ctx, r.storage, b))
}

// Blame returns the commit that introduced the current value of (fqdn, rrtype)
// at HEAD.
func (r *Repo) Blame(ctx context.Context, fqdn, rrtype string) (history.BlameInfo, error) {
	_, _, head, err := r.refs.ReadHEAD(ctx)
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
		_, _, h, err := r.refs.ReadHEAD(ctx)
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
	if z == "" {
		return ""
	}
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
	sortKeys(out)
	return out
}

func sortKeys(ks []stagingKey) {
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

// maybeBumpSOA auto-stages an SOA with serial+1 when the staging area has
// at least one non-SOA edit, no explicit SOA edit, and the parent tree
// has an apex SOA to bump.
//
// Called from Commit while r.mu is held.
func (r *Repo) maybeBumpSOA(ctx context.Context, parentTree store.Hash) error {
	if parentTree.IsZero() {
		return nil
	}
	hasNonSOA := false
	for k := range r.staging {
		if k.rrtype == "SOA" && k.fqdn == "" {
			return nil
		}
		hasNonSOA = true
	}
	if !hasNonSOA {
		return nil
	}
	soaBlobHash, err := object.WalkTree(ctx, r.storage, parentTree, nil, "SOA")
	if err != nil {
		// No apex SOA in the parent yet (e.g. the user is editing a
		// zone that doesn't have one). Silently skip — auto-bumping
		// requires something to bump.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	obj, err := r.storage.GetObject(ctx, soaBlobHash)
	if err != nil {
		return err
	}
	soa, err := zone.DecodeRRset(obj.Payload)
	if err != nil {
		return err
	}
	bumped, err := zone.BumpSOASerial(soa)
	if err != nil {
		return err
	}
	payload, err := zone.EncodeRRset(bumped.RRs)
	if err != nil {
		return err
	}
	b := object.Blob{Payload: payload}
	h, blobObj := b.Encode()
	if err := r.storage.PutObject(ctx, h, blobObj); err != nil {
		return err
	}
	r.staging[stagingKey{fqdn: "", rrtype: "SOA"}] = stagingValue{blob: h}
	return nil
}
