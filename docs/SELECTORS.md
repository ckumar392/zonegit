# Selector grammar (v2 spec)

> Status: **DRAFT, locked before v2 implementation begins.**
> Once v2 ships, the surface defined here is forward-compatible: we may
> add operators, fields, and namespaces, but we will not change the
> meaning of existing ones without a major version bump.

## 1. Why this document exists

In v0/v1 the daemon serves exactly one branch (`zonegitd --branch X`).
That is the wrong shape for the headline use case (UC5: canary serving),
where an operator wants something like:

> "Send 5% of EU traffic to branch `canary`. Send everyone else to
> `main`. If the canary blows up, change one ref and the 5% snaps back."

To express that, the daemon needs a way to look at a query and decide,
*per packet*, which branch's HEAD to resolve against. That decision is
made by a **selector**: a pure boolean expression over a `QueryContext`.
A **route table** is an ordered list of `(selector → branch)` rules,
first match wins, with a default branch as the fallthrough.

The roadmap (`docs/ROADMAP.md`) explicitly calls out scope creep on
this grammar as the highest-likelihood mistake of the project. The
goal of this document is to make the smallest grammar that covers the
known use cases and reject everything else, *now*, in writing, before
code ships.

---

## 2. Worked examples (the things the grammar must express)

These are the use cases we have committed to supporting in v2. Anything
the grammar in §4 cannot express is something we will *not* implement
in v2; it is a v3+ extension.

### 2.1 Percentage rollout (the canary itself)

> "5% of all traffic to `canary`, the rest to `main`."

```yaml
# zonegitd.yaml
zone: foo.com.
default_branch: main
routes:
  - select: hash(client.subnet, "api-rollout") % 100 < 5
    branch: canary
```

`hash(client.subnet, salt) % 100` produces a stable bucket per source
subnet so a given resolver consistently sees the same branch for the
duration of the rule.

### 2.2 Geo cutover

> "Germany sees the new authority server pool; everyone else sees the
> old one."

```yaml
routes:
  - select: client.geo.country == "DE"
    branch: new-pool
default_branch: old-pool
```

### 2.3 Time-windowed change freeze override

> "On Black Friday (Nov 28 2026, all day UTC), pin everyone to
> `frozen` regardless of any other rule."

```yaml
routes:
  - select: time.utc_date == "2026-11-28"
    branch: frozen
  - select: hash(client.subnet, "x") % 100 < 5
    branch: canary
default_branch: main
```

First-match-wins: the freeze rule sits at the top, so it preempts the
canary rule.

### 2.4 ASN allow-list for an internal-only branch

> "Anything from our own ASN goes to `internal`, which has private
> records the public branch doesn't."

```yaml
routes:
  - select: client.geo.asn == 64500
    branch: internal
default_branch: main
```

### 2.5 Combined: percentage canary, but only inside one country

> "5% of US traffic to `canary`. Non-US, and the other 95% of US,
> stay on `main`."

```yaml
routes:
  - select: client.geo.country == "US" && hash(client.subnet, "us-canary") % 100 < 5
    branch: canary
default_branch: main
```

Anything beyond these five shapes is a v3+ feature request, not a v2
gap.

---

## 3. Evaluation model

A selector is a pure function

```
eval : (Expr, QueryContext) → bool
```

with the following hard rules:

1. **No I/O.** Evaluation never reads the network, the filesystem, or
   the system clock. Any time-dependent value is read from
   `QueryContext.now`, which the daemon stamps once per query.
2. **No side effects.** Evaluation cannot write logs, metrics, or
   storage. Side effects (logging the matched rule, counting matches
   per branch) are the daemon's job, layered around `eval`.
3. **Total over a well-typed expression.** Once the parser accepts an
   expression, evaluation cannot fail at runtime. Field lookups on a
   `QueryContext` that does not carry that field (e.g. no EDNS Client
   Subnet present and the rule references `client.subnet`) yield a
   typed *absence*, not an error; comparison rules in §4.4 define how
   absences propagate.
4. **Deterministic.** Same expression + same context = same result,
   forever. This is what makes `hash(...)` stable for percentage
   rollout.

These rules let us test the selector engine offline against a corpus
of `(context, expr, expected)` triples and let us cache the parsed AST
per-rule (parsing happens once at config load).

