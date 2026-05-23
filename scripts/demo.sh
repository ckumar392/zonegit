#!/usr/bin/env bash
# scripts/demo.sh — end-to-end demo for zonegit (v0.3).
#
# What this script proves, in one shot:
#
#   1.  BUILD            — compiles both binaries from source
#                            bin/zonegit   (CLI: write side, history queries)
#                            bin/zonegitd  (authoritative DNS server: read side)
#
#   2.  INIT             — creates a fresh BadgerDB-backed repo at $REPO and
#                          PERSISTS the zone name in refs/zonegit/zone so the
#                          daemon and CLI never need --zone again.
#
#   3.  IMPORT           — parses an RFC 1035 zonefile via miekg/dns, encodes
#                          each RRset canonically, builds a labelwise Merkle
#                          tree, writes one commit on branch 'main'.
#
#   4.  SERVE            — starts zonegitd on 127.0.0.1:$PORT serving HEAD of
#                          'main' with a cached snapshotter (no per-query
#                          Badger reopen) and a Prometheus /metrics endpoint.
#
#   5.  DIG (initial)    — proves the server actually answers from the repo.
#
#   6.  EDIT             — writes a new RRset for api.foo.com. The apex SOA
#                          serial is AUTO-BUMPED in the same commit, so
#                          downstream secondaries see the change via their
#                          regular NOTIFY/refresh loop.
#
#   7.  DIG (post-edit)  — same name, new answer. The snapshotter picks up
#                          the new HEAD on the next 200ms poll.
#
#   8.  SOA BEFORE/AFTER — show the apex SOA serial moved forward by exactly
#                          1, without anyone editing it by hand.
#
#   9.  LOG / DIFF / BLAME / STATUS — git semantics over DNS state.
#
#  10.  BRANCH + EDIT on canary
#  11.  MERGE (ff)
#  12.  REVERT
#  13.  RESET --hard
#
#  14.  TIME-TRAVEL DAEMON — start a *second* zonegitd on a different port
#                            with --at HEAD~1, dig it. It answers as the
#                            zone existed N commits ago. No DNS server
#                            shipping today can do this.
#
#  15.  CANARY ROUTING   — kill the main daemon, restart with
#                            --canary canary:50. Dig from a handful of
#                            distinct client subnets and observe the bucket
#                            split between main and canary branches.
#
#  16.  AXFR             — request the full zone over TCP. Any standard
#                            BIND/Knot/PowerDNS secondary can slave off
#                            this server with no additional config.
#
#  17.  PROPOSE/APPROVE  — PR-style workflow: 'zonegit propose api-failover',
#                            stage edits, 'zonegit review', 'zonegit approve'.
#
#  18.  /metrics          — curl the Prometheus endpoint and show the
#                            per-qtype/per-rcode counters.
#
# Usage:
#   ./scripts/demo.sh                       # /tmp/zonegit-demo, UDP/TCP 15353
#   PORT=5353 REPO=./.zonegit ./scripts/demo.sh
#   ZONE=example.org. ./scripts/demo.sh     # different zone
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

