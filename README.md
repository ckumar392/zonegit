# zonegit

[![CI](https://github.com/ckumar392/zonegit/actions/workflows/ci.yml/badge.svg)](https://github.com/ckumar392/zonegit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ckumar392/zonegit.svg)](https://pkg.go.dev/github.com/ckumar392/zonegit)
[![Go Report Card](https://goreportcard.com/badge/github.com/ckumar392/zonegit)](https://goreportcard.com/report/github.com/ckumar392/zonegit)
[![Release](https://img.shields.io/github/v/release/ckumar392/zonegit?include_prereleases&sort=semver)](https://github.com/ckumar392/zonegit/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> Git semantics for authoritative DNS.

`zonegit` is a content-addressed, version-controlled object model for
authoritative DNS zones. Every change is an immutable commit; the live
zone is just a pointer to the latest commit on a branch. That single
inversion gives you `log`, `diff`, `blame`, time-travel reads, and
branch-based rollout — operations that today's authoritative DNS
servers (BIND, Knot, PowerDNS, Route 53) don't expose at all.

## Status

**v0.2 — public preview.** Single zone, single process, no replication.
Branches now mean something at serve time: `zonegit merge`, `revert`,
and `reset --hard` are wired through the daemon's per-query HEAD
re-resolve, so a `dig` issued moments after a `merge` sees the new
answer without restarting anything. The on-disk format and public Go
API are still not stable; expect breakage between minor versions until
v1.0.

## Demo

The repo ships with an end-to-end demo that builds both binaries,
imports a real zonefile, serves it on `127.0.0.1:15353`, mutates a
record, and shows the change reflected live — without a reload, an
SOA bump, or restarting the daemon.

```sh
make demo
```

Below is roughly what you should see.

### 1. Initialise a repo and import a zonefile

```sh
$ zonegit --repo ./.zonegit init foo.com.
initialised zonegit repo at ./.zonegit (zone: foo.com.)

$ cat foo.com.zone
$ORIGIN foo.com.
$TTL 300
@   IN SOA ns1.foo.com. admin.foo.com. 1 7200 3600 1209600 300
    IN NS  ns1.foo.com.
ns1 IN A   10.0.0.1
api IN A   1.2.3.4
www IN CNAME api.foo.com.

$ zonegit --repo ./.zonegit --zone foo.com. import foo.com.zone -m "initial import"
[main 4f1c2a9] initial import
 5 RRsets imported
```

### 2. Serve it, query it

```sh
$ zonegitd --repo ./.zonegit --zone foo.com. --listen 127.0.0.1:15353 &

$ dig @127.0.0.1 -p 15353 +short api.foo.com. A
1.2.3.4

$ dig @127.0.0.1 -p 15353 +short www.foo.com. A
api.foo.com.
1.2.3.4
```

### 3. Change a record. The daemon picks it up on the next packet.

```sh
$ zonegit --repo ./.zonegit --zone foo.com. \
    set api.foo.com. A 300 9.9.9.9 -m "failover to DR site"
[main 7c2af3b] failover to DR site

$ dig @127.0.0.1 -p 15353 +short api.foo.com. A
9.9.9.9
```

No reload, no SOA dance, no daemon restart. The server reopens its
read-only Badger handle per query and sees the new `HEAD`.

### 4. Inspect the history

```sh
$ zonegit --repo ./.zonegit log
commit 7c2af3b  Chandan Kumar  2026-04-25 22:13  failover to DR site
commit 4f1c2a9  Chandan Kumar  2026-04-25 22:08  initial import

$ zonegit --repo ./.zonegit diff HEAD~1 HEAD
~ api  A   1.2.3.4 -> 9.9.9.9

$ zonegit --repo ./.zonegit blame api.foo.com. A
api.foo.com. A  9.9.9.9   <- 7c2af3b  Chandan Kumar  "failover to DR site"
```

### 5. Time-travel

```sh
$ zonegit --repo ./.zonegit show api.foo.com. A HEAD~1
api.foo.com. 300 IN A 1.2.3.4
```

That last query — *"what did this name resolve to N commits ago?"* —
is the one that no DNS tool shipping today can answer.

### 6. Branch and merge (v0.2+)

Create a `canary` branch, edit it, then fast-forward merge into `main`.
The daemon picks up the new tip on the very next `dig` — no restart.

```sh
$ zonegit --repo ./.zonegit branch canary
$ zonegit --repo ./.zonegit checkout canary

$ zonegit --repo ./.zonegit --zone foo.com. \
    set api.foo.com. A 300 7.7.7.7 -m "canary: api -> 7.7.7.7"
[canary b4e10c8] canary: api -> 7.7.7.7

# daemon is still on --branch main, so dig still returns 9.9.9.9
$ dig @127.0.0.1 -p 15353 +short api.foo.com. A
9.9.9.9

$ zonegit --repo ./.zonegit checkout main
$ zonegit --repo ./.zonegit --zone foo.com. merge canary
Fast-forward to b4e10c8.

$ dig @127.0.0.1 -p 15353 +short api.foo.com. A
7.7.7.7
```

### 7. Revert

```sh
$ zonegit --repo ./.zonegit --zone foo.com. revert HEAD
Reverted as c353b7b

$ dig @127.0.0.1 -p 15353 +short api.foo.com. A
9.9.9.9
```

### 8. Reset

```sh
$ zonegit --repo ./.zonegit --zone foo.com. reset --hard HEAD~1
HEAD is now at b4e10c8

$ dig @127.0.0.1 -p 15353 +short api.foo.com. A
7.7.7.7
```

The full 15-step demo (including branch isolation, merge, revert, and
reset) runs end-to-end via `make demo`.

## Install

### Pre-built binaries

Download the latest release for your platform from the
[releases page](https://github.com/ckumar392/zonegit/releases).

### From source

Requires Go 1.24+.

```sh
go install github.com/ckumar392/zonegit/cmd/zonegit@latest
go install github.com/ckumar392/zonegit/cmd/zonegitd@latest
```

Or clone and build:

```sh
git clone https://github.com/ckumar392/zonegit.git
cd zonegit
make build      # produces ./bin/zonegit and ./bin/zonegitd
```

## How it works

`zonegit` models a zone as a Merkle DAG of immutable objects:

| Object        | What it holds                                                |
| ------------- | ------------------------------------------------------------ |
| **Blob**      | One canonicalised RRset (one `(name, type)` coordinate).     |
| **Tree**      | A directory of labels mapping to subtrees and RRset blobs.   |
| **Commit**    | A snapshot of the zone tree, with parent links and metadata. |
| **Tag / Ref** | Named pointers into the commit graph.                        |

Names with identical content hash to identical blobs, so equivalent
zones share storage. Subtree hashes mean `diff` skips unchanged
branches in O(changes), not O(zone size). Commits chain by parent
hash, so the history is verifiable end-to-end.

The full design is in [docs/OBJECT_MODEL.md](docs/OBJECT_MODEL.md).

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
  merge/        Three-way tree merge with conflict classification
  resolve/      DNS query path
  repo/         Public Go API
docs/           Design documentation
scripts/        Development helpers
```

## Build and test

```sh
make build       # build both binaries into ./bin
make test        # unit tests
make test-race   # tests with the race detector
make lint        # golangci-lint (or go vet as a fallback)
make demo        # end-to-end demo
make help        # list all targets
```

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — package layering, the
  storage seam, object lifecycles.
- [docs/OBJECT_MODEL.md](docs/OBJECT_MODEL.md) — canonical form,
  hashing, and invariants for Blob, Tree, Commit, Tag, and Ref.
- [docs/SELECTORS.md](docs/SELECTORS.md) — v2 selector grammar spec
  (canary serving, geo cutover, percentage rollout).
- [docs/ROADMAP.md](docs/ROADMAP.md) — milestones and sequencing.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the development setup, the change-proposal process, and the
[good first issue](https://github.com/ckumar392/zonegit/labels/good%20first%20issue)
list. Released versions are tracked in [CHANGELOG.md](CHANGELOG.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
