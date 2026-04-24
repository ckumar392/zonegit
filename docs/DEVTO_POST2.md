---
title: "What if your authoritative DNS server *was* a Git repository?"
published: false
description: A thought experiment, and a reference implementation, for treating DNS zone state as a versioned, content-addressed history instead of a file you overwrite.
tags: dns, go, infrastructure, distributedsystems
# cover_image: https://direct-url-to-an-image.jpg
# canonical_url:
---

*A thought experiment about applying a forty-year-old idea (version control) to a forty-year-old system (DNS) that somehow never got it.*

---

## The 2 a.m. question

Pick any on-call channel. Wait long enough. Eventually someone types:

> "What did `api.checkout.acme.com` point to ten minutes ago?"

There is no good answer. You grep five log files. You SSH into the secondary. You hope someone's terminal scrollback is still alive. You make a guess, ship the rollback, and write a postmortem with the words "lessons learned" in it for the fourth time this quarter.

We solved this for source code in 2005. Linus called it `git`. Twenty-one years later the most critical name-resolution layer on the internet still treats its state as a file you overwrite. No history. No diff. No blame. No rollback. No preview.

This post is about what happens if you remove that asymmetry. Not as a product pitch, but as a thought experiment carried far enough to compile and answer `dig` queries.

---

## The shape of the idea

A DNS zone is a tree of names. An RRset (one `(name, type)` coordinate, e.g. `api.foo.com. A`) is a leaf.

That is a filesystem. A filesystem with weird path separators (`.` instead of `/`, read right-to-left), but a filesystem.

We know how to version a filesystem. The recipe fits on a napkin:

1. Hash the contents of every leaf. That is a **blob**.
2. Hash a sorted list of `(name, child-hash)` pairs. That is a **tree**.
3. Hash a `(tree, parent, author, message, time)` tuple. That is a **commit**.
4. Branches are mutable pointers to commits. HEAD is a pointer to a branch.

Equal subtrees get the same hash. A billion-record zone with one A-record change writes a few dozen new nodes, not a billion. Diffing two commits is O(changed-subtrees), not O(zone-size). Time travel is free; every commit is already a complete, immutable snapshot.

That is the whole concept. The interesting question is whether it actually fits when you bolt it onto DNS, or whether the analogy leaks the moment you touch real RRsets, real apex records, real concurrency. The only honest way to find out is to build it.

---

## The concept, made concrete

To pressure-test the idea I wrote two small programs. They exist to prove the model is feasible end to end, not as a product you should adopt.

The first is a CLI shaped like git but with verbs that operate on DNS records:

```bash
zonegit init foo.com.
zonegit import zone.txt -m "initial import"
zonegit set api.foo.com. A 60 9.9.9.9 -m "scale to new endpoint"
zonegit log
zonegit diff HEAD~1 HEAD
zonegit blame api.foo.com. A
zonegit show api.foo.com. A HEAD~3
```

That last command, *"what did this name resolve to three commits ago?"*, is not something you can ask any DNS tool shipping today. Not BIND. Not Knot. Not PowerDNS. Not Route 53. The change log exists in some of those, in various shapes. The queryable historical state does not.

The second is a small authoritative DNS server that walks the Merkle tree at HEAD on every query. UDP and TCP. AA bit set. RFC-correct NXDOMAIN vs NODATA. SOA in the authority section. Sub-millisecond on a laptop.

Together they demonstrate the concept is reachable from current Go and current DNS libraries by one person in a long weekend. That is the whole point of writing them.

---

## The moment the concept stopped feeling theoretical

Two terminals, side by side.

**Left:**

```bash
while true; do
  printf "%s -> " "$(date +%T)"
  dig +short @127.0.0.1 -p 15353 api.foo.com. A
  sleep 1
done
```

```
14:23:01 -> 1.2.3.4
14:23:02 -> 1.2.3.4
14:23:03 -> 1.2.3.4
```

**Right:**

```bash
$ zonegit set api.foo.com. A 60 9.9.9.9 -m "fail over to new region"
[main 99f98c225a8a] fail over to new region
```

**Left, on the very next tick:**

```
14:23:04 -> 9.9.9.9
14:23:05 -> 9.9.9.9
```

No SIGHUP. No zone reload. No bumping the SOA serial like it's 1987. A commit, and the next query reads the new HEAD.

Then:

```bash
$ zonegit blame api.foo.com. A
99f98c225a8a    ckumar3 <ckumar3@host>    fail over to new region

$ zonegit diff HEAD~1 HEAD
~ api A
```