### 3.1 QueryContext

The fields available to selectors in v2 are exactly:

| Field                | Type     | Source                                                                       | Absent when                                                                   |
| -------------------- | -------- | ---------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `client.subnet`      | `cidr`   | EDNS Client Subnet (RFC 7871) if present, otherwise the source IP/32 or /128 | source IP unavailable (impossible for UDP/TCP DNS — never absent in practice) |
| `client.geo.country` | `string` | GeoIP MMDB lookup of `client.subnet`                                         | DB miss                                                                       |
| `client.geo.asn`     | `int`    | GeoIP ASN MMDB lookup                                                        | DB miss                                                                       |
| `query.name`         | `string` | lowercased FQDN of the question                                              | never                                                                         |
| `query.type`         | `string` | `dns.TypeToString[Qtype]` (e.g. `"A"`)                                       | never                                                                         |
| `query.class`        | `string` | usually `"IN"`                                                               | never                                                                         |
| `time.utc_date`      | `string` | `now.UTC().Format("2006-01-02")`                                             | never                                                                         |
| `time.utc_hour`      | `int`    | `now.UTC().Hour()`, range 0..23                                              | never                                                                         |
| `time.weekday`       | `string` | `"Mon"`..`"Sun"` (English, fixed)                                            | never                                                                         |

That is the entire v2 surface. Everything else is out of scope.

#### 3.1.1 Why so few fields

Adding a field is forward-compatible (old configs keep working). Removing
or renaming one is not. So the v2 list is deliberately the smallest set
that covers the §2 use cases. Likely v3+ additions: `client.tcp_or_udp`,
`query.do_bit`, `query.flags.cd`. We will not add them speculatively.

### 3.2 QueryContext absence semantics

If a rule references `client.geo.country` and the GeoIP lookup misses,
evaluation does **not** error. The field has the special value `null`
(typed absence) and the rules in §4.4 define the result of every
operator on `null`. The short version: any comparison with `null`
yields `false`, any boolean op treats `null` like `false`. This means
"unknown country" never matches a country-equality rule, and the rule
falls through to the default branch, which is the desired behaviour.

---

## 4. Grammar

### 4.1 Lexical

```
ident       = letter (letter | digit | "_")*
qualified   = ident ("." ident)*
int_literal = "-"? digit+
str_literal = '"' (char | escape)* '"'
              # Go-style escapes: \\ \" \n \t \xHH \uHHHH
cidr_literal = '"' ipv4_or_v6 ("/" digit+)? '"'   # parsed only inside `cidr(...)`
```

Whitespace and `#`-to-EOL comments are skipped between tokens. Strings
are UTF-8.

### 4.2 Operators (precedence, low → high)

| Level | Operators                | Associativity |
| ----- | ------------------------ | ------------- |
| 1     | `\|\|`                   | left          |
| 2     | `&&`                     | left          |
| 3     | `!` (unary)              | right         |
| 4     | `==`  `!=`               | non-assoc     |
| 5     | `<`  `<=`  `>`  `>=`     | non-assoc     |
| 6     | `in`                     | non-assoc     |
| 7     | `+`  `-`                 | left          |
| 8     | `*`  `/`  `%`            | left          |
| 9     | `()`, function call, `.` | —             |

`+`, `-`, `*`, `/`, `%` are integer-only in v2. There is no string
concatenation, no floating point, no bitwise ops.

### 4.3 Functions (the entire v2 standard library)

| Function                | Signature             | Notes                                                                                                                           |
| ----------------------- | --------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `hash(value, salt)`     | `(any, string) → int` | Stable, non-cryptographic 64-bit hash truncated to a non-negative `int`. Salt makes independent percentage cohorts independent. |
| `cidr(literal)`         | `(string) → cidr`     | Parses a CIDR literal; errors at *config load*, not at evaluation.                                                              |
| `contains(cidr, val)`   | `(cidr, cidr) → bool` | Subnet containment. `null` if either arg is `null`.                                                                             |
| `len(s)`                | `(string) → int`      | UTF-8 byte length.                                                                                                              |
| `lower(s)` / `upper(s)` | `(string) → string`   | ASCII-only case fold; non-ASCII passes through unchanged. (Avoids pulling in Unicode tables; DNS labels are ASCII-LDH anyway.)  |

