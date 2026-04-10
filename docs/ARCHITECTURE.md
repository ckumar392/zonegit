# dnsdb Architecture

> Sister doc to [OBJECT_MODEL.md](OBJECT_MODEL.md). This file says **how
> the code is laid out**, **which package depends on which**, and **what
> each layer is allowed and forbidden to do**.
>
> The goal: keep the design replaceable. v0 uses Badger; v4 swaps in
> Postgres without touching anything above the storage seam.

---

## 1. Layering (top = depends on bottom; never the reverse)

```
┌────────────────────────────────────────────────────────────┐
│  cmd/dnsdb         (CLI: cobra)                            │
│  cmd/dnsdbd        (server daemon: gRPC + DNS)             │
│  plugin/coredns    (CoreDNS plugin, v2+)                   │
└──────────────────────────────┬─────────────────────────────┘
                               │
┌──────────────────────────────▼─────────────────────────────┐
│  pkg/repo          (the public API: open repo, do an op)   │
└──────────────────────────────┬─────────────────────────────┘
                               │
       ┌───────────┬───────────┼───────────┬────────────┐
       │           │           │           │            │
┌──────▼─────┐ ┌──▼────────┐ ┌▼────────┐ ┌▼───────┐  ┌─▼─────────┐
│ pkg/history│ │ pkg/merge │ │pkg/zone │ │pkg/refs│  │pkg/resolve│
│ (log/diff/ │ │ (3-way    │ │(RR ↔    │ │(branches│ │(time-     │
│  blame)    │ │  RRset    │ │ blob)   │ │ HEAD,   │ │ travel    │
│            │ │  merge)   │ │         │ │ reflog) │ │ DNS)      │
└──────┬─────┘ └──┬────────┘ └┬────────┘ └─┬──────┘  └─┬─────────┘
       │          │           │            │            │
       └──────────┴───────┬───┴────────────┴────────────┘
                          │
                ┌─────────▼──────────┐
                │ pkg/object         │
                │ (Blob/Tree/Commit/ │
                │  Tag, hashing,     │
                │  canonical form)   │
                └─────────┬──────────┘
                          │
                ┌─────────▼──────────┐
                │ pkg/store          │
                │ Storage interface  │
                │ + Badger adapter   │
                │ + Postgres (v4)    │
                │ + Memory (testing) │
                └────────────────────┘
```

### Dependency rules (enforced by `go vet`/CI)

- `pkg/store` depends on **nothing** in this repo.
- `pkg/object` depends only on `pkg/store` and stdlib.
- `pkg/zone` depends on `pkg/object` and `miekg/dns`.
- `pkg/refs` depends only on `pkg/store` and stdlib.
- `pkg/history`, `pkg/merge`, `pkg/resolve` depend on
  `pkg/object` + `pkg/refs` + `pkg/zone`.
