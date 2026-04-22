#!/usr/bin/env bash
# scripts/demo.sh — end-to-end v0 demo for dnsdb.
#
# Walks the "Done" definition from ROADMAP.md:
#   init → import → dig → set → commit → dig → log → diff → blame
#
# Usage:
#   ./scripts/demo.sh              # uses /tmp/dnsdb-demo, port 15353
#   PORT=5353 REPO=./.dnsdb ./scripts/demo.sh
set -euo pipefail

# Resolve project root regardless of where the script is invoked from.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

REPO="${REPO:-/tmp/dnsdb-demo}"
PORT="${PORT:-15353}"
ZONE="${ZONE:-foo.com.}"
BIN="$ROOT/bin"
mkdir -p "$BIN"

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
step()  { echo; bold "── $* ──"; }
run()   { printf '\033[2m$ %s\033[0m\n' "$*"; eval "$@"; }

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

step "build"
run "go build -o $BIN/dnsdb  ./cmd/dnsdb"
run "go build -o $BIN/dnsdbd ./cmd/dnsdbd"

step "fresh repo"
rm -rf "$REPO"
run "$BIN/dnsdb --repo $REPO init $ZONE"

step "write zonefile"
ZONEFILE="$(mktemp /tmp/dnsdb-demo-zone.XXXXXX)"
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

step "import"
run "$BIN/dnsdb --repo $REPO --zone $ZONE import $ZONEFILE -m 'initial import'"

step "start dnsdbd in background"
"$BIN/dnsdbd" --repo "$REPO" --zone "$ZONE" --listen "127.0.0.1:$PORT" \
  > /tmp/dnsdb-demo.log 2>&1 &
SERVER_PID=$!
sleep 1
echo "  server pid: $SERVER_PID"

step "dig api.$ZONE — expect 1.2.3.4"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step "dig www.$ZONE — expect CNAME chase to 1.2.3.4"
run "dig +short @127.0.0.1 -p $PORT www.$ZONE A"

step "edit: bump api to 9.9.9.9"
run "$BIN/dnsdb --repo $REPO --zone $ZONE set -m 'bump api -> 9.9.9.9' api.$ZONE A 300 9.9.9.9"

step "dig again — expect 9.9.9.9 (no daemon restart)"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step "log"
run "$BIN/dnsdb --repo $REPO log"

step "diff HEAD~1 HEAD"
run "$BIN/dnsdb --repo $REPO diff HEAD~1 HEAD"

step "blame api.$ZONE A"
run "$BIN/dnsdb --repo $REPO --zone $ZONE blame api.$ZONE A"

step "status"
run "$BIN/dnsdb --repo $REPO --zone $ZONE status"

step "done — repo: $REPO   server log: /tmp/dnsdb-demo.log"
