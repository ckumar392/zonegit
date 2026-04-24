# zonegit Object Model

> **Read this before writing code.** Every other module derives from these
> shapes. If we get this right, branches/merge/canary/signing are all
> additive in later versions. If we get this wrong, we rewrite.

---

## 1. Design principles (non-negotiable)

1. **Content-addressed** — every object's identity is `SHA-256(canonical_bytes)`.
   Equal content ⇒ equal hash ⇒ deduplicated automatically.
2. **Immutable** — objects are never modified after write. New state = new
   object. This is what makes time-travel, audit, and forensics free.
3. **Canonical form is law** — two RRsets that mean the same thing MUST hash
   to the same value. Otherwise dedup, diff, and merge all break. Canonical
   form is defined per-object-kind below.
4. **Branches are pointers, not copies** — a branch is a 32-byte commit hash
   in a ref store. Creating a branch is O(1) and free. This is what makes
   canary serving (UC5) viable.
5. **Storage-agnostic** — object kinds know their canonical bytes; they do
   not know whether they live in Badger, Postgres, S3, or memory. The
   `store` interface is the only seam.
6. **Future-proof headers** — every object on disk carries a kind tag and a
   format version. We can evolve formats without breaking old repos.

---

## 2. Object kinds

There are exactly **five** object kinds in zonegit. Anything else is built on top.

| Kind | Purpose | Hashed? | Mutable? |
|---|---|---|---|
| `Blob` (RRset) | One RRset (name+type+class + TTL + sorted RR rdata list) | Yes | No |
| `Tree` | Sorted list of (label-path → blob-hash) entries forming a zone snapshot | Yes | No |
| `Commit` | (parent[s], tree, author, msg, timestamp, signature) | Yes | No |
| `Tag` | Named, optionally-signed pointer to a commit | Yes | No |
| `Ref` | Mutable pointer (branch / HEAD) → commit hash | **No** (just a name→hash map) | Yes |

`Ref` is the *only* mutable thing in the system. Everything else is immutable
forever once written.

---

## 3. Wire format header (all hashed objects)

Every hashed object begins with:

```
<kind> SP <length> NUL <payload>
```

- `kind` — ASCII string, one of `blob`, `tree`, `commit`, `tag`
- `length` — decimal ASCII length of payload in bytes
- payload follows immediately after `\x00`

This is intentionally identical to Git's loose object header. Reusing a
proven format means we get tooling, mental models, and a clear mapping for
anyone who knows Git.

The hash is computed over `<kind> SP <length> NUL <payload>` — header
included. This prevents trivial collision attacks where two different kinds
of object share the same payload.

```go
// pseudocode
func Hash(kind string, payload []byte) [32]byte {
    h := sha256.New()
    fmt.Fprintf(h, "%s %d\x00", kind, len(payload))
    h.Write(payload)
    return h.Sum(nil) // truncate to [32]byte
}
```

### Hash representation

- Internally: `[32]byte` (raw)
- On disk / in refs: 64 lowercase hex chars
- Display: 12-char abbreviation (`a3f29c81b04e`) like Git short SHAs

---

## 4. Blob (RRset)

A **Blob** stores one RRset. RRset = the unit DNS actually speaks: all RRs
sharing `(owner_name, type, class)`.

### Why RRset, not RR?

