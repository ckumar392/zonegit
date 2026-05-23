# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0: the on-disk format and the public Go API are not stable.
Breaking changes between minor versions will be called out explicitly.

## [Unreleased]

### Added
- _nothing yet_

## [0.4.0] - 2026-05-23

Multi-zone milestone. One repo can now hold many zones; one `zonegitd`
process serves all of them on a single port. This unlocks the
multi-tenant / MSP narrative (one daemon, many customer zones, per-zone
RBAC via ref isolation) without changing the protocol on the wire.

### Added
- **Multi-zone repo layout** — every branch and tag is scoped to its
  zone (`refs/heads/<zone>/<branch>`, `refs/tags/<zone>/<tag>`); zone
  membership is enumerable via `refs/zonegit/zones/<zone>` markers.
  Object storage is shared across zones; identical RRsets dedupe
  byte-for-byte regardless of which zone they appear in
  ([pkg/refs/refs.go](pkg/refs/refs.go)).
- **`zonegit zone add | list | switch`** — manage zones in a repo.
  Existing `init` registers the first zone; `zone add` joins additional
  zones without moving HEAD ([cmd/zonegit/zone.go](cmd/zonegit/zone.go)).
- **Multi-zone daemon** — `zonegitd` enumerates registered zones at
  startup and registers one `Resolver` per zone with miekg/dns. Time
  travel, canary, and AXFR all apply per zone. `--zone` becomes optional
  and selects a single zone if given ([cmd/zonegitd/main.go](cmd/zonegitd/main.go)).
- **Runtime zone discovery** — a 1s reconciler in the daemon notices
  zones added or removed at runtime (`zonegit zone add bar.com.` is
  picked up without a daemon restart). The snapshotter's
  `SetWatchedRefs` lets the reconciler extend the watch set on the fly
  ([cmd/zonegitd/main.go](cmd/zonegitd/main.go),
   [pkg/resolve/snapshot.go](pkg/resolve/snapshot.go)).
- **Object-backed HEAD symref** (`KindSymref`) — HEAD now points at a
  content-addressed object containing the target ref string, removing
  the 31-byte limit that the v0.3 length-prefix scheme imposed on
  `refs/heads/<zone>/<branch>` paths. Long zone names work
  ([pkg/object/object.go](pkg/object/object.go),
   [pkg/refs/refs.go](pkg/refs/refs.go)).
- **Automatic v0.3 → v0.4 migration on Open** — legacy single-zone
  repos are detected and converted in place: branches and tags are
  rewritten to the new zone-scoped paths, HEAD is re-encoded as a
  symref, the zone marker is created, and the legacy
  `refs/zonegit/zone` ref is removed. Idempotent and crash-safe to
  resume ([pkg/refs/refs.go](pkg/refs/refs.go) `MigrateLegacyV03`,
   [pkg/repo/repo.go](pkg/repo/repo.go) `Open`).

### Changed
- `Repo.Head` now returns `(zone, branch, commit, err)`. Callers must
  update to consume the zone segment.
- `refs.DB.CreateBranch / UpdateBranch / DeleteBranch / GetBranch /
  ListBranches / CreateTag / GetTag / DeleteTag / ListTags` and `SetHEAD`
  all take an additional zone parameter; bare-name `Resolve` now
  resolves against the active zone from HEAD.
- The 18-step demo grew to 19 steps with a new "MULTI-ZONE" step that
  registers a second zone (`bar.com.`) at runtime and proves a single
  daemon serves both `foo.com.` and `bar.com.` from one port
  without restart ([scripts/demo.sh](scripts/demo.sh)).
- `cmd/zonegit/main.go` `--zone` flag, when given, switches HEAD to that
  zone's current branch for the duration of the command (using the new
  `Repo.SwitchZone`).
- Snapshotter no longer owns its watched-ref list permanently; the
  daemon updates it as zones come and go via `SetWatchedRefs`.

### Removed
- `Repo.Zone()` and `Repo.SetZone()` — superseded by `ActiveZone()`,
  `AddZone()`, `Zones()`, `SwitchZone()`.
- The `MaxHeadTargetLen` constant — there is no length limit anymore.

### Known limitations (intentionally deferred to v0.5)
- The daemon's reconciler opens one fresh read-only Badger handle per
  second to discover zone changes. Cheap, but a sentinel-file watcher
  or Badger Subscribe would be cleaner at scale.