Who. What. When. One command each.

A note for DNS operators who will rightly flinch at "instant": the demonstration is not about speed. TTLs and downstream caches still exist for excellent reasons. The point is that propagation becomes an explicit, auditable, revertible decision instead of an emergent property of reload scripts and SOA-serial bumps. You stage on a branch. You diff. You cut over with a commit. You revert with a `reset`. Speed is a side effect; *control* is the property the model produces.

---

## What you learn when you actually build it

I expected the DNS server to be the hard part. I had braced myself for an RFC-1035 conformance war. It did not happen. The whole serving path (UDP, TCP, NXDOMAIN/NODATA, SOA-in-authority, CNAME chasing) came out small enough that it stopped feeling like the project.

The interesting code lives where Git's interesting code lives: the storage layer, the canonical encoding, the structural-sharing tree update, and the lockstep diff with the "subtree hashes match, skip the whole subtree" optimization that lets Git diff a 100,000-file repository in 40 ms.

Three observations from the build:

**Canonical encoding is the entire game.** For "equal RRsets produce equal hashes" to hold, you have to be ruthless. Owner names lowercased. Records inside an RRset sorted by their wire-format rdata (the same canonical form DNSSEC standardized in RFC 4034). Class normalized. TTL folded into the content hash, which is a deliberate design choice rather than an RFC requirement: changing a TTL is an audit-worthy event that deserves its own commit. Get any of this wrong and dedup does not dedup, structural sharing does not share, the diff lights up on every commit, and the whole concept collapses.

**The apex is the only place the filesystem analogy breaks.** A zone like `foo.com.` itself has records: SOA, NS, MX. So the "root directory" is also a leaf-bearing node. The fix is a literal `@` sentinel, the same trick zonefile syntax has been using for forty years. The old answer is the right answer.

**Read-mostly DNS plus content-addressed storage equals remarkably clean concurrency.** The serving path takes no locks. It opens the database read-only, walks an immutable tree at an immutable hash, returns bytes. The writer can be in the middle of a five-second commit and it cannot affect the read path. You cannot observe a half-written zone, because half-written zones do not exist as commits. It is the same property that lets you `git log` in one terminal while you `git commit` in another. The DNS read path inherits it for free.

None of these were obvious in advance. All three are properties of *the model*, not of any particular implementation. That is the encouraging signal.

---

## What the model unlocks

If DNS state is versioned, content-addressed, and branch-aware, several things that are currently impossible become trivial.

**Safe change preview.** Make the change on a branch. Run synthetic queries against that branch. Diff against main. Merge if green.

**Canary DNS.** Send 5% of queries to `branch-experimental`, the rest to `main`. Promote or roll back at one-commit granularity. Today this requires a parallel resolver fleet. Under this model it is a routing decision.

**Forensic-grade audit.** "Show me the exact state of this zone at 14:23:04 UTC last Tuesday" is one command, answered from immutable history rather than reconstructed from log scraping.

**GitOps for DNS that actually closes the loop.** Every infra team is moving toward declarative state in Git. DNS has been the awkward stepchild because the runtime knows nothing about Git. Under this model, the runtime *is* the repo. The pipeline does not push files; it pushes commits.

**Replication with cryptographic integrity.** Content-addressed objects are tamper-evident by construction. Two replicas agree on a commit hash, they have byte-for-byte identical state. No "hopefully the AXFR completed" prayers.

These are not features added on top of the model. They fall out of it.

---

## "But isn't this already solved?"

Fair pushback, worth answering directly. Partial answers exist.

- **Route 53 + CloudTrail** gives you a change log, but it is an audit feed bolted next to the system, not history baked into the system. You cannot `show api.foo.com. A HEAD~3` against it.
- **PowerDNS on a SQL backend** gives you rows you can query, but no commit graph, no branches, no content-addressed snapshots, no diff-by-subtree.
- **BIND + zonefiles in Git + an scp pipeline** is the closest cultural match, and millions of zones run that way today. It works. It is also a workflow wrapped around a stateless engine that knows nothing about the Git repo it came from. The runtime cannot tell you what it served at 14:23:04 last Tuesday. Only the repo can, and only if the repo and the runtime never drifted.

So the concept here is not "first time anyone thought of versioning DNS." It is "what changes when the runtime itself is the version-controlled artifact, instead of a workflow bolted around a stateless engine?" That is a different question. It is the question that makes branch-aware serving, byte-for-byte replica equivalence, and forensic time-travel queries fall out of the model rather than be chased after the fact.

