# zonegit

## Git semantics for authoritative DNS — SME pitch brief

> **One sentence:** zonegit is a content-addressed, version-controlled object model for authoritative DNS zones. Every change is an immutable commit. The live zone is a pointer to the latest commit on a branch. That single inversion gives you `log`, `diff`, `blame`, time-travel reads, and branch-based rollout — operations that today's authoritative DNS servers don't expose at all.

| | |
|---|---|
| **Repository** | <https://github.com/ckumar392/zonegit> |
| **Latest release** | [`v0.7.0`](https://github.com/ckumar392/zonegit/releases/tag/v0.7.0) — 24-step end-to-end demo |
| **Build status** | CI green on `main` · race-detector clean · all packages tested |
| **License** | Apache 2.0 |
| **Status** | Seven public-preview releases shipped. Vision-demo-ready. v0.8 (replication) and v0.9 (NIOS bridge) are the milestones that turn this into a credible Infoblox product feature. |

---

## Contents

1. [Executive summary](#executive-summary)
2. [NotebookLM input — technical brief](#notebooklm-input--technical-brief)
3. [Technical slide deck — 15 slides](#technical-slide-deck--15-slides)
4. [Recording and presentation tips](#recording-and-presentation-tips)
5. [Sequencing for the actual SME meeting](#sequencing-for-the-actual-sme-meeting)
6. [Appendix A — 24-step demo, what each step proves](#appendix-a--24-step-demo-what-each-step-proves)
7. [Appendix B — Performance numbers](#appendix-b--performance-numbers)
8. [Appendix C — Compliance control mapping](#appendix-c--compliance-control-mapping)
9. [Appendix D — Glossary](#appendix-d--glossary)

---

## Executive summary

Authoritative DNS hasn't changed how it stores zone state since the 1980s. Every other engineering discipline has version control. DNS doesn't. zonegit is the substrate that fixes that — a content-addressed Merkle DAG underneath authoritative DNS, replacing the mutable database that today holds zone records. The DNS wire protocol on the wire stays identical. Existing secondaries, resolvers, and zone-transfer mechanics don't change. Only the storage and change-management layer changes.

Seven public releases ship today. Branches, commits, signed history, time-travel, canary serving, full DNSSEC with Ed25519, AXFR + IXFR, a CoreDNS plugin, per-zone YAML configuration, SIGHUP-driven reload, multi-zone repos — all working, all tested, all reproducible by `make demo` in 90 seconds. Two remaining milestones (pull replication in v0.8, NIOS integration bridge in v0.9) turn this from an architecturally clean side project into a drop-in compliance upgrade for the Infoblox customer base most exposed to regulated audits — banks, hospitals, federal agencies, multi-tenant MSPs.

The ask of the SME audience is specific: **staff one engineer for six months to land v0.8 and v0.9.** At that point zonegit becomes a credible product feature in NIOS-X, a compliance-pack SKU for DDIaaS, or both. The architectural cost is already paid.

---

## NotebookLM input — technical brief

> **How to use this:** paste everything in this section (from the next heading down to *"The one thing to take away"*) into NotebookLM as a single source. NotebookLM will generate a two-host conversational podcast that an SME audience can absorb in ~12 minutes as pre-read. The brief is written as narrative prose, not bulleted notes, so the hosts have texture to riff on.

### What zonegit is

zonegit is a content-addressed, version-controlled object model for authoritative DNS zones. Every change is an immutable commit. The live zone is just a pointer to the latest commit on a branch. That one inversion gives you log, diff, blame, time-travel reads, and branch-based rollout — operations that BIND, Knot, PowerDNS, and Route 53 simply don't expose.

It is not a DNS server in the sense of replacing BIND. It is a versioned state store that sits underneath authoritative DNS, replacing the mutable database that today holds zone records. The DNS protocol on the wire stays identical. Existing secondaries and resolvers don't change. Only the storage and change-management layer changes.

### Why this matters now

Authoritative DNS hasn't changed how it stores zone state since the 1980s. Records live in mutable databases or flat files. Changes are append-only audit logs maintained out of band. Rollback is a restore-from-backup operation that nobody actually tests. Multi-team coordination is human bureaucracy implemented in ServiceNow tickets. Every regulated customer — banks, hospitals, federal agencies — spends weeks per year fighting their DNS audit story.

Meanwhile every modern engineering org treats source code as a versioned graph. Pull requests. Code review. Cryptographic commit signatures. Rollback in seconds. Audit trail by construction. None of that exists for DNS state today.

zonegit treats authoritative DNS state the way modern engineering treats source code. Every change becomes a signed, content-addressed commit. Rollback is one ref move. Change review is a pull request. Audit is a query against the commit graph.

### The conceptual model

Think of zone state as a tree. Each zone has a tree of labels. Each label points at either deeper labels or at the actual RRsets — the A records, MX records, NS records. Now version-control that tree the way Git version-controls source files. A blob is one canonicalised RRset. A tree is a directory of labels mapping to subtrees and RRset blobs. A commit is a snapshot of the zone tree with parent links and metadata. Refs — branches and tags — are named pointers into the commit graph.

The clever bit is content addressing. Two zones with the same RRset for `api.example.com A` share storage byte-for-byte. The hash of every node in the tree is determined by the hash of its children, all the way up to the commit root. That makes diffs cheap: changes propagate up exactly one path in the tree. It makes the audit log tamper-evident: you can't quietly modify a historical commit because doing so changes every descendant hash. And it makes replication a Merkle protocol: *"give me every object you have that I don't."*

### What's working today, end to end

Seven public preview releases, each adding capabilities without rewriting earlier work.

**v0.3 — demo readiness.** End-to-end demo on a single zone. Init, import, set, log, diff, blame. The daemon serves DNS over UDP and TCP, picks up commits without restart, and produces Prometheus metrics. Canary serving by stable hash of the client `/24`. AXFR for secondary servers. Time-travel: run a second daemon pinned to a historical commit and `dig` against the past.

**v0.4 — multi-zone.** One repo, many zones, one daemon serving all of them on one port. Branches are scoped per zone. The daemon's reconciler discovers zones added at runtime without restart. Object-backed HEAD so zone names of any length work.

**v0.5 — operational polish + DNSSEC scaffold.** IXFR over the commit DAG. Per-zone YAML configuration. DNSSEC scaffold proving the object pipeline carries DNSKEY, RRSIG, and NSEC records like any other RRset.

**v0.6 — real DNSSEC.** Ed25519 keypair generation and management. RRSIG records with valid signatures that resolvers can verify. SIGHUP-driven config reload — the daemon picks up new YAML without a restart. IXFR walks the full commit DAG so merge ancestors are reachable.

**v0.7 — integration completeness.** A CoreDNS plugin that lets operators drop zonegit into their existing CoreDNS deployment. A `zonegit ds` helper that prints the DS record for the parent zone to publish, completing the DNSSEC chain of trust. Auto-resigning on every commit that touches a signed RRset, so signatures never drift past expiration.

The demo is a 24-step shell script. It runs end-to-end on a laptop in roughly 90 seconds.

### How a customer would actually use this

**Sarah is a senior DNS engineer at a top-ten US bank.** Today, fourteen DNS changes per week land through ServiceNow tickets, manually applied to her NIOS Grid, manually verified, manually rolled back if something breaks. The DNS team is the company-wide bottleneck. With zonegit, each app team has a branch. They open proposals through a self-service portal. Sarah reviews like a code reviewer. She approves with her cryptographic key. The merge fast-forwards production, and secondaries pick up the change in two hundred milliseconds. Throughput goes from 14 changes per week to 14 changes per day. Better safety, faster delivery.

**Raj is an SRE at a SaaS company.** It's 03:42 UTC. His pager fires. NXDOMAIN spikes on production. His first move is to start a second `zonegitd` in time-travel mode pinned 30 minutes ago. He digs against that, sees what the answer used to be, compares to current, finds the bad commit, reverts in one command. Mean time to recovery drops from 90 minutes to 4. The post-incident review is one `git diff` instead of three days of log archaeology.

**Marcus is a compliance officer at a regional hospital network.** The auditor asks for proof that every DNS change touching ePHI systems over the past 18 months had a documented approver who was different from the requester. Today, that's a manual export from NIOS plus a written attestation. With zonegit, `zonegit log --signature --since=18mo` returns a cryptographically signed chain back to the root commit. Each commit has the proposer's signature *and* the approver's signature. The Merkle DAG proves nothing was inserted or modified after the fact. Audit prep goes from a two-week project to a 20-minute query.

**Lin is a cloud architect at a managed service provider serving 200 customer tenants.** Each customer is a branch in one zonegit repo. Role-based access mapped to the existing CSP identity restricts each customer's operators to their own branch. One `zonegitd` process serves all 200 tenant zones. When customer A's operator does something dangerous, only customer A's branch is affected — blast radius is bounded by the ref boundary, not by hope.

### Why this fits Infoblox specifically

Infoblox sells DDI: DNS, DHCP, IPAM. The customer base skews heavily regulated — financial services, healthcare, US federal, multi-tenant MSPs. Every one of those customer types has a compliance officer who wants the kind of audit trail zonegit produces by construction.

Today, Infoblox's NIOS Grid has a change-audit subsystem because customers demanded it during procurement. The export-to-CSV workflow is the workaround for *"our auditor needs a list of changes with approvers attached."* That workaround exists because the underlying data model doesn't naturally produce the artifact.

zonegit produces the artifact natively. The compliance-pack story writes itself: cryptographically signed DNS change history, tamper-evident audit logs, point-in-time reconstruction for forensic timelines, segregation of duties by construction. Map each feature to a control objective in FedRAMP AU-9, SOX ITGC change management, HIPAA Security Rule §164.312, PCI-DSS Requirement 10. Every customer with an auditor already has budget allocated to this problem.

The path to integration is the missing piece. The next milestone — version 0.9 — is a NIOS bridge: a Go service that subscribes to NIOS zone changes and lands them as zonegit commits. That sentence is what turns *"interesting research project"* into *"drop-in compliance upgrade for existing Infoblox customers."*

### How the architecture stays clean

Three architectural decisions are worth understanding.

The first is the **storage interface seam**. Every object — blob, tree, commit, tag, ref — goes through a single Go interface called `Storage`. The interface knows nothing about DNS. It moves bytes by hash. Today there's a Badger backend for embedded use and an in-memory backend for tests. A Postgres backend, an etcd backend, a cloud-object-store backend — all are just new implementations of the same interface. The in-tree conformance test suite ensures every backend behaves identically.

The second is **layered packages with strict dependency direction**. The storage interface depends on nothing. The object model depends only on storage. The resolver depends on the object model. The CLI and daemon depend on the resolver. No package can reach across layer boundaries. This is enforced by CI, not by convention. It's why every milestone added features without rewriting earlier ones.

The third is **content addressing all the way down**. Every blob, tree, and commit has a SHA-256 hash that is determined entirely by its contents. Two zones with the same record share storage. The audit log *is* the commit graph itself — there's no separate audit table to keep in sync. Replication is a Merkle walk of *"what objects do I need that you have."* This is the same insight that powers Git, Docker images, Bitcoin, and most modern distributed databases. Applying it to DNS state is, frankly, overdue.

### Real performance numbers

On an Apple M4 Max running darwin/arm64 against a 10,000-host zone:

- **Single-name lookup latency:** 303 µs, single goroutine. That's ~3,300 queries per second per goroutine; multi-core scales nearly linearly.
- **Single-commit cost including SOA auto-bump:** 1.04 ms. Sub-millisecond commits even at ten thousand hosts.
- **Bulk import of a 10,000-host subzone in one commit:** 3.7 s. That's the bulk-import path; normal operational commits stay in the millisecond range.
- **Subzone delete with parent pruning (100 hosts):** 800 µs.

These are not BIND-beating numbers — BIND has had 25 years of allocation-tuning. They are good-enough numbers for the use case zonegit targets, which is the *write path* (commits per second at scale) and the *audit path* (point-in-time queries in milliseconds), not the *read path* at internet scale. The read path uses the same `pkg/resolve` code, regardless of whether you front it with `zonegitd` directly or with CoreDNS via the plugin.

### What's not built yet, named explicitly

- **Pull replication** for primary-secondary deployments — designed, not coded. The Merkle DAG makes it almost trivial; v0.8.
- **NIOS integration bridge** — the Infoblox-specific connector. v0.9. Highest-leverage feature for the SME audience.
- **Web UI for pull-request-style change review** — v1.0. The visual artifact for compliance officers who don't live in the CLI.
- **Production storage backends beyond Badger** — Postgres adapter is the next storage backend, slated post-1.0. The interface is ready.

These are all listed in the public roadmap and in CHANGELOG entries. Nothing is hand-waved.

### The one thing to take away

The differentiation is not performance. BIND will always be faster on the read path. The differentiation is **capabilities BIND structurally cannot have**: cryptographic audit history, branch-based rollout, time-travel reads, content-addressed dedup, Merkle replication. Compete where the structure favors you, not where BIND has had a quarter-century head start.

The compliance angle is the easiest first sell. Every regulated Infoblox customer already has budget for audit-evidence improvement. *"Cryptographically signed DNS change history with mathematical tamper-evidence"* is a sentence a compliance officer immediately understands and wants.

What it would take to staff this as a real Infoblox internal project: one engineer for six months to ship v0.8 and v0.9, at which point it becomes a credible product feature. The platform underneath is built. The architectural seams are clean. The integration path is one well-defined service away.

---

## Technical slide deck — 15 slides

Each row is one slide. The **Content** column is what the slide displays. The **Speaker notes** column is what you say while it's up.

| # | Slide title | Content on the slide | Speaker notes |
|---|---|---|---|
| 1 | **zonegit** — Git semantics for authoritative DNS | Title, your name, date, `github.com/ckumar392/zonegit` | *"I've been building a side project. Today I want to walk you through where it is, the architecture, and where I think it could fit at Infoblox. Total time: 15 minutes plus questions."* |
| 2 | The problem with mutable DNS state | Three bullets: (1) audit logs out of band, (2) rollback = restore from backup, (3) multi-team coordination = ServiceNow tickets | *"DNS state storage hasn't changed since the 1980s. Every other engineering discipline has version control. DNS doesn't. That gap is what zonegit closes."* |
| 3 | The flip | Two-column "before/after" diagram. Before: mutable DB → zone files → BIND. After: Merkle DAG → HEAD → BIND/CoreDNS. | *"One change. Replace the mutable database with a content-addressed commit graph. The DNS protocol on the wire doesn't change. Existing secondaries don't change. This is the slide."* |
| 4 | The object model | Table: Blob = canonical RRset. Tree = labels. Commit = snapshot with parent links. Tag/Ref = pointer. Visual of a 3-commit DAG. | *"Same primitives Git uses. The clever bit: identical RRsets across zones share storage byte-for-byte. Subtree hashes mean diff is O(changes), not O(zone size)."* |
| 5 | 24-step demo, 90 seconds | Terminal screenshot of demo highlights: init, dig, commit, time-travel, canary split, AXFR, propose/approve, real DNSSEC | *"I won't read this. Let me show it."* (Cut to a live terminal or screen recording running `make demo`.) |
| 6 | The architecture | Layered diagram: cmd → pkg/repo → {resolve, history, merge, zone, refs} → pkg/object → pkg/store. Arrows. | *"Strict layering enforced by CI. Storage is one interface — Badger today, Postgres tomorrow. Adding the CoreDNS plugin in v0.7 was 150 lines because the resolver was already extracted three milestones earlier."* |
| 7 | Persona: Sarah, DNS engineer at a bank | Pull quote: *"14 changes per week → 14 changes per day, with better safety."* Three bullets: propose, review, approve. | *"Self-service for app teams, gatekeeping for the DNS team, both at the same time. The verb names exist so this slots into how change management already talks."* |
| 8 | Persona: Marcus, hospital compliance officer | Pull quote: *"Audit prep: 2 weeks → 20 minutes."* Three bullets: signed commits, Merkle DAG = tamper-evident, point-in-time replay. | *"The auditor wants math, not policy. Hash chains give them math. A signed-commit chain is mathematically tamper-evident in a way a database audit table is not."* |
| 9 | Persona: Raj, SaaS SRE at 03:42 UTC | Pull quote: *"MTTR: 90 min → 4 min."* Three bullets: time-travel daemon, revert command, postmortem from `git diff`. | *"Time-travel debugging changes what's possible to know after an outage. You run a second daemon pinned to an old commit and dig against it like nothing happened."* |
| 10 | Where this hits compliance frameworks | Table mapping controls: **AU-9** (FedRAMP audit log integrity), **AU-10** (non-repudiation), **CM-5** (segregation of duties), **SOX ITGC**, **HIPAA §164.312** | *"These are specific controls in specific frameworks. Every regulated Infoblox customer lives under several of them. zonegit answers each control with math instead of policy."* |
| 11 | What's built (v0.3 → v0.7) | Capability grid: SOA auto-bump · canary · AXFR · time-travel · multi-zone · IXFR · per-zone config · DNSSEC · SIGHUP reload · CoreDNS plugin · DS helper · auto-sign | *"Seven public releases. Each milestone added features without rewriting earlier ones. That's the architectural payoff of the storage seam and the layering rules."* |
| 12 | Where it fits at Infoblox | Diagram: NIOS Grid → **(v0.9 NIOS bridge)** → zonegit repo → zonegitd / CoreDNS-with-zonegit → secondaries | *"The next milestone is the NIOS bridge. Without it, this is research. With it, it's a compliance product. That's the architectural distance between today and shippable."* |
| 13 | What's NOT done yet | Numbered list with effort estimates: v0.8 pull replication (~2w), v0.9 NIOS bridge (~3w), v1.0 web UI (~4w), post-1.0 Postgres backend | *"I'm not pretending this is finished. The architecture is ready; some milestones are unbuilt. Here's the honest picture, including the things I expect you to push back on."* |
| 14 | Performance numbers (M4 Max, darwin/arm64) | Four rows: lookup 10k = 303 µs, commit 10k = 1.04 ms, subzone-add 100 = 880 µs, subzone-churn 100 = 1.7 ms | *"Not BIND-fast. Doesn't need to be. The differentiation is capability, not raw throughput on the read path."* |
| 15 | The ask | Three bullets: (1) staff one engineer for six months, (2) target outcome: compliance-pack SKU shape, (3) the architectural cost is already paid | *"If the room agrees the direction is right, I'd like to talk about what it would take to do this as part of the day job. Specifically: one engineer for six months to land v0.8 and v0.9."* |

---

## Recording and presentation tips

If you record this yourself instead of running NotebookLM:

1. **Open with the live demo, not the slides.** Run `make demo` on a real terminal for the first 60 seconds. The 24-step output is more persuasive than any slide about what the system does. Skip to slide 4 (architecture) once the demo has landed.
2. **Slide 3 (the flip diagram) is *the* slide.** That's where the whole pitch crystallises. Spend a full minute on it. The rest of the deck is supporting evidence.
3. **Don't read slides 7, 8, 9 — let them sit.** The personas are designed to be glanced at. Stay quiet for five seconds after each appears. The quote and the metric are doing the work.
4. **On slide 13 (what's NOT done), be the one who brings up the limitations.** SMEs trust people who admit gaps. Saying *"DNSSEC is real but pull replication isn't yet — here's why and here's the plan"* lands much better than waiting for them to ask.
5. **The ask on slide 15 needs to be specific.** *"Staff one engineer for six months"* is a real ask with a real shape. *"Would love your feedback"* is not. The whole deck is in service of getting the room to consider that specific commitment.
6. **Target run time: 12–15 minutes of presentation, plus 10 minutes of Q&A.** The deck supports that pace if you don't dwell on slide 11 (capability grid — they'll read it themselves) or slide 14 (perf numbers — these are reference, not argument).

### Tuning for audience composition

- **Senior engineers** buy on the architecture (slides 4 and 6), not the personas. Expand those slides to two each; cut a persona.
- **Executives** buy on slide 10 (compliance frameworks) and slide 15 (the ask). They don't care about the Merkle DAG. They care about which budget line item this lands on. Expand compliance to three slides (one per vertical: FinServ, Healthcare, FedRAMP); make the ask more specific with named names.

The deck above is the engineer-default. Adjust the mix if your SME audience leans business.

---

## Sequencing for the actual SME meeting

| When | What |
|---|---|
| **Day 0** | Run NotebookLM on the brief in this document to produce a ~12-minute audio. Share it with the SMEs as pre-read so the meeting itself is for discussion, not exposition. |
| **Day 0 + 3** | Send a calendar invite with this brief (PDF) + the deck attached + the GitHub release URL. Title the meeting *"Versioned DNS state — direction review"*, not *"zonegit demo"*. |
| **Meeting day** | Run the live demo first. Walk slides 3, 6, 10, 12, 13, 15. Skip 2, 4, 5, 7–9, 11, 14 unless asked. Reserve the last 15 minutes for the ask and the discussion. |
| **Day +1** | Send a one-page follow-up summarising the questions the room asked, what you committed to follow up on, and the specific next decision (e.g., *"please confirm by Friday whether DDI Platform can carve out 0.25 FTE for v0.8 in Q3"*). |

The whole sequence is engineered to converge on one sentence in the room: **"What would it take to staff this?"** Everything in the brief and deck either earns that sentence or is overhead.

---

## Appendix A — 24-step demo, what each step proves

The shell script is at `scripts/demo.sh`. Each step prints the command it's running so the audience can read along.

| # | Step | What it proves |
|---|---|---|
| 1 | `go build` both binaries | Builds from source; nothing precompiled |
| 2 | `zonegit init foo.com.` | Persists zone metadata; --zone optional after |
| 3 | Import RFC 1035 zonefile | miekg/dns parsing, canonical RRset encoding, Merkle tree build |
| 4 | Start `zonegitd` with `/metrics` | Cached snapshotter (no per-query Badger reopen), Prom endpoint |
| 5 | `dig api.foo.com.` | Server-side CNAME chase, NODATA/NXDOMAIN classification |
| 6 | Edit api → 9.9.9.9 | Auto-bump apex SOA serial — secondaries detect via standard refresh |
| 7 | `dig` again | Same daemon, no restart, no zone reload |
| 8 | SOA before/after | Serial moved by exactly 1, automatically |
| 9 | log / diff / blame / status | Git semantics over DNS state |
| 10–11 | Branch, edit, fast-forward merge | Per-branch isolation; daemon picks up new HEAD on next packet |
| 12 | Revert HEAD | Inverse commit produced; api back to 9.9.9.9 |
| 13 | `reset --hard HEAD~1` | Branch tip moves; reverted commit becomes unreachable but objects remain |
| 14 | **Time-travel daemon** (`--at HEAD~1`) | Two ports, two answers, two points in time. No DNS tool shipping today can do this |
| 15 | **Canary routing** (`--canary canary:50`) | Stable bucket-hash by client `/24`; 16 distinct subnets split ~8/8 |
| 16 | **AXFR** | Full zone transfer over TCP, RFC 5936 compliant. Slaves any BIND/Knot/PowerDNS secondary |
| 17 | **propose / review / approve** | PR-style change-management vocabulary over branch+merge primitives |
| 18 | Multi-zone (add bar.com. at runtime) | One daemon, one port, two zones, no restart |
| 19 | **IXFR=1** | Live commit-DAG-driven delta transfer, RFC 1995 framing |
| 20 | `zone-keygen` + `sign-zone` | Real Ed25519 DNSKEY (KSK+ZSK) + RRSIG that resolvers validate |
| 21 | `zonegit ds foo.com.` | Parent-zone DS line ready to publish — completes chain of trust |
| 22 | `set --auto-sign` | RRset rotation re-signs in the same commit; signatures never drift |
| 23 | CoreDNS plugin side-by-side | Same repo, served from both `zonegitd` and a custom CoreDNS binary, identical answers |
| 24 | `/metrics` | Prometheus exposition with per-qtype/per-rcode counters and active-branch gauge |

---

## Appendix B — Performance numbers

Apple M4 Max, darwin/arm64, Go 1.26, 10,000-host zone unless noted.

| Benchmark | Time / op | Allocs / op | What it measures |
|---|---|---|---|
| `Lookup_10k` | **303 µs** | 10,041 | One name lookup against a 10k-host zone |
| `CommitOneChange_10k` | **1.04 ms** | 20,152 | One RR change + SOA auto-bump |
| `AddSubzone/Empty` | **8.8 µs** | 134 | Create an empty subzone label |
| `AddSubzone/Small100` | **890 µs** | 12,020 | Add a 100-host subzone in one commit |
| `AddSubzone/Large10000` | **3.7 s** | 50,752,744 | Bulk-import a 10k-host subzone — one-shot worst case |
| `AddSubzone/DeepNestedName` | **1.09 ms** | 16,190 | Add at `a.b.c.d` depth |
| `DeleteSubzone/Small100Cascade` | **800 µs** | 10,262 | Delete 100 hosts with parent pruning |
| `SubzoneChurn/Small100` | **1.7 ms** | 22,117 | Add-then-delete-then-add cycle |

**The two numbers worth quoting at the SME demo:** `CommitOneChange_10k ≈ 1 ms` (so the SOA-bump cost is ~5–8% of one commit, paid once) and `Lookup_10k ≈ 300 µs` (single-threaded; multi-core scales near-linearly to roughly 1× to 2× of BIND on the same hardware, which is the right order of magnitude for a first-cut implementation with no resolution cache).

---

## Appendix C — Compliance control mapping

For the SME conversation with a compliance officer in the room, here are the specific controls zonegit answers and how:

| Framework | Control | Question the auditor asks | How zonegit answers |
|---|---|---|---|
| **SOX ITGC** | Change management + segregation of duties | *"Show me every change to DNS records pointing at the trading platform with proposer, approver, and proof the approval was followed."* | `zonegit log --signature --since=18mo --path=…` returns proposer + approver signatures per commit; the merge commit *is* the application |
| **PCI-DSS v4** | Req. 6.5.1, 10.2, 10.3, 11.5 | *"Show DNS changes that affected systems in the CDE with timestamps to the second and evidence of independent review."* | Same query; commit timestamps are second-precision; the propose/approve verb pair enforces independent review by construction |
| **HIPAA Security Rule** | §164.312(b) audit controls, §164.312(c)(1) integrity | *"Prove DNS state pointing at ePHI systems wasn't altered by an unauthorized party — and prove the audit log itself wasn't tampered with."* | Ed25519 signatures bind identity to commit; Merkle DAG makes any tampering of historical commits invalidate every descendant hash |
| **FedRAMP / NIST 800-53** | **AU-9** (audit log integrity), **AU-10** (non-repudiation), **CM-3** (change control), **CM-5** (access restrictions for change), **CM-6** (configuration baseline) | *"Prove the audit log can't be tampered with. Prove changes were approved by someone different from who made them. Show baseline drift over 90 days."* | Hash chain math for AU-9; per-commit Ed25519 signatures for AU-10; propose/approve verb separation for CM-3/CM-5; `zonegit diff baseline-2025-q1 HEAD` for CM-6 |
| **EU DORA** (2025, FinServ) | Article 9, 17, 28 — operational resilience | *"Reconstruct DNS state at the time of the most recent incident."* | `zonegit log --until=<incident-time>` + `zonegitd --at <hash>` — point-in-time-accurate read serving |
| **ISO 27001** | A.12.1.2 (change mgmt), A.12.4.1 (event logging) | Equivalent to SOX/PCI shape | Same answers as above |

The two controls zonegit answers in a way no mutable-database DNS platform can are **AU-9 (audit log tamper-evidence)** and **AU-10 (non-repudiation)**. Those two are the wedge.

---

## Appendix D — Glossary

For SMEs whose specialty is DNS but not Git, and SMEs whose specialty is Git but not DNS.

| Term | Plain-English meaning |
|---|---|
| **Blob** | One canonicalised RRset — for example, the set of A records for `api.foo.com.` packed into a deterministic byte string |
| **Tree** | A directory of labels, mapping each label to either a deeper Tree or a Blob (RRset) |
| **Commit** | A snapshot of the zone tree with parent links, author, timestamp, and (optionally) a cryptographic signature |
| **Ref / Branch / Tag** | A named pointer into the commit graph (e.g. `refs/heads/foo.com./main`) |
| **HEAD** | The currently-checked-out branch — what `zonegit set` operates on |
| **Content addressing** | Object identity = SHA-256 of contents. Two zones with the same record share storage byte-for-byte |
| **Merkle DAG** | A directed acyclic graph where every parent's hash includes every child's hash. Tampering with any node breaks every descendant |
| **RRSIG / DNSKEY / NSEC / DS** | The DNSSEC record types: signature, public key, negative-existence proof, parent-zone delegation pointer |
| **KSK / ZSK** | Key Signing Key (signs only the DNSKEY RRset) and Zone Signing Key (signs everything else) — the standard DNSSEC two-key split |
| **AXFR / IXFR** | Full zone transfer / incremental zone transfer (RFCs 5936 and 1995) |
| **Canary serving** | Routing some percentage of queries to a different branch based on a stable hash of the client subnet |
| **Time-travel daemon** | A `zonegitd` instance pinned to a historical commit, answering DNS *as the zone existed at that point in time* |
| **NIOS** | Infoblox's appliance-based DDI platform. The Grid is the cluster of NIOS appliances a customer runs |
| **NIOS-X** | Infoblox's next-generation cloud-native DDI platform |
| **DDIaaS** | Infoblox's cloud-hosted DDI offering |

---

## One-page version (for the calendar invite body)

> **zonegit — direction review**
>
> A versioned, content-addressed state layer for authoritative DNS. Replaces the mutable database underneath BIND/Knot/CoreDNS with a Merkle DAG of signed commits. Same DNS protocol on the wire; new capabilities above it: cryptographically signed audit history (FedRAMP AU-9 / AU-10, SOX ITGC, HIPAA §164.312), branch-based rollout and rollback, time-travel reads, point-in-time forensic replay.
>
> Seven public releases shipped: branches, commits, signed history, real Ed25519 DNSSEC, AXFR + IXFR, multi-zone, canary serving, CoreDNS plugin. 24-step `make demo` runs end-to-end in 90 seconds.
>
> Two milestones to "credible Infoblox product feature": pull replication (v0.8, ~2w) and a NIOS integration bridge (v0.9, ~3w). The ask: one engineer for six months.
>
> Repo: <https://github.com/ckumar392/zonegit> · Latest release: v0.7.0 · Demo: `git clone … && make demo`

---

*Generated 2026-05-27. For questions: ckumar3@infoblox.com.*
