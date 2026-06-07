# zonegit as a version-control front-end for BIND

Use `zonegit` for what nothing else gives you — `log` / `diff` / `blame` /
`propose` / `approve` on your zone — while a battle-tested **BIND** keeps
doing the actual serving. zonegit is never in the query path, so a pre-1.0
on-disk format change costs you a re-import, not a DNS outage.

```
   you ── git-style edits ──▶  zonegit         (authoring + history, on disk)
                                  │
                                  │  zonegitd answers AXFR on 127.0.0.1:5354
                                  ▼
                               BIND named       (slaves the zone, serves :1053)
                                  ▲
   dig / resolvers ──────────────┘
```

The one seam between the two is a standard **AXFR zone transfer**. zonegit
auto-bumps the apex SOA serial on every commit, so BIND's ordinary
"serial went up → re-transfer" logic just works.

## Prerequisites

- `brew install bind` (provides `named`, `rndc`, `tsig-keygen`)
- The zonegit binaries (`up.sh` runs `make build` for you if they're missing)

## Quick start

```sh
cd examples/bind-frontend
./up.sh                                          # build, seed, start both daemons
dig @127.0.0.1 -p 1053 api.lab.internal. +short  # 10.0.0.10  (served by BIND)

./publish.sh set api.lab.internal. A 300 10.9.9.9 -m "failover api"
dig @127.0.0.1 -p 1053 api.lab.internal. +short  # 10.9.9.9   (BIND picked it up)

./down.sh                                        # stop both daemons
```

Everything lives under `run/` (git-ignored): the zonegit repo, BIND's
working dir, the slaved zone file, the rndc key, logs, and pid files.

## The two editing workflows

**Direct edit** — fastest; commits straight to `main`. (All commands below
assume you're in `examples/bind-frontend`, where the quickstart left you.)

```sh
./publish.sh set    grafana.lab.internal. A 300 10.0.0.30 -m "add grafana"
./publish.sh delete grafana.lab.internal. A
```

**PR-style** — propose on a branch, diff it, approve, then push. Drive
`zonegit` directly and **do not pass `--zone`** mid-flow (passing `--zone`
resets HEAD to the zone's current branch, which would yank you off the
proposal branch). Then `./publish.sh` with no args to push `main`:

```sh
# a shorthand for the CLI against this repo (works in bash and zsh)
zg() { ../../bin/zonegit --repo run/.zonegit "$@"; }

zg propose api-failover --from main           # HEAD moves onto api-failover
zg set api.lab.internal. A 300 10.0.0.99 -m "failover api"
zg review  api-failover --into main           # show the diff
zg approve api-failover --into main           # fast-forward main
./publish.sh                                  # BIND pulls the new main
```

Inspect history any time: `zg log`, `zg diff HEAD~1 HEAD`,
`zg blame api.lab.internal. A`.

## Why `publish.sh` runs `rndc refresh`

`zonegitd` does **not** send outbound DNS NOTIFY. Without it, BIND only
notices a change when its SOA **refresh** timer fires (60s in the sample
zone). `rndc refresh` forces an immediate SOA check + re-transfer, so a
publish is effectively instant. The 60s timer is the automatic fallback if
you change the zone without going through `publish.sh`.

## Ports

| Port | Process    | Purpose                          |
| ---- | ---------- | -------------------------------- |
| 5354 | `zonegitd` | AXFR source (the hidden primary) |
| 1053 | `named`    | serves DNS to clients            |
| 1953 | `named`    | `rndc` control channel           |

All bound to `127.0.0.1`, all > 1024, so nothing here needs `sudo`.

## Make it real

- **Your own domain.** Edit the top block of `env.sh` (`ZONE`,
  `ZONEFILE_NAME`) and drop your zonefile next to it, then `./up.sh`.
- **Resolve it system-wide (macOS, no port 53).** Point just this domain at
  BIND without touching your default resolver:
  ```sh
  sudo mkdir -p /etc/resolver
  printf 'nameserver 127.0.0.1\nport 1053\n' | sudo tee /etc/resolver/lab.internal
  dscacheutil -q host -a name api.lab.internal     # now resolves via BIND
  ```
- **Serve on the standard port 53.** Set `SERVE_PORT=53` in `env.sh`; `named`
  then needs to start as root (`sudo`), or redirect 53→1053 with `pfctl`.
- **Survive logout/reboot.** `up.sh` starts the daemons with `nohup`, so they
  outlive the terminal — but not a reboot. For always-on, wrap `zonegitd` and
  `named` in launchd plists (or run them under whatever process manager you
  already use).
- **Internet-facing / production.** Delegate the zone to this BIND from the
  parent (registrar NS records) and give BIND a public listener. Keep zonegit
  as the hidden primary. Heed the pre-1.0 caveat: the zonegit on-disk format
  isn't stable yet — but because BIND holds the served copy, a re-init only
  means re-importing into zonegit, not downtime.
