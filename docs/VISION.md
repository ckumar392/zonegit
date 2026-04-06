# dnsdb — Vision & Planning Notes

> Captured from initial design discussion. This file is the canonical "why"
> of the project. Re-read before any major design decision.

---

## 1. One-liner

**Git semantics for authoritative DNS.** Zone state is content-addressed and
immutable; the "current zone" is just a pointer to the latest commit. Every
byte that ever resolved is still in the store, cryptographically chained,
signed, and queryable.

## 2. The inversion that makes this special

| Today's DNS | dnsdb |
|---|---|
| Zone state is **mutable** — SET A record, old value is gone | Zone state is **immutable + content-addressed** |
| Audit logs are bolted-on side effects | Audit is the data model |
| Rollback = restore from backup, pray | Rollback = `dnsdb revert <hash>` |
| Change review = Jira ticket | Change review = PR with RR-aware diff |
| One global state | N branches, all valid, all servable |

That single inversion unlocks every capability below.

## 3. Capabilities unlocked

- **Point-in-time resolution** — `dig @dnsdb foo.com @2025-10-01T14:00`
- **Cryptographic audit** — Merkle DAG + Ed25519 signed commits, tamper-evident
- **Surgical rollback** — undo one change without clobbering 14 unrelated ones
- **Zone PRs** — branch, edit, diff, review, merge
- **Multi-region replication** — same algorithm Git uses to sync the kernel repo
- **Compliance proofs** — "prove this record existed on Jan 14" in one command
- **Blast-radius preview** — `diff main..staging` before you ship
- **Branch-based canary serving** — 5% of traffic resolves against a candidate branch

## 4. Killer demos to drive toward

```bash
# Time travel
dig @dnsdb foo.com A @2025-10-01T14:00Z

# Forensics
dnsdb blame foo.com A
# api.foo.com  A  10.0.99.4  <- bob@acme, commit 7c2a, 12 days ago, "failover to DR"

# Surgical revert
dnsdb revert 7c2a
# undoes ONE change, leaves 14 others alone

# PR-style workflow
dnsdb checkout -b hotfix
dnsdb set foo.com A 1.2.3.4
dnsdb diff main hotfix
dnsdb merge hotfix → main

# Canary cutover
dnsdb branch create canary
dnsdb set api.foo.com A 5.6.7.8
dnsdb serve --branch=canary --select="client.subnet=10.0.0.0/8"
dnsdb merge canary main      # promote, atomic
# OR
dnsdb branch delete canary   # rollback, zero impact
```

---

## 5. Use cases (real-world, named pain)

### UC1 — The 3 AM Rollback
**Persona:** SRE at an  customer (Acme Corp). Junior NetOps fat-fingers
a record at 11 PM. 14 legitimate changes happen between then and 2 AM PagerDuty.
Today's only safe recovery is "restore 6h-old snapshot" → causes a *second*
outage. With dnsdb: `dnsdb log → dnsdb blame → dnsdb revert <hash>` in 45 sec.

**Important clarification:** the "engineer" in this story is the **customer's
SRE** managing **their own** zone data via the  product, NOT an 
engineer.  engineers operate the platform; customers operate their data
on top of it.

### UC2 — The Compliance Auditor
PCI-DSS / SOC2 / FedRAMP environments. "Prove `payments.bank.com` resolved to
IP X on March 14 09:00 UTC, and the change was approved by an authorized
engineer." Today: 3 days of log archaeology → low-confidence PDF.
With dnsdb: one signed-and-verifiable command.

### UC3 — GitOps for DNS
Cloud-native teams cobble `octodns`/`dnscontrol`/CI to manage DNS like
Terraform. The DNS server still has its own state that drifts. With dnsdb,
**the DNS server IS the Git repo.** No drift possible. Push → served. Merge
PR → atomic cutover.

