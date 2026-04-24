DNS has no `git`. I built one.

This is a working prototype, not a finished product. The point of the post is to show that a long-standing gap in how DNS handles its own state is real, and that closing it is more tractable than it looks.

---

## The 2 a.m. question

Pick any on-call channel. Wait long enough. Eventually someone types:

> "What did `api.checkout.acme.com` point to ten minutes ago?"

There is no good answer. You grep five log files. You SSH into the secondary. You hope someone's terminal scrollback is still alive. You make a guess, ship the rollback, and write a postmortem with the words "lessons learned" in it for the fourth time this quarter.

We have had a working model for this since 2005. Linus called it `git`. Twenty-one years later the most critical name-resolution layer on the internet still treats its state as a file you overwrite. No history. No diff. No blame. No rollback. No preview.

That gap is what this project is about.

---

## What I built

The project is two binaries written in Go, race-detector clean, and small enough to read end-to-end in an afternoon.

**`dnsdb`** is a CLI that looks and feels like git, but the verbs operate on DNS records:

```bash
dnsdb init foo.com.
dnsdb import zone.txt -m "initial import"
dnsdb set api.foo.com. A 60 9.9.9.9 -m "scale to new endpoint"
dnsdb log
dnsdb diff HEAD~1 HEAD
dnsdb blame api.foo.com. A
dnsdb show api.foo.com. A HEAD~3
```

That last command, *"what did this name resolve to three commits ago?"*, is not something you can ask any DNS tool shipping today, whether that is BIND, Knot, PowerDNS, or Route 53. Some of them keep a change log in one shape or another. None of them keep the queryable historical state.

**`dnsdbd`** is a reference authoritative DNS server. It speaks UDP and TCP, sets the AA bit, distinguishes NXDOMAIN from NODATA correctly, returns SOA in the authority section, and answers `dig` in well under a millisecond by walking a Merkle tree at HEAD.

`dnsdbd` is not trying to replace BIND or Knot or NSD. Those are decades-hardened pieces of infrastructure and ripping them out is nobody's day-one move. The intended deployment shape is **dnsdb as the control plane, your existing authoritative server as a downstream secondary** over standard AXFR/IXFR. The reference server exists so the demo is honest end-to-end and so greenfield deployments have a path. It is not the adoption vector.

Together they form a versioned, content-addressed, branch-aware control plane for authoritative DNS, running on my laptop today.

---

## The shape of the idea

A DNS zone is a tree of names. An RRset (one `(name, type)` coordinate, e.g. `api.foo.com. A`) is a leaf.

In other words, it is a filesystem, with somewhat unusual path separators (`.` instead of `/`, read right-to-left).

We know how to version a filesystem. The recipe fits on a napkin:

1. Hash the contents of every leaf. That is a **blob**.
2. Hash a sorted list of `(name, child-hash)` pairs. That is a **tree**.
3. Hash a `(tree, parent, author, message, time)` tuple. That is a **commit**.
4. Branches are mutable pointers to commits. HEAD is a pointer to a branch.

Equal subtrees get the same hash. A billion-record zone with one A-record change writes a few dozen new nodes, not a billion. Diffing two commits is O(changed-subtrees), not O(zone-size). Time travel is free; every commit is already a complete, immutable snapshot.

I knew all of this in theory. The interesting question was whether the model fits when you bolt it onto DNS.

It fits well enough that the surprises were on the upside.

---

## The moment it stopped being theory

Two terminals, side by side. Try it yourself.

**Left:**

```bash
while true; do
  printf "%s -> " "$(date +%T)"
  dig +short @127.0.0.1 -p 15353 api.foo.com. A
  sleep 1
done
```

```plaintext
14:23:01 -> 1.2.3.4
14:23:02 -> 1.2.3.4
14:23:03 -> 1.2.3.4
```

**Right:**

```bash
$ dnsdb set api.foo.com. A 60 9.9.9.9 -m "fail over to new region"
[main 99f98c225a8a] fail over to new region
```

**Left, on the very next tick:**

```plaintext
14:23:04 -> 9.9.9.9
14:23:05 -> 9.9.9.9
```

No SIGHUP, no zone reload, no SOA-serial bump, no 30-second wait for a secondary to catch up. A commit lands and the server sees the new HEAD on the next packet it answers.

Then:

```bash
$ dnsdb blame api.foo.com. A
99f98c225a8a    ckumar3 <ckumar3@host>    fail over to new region

$ dnsdb diff HEAD~1 HEAD
~ api A
```