- `pkg/repo` depends on all of the above and is the *only* package the
  cmd/* entry points are allowed to touch.
- No `cmd/*` package imports anything besides `pkg/repo` and stdlib/cobra.

This gives us a **strict tree** — no cycles, no surprises, easy to test
each layer in isolation.

---

## 2. The single most important seam: `pkg/store`

```go
// pkg/store/store.go
type Hash [32]byte

type Object struct {
    Kind    string  // "blob" | "tree" | "commit" | "tag"
    Payload []byte  // canonical bytes (without the kind/length header)
}

// Storage is the only thing pkg/object talks to for persistence.
// Every backend (Badger, Postgres, S3, in-memory) implements this.
type Storage interface {
    // Object CAS
    PutObject(ctx context.Context, h Hash, o Object) error
    GetObject(ctx context.Context, h Hash) (Object, error)
    HasObject(ctx context.Context, h Hash) (bool, error)
    IterObjects(ctx context.Context, fn func(Hash, Object) error) error

    // Refs (mutable, atomic)
    GetRef(ctx context.Context, name string) (Hash, bool, error)
    CASRef(ctx context.Context, name string, expected, new Hash) error
    DeleteRef(ctx context.Context, name string, expected Hash) error
    ListRefs(ctx context.Context, prefix string) ([]RefEntry, error)

    // Reflog (append-only)
    AppendReflog(ctx context.Context, name string, e ReflogEntry) error
    ReadReflog(ctx context.Context, name string) ([]ReflogEntry, error)

    Close() error
}
```

If a future engineer wants to add Postgres, they implement this
interface. Nothing else changes.

### Why this seam is sacred

Every "we should have abstracted this earlier" pain in storage systems
comes from leaking storage details upward. We pay the cost of one
indirection now to never pay the cost of a v4 rewrite.

**Forbidden:** any package above `pkg/store` knowing the difference
between Badger and Postgres. No `if pgBackend { ... }`. Ever.

---

## 3. Package responsibilities

### `pkg/store`
- Exactly the interface above.
- Implementations: `badger/`, `memory/` (for tests). Postgres in v4.
- No knowledge of what objects mean. It moves bytes.

### `pkg/object`
- Defines `Blob`, `Tree`, `Commit`, `Tag` types.
- Implements canonical encoding/decoding (per OBJECT_MODEL.md §3–§7).
- Computes hashes.
- Walks trees (path → blob hash).
- Pure: given the same input, always produces the same bytes/hash.
- Talks to `pkg/store` only via the `Storage` interface.

### `pkg/zone`
- Bridges between `miekg/dns` RR types and `pkg/object` blobs.
- `BlobFromRRset(rrs []dns.RR) Blob` and `RRsetFromBlob(b Blob) []dns.RR`.
- Owns the RR canonicalization rules (lowercase names, sort rdata, etc.).
- Zone-file (RFC 1035 text) import/export lives here.

### `pkg/refs`
- Branches, HEAD, tag refs.
- Atomic CAS via `pkg/store`.
- Reflog append/read.
- Resolves "ref-ish" strings: `main`, `HEAD`, `HEAD~3`, full hash, short hash, tag name.
- No knowledge of commit semantics — just name → hash.

### `pkg/history`
- `Log(ref) → []Commit`
- `Diff(commitA, commitB) → []RRsetChange`
- `Blame(name, type) → BlameLine` (walks reverse history of one RRset)
- `WalkAt(ref, time) → Commit` (point-in-time)
- All read-only; mutates nothing.

### `pkg/merge` (v1+)
- 3-way RRset merge: `merge(base, ours, theirs) → (merged, []Conflict)`
- Conflict types: same-RRset-different-rdata, type-conflict, etc.
- Pure function over RRsets; storage-free.

### `pkg/resolve`
- The **DNS query path**.
- Given a query + branch (or selector match → branch), walks the tree,
  loads the blob, returns the RRset.
- This is the hot path. Must not allocate per query in v1+; v0 can.
- Caches the active commit's tree-walk in memory (read-mostly).

### `pkg/repo`
- The single Go API the CLI and server use.
- Combines the above into operations: `Init()`, `Add()`, `Commit()`,
  `Log()`, `Diff()`, `Blame()`, `Branch()`, `Checkout()`, `Resolve()`.
- The "Repo" struct holds the open `Storage`, the loaded HEAD, an in-memory
  staging area (for `add` → `commit` workflow).
- This is the public Go API anyone embedding dnsdb consumes.

### `cmd/dnsdb`
- cobra-based CLI. Each subcommand is ~30 lines: parse flags, call
  `pkg/repo`, format output.
- No business logic in cmd/.

### `cmd/dnsdbd`
- Long-running server: gRPC for control plane, UDP/TCP DNS for resolution.
- Reads from `pkg/resolve`. Watches refs for branch promotions and
  invalidates caches. v0 has no gRPC yet — just DNS.

### `plugin/coredns` (v2+)
- Thin shim that wraps `pkg/resolve` as a CoreDNS plugin.
- Lets dnsdb be embedded into existing CoreDNS deployments.

---

## 4. Lifecycle of a request: `dnsdb commit -m "promote api"`

1. **`cmd/dnsdb commit`** parses flags, calls `repo.Commit(ctx, msg)`.
2. **`pkg/repo`** reads its in-memory staging area, computes the new tree
   on top of HEAD's current tree:
   - Calls `pkg/zone.BlobFromRRset(...)` for each modified RRset.
   - Calls `pkg/object.UpdateTree(...)` to produce a new tree hash with
     only the changed path's nodes rewritten.
   - Calls `pkg/object.NewCommit(parent=HEAD, tree, author, msg)`.
3. **`pkg/object`** writes new blob/tree/commit objects via `Storage.PutObject`.
4. **`pkg/refs`** does `Storage.CASRef("refs/heads/" + currentBranch,
   expected=oldHEAD, new=newCommit)`.
5. **`pkg/refs`** appends a reflog entry on success.
6. **`pkg/repo`** returns the new commit hash to the CLI, which prints it.

If step 4 fails (someone else committed), repo can either fast-fail or
auto-rebase the staged changes onto the new head. v0 fails loudly.

---

## 5. Lifecycle of a request: `dig @dnsdb api.foo.com`

1. **`cmd/dnsdbd`** receives a DNS message via `miekg/dns.Server`.
2. Hands it to **`pkg/resolve.Resolve(query)`**.
3. `pkg/resolve` evaluates the (selector → branch) rules, picks branch
   (default: `main`).
4. Looks up the branch's commit hash via `pkg/refs.GetRef`.
5. Looks up the commit's tree hash. Walks the tree by query name labels.
6. Loads the matching leaf blob. Decodes RRset via `pkg/zone`.
7. Builds the response, returns to client.

Hot-path budget: this is 1 ref read + ~N hash lookups (N = label depth,
typically 2–4) + 1 blob load. With an in-memory cache of the active
commit's tree, this is O(N) memory reads. Fast.

---

## 6. Concurrency model

- **Reads** (resolve, log, diff): wide-open concurrent. Objects are immutable;
  the only contention is loading them.
- **Writes** (commit, branch, merge): serialized at the *ref* level via
  `Storage.CASRef`. Object writes can happen in parallel — they're CAS by
  hash and content-addressed, so two concurrent writers writing the same
  object both succeed (last write wins, with identical bytes).
- The Repo struct is safe to share across goroutines for reads. Writes
  go through a per-branch mutex inside `pkg/repo` to avoid wasted CAS
  retries against ourselves.

---

## 7. Error model

- Every error returned to a CLI user is wrapped with `fmt.Errorf("%w", ...)`
  carrying enough context to identify the operation.
- Sentinel errors: `ErrNotFound`, `ErrConflict` (CAS lost), `ErrInvalidObject`,
  `ErrCorruptStore`.
- A corrupt object (hash mismatch on read) is **always** fatal — return
  `ErrCorruptStore` with the offending hash, never silently degrade.

---

## 8. Observability

- Structured logging via `log/slog` (Go 1.21+ stdlib). No logrus.
- Metrics via `prometheus/client_golang` exposed on a sidecar port.
- Key metrics from day 1:
  - `dnsdb_object_reads_total{kind=...}`
  - `dnsdb_object_writes_total{kind=...}`
  - `dnsdb_ref_cas_attempts_total{result=ok|conflict}`
  - `dnsdb_resolve_latency_seconds` (histogram)
  - `dnsdb_repo_open_seconds`
- Tracing via OpenTelemetry, optional, behind a flag.

---

## 9. Testing strategy

- **Unit tests** per package, no network/disk except `pkg/store/badger`.
- A shared **conformance suite** in `pkg/store/storetest` that any
  `Storage` implementation must pass. Run against `memory` AND `badger` in
  CI — Postgres adapter will plug into the same suite later.
- **Property tests** (`testing/quick` or `gopter`) for invariants:
  - "Encode then decode is identity"
  - "Hash is deterministic"
  - "RR list permutation does not change blob hash"
- **End-to-end test** (`tests/e2e/`) that spawns `dnsdbd`, runs the CLI,
  fires `miekg/dns` queries, asserts answers.

---

## 10. What lives outside this repo (intentionally)

- The web UI for change review (PR-style). Future, separate repo.
- A web-hooks/event bus emitter for downstream systems. Future.
- Replication wire protocol implementations beyond a reference one. Future.
- Anything that's "an  product surface" lives in your control plane, not here.
  This repo is the **engine**, not the product.

---

## 11. Deployment shapes — where dnsdb fits

> dnsdb is **not** a DNS server. It is a **versioned state store** that
> sits inside the control plane, replacing the mutable database that today
> holds authoritative zone data.

### Before vs After

```
BEFORE                                  AFTER
──────                                  ─────
[ UI / API ]                            [ UI / API ]
      ↓                                       ↓
[ Mutable DB ]                          [ dnsdb (Merkle DAG, commits) ]
      ↓                                       ↓
[ Zone files ]                          [ Zone snapshot (HEAD) ]
      ↓                                       ↓
[ DNS servers (BIND/CoreDNS) ]          [ DNS servers (BIND/CoreDNS) ]
```

### What gets replaced

| Before (mutable)       | After (dnsdb)                        |
|------------------------|--------------------------------------|
| DB tables / rows       | Append-only commits (Blob/Tree/Commit) |
| Periodic snapshots     | Commit history (full DAG)            |
| Audit log tables       | Cryptographic history (signed commits v3+) |
| Import / sync scripts  | Commit-based diffs + merge           |

### What stays exactly the same

- **DNS servers** — BIND, CoreDNS, NSD, whatever answers port 53.
- **DNS wire protocol** — standard RFC 1035 / RFC 8484 queries.
- **External APIs** — your existing control-plane REST / gRPC surface is unchanged.

### Two shipped shapes

1. **Standalone `dnsdbd`** (v0+) — a binary that opens a dnsdb repo and
   serves DNS on port 53 directly. Useful for demos, dev, and small
   deployments. Internally uses `pkg/resolve`.
2. **CoreDNS plugin** (v6) — a thin shim (~200 LoC) wrapping
   `pkg/resolve` as a CoreDNS plugin. Embeds into existing CoreDNS
   deployments with a single `Corefile` directive.

Neither shape changes the DNS protocol — they are wire-identical to any
other authoritative server.

### BIND plugin — explicitly not pursued

BIND is C; writing a safe, maintained C plugin that bridges to a Go
object store is fragile and unlikely to be accepted upstream. Instead,
`dnsdbd` is a **drop-in replacement** for BIND — same port, same
protocol, same zone transfer (AXFR/IXFR, v5+).


---

## 12. Anti-goals (things we will NOT do, no matter how tempting)

- **No "convenience" methods that bypass the Storage interface.**
  If `pkg/repo` needs to do something with bytes, it goes through
  `pkg/store`. No exceptions.
- **No global state.** No package-level singletons, no `init()` magic.
  Every dependency is injected. (Yes, that means no `log.Println` either.)
- **No reflection-based serialization for object payloads.** Canonical
  form is hand-written and unit-tested. Reflection-based encoding
  invariably breaks canonicality on Go upgrades.
- **No reaching across layer boundaries.** `cmd/dnsdb` calling
  `pkg/object` directly is a CI failure.
- **No "TODO: handle this later" without a linked issue.** TODOs that
  reference an issue number are fine; loose TODOs are not.