### UC4 — NIOS-to-your control plane Migration (hits home)
Current import pipeline (`DeleteOrphanNstarObjects`,
`ReplaceExistingSnapshotWithCurrentSnapshot`, the metadata-NULL bug fixed in
DDIDNS-7943) is reinventing what content-addressable storage gives free:
- Each NIOS sync = one commit on `nios/<grid-id>` branch
- "What changed?" = `dnsdb diff HEAD~1`
- "Roll back bad import" = `dnsdb reset --hard`
- Orphan detection = trivial set difference between trees
- The DDIDNS-7943 bug class **literally cannot exist** in this model

### UC5 — Canary DNS / Blue-Green for Zones
DNS today has no "5% of users get the new value" knob. Weighted RR is random,
not targeted. Geo-steering targets only by location. None are versioned.

dnsdb makes the candidate state a **first-class versioned object**. Server
matches incoming queries against selectors → routes to a branch's tree:

```
incoming query for api.foo.com from 10.0.5.7
  → selector "client.subnet=10.0.0.0/8" matches → branch=canary → 5.6.7.8
incoming query from 203.0.113.9
  → no selector matches → branch=main → 1.2.3.4
```

Selector grammar (envisioned):
- `client.subnet=10.0.0.0/8`
- `hash(client_ip) % 100 < 5`        — sticky 5% canary
- `edns.cookie.tag=canary-cohort-A`  — explicit opt-in
- `time.hour in [2,3,4]`              — change-window only
- `geo.country=IN`                    — regional rollout

**Why this is unique:** every other DNS vendor does runtime tricks bolted on
top of mutable zone storage. None of them have a versioned candidate, RR-aware
diff, atomic promotion, atomic rollback, or permanent forensic record of the
experiment. You can't bolt this onto a mutable store — you have to design
branches as first-class from day 0. That's why we start there.

Real scenarios where UC5 changes how customers operate:
- **Datacenter migration** (40 RRs flipped over 2 weeks of zero-risk testing
  instead of one terrifying maintenance window)
- **CDN vendor swap** (200 CNAMEs)
- **DNSSEC rollout** (gradual cohort expansion instead of big-bang)
- **A/B testing infra at the DNS layer**
- **Compliance change windows** (auto-merge at 6:01 PM)

### UC6 — Forensics after a DNS hijack
Phished account injects malicious A record for 47 minutes, reverts. Three
months later, security wants forensics. Today: maybe possible, low confidence.
With dnsdb: malicious commit still in the object store, signed (or notably
unsigned), exact byte range of impact, signed diff, reflog of every branch
movement. Git-style forensics for DNS doesn't exist anywhere.

---

## 6. Why this could be an  product

 sells NIOS (legacy on-prem) and your control plane DDI (cloud-managed). Strategic
narrative: enterprises want cloud-managed DNS *with enterprise-grade governance*.

**dnsdb is the governance layer your control plane is missing today:**
- No safe rollback of a single change
- No diff/preview before apply
- No multi-stage promotion (dev → staging → prod)
- No cryptographic compliance story

### Three product packagings (pick altitude)

1. **Internal infrastructure play** — re-platform your control plane config store on a
   versioned core. Customers see no API change, just instant rollback, perfect
   audit, zero NIOS-import drift bugs. Sells itself to eng org by killing
   entire bug classes.

2. **Premium feature for regulated verticals** — "your control plane Compliance Edition."
   Cryptographic audit, point-in-time queries, signed commits, FIPS-validated
   KMS. Target: banks, healthcare, federal. Cisco Umbrella / Akamai / NS1 /
   Cloudflare have **nothing like it.**

3. **Open-source halo project** — Apache 2.0 `dnsdb` + CoreDNS plugin. Talks
   at DNS-OARC, KubeCon, USENIX LISA. HashiCorp/Confluent/Grafana playbook:
   open-core drives enterprise sales.

### Objections answered

- *"Doesn't Route 53 / Azure DNS already version zones?"* — They version
  *configuration*, not *resolution-time state*. No point-in-time queries, no
  diffs, no merges, no signed history.
