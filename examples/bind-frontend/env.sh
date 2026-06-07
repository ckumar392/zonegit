#!/usr/bin/env bash
# Shared configuration + helpers for the zonegit -> BIND front-end example.
# Sourced by up.sh / publish.sh / down.sh. Edit the first block to point at
# your own zone; everything below it is derived.

# ---- edit these for your zone ---------------------------------------------
ZONE="lab.internal."                 # zone name, trailing dot
ZONEFILE_NAME="lab.internal.zone"    # seed zonefile (sits next to this script)
ZONEGITD_ADDR="127.0.0.1:5354"       # zonegitd AXFR source ("hidden primary")
SERVE_PORT="1053"                    # port BIND serves the zone to clients on
CTRL_PORT="1953"                     # rndc control channel port
# ---------------------------------------------------------------------------

# ---- derived (no need to edit) --------------------------------------------
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
RUN="$HERE/run"
ZREPO="$RUN/.zonegit"
BINDDIR="$RUN/bind"
ZONEFILE="$HERE/$ZONEFILE_NAME"
ZONE_NODOT="${ZONE%.}"

ZONEGIT="$REPO_ROOT/bin/zonegit"
ZONEGITD="$REPO_ROOT/bin/zonegitd"

ZGD_HOST="${ZONEGITD_ADDR%:*}"
ZGD_PORT="${ZONEGITD_ADDR##*:}"

BIND_PREFIX="$(brew --prefix bind 2>/dev/null || echo /opt/homebrew/opt/bind)"
NAMED="$BIND_PREFIX/sbin/named"
RNDC="$BIND_PREFIX/sbin/rndc"
TSIG_KEYGEN="$BIND_PREFIX/sbin/tsig-keygen"
NAMED_CHECKCONF="$BIND_PREFIX/bin/named-checkconf"

# rndc against our local control channel, authenticated with the generated key.
rndc_cmd() { "$RNDC" -s 127.0.0.1 -p "$CTRL_PORT" -k "$BINDDIR/rndc.key" "$@"; }

# Print the SOA serial a server is currently answering with: soa_serial HOST PORT
soa_serial() { dig @"$1" -p "$2" +short "$ZONE" SOA 2>/dev/null | awk '{print $3}'; }

# Wait (up to ~10s) for something to be listening: wait_port TCP|UDP PORT
wait_port() {
    local proto="$1" port="$2" i
    for i in $(seq 1 100); do
        if lsof -nP -i"$proto":"$port" >/dev/null 2>&1; then return 0; fi
        sleep 0.1
    done
    return 1
}
