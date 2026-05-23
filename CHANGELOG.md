# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0: the on-disk format and the public Go API are not stable.
Breaking changes between minor versions will be called out explicitly.

## [Unreleased]

### Added
- _nothing yet_

## [0.6.0] - 2026-05-23

Real DNSSEC + operational completeness. v0.6 turns the v0.5 DNSSEC
scaffold into actual cryptographic signing (Ed25519, RFC 8080), adds
SIGHUP-driven config reload so the daemon never needs to restart for a
policy change, and fixes the IXFR walk to handle merge ancestors.

### Added
- **pkg/dnssec** — Ed25519 (algorithm 15, RFC 8080) keypair management
  and RRSIG generation. `Generate()` mints a fresh KSK + ZSK,
  `WriteToDir` / `LoadFromDir` persist them as base64 files, and
  `SignRRset` wraps miekg/dns's `RRSIG.Sign` to produce signatures that
  validate end-to-end. KSK signs DNSKEY; ZSK signs everything else
  ([pkg/dnssec/dnssec.go](pkg/dnssec/dnssec.go)).
- **`zonegit zone-keygen`** — generate and persist a zone's KSK + ZSK
  under `<repo>/keys/`. One command per zone.
- **`zonegit sign-zone` (real signing)** — when keys are present, emits
  real RRSIGs over every RRset. NSEC chain regenerated, DNSKEYs at the
  apex point at the loaded public keys. `--dry-run` remains for tests
  and demos that don't want to roll keys.
- **RRSIG-batching** — multiple RRsets at the same owner each get an
  RRSIG; all of them are stored together as a single RRSIG RRset (one
  blob per owner) so the existing `(owner, rrtype)` storage model
  carries DNSSEC without schema changes.
- **DO-bit-aware resolver** — when the requester sets the DNSSEC-OK
  bit in the OPT pseudo-record, the resolver looks up the RRSIG RRset
  at the answer's owner and appends any RRSIG whose `TypeCovered`
  matches the query type. `dig +dnssec` now returns answer + RRSIG
  inline ([pkg/resolve/resolve.go](pkg/resolve/resolve.go)).
- **SIGHUP config reload** — `zonegitd` listens for SIGHUP, re-parses
  `--config`, and atomically swaps it. The reconciler tracks the rule
  each zone was registered with and re-registers only those whose
  config actually changed. No restart, no dropped queries
  ([cmd/zonegitd/main.go](cmd/zonegitd/main.go)).
- **IXFR full-DAG walk** — `findCommitBySOASerial` now BFS-walks all
  parents (not just first-parent), so an IXFR with a serial that
  landed via a merge ancestor is still resolved correctly. Bounded at
  10k commits to defend against pathological graphs
  ([pkg/resolve/ixfr.go](pkg/resolve/ixfr.go)).

### Changed
- Demo step 20 now generates a real keypair, runs `sign-zone`, and
  shows `dig +dnssec` returning an A + RRSIG (96-char Ed25519
  signature, KSK key tag visible in the RRSIG header).
- `cmd/zonegit/sign_zone.go` is now backed by `pkg/dnssec` for the
  signing path; the placeholder mode (`--dry-run`) remains for tests.

### Known limitations (deferred to v0.7)
- **CoreDNS plugin** — the `pkg/resolve.Resolver` is plugin-shaped and
  ready to wrap; the remaining work is the plugin scaffold, setup
  function, and Corefile parser registration (~150 LoC + a Makefile
  target that builds a `coredns-with-zonegit` binary). Slated as the
  v0.7 headline so the "plug into your existing CoreDNS deployment"
  story has running code behind it.
- **Trust anchor publishing** — DS records for the apex KSK need to be
  uploaded to the parent zone for true validation. A `zonegit ds`
  helper that prints the DS record for the KSK is trivial to add and
  will ship with v0.7.
- **Automatic re-signing on commit** — today RRSIGs only refresh when
  `sign-zone` is run. Resolvers won't accept signatures past their
  expiration window. v0.7 adds an opt-in `--auto-sign` flag to
  `zonegit set` / `commit` that re-signs touched RRsets in the same
  commit.

## [0.5.0] - 2026-05-23

Operational polish + DNSSEC scaffold. v0.5 makes the daemon credible
to a DDI SME who lives in primary-secondary deployments: IXFR replaces
the "re-AXFR on every change" approximation, per-zone YAML config
unlocks "production on tenant A, canary 20% on tenant B" without
restarts, and the DNSSEC scaffold proves the object pipeline is
DNSSEC-shaped before v0.6 adds actual crypto.

### Added
- **IXFR (incremental zone transfer)** — `pkg/resolve/ixfr.go` walks
  first-parent commits to find the historical commit whose apex SOA
  matches the client's serial, then emits a single-delta IXFR response
  (RFC 1995 §4: latest SOA → old SOA → removals → latest SOA →
  additions → latest SOA). Falls back to AXFR when the historical
  commit isn't reachable (e.g. after `reset --hard`) or when serials
  match. Driven entirely by `pkg/history.Diff` — the same routine that
  powers `zonegit diff`.
- **Per-zone YAML config** — `zonegitd --config &lt;file&gt;` lets each
  zone get its own branch / canary / time-travel pin. Top-level
  defaults apply to zones not in the map; per-zone overrides win on a
  field-by-field basis. The daemon's reconciler reads this every tick,
  so adding a zone to the YAML and SIGHUP-equivalent flows are
  straightforward extensions ([cmd/zonegitd/config.go](cmd/zonegitd/config.go)).
- **DNSSEC scaffold** — `zonegit sign-zone --dry-run` enumerates every
  RRset, computes an NSEC chain over the canonical sort of owner
  names, stages KSK + ZSK at the apex, and stages an RRSIG per RRset
  with placeholder signature bytes. All five DNSSEC RR types
  (DNSKEY, RRSIG, NSEC, plus the bits encoded in NSEC's type bitmap)
  flow through the existing content-addressed object pipeline as
  ordinary RRsets — proving the architecture supports DNSSEC before
  v0.6 adds real crypto ([cmd/zonegit/sign_zone.go](cmd/zonegit/sign_zone.go)).
- **`KindSymref` and `Repo.SwitchZone` continue to underpin the demo**
  — no new layout changes in v0.5; the v0.4 work paid off here.

### Changed
- The 19-step demo grew to 21 steps. New step 19 demonstrates IXFR
  with an older serial, showing the live commit-DAG-driven delta;
  step 20 runs `sign-zone --dry-run` and digs the resulting DNSKEY
  and NSEC records.
- `cmd/zonegitd/main.go` now resolves per-zone settings through
  `daemonConfig.ruleFor(zone)` during reconciliation. The CLI
  `--branch` / `--canary` / `--at` / `--canary-salt` flags are
  preserved as the top-level config defaults.
- Adds `gopkg.in/yaml.v3` as a direct dependency (it was already
  present transitively); no other new deps.

### Known limitations (intentionally deferred to v0.6)
- DNSSEC signatures are placeholder bytes. Resolvers will not validate
  them. Real Ed25519 / RSA / ECDSA signing requires KMS or
  file-backed-key wiring on the write path — that's the v0.6
  milestone.
- The daemon doesn't reload the YAML config on SIGHUP yet. Restart
  is required to pick up config edits; zone additions / removals are
  picked up automatically by the existing reconciler.
- IXFR finds the historical commit by walking first-parent only. A
  zone with merge commits and a request against a serial that lived
  on a merged-in branch (not first-parent) will fall back to AXFR.
  Production deployments rarely encounter this pattern.

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