- *"Storage cost?"* — Typical zones change <1% per day. Deduplicated CAS is
  *smaller* than today's audit logs + backups combined.
- *"Performance?"* — Resolution path doesn't touch history; pointer-deref
  into a hot tree. Same latency as a zone-file backed server.
- *"Who buys this?"* — Anyone who's been on a 3 AM DNS bridge call.

---

## 7. Tech decisions locked

| Decision | Choice | Reason |
|---|---|---|
| Language | Go 1.22 | Existing stack; miekg/dns; CoreDNS-compatible |
| Wire format | `miekg/dns` | Battle-tested in 's stack |
| v0 storage | **BadgerDB** (embedded) | Zero-deps demo, fast |
| v1+ storage | Postgres adapter | Ops parity with prod |
| Hash | SHA-256 | Standard, no surprises |
| Signing | Ed25519 | Fast, small, well-supported |
| RPC | gRPC | Plumbing/porcelain split a la Git |
| Server | Custom DNS server (miekg/dns) → CoreDNS plugin later | Faster path to demo than forking BIND |
| Module path | `github.com/ckumar392/dnsdb` | Per user decision |

## 8. v0 scope (locked)

**Goals:**
- Single zone
- Single Badger backend
- Round-trip: `init → add → commit → log → diff → blame → serve`
- `dig @dnsdb foo.com` answers correctly from the latest commit
- All operations covered by tests

**Non-goals for v0:**
- Branches/merge (v1)
- Canary serving (v2)
- Signed commits (v3)
- Postgres backend (v4)
- Multi-zone (v5)
- Replication / push/fetch (v6)

This sequencing is deliberate: each layer builds on the last without rework.
Branches are designed *into* v0's data model so v1 is additive, not a rewrite.

## 9. Glossary

| Term | Meaning |
|---|---|
| **RR** | Resource Record — a single DNS record (name, type, class, ttl, rdata) |
| **RRset** | Set of RRs sharing name+type+class. The atomic unit DNS speaks. |
| **Blob** | Content-addressed serialization of one RRset. Hash = SHA-256 of canonical form. |
| **Tree** | Sorted Merkle node listing all RRsets in a zone snapshot. |
| **Commit** | (parent, tree, author, msg, timestamp, signature). Identifies a zone version. |
| **Branch** | Mutable named pointer to a commit. e.g. `main`, `canary`, `staging`. |
| **Tag** | Immutable named pointer to a commit. e.g. `v2026.04.25-prod`. |
| **Ref** | Generic name for branch or tag. `refs/heads/main`, `refs/tags/...`. |
| **HEAD** | Current branch pointer for the working session. |
| **Reflog** | Append-only log of every ref movement. Forensic recovery. |
| **Object store** | The CAS — given a hash, get the bytes. Pluggable backend. |
| **Working tree** | The materialized RRset state that the server resolves against. |

## 10. Open questions (revisit before each milestone)

- [ ] Selector DSL grammar for canary serving (UC5). Lock before v2.
- [ ] DNSSEC story — sign on commit, or sign on serve? Lock before v3.
- [ ] Multi-tenancy model — branch namespaces vs separate repos. Lock before v5.
- [ ] Replication wire protocol — gRPC streaming vs Git-pack-style. Lock before v6.
- [ ] Garbage collection — when can old commits be pruned? Probably never for compliance. Document.

## 11. Success criteria (how we know it worked)

- **Engineer-test:** show a senior DNS engineer the `dig @past-time` demo;
  measure time-to-"wait, how?"
- **Bug-class test:** can the DDIDNS-7943 bug class be expressed in this
  model? (Should be impossible by construction.)
- **Conference test:** could this be a 30-minute talk at DNS-OARC? (Yes.)
- **Product test:** can a your control plane PM see the customer feature in 60 seconds?
  (Yes — `View History` / `Revert` / `Compare with staging` UI buttons.)
