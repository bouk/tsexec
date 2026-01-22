#!/bin/bash
# POC: Run normal tailscaled inside a pasta namespace
#
# This demonstrates that tailscaled can run directly inside a pasta namespace
# without any FD passing dance - pasta provides NAT'd networking so tailscaled
# can connect to the control plane normally.

set -e

# Parse arguments
AUTH_KEY="${TS_AUTHKEY:-}"
AUTH_KEY_FILE=""
HOSTNAME="pasta-tailscaled-$$"
STATE_DIR=""
VERBOSE=""
USER_CMD=""

usage() {
    echo "Usage: $0 [options] -- command [args...]"
    echo ""
    echo "Options:"
    echo "  -k, --auth-key KEY       Tailscale auth key (or use TS_AUTHKEY env)"
    echo "  -f, --auth-key-file FILE Read auth key from file"
    echo "  -h, --hostname NAME      Node hostname (default: pasta-tailscaled-PID)"
    echo "  -s, --state-dir DIR      State directory (default: temp)"
    echo "  -v, --verbose            Enable verbose output"
    echo "  --help                   Show this help"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -k|--auth-key)
            AUTH_KEY="$2"
            shift 2
            ;;
        -f|--auth-key-file)
            AUTH_KEY_FILE="$2"
            shift 2
            ;;
        -h|--hostname)
            HOSTNAME="$2"
            shift 2
            ;;
        -s|--state-dir)
            STATE_DIR="$2"
            shift 2
            ;;
        -v|--verbose)
            VERBOSE="1"
            shift
            ;;
        --help)
            usage
            ;;
        --)
            shift
            USER_CMD="$*"
            break
            ;;
        *)
            echo "Unknown option: $1"
            usage
            ;;
    esac
done

# Read auth key from file if specified
if [[ -n "$AUTH_KEY_FILE" ]]; then
    if [[ ! -f "$AUTH_KEY_FILE" ]]; then
        echo "Error: auth key file not found: $AUTH_KEY_FILE"
        exit 1
    fi
    AUTH_KEY=$(cat "$AUTH_KEY_FILE")
fi

if [[ -z "$AUTH_KEY" ]]; then
    echo "Error: auth key required (use -k, -f, or TS_AUTHKEY env)"
    exit 1
fi

# Create temp state dir if not specified
if [[ -z "$STATE_DIR" ]]; then
    STATE_DIR=$(mktemp -d /tmp/pasta-tailscaled.XXXXXX)
    CLEANUP_STATE=1
fi

SOCK="$STATE_DIR/tailscaled.sock"

cleanup() {
    if [[ -n "$TAILSCALED_PID" ]]; then
        [[ -n "$VERBOSE" ]] && echo "Stopping tailscaled (pid $TAILSCALED_PID)..."
        kill "$TAILSCALED_PID" 2>/dev/null || true
        wait "$TAILSCALED_PID" 2>/dev/null || true
    fi
    if [[ -n "$CLEANUP_STATE" ]]; then
        [[ -n "$VERBOSE" ]] && echo "Cleaning up state dir: $STATE_DIR"
        rm -rf "$STATE_DIR"
    fi
}

trap cleanup EXIT

[[ -n "$VERBOSE" ]] && echo "State directory: $STATE_DIR"
[[ -n "$VERBOSE" ]] && echo "Hostname: $HOSTNAME"

# This is the script that runs inside the pasta namespace
inner_script() {
    cat << 'INNEREOF'
#!/bin/bash
set -e

STATE_DIR="$1"
SOCK="$2"
AUTH_KEY="$3"
HOSTNAME="$4"
VERBOSE="$5"
shift 5
USER_CMD="$*"

[[ -n "$VERBOSE" ]] && echo "[inner] Starting tailscaled..."

# Start tailscaled in userspace mode (no root needed for tun)
tailscaled \
    --statedir="$STATE_DIR" \
    --socket="$SOCK" \
    --tun=userspace-networking \
    ${VERBOSE:+--verbose=1} \
    &
TAILSCALED_PID=$!

# Wait for socket to be ready
[[ -n "$VERBOSE" ]] && echo "[inner] Waiting for tailscaled socket..."
for i in {1..30}; do
    if [[ -S "$SOCK" ]]; then
        break
    fi
    sleep 0.1
done

if [[ ! -S "$SOCK" ]]; then
    echo "[inner] Error: tailscaled socket not ready"
    exit 1
fi

[[ -n "$VERBOSE" ]] && echo "[inner] Running tailscale up..."

# Connect to tailscale
tailscale --socket="$SOCK" up \
    --authkey="$AUTH_KEY" \
    --hostname="$HOSTNAME" \
    --accept-routes=false \
    --accept-dns=false

[[ -n "$VERBOSE" ]] && echo "[inner] Connected! Status:"
[[ -n "$VERBOSE" ]] && tailscale --socket="$SOCK" status

# Run the user command if provided
if [[ -n "$USER_CMD" ]]; then
    [[ -n "$VERBOSE" ]] && echo "[inner] Running: $USER_CMD"
    # Use tailscale's SOCKS proxy or just run command directly
    # Since we're using userspace networking, we need to use the proxy

    SOCKS_PORT=1055

    # Start SOCKS proxy in background (tailscaled in userspace mode provides this)
    # Actually, userspace-networking mode makes tailscale act as a SOCKS proxy
    # We need to use it via environment or proxychains

    # For now, let's just exec the command - it will use the pasta network
    # which has NAT to the host. To use tailscale network, we'd need socks proxy.
    exec $USER_CMD
else
    [[ -n "$VERBOSE" ]] && echo "[inner] No command specified, dropping to shell"
    exec bash
fi
INNEREOF
}

# Create the inner script
INNER_SCRIPT="$STATE_DIR/inner.sh"
inner_script > "$INNER_SCRIPT"
chmod +x "$INNER_SCRIPT"

[[ -n "$VERBOSE" ]] && echo "Launching pasta namespace..."

# Run inside pasta namespace
# pasta --config-net sets up the namespace with NAT networking
exec pasta \
    --config-net \
    -- \
    "$INNER_SCRIPT" \
    "$STATE_DIR" \
    "$SOCK" \
    "$AUTH_KEY" \
    "$HOSTNAME" \
    "$VERBOSE" \
    $USER_CMD
