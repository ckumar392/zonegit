# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0: the on-disk format and the public Go API are not stable.
Breaking changes between minor versions will be called out explicitly.

## [Unreleased]

### Added
- _nothing yet_

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
