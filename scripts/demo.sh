#!/usr/bin/env bash
# scripts/demo.sh — end-to-end v0 demo for zonegit.
#
# What this script proves, in one shot:
#
#   1. BUILD            — compiles both binaries from source
#                           bin/zonegit   (CLI: write side, history queries)
#                           bin/zonegitd  (authoritative DNS server: read side)
#
#   2. INIT             — creates a fresh BadgerDB-backed repo at $REPO and
#                         records the zone name as a config-time fact.
#
#   3. IMPORT           — parses an RFC 1035 zonefile via miekg/dns, encodes
#                         each RRset canonically, builds a labelwise Merkle
#                         tree, writes one commit on branch 'main'. After
#                         this step the repo has 1 commit, 5 RRsets, and a
#                         content-addressed root tree.
#
#   4. SERVE            — starts zonegitd on 127.0.0.1:$PORT serving HEAD of
#                         'main'. The daemon opens Badger READ-ONLY (no
#                         lock), so the writer side stays usable.
#
#   5. DIG (initial)    — proves the server actually answers from the repo:
#                           api.foo.com.  A     -> 1.2.3.4
#                           www.foo.com.  A     -> CNAME api.foo.com. -> 1.2.3.4
#                         The daemon performs the in-zone CNAME chase server-side.
#
#   6. EDIT             — writes a new RRset for api.foo.com. (1.2.3.4 ->
#                         9.9.9.9). The CLI auto-commits, producing a 2nd
#                         commit on 'main'. Note: the daemon is NOT
#                         restarted. Each query reopens the read-only
#                         Badger handle, so it sees the new HEAD on the
#                         very next packet.
#
#   7. DIG (post-edit)  — same name, new answer:
#                           api.foo.com.  A     -> 9.9.9.9
#                         This is the whole point of the project — DNS
#                         state changed by a versioned commit, served
#                         live, no zone reload, no SOA bump rituals.
#
#   8. LOG              — shows the 2 commits (Git-style: hash + author +
#                         date + message). First-parent walk; reads come
#                         from the same content-addressed object store
#                         the daemon serves from.
#
#   9. DIFF HEAD~1 HEAD — lockstep walk of two trees, with structural-
#                         sharing skip when subtree hashes match. Output:
#                           ~ api A
#                         (one RRset modified between commits).
#
#  10. BLAME            — answers "who set api.foo.com. A to its current
#                         value, and when?" by walking the first-parent
#                         chain from HEAD until the blob hash at this
#                         (name, type) coordinate changes.
#
#  11. STATUS           — repo path, zone, current branch, HEAD hash.
#
# This is exactly the v0 "Done" definition from docs/ROADMAP.md.
#
# Usage:
#   ./scripts/demo.sh                       # /tmp/zonegit-demo, UDP/TCP 15353
#   PORT=5353 REPO=./.zonegit ./scripts/demo.sh
#   ZONE=example.org. ./scripts/demo.sh     # different zone
#
# Side effects:
#   - rebuilds binaries into ./bin/
#   - wipes and recreates $REPO
#   - leaves zonegitd's stdout/stderr at /tmp/zonegit-demo.log
#   - kills the daemon on exit (trap)
set -euo pipefail

# Resolve project root regardless of where the script is invoked from.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

REPO="${REPO:-/tmp/zonegit-demo}"
PORT="${PORT:-15353}"
ZONE="${ZONE:-foo.com.}"
BIN="$ROOT/bin"
mkdir -p "$BIN"

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }
step()  { echo; bold "── [$1/11] $2 ──"; shift 2; [[ $# -gt 0 ]] && dim "      $*"; }
run()   { printf '\033[2m$ %s\033[0m\n' "$*"; eval "$@"; }

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

step 1 "build" "compile both binaries from source"
run "go build -o $BIN/zonegit  ./cmd/zonegit"
run "go build -o $BIN/zonegitd ./cmd/zonegitd"

step 2 "init" "create a fresh BadgerDB repo and record the zone name"
rm -rf "$REPO"
run "$BIN/zonegit --repo $REPO init $ZONE"

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
run "$BIN/zonegit --repo $REPO --zone $ZONE import $ZONEFILE -m 'initial import'"

step 4 "start zonegitd" "open Badger READ-ONLY (no lock) so the writer side stays usable"
"$BIN/zonegitd" --repo "$REPO" --zone "$ZONE" --listen "127.0.0.1:$PORT" \
  > /tmp/zonegit-demo.log 2>&1 &
SERVER_PID=$!
sleep 1
echo "  server pid: $SERVER_PID   log: /tmp/zonegit-demo.log"

step 5a "dig api.$ZONE A" "expect 1.2.3.4 (direct answer from HEAD's tree)"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 5b "dig www.$ZONE A" "expect CNAME -> api.$ZONE -> 1.2.3.4 (server-side single-hop chase)"
run "dig +short @127.0.0.1 -p $PORT www.$ZONE A"

step 6 "edit" "change api A from 1.2.3.4 to 9.9.9.9 — this auto-commits commit #2 on main"
run "$BIN/zonegit --repo $REPO --zone $ZONE set -m 'bump api -> 9.9.9.9' api.$ZONE A 300 9.9.9.9"

step 7 "dig api.$ZONE A again" "expect 9.9.9.9 — same daemon, no restart, no zone-reload, no SOA bump"
run "dig +short @127.0.0.1 -p $PORT api.$ZONE A"

step 8 "log" "first-parent walk of commits (Git semantics over DNS state)"
run "$BIN/zonegit --repo $REPO log"

step 9 "diff HEAD~1 HEAD" "lockstep tree walk; structural sharing skips unchanged subtrees"
run "$BIN/zonegit --repo $REPO diff HEAD~1 HEAD"

step 10 "blame api.$ZONE A" "find the commit that introduced the *current* RRset value"
run "$BIN/zonegit --repo $REPO --zone $ZONE blame api.$ZONE A"

step 11 "status" "repo path, zone, branch, HEAD"
run "$BIN/zonegit --repo $REPO --zone $ZONE status"

echo
bold "── done ──"
dim   "  repo:        $REPO"
dim   "  server log:  /tmp/zonegit-demo.log"
dim   "  zonefile:    $ZONEFILE"
dim   "  inspect manually:  dig @127.0.0.1 -p $PORT <name> <type>"
dim   "  daemon will be killed on script exit (trap)."