- Per-zone `--branch` / `--canary` configuration is uniform; v0.5 will
  add a per-zone config file so different zones can have different
  rollout policies.
- AXFR is still full-only (no IXFR), inherited from v0.3.

## [0.3.0] - 2026-05-23

Demo-readiness milestone. Every claim the README makes now corresponds to
running code (and a step in `make demo`). The five additions below are what
moved this from "interesting weekend project" to "credible authority
direction":

### Added
- **SOA serial auto-increment on commit** — `pkg/repo.Commit` now stages a
  bumped apex SOA whenever any non-SOA RRset is mutated and the user has
  not explicitly staged an SOA. Without this, the README's "no SOA dance"
  pitch left secondaries with no way to know anything changed
  ([pkg/repo/repo.go](pkg/repo/repo.go), [pkg/zone/soa.go](pkg/zone/soa.go)).
- **`pkg/resolve`** — the DNS query path, extracted from `cmd/zonegitd`
  into its own package per the architecture diagram. Provides
  `Resolver.Handle`, `Resolver.HandleWithRemote`, AXFR streaming, and the
  `Snapshotter`/`Router`/`MetricsHook` seams ([pkg/resolve](pkg/resolve)).
- **Cached snapshotter** (`pkg/resolve.PollingSnapshotter`) — replaces the
  v0/v1 per-query Badger reopen with a single cached handle invalidated
  only when a watched branch's tip hash actually changes. Per-query cost
  drops from one Badger Open to one atomic pointer load.
- **Time-travel daemon** — `zonegitd --at <refish>` pins serving to a
  historical commit. Any `dig` against that daemon answers as the zone
  existed at that commit. The README's "what did this resolve to N
  commits ago?" claim is now answerable by real DNS, not just a CLI
  dump ([cmd/zonegitd/main.go](cmd/zonegitd/main.go)).
- **Canary routing** — `zonegitd --canary canary:20` plus a tiny
  subnet-bucket selector in `pkg/route` send X% of traffic (by stable
  hash of the client `/24`) to a canary branch. Rollback is one ref
  move ([pkg/route](pkg/route)). This is the v2 SELECTORS.md headline
  use case (UC5) implemented in its smallest defensible form;
  the full grammar remains a v3 milestone.
- **AXFR** — full-zone transfer over TCP, RFC 5936 compliant
  (leading + trailing SOA, RRsets in canonical tree-walk order). Makes
  the "drop-in BIND replacement" claim real for primary-secondary
  deployments ([pkg/resolve/axfr.go](pkg/resolve/axfr.go)).
- **Prometheus metrics endpoint** — `--metrics-listen :9353` exposes
  `zonegit_dns_queries_total{qtype,rcode}` and an active-branch info
  gauge. Hand-rolled (no `client_golang` dep)
  ([pkg/resolve/metrics.go](pkg/resolve/metrics.go)).
- **Ed25519 commit signing** — `zonegit keygen`, `zonegit sign-commit`,
  `zonegit verify [--chain]`. The signature header was already reserved
  on the commit object; this adds the actual sign/verify primitive in
  `pkg/sign` ([pkg/sign](pkg/sign)).
- **PR-style change verbs** — `zonegit propose <name> --from main`,
  `zonegit review <name> --into main`, `zonegit approve <name> --into main`.
  Thin convenience over branch/diff/merge, but the vocabulary matches
  how change-management SMEs actually talk
  ([cmd/zonegit/propose.go](cmd/zonegit/propose.go)).
- **Persisted zone metadata** — `zonegit init <zone>` writes the zone
  name to `refs/zonegit/zone`. Subsequent CLI and daemon invocations
  auto-load it, so `--zone` is now optional after the first
  `init` ([pkg/refs/refs.go](pkg/refs/refs.go)).
- **`object.WalkAllLeaves`** — depth-first leaf enumeration over a tree.
  Powers AXFR; will also be the seam for zone-export and signed-zone
  workflows ([pkg/object/treeops.go](pkg/object/treeops.go)).

### Changed
- The 15-step demo grew to 18 steps. Coverage now includes SOA
  before/after observation, time-travel `dig`, canary bucket split,
  AXFR, propose/approve, and a curl against `/metrics`
  ([scripts/demo.sh](scripts/demo.sh)).
- `cmd/zonegitd/main.go` shrank from ~260 LoC of inline DNS handling
  down to ~150 LoC of flag parsing + wiring; the heavy lifting moved
  into `pkg/resolve`.

