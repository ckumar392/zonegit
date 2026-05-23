# CoreDNS plugin: zonegit

Serve authoritative DNS from a zonegit repository as a [CoreDNS](https://coredns.io/)
plugin. All of `pkg/resolve.Resolver`'s features (CNAME chase, AXFR,
IXFR, DNSSEC-OK RRSIG attachment, canary routing, time-travel) carry
across unchanged — the plugin is a thin wrapper.

## Why a separate Go module

CoreDNS's dependency tree is large (hundreds of transitive modules
including Prometheus, etcd, Kubernetes client libs, etc.). Vendoring it
into the main zonegit module would explode the vendor directory and
slow down every build. Keeping the plugin in its own module means:

- The main `zonegit` and `zonegitd` binaries stay lean.
- The plugin can be built only when needed.
- A future operator can pin a different CoreDNS version without
  touching the main repo.

## Corefile syntax

```
foo.com. {
    zonegit /path/to/zonegit-repo {
        branch main
        canary canary:20
        canary-salt api-rollout
        at HEAD~5
    }
    log
    errors
}
```

| Directive | Required | Default | Meaning |
|-----------|----------|---------|---------|
| `<repo>`  | yes      | —       | Filesystem path to a zonegit Badger repo. |
| `branch`  | no       | `main`  | Branch to serve by default. |
| `canary`  | no       | (off)   | `<branch>:<pct>`. Splits traffic by stable hash of client /24. |
| `canary-salt` | no   | `zonegit` | Salt for the canary bucket hash. |
| `at`      | no       | (off)   | Pin to a historical commit (ref-ish). |

Exactly one zone per `zonegit` block.

## Building

```sh
# from the repository root:
cd cmd/coredns-with-zonegit
go build -o ../../bin/coredns
```

Or via the top-level Makefile:

```sh
make coredns
```

This builds a custom CoreDNS binary at `./bin/coredns` with the zonegit
plugin compiled in. The build pulls in the full CoreDNS dependency
tree on first run (~5 minutes the first time; subsequent builds use
the Go module cache).

## Running

```sh
$ cat /tmp/Corefile <<EOF
foo.com. {
    zonegit /tmp/zonegit-demo
    log
    errors
}
EOF

$ ./bin/coredns -conf /tmp/Corefile -dns.port 5353

# Then in another terminal:
$ dig @127.0.0.1 -p 5353 api.foo.com. A
```

## Limitations

- The Corefile block must specify exactly one zone (the standard
  CoreDNS pattern). To serve multiple zones, use multiple `zonegit`
  blocks against the same repo path — each gets its own `Resolver`.
- `SIGHUP` reloads the Corefile through CoreDNS itself; zonegit's
  internal SIGHUP-driven config reload is bypassed in this mode.
- The plugin uses CoreDNS's standard plugin lifecycle; per-query
  metrics are CoreDNS's `coredns_*_metrics`, not zonegit's
  `zonegit_*`. Wire up zonegit's `resolve.Metrics` via Corefile in
  v0.8 if needed.
