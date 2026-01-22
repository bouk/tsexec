#!/bin/bash
# POC: Run tailscaled with real TUN inside pasta namespace
#
# Usage: TS_AUTHKEY=tskey-xxx ./poc-tun.sh [command]
#
# In a pasta namespace, we have CAP_NET_ADMIN in the user namespace,
# so tailscaled should be able to create a TUN device directly.
# This means traffic actually flows through the TUN and tailscale handles routing.

set -e

AUTH_KEY="${TS_AUTHKEY:?Error: set TS_AUTHKEY}"
STATE_DIR=$(mktemp -d /tmp/pasta-ts.XXXXXX)
SOCK="$STATE_DIR/tailscaled.sock"

cleanup() {
    echo "Cleaning up..."
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

    echo "[pasta] Network interfaces before tailscaled:"
    ip link show
    echo

    echo "[pasta] Starting tailscaled with TUN device..."
    # Let tailscaled create its own TUN device
    # In pasta namespace we should have the necessary caps
    tailscaled \
        --statedir="$STATE_DIR" \
        --socket="$SOCK" \
        --verbose=1 \
        &
    PID=$!

    # Wait for socket
    echo "[pasta] Waiting for tailscaled..."
    for i in {1..100}; do
        [[ -S "$SOCK" ]] && break
        sleep 0.1
    done

    if [[ ! -S "$SOCK" ]]; then
        echo "[pasta] ERROR: tailscaled socket not ready"
        exit 1
    fi

    echo "[pasta] Connecting to tailscale..."
    tailscale --socket="$SOCK" up \
        --authkey="$AUTH_KEY" \
        --hostname="pasta-tun-$$" \
        --accept-dns=false \
        --accept-routes=false

    echo
    echo "[pasta] Network interfaces after tailscale up:"
    ip link show
    echo

    echo "[pasta] Routes:"
    ip route show
    echo

    echo "[pasta] Status:"
    tailscale --socket="$SOCK" status
    echo

    TSIP=$(tailscale --socket="$SOCK" ip -4 2>/dev/null || echo "no-ip")
    echo "[pasta] Tailscale IP: $TSIP"

    if [[ $# -gt 0 ]]; then
        echo "[pasta] Running: $*"
        "$@"
        EXITCODE=$?
    else
        echo
        echo "[pasta] Dropping to shell."
        echo "  - Tailscale socket: $SOCK"
        echo "  - Example: ping \$SOME_TAILSCALE_IP"
        echo "  - Example: tailscale --socket=$SOCK status"
        bash
        EXITCODE=$?
    fi

    echo "[pasta] Shutting down tailscaled..."
    kill $PID 2>/dev/null
    wait $PID 2>/dev/null || true
    exit $EXITCODE
' _ "$STATE_DIR" "$SOCK" "$AUTH_KEY" "$@"
