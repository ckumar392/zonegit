# zonegit

> **Git semantics for authoritative DNS.**
> Zone state is content-addressed and immutable. The current zone is just
> a pointer to the latest commit. Every byte that ever resolved is still
> in the store — cryptographically chained, signed, and queryable.

```bash
# Time-travel
$ dig @zonegit api.foo.com A @2025-10-01T14:00:00Z
;; ANSWER SECTION:
api.foo.com.    300    IN    A    10.0.7.42

# Forensics
$ zonegit blame api.foo.com A
api.foo.com.  A  10.0.99.4   <- alice@acme, commit 7c2a, 12 days ago
                                "failover to DR site"

# Surgical undo
$ zonegit revert 7c2a            # reverts ONE change, leaves 14 others alone

# Zero-impact canary
$ zonegit branch create canary
$ zonegit set api.foo.com A 5.6.7.8
$ zonegit serve --branch=canary --select="client.subnet=10.0.0.0/8"
$ zonegit merge canary main      # promote, atomic. or:
$ zonegit branch delete canary   # rollback, zero impact
```

## Status

🚧 **v0 in active development.** See [docs/ROADMAP.md](docs/ROADMAP.md).

## Why?

Today, every authoritative DNS system on the planet treats zone state as
**mutable**. You SET an A record, the old value is gone. Audit logs are
bolted on. Rollback means "restore from backup and pray." Change review is
a Jira ticket, not a code review.

`zonegit` flips that. Zone state becomes immutable + content-addressed. That
single inversion unlocks: time-travel, surgical revert, branch-based
canary serving, cryptographic compliance, GitOps-for-DNS, and forensic
audit — none of which any DNS product on the market offers today.

Read the full pitch in [docs/VISION.md](docs/VISION.md).

## Documentation

| Doc | Purpose |
|---|---|
| [docs/VISION.md](docs/VISION.md) | The why: use cases, product framing, glossary |
| [docs/OBJECT_MODEL.md](docs/OBJECT_MODEL.md) | The what: Blob/Tree/Commit/Tag/Ref shapes, canonical form, invariants |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | The how: package layering, the `Storage` seam, lifecycles |
| [docs/ROADMAP.md](docs/ROADMAP.md) | The when: v0…v6 sequencing, risk register |

## Quickstart (once v0 lands)

```bash
make build
./bin/zonegit init my-zone
./bin/zonegit import foo.com.zone
./bin/zonegit commit -m "initial import"
./bin/zonegit log
./bin/zonegitd --addr :5353 &
dig @127.0.0.1 -p 5353 api.foo.com A
```

## Repo layout

```
zonegit/
├── cmd/
│   ├── zonegit/             # CLI (cobra)
│   └── zonegitd/            # DNS + control-plane server
├── pkg/
│   ├── store/             # Storage interface + Badger/memory backends
│   ├── object/            # Blob/Tree/Commit/Tag, canonical form, hashing
│   ├── zone/              # miekg/dns ↔ object bridge, RR canonicalization
│   ├── refs/              # Branches, HEAD, reflog, atomic CAS
│   ├── history/           # log, diff, blame
│   ├── merge/             # 3-way RRset merge (v1+)
│   ├── resolve/           # DNS query path
│   └── repo/              # Public Go API
├── internal/              # Implementation details, not for external use
├── plugin/coredns/        # CoreDNS plugin (v6+)
├── scripts/               # demo, dev helpers
├── docs/
└── tests/e2e/
```

## Build / Test

```bash
make help        # see all targets
make test        # unit tests
make test-race   # tests with race detector
make lint        # golangci-lint if installed, else go vet
make demo        # end-to-end demo
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
