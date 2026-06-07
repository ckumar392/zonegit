#!/usr/bin/env bash
# Publish the current zone state to BIND. Optionally make an edit first.
#
#   ./publish.sh set <name> <type> <ttl> <rdata...> [-m "msg"]   # edit, then push
#   ./publish.sh delete <name> <type>                            # delete, then push
#   ./publish.sh                                                 # push current main as-is
#
# For the PR-style flow, drive zonegit directly (WITHOUT --zone, so HEAD stays
# on the proposal branch), then run ./publish.sh with no args:
#
#   bin/zonegit --repo run/.zonegit propose api-failover --from main
#   bin/zonegit --repo run/.zonegit set api.lab.internal. A 300 10.0.0.99 -m "failover"
#   bin/zonegit --repo run/.zonegit review  api-failover --into main
#   bin/zonegit --repo run/.zonegit approve api-failover --into main
#   ./publish.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

# Optional edit. (set/delete take --zone safely; it commits to the zone's main.)
if [ "$#" -gt 0 ]; then
    echo "==> zonegit $*"
    "$ZONEGIT" --repo "$ZREPO" --zone "$ZONE" "$@"
fi

before="$(soa_serial 127.0.0.1 "$SERVE_PORT")"
# Wait for zonegitd's served serial to settle: its snapshotter polls every
# 200ms, so a just-made commit (a direct edit here OR a proposal you approved
# in zonegit directly) takes a moment to appear. Two equal reads = settled.
src=""; prev="__none__"
for i in $(seq 1 30); do
    src="$(soa_serial "$ZGD_HOST" "$ZGD_PORT")"
    if [ -n "$src" ] && [ "$src" = "$prev" ]; then break; fi
    prev="$src"
    sleep 0.1
done

echo "==> rndc refresh $ZONE_NODOT   (source serial=$src, BIND had=$before)"
rndc_cmd refresh "$ZONE_NODOT" >/dev/null
for i in $(seq 1 50); do
    if [ "$(soa_serial 127.0.0.1 "$SERVE_PORT")" = "$src" ]; then break; fi
    sleep 0.1
done
after="$(soa_serial 127.0.0.1 "$SERVE_PORT")"
echo "==> BIND now serving serial $after:"
dig @127.0.0.1 -p "$SERVE_PORT" +noall +answer "$ZONE" AXFR || true