That's it. No `regex`, no `now()`, no `geoip(ip, db)`, no user-defined
functions. Each addition is a v3+ proposal.

### 4.4 Type system and absence

Types: `bool`, `int`, `string`, `cidr`, plus the polymorphic absence
value `null`.

Rules:

- `null == X` is `false` for every `X`, *including* `null == null`.
  (This intentionally diverges from SQL three-valued logic to keep
  selectors total in `bool`.) Use `is_set(field)` (added in v3 if a
  use case appears) to test for presence; for v2, simply put the
  positive case at the top of the route table and let absence fall
  through.
- `null != X` is `true` iff `X` is not `null`. Symmetric with the
  above; `null != null` is `false`.
- `!null` is `true`. (`null` behaves like `false` under boolean
  operators; this falls out from §4.4.4.)
- Any arithmetic on `null` yields `null`. Comparing `null < 5` is
  `false`.
- The implicit conversion of `null` to `bool` is `false`. So
  `client.geo.country == "DE" || time.utc_hour == 3` works the way
  you'd expect when GeoIP is missing.

### 4.5 Operator semantics

- `==` / `!=`: same-type only. Mismatched types are a *parse-time*
  error. `cidr == cidr` is bytewise; `string == string` is bytewise
  (case-sensitive — use `lower(...)` to fold).
- `<` / `<=` / `>` / `>=`: defined for `int` only.
- `in`: `string in [string, ...]`, `int in [int, ...]`, or
  `cidr in [cidr, ...]`. List literals use `[a, b, c]` syntax. Lists
  are not first-class values; they only appear on the right of `in`.
