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
SOCK="$STATE_DIR/tailscaled.sock"

cleanup() {
    rm -rf "$STATE_DIR"
}
trap cleanup EXIT

# Everything below runs inside pasta namespace
pasta --config-net -- bash -c '
    STATE_DIR="$1"
    SOCK="$2"
    shift 2

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

    tailscaled --statedir="$STATE_DIR" --socket="$SOCK" &
    PID=$!

    for i in {1..100}; do
        [[ -S "$SOCK" ]] && break
        sleep 0.1
    done

    if [[ ! -S "$SOCK" ]]; then
        echo "ERROR: tailscaled socket not ready"
        exit 1
    fi

    tailscale --socket="$SOCK" up "${TS_ARGS[@]}"

    tailscale --socket="$SOCK" status

    if [[ ${#CMD_ARGS[@]} -gt 0 ]]; then
        "${CMD_ARGS[@]}"
        EXITCODE=$?
    else
        bash
        EXITCODE=$?
    fi

    kill $PID 2>/dev/null
    wait $PID 2>/dev/null || true
    exit $EXITCODE
' _ "$STATE_DIR" "$SOCK" "${TS_ARGS[@]}" "" "${CMD_ARGS[@]}"
