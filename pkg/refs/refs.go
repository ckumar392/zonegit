package refs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Well-known ref prefixes.
//
// v0.4 introduces multi-zone repos. Every branch and tag is scoped under
// its zone:
//
//	refs/heads/<zone>/<branch>     # branch tips
//	refs/tags/<zone>/<tag>         # tags
//	refs/zonegit/zones/<zone>      # presence marker for zone enumeration
//	HEAD                           # symbolic ref, encodes "<zone>/<branch>"
//
// Zone names are canonical FQDNs with trailing dot (e.g. "foo.com.").
// They participate verbatim in ref paths; the trailing dot makes a zone
// segment unambiguous against a branch named like the zone.
const (
	HeadRef          = "HEAD"
	BranchPrefix     = "refs/heads/"
	TagPrefix        = "refs/tags/"
	ZoneMarkerPrefix = "refs/zonegit/zones/"

	// LegacyZoneNameRef is the v0.3 single-zone marker. v0.4 detects it
	// on Open and refuses to proceed without migration. Kept as a constant
	// for that detection path.
	LegacyZoneNameRef = "refs/zonegit/zone"
)

// HEAD is stored as an object-backed symref: the HEAD ref points at the
// hash of a KindSymref object whose payload is the target ref path. This
// has no length limit (vs. the v0.3/early-v0.4 length-prefix-in-32-bytes
// scheme, which capped at 31 bytes).

// DB wraps a store.Storage and provides higher-level ref operations:
// zones, branches, HEAD, tags, reflog, and ref-ish resolution.
type DB struct {
	s store.Storage
}

// New returns a DB backed by s.
func New(s store.Storage) *DB { return &DB{s: s} }

// --- Zones (presence markers + enumeration) ---

// RegisterZone records the existence of a zone in this repo. Idempotent.
func (db *DB) RegisterZone(ctx context.Context, zone string) error {
	zone = CanonZone(zone)
	if zone == "" {
		return fmt.Errorf("RegisterZone: empty name")
	}
	ref := ZoneMarkerPrefix + zone
	// Zone markers are presence-only; we store a fixed sentinel value so
	// CAS(ZeroHash, sentinel) creates and any subsequent re-register is a
	// no-op via CAS(sentinel, sentinel).
	old, ok, err := db.s.GetRef(ctx, ref)
	if err != nil {
		return fmt.Errorf("RegisterZone: %w", err)
	}
	sentinel := zoneSentinel()
	if ok {
		if old == sentinel {
			return nil // already registered
		}
		// Some other content — refuse to clobber.
		return fmt.Errorf("RegisterZone: ref %s holds unexpected content", ref)
	}
	return db.s.CASRef(ctx, ref, store.ZeroHash, sentinel)
}

// UnregisterZone removes a zone marker. Refuses if any branches or tags
// still exist under that zone unless force is true.
func (db *DB) UnregisterZone(ctx context.Context, zone string, force bool) error {
	zone = CanonZone(zone)
	if !force {
		bs, err := db.ListBranches(ctx, zone)
		if err != nil {
			return err
		}
		if len(bs) > 0 {
			return fmt.Errorf("UnregisterZone: %s has %d branch(es); pass force=true to remove anyway", zone, len(bs))
		}
	}
	return db.s.DeleteRef(ctx, ZoneMarkerPrefix+zone, zoneSentinel())
}

// ListZones returns all registered zones, sorted.
func (db *DB) ListZones(ctx context.Context) ([]string, error) {
	entries, err := db.s.ListRefs(ctx, ZoneMarkerPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimPrefix(e.Name, ZoneMarkerPrefix))
	}
	sort.Strings(out)
	return out, nil
}

// IsZoneRegistered reports whether zone has been registered.
func (db *DB) IsZoneRegistered(ctx context.Context, zone string) (bool, error) {
	_, ok, err := db.s.GetRef(ctx, ZoneMarkerPrefix+CanonZone(zone))
	if err != nil {
		return false, err
	}
	return ok, nil
}

// --- HEAD (object-backed symref) ---