- `+`, `-`, `*`, `/`, `%`: `int × int → int`. Division by zero is a
  *parse-time* error if both sides are constants; otherwise it's a
  runtime `null` (which is then `false` in any boolean position, so
  the rule simply doesn't match). This matters for expressions like
  `len(query.name) % bucket_count`.

### 4.6 EBNF

```
expr        = or_expr .
or_expr     = and_expr { "||" and_expr } .
and_expr    = not_expr { "&&" not_expr } .
not_expr    = [ "!" ] cmp_expr .
cmp_expr    = sum_expr [ ( "==" | "!=" | "<" | "<=" | ">" | ">=" ) sum_expr ]
            | sum_expr "in" list_literal .
sum_expr    = term { ( "+" | "-" ) term } .
term        = factor { ( "*" | "/" | "%" ) factor } .
factor      = literal
            | qualified                       # field or 0-ary function
            | qualified "(" [ expr { "," expr } ] ")"
            | "(" expr ")" .
literal     = int_literal | str_literal | "null" | "true" | "false" .
list_literal = "[" [ literal { "," literal } ] "]" .
```

---

## 5. Configuration shape

YAML at the daemon level. The exact field names below are part of the
v2 stability surface.

```yaml
# zonegitd.yaml
zone: foo.com.

# Fallback branch when no rule matches. Required.
default_branch: main

# Optional list of rules, evaluated top-to-bottom, first match wins.
routes:
  - name: black-friday-freeze
    select: time.utc_date == "2026-11-28"
    branch: frozen
  - name: us-canary
    select: client.geo.country == "US" && hash(client.subnet, "us-canary") % 100 < 5
    branch: canary
  # If you want a true catch-all rule (e.g. for metrics), make it explicit:
  - name: catchall-main
    select: true
    branch: main

# Optional GeoIP DB paths. If absent, GeoIP fields evaluate to null.
geoip:
  country_db: /var/lib/geoip/GeoLite2-Country.mmdb
  asn_db:     /var/lib/geoip/GeoLite2-ASN.mmdb
```

### 5.1 Loading and validation

At startup the daemon:

1. Parses `routes[*].select` into ASTs. Any parse error fails
   startup; the daemon refuses to bind a port with an unparseable
   config. (No "best-effort, log a warning" — that's how you ship a
   broken canary.)
2. Resolves every `branch` field against the repo. Refusing-to-start
   on a missing branch is intentional; selectors that target a branch
   that doesn't exist would silently fall through, which is the wrong
   default.
3. Opens GeoIP DBs if configured; missing files fail startup.

`SIGHUP` reloads the config (re-parses, re-resolves, swaps atomically).
Any failure leaves the running config in place.

### 5.2 Hot-reload of branch HEADs

Independent of config reload. Branch tip changes are picked up on the
next query because the daemon already re-resolves
`refs/heads/<branch>` per-packet (v0/v1 mechanism). Selector
evaluation does not change that loop.

---

## 6. Metrics surface

Each rule contributes one labelled counter:

```
zonegitd_route_match_total{rule="us-canary",branch="canary"}
zonegitd_route_match_total{rule="(default)",branch="main"}
```

Plus a histogram of selector evaluation latency
(`zonegitd_select_eval_seconds`) so we can catch a poorly-written rule
that scans something it shouldn't.

These metric names are part of the v2 stability surface.

---

## 7. Anti-goals (do NOT do these in v2)

These are the things we explicitly won't add to the grammar in v2,
listed here so the temptation is on the record:

- **Regex.** Tempting for `query.name`. Adds a re2 dependency, an
  attack surface, and a cost story. If a rule needs "match all
  `*.api.foo.com.`", the right shape is a separate `query.suffix`
  field added in v3.
- **Floating point.** Percentages are integers (`< 5`, not `< 0.05`);
  hash buckets are integers. No motivating use case for floats and
  every motivating use case for *not* having them (precision bugs,
  NaN semantics).
- **User-defined functions / let-bindings.** Selectors stay
  expression-only.
- **Cross-rule references.** No `routes[0].matched`. Each rule is a
  pure function of `QueryContext`.
- **String concatenation.** Use a richer field (e.g. add
  `client.geo.country_lower`) before adding `+` over strings.
- **Mutable state.** No counters readable from selectors.
  `hash(client.subnet, salt) % N` is the canonical "spread traffic"
  primitive.

If a customer needs one of these, we add the smallest possible field
or function in v3+, behind an explicit version bump of this spec.

---

## 8. Open questions to resolve before writing code

1. **Hash function choice.** Candidates: xxh3-64, FNV-1a, FarmHash.
   Constraint: deterministic across platforms and Go versions. xxh3
   is the leading candidate (already vendored as a transitive dep
   via Badger via cespare/xxhash).
2. **GeoIP DB ownership.** Do we vendor a tiny test DB for CI, or
   gate GeoIP tests behind `-tags=geoip`? Probably the latter.
3. **EDNS Client Subnet trust.** Do we trust ECS from any resolver,
   or only from a configured allowlist? ECS spoofing trivially
   defeats `client.geo.country` rules. Default in v2: trust ECS, with
   a `geoip.trust_ecs: false` opt-out that falls back to source IP.
4. **Metrics cardinality.** Bounded by `len(routes) + 1`, which is
   fine. But if rules are auto-generated in the future this could
   blow up; add a documented soft limit.
5. **Selector vs. branch existence at config load.** What if the
   branch exists at startup but is deleted at runtime? Per-query
   resolve will fail; the daemon should fall through to
   `default_branch` and emit a metric, not SERVFAIL.

These are the only questions blocking the start of code. Once §1–§7
are agreed and §8 has a one-line answer to each, v2 implementation
proceeds.

---

## 9. Implementation plan (informational)

This is not part of the spec; it's how we expect to land it.

1. New package `pkg/select` with: AST, parser, evaluator, an
   `Evaluator` interface that takes a `QueryContext` and returns
   `bool`. Pure, no DNS or repo deps.
2. New package `pkg/geo` with a thin wrapper around
   `github.com/oschwald/maxminddb-golang` (added as a vendored dep).
   Exposes `Lookup(ip) → (country, asn, ok)`.
3. New `pkg/route` glues §5's YAML into a `[]CompiledRule`. One
   parse, many evaluations.
4. `cmd/zonegitd` gains a `--config` flag accepting the YAML in §5.
   The existing `--branch` flag becomes a shorthand for a
   single-rule config (`select: true`).
5. A soak test under `bench/` that pushes 10k qps of mixed traffic
   and asserts p99 < 1ms, no memory growth over 5 min, and bucket
   distribution within ±1% of the configured percentage.

Each of those is its own PR; none of them changes the `pkg/repo` or
`pkg/object` API surface.
