# zonegit

A content-addressed, version-controlled object model for authoritative DNS
zones, with Git-style semantics (commits, branches, tags, refs, blame, diff).

## Status

Pre-release. The on-disk format and public Go API are not yet stable.

## Overview

`zonegit` models a DNS zone as a Merkle DAG of immutable objects:

- **Blob** — a canonicalized RRset (one `(name, type)` coordinate).
- **Tree** — a directory of names mapping to subtrees and RRset blobs.
- **Commit** — a snapshot of the zone tree, with parent links and metadata.
- **Tag / Ref** — named pointers into the commit graph.

The current zone served by the authoritative path is just a pointer to the
latest commit on a branch. Historical state is preserved by construction,
which makes time-travel queries, per-RRset blame, and atomic branch-based
rollout straightforward operations rather than bolt-on features.

## Repository layout

```
cmd/
  zonegit/      CLI entry point
  zonegitd/     Authoritative DNS server entry point
pkg/
  store/        Storage interface + Badger and in-memory backends
  object/       Blob / Tree / Commit / Tag, canonical encoding, hashing
  zone/         Bridge between miekg/dns RRs and the object model
  refs/         Branches, HEAD, reflog, atomic compare-and-swap
  history/      log, diff, blame
  merge/        Three-way RRset merge
  resolve/      DNS query path
  repo/         Public Go API
docs/           Design documentation
scripts/        Development helpers
```

## Build

```sh
make build
```

This produces `bin/zonegit` and `bin/zonegitd`.

## Test

```sh
make test        # unit tests
make test-race   # tests with the race detector
make lint        # golangci-lint (or go vet as a fallback)
make demo        # end-to-end demo script
```

Run `make help` to list all targets.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — package layering, the
  storage seam, object lifecycles.
- [docs/OBJECT_MODEL.md](docs/OBJECT_MODEL.md) — canonical form, hashing,
  and invariants for Blob, Tree, Commit, Tag, and Ref.
- [docs/ROADMAP.md](docs/ROADMAP.md) — milestones and sequencing.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