Because:
- DNS responses are RRsets, not individual RRs
- DNSSEC signs RRsets, not RRs
- Most "edits" semantically affect whole RRsets ("change A records for
  api.foo.com" not "change exactly one A among five")

### Canonical payload format

```
version    : uint8                       // currently 1
owner      : DNS name in RFC 4034 §6.2 canonical form (lowercase, length-prefixed labels)
class      : uint16 (network order)
type       : uint16
ttl        : uint32                      // see TTL note below
rr_count   : uint16
rr_records : sorted [count]rdata
```

Where each `rdata`:
```
length : uint16
bytes  : []byte    // type-specific wire format per miekg/dns Pack()
```

### Sorting rule

`rr_records` is sorted by **canonical RDATA bytes**, lexicographically.
This ensures `{1.2.3.4, 5.6.7.8}` and `{5.6.7.8, 1.2.3.4}` produce the
same blob hash.

### TTL handling

TTL is part of the canonical form. Two RRsets that differ only in TTL are
two distinct blobs. This is correct: changing a TTL is a real DNS change
and must be tracked. (Git analogy: changing whitespace creates a new blob;
that's fine.)

### Example

```
owner=api.foo.com.  type=A  class=IN  ttl=300  rdata=[1.2.3.4, 5.6.7.8]
```
hashes to a unique 32-byte blob ID, regardless of how it's serialized
on the wire.

---

## 5. Tree

A **Tree** represents a zone snapshot — the full set of RRsets at one moment.

### Naive design (rejected)

A flat list of `(owner_name, type, blob_hash)`. Problem: a 1M-record zone
gives a 50 MB tree object that has to be fully rewritten on any change.
Hashing the whole tree on every commit kills performance.

### Chosen design — **labelwise tree** (mirrors Git's tree-of-trees)

A Tree node represents one label-segment of the zone hierarchy. Children
are either:
- **Sub-trees** (for non-leaf labels), or
- **Leaf entries** mapping `type → blob_hash` (for owner names with RRsets)

Example for `foo.com` zone:

```
Tree("foo.com.")
├── A     → blob:abc...    (apex A records)
├── NS    → blob:def...    (apex NS)
├── SOA   → blob:111...
├── "api"
│   └── Tree
│       ├── A     → blob:222...
│       └── AAAA  → blob:333...
└── "internal"
    └── Tree
        ├── "db"
        │   └── Tree
        │       └── A → blob:444...
        └── "cache"
            └── Tree
                └── A → blob:555...
```

### Canonical payload format

```
version  : uint8                 // currently 1
entries  : sorted [n]entry

entry =
  type_byte : uint8              // 0 = subtree, 1 = leaf-rrset
  name_len  : uint8              // length of label OR rrtype string
  name      : []byte             // label (subtree) or rrtype mnemonic (leaf)
  hash      : [32]byte           // child tree hash (subtree) or blob hash (leaf)
```

Entries are sorted by `(type_byte, name)` ascending. Subtrees come before
leaves at the same level (type_byte=0 < 1). Within each kind, names are
compared as canonical (lowercase) byte strings.

### Why this design wins

| Property | Flat list | Labelwise tree |
|---|---|---|
| O(1) dedup of unchanged subtrees across commits | ❌ | ✅ |
| Diff cost between commits | O(zone size) | O(changed branch) |
| Memory to load just `api.foo.com` | full zone | path-walk only |
| Replication payload | full zone | only changed paths |
| Mental model | flat | matches DNS hierarchy |

The Linux kernel commits 30k+ files but git diff between two commits
touches only changed subtrees because of this exact design. We get the
same property: even a 10M-record zone has cheap diffs if only a handful
changed.

---

## 6. Commit

A **Commit** is the version identifier — what `zonegit log` walks.

### Canonical payload format (text, line-oriented, like Git)

```
version 1
tree <hex hash>
parent <hex hash>          // zero or more lines; root commit has none
author <name> <email> <unix_ts> <tz_offset>
committer <name> <email> <unix_ts> <tz_offset>
[signature ed25519 <base64>]   // optional, present iff signed (v3)
[selector <expr>]              // optional, see Canary section

<blank line>
<message>
```

### Why text format

- Trivial to debug with hex tools
- Easy to extend (new optional headers like `signature`, `selector` don't
  break old parsers — unknown headers ignored)
- Direct path to a `zonegit cat-commit <hash>` plumbing command

### Multi-parent commits

A commit can have 0, 1, or 2+ parents:
- **0 parents** — root commit (zone init)
- **1 parent** — normal commit
- **2 parents** — merge commit (v1+)

This is how `zonegit merge` records that two branches converged.

### Signature header

In v3, `signature` carries an Ed25519 sig over the canonical commit bytes
*excluding* the `signature` line itself (recursive-signature problem
solved the Git way). The signing key identity is referenced by a fingerprint
that resolves to a key in the (out-of-band) key store.

### Selector header (forward-compat for v2 canary)

We reserve the `selector` header now even though we won't use it until v2.
A commit on a canary branch may carry an embedded selector expression, e.g.:

```
selector client.subnet=10.0.0.0/8
```

The server's resolver consults this when deciding which branch to serve a
given query against. **Reserving the slot now means v0 commits remain
forward-compatible** — old commits just have no selector.

---

## 7. Tag

A **Tag** is an immutable named pointer with optional signature and message.
Use cases: `v2026.04.25-prod`, `pre-migration-snapshot`, audit checkpoints.

### Canonical payload

```
version 1
object <commit hash>
type commit
tag <name>
tagger <name> <email> <unix_ts> <tz_offset>
[signature ed25519 <base64>]

<blank line>
<message>
```

Once a tag is written, the (name → tag-hash) ref entry is conventionally
also immutable — overwriting a tag is technically possible but flagged
loudly by the CLI ("tag already exists; use --force"). We emit a reflog
entry on overwrite so even forced changes are forensically traceable.

---

## 8. Refs (the only mutable thing)

A **Ref** is a name → hash mapping. That's it.

### Layout

```
refs/
├── heads/
│   ├── main          → <commit-hash>
│   ├── canary        → <commit-hash>
│   └── staging       → <commit-hash>
├── tags/
│   ├── v2026.04.25   → <tag-hash>
│   └── pre-migrate   → <tag-hash>
└── HEAD              → "ref: refs/heads/main"   (or detached: "<commit-hash>")
```

### Atomic update protocol

1. Read current value of `refs/heads/X` (the *expected* value)
2. Compute new commit on top of expected value
3. Compare-and-swap: write new value iff current still equals expected
4. On success: append reflog entry `(ref, old, new, who, when, msg)`
5. On conflict: caller retries (rebases new commit onto current head)

This gives us the same race-safety as `git push --force-with-lease` and is
the foundation for safe concurrent edits.

### Reflog

Append-only log. Format per ref:

```
<old-hash> <new-hash> <author> <unix_ts> <tz>  <ref-op>  <message>
```

Never compacted, never deleted (in v0). Forensic recovery: `zonegit reflog
<branch>` shows every movement. Recovery: `zonegit reset --hard <hash>` from
any past entry.

---

## 9. Working tree (server-resolution view)

The "working tree" is the materialized state the **DNS server** resolves
against. In zonegit it's nothing more than:

> **A pointer (HEAD or chosen branch) to a commit, plus a cache of that
> commit's tree for fast lookup.**

There is no separate mutable "live state." The server resolves
`api.foo.com.` as:

```
1. tree_hash := commit_for_branch(branch).tree
2. blob_hash := walk_tree(tree_hash, ["com", "foo", "api"], type=A)
3. blob      := load_blob(blob_hash)
4. response  := decode_rrset(blob)
```

This means **flipping a branch ref is the entire cutover**. No reload,
no rebuild, no propagation. Microseconds. This is why `zonegit merge canary
main` is atomic from a serving perspective.

For canary mode (v2), the resolver evaluates a list of `(selector, branch)`
rules in order before falling back to the default branch. Same algorithm,
just preceded by a selector match.

---

## 10. Storage layout (Badger v0)

Two logical key spaces:

```
obj/<hex-hash>            → <object bytes including header>
ref/<ref path>            → <hex hash bytes>
reflog/<ref path>         → <append-only log entries>
HEAD                      → <"ref: refs/heads/main" or "<hex hash>">
```

Why two spaces:
- `obj/` is content-addressed and append-only forever. Perfect for
  Badger's LSM (lots of writes, no overwrites, GC well-defined).
- `ref/` is mutable. Small. Small CAS operations are easy.

Postgres adapter (v4) maps the same two spaces to two tables:
`objects(hash PK, kind, payload)` and `refs(name PK, hash, updated_at)`.

---

## 11. Worked example: committing one A-record change

Starting state: `main` points to commit `C0` with tree `T0` containing
RRset `api.foo.com A 1.2.3.4` as blob `B0`.

User runs:
```
zonegit set api.foo.com A 5.6.7.8
zonegit commit -m "promote new api"
```

Steps:
1. New blob `B1` for `{api.foo.com, A, IN, 300, [5.6.7.8]}`. Hash computed
   from canonical form. Stored under `obj/B1`.
2. Walk T0 to `api.foo.com` path. Replace leaf `A → B0` with `A → B1`.
3. Re-hash that leaf's parent tree. Re-hash *only the path back to the
   root*: `Tree(api) → Tree(foo) → Tree(root)`. Sibling subtrees are
   untouched and reused by hash. New root tree: `T1`.
4. New commit `C1`: parent=C0, tree=T1, author=..., msg="promote new api",
   timestamp=now. Stored under `obj/C1`.
5. CAS update `ref/refs/heads/main`: expected=C0, new=C1.
6. Append reflog entry: `C0 C1 alice@ ts commit "promote new api"`.

Total new objects: 1 blob + 3 trees + 1 commit = 5 small writes.
Total old objects: untouched, still referenced by `C0`, still resolvable.

`zonegit resolve api.foo.com A --at C0` returns 1.2.3.4.
`zonegit resolve api.foo.com A --at C1` returns 5.6.7.8.
`zonegit diff C0 C1` returns:
```
- api.foo.com.  A  300  1.2.3.4
+ api.foo.com.  A  300  5.6.7.8
```

---

## 12. What we're NOT doing in v0 (deferred to keep v0 small)

- **Packfiles** — Git compresses many objects into packs. We use loose
  objects only in v0; pack format can be added later as a storage detail
  invisible to higher layers.
- **Delta compression** — same as above.
- **Garbage collection** — never delete in v0. Compliance use cases
  (UC2, UC6) prefer this anyway.
- **Submodules / cross-repo refs** — not needed.
- **Worktrees** — only one branch is "checked out" for editing at a time;
  HEAD points to it. Multiple-worktree support deferred.

---

## 13. Invariants the implementation MUST uphold

These are the test assertions that protect future development.

1. `Hash(canonical_form(x)) == Hash(canonical_form(x))` always — pure function.
2. `Hash(canonical_form(rrset_a)) == Hash(canonical_form(rrset_b))` iff
   `rrset_a` and `rrset_b` are semantically equal (same name/type/class/ttl/
   sorted rdata).
3. Loading then re-encoding any object reproduces identical bytes.
4. Walking a tree to a non-existent path returns a `NotFound` error, not nil.
5. Ref updates are atomic — either the new hash is visible AND the reflog
   entry is appended, or neither happens.
6. Two clients committing on the same branch concurrently must result in
   exactly one CAS winner; the loser sees an explicit conflict error.
7. No object is ever deleted by normal operations (only by an explicit,
   logged GC pass — not in v0).

We will encode each of these as Go tests in `pkg/object` and `pkg/refs`
before any feature work proceeds.