// ReadHEAD returns (zone, branch, commit). commit may be ZeroHash for an
// orphan branch (branch exists symbolically but has no commits yet).
//
// Returns store.ErrNotFound if HEAD is not set.
func (db *DB) ReadHEAD(ctx context.Context) (zone, branch string, commit store.Hash, err error) {
	headRef, ok, err := db.s.GetRef(ctx, HeadRef)
	if err != nil {
		return "", "", store.ZeroHash, fmt.Errorf("read HEAD: %w", err)
	}
	if !ok {
		return "", "", store.ZeroHash, fmt.Errorf("read HEAD: %w", store.ErrNotFound)
	}
	target, err := readSymref(ctx, db.s, headRef)
	if err != nil {
		return "", "", store.ZeroHash, fmt.Errorf("read HEAD: %w", err)
	}
	zone, branch, ok = parseBranchTarget(target)
	if !ok {
		return "", "", store.ZeroHash, fmt.Errorf("read HEAD: target %q is not a refs/heads/<zone>/<branch>", target)
	}
	commit, found, err := db.s.GetRef(ctx, target)
	if err != nil {
		return zone, branch, store.ZeroHash, fmt.Errorf("read HEAD -> %s: %w", target, err)
	}
	if !found {
		return zone, branch, store.ZeroHash, nil // orphan
	}
	return zone, branch, commit, nil
}

// SetHEAD points HEAD at refs/heads/<zone>/<branch>. No length limit.
func (db *DB) SetHEAD(ctx context.Context, zone, branch string) error {
	zone = CanonZone(zone)
	target := BranchPrefix + zone + "/" + branch
	newH, err := writeSymref(ctx, db.s, target)
	if err != nil {
		return fmt.Errorf("SetHEAD: %w", err)
	}
	old, ok, err := db.s.GetRef(ctx, HeadRef)
	if err != nil {
		return fmt.Errorf("SetHEAD: %w", err)
	}
	if !ok {
		return db.s.CASRef(ctx, HeadRef, store.ZeroHash, newH)
	}
	return db.s.CASRef(ctx, HeadRef, old, newH)
}

// writeSymref persists a KindSymref object whose payload is target.
func writeSymref(ctx context.Context, s store.Storage, target string) (store.Hash, error) {
	h, obj := object.Encode(object.KindSymref, []byte(target))
	if err := s.PutObject(ctx, h, obj); err != nil {
		return store.ZeroHash, err
	}
	return h, nil
}

// readSymref loads a symref object and returns its target. Falls back to
// the legacy length-prefix-in-32-bytes encoding when h is not a real
// object hash but a length-prefix slot — this is how we transparently
// read HEAD on a not-yet-migrated v0.3 repo.
func readSymref(ctx context.Context, s store.Storage, h store.Hash) (string, error) {
	obj, err := s.GetObject(ctx, h)
	if err == nil && obj.Kind == string(object.KindSymref) {
		return string(obj.Payload), nil
	}
	// Legacy fallback: HEAD ref slot held the target as a length-prefixed
	// ASCII string. Used by v0.3 repos before migration completes.
	if t := decodeLegacyRefSlot(h); t != "" {
		return t, nil
	}
	if err != nil {
		return "", fmt.Errorf("symref load: %w", err)
	}
	return "", fmt.Errorf("symref load: unexpected kind %q", obj.Kind)
}

// --- Branches (zone-scoped) ---

// BranchRef builds the full ref path for (zone, branch).
func BranchRef(zone, branch string) string {
	return BranchPrefix + CanonZone(zone) + "/" + branch
}

// TagRef builds the full ref path for (zone, tag).
func TagRef(zone, tag string) string {
	return TagPrefix + CanonZone(zone) + "/" + tag
}

func parseBranchTarget(target string) (zone, branch string, ok bool) {
	if !strings.HasPrefix(target, BranchPrefix) {
		return "", "", false
	}
	rest := target[len(BranchPrefix):]
	// Zone names end at the LAST "/" — zone is FQDN with trailing dot,
	// then "/", then a branch name (no slash allowed).
	slash := strings.LastIndex(rest, "/")
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", false
	}
	return rest[:slash], rest[slash+1:], true
}

// CreateBranch creates a new branch pointing at commit.
func (db *DB) CreateBranch(ctx context.Context, zone, name string, commit store.Hash) error {
	ref := BranchRef(zone, name)
	if err := db.s.CASRef(ctx, ref, store.ZeroHash, commit); err != nil {
		return fmt.Errorf("create branch %s/%s: %w", zone, name, err)
	}
	return nil
}

