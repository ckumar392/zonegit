#!/usr/bin/env bash
# Stop named + zonegitd started by up.sh. Leaves run/ (the repo + slaved
# zone) in place; delete run/ yourself for a full reset, or just re-run up.sh.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

stop_pid() {
    local name="$1" f="$RUN/$1.pid" pid
    [ -f "$f" ] || return 0
    pid="$(cat "$f")"
    if kill "$pid" 2>/dev/null; then echo "stopped $name (pid $pid)"; fi
    rm -f "$f"
}
stop_pid named
stop_pid zonegitd

# Fallback: free our ports in case the pid files were lost.
for p in "$SERVE_PORT" "$ZGD_PORT" "$CTRL_PORT"; do
    pids="$(lsof -nP -ti:"$p" 2>/dev/null || true)"
    if [ -n "$pids" ]; then
        # shellcheck disable=SC2086
        kill $pids 2>/dev/null && echo "freed port $p"
    fi
done
echo "down."