Better model. Not first model.

---

## What the model still has to answer

The fact that the concept compiles and serves `dig` does not mean it is finished as an idea. The serious open questions are not in the storage layer; they are at the edges where DNS-the-protocol and operations-the-discipline live.

**Merge semantics for DNS records.** Git diffs lines. DNS records are not lines. An RRset is a set of records sharing a single TTL and class. "Both branches added an A record to `api.foo.com.`": does that union, conflict, prefer the later commit? Each RR type plausibly wants different semantics. This is a design problem, not a coding problem, and it is unsolved.

**Distributed consistency across writers and regions.** A single-writer model with read replicas backed by Raft is well-trodden ground and cleanly fits the commit graph. Multi-writer with conflict-free convergence is harder and at least partly a research problem in this domain. It deserves a careful answer rather than a hand-wave.

**DNSSEC.** The cleanest framing keeps signing downstream: the versioned store holds the unsigned authoritative state, an existing signer signs on the way out, the signature is not part of the canonical content hash. Online signing as a commit hook is interesting but is a separate concept and should be treated as one.

**Operational migration.** None of this matters if there is no plausible path from a real BIND or Route 53 deployment to running it. The honest answer is shadow mode (next section), not "rip and replace."

**Trust.** The deepest open question, and the only one that decides whether any of this ever becomes real. Treated separately below because it deserves it.

---

## How would anyone ever trust this?

A reviewer put the only question that matters into one sentence:

> *"Why would a production team trust this with their authoritative DNS?"*

The honest answer today is: they would not, and they should not. Race-detector-clean correctness and an elegant model are not the same thing as an operational track record.

The interesting part of the question is whether there is a *path* to trust that does not require a leap of faith. There is, and it is implied by the model itself.

1. Run the versioned store as an AXFR secondary of the existing authoritative server.
2. Every transferred zone state becomes a commit. You instantly have a `git log` for a system you already trust.
3. Have it serve a small, sampled fraction of real query traffic in parallel.
4. Continuously diff its answers against the production server's answers, byte for byte, on every sampled query.
5. When the diff has been zero across enough zones for enough days, the cutover stops being a leap and becomes a rounding error.

This is "shadow mode," and it is the only honest path from a new idea to a trusted piece of authoritative infrastructure. It earns trust the only way infrastructure ever earns trust: by agreeing with the system that is already trusted, in front of real traffic, for long enough that disagreement would have surfaced.

What the concept proves so far is *"this should exist."* What is left to prove is *"this is safer than what you are already running."* The second is harder, and it is where most of the remaining work lives.

---

## Why this is worth thinking about beyond DNS

DNS is not the only piece of infrastructure that has dodged version control. Firewall rules. BGP route policies. Kubernetes RBAC. IAM policies. Feature flags. Service-mesh configs. Rate-limit tables. All critical. All mutating. None of them have a `blame`.

Each one has someone, somewhere, trying to bolt Git on after the fact: GitOps for K8s, Terraform-in-CI for cloud, BIND zonefiles in a repo with an scp pipeline. It works. It is also a permanent cognitive tax on every operator who has ever had to mentally reconcile "what is running" with "what we wish was running."

The pattern repeats because the underlying engines all made the same mistake. They treated mutable state as the primary representation, and history as something you might layer on later if you cared. Forty years on, the lesson from source control is hard to dispute: history is not optional. History is the floor.

The shape of the answer (immutable, content-addressed, branch-aware state with the runtime *as* the repo) plausibly applies to several of those layers. DNS is just the cleanest place to test the idea, because zones are already tree-shaped, already small, already read-mostly. Whether the same model survives contact with firewall rules or RBAC is an interesting question for someone, possibly me, possibly you.

---

## A reference implementation

If you want to read the code or run the demo:

```bash
git clone https://github.com/ckumar392/zonegit
cd zonegit
./scripts/demo.sh
```

Two terminals. One running `while true; do dig …; done`. One running `zonegit set`. The answer changes mid-loop. That is the whole demonstration.

The code is there to make the concept argue for itself. It is not a product, an adoption pitch, or a roadmap. It is what the idea looks like once you stop hand-waving and let a compiler tell you which parts of the analogy survive.

---

*Pushback, holes, "you are wrong because…": all welcome. The point of writing this down is to find out which parts of the concept survive the next reader who knows more than I do.*

*Code: [github.com/ckumar392/zonegit](https://github.com/ckumar392/zonegit) · Apache 2.0.*