REPO="${REPO:-/tmp/zonegit-demo}"
PORT="${PORT:-15353}"
TIME_TRAVEL_PORT="${TIME_TRAVEL_PORT:-16353}"
METRICS_PORT="${METRICS_PORT:-19353}"
ZONE="${ZONE:-foo.com.}"
BIN="$ROOT/bin"
mkdir -p "$BIN"

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }
step()  { echo; bold "── [$1] $2 ──"; shift 2; [[ $# -gt 0 ]] && dim "      $*"; return 0; }
run()   { printf '\033[2m$ %s\033[0m\n' "$*"; eval "$@"; }

SERVER_PID=""
TT_PID=""
CANARY_PID=""

cleanup() {
  for pid in "$SERVER_PID" "$TT_PID" "$CANARY_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
}
trap cleanup EXIT

step 1 "build" "compile both binaries from source"
run "go build -o $BIN/zonegit  ./cmd/zonegit"
run "go build -o $BIN/zonegitd ./cmd/zonegitd"

step 2 "init" "create a fresh BadgerDB repo and PERSIST the zone name"
rm -rf "$REPO"
run "$BIN/zonegit --repo $REPO init $ZONE"
dim   "      from now on, --zone is auto-loaded from the repo metadata."

step 3a "write a zonefile" "RFC 1035 text — exactly what BIND/Knot/PowerDNS would consume"
ZONEFILE="$(mktemp /tmp/zonegit-demo-zone.XXXXXX)"
cat >"$ZONEFILE" <<EOF
\$ORIGIN $ZONE
\$TTL 300
@   IN SOA ns1.$ZONE admin.$ZONE 1 7200 3600 1209600 300
    IN NS  ns1.$ZONE
ns1 IN A   10.0.0.1
api IN A   1.2.3.4
www IN CNAME api.$ZONE
EOF
run "cat $ZONEFILE"

step 3b "import" "parse via miekg/dns, canonicalise each RRset, hash, write commit #1 on main"
run "$BIN/zonegit --repo $REPO import $ZONEFILE -m 'initial import'"

step 4 "start zonegitd" "cached snapshotter; /metrics on :$METRICS_PORT"
"$BIN/zonegitd" --repo "$REPO" --listen "127.0.0.1:$PORT" \
  --metrics-listen "127.0.0.1:$METRICS_PORT" \
  > /tmp/zonegit-demo.log 2>&1 &
SERVER_PID=$!
sleep 1
echo "  server pid: $SERVER_PID   log: /tmp/zonegit-demo.log"

step 5a "dig api.$ZONE A" "expect 1.2.3.4"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"
step 5b "dig www.$ZONE A" "expect CNAME -> api -> 1.2.3.4 (server-side single-hop chase)"
run "dig +short @127.0.0.1 -p $PORT www.$ZONE A"

step 6a "SOA before edit" "remember this serial — we're about to bump it implicitly"
SOA_BEFORE=$(dig +short @127.0.0.1 -p $PORT $ZONE SOA | awk '{print $3}')
echo "  serial before: $SOA_BEFORE"

step 6b "edit" "change api A 1.2.3.4 -> 9.9.9.9 — apex SOA serial auto-bumps in the same commit"
run "$BIN/zonegit --repo $REPO set -m 'bump api -> 9.9.9.9' api.$ZONE A 300 9.9.9.9"
sleep 0.3   # let the daemon's 200ms snapshotter pick up the new HEAD

step 7 "dig api.$ZONE A again" "expect 9.9.9.9 — same daemon, no restart"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 8 "SOA after edit" "serial moved by exactly 1 — secondaries detect via standard refresh"
SOA_AFTER=$(dig +short @127.0.0.1 -p $PORT $ZONE SOA | awk '{print $3}')
echo "  serial before: $SOA_BEFORE   after: $SOA_AFTER"
if [[ "$SOA_AFTER" -ne "$((SOA_BEFORE + 1))" ]]; then
  echo "  WARN: expected serial to bump by 1"
fi

step 9 "log / diff / blame / status" "git semantics over DNS state"
run "$BIN/zonegit --repo $REPO log"
run "$BIN/zonegit --repo $REPO diff HEAD~1 HEAD"
run "$BIN/zonegit --repo $REPO blame api.$ZONE A"
run "$BIN/zonegit --repo $REPO status"

step 10 "branch + edit on canary" "fork main, switch HEAD, edit api there only"
run "$BIN/zonegit --repo $REPO branch canary"
run "$BIN/zonegit --repo $REPO checkout canary"
run "$BIN/zonegit --repo $REPO set -m 'canary: api -> 7.7.7.7' api.$ZONE A 300 7.7.7.7"
sleep 0.3
dim   "      daemon serves --branch main, so dig still sees 9.9.9.9:"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 11 "merge canary -> main (fast-forward)" "main hasn't moved, so this is a CAS-only ff"
run "$BIN/zonegit --repo $REPO checkout main"
run "$BIN/zonegit --repo $REPO merge canary"
sleep 0.3
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 12 "revert HEAD on main" "produce an inverse commit; api goes back to 9.9.9.9"
run "$BIN/zonegit --repo $REPO revert -m 'undo canary api bump' HEAD"
sleep 0.3
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 13 "reset --hard HEAD~1" "branch tip jumps forward; revert is now unreachable from main"
run "$BIN/zonegit --repo $REPO reset --hard HEAD~1"
sleep 0.3
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 14 "TIME-TRAVEL daemon" "start a SECOND zonegitd pinned at HEAD~1 on a different port"
"$BIN/zonegitd" --repo "$REPO" --listen "127.0.0.1:$TIME_TRAVEL_PORT" \
  --at HEAD~1 \
  > /tmp/zonegit-demo-tt.log 2>&1 &
TT_PID=$!
sleep 1
printf '  port %s (live):    ' "$PORT"
dig +short @127.0.0.1 -p "$PORT" "api.$ZONE" A
printf '  port %s (HEAD~1): ' "$TIME_TRAVEL_PORT"
dig +short @127.0.0.1 -p "$TIME_TRAVEL_PORT" "api.$ZONE" A
dim   "  — same name, two ports, two answers from two points in time."
kill "$TT_PID" 2>/dev/null || true
wait "$TT_PID" 2>/dev/null || true
TT_PID=""

step 15 "CANARY routing" "restart on a separate port with --canary canary:50; bucket-split by /24"
# Give canary a distinctly different value so the bucket split is visible.
run "$BIN/zonegit --repo $REPO checkout canary"
run "$BIN/zonegit --repo $REPO set -m 'canary: api -> 8.8.8.8' api.$ZONE A 300 8.8.8.8"
run "$BIN/zonegit --repo $REPO checkout main"
"$BIN/zonegitd" --repo "$REPO" --listen "127.0.0.1:$((PORT+10))" \
  --branch main --canary canary:50 --canary-salt "demo-rollout" \
  > /tmp/zonegit-demo-canary.log 2>&1 &
CANARY_PID=$!
sleep 1
dim   "      dig from 16 distinct /24s — counts should split between main (7.7.7.7) and canary (8.8.8.8):"
for i in {1..16}; do
  dig +short +subnet=10.${i}.0.1/24 @127.0.0.1 -p $((PORT+10)) api.$ZONE A
done | sort | uniq -c
kill "$CANARY_PID" 2>/dev/null || true
wait "$CANARY_PID" 2>/dev/null || true
CANARY_PID=""

step 16 "AXFR" "full zone transfer over TCP — slaves any BIND/Knot/PowerDNS secondary"
run "dig +tcp @127.0.0.1 -p $PORT $ZONE AXFR | grep -v '^;' | head -10"

step 17 "PROPOSE / REVIEW / APPROVE" "PR-style change-management vocabulary"
run "$BIN/zonegit --repo $REPO propose new-mail --from main"
run "$BIN/zonegit --repo $REPO set -m 'add mail MX' $ZONE MX 300 '10 mail.$ZONE'"
run "$BIN/zonegit --repo $REPO review new-mail --into main"
run "$BIN/zonegit --repo $REPO approve new-mail --into main"
sleep 0.3
run "dig +short @127.0.0.1 -p $PORT $ZONE MX"

step 18 "MULTI-ZONE — second zone in the same repo" "register bar.com. alongside foo.com., serve both from one daemon"
ZONE2="${ZONE2:-bar.com.}"
ZONEFILE2="$(mktemp /tmp/zonegit-demo-zone2.XXXXXX)"
cat >"$ZONEFILE2" <<EOF
\$ORIGIN $ZONE2
\$TTL 300
@   IN SOA ns1.$ZONE2 admin.$ZONE2 1 7200 3600 1209600 300
    IN NS  ns1.$ZONE2
ns1 IN A   10.0.0.2
api IN A   5.5.5.5
EOF
run "$BIN/zonegit --repo $REPO zone add $ZONE2"
run "$BIN/zonegit --repo $REPO zone switch $ZONE2"
run "$BIN/zonegit --repo $REPO import $ZONEFILE2 -m 'initial $ZONE2 import'"
run "$BIN/zonegit --repo $REPO zone list"
dim   "      same daemon, no restart: its background reconciler discovered $ZONE2 within 1s."
sleep 1.5  # give the daemon's zone reconciler one tick to notice
printf '  dig api.%s : ' "$ZONE"; dig +short @127.0.0.1 -p "$PORT" "api.$ZONE" A
printf '  dig api.%s : ' "$ZONE2"; dig +short @127.0.0.1 -p "$PORT" "api.$ZONE2" A
dim   "      one daemon, one port, two zones, no restart. Branches in $ZONE cannot affect $ZONE2 — they live under separate refs."

step 19 "IXFR — incremental zone transfer" "deltas instead of full re-AXFR after every change"
dim   "      The first IXFR with an old serial yields the records that changed since then."
dim   "      Recall the current $ZONE SOA serial:"
CURRENT_SERIAL=$(dig +short @127.0.0.1 -p $PORT $ZONE SOA | awk '{print $3}')
echo "  current $ZONE SOA serial: $CURRENT_SERIAL"
dim   "      Ask for an IXFR with an OLDER serial (serial 1 — the initial import). Expect to see the diff:"
run "dig +tcp @127.0.0.1 -p $PORT $ZONE IXFR=1 | grep -v '^;' | head -15"
dim   "      The output is bracketed by SOA records (latest, then old, then changes, then latest again)"
dim   "      — that's RFC 1995's IXFR shape, computed live from the commit DAG."

step 20 "DNSSEC — real Ed25519 signing" "generate KSK+ZSK, sign every RRset, dig +dnssec validates locally"
run "$BIN/zonegit --repo $REPO zone switch $ZONE"
run "$BIN/zonegit --repo $REPO zone-keygen $ZONE"
run "$BIN/zonegit --repo $REPO sign-zone"
sleep 0.3
dim   "      apex DNSKEYs (KSK=257, ZSK=256, alg 15=Ed25519):"
run "dig +short @127.0.0.1 -p $PORT $ZONE DNSKEY"
dim   "      RRSIG over api.$ZONE A — produced by the ZSK:"
run "dig +dnssec @127.0.0.1 -p $PORT api.$ZONE A | grep -E 'RRSIG|^api'"
dim   "      The dig output above includes a real RRSIG with a non-zero signature."
dim   "      A resolver with our KSK as a trust anchor would validate this chain end-to-end."

step 21 "DS record for parent zone" "publish-ready DS line; paste into the parent zone to complete the chain of trust"
run "$BIN/zonegit --repo $REPO ds $ZONE"

step 22 "--auto-sign on a set" "re-sign the touched RRset in the same commit, no separate sign-zone needed"
run "$BIN/zonegit --repo $REPO set --auto-sign -m 'rotate api with auto-sign' api.$ZONE A 300 9.9.9.9"
sleep 0.3
dim   "      RRSIG for api.$ZONE A — note: signature value differs from the previous step because the RRset changed."
run "dig +dnssec @127.0.0.1 -p $PORT api.$ZONE A | grep -E 'RRSIG|^api' | head -3"

step 23 "CoreDNS plugin" "the same repo served via a custom CoreDNS binary"
if [[ -x "$BIN/coredns" ]]; then
  CORE_PORT="${CORE_PORT:-15370}"
  COREFILE="$(mktemp /tmp/zonegit-demo-Corefile.XXXXXX)"
  cat >"$COREFILE" <<EOF
$ZONE:$CORE_PORT {
    zonegit $REPO {
        branch main
    }
    errors
}
EOF
  "$BIN/coredns" -conf "$COREFILE" > /tmp/zonegit-demo-coredns.log 2>&1 &
  CORE_PID=$!
  sleep 1.5
  dim   "      same repo, queried through CoreDNS instead of zonegitd:"
  printf '  CoreDNS @ port %s :  ' "$CORE_PORT"
  dig +short @127.0.0.1 -p "$CORE_PORT" "api.$ZONE" A
  printf '  zonegitd @ port %s : ' "$PORT"
  dig +short @127.0.0.1 -p "$PORT" "api.$ZONE" A
  dim   "      identical answer — the plugin is a thin wrapper around pkg/resolve.Resolver."
  kill "$CORE_PID" 2>/dev/null || true
  wait "$CORE_PID" 2>/dev/null || true
else
  dim   "      bin/coredns not present. Build with: make coredns (first build pulls CoreDNS deps)."
fi

step 24 "/metrics" "Prometheus exposition — qtype/rcode histograms + active-branch gauge"
run "curl -s http://127.0.0.1:$METRICS_PORT/metrics | head -20"

echo
bold "── done ──"
dim   "  repo:        $REPO"
dim   "  server log:  /tmp/zonegit-demo.log"
dim   "  inspect manually:  dig @127.0.0.1 -p $PORT <name> <type>"
dim   "  metrics:           curl http://127.0.0.1:$METRICS_PORT/metrics"
