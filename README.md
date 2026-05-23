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

**v0.3 — public preview.** Single zone, single process, no replication.
v0.3 adds the four things you'd actually want before pointing a
production secondary at this:

- **Canary serving** (`zonegitd --canary canary:20`): a stable
  subnet-bucket router that sends X% of traffic to a canary branch and
  snaps it back with one ref move. See *Canary serving* below.
- **AXFR**: respond to full-zone transfer requests, so any existing
  BIND / Knot / PowerDNS secondary can slave off `zonegitd`.
- **Time-travel daemon** (`zonegitd --at HEAD~5`): pin the server to a
  historical commit and `dig` against the past, not just the current
  branch tip.
- **Auto-incrementing SOA serial**: changes that touch any non-SOA RRset
  bump the apex SOA serial automatically, so existing IXFR/NOTIFY
  pipelines pick up changes the way they always have.

Plus: a cached, polling snapshotter (the daemon no longer opens Badger
per packet), a Prometheus `/metrics` endpoint, Ed25519 commit signing
(`zonegit sign-commit` / `verify`), and a PR-style verb pair
(`zonegit propose` / `approve` / `review`). The on-disk format and
public Go API are still not stable; expect breakage between minor
versions until v1.0.

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

The full demo (including branch isolation, merge, revert, reset, canary
routing, time-travel, AXFR, and the PR-style propose/approve flow) runs
end-to-end via `make demo`.

### 9. Canary serving by client subnet

Send 20% of traffic to a canary branch — selected by a stable hash of
the client `/24` — and snap it back to 100% main with one ref move.

```sh
$ zonegitd --repo ./.zonegit --zone foo.com. \
    --listen 127.0.0.1:15353 \
    --branch main --canary canary:20 --canary-salt "api-rollout" &

# 20% of /24s land on canary; the other 80% land on main.
$ for ip in 10.{1..20}.0.1; do dig +short +subnet=$ip/24 \
    @127.0.0.1 -p 15353 api.foo.com. A; done | sort | uniq -c
     16 9.9.9.9       # main
      4 7.7.7.7       # canary

# Rollback is a ref move: zero packets dropped.
$ zonegit --repo ./.zonegit reset --hard main
```

Per-rule match counts ship out the `/metrics` endpoint
(`zonegit_dns_queries_total{qtype,rcode}` plus the active-branch info
gauge) so a Grafana dashboard sees the cohort split in real time.

### 10. AXFR — serve secondaries like any other authority

```sh
$ dig @127.0.0.1 -p 15353 +tcp foo.com. AXFR
foo.com.   300  IN  SOA  ns1.foo.com. admin.foo.com. 2 7200 3600 1209600 300
foo.com.   300  IN  NS   ns1.foo.com.
ns1.foo.com. 300 IN  A    10.0.0.1
api.foo.com. 300 IN  A    9.9.9.9
www.foo.com. 300 IN  CNAME api.foo.com.
foo.com.   300  IN  SOA  ns1.foo.com. admin.foo.com. 2 7200 3600 1209600 300
```

Any standard BIND / Knot / PowerDNS secondary can `transfer foo.com from
127.0.0.1` and stay in sync via its usual refresh loop. Because the apex
SOA serial auto-increments on every commit that touches the zone,
IXFR-style polling Just Works (we don't ship IXFR yet, so secondaries
re-AXFR; that's a v4 optimisation).

### 11. Signed commits

```sh
$ zonegit keygen ~/.zonegit/zonegit.pub ~/.zonegit/zonegit.key
$ zonegit --repo ./.zonegit sign-commit HEAD --key ~/.zonegit/zonegit.key
signed 7c2af3b -> 9e10c2d
$ zonegit --repo ./.zonegit verify HEAD --key ~/.zonegit/zonegit.pub --chain
OK     9e10c2d  failover to DR site
OK     4f1c2a9  initial import
```

The signature lives in a reserved header in the commit object
(`pkg/object/commit.go`), so signed and unsigned commits share storage
byte-for-byte except for that single line.

### 12. PR-style change review

```sh
$ zonegit --repo ./.zonegit propose api-failover --from main
proposal "api-failover" created from 7c2af3b (HEAD now on api-failover)

$ zonegit --repo ./.zonegit set api.foo.com. A 300 9.9.9.9 -m "failover api"
[api-failover a31b07d] failover api

$ zonegit --repo ./.zonegit review api-failover --into main
proposal "api-failover" vs main — 2 change(s):
  ~ api A
  ~ @  SOA       # auto-bumped serial

$ zonegit --repo ./.zonegit approve api-failover --into main
Approved "api-failover": fast-forward to a31b07d on main.
```

The verb names exist purely to make ServiceNow / change-management
conversations feel native. Underneath it's `branch + checkout`,
`diff a..b`, and `checkout main; merge`.

## What's coming next (talking points)

These are real items on the [roadmap](docs/ROADMAP.md) and the surface
that supports them is already in place. The code is not.

- **DNSSEC** — DNSKEY, RRSIG, NSEC/NSEC3 live in the apex like any
  other RRset, so they're already storable as Blobs and reachable by
  the resolver. v4 adds signing on the write path (`zonegit sign-zone
  --ksk ... --zsk ...`) and verification on the resolver. KSK/ZSK
  rollover becomes "branch + ref move".
- **Replication** — branches are content-addressed pointers, so a pull
  replica is *"give me every reachable object from `refs/heads/main`
  that I don't already have"*. The wire protocol is dumb: it walks the
  Merkle DAG. v5 ships a pull mode; multi-master with per-branch
  ownership is v6.
- **Multi-zone** — today, one repo = one zone. The object model is
  already zone-blind (the zone name is just persisted metadata), so v5
  reshapes the on-disk layout to `refs/heads/<zone>/<branch>` and
  unlocks one daemon serving many zones.
- **CoreDNS plugin** — `pkg/resolve.Handle` is already the seam. A
  CoreDNS plugin is ~200 LoC of glue wrapping that function and
  registering it with the Corefile parser. Listed at v6 because the
  authority story has to be airtight first.

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
make bench       # benchmarks (5 runs per case; benchstat-friendly)
make lint        # golangci-lint (or go vet as a fallback)
make demo        # end-to-end demo
make help        # list all targets
```

## Benchmarks

Run:

```sh
make bench
```

To compare two benchmark runs with [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat):

```sh
go test -run=^$ -bench=. -benchmem -count=5 ./... > old.txt
go test -run=^$ -bench=. -benchmem -count=5 ./... > new.txt
benchstat old.txt new.txt
```

Benchmark numbers are indicative and environment-dependent; treat them as
regression signals, not strict performance contracts.

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