Who changed it, what changed, and when, each available as a single command.

One disclaimer, because experienced DNS operators will rightly flinch at "instant": the point of this demo is not speed. TTLs and downstream caches still exist for excellent reasons and dnsdb does not pretend otherwise. The point is that propagation becomes an explicit, auditable, revertible decision instead of an emergent property of reload scripts and SOA-serial bumps. You stage a change on a branch, diff it against main, cut over with a commit, and revert with a `reset` if it goes wrong. Speed is a side effect; control is the feature.

> **Want to see this yourself in two minutes?**
> ```bash
> git clone https://github.com/dnsdb && cd dnsdb && ./scripts/demo.sh
> ```
> Two terminals, one `dig` loop, one `dnsdb set`. The answer changes mid-loop. The rest of the post is what is happening underneath.

---

## The part that surprised me

I expected the DNS server to be the hard bit. It wasn't. The whole serving path (UDP, TCP, NXDOMAIN/NODATA, SOA-in-authority, CNAME chasing) came out small enough that it stopped feeling like the project. The interesting code lives where Git's interesting code lives: canonical encoding, structural-sharing tree updates, and the lockstep diff with the "subtree hashes match, skip the whole subtree" optimization that makes Git diff a 100,000-file repository in 40 ms.

Three lessons from the build.

**Canonical encoding is the entire game.** For "equal RRsets produce equal hashes" to hold, you have to be ruthless. Owner names lowercased. Records inside an RRset sorted by their wire-format rdata (the canonical form DNSSEC standardized in RFC 4034). Class normalized. TTL folded into the content hash, which is a deliberate design choice rather than an RFC requirement: changing a TTL is an audit-worthy event that deserves its own commit. Get any of this wrong and dedup does not dedup, structural sharing does not share, and the whole illusion collapses.

**The apex breaks the filesystem analogy.** A zone like `foo.com.` itself has records (SOA, NS, MX), so the "root directory" is also leaf-bearing. I tried the obvious thing first: a parallel apex blob hanging off the tree object, separate from the children. It worked, but every traversal then had two cases ("check apex blob, then walk children") and every diff had to special-case the apex on both sides. The fix was a literal `@` sentinel as a child name, the same trick zonefile syntax has used for forty years. The apex becomes just another leaf, traversal stays uniform, diff stays uniform, and the special case disappears from the code entirely.

**Read-mostly DNS plus content-addressed storage equals lockless serving.** The server opens the database read-only, walks an immutable tree at an immutable hash, returns bytes. The writer can be in the middle of a five-second commit and it cannot affect the read path. Half-written zones do not exist as commits. It is the same property that lets you `git log` while you `git commit`. The DNS read path inherits it for free.

---

## What this unlocks

A versioned, content-addressed, branch-aware DNS state store does not just close the audit-log gap. It makes a class of things trivial that are currently impossible.

**Safe change preview.** Make the change on a branch. Run synthetic queries against that branch. Diff against main. Merge if green. The model is so familiar to anyone who has shipped code that it requires zero training.

**Canary DNS.** Send 5% of queries to `branch-experimental`, the rest to `main`. Watch the dashboards. Promote or roll back at one-commit granularity. Today this requires standing up a parallel resolver fleet. With a versioned core, it is a routing decision.

**Forensic-grade audit.** "Show me the exact state of this zone at 14:23:04 UTC last Tuesday" is one command, answered from immutable history rather than reconstructed from log scraping.

**True GitOps for DNS.** Every modern infra team is moving to declarative state in Git. DNS has been the awkward stepchild because the runtime knows nothing about Git. This makes the runtime itself a Git repo. The pipeline does not push files; it pushes commits.

**Replication with cryptographic integrity.** Content-addressed objects are tamper-evident by construction. Two replicas agree on a commit hash, they have byte-for-byte identical state. No "hopefully the AXFR completed" prayers.

I did not build all of this. I built the foundation that reduces each of them to a well-scoped engineering task instead of a research problem.

---

## "But isn't this already solved?"

Fair pushback, worth answering directly. Partial answers exist.