// UpdateBranch moves branch from expected to next (atomic CAS).
func (db *DB) UpdateBranch(ctx context.Context, zone, name string, expected, next store.Hash) error {
	return db.s.CASRef(ctx, BranchRef(zone, name), expected, next)
}

// DeleteBranch deletes a branch if it points at expected.
func (db *DB) DeleteBranch(ctx context.Context, zone, name string, expected store.Hash) error {
	return db.s.DeleteRef(ctx, BranchRef(zone, name), expected)
}

// GetBranch returns the hash a branch points to.
func (db *DB) GetBranch(ctx context.Context, zone, name string) (store.Hash, error) {
	ref := BranchRef(zone, name)
	h, ok, err := db.s.GetRef(ctx, ref)
	if err != nil {
		return h, err
	}
	if !ok {
		return h, fmt.Errorf("branch %s/%s: %w", zone, name, store.ErrNotFound)
	}
	return h, nil
}

// ListBranches returns all branch names in zone (without prefix), sorted.
func (db *DB) ListBranches(ctx context.Context, zone string) ([]string, error) {
	prefix := BranchPrefix + CanonZone(zone) + "/"
	entries, err := db.s.ListRefs(ctx, prefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimPrefix(e.Name, prefix))
	}
	sort.Strings(names)
	return names, nil
}

// --- Tags (zone-scoped) ---

// CreateTag creates a lightweight tag.
func (db *DB) CreateTag(ctx context.Context, zone, name string, target store.Hash) error {
	return db.s.CASRef(ctx, TagRef(zone, name), store.ZeroHash, target)
}

// GetTag returns the hash a tag points to.
func (db *DB) GetTag(ctx context.Context, zone, name string) (store.Hash, error) {
	ref := TagRef(zone, name)
	h, ok, err := db.s.GetRef(ctx, ref)
	if err != nil {
		return h, err
	}
	if !ok {
		return h, fmt.Errorf("tag %s/%s: %w", zone, name, store.ErrNotFound)
	}
	return h, nil
}

// ListTags returns all tag names in zone (without prefix), sorted.
func (db *DB) ListTags(ctx context.Context, zone string) ([]string, error) {
	prefix := TagPrefix + CanonZone(zone) + "/"
	entries, err := db.s.ListRefs(ctx, prefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimPrefix(e.Name, prefix))
	}
	sort.Strings(names)
	return names, nil
}