### Known limitations
- AXFR is full-only — no IXFR (delta) yet. Secondaries with a primary
  pointing at `zonegitd` re-AXFR on every NOTIFY. v4 will add IXFR
  over the existing commit-diff plumbing in `pkg/history`.
- The selector engine in `pkg/route` is one rule shape
  (`hash(client.subnet, salt) % 100 < pct`). The full SELECTORS.md
  grammar (geo, ASN, time windows, list literals) remains a v3 item.
- Snapshotter invalidates via a 200ms polling reopener — fine for the
  demo and a small repo, but production deployments should switch to
  fsnotify (`Badger`'s on-disk manifest) or writer-pushed signals.
- Commit signing is file-keyed; no KMS yet, no server-side
  "refuse unsigned" policy yet. Both are slated for the next
  milestone.

## [0.2.0] - 2026-04-27

v1 milestone: branches mean something at serve time, plus three-way
merge, revert, and reset. The reference daemon already picks up branch
moves on the next packet (per-query read-only Badger reopen), so all
four new write-side verbs are visible live via `dig` in the demo.

### Added
- `pkg/merge`: structural 3-way merge over zone trees, with explicit
  conflict types (`both-modified`, `deleted-modified`, `add-add`).
  Identical-subtree fast-paths so disjoint branches merge in
  O(changed paths) ([pkg/merge](pkg/merge)).
- `Repo.Merge`: integrates a branch into the current branch with
  fast-forward, already-up-to-date, and 3-way merge-commit cases.
  Conflicts abort the merge without advancing the ref
  ([pkg/repo/merge.go](pkg/repo/merge.go)).
- `Repo.MergeBase`: lowest-common-ancestor over the commit DAG.
- `Repo.Revert`: produces an inverse commit on the current branch by
  applying the reverse of `target` vs its first parent.
- `Repo.ResetHard`: atomically moves the current branch tip to a
  ref-ish and clears staging, with a reflog entry.
- CLI: `zonegit merge`, `zonegit revert`, `zonegit reset --hard`
  ([cmd/zonegit/merge.go](cmd/zonegit/merge.go)).
- Demo script extended to 15 steps covering branch → fast-forward merge
  → revert → reset, all observed live through the same running daemon
  ([scripts/demo.sh](scripts/demo.sh)).

### Known limitations
- Three-way merge currently treats any divergent same-leaf modification
  as a conflict. RR-set-aware automatic resolution (e.g. union of `A`
  records when both sides only added new addresses) is not implemented.
- The reference daemon serves a single configured branch. Selector-based
  branch routing for canary serving is the v2 milestone.

## [0.1.0] - 2026-04-25

First public preview. End-to-end demo of `init → import → set → log → diff → blame → serve` works on a single zone, on a single machine.

### Added
- Content-addressed object model: `Blob`, `Tree`, `Commit`, `Tag` with
  canonical encoding and content hashing ([pkg/object](pkg/object)).
- Pluggable storage with a conformance suite that every backend must
  pass; in-memory and Badger backends ship in-tree
  ([pkg/store](pkg/store)).
- Refs: branches, `HEAD`, tags, reflog, atomic compare-and-swap, and a
  small ref-ish resolver ([pkg/refs](pkg/refs)).
- Zone bridge between `miekg/dns` records and the object model, with
  canonical RRset encoding and zonefile import/export
  ([pkg/zone](pkg/zone)).
- History operations: `log`, `diff`, `blame`, and walk-at-time
  ([pkg/history](pkg/history)).
- Public Go API ([pkg/repo](pkg/repo)) and a Cobra-based CLI
  ([cmd/zonegit](cmd/zonegit)).
- Reference authoritative DNS server ([cmd/zonegitd](cmd/zonegitd)):
  UDP + TCP, sets the `AA` bit, distinguishes `NXDOMAIN` from
  `NODATA`, returns `SOA` in the authority section.
- End-to-end demo script ([scripts/demo.sh](scripts/demo.sh)).
- CI pipeline: `go vet`, `go test -race`, golangci-lint v2.

### Known limitations
- Single-zone, single-process. No replication, no AXFR/IXFR yet.
- No DNSSEC.
- No three-way merge for divergent branches (planned for v1).
- On-disk Badger layout is not stable; expect to re-init repos when
  upgrading.

[Unreleased]: https://github.com/ckumar392/zonegit/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/ckumar392/zonegit/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ckumar392/zonegit/releases/tag/v0.1.0
