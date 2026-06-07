#!/usr/bin/env bash
# Bring up the whole front-end stack from a clean slate:
#   zonegit (author) -> zonegitd (AXFR source) -> BIND named (serves DNS).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

# 0. binaries
if [[ ! -x "$ZONEGIT" || ! -x "$ZONEGITD" ]]; then
    echo "==> building zonegit binaries (make build)"
    ( cd "$REPO_ROOT" && make build )
fi
if [[ ! -x "$NAMED" ]]; then
    echo "BIND not found at $NAMED — install it with:  brew install bind" >&2
    exit 1
fi

# 1. clean slate
"$HERE/down.sh" >/dev/null 2>&1 || true
rm -rf "$RUN"
mkdir -p "$BINDDIR"

# 2. author the zone in zonegit
echo "==> zonegit init + import $ZONE"
"$ZONEGIT" --repo "$ZREPO" init "$ZONE" >/dev/null
"$ZONEGIT" --repo "$ZREPO" --zone "$ZONE" import "$ZONEFILE" -m "seed import"

# 3. zonegitd = the AXFR source ("hidden primary")
echo "==> starting zonegitd on $ZONEGITD_ADDR"
nohup "$ZONEGITD" --repo "$ZREPO" --zone "$ZONE" --listen "$ZONEGITD_ADDR" \
    > "$RUN/zonegitd.log" 2>&1 &
echo $! > "$RUN/zonegitd.pid"
disown 2>/dev/null || true
wait_port TCP "$ZGD_PORT" || { echo "zonegitd did not come up; see $RUN/zonegitd.log" >&2; exit 1; }

# 4. generate the BIND secondary config (rndc key + named.conf)
echo "==> generating BIND config"
"$TSIG_KEYGEN" -a hmac-sha256 rndc-key > "$BINDDIR/rndc.key"
cat > "$BINDDIR/named.conf" <<EOF
include "$BINDDIR/rndc.key";

controls {
    inet 127.0.0.1 port $CTRL_PORT allow { 127.0.0.1; } keys { "rndc-key"; };
};

options {
    directory "$BINDDIR";
    pid-file "$BINDDIR/named.pid";
    listen-on port $SERVE_PORT { 127.0.0.1; };
    listen-on-v6 { none; };
    recursion no;
    dnssec-validation no;
    allow-transfer { 127.0.0.1; };   // local-only, so 'dig AXFR' can inspect the zone
};

// Slave $ZONE from zonegitd ($ZONEGITD_ADDR). zonegit auto-bumps the SOA
// serial on every commit, so BIND's normal refresh check sees the change.
// There is no NOTIFY, so publish.sh runs 'rndc refresh' to force an instant
// pull instead of waiting for the SOA refresh timer.
zone "$ZONE_NODOT" {
    type secondary;
    file "$ZONE_NODOT.db";
    primaries { $ZGD_HOST port $ZGD_PORT; };
};
EOF
"$NAMED_CHECKCONF" "$BINDDIR/named.conf"

# 5. start BIND
echo "==> starting BIND (named) on 127.0.0.1:$SERVE_PORT"
nohup "$NAMED" -g -c "$BINDDIR/named.conf" > "$RUN/named.log" 2>&1 &
echo $! > "$RUN/named.pid"
disown 2>/dev/null || true
wait_port UDP "$SERVE_PORT" || { echo "named did not come up; see $RUN/named.log" >&2; exit 1; }

# 6. force the initial transfer and wait for BIND to catch up to the source
rndc_cmd refresh "$ZONE_NODOT" >/dev/null 2>&1 || true
src="$(soa_serial "$ZGD_HOST" "$ZGD_PORT")"
for i in $(seq 1 50); do
    if [ "$(soa_serial 127.0.0.1 "$SERVE_PORT")" = "$src" ]; then break; fi
    sleep 0.1
done

cat <<EOF

==> UP.
    zonegit   authors the zone        (repo:  $ZREPO)
    zonegitd  serves AXFR             ($ZONEGITD_ADDR)
    BIND      serves DNS to clients   (127.0.0.1:$SERVE_PORT, serial $(soa_serial 127.0.0.1 "$SERVE_PORT"))

    Try it:
      dig @127.0.0.1 -p $SERVE_PORT api.$ZONE +short
      ./publish.sh set api.$ZONE A 300 10.9.9.9 -m "demo change"
      ./down.sh
EOF
dig @127.0.0.1 -p "$SERVE_PORT" "api.$ZONE" +short || true