// DeleteTag deletes a tag if it points at expected.
func (db *DB) DeleteTag(ctx context.Context, zone, name string, expected store.Hash) error {
	return db.s.DeleteRef(ctx, TagRef(zone, name), expected)
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

// --- Legacy detection ---

// MigrateLegacyV03 converts a v0.3 single-zone repo to the v0.4 multi-zone
// layout in place.
//
// What changes:
//   - refs/heads/<branch>               → refs/heads/<zone>/<branch>
//   - refs/tags/<tag>                   → refs/tags/<zone>/<tag>
//   - HEAD (length-prefix legacy slot)  → KindSymref object pointing at the new branch path
//   - refs/zonegit/zones/<zone>         created (zone registration)
//   - refs/zonegit/zone                 deleted (legacy marker)
//
// The function is idempotent: if no migration is needed, it returns
// (migrated=false, zone="") with no error.
//
// Ordering matters for crash safety: each refs.* entry is copied to the
// new location first, then the old name is deleted. If we crash partway,
// the worst case is some orphan legacy refs alongside the new refs — both
// are reachable, the daemon only consults the new ones, and a future
// migration run cleans them up.
func (db *DB) MigrateLegacyV03(ctx context.Context) (bool, string, error) {
	legacy, zoneName, err := db.IsLegacyV03(ctx)
	if err != nil || !legacy {
		return false, "", err
	}
	zoneName = CanonZone(zoneName)
	if zoneName == "" {
		return false, "", fmt.Errorf("legacy zone marker present but empty")
	}

	// Recover legacy HEAD target so we know which branch to point HEAD at
	// after migration. v0.3 stored HEAD as a length-prefixed ref slot
	// holding "refs/heads/<branch>".
	headBranch := "main"
	if headSlot, ok, err := db.s.GetRef(ctx, HeadRef); err == nil && ok {
		legacyTarget := decodeLegacyRefSlot(headSlot)
		if rest := strings.TrimPrefix(legacyTarget, BranchPrefix); rest != "" && !strings.Contains(rest, "/") {
			headBranch = rest
		}
	}

	// Step 1: register the zone (no-op if already present).
	if err := db.RegisterZone(ctx, zoneName); err != nil {
		return false, "", fmt.Errorf("migrate: register zone: %w", err)
	}

	// Step 2: migrate every legacy branch ref. Skip entries already in
	// the new form (they contain a "/" after the prefix).
	branchEntries, err := db.s.ListRefs(ctx, BranchPrefix)
	if err != nil {
		return false, "", fmt.Errorf("migrate: list branches: %w", err)
	}
	for _, e := range branchEntries {
		rest := strings.TrimPrefix(e.Name, BranchPrefix)
		if strings.Contains(rest, "/") {
			continue // already migrated
		}
		newRef := BranchRef(zoneName, rest)
		if err := db.s.CASRef(ctx, newRef, store.ZeroHash, e.Hash); err != nil {
			return false, "", fmt.Errorf("migrate: copy branch %s: %w", rest, err)
		}
		if err := db.s.DeleteRef(ctx, e.Name, e.Hash); err != nil {
			return false, "", fmt.Errorf("migrate: delete legacy branch %s: %w", rest, err)
		}
	}

	// Step 3: migrate every legacy tag ref the same way.
	tagEntries, err := db.s.ListRefs(ctx, TagPrefix)
	if err != nil {
		return false, "", fmt.Errorf("migrate: list tags: %w", err)
	}
	for _, e := range tagEntries {
		rest := strings.TrimPrefix(e.Name, TagPrefix)
		if strings.Contains(rest, "/") {
			continue
		}
		newRef := TagRef(zoneName, rest)
		if err := db.s.CASRef(ctx, newRef, store.ZeroHash, e.Hash); err != nil {
			return false, "", fmt.Errorf("migrate: copy tag %s: %w", rest, err)
		}
		if err := db.s.DeleteRef(ctx, e.Name, e.Hash); err != nil {
			return false, "", fmt.Errorf("migrate: delete legacy tag %s: %w", rest, err)
		}
	}

	// Step 4: rewrite HEAD as an object-backed symref.
	if err := db.SetHEAD(ctx, zoneName, headBranch); err != nil {
		return false, "", fmt.Errorf("migrate: rewrite HEAD: %w", err)
	}

	// Step 5: delete the legacy zone-name marker. From this point
	// IsLegacyV03 returns false on future opens.
	if h, ok, _ := db.s.GetRef(ctx, LegacyZoneNameRef); ok {
		_ = db.s.DeleteRef(ctx, LegacyZoneNameRef, h)
	}

	return true, zoneName, nil
}

// IsLegacyV03 reports whether this repo was created by zonegit v0.3 or
// earlier (single-zone layout) and has not yet been migrated. Used by
// pkg/repo.Open to emit a clear, actionable error.
func (db *DB) IsLegacyV03(ctx context.Context) (bool, string, error) {
	h, ok, err := db.s.GetRef(ctx, LegacyZoneNameRef)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "", nil
	}
	zones, err := db.ListZones(ctx)
	if err != nil {
		return false, "", err
	}
	if len(zones) > 0 {
		// Both legacy marker AND new layout — already migrated; the
		// legacy ref is harmless dead weight.
		return false, "", nil
	}
	return true, decodeLegacyRefSlot(h), nil
}

// --- Resolve ref-ish ---

// Resolve parses a ref-ish string and returns the commit hash it points
// to. Supported forms:
//
//   - Full hex hash (64 chars)
//   - "HEAD" / "HEAD~N"
//   - Bare branch name "main" — resolved within the active zone
//   - Zone-qualified branch "foo.com./main" — explicit zone
//   - Tag name "v1" — within the active zone
//   - Full ref path "refs/heads/foo.com./main" or "refs/tags/foo.com./v1"
//
// Multi-zone-aware: bare names use the active zone derived from HEAD.
// Pass ResolveInZone for cross-zone resolution.
func (db *DB) Resolve(ctx context.Context, refish string) (store.Hash, error) {
	base, ancestor := splitAncestor(refish)
	h, err := db.resolveBase(ctx, base, "")
	if err != nil {
		return store.ZeroHash, fmt.Errorf("resolve %q: %w", refish, err)
	}
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
		h = c.Parents[0]
	}
	return h, nil
}

