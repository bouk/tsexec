#!/bin/bash
# Minimal POC: Run tailscaled inside pasta namespace
#
# Usage: TS_AUTHKEY=tskey-xxx ./poc-simple.sh [command]
#
# This is the simplest possible version - pasta provides NAT so tailscaled
# can connect to control plane, and we use userspace-networking mode.

set -e

AUTH_KEY="${TS_AUTHKEY:?Error: set TS_AUTHKEY}"
STATE_DIR=$(mktemp -d /tmp/pasta-ts.XXXXXX)
SOCK="$STATE_DIR/tailscaled.sock"

cleanup() {
    echo "Cleaning up..."
    [[ -n "$PID" ]] && kill "$PID" 2>/dev/null || true
    rm -rf "$STATE_DIR"
}
trap cleanup EXIT

echo "State: $STATE_DIR"

# Everything below runs inside pasta namespace
pasta --config-net -- bash -c '
    STATE_DIR="$1"
    SOCK="$2"
    AUTH_KEY="$3"
    shift 3

    echo "[pasta] Starting tailscaled in userspace mode..."
    tailscaled \
        --statedir="$STATE_DIR" \
        --socket="$SOCK" \
        --tun=userspace-networking \
        &
    PID=$!

    # Wait for socket
    for i in {1..50}; do
        [[ -S "$SOCK" ]] && break
        sleep 0.1
    done

    echo "[pasta] Connecting to tailscale..."
    tailscale --socket="$SOCK" up \
        --authkey="$AUTH_KEY" \
        --hostname="pasta-test-$$" \
        --accept-dns=false

    echo "[pasta] Status:"
    tailscale --socket="$SOCK" status

    echo "[pasta] Tailscale IP:"
    tailscale --socket="$SOCK" ip

    if [[ $# -gt 0 ]]; then
        echo "[pasta] Running: $*"
        # Commands run in pasta network (NAT to host), not tailscale network
        # To use tailscale network, use: tailscale --socket=$SOCK nc <host> <port>
        "$@"
    else
        echo "[pasta] Dropping to shell. Use: tailscale --socket=$SOCK <cmd>"
        exec bash
    fi

    kill $PID 2>/dev/null
' _ "$STATE_DIR" "$SOCK" "$AUTH_KEY" "$@"