- **Route 53 + CloudTrail** gives you a change log, but it is an audit feed bolted next to the system, not history baked into the system. You cannot `show api.foo.com. A HEAD~3` against it.
- **PowerDNS on a SQL backend** gives you rows you can query, but no commit graph, no branches, no content-addressed snapshots, no diff-by-subtree.
- **BIND + zonefiles in Git + an scp pipeline** is the closest cultural match, and millions of zones are run that way today. It works. It is also a workflow wrapped around a stateless engine that knows nothing about the Git repo it came from. The runtime cannot tell you what it served at 14:23:04 last Tuesday. Only the repo can, and only if the repo and the runtime never drifted.

So this is not the first time anyone has thought of versioning DNS. It is the first time, that I know of, that the runtime itself is the version-controlled artifact instead of a workflow bolted around a stateless engine. That is a meaningfully different claim, and it is the one that makes branch-aware serving, byte-for-byte replica equivalence, and forensic time-travel queries fall out of the model rather than be chased after the fact.

---

## The honest part

This is not production. What works today is a single zone on a single host, with branches in the CLI and a daemon that serves only `main`. The interesting work that is genuinely still ahead, in roughly the order it should happen:

- **Branch-aware serving and canary cutover.** Branches exist in the storage; the daemon needs to learn how to route a fraction of queries to one branch and the rest to another. Small, well-scoped, and the next thing on the bench.
- **Speak the standard wire protocol outward.** AXFR and IXFR out, so `dnsdbd` can be a primary that feeds existing BIND/Knot/NSD secondaries. Each commit becomes an SOA serial bump; IXFR is the diff between two commits, which is exactly what the storage already computes.
- **Shadow mode and migration, together.** Run as an AXFR secondary of your existing authoritative server, commit every transferred state (you instantly have a `git log` for a system you already trust), and serve a sampled fraction of real query traffic in parallel. A continuous-diff process compares answers byte for byte. When the diff has been zero across enough zones for enough days, the cutover stops being a leap. This is also the migration story: no flag-day, no rip-and-replace, just a quietly accumulating commit log next to production until it has earned a turn at the front.
- **Three-way merge with DNS-aware semantics.** Git diffs lines; DNS records are not lines. An RRset is a set of records sharing a single TTL and class. "Both branches added an A record to `api.foo.com.`": does that union, conflict, prefer the later commit? Each RR type plausibly wants different semantics. This is a design problem, not a coding problem.
- **Distributed consistency across writers and regions.** Single-writer CP via Raft is well-trodden ground and the obvious first stop. Multi-writer with conflict-free convergence is genuinely harder and at least partly research territory for this domain. The honest plan is to start with single-writer plus read replicas and earn the right to anything more ambitious.
- **DNSSEC stays downstream.** The store holds unsigned authoritative state; your existing signer signs on the way out. Online signing as a commit hook is interesting but is a separate concept and should be treated as one.

What exists today is a complete, end-to-end, working v0 with a clear runway. Storage is pluggable (BadgerDB today, Postgres or S3 next). Serving is pluggable. Nothing in the design painted itself into a corner.

What exists today works end-to-end and is clean under the race detector. It is also not yet a thing you would put in front of `acme.com.` on a Tuesday, and the post would be dishonest if it pretended otherwise.

---

## The question that decides everything

A reviewer I respect put the only question that matters into one sentence:

> *"Why would a production team trust this with their authoritative DNS?"*

The honest answer today is: they would not, and they should not. Race-detector-clean correctness and an elegant model are not the same thing as an operational track record. Pretending otherwise would be the worst possible move.

The interesting part of the question is whether there is a *path* to trust that does not require a leap of faith. The answer is shadow mode, described in the roadmap above, which earns trust the only way infrastructure ever earns trust: by agreeing with what is already trusted, in front of real traffic, for long enough that disagreement would have surfaced.

What is proven so far is *"this should exist."* What is left to prove is *"this is safer than what you are already running."* I know which one is harder.

---

## Try it

```bash
git clone https://github.com/dnsdb
cd dnsdb
./scripts/demo.sh
```

Two terminals. One running `while true; do dig …; done`. One running `dnsdb set`. Watch the answer change mid-loop.

If the small voice in the back of your head says *"wait, that should have been a thing already"*: yes. That is the project.

---

*If you build infrastructure for a living, fork it, break it, tell me what is wrong. If you operate infrastructure for a living, run the demo and ask yourself which other system in your stack (firewall rules, RBAC, route policies, feature flags) has been quietly missing a `git log` this whole time.*

*Code: [github.com/dnsdb](https://github.com/dnsdb) · Apache 2.0.*