// ResolveInZone is like Resolve but pins bare branch / tag lookups to
// the given zone instead of reading HEAD. Useful for the CLI's --zone
// override.
func (db *DB) ResolveInZone(ctx context.Context, zone, refish string) (store.Hash, error) {
	base, ancestor := splitAncestor(refish)
	h, err := db.resolveBase(ctx, base, CanonZone(zone))
	if err != nil {
		return store.ZeroHash, fmt.Errorf("resolve %q in %s: %w", refish, zone, err)
	}
	for i := 0; i < ancestor; i++ {
		obj, err := db.s.GetObject(ctx, h)
		if err != nil {
			return store.ZeroHash, err
		}
		c, err := object.DecodeCommit(obj.Payload)
		if err != nil {
			return store.ZeroHash, err
		}
		if len(c.Parents) == 0 {
			return store.ZeroHash, fmt.Errorf("resolve %q in %s: no parent at ~%d", refish, zone, i+1)
		}
		h = c.Parents[0]
	}
	return h, nil
}

func (db *DB) resolveBase(ctx context.Context, base, zoneOverride string) (store.Hash, error) {
	// 1) Full hex hash?
	if len(base) == 2*store.HashSize {
		return store.ParseHash(base)
	}

	// 2) HEAD?
	if base == HeadRef {
		_, _, commit, err := db.ReadHEAD(ctx)
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

	// 4) Zone-qualified branch "<zone>/<branch>"?  Zone segment ends with
	// a dot; branch name does not contain a slash.
	if slash := strings.LastIndex(base, "/"); slash > 0 {
		zone := base[:slash]
		name := base[slash+1:]
		if h, ok, err := db.s.GetRef(ctx, BranchRef(zone, name)); err != nil {
			return store.ZeroHash, err
		} else if ok {
			return h, nil
		}
		if h, ok, err := db.s.GetRef(ctx, TagRef(zone, name)); err != nil {
			return store.ZeroHash, err
		} else if ok {
			return h, nil
		}
	}

	// 5) Bare name — resolve against active or overridden zone.
	zone := zoneOverride
	if zone == "" {
		z, _, _, err := db.ReadHEAD(ctx)
		if err != nil {
			return store.ZeroHash, fmt.Errorf("bare ref %q: cannot resolve without HEAD: %w", base, err)
		}
		zone = z
	}
	if h, ok, err := db.s.GetRef(ctx, BranchRef(zone, base)); err != nil {
		return store.ZeroHash, err
	} else if ok {
		return h, nil
	}
	if h, ok, err := db.s.GetRef(ctx, TagRef(zone, base)); err != nil {
		return store.ZeroHash, err
	} else if ok {
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
		return s, 0
	}
	return base, n
}

// --- low-level encoding helpers ---

// CanonZone normalises a zone name to lowercase with trailing dot.
func CanonZone(z string) string {
	z = strings.ToLower(z)
	if z == "" {
		return ""
	}
	if !strings.HasSuffix(z, ".") {
		z += "."
	}
	return z
}

// zoneSentinel is the fixed marker value used for ZoneMarkerPrefix refs.
// The exact bytes are arbitrary — we just need a non-zero hash that
// won't collide with a real commit hash.
func zoneSentinel() store.Hash {
	var h store.Hash
	copy(h[:], "zonegit:zone:marker:v04")
	return h
}

// decodeLegacyRefSlot reads the v0.3 length-prefix-in-32-bytes encoding
// (1-byte length, then ASCII payload). Used only to (a) read a
// not-yet-migrated v0.3 HEAD, and (b) decode the legacy single-zone
// marker at LegacyZoneNameRef.
//
// Returns "" if the slot doesn't look like a legacy encoding.
func decodeLegacyRefSlot(h store.Hash) string {
	n := int(h[0])
	if n == 0 || n > store.HashSize-1 {
		return ""
	}
	return string(h[1 : 1+n])
}
