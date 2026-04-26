# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0: the on-disk format and the public Go API are not stable.
Breaking changes between minor versions will be called out explicitly.

## [Unreleased]

### Added
- _nothing yet_

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

[Unreleased]: https://github.com/ckumar392/zonegit/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ckumar392/zonegit/releases/tag/v0.1.0
