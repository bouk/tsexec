#!/bin/bash
# POC: Run normal tailscaled inside pasta namespace
#
# Usage: ./poc.sh [tailscale up flags] -- [command]
#
# Pasta provides CAP_NET_ADMIN in the user namespace, so tailscaled
# can create a TUN device directly. No FD passing needed.

set -e

# Split args at --
TS_ARGS=()
CMD_ARGS=()
seen_sep=false
for arg in "$@"; do
    if [[ "$arg" == "--" ]]; then
        seen_sep=true
    elif $seen_sep; then
        CMD_ARGS+=("$arg")
    else
        TS_ARGS+=("$arg")
    fi
done

STATE_DIR=$(mktemp -d /tmp/pasta-ts.XXXXXX)

cleanup() {
    rm -rf "$STATE_DIR"
}
trap cleanup EXIT

# Everything below runs inside pasta namespace
pasta --config-net -- unshare --mount bash -c '
    set -e
    STATE_DIR="$1"
    shift

    # Split remaining args - TS_ARGS until empty string marker, then CMD_ARGS
    TS_ARGS=()
    CMD_ARGS=()
    seen_marker=false
    for arg in "$@"; do
        if [[ "$arg" == "" ]] && ! $seen_marker; then
            seen_marker=true
        elif $seen_marker; then
            CMD_ARGS+=("$arg")
        else
            TS_ARGS+=("$arg")
        fi
    done

    # Mount tmpfs at /var/run/tailscale so tailscaled can use default socket path
    mkdir -p /var/run/tailscale
    mount -t tmpfs tmpfs /var/run/tailscale

    tailscaled --statedir="$STATE_DIR" &
    PID=$!

    for i in {1..100}; do
        [[ -S /var/run/tailscale/tailscaled.sock ]] && break
        sleep 0.1
    done

    if [[ ! -S /var/run/tailscale/tailscaled.sock ]]; then
        echo "ERROR: tailscaled socket not ready"
        exit 1
    fi

    tailscale up "${TS_ARGS[@]}"

    tailscale status

    cleanup() {
        kill $PID 2>/dev/null
        wait $PID 2>/dev/null || true
    }
    trap cleanup EXIT

    if [[ ${#CMD_ARGS[@]} -gt 0 ]]; then
        "${CMD_ARGS[@]}"
    else
        bash
    fi
' _ "$STATE_DIR" "${TS_ARGS[@]}" "" "${CMD_ARGS[@]}"
