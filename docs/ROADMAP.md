# zonegit Roadmap

> Strict sequencing. Each version builds on the previous without rework.
> If a v(N) need would force a v(N-k) rewrite, we redesign v(N-k) NOW
> rather than ship and regret.

---

## v0 — "It works on one zone, on one machine" (target: ~2–3 weeks of evenings)

**Goal:** end-to-end demo of init → commit → log → diff → blame → serve.

### Must have
- [x] Repo skeleton, `go.mod`, Makefile, CI
- [x] `pkg/store` interface + Badger adapter + memory adapter + conformance suite
- [x] `pkg/object` — Blob/Tree/Commit/Tag, canonical encoding, hashing
- [x] `pkg/zone` — miekg/dns ↔ Blob, zonefile parser using miekg/dns
- [x] `pkg/refs` — branches, HEAD, reflog, CAS
- [x] `pkg/history` — log, diff, blame
- [x] `pkg/repo` — public Go API
- [x] `cmd/zonegit` — init, import (zonefile), set, delete, log, show, diff, blame, status, branch (list/create), checkout, cat-object
- [x] `cmd/zonegitd` — minimal DNS server resolving against HEAD (read-only Badger; per-query reopen for live updates)
- [x] Demo script that scripts the full flow (`scripts/demo.sh`)

### Explicitly NOT in v0
- Branch *cutover at the server* (server reads HEAD only; branches exist
  but aren't yet swappable at runtime — that's v1)
- Merge (v1)
- Revert (v1)
- Canary serving / selectors (v2)
- Signed commits (v3)
- Postgres backend (v4)
- gRPC API (v4)
- Replication / push/fetch (v5)
- Multi-zone (v5)
- CoreDNS plugin (v6)

### Done definition
A senior engineer can clone the repo, run `make demo`, and watch:
1. A zonefile import and commit
2. `zonegit log` show one commit
3. `dig @127.0.0.1 -p 5353 api.foo.com` return the right A
4. `zonegit set api.foo.com A 9.9.9.9 && zonegit commit -m "test"`
5. `zonegit log` show two commits
6. `zonegit diff HEAD~1 HEAD` show the RR change
7. `zonegit blame api.foo.com A` show "you, just now"
8. `dig` return the new value

---

## v1 — "Branches mean something at serve time" (~1 week)

- [x] Server can be told "serve branch X" and switch atomically on ref change
      (`zonegitd --branch X` resolves `refs/heads/X` per query, picking up
       commits from the writer process without a restart)
- [x] `zonegit merge <branch>` (fast-forward + 3-way for non-conflicting changes)
- [x] `zonegit revert <commit>` — produces an inverse commit
- [x] `zonegit reset --hard <ref-ish>`
- [x] `pkg/merge` with conflict types (`both-modified`, `deleted-modified`,
      `add-add`)

### Why this is a separate version
Merge correctness deserves focused testing. Adding it to v0 risks shipping
broken merges with the demo, which would be embarrassing.

---

## v2 — "Canary serving" (~1–2 weeks)

The headline UC5 feature.

- [ ] Selector DSL: minimal grammar (`client.subnet`, `hash`, `geo`,
      `time`, boolean ops)
- [ ] Server config: ordered list of `(selector → branch)` rules
- [ ] `zonegit serve --branch=canary --select="..."` shorthand
- [ ] EDNS Client Subnet handling
- [ ] Metrics: per-branch query rate so customers can see canary % in
      Grafana
- [ ] Soak test: 10k qps mixed traffic, no leaks, p99 < 1ms

### Why this is a separate version
Selector grammar deserves a real spec doc ([docs/SELECTORS.md](SELECTORS.md))
before implementation; getting the syntax wrong is annoying to fix later.
The spec is locked; v2 code starts only after the open questions in
SELECTORS.md §8 are answered.

---

## v3 — "Signed history" (~1 week)

- [ ] Ed25519 keypair management (file-based v3, KMS in v4+)
- [ ] Sign commits and tags
- [ ] `zonegit verify <ref>` — verifies signature chain back to root
- [ ] `zonegit log --signature` shows signer per commit
- [ ] Server policy: refuse to serve unsigned commits when `--require-signed`

### Why now
Every preceding version is fully forward-compatible with adding
signatures (the `signature` header was reserved in v0's commit format).
This is purely additive.

---

## v4 — "Production storage + control plane" (~2 weeks)

- [ ] Postgres adapter implementing `Storage` interface
- [ ] Migration tool: Badger → Postgres
- [ ] gRPC API (mirror of `pkg/repo` operations)
- [ ] Auth: mTLS + simple ACL on branches
- [ ] Container image, helm chart in a sister repo

### Why now
By v4 we've validated the design with a real backend. Postgres adapter
is "just" implementing one interface. If it isn't — that's the signal
the interface is wrong, and we fix it before going further.

---

## v5 — "Replication" (~2–3 weeks)

- [ ] Push/fetch wire protocol over gRPC streaming
- [ ] Pull replication: secondary nodes fetch and serve read-only
- [ ] Multi-zone repo layout (today: one-zone-per-repo; v5: many-zones-per-repo)
- [ ] Conflict-free per-branch ownership (each branch has a "home" node;
      pushes route there)

---

## v6 — "Ecosystem" (open-ended)

- [ ] CoreDNS plugin
- [ ] Terraform provider
- [ ] OpenAPI for gRPC API
- [ ] Web UI for change review (separate repo)
- [ ] BIND9 catalog-zone bridge (publish zonegit branches as BIND zones)

---

## Risk register (revisit before starting each version)

| Risk                                                       | Likelihood | Impact   | Mitigation                                                                 |
| ---------------------------------------------------------- | ---------- | -------- | -------------------------------------------------------------------------- |
| Canonical RR encoding has subtle bugs that break dedup     | Med        | High     | Property tests in v0; corpus from public zonefiles                         |
| Badger's GC behavior surprises us at scale                 | Low        | Med      | Memory adapter for tests; doc behavior; switch to Pebble if needed         |
| Selector DSL grows into a programming language             | Med        | Med      | Lock minimal grammar before v2 starts; resist scope creep                  |
| Postgres adapter exposes leaky `Storage` interface         | Med        | High     | Build memory + Badger BEFORE designing Postgres so interface is real       |
| Merge conflicts in DNS are weirder than Git's              | High       | Med      | Explicit conflict-type taxonomy in v1 with tests per type                  |
| Performance regression as we add layers                    | Med        | Med      | Continuous benchmarks (`testing/Benchmark`) tracked in CI from v0          |
| The whole thing turns out to be slower than zone-file BIND | Low        | Critical | Day-1 benchmark dimension. Stop and redesign if we exceed 2x BIND latency. |

---

## Forcing functions (deadlines we set ourselves)

- **End of v0:** working `dig` answer in front of one trusted skeptical
  engineer. If they don't say "huh, neat" → revisit pitch before continuing.
- **End of v2:** internal demo of canary cutover. UC5 is the
  pitch; if it doesn't impress, the product story is wrong, not the code.
- **End of v4:** open-source the v0–v3 code under Apache 2.0 with a real
  README, examples, and a CoreDNS-OARC-style blog post.
